package diagram

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// A node that writes down when it ran, so a test can see the order a run took.
var (
	markMu  sync.Mutex
	markLog []string
)

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-test-mark", PaletteLabel: "mark", InputPorts: []string{"value?"}, OutputPorts: []string{"value"},
			Props: []node.Prop{prop("tag", "Tag", node.PropString, ""), prop("ms", "Delay", node.PropNumber, 0)},
		},
		New: func(raw map[string]any) (Op, error) {
			tag, ms := strProp(raw, "tag", "?"), floatProp(raw, "ms", 0)
			return opFunc(func(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				_ = rc.Wait(ctx, time.Duration(ms*float64(time.Millisecond)))
				markMu.Lock()
				markLog = append(markLog, tag)
				markMu.Unlock()
				return map[string]any{"value": in["value"]}, nil
			}), nil
		},
	})
}

func marks() []string {
	markMu.Lock()
	defer markMu.Unlock()
	return append([]string(nil), markLog...)
}

func resetMarks() {
	markMu.Lock()
	markLog = nil
	markMu.Unlock()
}

type recorder struct {
	mu     sync.Mutex
	events []rec
}

type rec struct {
	topic string
	data  map[string]any
	at    time.Time
}

func (r *recorder) publish(topic string, data map[string]any) {
	r.mu.Lock()
	r.events = append(r.events, rec{topic, data, time.Now()})
	r.mu.Unlock()
}

func (r *recorder) nodeEvent(id, state string) (time.Time, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first time.Time
	n := 0
	for _, e := range r.events {
		if e.topic != "diagram/main/node" || e.data["node"] != id || e.data["state"] != state {
			continue
		}
		if n == 0 {
			first = e.at
		}
		n++
	}
	return first, n
}

// X1: a node runs once, when every input has a value.
func TestAllInputsReadyRunsOnceAfterBoth(t *testing.T) {
	b := newFlow(t).tab("main", nil).
		node("main", "one", "dia-const", map[string]any{"value": "1"}).
		node("main", "two", "dia-const", map[string]any{"value": "2"}).
		node("main", "waitA", "dia-wait", map[string]any{"ms": 80.0}).
		node("main", "waitB", "dia-wait", map[string]any{"ms": 250.0}).
		node("main", "sum", "dia-math", map[string]any{"op": "add"}).
		node("main", "out", "dia-indicator", nil).
		wire("sum.result", "out.value").
		wire("one.value", "waitA.value").wire("two.value", "waitB.value").
		wire("waitA.value", "sum.a").wire("waitB.value", "sum.b")
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
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	sumStart, starts := rc.nodeEvent("sum", "start")
	_, bDone := rc.nodeEvent("waitB", "done")
	bDoneAt, _ := rc.nodeEvent("waitB", "done")
	aDoneAt, _ := rc.nodeEvent("waitA", "done")
	if starts != 1 || bDone != 1 {
		t.Fatalf("sum started %d times, waitB finished %d times", starts, bDone)
	}
	if sumStart.Before(bDoneAt) || sumStart.Before(aDoneAt) {
		t.Fatalf("sum started at %v, before its inputs finished (A %v, B %v)", sumStart, aDoneAt, bDoneAt)
	}
	if got := lastOn(t, r, "main/sum.result>out.value"); got != 3.0 {
		t.Fatalf("sum is %v, want 3", got)
	}
}

// X2: branches with no wire between them run at the same time.
func TestIndependentBranchesRunInParallel(t *testing.T) {
	b := newFlow(t).tab("main", nil).
		node("main", "a", "dia-wait", map[string]any{"ms": 400.0}).
		node("main", "b", "dia-wait", map[string]any{"ms": 400.0})
	start := time.Now()
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	if took < 400*time.Millisecond || took > 520*time.Millisecond {
		t.Fatalf("two 400 ms branches took %v; in parallel that is about 400 ms, in series 800 ms", took)
	}
}

// loopFlow is the counter loop of the spec: carried sum, stop at ten passes.
func counterLoop(t *testing.T) *flowBuilder {
	return newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
		node("main", "loop", "dia-while", map[string]any{"body": "body", "carried": "sum", "start": `{"sum": 0}`}).
		node("main", "total", "dia-indicator", nil).
		node("main", "passes", "dia-indicator", nil).
		wire("loop.sum", "total.value").wire("loop.iterations", "passes.value").
		node("body", "iter", "dia-body-in", map[string]any{"name": "iter"}).
		node("body", "sumIn", "dia-body-in", map[string]any{"name": "sum"}).
		node("body", "add", "dia-math", map[string]any{"op": "add"}).
		node("body", "sumOut", "dia-body-out", map[string]any{"name": "sum"}).
		node("body", "nine", "dia-const", map[string]any{"value": "9"}).
		node("body", "ge", "dia-compare", map[string]any{"op": "ge"}).
		node("body", "stop", "dia-body-out", map[string]any{"name": "stop"}).
		wire("sumIn.value", "add.a").wire("iter.value", "add.b").wire("add.result", "sumOut.value").
		wire("iter.value", "ge.a").wire("nine.value", "ge.b").wire("ge.result", "stop.value")
}

