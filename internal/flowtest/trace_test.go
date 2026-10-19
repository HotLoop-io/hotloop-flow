package flowtest_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// fan is the flow tracing exists for: one reading is split into three, each is
// changed, a join puts them back together, a subflow handles the result, and a
// Debug node shows it. A Switch off to the side drops the message into a second
// Debug node when it matches, so there's a branch the message does not take.
const fan = `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"go","type":"inject","z":"t1","name":"go","props":[{"p":"payload","v":"[1,2,3]","vt":"json"}],"x":1,"y":1,"wires":[["split"]]},
  {"id":"split","type":"split","z":"t1","name":"split","x":2,"y":1,"wires":[["bump"]]},
  {"id":"bump","type":"change","z":"t1","name":"bump","rules":[{"t":"set","p":"seen","pt":"msg","to":"yes","tot":"str"}],"x":3,"y":1,"wires":[["join"]]},
  {"id":"join","type":"join","z":"t1","name":"join","mode":"custom","build":"array","count":"3","timeout":"30","x":4,"y":1,"wires":[["inst"]]},
  {"id":"sf","type":"subflow","name":"stamp","in":[{"x":0,"y":0,"wires":[{"id":"mark"}]}],"out":[{"x":0,"y":0,"wires":[{"id":"mark","port":0}]}]},
  {"id":"mark","type":"change","z":"sf","name":"mark","rules":[{"t":"set","p":"stamped","pt":"msg","to":"true","tot":"bool"}],"x":1,"y":1,"wires":[[]]},
  {"id":"inst","type":"subflow:sf","z":"t1","name":"stamper","x":5,"y":1,"wires":[["end"]]},
  {"id":"end","type":"debug","z":"t1","name":"end","complete":"true","x":6,"y":1,"wires":[]}
]`

// traced runs one injection through a flow with a tracer on and gives the
// tracer back.
func traced(t *testing.T, flows, node string, msg map[string]any) *engine.Tracer {
	t.Helper()
	tracer := engine.NewTracer(engine.TraceLimits{})
	s := &flowtest.Suite{Tests: []flowtest.Case{{
		Name:   "trace",
		Inject: []flowtest.Injection{{Node: node, Msg: msg}},
	}}}
	rep := flowtest.Run(context.Background(), []byte(flows), s, flowtest.Options{Tracer: tracer})
	if len(rep.Tests) != 1 || rep.Tests[0].Status != flowtest.Pass {
		t.Fatalf("the run didn't pass: %+v", rep)
	}
	return tracer
}

func only(t *testing.T, tracer *engine.Tracer) engine.TraceSnapshot {
	t.Helper()
	all := tracer.Traces()
	if len(all) != 1 {
		t.Fatalf("got %d traces, want 1", len(all))
	}
	return all[0].Snapshot()
}

func nodesOf(s engine.TraceSnapshot, ids []int) []string {
	var out []string
	for _, id := range ids {
		out = append(out, s.Hops[id].Node)
	}
	return out
}

func count(s engine.TraceSnapshot, node string) int {
	n := 0
	for _, h := range s.Hops {
		if h.Node == node {
			n++
		}
	}
	return n
}

