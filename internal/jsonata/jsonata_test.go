package jsonata

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// services is a node's view of the runtime, on the real context stores.
type services struct {
	contexts *store.ScopedContexts
	env      map[string]string
}

func newServices() *services {
	return &services{contexts: store.NewScopedContexts(), env: map[string]string{}}
}

func (s *services) Context(scope node.ContextScope) node.Context {
	switch scope {
	case node.ScopeGlobal:
		return s.contexts.Global()
	case node.ScopeFlow:
		return s.contexts.Flow("line-3")
	default:
		return s.contexts.Node("n1")
	}
}
func (s *services) Credential(string) (string, bool)    { return "", false }
func (s *services) ConfigNode(string) (node.Node, bool) { return nil, false }
func (s *services) Env(name string) (string, bool)      { v, ok := s.env[name]; return v, ok }
func (s *services) Log(node.LogLevel, string, ...any)   {}

func eval(t *testing.T, src string, svc node.Services, m *engine.Msg) (any, bool) {
	t.Helper()
	x, err := Compile(src, svc)
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	v, ok, err := x.EvalMsg(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("eval %q: %v", src, err)
	}
	return v, ok
}

func TestEvaluatesAgainstTheMessage(t *testing.T) {
	m := engine.WrapMsg(map[string]any{
		"payload": map[string]any{"readings": []any{21.5, 22.0, 23.5}},
		"topic":   "line-3/temp",
	})
	if v, _ := eval(t, `$average(payload.readings)`, nil, m); v != 22.333333333333332 {
		t.Errorf("$average = %v", v)
	}
	if v, _ := eval(t, `topic & " ok"`, nil, m); v != "line-3/temp ok" {
		t.Errorf("topic = %v", v)
	}
	if v, ok := eval(t, `payload.nothing`, nil, m); ok || v != nil {
		t.Errorf("a missing path gave %v (defined=%v), want undefined", v, ok)
	}
	if v, ok := eval(t, `null`, nil, m); !ok || v != nil {
		t.Errorf("null gave %v (defined=%v), want a defined null", v, ok)
	}
}

// Expressions written before Node-RED 0.17 name msg explicitly, and plenty of
// imported flows still do. Same test Node-RED uses to tell them apart.
func TestLegacyModeEvaluatesAgainstMsg(t *testing.T) {
	m := engine.NewMsgWithPayload(5.0)
	if v, _ := eval(t, `msg.payload * 2`, nil, m); v != 10.0 {
		t.Errorf("msg.payload * 2 = %v", v)
	}
	if v, _ := eval(t, `payload * 2`, nil, m); v != 10.0 {
		t.Errorf("payload * 2 = %v", v)
	}
	// "msg" inside a string or as part of a longer name is not a reference.
	if v, _ := eval(t, `"msg" & payload`, nil, m); v != "msg5" {
		t.Errorf(`"msg" & payload = %v`, v)
	}
}

func TestNodeREDFunctions(t *testing.T) {
	svc := newServices()
	if err := svc.contexts.Flow("line-3").Set("line", map[string]any{"speed": 42.0}); err != nil {
		t.Fatal(err)
	}
	if err := svc.contexts.Global().Set("site", "Shepherd Boy"); err != nil {
		t.Fatal(err)
	}
	svc.env["LINE"] = "3"
	m := engine.NewMsg()

	if v, _ := eval(t, `$flowContext("line.speed")`, svc, m); v != 42.0 {
		t.Errorf("$flowContext(line.speed) = %v", v)
	}
	if v, _ := eval(t, `$globalContext("site", "memory")`, svc, m); v != "Shepherd Boy" {
		t.Errorf("$globalContext(site) = %v", v)
	}
	if _, ok := eval(t, `$flowContext("never-set")`, svc, m); ok {
		t.Error("a context key that was never set came back defined")
	}
	if v, _ := eval(t, `$env("LINE")`, svc, m); v != "3" {
		t.Errorf("$env(LINE) = %v", v)
	}
	// Node-RED answers "" rather than undefined for an unset variable.
	if v, ok := eval(t, `$env("NOT_SET_ANYWHERE_42")`, svc, m); !ok || v != "" {
		t.Errorf(`$env of an unset variable = %v (defined=%v), want ""`, v, ok)
	}
}

