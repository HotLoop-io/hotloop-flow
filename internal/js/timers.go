package js

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/dop251/goja"
)

// Timers: setTimeout, setInterval and their clear functions.
//
// A Function node that names one of them runs on a single runtime of its own
// instead of the shared pool, because the callback is a closure in whatever
// runtime scheduled it. Everything on that runtime happens under one mutex, a
// message, a setup run or a timer callback, one at a time, which is the
// guarantee Node's single thread gives a function.
//
// The timers use the node's clock (node.ClockOf), so a flow test moves them,
// and every callback has the same CPU ceiling as a message. A callback's
// node.send goes out through the function set with SetAsync. Stop clears every
// timer, so a redeploy leaves nothing ticking.

var timerWord = regexp.MustCompile(`\b(setTimeout|setInterval|clearTimeout|clearInterval)\b`)

func usesTimers(src string) bool { return timerWord.MatchString(src) }

// MaxTimers is how many timers one function may have waiting at once. A
// function that schedules one per message and never clears them is a slow leak,
// so it is stopped with a message instead.
const MaxTimers = 1000

// MinInterval is the shortest interval honored. Node clamps to 1 ms, and a
// runtime that is asked for 0 would otherwise spin.
const MinInterval = time.Millisecond

type timerEntry struct {
	fn       goja.Callable
	args     []goja.Value
	every    time.Duration // zero for a one shot
	handle   node.Timer
	canceled bool
}

type session struct {
	mu     sync.Mutex
	vm     *goja.Runtime
	sb     Sandbox
	res    Result
	clock  node.Clock
	closed bool
	nextID int
	timers map[int]*timerEntry

	nodeObj, ctxObj, flowObj, globalObj, envObj, utilObj goja.Value
	mainFn, startFn, stopFn                              goja.Callable
}

// SetAsync sets where output from a timer callback goes. The Function node
// passes its emit.
func (p *Program) SetAsync(f func(*Result)) {
	p.smu.Lock()
	p.async = f
	p.smu.Unlock()
}

// Stop clears every timer and ends the dedicated runtime's life. It is safe to
// call on a program that never started one.
func (p *Program) Stop() {
	p.smu.Lock()
	s := p.sess
	p.smu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stopTimersLocked()
	s.mu.Unlock()
}

// TimerCount is how many timers are waiting. A test and the status line use it.
func (p *Program) TimerCount() int {
	p.smu.Lock()
	s := p.sess
	p.smu.Unlock()
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.timers)
}

func (s *session) stopTimersLocked() {
	s.closed = true
	for id, e := range s.timers {
		e.canceled = true
		if e.handle != nil {
			e.handle.Stop()
		}
		delete(s.timers, id)
	}
}

// getSession builds the dedicated runtime on first use.
func (p *Program) getSession(sb Sandbox) (*session, error) {
	p.smu.Lock()
	defer p.smu.Unlock()
	if p.sess != nil {
		return p.sess, nil
	}
	s := &session{
		vm:     newRuntime(),
		sb:     sb,
		clock:  node.ClockOf(sb.Services),
		timers: map[int]*timerEntry{},
	}
	s.sb.Msg = nil
	s.vm.SetTimeSource(s.clock.Now)
	s.installTimers(p)

	s.nodeObj = p.nodeAPI(s.vm, &s.sb, &s.res)
	s.ctxObj = p.contextAPI(s.vm, sb.Services, node.ScopeNode)
	s.flowObj = p.contextAPI(s.vm, sb.Services, node.ScopeFlow)
	s.globalObj = p.contextAPI(s.vm, sb.Services, node.ScopeGlobal)
	s.envObj = p.envAPI(s.vm, sb.Services)
	s.utilObj = p.utilAPI(s.vm)

	var err error
	if s.mainFn, err = p.callable(s.vm, p.main); err != nil {
		return nil, err
	}
	if p.onStart != nil {
		if s.startFn, err = p.callable(s.vm, p.onStart); err != nil {
			return nil, err
		}
	}
	if p.onStop != nil {
		if s.stopFn, err = p.callable(s.vm, p.onStop); err != nil {
			return nil, err
		}
	}
	p.sess = s
	return s, nil
}

// begin readies the shared result for one run. The caller holds s.mu.
func (s *session) begin(m *engine.Msg) {
	s.sb.Msg = m
	s.res = Result{ByPort: make([][]*engine.Msg, s.sb.Outputs)}
}

// finish hands back what the run produced. The caller holds s.mu.
func (s *session) finish() *Result {
	out := s.res
	s.res = Result{ByPort: make([][]*engine.Msg, s.sb.Outputs)}
	return &out
}

