package diagram

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// Each analysis test wires a constant array into the node under test and an
// indicator onto each output it checks, then reads the indicator wire.

func analysisFlow(t *testing.T, y string, typ string, props map[string]any, outs ...string) *flowBuilder {
	t.Helper()
	b := newFlow(t).tab("main", nil).
		node("main", "y", "dia-const", map[string]any{"value": y}).
		node("main", "n", typ, props).
		wire("y.value", "n.y")
	for _, o := range outs {
		b.node("main", "show-"+o, "dia-indicator", nil).wire("n."+o, "show-"+o+".value")
	}
	return b
}

func runClean(t *testing.T, b *flowBuilder) *Run {
	t.Helper()
	m := b.manager()
	mustProblemFree(t, m, "main")
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	return r
}

func wireVal(t *testing.T, r *Run, node, port string) any {
	return lastOn(t, r, fmt.Sprintf("main/n.%s>show-%s.value", port, port))
}

func approx(t *testing.T, name string, got any, want, tol float64) {
	t.Helper()
	g, ok := got.(float64)
	if !ok || math.Abs(g-want) > tol {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// A1 through the node: the statistics of 2 4 4 4 5 5 7 9 (hand computed in the dsp
// tests: mean 5, RMS sqrt(29), sample std dev sqrt(32/7)).
func TestStatsNodeGivesTheKnownAnswers(t *testing.T) {
	r := runClean(t, analysisFlow(t, "[2,4,4,4,5,5,7,9]", "dia-stats", nil, "mean", "rms", "min", "max", "stddev"))
	approx(t, "mean", wireVal(t, r, "n", "mean"), 5, 1e-12)
	approx(t, "rms", wireVal(t, r, "n", "rms"), math.Sqrt(29), 1e-12)
	approx(t, "min", wireVal(t, r, "n", "min"), 2, 0)
	approx(t, "max", wireVal(t, r, "n", "max"), 9, 0)
	approx(t, "stddev", wireVal(t, r, "n", "stddev"), math.Sqrt(32.0/7), 1e-12)
}

// A1 acceptance through a diagram: a sine of amplitude 3 has RMS 3/sqrt(2).
func TestStatsNodeRMSOfASine(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 1000; i++ { // 10 whole periods
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%.17g", 3*math.Sin(2*math.Pi*float64(i)/100))
	}
	sb.WriteString("]")
	r := runClean(t, analysisFlow(t, sb.String(), "dia-stats", nil, "rms"))
	got := wireVal(t, r, "n", "rms").(float64)
	if math.Abs(got-3/math.Sqrt2)/(3/math.Sqrt2) > 1e-6 {
		t.Fatalf("rms %v, want %v", got, 3/math.Sqrt2)
	}
}

func TestIntegralAndDerivativeNodes(t *testing.T) {
	// A ramp 0..10 at 100 samples per second: area 10*10/2 * 0.01 = 0.5, slope 100 per second.
	ramp := "[0,1,2,3,4,5,6,7,8,9,10]"
	r := runClean(t, analysisFlow(t, ramp, "dia-integral", map[string]any{"rate": 100.0}, "result"))
	approx(t, "total", wireVal(t, r, "n", "result"), 0.5, 1e-12)

	r = runClean(t, analysisFlow(t, ramp, "dia-integral", map[string]any{"rate": 100.0, "mode": "running"}, "result"))
	run, _ := wireVal(t, r, "n", "result").([]any)
	if len(run) != 11 || run[0] != 0.0 {
		t.Fatalf("running integral %v", run)
	}
	approx(t, "running at 4", run[4], 4*4*0.01/2, 1e-12)
	approx(t, "running at the end", run[10], 0.5, 1e-12)

	r = runClean(t, analysisFlow(t, ramp, "dia-derivative", map[string]any{"rate": 100.0}, "result"))
	d, _ := wireVal(t, r, "n", "result").([]any)
	if len(d) != 11 {
		t.Fatalf("derivative has %d values", len(d))
	}
	for i, v := range d {
		approx(t, fmt.Sprint("slope ", i), v, 100, 1e-9)
	}

	// The dt input wins over the property: 0.5 s between samples, so the slope is 2 per second.
	b := analysisFlow(t, ramp, "dia-derivative", map[string]any{"rate": 100.0}, "result").
		node("main", "dt", "dia-const", map[string]any{"value": "0.5"}).wire("dt.value", "n.dt")
	r = runClean(t, b)
	d, _ = wireVal(t, r, "n", "result").([]any)
	approx(t, "slope with dt", d[3], 2, 1e-12)

	// The rate input wins over the property.
	b = analysisFlow(t, ramp, "dia-derivative", map[string]any{"rate": 100.0}, "result").
		node("main", "rate", "dia-const", map[string]any{"value": "10"}).wire("rate.value", "n.rate")
	r = runClean(t, b)
	d, _ = wireVal(t, r, "n", "result").([]any)
	approx(t, "slope with rate", d[3], 10, 1e-12)
}

// A2 through a diagram: 50 Hz plus 500 Hz at 5 kHz through a 4th order lowpass at
// 100 Hz loses at least 40 dB of the 500 Hz tone. Each tone is filtered on its own and
// measured by its RMS over whole periods, which is its amplitude over root 2.
func TestFilterNodeLowpassLosesTheHighTone(t *testing.T) {
	signal := func(f float64) string {
		var sb strings.Builder
		sb.WriteString("[")
		for i := 0; i < 5000; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, "%.17g", math.Sin(2*math.Pi*f*float64(i)/5000))
		}
		sb.WriteString("]")
		return sb.String()
	}
	props := map[string]any{"kind": "lowpass", "cutoff": 100.0, "order": 4.0, "rate": 5000.0}
	rmsOfTail := func(f float64) float64 {
		r := runClean(t, analysisFlow(t, signal(f), "dia-filter", props, "y"))
		y, _ := floats(wireVal(t, r, "n", "y"))
		if len(y) != 5000 {
			t.Fatalf("filtered %d samples", len(y))
		}
		// Past the settle: the last 2000 samples are whole periods of both tones.
		rms, err := rmsOf(y[3000:])
		if err != nil {
			t.Fatal(err)
		}
		return rms * math.Sqrt2
	}
	low, high := rmsOfTail(50), rmsOfTail(500)
	lossHigh := 20 * math.Log10(high)
	lossLow := 20 * math.Log10(low)
	t.Logf("500 Hz: %.2f dB, 50 Hz: %.3f dB", lossHigh, lossLow)
	if lossHigh > -40 {
		t.Errorf("500 Hz only lost %.2f dB", -lossHigh)
	}
	if math.Abs(lossLow) > 0.5 {
		t.Errorf("50 Hz moved %.3f dB", lossLow)
	}
}

