package js

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// services hands a function the real in-memory context stores, one per scope,
// and a fixed environment. It is the same contract the runtime fulfils.
type services struct {
	scopes map[node.ContextScope]node.Context
	env    map[string]string
}

func newServices() *services {
	return &services{
		scopes: map[node.ContextScope]node.Context{
			node.ScopeNode:   store.NewMemoryContext(),
			node.ScopeFlow:   store.NewMemoryContext(),
			node.ScopeGlobal: store.NewMemoryContext(),
		},
		env: map[string]string{"LINE": "3"},
	}
}

func (s *services) Context(scope node.ContextScope) node.Context { return s.scopes[scope] }
func (s *services) Credential(string) (string, bool)             { return "", false }
func (s *services) ConfigNode(string) (node.Node, bool)          { return nil, false }
func (s *services) Env(name string) (string, bool) {
	v, ok := s.env[name]
	return v, ok
}
func (s *services) Log(node.LogLevel, string, ...any) {}

func compile(t *testing.T, body string, limits Limits) *Program {
	t.Helper()
	p, err := Compile("test", body, "", "", limits)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p
}

func run(t *testing.T, p *Program, payload any, outputs int, svc node.Services) (*Result, error) {
	t.Helper()
	m := engine.WrapMsg(map[string]any{"payload": payload})
	return p.Run(context.Background(), Sandbox{Msg: m, Services: svc, Outputs: outputs})
}

func payloadOf(t *testing.T, m *engine.Msg) any {
	t.Helper()
	v, _, err := m.Get("payload")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReturnedObjectGoesToTheFirstPort(t *testing.T) {
	p := compile(t, "msg.payload = msg.payload * 2; return msg;", Limits{})
	res, err := run(t, p, 21.0, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ByPort[0]) != 1 || payloadOf(t, res.ByPort[0][0]) != 42.0 {
		t.Fatalf("port 0 = %v", res.ByPort[0])
	}
}

// An array indexes ports, null skips one, and a nested array sends several
// messages on one port.
func TestReturnedArrayRoutesByPort(t *testing.T) {
	p := compile(t, `return [null, [{payload: "a"}, {payload: "b"}], {payload: "c"}];`, Limits{})
	res, err := run(t, p, nil, 3, newServices())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ByPort[0]) != 0 {
		t.Errorf("port 0 got %d messages, want none", len(res.ByPort[0]))
	}
	if len(res.ByPort[1]) != 2 || payloadOf(t, res.ByPort[1][0]) != "a" || payloadOf(t, res.ByPort[1][1]) != "b" {
		t.Errorf("port 1 = %v, want a then b", res.ByPort[1])
	}
	if len(res.ByPort[2]) != 1 || payloadOf(t, res.ByPort[2][0]) != "c" {
		t.Errorf("port 2 = %v, want c", res.ByPort[2])
	}
}

