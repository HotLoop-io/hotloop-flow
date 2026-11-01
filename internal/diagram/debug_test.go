package diagram

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// values is every wire's last value, without the times.
func values(r *Run) map[string]any {
	out := map[string]any{}
	for k, w := range r.WireValues() {
		out[k] = w.Last
	}
	return out
}

// G2: a probe never changes a run. The same diagram, run with and without a
// probe on a wire, ends with the same value on every wire and the same count of
// node runs; the probe only remembers more of what went by.
func TestProbeDoesNotChangeARun(t *testing.T) {
	run := func(probe bool) (*Run, *Manager) {
		m := counterLoop(t).manager()
		if probe {
			m.SetProbes("main", []string{"body/add.result>sumOut.value"}, 100)
		}
		r, err := m.Start("main", StartOptions{})
		if err != nil {
			t.Fatal(err)
		}
		<-r.Done()
		return r, m
	}
	plain, _ := run(false)
	probed, _ := run(true)
	if !reflect.DeepEqual(values(plain), values(probed)) {
		t.Fatalf("a probe changed the run\nwithout %v\nwith    %v", values(plain), values(probed))
	}
	a, _ := plain.Stats()
	b, _ := probed.Stats()
	if a != b {
		t.Fatalf("node runs: %d without a probe, %d with", a, b)
	}
	hist := probed.WireValues()["body/add.result>sumOut.value"].History
	if len(hist) != 10 {
		t.Fatalf("the probe kept %d values, want all 10", len(hist))
	}
	want := []float64{0, 1, 3, 6, 10, 15, 21, 28, 36, 45}
	for i, h := range hist {
		if h.Value != want[i] {
			t.Fatalf("probe history %v", hist)
		}
	}
	if n := len(plain.WireValues()["body/add.result>sumOut.value"].History); n != defaultHistory {
		t.Fatalf("an unprobed wire kept %d values, want %d", n, defaultHistory)
	}
}

// G5: after the run, every wire shows its last value.
func TestEveryWireKeepsItsLastValue(t *testing.T) {
	r, err := counterLoop(t).run("main")
	if err != nil {
		t.Fatal(err)
	}
	all := r.WireValues()
	if len(all) == 0 {
		t.Fatal("no wires kept")
	}
	for _, key := range []string{"main/loop.sum>total.value", "body/iter.value>add.b", "body/ge.result>stop.value"} {
		if _, ok := all[key]; !ok {
			t.Fatalf("no value kept for %s", key)
		}
	}
	if all["body/ge.result>stop.value"].Last != true {
		t.Fatalf("the last stop was %v", all["body/ge.result>stop.value"].Last)
	}
}

// chain is a -> b -> c, each waiting a little, with an indicator at the end.
func chain(t *testing.T) *flowBuilder {
	return newFlow(t).tab("main", nil).
		node("main", "a", "dia-wait", map[string]any{"ms": 5.0}).
		node("main", "b", "dia-wait", map[string]any{"ms": 5.0}).
		node("main", "c", "dia-wait", map[string]any{"ms": 5.0}).
		node("main", "seven", "dia-const", map[string]any{"value": "7"}).
		wire("seven.value", "a.value").wire("a.value", "b.value").wire("b.value", "c.value")
}

// G3: a breakpoint on a node pauses the run before that node runs, and resume
// carries on.
func TestBreakpointPausesBeforeTheNode(t *testing.T) {
	m := chain(t).manager()
	rc := &recorder{}
	m.SetPublisher(rc.publish)
	m.SetBreakpoints("main", []string{"c"})
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to pause", func() bool { st, _ := r.State(); return st == StatePaused })
	if got := r.Controller().Waiting(); fmt.Sprint(got) != "[c]" {
		t.Fatalf("waiting before %v, want c", got)
	}
	if _, n := rc.nodeEvent("c", "start"); n != 0 {
		t.Fatal("c started while paused before it")
	}
	if _, n := rc.nodeEvent("b", "done"); n != 1 {
		t.Fatal("b had not finished, so the pause was too early")
	}
	r.Controller().Resume()
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not finish after resume")
	}
	if _, n := rc.nodeEvent("c", "done"); n != 1 {
		t.Fatalf("c ran %d times after resume", n)
	}
}

