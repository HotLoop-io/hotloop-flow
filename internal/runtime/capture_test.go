package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// TestCaptureKeepsWhatCameInAndWhatTheDebugShowed: a message that enters at a
// source and reaches a Debug node is kept at both ends under the one id it
// carries all the way, which is what lets the editor make a test of it.
// Nothing else is kept: not the hops in between, not a message too big to be
// worth it, and nothing past the ring's size.
func TestCaptureKeepsWhatCameInAndWhatTheDebugShowed(t *testing.T) {
	tr := newTestRegistry()
	tr.add("source", 0, 1, func(string) node.Node { return &sourceNode{done: make(chan struct{})} })
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("debug", 1, 0, func(string) node.Node { return &sinkNode{} })

	rt := New(tr.Registry, mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"src","type":"source","z":"t1","x":1,"y":1,"wires":[["mid"]]},
        {"id":"mid","type":"pass","z":"t1","x":2,"y":1,"wires":[["dbg"]]},
        {"id":"dbg","type":"debug","z":"t1","x":3,"y":1,"wires":[]}
    ]`), Options{})
	capture := NewCapture(4, 64)
	rt.SetCapture(capture)
	drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	defer rt.Stop(context.Background())

	m := engine.NewMsgWithPayload("21.5")
	id := m.ID()
	if err := rt.SendFrom("src", 0, m); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both ends kept", func() bool { return len(capture.Find(id)) == 2 })
	got := capture.Find(id)
	if got[0].NodeID != "src" || got[0].Port != 0 || got[1].NodeID != "dbg" || got[1].Port != -1 {
		t.Fatalf("kept %+v, want the source's send then the debug's arrival", got)
	}
	if got[1].Data["payload"] != "21.5" {
		t.Errorf("kept %v", got[1].Data)
	}

	big := engine.NewMsgWithPayload(strings.Repeat("x", 100))
	if err := rt.SendFrom("src", 0, big); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the runtime to settle", rt.Settled)
	if n := len(capture.Find(big.ID())); n != 0 {
		t.Errorf("a message over the size cap was kept %d times", n)
	}

	for range 3 {
		if err := rt.SendFrom("src", 0, engine.NewMsgWithPayload("y")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the runtime to settle", rt.Settled)
	}
	if n := len(capture.Find(id)); n != 0 {
		t.Errorf("the ring holds four and six came after; the first is still there %d times", n)
	}
}
