package js

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// What Node-RED's function node does with a promise, read from
// packages/node_modules/@node-red/nodes/core/function/10-function.js at 5.0.10:
// the body is an async function, a promise the Setup code returns is waited for
// before the first message runs, and node.linkcall returns a promise.

func TestAwaitAtTheTopLevelOfTheBodyReturnsTheMessage(t *testing.T) {
	p := compile(t, `const v = await Promise.resolve(41); msg.payload = v + 1; return msg;`, Limits{})
	if !p.UsesPromises() {
		t.Fatal("a body that awaits must run on the promise runtime")
	}
	m := engine.NewMsg()
	res, err := p.Run(context.Background(), Sandbox{Msg: m, Services: newServices(), Outputs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ByPort[0]) != 1 {
		t.Fatalf("%d messages out, want 1", len(res.ByPort[0]))
	}
	if v, _, _ := res.ByPort[0][0].Get("payload"); v != float64(42) {
		t.Fatalf("payload = %v, want 42", v)
	}
}

func TestARejectedPromiseFromTheBodyIsAnErrorWithItsMessage(t *testing.T) {
	p := compile(t, `await Promise.reject(new Error("nope")); return msg;`, Limits{})
	_, err := p.Run(context.Background(), Sandbox{Msg: engine.NewMsg(), Services: newServices(), Outputs: 1})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want one saying nope", err)
	}
}

func TestAFunctionWithNoPromisesKeepsThePool(t *testing.T) {
	p := compile(t, `return msg;`, Limits{})
	if p.UsesPromises() || p.timed {
		t.Fatal("a function that uses no promise must stay on the pooled runtimes")
	}
}

func TestSetupCodeThatReturnsAPromiseHoldsTheFirstMessageUntilItSettles(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, `msg.payload = global.get("message"); return msg;`, `
		global.set("message", "Initializing, World!");
		return new Promise((resolve) => {
			setTimeout(() => { global.set("message", "Hello, World!"); resolve(); }, 1000);
		});`, "", Limits{})
	collect(p)
	if _, err := p.RunLifecycle(context.Background(), Sandbox{Services: svc, Outputs: 1}, "start"); err != nil {
		t.Fatal(err)
	}
	type out struct {
		res *Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := p.Run(context.Background(), Sandbox{Msg: engine.NewMsg(), Services: svc, Outputs: 1})
		done <- out{r, err}
	}()
	select {
	case <-done:
		t.Fatal("the message ran before the setup promise settled")
	case <-time.After(150 * time.Millisecond):
	}
	svc.clock.advance(1000 * time.Millisecond)
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatal(o.err)
		}
		if v, _, _ := o.res.ByPort[0][0].Get("payload"); v != "Hello, World!" {
			t.Fatalf("payload = %v, want the value the setup promise set", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the message never ran after the setup promise settled")
	}
}

func TestSetupCodeThatRejectsIsReportedAndEveryMessageGetsThatError(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, `return msg;`, `
		return new Promise((_, reject) => { setTimeout(() => reject(new Error("no broker")), 10); });`, "", Limits{})
	got, mu := collect(p)
	if _, err := p.RunLifecycle(context.Background(), Sandbox{Services: svc, Outputs: 1}, "start"); err != nil {
		t.Fatal(err)
	}
	svc.clock.advance(20 * time.Millisecond)
	mu.Lock()
	reported := len(*got) == 1 && len((*got)[0].Errors) == 1 && strings.Contains((*got)[0].Errors[0], "no broker")
	mu.Unlock()
	if !reported {
		t.Fatalf("the rejection was not reported once: %v", *got)
	}
	_, err := p.Run(context.Background(), Sandbox{Msg: engine.NewMsg(), Services: svc, Outputs: 1})
	if err == nil || !strings.Contains(err.Error(), "no broker") {
		t.Fatalf("a message after a failed setup got %v, want the setup's error", err)
	}
}