func (p *Program) runTimed(ctx context.Context, sb Sandbox) (*Result, error) {
	s, err := p.getSession(sb)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begin(sb.Msg)

	timer := time.AfterFunc(p.limits.Timeout, func() { s.vm.Interrupt(ErrTimeout) })
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() { s.vm.Interrupt(ctx.Err()) })
	defer stop()

	out, err := s.mainFn(goja.Undefined(), s.vm.ToValue(sb.Msg.Data), s.nodeObj, s.ctxObj,
		s.flowObj, s.globalObj, s.envObj, s.utilObj)
	s.vm.ClearInterrupt()
	if err != nil {
		return s.finish(), translateError(err)
	}
	if err := p.collectReturn(s.vm, out, &s.sb, &s.res); err != nil {
		return s.finish(), err
	}
	res := s.finish()
	if err := p.checkSize(res); err != nil {
		return res, err
	}
	return res, nil
}

func (p *Program) lifecycleTimed(ctx context.Context, sb Sandbox, which string) (*Result, error) {
	s, err := p.getSession(sb)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var fn goja.Callable
	switch which {
	case "start":
		fn = s.startFn
	case "stop":
		// Nothing may tick during cleanup, or after it.
		s.stopTimersLocked()
		fn = s.stopFn
	}
	if fn == nil {
		return &Result{}, nil
	}
	s.begin(nil)
	timer := time.AfterFunc(p.limits.Timeout, func() { s.vm.Interrupt(ErrTimeout) })
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() { s.vm.Interrupt(ctx.Err()) })
	defer stop()
	_, err = fn(goja.Undefined(), s.nodeObj, s.ctxObj, s.flowObj, s.globalObj, s.envObj, s.utilObj)
	s.vm.ClearInterrupt()
	if err != nil {
		return s.finish(), translateError(err)
	}
	return s.finish(), nil
}

// installTimers defines the four global functions on the dedicated runtime.
func (s *session) installTimers(p *Program) {
	vm := s.vm
	schedule := func(call goja.FunctionCall, every bool) goja.Value {
		fn, ok := goja.AssertFunction(call.Argument(0))
		if !ok {
			e := vm.NewTypeError("The \"callback\" argument must be of type function")
			panic(e)
		}
		d := time.Duration(call.Argument(1).ToFloat()) * time.Millisecond
		if call.Argument(1) == nil || goja.IsUndefined(call.Argument(1)) || d < MinInterval {
			d = MinInterval
		}
		var args []goja.Value
		if len(call.Arguments) > 2 {
			args = append(args, call.Arguments[2:]...)
		}
		if s.closed {
			return goja.Undefined()
		}
		if len(s.timers) >= MaxTimers {
			panic(vm.NewTypeError(fmt.Sprintf("more than %d timers are waiting in this function. "+
				"Clear the ones that are done with clearTimeout or clearInterval", MaxTimers)))
		}
		s.nextID++
		id := s.nextID
		e := &timerEntry{fn: fn, args: args}
		if every {
			e.every = d
		}
		s.timers[id] = e
		e.handle = s.clock.AfterFunc(d, func() { p.fire(s, id) })

		obj := vm.NewObject()
		_ = obj.Set("_id", id)
		noop := func(goja.FunctionCall) goja.Value { return obj }
		_ = obj.Set("unref", noop)
		_ = obj.Set("ref", noop)
		_ = obj.Set("hasRef", func(goja.FunctionCall) goja.Value { return vm.ToValue(true) })
		return obj
	}
	clear := func(call goja.FunctionCall) goja.Value {
		var id int
		switch v := call.Argument(0).Export().(type) {
		case int64:
			id = int(v)
		case float64:
			id = int(v)
		case map[string]any:
			if n, ok := v["_id"].(int64); ok {
				id = int(n)
			}
		}
		if e, ok := s.timers[id]; ok {
			e.canceled = true
			if e.handle != nil {
				e.handle.Stop()
			}
			delete(s.timers, id)
		}
		return goja.Undefined()
	}
	_ = vm.Set("setTimeout", func(c goja.FunctionCall) goja.Value { return schedule(c, false) })
	_ = vm.Set("setInterval", func(c goja.FunctionCall) goja.Value { return schedule(c, true) })
	_ = vm.Set("clearTimeout", clear)
	_ = vm.Set("clearInterval", clear)
}

// fire runs one timer's callback, on whatever goroutine the clock calls it on.
func (p *Program) fire(s *session, id int) {
	s.mu.Lock()
	e := s.timers[id]
	if e == nil || e.canceled || s.closed {
		s.mu.Unlock()
		return
	}
	if e.every > 0 {
		// Rescheduled before the callback runs, so a slow callback does not
		// stretch the interval and clearInterval inside it stops the next one.
		e.handle = s.clock.AfterFunc(e.every, func() { p.fire(s, id) })
	} else {
		delete(s.timers, id)
	}
	s.begin(nil)
	timer := time.AfterFunc(p.limits.Timeout, func() { s.vm.Interrupt(ErrTimeout) })
	_, err := e.fn(goja.Undefined(), e.args...)
	timer.Stop()
	s.vm.ClearInterrupt()
	if err != nil {
		s.res.Errors = append(s.res.Errors, translateError(err).Error())
	}
	res := s.finish()
	s.mu.Unlock()

	p.smu.Lock()
	f := p.async
	p.smu.Unlock()
	if f != nil {
		f(res)
	}
}
