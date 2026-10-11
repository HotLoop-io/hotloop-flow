package flowtest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// TestADebugMessageBecomesATestThatPasses: the draft made from what the flow
// did, run against the same flow, passes, and run against a changed flow,
// fails at the Debug node.
func TestADebugMessageBecomesATestThatPasses(t *testing.T) {
	captured := []runtime.Captured{
		{NodeID: "reading", Port: 0, MsgID: "m1", Data: map[string]any{"_msgid": "m1", "payload": 92.5, "topic": "line3/temp", "res": struct{}{}}},
		{NodeID: "alarm-out", Port: -1, MsgID: "m1", Data: map[string]any{"_msgid": "m1", "payload": "HIGH", "topic": "line3/temp"}},
	}
	c, err := flowtest.FromDebug([]byte(line3), "alarm-out", captured)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != `alarm out gets "HIGH" from reading` {
		t.Errorf("name %q", c.Name)
	}
	if _, kept := c.Inject[0].Msg["res"]; kept {
		t.Error("a value no test file can say went into the draft")
	}
	text, err := flowtest.CaseYAML(c)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, one(t, line3, text), flowtest.Pass)

	changed := strings.Replace(line3, `"to":"HIGH"`, `"to":"ALARM"`, 1)
	r := one(t, changed, text)
	wantStatus(t, r, flowtest.Fail)
	if r.Problems[0].Node != "alarm-out" {
		t.Errorf("the failure should be on the Debug node: %+v", r.Problems)
	}
}

func TestADebugMessageThatCantBeATest(t *testing.T) {
	in := runtime.Captured{NodeID: "reading", Port: 0, MsgID: "m1", Data: map[string]any{"payload": 1.0}}
	out := runtime.Captured{NodeID: "fine", Port: -1, MsgID: "m1", Data: map[string]any{"payload": 1.0}}
	for name, tc := range map[string]struct {
		node string
		caps []runtime.Captured
		says string
	}{
		"pushed out of the ring": {"fine", []runtime.Captured{out}, flowtest.ErrNotCaptured.Error()},
		"not a Debug node":       {"chk", []runtime.Captured{in, out}, "is a switch, not a Debug node"},
		"no such node":           {"ghost", []runtime.Captured{in, out}, `there's no node "ghost"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := flowtest.FromDebug([]byte(line3), tc.node, tc.caps)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("got %v, want %q", err, tc.says)
			}
		})
	}
	_, err := flowtest.FromDebug([]byte(line3), "fine", []runtime.Captured{out})
	if !errors.Is(err, flowtest.ErrNotCaptured) {
		t.Errorf("a message no longer kept should say so: %v", err)
	}
}

// TestAppendingKeepsWhatSomebodyWrote: a test added from the editor goes on
// the end of the suite, the comments and the order of what's there stay, and
// a name the suite already has gets a number rather than replacing anything.
func TestAppendingKeepsWhatSomebodyWrote(t *testing.T) {
	suite := []byte(`# line 3's tests, reviewed with the flows
flows: line3.json
tests:
  # the one that matters
  - name: a high reading
    inject: [{node: reading, msg: {payload: 92.5}}]
    expect: [{node: alarm out, msg: {payload: HIGH}}]
`)
	add := flowtest.Case{Name: "a high reading", Expect: []flowtest.Expectation{{Node: "fine", Nothing: true}}}
	out, name, err := flowtest.AppendCase(suite, add)
	if err != nil {
		t.Fatal(err)
	}
	if name != "a high reading (2)" {
		t.Errorf("name %q", name)
	}
	text := string(out)
	for _, want := range []string{"# line 3's tests, reviewed with the flows", "# the one that matters", "flows: line3.json", "name: a high reading (2)"} {
		if !strings.Contains(text, want) {
			t.Errorf("lost %q:\n%s", want, text)
		}
	}
	s, err := flowtest.Parse(out)
	if err != nil || len(s.Tests) != 2 || s.Tests[0].Name != "a high reading" {
		t.Fatalf("the suite after: %v %+v", err, s)
	}

	fresh, _, err := flowtest.AppendCase(nil, add)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := flowtest.Parse(fresh); err != nil || len(s.Tests) != 1 {
		t.Fatalf("a first test into an empty suite: %v\n%s", err, fresh)
	}
}