// G4: step over a loop runs its whole body; step into stops at the first node
// inside it.
func TestStepOverAndStepInto(t *testing.T) {
	order := func(mode StepMode) []string {
		m := counterLoop(t).manager()
		rc := &recorder{}
		m.SetPublisher(rc.publish)
		m.SetBreakpoints("main", []string{"loop"})
		r, err := m.Start("main", StartOptions{})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the pause before the loop", func() bool { return fmt.Sprint(r.Controller().Waiting()) == "[loop]" })
		r.Controller().Step(mode)
		waitFor(t, "the next pause", func() bool {
			w := r.Controller().Waiting()
			return len(w) > 0 && w[0] != "loop"
		})
		next := r.Controller().Waiting()[0]
		started := 0
		rc.mu.Lock()
		for _, e := range rc.events {
			if e.data["state"] == "start" {
				started++
			}
		}
		rc.mu.Unlock()
		r.Stop()
		<-r.Done()
		return []string{next, fmt.Sprint(started)}
	}
	over := order(StepOver)
	into := order(StepInto)
	if over[0] == "" || into[0] == "" {
		t.Fatal("no next pause")
	}
	// Over: the whole body ran (10 passes of 7 nodes and more), and the pause is
	// at a node of the top diagram, not in the body.
	if over[0] != "total" && over[0] != "passes" {
		t.Fatalf("step over paused before %s, want a node after the loop", over[0])
	}
	if over[1] == "0" {
		t.Fatal("step over ran nothing in the body")
	}
	// Into: stops at a node inside the body before any of it has run.
	bodyNodes := []string{"iter", "sumIn", "add", "sumOut", "nine", "ge", "stop"}
	sort.Strings(bodyNodes)
	if i := sort.SearchStrings(bodyNodes, into[0]); i >= len(bodyNodes) || bodyNodes[i] != into[0] {
		t.Fatalf("step into paused before %s, want a node of the body", into[0])
	}
}

// A panel control is read when the node runs, a button latches until it is read,
// and a value outside the range is refused.
func TestControlsAreReadWhenTheNodeRunsAndCheckedOnTheWay(t *testing.T) {
	b := newFlow(t).tab("main", nil).
		node("main", "gain", "dia-control", map[string]any{"label": "Gain", "kind": "number", "default": "2", "min": "0", "max": "10"}).
		node("main", "go", "dia-control", map[string]any{"label": "Go", "kind": "bool", "style": "button", "latch": true}).
		node("main", "out", "dia-indicator", nil).
		node("main", "pressed", "dia-indicator", nil).
		wire("gain.value", "out.value").wire("go.value", "pressed.value")
	m := b.manager()
	p, err := m.Panel("main")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := m.Start("main", StartOptions{})
	<-r.Done()
	if got := p.Snapshot().Indicators["out"]; got != 2.0 {
		t.Fatalf("the control's starting value was %v", got)
	}
	if _, err := p.SetControl("gain", 11.0); err == nil {
		t.Fatal("a value above the maximum was accepted")
	}
	if _, err := p.SetControl("gain", "abc"); err == nil {
		t.Fatal("text was accepted by a numeric control")
	}
	if v, err := p.SetControl("gain", "7.5"); err != nil || v != 7.5 {
		t.Fatalf("setting 7.5: %v, %v", v, err)
	}
	if _, err := p.SetControl("go", true); err != nil {
		t.Fatal(err)
	}
	r, _ = m.Start("main", StartOptions{})
	<-r.Done()
	s := p.Snapshot()
	if s.Indicators["out"] != 7.5 || s.Indicators["pressed"] != true {
		t.Fatalf("the run read %v and %v", s.Indicators["out"], s.Indicators["pressed"])
	}
	if s.Controls["go"] != false {
		t.Fatalf("the button stayed pressed: %v", s.Controls["go"])
	}
}

