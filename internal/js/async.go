package js

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/dop251/goja"
)

// Promises in a Function node.
//
// Node-RED wraps the function body in an async function, so `await` works at
// the top level of the body, a promise the body returns is waited for, and a
// promise the Setup code returns is waited for before the first message is
// processed (10-function.js, "asynchronous setup", and the node.linkcall API).
//
// A function that names await, async, Promise or linkcall runs here, on the
// dedicated runtime the timers use (timers.go), because a continuation has to
// live in the runtime that made the promise. Everything else keeps the pooled
// runtimes and the synchronous path it always had.
//
// Whatever settles a promise does so with the session lock held, then lets
// goja run its jobs, and what the continuation sends leaves through the
// function set with SetAsync, the same way a timer callback's output does.

var asyncWord = regexp.MustCompile(`\b(await|async|Promise|linkcall)\b`)

func usesAsync(src string) bool { return asyncWord.MatchString(src) }

// LinkCaller starts a call to a Link In for node.linkcall. It returns an error
// when the call cannot start (no such Link In), and otherwise calls deliver
// exactly once, later and from any goroutine, with the message that came back
// or the error that ended the wait (a timeout).
type LinkCaller func(target string, m *engine.Msg, timeout time.Duration, deliver func(*engine.Msg, error)) error

// DefaultLinkCallTimeout is node.linkcall's wait when the call gives none, from
// Node-RED (10-function.js: options.timeout || 5000).
const DefaultLinkCallTimeout = 5 * time.Second

// installLinkCall adds node.linkcall to the session's node object.
func (s *session) installLinkCall(p *Program, nodeObj goja.Value) {
	vm := s.vm
	obj := nodeObj.ToObject(vm)
	_ = obj.Set("linkcall", func(call goja.FunctionCall) goja.Value {
		promise, resolve, reject := vm.NewPromise()
		fail := func(msg string) goja.Value {
			_ = reject(vm.NewGoError(errors.New(msg)))
			return vm.ToValue(promise)
		}
		target, _ := call.Argument(0).Export().(string)
		if target == "" {
			return fail("node.linkcall: the target must be the name or id of a link in node")
		}
		raw, ok := call.Argument(1).Export().(map[string]any)
		if !ok {
			return fail("node.linkcall: the second argument must be a message object")
		}
		timeout := DefaultLinkCallTimeout
		clone := true
		if opts, ok := call.Argument(2).Export().(map[string]any); ok {
			switch t := opts["timeout"].(type) {
			case int64:
				if t > 0 {
					timeout = time.Duration(t) * time.Millisecond
				}
			case float64:
				if t > 0 {
					timeout = time.Duration(t * float64(time.Millisecond))
				}
			}
			if c, ok := opts["clone"].(bool); ok {
				clone = c
			}
		}
		m := engine.WrapMsg(raw)
		if clone {
			m = m.Clone()
		}
		if s.sb.LinkCall == nil {
			return fail("node.linkcall is not available here")
		}
		err := s.sb.LinkCall(target, m, timeout, func(back *engine.Msg, err error) {
			s.settle(p, func() {
				if err != nil {
					_ = reject(vm.NewGoError(err))
					return
				}
				_ = resolve(vm.ToValue(back.Data))
			})
		})
		if err != nil {
			return fail(err.Error())
		}
		return vm.ToValue(promise)
	})
}

// settle runs f, which settles a promise, on the runtime, under its lock, then
// lets the continuations run and sends what they produced.
func (s *session) settle(p *Program, f func()) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.begin(nil)
	f()
	// Promise jobs run when a call into the runtime ends.
	_, _ = s.vm.RunString("void 0")
	res := s.finish()
	s.mu.Unlock()
	p.smu.Lock()
	send := p.async
	p.smu.Unlock()
	if send != nil {
		send(res)
	}
}

// promiseOf returns the promise a call gave back, if it gave one.
func promiseOf(v goja.Value) (*goja.Promise, bool) {
	if v == nil {
		return nil, false
	}
	pr, ok := v.Export().(*goja.Promise)
	return pr, ok
}

