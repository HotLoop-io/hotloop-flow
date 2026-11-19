package diagram

import (
	"errors"
	"testing"
	"time"
)

// G1: a run can be replayed after it is over, from what it recorded. The counter
// loop's recording holds, in order, every node start and finish and every value
// that crossed a wire, and the values on the wires are the ones the run left.
func TestARunCanBeReplayedFromItsTrace(t *testing.T) {
	m := counterLoop(t).manager()
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()

	tr, err := m.Trace("main")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Run != r.ID || tr.Diagram != "main" || tr.State != StateDone || tr.Truncated {
		t.Fatalf("trace header %+v", tr)
	}
	var starts, dones, tokens int
	last := -1.0
	for _, e := range tr.Events {
		if e.T < last {
			t.Fatalf("events are not in time order: %v after %v", e.T, last)
		}
		last = e.T
		if e.Data["diagram"] != "main" {
			t.Fatalf("event %+v does not name its diagram, so a replay could not feed the live handler", e)
		}
		switch e.Kind {
		case "node":
			switch e.Data["state"] {
			case "start":
				starts++
			case "done":
				dones++
			}
		case "token":
			tokens++
			if _, ok := e.Data["wire"].(string); !ok {
				t.Fatalf("a token without a wire: %+v", e)
			}
		default:
			t.Fatalf("a %q event in the trace", e.Kind)
		}
	}
	nodeRuns, _ := r.Stats()
	if int64(starts) != nodeRuns || starts != dones {
		t.Fatalf("%d starts and %d finishes recorded, but the run ran %d nodes", starts, dones, nodeRuns)
	}
	if tokens == 0 {
		t.Fatal("no values were recorded")
	}
	// The last value recorded on the sum wire is the 45 the run ended with.
	var lastSum any
	for _, e := range tr.Events {
		if e.Kind == "token" && e.Data["wire"] == "main/loop.sum>total.value" {
			lastSum = e.Data["value"]
		}
	}
	if lastSum != 45.0 {
		t.Fatalf("the sum wire's last recorded value is %v, want 45", lastSum)
	}
}

// A run that goes on longer than the recording keeps the first of it and says so,
// and what it kept is still in order, so a replay starts from the beginning.
func TestATraceKeepsTheStartOfARunThatIsTooLong(t *testing.T) {
	var rc traceRecorder
	rc.started = time.Now()
	for i := 0; i < traceMax+5; i++ {
		rc.add("token", map[string]any{"wire": "w", "value": float64(i)})
	}
	if len(rc.events) != traceMax || rc.dropped != 5 {
		t.Fatalf("kept %d, dropped %d", len(rc.events), rc.dropped)
	}
	if rc.events[0].Data["value"] != 0.0 || rc.events[traceMax-1].Data["value"] != float64(traceMax-1) {
		t.Fatal("the recording is not the first events in order")
	}
}

func TestATraceOfADiagramThatHasNotRunIsRefused(t *testing.T) {
	m := counterLoop(t).manager()
	if _, err := m.Trace("main"); !errors.Is(err, ErrNoRun) {
		t.Fatalf("got %v, want ErrNoRun", err)
	}
	if _, err := m.Trace("nope"); !errors.Is(err, ErrNoDiagram) {
		t.Fatalf("got %v, want ErrNoDiagram", err)
	}
}