// TestTraceFollowsASplitAJoinAndASubflow is the test that fails without
// tracing: the path to the Debug node has to run through every piece of the
// split, all three arrivals at the join that made one message of them, and the
// node inside the subflow.
func TestTraceFollowsASplitAJoinAndASubflow(t *testing.T) {
	s := only(t, traced(t, fan, "go", map[string]any{"payload": []any{1.0, 2.0, 3.0}}))

	for node, want := range map[string]int{"go": 1, "split": 1, "bump": 3, "join": 3, "inst": 1, "inst:mark": 1, "end": 1} {
		if got := count(s, node); got != want {
			t.Errorf("%s was handled %d time(s) in the trace, want %d\n%s", node, got, want, dump(s))
		}
	}

	var end engine.Hop
	for _, h := range s.Hops {
		if h.Node == "end" {
			end = h
		}
	}
	path := nodesOf(s, s.PathTo(end.ID))
	for _, want := range []string{"go", "split", "bump", "join", "inst", "inst:mark", "end"} {
		if !slices.Contains(path, want) {
			t.Errorf("the path to end doesn't go through %s: %v", want, path)
		}
	}
	// The path takes in every piece of the split and every arrival at the
	// join, not just the one that happened to be last.
	pathCount := map[string]int{}
	for _, n := range path {
		pathCount[n]++
	}
	if pathCount["bump"] != 3 || pathCount["join"] != 3 {
		t.Errorf("the path has %d bump and %d join hops, want 3 and 3\n%s", pathCount["bump"], pathCount["join"], dump(s))
	}

	// Timing and the message are on every hop.
	for _, h := range s.Hops {
		if !h.Done {
			t.Errorf("hop %d (%s) never finished", h.ID, h.Node)
		}
		if h.Nanos < 0 || h.At.IsZero() {
			t.Errorf("hop %d (%s) has no timing: %d ns at %v", h.ID, h.Node, h.Nanos, h.At)
		}
		if h.Msg == nil {
			t.Errorf("hop %d (%s) kept no message", h.ID, h.Node)
		}
	}
	// What the Debug node was handed is what the hop says it was.
	if end.Msg["stamped"] != true {
		t.Errorf("the message at end was %v, want stamped:true", end.Msg)
	}
	if end.Port != 0 || len(end.Parents) != 1 {
		t.Errorf("end came by port %d from %v, want port 0 from one hop", end.Port, end.Parents)
	}
}

func dump(s engine.TraceSnapshot) string {
	var b strings.Builder
	for _, h := range s.Hops {
		fmt.Fprintf(&b, "  %d %s parents=%v port=%d sent=%d entry=%q\n", h.ID, h.Node, h.Parents, h.Port, h.Sent, h.Entry)
	}
	return b.String()
}

// TestAJoinHopHasEveryPieceItWasMadeFrom: the hop after a join is the child of
// all the arrivals, which is what lights the three branches on the canvas.
func TestAJoinHopHasEveryPieceItWasMadeFrom(t *testing.T) {
	s := only(t, traced(t, fan, "go", map[string]any{"payload": []any{1.0, 2.0, 3.0}}))
	for _, h := range s.Hops {
		if h.Node != "inst" {
			continue
		}
		joins := 0
		for _, p := range h.Parents {
			if s.Hops[p].Node == "join" {
				joins++
			}
		}
		if joins != 3 {
			t.Fatalf("the subflow hop came from %d join hops, want 3\n%s", joins, dump(s))
		}
		return
	}
	t.Fatal("no subflow hop in the trace")
}

// TestAnErrorReachesTheCatchOnTheTrace: a node that fails is a hop with the
// error on it, and the Catch that gets it is the next hop, reached "via error".
func TestAnErrorReachesTheCatchOnTheTrace(t *testing.T) {
	s := only(t, traced(t, line3, "reading", map[string]any{"payload": -5.0, "topic": "line3/temp"}))
	var failed, caught *engine.Hop
	for i := range s.Hops {
		h := &s.Hops[i]
		if h.Node == "sanity" {
			failed = h
		}
		if h.Node == "errors" {
			caught = h
		}
	}
	if failed == nil || caught == nil {
		t.Fatalf("expected sanity and errors in the trace\n%s", dump(s))
	}
	if !strings.Contains(failed.Error, "reading out of range") {
		t.Errorf("the failing hop's error is %q", failed.Error)
	}
	if caught.Via != "error" || !slices.Contains(caught.Parents, failed.ID) {
		t.Errorf("the Catch hop came via %q from %v, want error from %d", caught.Via, caught.Parents, failed.ID)
	}
}

// TestTracingOffChangesNothing: no tracer, no tags, no cost to speak of.
func TestTracingOffChangesNothing(t *testing.T) {
	r := one(t, fan, `
tests:
  - name: same
    inject: [{node: go, msg: {payload: [1, 2, 3]}}]
    expect:
      - {node: end, msg: {payload: [1, 2, 3], stamped: true}}
`)
	wantStatus(t, r, flowtest.Pass)
}

