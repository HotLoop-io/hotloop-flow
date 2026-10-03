package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// watcher is an Observer that keeps everything.
type watcher struct {
	mu       sync.Mutex
	received map[string][]*engine.Msg
	sent     map[string]map[int][]*engine.Msg
}

func newWatcher() *watcher {
	return &watcher{received: map[string][]*engine.Msg{}, sent: map[string]map[int][]*engine.Msg{}}
}

func (w *watcher) Received(id string, m *engine.Msg) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.received[id] = append(w.received[id], m)
}

func (w *watcher) Sent(id string, port int, m *engine.Msg) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sent[id] == nil {
		w.sent[id] = map[int][]*engine.Msg{}
	}
	w.sent[id][port] = append(w.sent[id][port], m)
}

func (w *watcher) sentOn(id string, port int) []*engine.Msg {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*engine.Msg(nil), w.sent[id][port]...)
}

func (w *watcher) receivedBy(id string) []*engine.Msg {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*engine.Msg(nil), w.received[id]...)
}

// twoPorts sends what it gets out of both outputs.
type twoPorts struct{}

func (twoPorts) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	out.SendAll([][]*engine.Msg{{m.Clone()}, {m}})
	return nil
}

// TestObserverSeesUnwiredPortsAndWhatEachNodeWasHanded is what lets a flow test
// watch a port nobody wired, which is most of the ports a test cares about: the
// last node in a flow is usually wired to nothing.
func TestObserverSeesUnwiredPortsAndWhatEachNodeWasHanded(t *testing.T) {
	tr := newTestRegistry()
	sink := &sinkNode{}
	tr.add("two", 1, 2, func(string) node.Node { return twoPorts{} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })

	rt := New(tr.Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"a","type":"two","z":"t1","x":1,"y":1,"wires":[["b"],[]]},
        {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
    ]`), Options{})
	w := newWatcher()
	rt.SetObserver(w)
	drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	defer rt.Stop(context.Background())

	if err := rt.Inject("a", engine.NewMsgWithPayload("x")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the sink to get it", func() bool { return sink.count() == 1 })
	waitFor(t, "the runtime to settle", rt.Settled)

	if got := w.sentOn("a", 1); len(got) != 1 || got[0].Payload() != "x" {
		t.Errorf("port 2 is wired to nothing and the observer saw %d message(s) leave it, want 1", len(got))
	}
	if got := w.sentOn("a", 0); len(got) != 1 {
		t.Errorf("port 1 sent %d, want 1", len(got))
	}
	if got := w.receivedBy("b"); len(got) != 1 || got[0].Payload() != "x" {
		t.Errorf("the sink was handed %d message(s) by the observer's count, want 1", len(got))
	}

	// The observer gets a copy. A node changing its message afterwards must
	// not change what a test recorded.
	sink.last().SetPayload("changed by the node")
	if got := w.receivedBy("b")[0].Payload(); got != "x" {
		t.Errorf("the recorded message changed with the node's own: %v", got)
	}
}

// burstNode sends n messages for every one it receives.
type burstNode struct{ n int }

func (b burstNode) Receive(_ context.Context, _ *engine.Msg, out node.Emitter) error {
	for i := range b.n {
		out.Send(0, engine.NewMsgWithPayload(float64(i)))
	}
	return nil
}

// TestSettledNeverLiesWhileMessagesAreMoving is the property a flow test's
// "nothing came out" leans on: the runtime never reports settled while a
// message is still travelling. One message turns into 300 that cross three
// nodes, and a watcher asks the whole time. Any moment it says settled with
// fewer than 300 at the end is a message it lost track of.
func TestSettledNeverLiesWhileMessagesAreMoving(t *testing.T) {
	const burst = 300
	tr := newTestRegistry()
	sink := &sinkNode{}
	tr.add("burst", 1, 1, func(string) node.Node { return burstNode{n: burst} })
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })

	rt := New(tr.Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"a","type":"burst","z":"t1","x":1,"y":1,"wires":[["b"]]},
        {"id":"b","type":"pass","z":"t1","x":2,"y":1,"wires":[["c"]]},
        {"id":"c","type":"pass","z":"t1","x":3,"y":1,"wires":[["d"]]},
        {"id":"d","type":"sink","z":"t1","x":4,"y":1,"wires":[]}
    ]`), Options{InboxCapacity: 8})
	drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	defer rt.Stop(context.Background())

	for round := range 5 {
		before := sink.count()
		if err := rt.Inject("a", engine.NewMsg()); err != nil {
			t.Fatal(err)
		}
		for !rt.Settled() {
			// Checked in this order on purpose: settled first, then the
			// count. Settled at a moment the sink is short is the lie.
		}
		if got := sink.count() - before; got != burst {
			t.Fatalf("round %d: settled with %d of %d messages at the sink", round, got, burst)
		}
	}
}