func TestABodyAwaitingATimerSendsItsMessageWhenTheTimerFires(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, `
		await new Promise((r) => setTimeout(r, 500));
		msg.payload = "late";
		return msg;`, "", "", Limits{})
	got, mu := collect(p)
	res, err := p.Run(context.Background(), Sandbox{Msg: engine.NewMsg(), Services: svc, Outputs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ByPort[0]) != 0 {
		t.Fatal("the message went out before the timer fired")
	}
	svc.clock.advance(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 || len((*got)[0].ByPort[0]) != 1 {
		t.Fatalf("%d async results after the timer, want one carrying the message", len(*got))
	}
	if v, _, _ := (*got)[0].ByPort[0][0].Get("payload"); v != "late" {
		t.Fatalf("payload = %v", v)
	}
}

// linkStub stands in for the Link In the call reaches: it keeps the calls and
// answers them when the test says so.
type linkStub struct {
	mu      sync.Mutex
	targets []string
	msgs    []*engine.Msg
	deliver []func(*engine.Msg, error)
	refuse  error
}

func (l *linkStub) call(target string, m *engine.Msg, _ time.Duration, d func(*engine.Msg, error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refuse != nil {
		return l.refuse
	}
	l.targets = append(l.targets, target)
	l.msgs = append(l.msgs, m)
	l.deliver = append(l.deliver, d)
	return nil
}

func TestLinkcallResolvesWithTheMessageThatComesBack(t *testing.T) {
	svc := newClockServices()
	stub := &linkStub{}
	p := compileLife(t, `
		const m = await node.linkcall('subroutine-1', msg, { timeout: 1000 });
		return m;`, "", "", Limits{})
	got, mu := collect(p)
	msg := engine.NewMsg()
	_ = msg.Set("payload", "in")
	res, err := p.Run(context.Background(), Sandbox{Msg: msg, Services: svc, Outputs: 1, LinkCall: stub.call})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ByPort[0]) != 0 {
		t.Fatal("the function returned before the call came back")
	}
	if len(stub.targets) != 1 || stub.targets[0] != "subroutine-1" {
		t.Fatalf("calls = %v", stub.targets)
	}
	if v, _, _ := stub.msgs[0].Get("payload"); v != "in" {
		t.Fatalf("the callee got payload %v", v)
	}
	if &stub.msgs[0].Data == &msg.Data || stub.msgs[0] == msg {
		t.Fatal("the callee got the caller's own message, not a copy (clone defaults to true)")
	}
	back := engine.NewMsg()
	_ = back.Set("payload", "Hello, from subroutine-1")
	stub.deliver[0](back, nil)
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 || len((*got)[0].ByPort[0]) != 1 {
		t.Fatalf("%d results after the return, want the returned message sent on", len(*got))
	}
	if v, _, _ := (*got)[0].ByPort[0][0].Get("payload"); v != "Hello, from subroutine-1" {
		t.Fatalf("payload = %v", v)
	}
}

func TestLinkcallRejectsOnTimeoutAndWhenTheTargetIsNotThere(t *testing.T) {
	svc := newClockServices()
	stub := &linkStub{}
	p := compileLife(t, `
		try { await node.linkcall('x', msg); } catch (e) { msg.err = e.message; }
		return msg;`, "", "", Limits{})
	got, mu := collect(p)
	if _, err := p.Run(context.Background(), Sandbox{Msg: engine.NewMsg(), Services: svc, Outputs: 1, LinkCall: stub.call}); err != nil {
		t.Fatal(err)
	}
	stub.deliver[0](nil, errors.New("timeout"))
	mu.Lock()
	if len(*got) != 1 {
		mu.Unlock()
		t.Fatal("no result after the timeout")
	}
	if v, _, _ := (*got)[0].ByPort[0][0].Get("err"); v != "timeout" {
		mu.Unlock()
		t.Fatalf("err = %v, want timeout", v)
	}
	mu.Unlock()

	stub.refuse = errors.New(`target link in node "x" is not running`)
	res, err := p.Run(context.Background(), Sandbox{Msg: engine.NewMsg(), Services: svc, Outputs: 1, LinkCall: stub.call})
	if err != nil {
		t.Fatal(err)
	}
	if v, _, _ := res.ByPort[0][0].Get("err"); v != `target link in node "x" is not running` {
		t.Fatalf("err = %v", v)
	}
}

func TestLinkcallWantsATargetAndAMessage(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, `
		const out = [];
		for (const args of [[], ['a'], ['a', 5]]) {
			try { await node.linkcall(...args); out.push("ok"); } catch (e) { out.push(e.message); }
		}
		msg.payload = out;
		return msg;`, "", "", Limits{})
	res, err := p.Run(context.Background(), Sandbox{Msg: engine.NewMsg(), Services: svc, Outputs: 1, LinkCall: (&linkStub{}).call})
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ := res.ByPort[0][0].Get("payload")
	list, _ := v.([]any)
	if len(list) != 3 {
		t.Fatalf("payload = %v", v)
	}
	for i, e := range list {
		if s, _ := e.(string); s == "ok" || s == "" {
			t.Fatalf("call %d was accepted: %v", i, list)
		}
	}
}