// TestATraceBecomesATestThatPassesAndThatFails: the generated test is a real
// Phase 2 test. Against the flow it came from it passes; against a flow that
// does something else it fails, and says where.
func TestATraceBecomesATestThatPassesAndThatFails(t *testing.T) {
	snap := only(t, traced(t, fan, "go", map[string]any{"payload": []any{1.0, 2.0, 3.0}}))
	c, err := flowtest.FromTrace([]byte(fan), snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Inject) != 1 || c.Inject[0].Node != "go" || len(c.Expect) != 1 || c.Expect[0].Node != "end" {
		t.Fatalf("the test is %+v", c)
	}
	yaml, err := flowtest.CaseYAML(c)
	if err != nil {
		t.Fatal(err)
	}

	pass := one(t, fan, yaml)
	wantStatus(t, pass, flowtest.Pass)

	changed := strings.Replace(fan, `"to":"true","tot":"bool"`, `"to":"false","tot":"bool"`, 1)
	fail := one(t, changed, yaml)
	wantStatus(t, fail, flowtest.Fail)
	if !strings.Contains(problems(fail), "stamped") {
		t.Errorf("the failure doesn't say what was different:\n%s", problems(fail))
	}
}

// TestATraceWithNothingToWatchIsRefused: a message a Switch dropped ends
// nowhere a test can watch, and saying so beats writing a test that checks
// nothing, because a test with nothing to check passes.
func TestATraceWithNothingToWatchIsRefused(t *testing.T) {
	flows := strings.Replace(line3, `"wires":[["alarm"],["fine"]]`, `"wires":[[],[]]`, 1)
	snap := only(t, traced(t, flows, "reading", map[string]any{"payload": 50.0}))
	if _, err := flowtest.FromTrace([]byte(flows), snap); err == nil || err != flowtest.ErrNoEnd {
		t.Fatalf("got %v, want ErrNoEnd", err)
	}
}

// TestReplayIntoADraftComparesTheOutput: the same message into the flow as it
// is says it came out the same, and into a draft that changed a Change node it
// says exactly which property is different at which node.
func TestReplayIntoADraftComparesTheOutput(t *testing.T) {
	snap := only(t, traced(t, line3, "reading", map[string]any{"payload": 92.5, "topic": "line3/temp"}))

	same := flowtest.Replay(context.Background(), []byte(line3), snap, flowtest.Options{})
	if len(same.Problems) > 0 || !same.Same {
		t.Fatalf("a replay into the same flow differed: %+v", same)
	}
	if same.Trace == nil || len(same.Trace.Hops) != len(snap.Hops) {
		t.Fatalf("the replay's own trace has %v hops, the original %d", same.Trace, len(snap.Hops))
	}

	draft := strings.Replace(line3, `"to":"HIGH"`, `"to":"CRITICAL"`, 1)
	res := flowtest.Replay(context.Background(), []byte(draft), snap, flowtest.Options{})
	if len(res.Problems) > 0 {
		t.Fatal(res.Problems)
	}
	if res.Same || len(res.Differences) != 1 {
		t.Fatalf("want one difference, got %+v", res.Differences)
	}
	d := res.Differences[0]
	if d.Node != "alarm-out" || d.Kind != "changed" || len(d.Changes) != 1 || !strings.Contains(d.Changes[0], `payload: "HIGH" -> "CRITICAL"`) {
		t.Errorf("the difference is %+v", d)
	}

	// A draft that sends it somewhere else is a message that ended in a
	// different place, and the comparison says so both ways.
	rerouted := strings.Replace(line3, `"wires":[["alarm"],["fine"]]`, `"wires":[["fine"],["fine"]]`, 1)
	res = flowtest.Replay(context.Background(), []byte(rerouted), snap, flowtest.Options{})
	kinds := map[string]string{}
	for _, d := range res.Differences {
		kinds[d.Node] = d.Kind
	}
	if kinds["alarm-out"] != "missing" || kinds["fine"] != "extra" {
		t.Errorf("rerouted differences: %v", kinds)
	}
}

// TestReplayRefusesAStartTheDraftLacks: a trace that started at a node the
// draft no longer has can't be replayed, and it says so.
func TestReplayRefusesAStartTheDraftLacks(t *testing.T) {
	snap := only(t, traced(t, line3, "reading", map[string]any{"payload": 50.0}))
	draft := strings.Replace(line3, `"id":"reading"`, `"id":"renamed"`, 1)
	res := flowtest.Replay(context.Background(), []byte(draft), snap, flowtest.Options{})
	if len(res.Problems) == 0 || !strings.Contains(res.Problems[0].Message, "reading") {
		t.Fatalf("got %+v", res)
	}
}