// A function that builds a fresh object still carries the message id, or the
// debug sidebar can't tie what came out to what went in.
func TestFreshObjectKeepsTheMessageID(t *testing.T) {
	p := compile(t, `return {payload: "new"};`, Limits{})
	m := engine.WrapMsg(map[string]any{"payload": 1.0})
	m.EnsureID()
	res, err := p.Run(context.Background(), Sandbox{Msg: m, Services: newServices(), Outputs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.ByPort[0][0].ID(); got != m.ID() || got == "" {
		t.Fatalf("id = %q, want %q", got, m.ID())
	}
}

func TestReturningAScalarIsAnError(t *testing.T) {
	for _, body := range []string{`return 5;`, `return "five";`, `return true;`} {
		p := compile(t, body, Limits{})
		if _, err := run(t, p, nil, 1, newServices()); err == nil {
			t.Errorf("%s: no error", body)
		}
	}
}

// Integers come out of goja as int64. Everywhere else in the engine a number
// is a float64, so a payload of 42 has to be 42.0 by the time it leaves.
func TestNumbersLeaveAsFloat64(t *testing.T) {
	p := compile(t, `return {payload: 42, nested: {list: [1, 2]}};`, Limits{})
	res, err := run(t, p, nil, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	m := res.ByPort[0][0]
	if v, ok := payloadOf(t, m).(float64); !ok || v != 42 {
		t.Fatalf("payload is %T %v, want float64 42", payloadOf(t, m), payloadOf(t, m))
	}
	list, _, _ := m.Get("nested.list")
	if l, ok := list.([]any); !ok || len(l) != 2 {
		t.Fatalf("nested.list = %#v", list)
	} else if _, ok := l[0].(float64); !ok {
		t.Fatalf("nested.list[0] is %T, want float64", l[0])
	}
}

// The timeout is the only thing standing between a typo'd loop and a core
// spinning until the pod dies.
func TestInfiniteLoopIsStoppedAtTheTimeout(t *testing.T) {
	p := compile(t, `if (msg.payload === "loop") { while (true) {} } return msg;`,
		Limits{Timeout: 100 * time.Millisecond})
	start := time.Now()
	_, err := run(t, p, "loop", 1, newServices())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %v to stop a loop with a 100ms limit", took)
	}

	// The runtime goes back in the pool. The next message must not inherit
	// the interrupt and fail for no reason.
	if _, err := run(t, p, "fine", 1, newServices()); err != nil {
		t.Fatalf("the pooled runtime is still interrupted: %v", err)
	}
}

// A deploy cancels the context of everything in flight, and a function stuck
// in a loop has to let go when that happens rather than at its own timeout.
func TestCancelledContextStopsAFunction(t *testing.T) {
	p := compile(t, `while (true) {}`, Limits{Timeout: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := p.Run(ctx, Sandbox{Msg: engine.NewMsg(), Services: newServices(), Outputs: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v to notice the cancel", took)
	}
}

func TestOutputCeiling(t *testing.T) {
	p := compile(t, `return {payload: "x".repeat(4096)};`, Limits{MaxOutputBytes: 1024})
	_, err := run(t, p, nil, 1, newServices())
	if err == nil || !strings.Contains(err.Error(), "more than 1024 bytes") {
		t.Fatalf("err = %v, want the output ceiling", err)
	}
}

// Nothing from the host is reachable. These are the names a function would
// reach for to get out, and the constructor walk is the classic way out of a
// JavaScript sandbox that hands in a host object.
func TestNoHostBindings(t *testing.T) {
	p := compile(t, `return {payload: [
		typeof require, typeof process, typeof Buffer, typeof module,
		typeof setTimeout, typeof fetch, typeof globalThis.process
	].join(",")};`, Limits{})
	res, err := run(t, p, nil, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	want := "undefined,undefined,undefined,undefined,undefined,undefined,undefined"
	if got := payloadOf(t, res.ByPort[0][0]); got != want {
		t.Fatalf("typeof the host = %v", got)
	}

	escape := compile(t, `
		const F = node.send.constructor.constructor;
		const g = F("return this")();
		return {payload: typeof g.process + "," + typeof g.require};`, Limits{})
	res, err = run(t, escape, nil, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	if got := payloadOf(t, res.ByPort[0][0]); got != "undefined,undefined" {
		t.Fatalf("the constructor walk reached %v", got)
	}
}

func TestSyntaxErrorPointsAtTheAuthorsCode(t *testing.T) {
	_, err := Compile("test", "return msg +;", "", "", Limits{})
	if err == nil {
		t.Fatal("compiled a syntax error")
	}
	if strings.Contains(err.Error(), "function(msg, node") {
		t.Fatalf("the error shows the wrapper the author never wrote: %v", err)
	}
}

func TestNodeAPI(t *testing.T) {
	p := compile(t, `
		node.status({fill: "green", shape: "dot", text: 7});
		node.warn("careful", 2);
		node.error("bad reading");
		node.send({payload: "early"});
		node.done();
		return null;`, Limits{})
	res, err := run(t, p, nil, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == nil || res.Status.Fill != "green" || res.Status.Text != "7" {
		t.Errorf("status = %+v", res.Status)
	}
	if len(res.Logs) != 1 || res.Logs[0].Level != node.LogWarn || res.Logs[0].Message != "careful 2" {
		t.Errorf("logs = %+v", res.Logs)
	}
	if len(res.Errors) != 1 || res.Errors[0] != "bad reading" {
		t.Errorf("errors = %v", res.Errors)
	}
	if len(res.ByPort[0]) != 1 || payloadOf(t, res.ByPort[0][0]) != "early" {
		t.Errorf("node.send = %v", res.ByPort[0])
	}
	if !res.Done {
		t.Error("node.done() was not recorded")
	}
}

func TestContextScopesAreSeparate(t *testing.T) {
	svc := newServices()
	p := compile(t, `
		context.set("k", "node"); flow.set("k", "flow"); global.set("k", "global");
		return {payload: [context.get("k"), flow.get("k"), global.get("k"), env.get("LINE"), typeof env.get("NOPE")].join(",")};`, Limits{})
	res, err := run(t, p, nil, 1, svc)
	if err != nil {
		t.Fatal(err)
	}
	if got := payloadOf(t, res.ByPort[0][0]); got != "node,flow,global,3,undefined" {
		t.Fatalf("got %v", got)
	}
	if v, _, _ := svc.scopes[node.ScopeFlow].Get("k"); v != "flow" {
		t.Fatalf("flow context holds %v", v)
	}

	del := compile(t, `flow.set("k"); return {payload: typeof flow.get("k")};`, Limits{})
	res, err = run(t, del, nil, 1, svc)
	if err != nil {
		t.Fatal(err)
	}
	if got := payloadOf(t, res.ByPort[0][0]); got != "undefined" {
		t.Fatalf("set with no value left %v behind", got)
	}
}

// incr and cas are the reason a shared counter in a Function node can be
// right. Hammer one counter from many goroutines through the JavaScript API
// and count.
func TestIncrementFromJavaScriptLosesNothing(t *testing.T) {
	svc := newServices()
	p := compile(t, `global.incr("count"); return null;`, Limits{})
	const workers, each = 8, 250
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := p.Run(context.Background(), Sandbox{Msg: engine.NewMsg(), Services: svc, Outputs: 1}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	v, _, _ := svc.scopes[node.ScopeGlobal].Get("count")
	if v != float64(workers*each) {
		t.Fatalf("count = %v, want %d", v, workers*each)
	}

	cas := compile(t, `return {payload: [global.cas("count", 1, 2), global.cas("count", 2000, 1)]};`, Limits{})
	res, err := run(t, cas, nil, 1, svc)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := payloadOf(t, res.ByPort[0][0]).([]any)
	if len(got) != 2 || got[0] != false || got[1] != true {
		t.Fatalf("cas results = %v, want [false true]", got)
	}
}

func TestUtil(t *testing.T) {
	p := compile(t, `
		const c = util.cloneMessage(msg); c.payload.v = 2;
		return {payload: [
			msg.payload.v, c.payload.v,
			util.base64Encode("line 3"), util.base64Decode("bGluZSAz"), util.hexEncode("AB"),
			util.getProperty({a: {b: 5}}, "a.b"),
			util.setProperty({}, "x.y", 1).x.y,
			typeof util.generateId()
		].join(",")};`, Limits{})
	res, err := run(t, p, map[string]any{"v": 1.0}, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	if got := payloadOf(t, res.ByPort[0][0]); got != "1,2,bGluZSAz,line 3,4142,5,1,string" {
		t.Fatalf("got %v", got)
	}
}

func TestLifecycleCode(t *testing.T) {
	svc := newServices()
	p, err := Compile("test", "return msg;", `flow.set("started", true);`, `flow.set("stopped", true);`, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"start", "stop"} {
		if _, err := p.RunLifecycle(context.Background(), Sandbox{Services: svc, Outputs: 1}, which); err != nil {
			t.Fatalf("%s: %v", which, err)
		}
	}
	for _, k := range []string{"started", "stopped"} {
		if v, _, _ := svc.scopes[node.ScopeFlow].Get(k); v != true {
			t.Errorf("%s = %v after the lifecycle code ran", k, v)
		}
	}
	if _, err := Compile("test", "return msg;", "this is not javascript", "", Limits{}); err == nil {
		t.Error("compiled setup code that is not JavaScript")
	}
}

func TestThrownErrorKeepsItsMessage(t *testing.T) {
	p := compile(t, `throw new Error("sensor 4 offline");`, Limits{})
	_, err := run(t, p, nil, 1, newServices())
	if err == nil || !strings.Contains(err.Error(), "sensor 4 offline") {
		t.Fatalf("err = %v", err)
	}
}