func TestCloneIsADeepCopy(t *testing.T) {
	inner := map[string]any{"a": 1.0}
	m := engine.WrapMsg(map[string]any{"payload": map[string]any{"inner": inner}})
	v, _ := eval(t, `$clone(payload)`, nil, m)
	got := v.(map[string]any)
	got["inner"].(map[string]any)["a"] = 2.0
	if inner["a"] != 1.0 {
		t.Error("$clone shared state with the message")
	}
}

// $moment is moment.js in Node-RED. It is refused by name rather than missing,
// so the error says what to use instead.
func TestMomentIsRefusedWithAnAlternative(t *testing.T) {
	x, err := Compile(`$moment()`, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = x.EvalMsg(context.Background(), engine.NewMsg(), nil)
	if err == nil || !strings.Contains(err.Error(), "$fromMillis") {
		t.Fatalf("err = %v, want a refusal that names an alternative", err)
	}
}

func TestMessageValuesConvert(t *testing.T) {
	when := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	m := engine.WrapMsg(map[string]any{
		"buf":   []byte{1, 2, 255},
		"ibuf":  engine.ImmutableBytes{7},
		"count": 3,
		"at":    when,
		"tags":  []string{"a", "b"},
		"nil":   nil,
		"obj":   map[string]any{"z": 1.0, "a": 2.0, "m": 3.0},
	})
	if v, _ := eval(t, `buf`, nil, m); !reflect.DeepEqual(v, []any{1.0, 2.0, 255.0}) {
		t.Errorf("a buffer converted to %#v, want its byte values", v)
	}
	if v, _ := eval(t, `ibuf[0]`, nil, m); v != 7.0 {
		t.Errorf("an immutable buffer converted to %#v", v)
	}
	if v, _ := eval(t, `count + 1`, nil, m); v != 4.0 {
		t.Errorf("an int converted to %#v", v)
	}
	if v, _ := eval(t, `at`, nil, m); v != "2026-10-02T06:00:00.000Z" {
		t.Errorf("a time converted to %#v", v)
	}
	if v, _ := eval(t, `$count(tags)`, nil, m); v != 2.0 {
		t.Errorf("a []string converted badly: %#v", v)
	}
	if v, ok := eval(t, `nil`, nil, m); !ok || v != nil {
		t.Errorf("a nil property gave %v (defined=%v), want a defined null", v, ok)
	}
	// A message map has no order. It reaches the expression sorted, so the
	// answer is the same every time instead of whatever the map iterated.
	for range 20 {
		if v, _ := eval(t, `$keys(obj)`, nil, m); !reflect.DeepEqual(v, []any{"a", "m", "z"}) {
			t.Fatalf("$keys = %v, want sorted", v)
		}
	}
}

// A runaway expression is cut off rather than holding the node's goroutine,
// and with it the node's whole inbox, forever.
func TestEvaluationIsBounded(t *testing.T) {
	defer func(d time.Duration) { Timeout = d }(Timeout)
	Timeout = 20 * time.Millisecond

	x, err := Compile(`$sum([1..5000000].($ * 2))`, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err = x.Eval(context.Background(), nil, nil)
	if ErrorCode(err) != "D1012" {
		t.Fatalf("err = %v, want the D1012 timeout", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the timeout took %s to fire", took)
	}
}

func TestCompileErrorsSayWhatAndWhere(t *testing.T) {
	_, err := Compile(`payload.(`, nil)
	if err == nil {
		t.Fatal("a broken expression compiled")
	}
	if !strings.Contains(err.Error(), "payload.(") || ErrorCode(err) == "" {
		t.Errorf("err = %v, want the expression and a JSONata error code", err)
	}
}