func rmsOf(y []float64) (float64, error) {
	s := 0.0
	for _, v := range y {
		s += v * v
	}
	return math.Sqrt(s / float64(len(y))), nil
}

// A new run starts from rest, and one run keeps its memory across passes of a loop:
// a step fed in four pieces comes out the same as the same step fed whole.
func TestFilterNodeKeepsItsMemoryInARunAndStartsCleanInTheNext(t *testing.T) {
	props := map[string]any{"kind": "lowpass", "cutoff": 50.0, "order": 2.0, "rate": 1000.0}
	step := "[1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1]"
	whole := runClean(t, analysisFlow(t, step, "dia-filter", props, "y"))
	want, _ := floats(wireVal(t, whole, "n", "y"))

	b := newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
		node("main", "chunks", "dia-const", map[string]any{"value": "[[1,1,1,1,1],[1,1,1,1,1],[1,1,1,1,1],[1,1,1,1,1]]"}).
		node("main", "loop", "dia-for", map[string]any{"body": "body"}).
		node("main", "out", "dia-indicator", nil).
		wire("chunks.value", "loop.items").wire("loop.collected", "out.value").
		node("body", "item", "dia-body-in", map[string]any{"name": "item"}).
		node("body", "n", "dia-filter", props).
		node("body", "collect", "dia-body-out", map[string]any{"name": "collect"}).
		wire("item.value", "n.y").wire("n.y", "collect.value")
	m := b.manager()
	mustProblemFree(t, m, "main")
	var first []float64
	for run := 0; run < 2; run++ {
		r, err := m.Start("main", StartOptions{})
		if err != nil {
			t.Fatal(err)
		}
		<-r.Done()
		if st, err := r.State(); st != StateDone {
			t.Fatalf("state %s, %v", st, err)
		}
		var got []float64
		parts, _ := lastOn(t, r, "main/loop.collected>out.value").([]any)
		for _, p := range parts {
			c, _ := floats(p)
			got = append(got, c...)
		}
		if len(got) != len(want) {
			t.Fatalf("run %d: %d samples, want %d", run, len(got), len(want))
		}
		for i := range want {
			if math.Abs(got[i]-want[i]) > 1e-12 {
				t.Fatalf("run %d sample %d: chunked %v, whole %v", run, i, got[i], want[i])
			}
		}
		if run == 0 {
			first = got
		} else if got[0] != first[0] {
			t.Fatalf("the second run did not start from rest: %v then %v", first[0], got[0])
		}
	}
}