// heldNode holds one message until released, the way a Delay does.
type heldNode struct {
	mu   sync.Mutex
	held []*engine.Msg
	out  node.Emitter
}

func (h *heldNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = append(h.held, m)
	h.out = out
	return nil
}

func (h *heldNode) Pending() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.held)
}

func (h *heldNode) release() {
	h.mu.Lock()
	held, out := h.held, h.out
	h.mu.Unlock()
	for _, m := range held {
		out.Send(0, m)
	}
	h.mu.Lock()
	h.held = nil
	h.mu.Unlock()
}

// TestQuietWaitsForWhatANodeIsHolding: settled is about messages moving, quiet
// is about work outstanding. A test that ended on settled would miss whatever a
// Delay lets go of a moment later.
func TestQuietWaitsForWhatANodeIsHolding(t *testing.T) {
	tr := newTestRegistry()
	held := &heldNode{}
	sink := &sinkNode{}
	tr.add("hold", 1, 1, func(string) node.Node { return held })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })

	rt := New(tr.Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"a","type":"hold","z":"t1","x":1,"y":1,"wires":[["b"]]},
        {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
    ]`), Options{})
	drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	defer rt.Stop(context.Background())

	if err := rt.Inject("a", engine.NewMsg()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the message to be held", func() bool { return held.Pending() == 1 })
	waitFor(t, "the runtime to settle", rt.Settled)
	if rt.Quiet() {
		t.Fatal("quiet while a node holds a message")
	}
	held.release()
	waitFor(t, "quiet once it's let go", rt.Quiet)
	if sink.count() != 1 {
		t.Fatalf("the sink got %d, want 1", sink.count())
	}
}

// TestSettledAfterEveryKindOfOverflow walks the paths where a message never
// reaches an inbox. Each has to give its count back, or the runtime would
// never settle again after the first overflow and every test after it would
// sit out its timeout.
func TestSettledAfterEveryKindOfOverflow(t *testing.T) {
	for _, policy := range []OverflowPolicy{OverflowDropNewest, OverflowDropOldest, OverflowError} {
		t.Run(string(policy), func(t *testing.T) {
			tr := newTestRegistry()
			gate := make(chan struct{})
			sink := &sinkNode{gate: gate}
			tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
			tr.add("sink", 1, 0, func(string) node.Node { return sink })

			rt := New(tr.Registry, mustFlows(t, `[
                {"id":"t1","type":"tab","label":"T"},
                {"id":"a","type":"pass","z":"t1","x":1,"y":1,"wires":[["b"]]},
                {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[],"ew_inboxCapacity":1,"ew_overflow":"`+string(policy)+`"}
            ]`), Options{})
			drain(rt)
			if fails := rt.Start(context.Background()); len(fails) > 0 {
				t.Fatal(fails)
			}
			defer rt.Stop(context.Background())

			for range 5 {
				if err := rt.Inject("a", engine.NewMsg()); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, "something to overflow", func() bool {
				for _, s := range rt.Snapshots() {
					if s.NodeID == "b" && s.Dropped > 0 {
						return true
					}
				}
				return false
			})
			close(gate)
			waitFor(t, "the runtime to settle after the overflow", rt.Settled)
		})
	}
}

// TestSendFromStandsInForASource: a node with no input can still put a message
// into the flow, as if it had produced it.
func TestSendFromStandsInForASource(t *testing.T) {
	tr := newTestRegistry()
	sink := &sinkNode{}
	tr.add("source", 0, 1, func(string) node.Node { return &sourceNode{done: make(chan struct{})} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })

	rt := New(tr.Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"a","type":"source","z":"t1","x":1,"y":1,"wires":[["b"]]},
        {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
    ]`), Options{})
	drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	defer rt.Stop(context.Background())

	if err := rt.SendFrom("a", 0, engine.NewMsgWithPayload("from the test")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the message", func() bool { return sink.count() == 1 })
	if got := sink.last().Payload(); got != "from the test" {
		t.Fatalf("payload %v", got)
	}
	if err := rt.SendFrom("nope", 0, engine.NewMsg()); err == nil {
		t.Fatal("sending from a node that isn't running worked")
	}
}
