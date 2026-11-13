package js

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// fakeClock is a clock the test moves.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	list []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool { was := !t.stopped && !t.fired; t.stopped = true; return was }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) AfterFunc(d time.Duration, f func()) node.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.list = append(c.list, t)
	return t
}

// advance moves time and fires what is due, in order.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.list {
			if !t.stopped && !t.fired && !t.at.After(target) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		next.fired = true
		c.now = next.at
		c.mu.Unlock()
		next.f()
	}
}

type clockServices struct {
	*services
	clock *fakeClock
}

func (s *clockServices) Clock() node.Clock { return s.clock }

func newClockServices() *clockServices {
	return &clockServices{services: newServices(), clock: &fakeClock{now: time.Unix(1_700_000_000, 0)}}
}

func compileLife(t *testing.T, body, start, stop string, lim Limits) *Program {
	t.Helper()
	p, err := Compile("timers", body, start, stop, lim)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func collect(p *Program) (*[]*Result, *sync.Mutex) {
	var mu sync.Mutex
	var got []*Result
	p.SetAsync(func(r *Result) { mu.Lock(); got = append(got, r); mu.Unlock() })
	return &got, &mu
}

func TestSetIntervalInSetupCodeSendsEveryTickAndStopsWhenStopped(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, "return msg;", `
		let n = 0;
		setInterval(function () { n++; node.send({payload: n}); }, 500);`, "", Limits{})
	got, mu := collect(p)
	sb := Sandbox{Services: svc, Outputs: 1}
	if _, err := p.RunLifecycle(context.Background(), sb, "start"); err != nil {
		t.Fatal(err)
	}
	svc.clock.advance(1600 * time.Millisecond)
	mu.Lock()
	n := len(*got)
	mu.Unlock()
	if n != 3 {
		t.Fatalf("%d ticks in 1.6 s at 500 ms, want 3", n)
	}
	last := (*got)[2].ByPort[0][0]
	if v, _, _ := last.Get("payload"); v != float64(3) {
		t.Fatalf("third payload = %v, want 3 (the counter lives in the setup code's closure)", v)
	}
	if p.TimerCount() != 1 {
		t.Fatalf("TimerCount = %d", p.TimerCount())
	}
	p.Stop()
	svc.clock.advance(5000 * time.Millisecond)
	mu.Lock()
	n = len(*got)
	mu.Unlock()
	if n != 3 || p.TimerCount() != 0 {
		t.Fatalf("after Stop: %d results and %d timers, want 3 and 0", n, p.TimerCount())
	}
}

func TestCleanupRunsAfterTheTimersAreCleared(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, "return msg;",
		`setInterval(function () { node.send({payload: 1}); }, 100);`,
		`flow.set("closed", true);`, Limits{})
	got, mu := collect(p)
	sb := Sandbox{Services: svc, Outputs: 1}
	if _, err := p.RunLifecycle(context.Background(), sb, "start"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RunLifecycle(context.Background(), sb, "stop"); err != nil {
		t.Fatal(err)
	}
	svc.clock.advance(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Fatalf("%d ticks after the node was stopped", len(*got))
	}
	if v, ok, _ := svc.Context(node.ScopeFlow).Get("closed"); !ok || v != true {
		t.Fatal("the cleanup code did not run")
	}
}

func TestSetTimeoutFiresOnceAndClearTimeoutCancels(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, `
		setTimeout(function (a) { node.send({payload: a}); }, 200, "once");
		const t = setTimeout(function () { node.send({payload: "never"}); }, 300);
		clearTimeout(t);
		return null;`, "", "", Limits{})
	got, mu := collect(p)
	m := engine.WrapMsg(map[string]any{"payload": 1})
	if _, err := p.Run(context.Background(), Sandbox{Msg: m, Services: svc, Outputs: 1}); err != nil {
		t.Fatal(err)
	}
	svc.clock.advance(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("%d sends, want 1", len(*got))
	}
	if v, _, _ := (*got)[0].ByPort[0][0].Get("payload"); v != "once" {
		t.Fatalf("payload = %v", v)
	}
}

func TestClearIntervalInsideItsOwnCallbackStopsIt(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, "return msg;", `
		let n = 0;
		const id = setInterval(function () {
			n++; node.send({payload: n});
			if (n === 2) clearInterval(id);
		}, 100);`, "", Limits{})
	got, mu := collect(p)
	if _, err := p.RunLifecycle(context.Background(), Sandbox{Services: svc, Outputs: 1}, "start"); err != nil {
		t.Fatal(err)
	}
	svc.clock.advance(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("%d ticks, want 2", len(*got))
	}
}

func TestATimerCallbackThatSpinsIsStopped(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, "return msg;",
		`setTimeout(function () { while (true) {} }, 10); setTimeout(function () { node.send({payload: "after"}); }, 20);`,
		"", Limits{Timeout: 50 * time.Millisecond})
	got, mu := collect(p)
	if _, err := p.RunLifecycle(context.Background(), Sandbox{Services: svc, Outputs: 1}, "start"); err != nil {
		t.Fatal(err)
	}
	svc.clock.advance(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("%d results, want the failed callback and the one after it", len(*got))
	}
	if len((*got)[0].Errors) != 1 || !strings.Contains((*got)[0].Errors[0], "timed out") {
		t.Fatalf("errors = %v", (*got)[0].Errors)
	}
}

func TestAMessageAndATimerShareTheSameRuntime(t *testing.T) {
	svc := newClockServices()
	p := compileLife(t, `n = (typeof n === "undefined" ? 0 : n) + 1; return {payload: n};`,
		`globalThis.n = 100; setInterval(function () { n++; node.send({payload: "tick" + n}); }, 100);`, "", Limits{})
	got, mu := collect(p)
	if _, err := p.RunLifecycle(context.Background(), Sandbox{Services: svc, Outputs: 1}, "start"); err != nil {
		t.Fatal(err)
	}
	svc.clock.advance(100 * time.Millisecond)
	res, err := p.Run(context.Background(), Sandbox{Msg: engine.WrapMsg(map[string]any{}), Services: svc, Outputs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if v := payloadOf(t, res.ByPort[0][0]); v != float64(102) {
		t.Fatalf("message saw n = %v, want 102 after one tick and one message", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("%d ticks", len(*got))
	}
}

func TestRealClockIntervalTicks(t *testing.T) {
	svc := newServices()
	p := compileLife(t, "return msg;", `setInterval(function () { node.send({payload: 1}); }, 10);`, "", Limits{})
	var n atomic.Int32
	p.SetAsync(func(*Result) { n.Add(1) })
	if _, err := p.RunLifecycle(context.Background(), Sandbox{Services: svc, Outputs: 1}, "start"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	p.Stop()
	if n.Load() < 3 {
		t.Fatalf("only %d ticks on the wall clock", n.Load())
	}
	after := n.Load()
	time.Sleep(60 * time.Millisecond)
	if n.Load() != after {
		t.Fatal("ticks continued after Stop")
	}
}

func TestAFunctionWithNoTimersStillUsesThePool(t *testing.T) {
	p := compile(t, "return msg;", Limits{})
	if p.timed {
		t.Fatal("a function that names no timer must not get a dedicated runtime")
	}
}