// P3: a chart keeps the last 1,024 points, and counts what it let go of.
func TestChartKeepsTheLastThousandAndTwentyFour(t *testing.T) {
	b := newFlow(t).tab("main", nil).
		node("main", "n", "dia-const", map[string]any{"value": "5000"}).
		node("main", "chart", "dia-chart", map[string]any{"mode": "chart"}).
		wire("n.value", "chart.y")
	m := b.manager()
	p, _ := m.Panel("main")
	r, _ := m.Start("main", StartOptions{})
	<-r.Done()
	for i := 0; i < 5000; i++ {
		p.PutChart("chart", nil, []float64{float64(i)})
	}
	c := p.Snapshot().Charts["chart"]
	if len(c.Y) != 1024 || c.Y[0] != 5000-1024 || c.Y[1023] != 4999 {
		t.Fatalf("the chart holds %d points from %v to %v", len(c.Y), c.Y[0], c.Y[len(c.Y)-1])
	}
	// 5,001 points went in: the diagram's own 5000 and the loop's 0 to 4999.
	if c.Dropped != 5001-1024 {
		t.Fatalf("dropped %d, want %d", c.Dropped, 5001-1024)
	}
	// Changing the mode to graph changes what it shows and nothing is rewired.
	b2 := newFlow(t).tab("main", nil).
		node("main", "src", "dia-const", map[string]any{"value": "[1,2,3]"}).
		node("main", "chart", "dia-chart", map[string]any{"mode": "graph"}).
		wire("src.value", "chart.y")
	m2 := b2.manager()
	p2, _ := m2.Panel("main")
	r2, _ := m2.Start("main", StartOptions{})
	<-r2.Done()
	if got := p2.Snapshot().Charts["chart"].Y; fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("the diagram gave the graph %v, want [1 2 3]", got)
	}
	p2.PutChart("chart", nil, []float64{9, 8})
	if got := p2.Snapshot().Charts["chart"].Y; fmt.Sprint(got) != "[9 8]" {
		t.Fatalf("a graph shows %v, want the last array only", got)
	}
}

// G6: a diagram that cannot run says why, naming the node and the input.
func TestProblemsNameTheNodeAndInput(t *testing.T) {
	b := newFlow(t).tab("main", nil).
		node("main", "m", "dia-math", map[string]any{"op": "add"}).
		node("main", "k", "dia-const", map[string]any{"value": "1"}).
		wire("k.value", "m.b")
	m := b.manager()
	d, _ := m.Diagram("main")
	if len(d.Problems) != 1 || d.Problems[0].Node != "m" || d.Problems[0].Port != "a" {
		t.Fatalf("problems: %v", d.Problems)
	}
	if _, err := m.Start("main", StartOptions{}); err == nil {
		t.Fatal("a diagram with an unwired required input started")
	}
	// A wire loop is a problem too.
	c := newFlow(t).tab("main", nil).
		node("main", "x", "dia-math", map[string]any{"op": "add"}).
		node("main", "y", "dia-math", map[string]any{"op": "add"}).
		wire("x.result", "y.a").wire("y.result", "x.a")
	d, _ = c.manager().Diagram("main")
	found := false
	for _, p := range d.Problems {
		if p.Node == "x" && p.Text != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a wire loop was not reported: %v", d.Problems)
	}
}

// An output slot with no wire in it, past the last output, is padding and not a
// mistake: the editor pads a node's wires to its default count.
func TestEmptyPaddingPastTheLastOutputIsNotAProblem(t *testing.T) {
	b := newFlow(t).tab("main", nil).node("main", "k", "dia-const", map[string]any{"value": "1"})
	b.byID["k"]["wires"] = []any{[]any{}, []any{}, []any{}, []any{}, []any{}}
	m := b.manager()
	if d, _ := m.Diagram("main"); len(d.Problems) != 0 {
		t.Fatalf("problems: %v", d.Problems)
	}
}

// Resume after a step runs at full speed to the end, with nothing left stepping.
func TestResumeAfterAStepRunsToTheEnd(t *testing.T) {
	m := counterLoop(t).manager()
	m.SetBreakpoints("main", []string{"loop"})
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pause before the loop", func() bool { return fmt.Sprint(r.Controller().Waiting()) == "[loop]" })
	r.Controller().Step(StepOver)
	waitFor(t, "the next pause", func() bool { w := r.Controller().Waiting(); return len(w) > 0 && w[0] != "loop" })
	m.SetBreakpoints("main", nil)
	r.Controller().Resume()
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("still waiting before %v after resume", r.Controller().Waiting())
	}
}