// rejection turns what a promise was rejected with into an error.
func rejection(v goja.Value) error {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return errors.New("rejected")
	}
	if o, ok := v.(*goja.Object); ok {
		if m := o.Get("message"); m != nil && !goja.IsUndefined(m) {
			if name := o.Get("name"); name != nil && !goja.IsUndefined(name) && name.String() != "Error" {
				return fmt.Errorf("%s: %s", name.String(), m.String())
			}
			return errors.New(m.String())
		}
	}
	return errors.New(v.String())
}

// afterPromise calls ok with the value of a promise, or bad with its
// rejection, as soon as it settles. A promise that has already settled is
// answered at once. The caller holds the session lock, and so does whatever
// settles the promise later, so ok and bad run with it held.
func (s *session) afterPromise(pr *goja.Promise, ok func(goja.Value), bad func(error)) {
	switch pr.State() {
	case goja.PromiseStateFulfilled:
		ok(pr.Result())
		return
	case goja.PromiseStateRejected:
		bad(rejection(pr.Result()))
		return
	}
	then, isFn := goja.AssertFunction(s.vm.ToValue(pr).ToObject(s.vm).Get("then"))
	if !isFn {
		bad(errors.New("internal: a promise with no then"))
		return
	}
	_, err := then(s.vm.ToValue(pr),
		s.vm.ToValue(func(call goja.FunctionCall) goja.Value { ok(call.Argument(0)); return goja.Undefined() }),
		s.vm.ToValue(func(call goja.FunctionCall) goja.Value { bad(rejection(call.Argument(0))); return goja.Undefined() }))
	if err != nil {
		bad(translateError(err))
	}
}

// waitSetup holds a message back until the Setup code's promise has settled,
// as Node-RED queues messages behind it.
func (s *session) waitSetup(ctx context.Context) error {
	s.mu.Lock()
	ready, failed := s.ready, s.setupErr
	s.mu.Unlock()
	if failed != nil {
		return fmt.Errorf("the setup code failed: %w", failed)
	}
	if ready == nil {
		return nil
	}
	select {
	case <-ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setupErr != nil {
		return fmt.Errorf("the setup code failed: %w", s.setupErr)
	}
	return nil
}

// startSetup records the Setup code's outcome. A promise that is still pending
// makes later messages wait for it (waitSetup); one that rejects is reported
// once, and every later message gets the same error, which is what Node-RED's
// error state does to the node.
func (s *session) startSetup(out goja.Value) error {
	pr, isPromise := promiseOf(out)
	if !isPromise {
		return nil
	}
	if pr.State() == goja.PromiseStateFulfilled {
		return nil
	}
	if pr.State() == goja.PromiseStateRejected {
		s.setupErr = rejection(pr.Result())
		return s.setupErr
	}
	s.ready = make(chan struct{})
	ready := s.ready
	s.afterPromise(pr,
		func(goja.Value) { close(ready) },
		func(err error) {
			s.setupErr = err
			s.res.Errors = append(s.res.Errors, "setup code: "+err.Error())
			close(ready)
		})
	return nil
}

// finishMain settles what the body returned. A promise that is already settled
// is read at once; one still waiting sends its result later, through the
// function set with SetAsync, and reports its own failure the same way. orig is
// the message the body was given.
func (p *Program) finishMain(s *session, out goja.Value, orig *engine.Msg) error {
	pr, isPromise := promiseOf(out)
	if !isPromise {
		return p.collectReturn(s.vm, out, &s.sb, &s.res)
	}
	sb := s.sb
	sb.Msg = orig
	var settled error
	sync := true
	report := func(err error) {
		if sync {
			settled = err
			return
		}
		s.res.Errors = append(s.res.Errors, err.Error())
	}
	s.afterPromise(pr,
		func(v goja.Value) {
			if err := p.collectReturn(s.vm, v, &sb, &s.res); err != nil {
				report(err)
			}
		},
		report)
	sync = false
	return settled
}

// UsesPromises reports whether the function runs on the promise-aware runtime.
func (p *Program) UsesPromises() bool { return p.promised }