// X3: a while loop with carried state sums 0 to 9 and stops at ten passes.
func TestWhileLoopCarriesState(t *testing.T) {
	r, err := counterLoop(t).run("main")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	if got := lastOn(t, r, "main/loop.sum>total.value"); got != 45.0 {
		t.Fatalf("sum is %v, want 45", got)
	}
	if got := lastOn(t, r, "main/loop.iterations>passes.value"); got != 10.0 {
		t.Fatalf("passes %v, want 10", got)
	}
}

// X4: a for loop over an array doubles each element, and a count of zero gives
// back the start value.
func TestForLoopAutoIndexesAndZeroGivesTheStartValue(t *testing.T) {
	build := func(items string) *flowBuilder {
		b := newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
			node("main", "loop", "dia-for", map[string]any{"body": "body", "carried": "acc", "start": `{"acc": 7}`}).
			node("main", "doubled", "dia-indicator", nil).
			node("main", "acc", "dia-indicator", nil).
			wire("loop.collected", "doubled.value").wire("loop.acc", "acc.value").
			node("body", "item", "dia-body-in", map[string]any{"name": "item"}).
			node("body", "two", "dia-const", map[string]any{"value": "2"}).
			node("body", "mul", "dia-math", map[string]any{"op": "mul"}).
			node("body", "collect", "dia-body-out", map[string]any{"name": "collect"}).
			node("body", "accIn", "dia-body-in", map[string]any{"name": "acc"}).
			node("body", "accOut", "dia-body-out", map[string]any{"name": "acc"}).
			wire("item.value", "mul.a").wire("two.value", "mul.b").wire("mul.result", "collect.value").
			wire("accIn.value", "accOut.value")
		b.node("main", "items", "dia-const", map[string]any{"value": items}).wire("items.value", "loop.items")
		return b
	}
	r, err := build("[1,2,3]").run("main")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := lastOn(t, r, "main/loop.collected>doubled.value").([]any)
	if fmt.Sprint(got) != "[2 4 6]" {
		t.Fatalf("collected %v, want [2 4 6]", got)
	}
	r, err = build("[]").run("main")
	if err != nil {
		t.Fatal(err)
	}
	if got := lastOn(t, r, "main/loop.acc>acc.value"); got != 7.0 {
		t.Fatalf("zero passes gave %v, want the start value 7", got)
	}
	if got, _ := lastOn(t, r, "main/loop.collected>doubled.value").([]any); len(got) != 0 {
		t.Fatalf("zero passes collected %v", got)
	}
}

// X7: an after wire orders two nodes that share no data, every time. Without it
// the order is whatever the scheduler picks.
func TestAfterWireOrdersEveryRun(t *testing.T) {
	ordered := newFlow(t).tab("main", nil).
		node("main", "a", "dia-test-mark", map[string]any{"tag": "A", "ms": 1.0}).
		node("main", "b", "dia-test-mark", map[string]any{"tag": "B"}).
		wire("a.done", "b.after")
	m := ordered.manager()
	for i := 0; i < 1000; i++ {
		resetMarks()
		r, err := m.Start("main", StartOptions{})
		if err != nil {
			t.Fatal(err)
		}
		<-r.Done()
		if got := fmt.Sprint(marks()); got != "[A B]" {
			t.Fatalf("run %d ran %s; the after wire says A then B", i, got)
		}
	}

	free := newFlow(t).tab("main", nil).
		node("main", "a", "dia-test-mark", map[string]any{"tag": "A", "ms": 1.0}).
		node("main", "b", "dia-test-mark", map[string]any{"tag": "B"})
	m = free.manager()
	flipped := 0
	for i := 0; i < 50 && flipped == 0; i++ {
		resetMarks()
		r, _ := m.Start("main", StartOptions{})
		<-r.Done()
		if fmt.Sprint(marks()) == "[B A]" {
			flipped++
		}
	}
	if flipped == 0 {
		t.Fatal("without the after wire B never ran first, so the 1,000 runs above proved nothing")
	}
}