// One number at a time works too, and comes out a number.
func TestFilterNodeTakesOneNumber(t *testing.T) {
	r := runClean(t, analysisFlow(t, "1", "dia-filter", map[string]any{"cutoff": 100.0, "order": 1.0, "rate": 1000.0}, "y"))
	v, ok := wireVal(t, r, "n", "y").(float64)
	if !ok || v <= 0 || v >= 1 {
		t.Fatalf("one sample of a step through a lowpass gave %v", wireVal(t, r, "n", "y"))
	}
}

// A non-number on the input is an error on the err wire, with the bad-input code.
func TestAnalysisNodesPutBadInputOnTheErrorWire(t *testing.T) {
	cases := []struct {
		typ   string
		y     string
		props map[string]any
	}{
		{"dia-stats", `"hello"`, nil},
		{"dia-stats", `[1,"x",3]`, nil},
		{"dia-stats", `[]`, nil},
		{"dia-stats", `[1]`, nil}, // one reading has no standard deviation
		{"dia-integral", `"hello"`, nil},
		{"dia-derivative", `[1]`, nil},
		{"dia-filter", `"hello"`, nil},
		{"dia-filter", `[]`, nil},
	}
	for _, c := range cases {
		b := analysisFlow(t, c.y, c.typ, c.props, "err")
		b.byID["show-err"]["kind"] = "error"
		r := runClean(t, b)
		e, ok := wireVal(t, r, "n", "err").(map[string]any)
		if !ok || e["status"] != true || e["code"] != float64(CodeBadInput) || e["node"] != "n" || e["source"] == "" {
			t.Errorf("%s %s: error out is %v", c.typ, c.y, wireVal(t, r, "n", "err"))
		}
	}
}

// With an error coming in the node does no work and hands the error on.
func TestAnalysisNodesSkipOnAnErrorIn(t *testing.T) {
	b := newFlow(t).tab("main", nil).
		node("main", "line", "dia-const", map[string]any{"value": `"not a frame"`}).
		node("main", "parse", "dia-parse-reading", nil).
		node("main", "stats", "dia-stats", nil).
		node("main", "filt", "dia-filter", nil).
		node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
		node("main", "err2", "dia-indicator", map[string]any{"kind": "error"}).
		wire("line.value", "parse.line").
		wire("parse.err", "stats.err").wire("parse.value", "stats.y").
		wire("parse.err", "filt.err").wire("parse.value", "filt.y").
		wire("stats.err", "err.value").wire("filt.err", "err2.value")
	m := NewManager(nil)
	rc := &recorder{}
	m.SetPublisher(rc.publish)
	t.Cleanup(m.Close)
	m.Apply(b.flows())
	mustProblemFree(t, m, "main")
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	for _, id := range []string{"stats", "filt"} {
		if _, n := rc.nodeEvent(id, "skipped"); n != 1 {
			t.Errorf("%s skipped %d times, want 1", id, n)
		}
		if _, n := rc.nodeEvent(id, "done"); n != 0 {
			t.Errorf("%s ran with an error in", id)
		}
	}
	e, ok := lastOn(t, r, "main/stats.err>err.value").(map[string]any)
	if !ok || e["node"] != "parse" || e["code"] != float64(CodeParse) {
		t.Errorf("the error handed on is %v", lastOn(t, r, "main/stats.err>err.value"))
	}
}

// A bad design shows in the problems list before the diagram runs.
func TestAnalysisNodesRefuseABadDesignWhenDeployed(t *testing.T) {
	for name, props := range map[string]map[string]any{
		"cutoff above half the rate": {"cutoff": 600.0, "rate": 1000.0},
		"order 11":                   {"order": 11.0},
		"upper below lower edge":     {"kind": "bandpass", "cutoff": 200.0, "cutoff2": 100.0},
		"no such kind":               {"kind": "notch"},
		"rate zero":                  {"rate": -1.0},
	} {
		m := analysisFlow(t, "[1,2,3]", "dia-filter", props).manager()
		d, _ := m.Diagram("main")
		found := false
		for _, p := range d.Problems {
			if p.Node == "n" && strings.Contains(p.Text, "cannot be built") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: problems are %v", name, d.Problems)
		}
	}
	for name, props := range map[string]map[string]any{
		"integral mode": {"mode": "sideways"},
		"integral rate": {"rate": -1.0},
	} {
		d, _ := analysisFlow(t, "[1,2,3]", "dia-integral", props).manager().Diagram("main")
		if len(d.Problems) == 0 {
			t.Errorf("%s: no problem listed", name)
		}
	}
	m := analysisFlow(t, "[1,2,3]", "dia-filter", map[string]any{"kind": "bandstop", "cutoff": 45.0, "cutoff2": 55.0, "rate": 1000.0, "order": 3.0}).manager()
	mustProblemFree(t, m, "main")
}
