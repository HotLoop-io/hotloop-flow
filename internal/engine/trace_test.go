package engine

import (
	"strings"
	"testing"
	"time"
)

// start puts a message into the tracer at a node the way a source does and
// returns the message, tagged and ready to be handed on.
func start(tr *Tracer, node string, payload any) *Msg {
	m := NewMsgWithPayload(payload)
	tr.Outgoing(m, node, "inject", 0, "", nil, -1, time.Now())
	return m
}

func TestTheTracerKeepsTheNewestTracesAndForgetsTheRest(t *testing.T) {
	tr := NewTracer(TraceLimits{Traces: 4})
	var ids []string
	for i := 0; i < 20; i++ {
		m := start(tr, "src", i)
		ids = append(ids, m.Trace.Trace.ID)
	}
	st := tr.Stats()
	if st.Kept != 4 || st.Evicted != 16 || st.Started != 20 {
		t.Fatalf("stats are %+v, want 4 kept, 16 evicted, 20 started", st)
	}
	kept := tr.Traces()
	for i, k := range kept {
		if want := ids[19-i]; k.ID != want {
			t.Errorf("kept[%d] is %s, want %s (newest first)", i, k.ID, want)
		}
	}
	if _, ok := tr.Get(ids[0]); ok {
		t.Error("the oldest trace is still there")
	}
}

func TestTheTracerStaysUnderItsByteBudget(t *testing.T) {
	tr := NewTracer(TraceLimits{Traces: 100000, MaxBytes: 64 << 10, MaxMessageBytes: 4 << 10})
	payload := strings.Repeat("x", 1000)
	for i := 0; i < 5000; i++ {
		start(tr, "src", payload)
	}
	st := tr.Stats()
	if st.Bytes > 64<<10 {
		t.Fatalf("holding %d bytes with a budget of %d", st.Bytes, 64<<10)
	}
	if st.Kept < 10 || st.Kept > 100 || st.Evicted < 4000 {
		t.Fatalf("stats are %+v", st)
	}
}

func TestATraceStopsAtItsHopLimitInsteadOfWritingALoopDownForever(t *testing.T) {
	tr := NewTracer(TraceLimits{MaxHops: 8})
	m := start(tr, "a", 1)
	for i := 0; i < 100; i++ {
		c, hop := tr.Receive(m, "b", "change", time.Now())
		c.Finish(hop, time.Microsecond, nil)
		tr.Outgoing(m, "b", "change", 0, "", c, hop, time.Now())
	}
	snap := tr.Traces()[0].Snapshot()
	if len(snap.Hops) != 8 || !snap.Truncated {
		t.Fatalf("%d hops, truncated=%v; want 8 and true", len(snap.Hops), snap.Truncated)
	}
	if tr.Stats().HopsDropped == 0 {
		t.Error("the dropped hops weren't counted")
	}
}

func TestAMessageTooBigToKeepIsMarkedNotCopied(t *testing.T) {
	tr := NewTracer(TraceLimits{MaxMessageBytes: 256})
	m := start(tr, "src", strings.Repeat("y", 10000))
	c, hop := tr.Receive(m, "dbg", "debug", time.Now())
	c.Finish(hop, 0, nil)
	snap := tr.Traces()[0].Snapshot()
	for _, h := range snap.Hops {
		if h.Msg != nil || !h.Cut {
			t.Errorf("hop %d kept a %v message, cut=%v", h.ID, h.Msg != nil, h.Cut)
		}
	}
}

func TestFindLocatesTheHopByMessageIDAndNode(t *testing.T) {
	tr := NewTracer(TraceLimits{})
	m := start(tr, "src", "a")
	id := m.ID()
	c, hop := tr.Receive(m, "dbg", "debug", time.Now())
	c.Finish(hop, 0, nil)
	got, h, ok := tr.Find(id, "dbg")
	if !ok || got.ID != c.ID || h != hop {
		t.Fatalf("Find gave %v %d %v", got, h, ok)
	}
	if _, _, ok := tr.Find(id, "nowhere"); ok {
		t.Error("found a hop at a node the message never reached")
	}
	if _, _, ok := tr.Find("nope", ""); ok {
		t.Error("found a message that never existed")
	}
}

func TestANilTracerDoesNothing(t *testing.T) {
	var tr *Tracer
	m := NewMsgWithPayload(1)
	tr.Outgoing(m, "a", "x", 0, "", nil, -1, time.Now())
	c, hop := tr.Receive(m, "b", "x", time.Now())
	c.Finish(hop, 0, nil)
	if m.Trace != nil || c != nil || hop != -1 {
		t.Fatalf("a nil tracer tagged a message: %+v", m.Trace)
	}
}

func TestACloneStaysOnTheTrace(t *testing.T) {
	tr := NewTracer(TraceLimits{})
	m := start(tr, "src", 1)
	if m.Clone().Trace != m.Trace {
		t.Fatal("a clone lost its trace")
	}
}

func TestThePerSecondLimitSkipsTheRestAndTheSkippedStayUntraced(t *testing.T) {
	tr := NewTracer(TraceLimits{PerSecond: 10})
	at := time.Unix(1_000_000, 0)
	var tagged, skipped int
	for i := 0; i < 100; i++ {
		m := NewMsgWithPayload(i)
		tr.Outgoing(m, "src", "inject", 0, "", nil, -1, at)
		if m.Trace == untraced {
			skipped++
			// Downstream never starts a trace for the tail of one it skipped.
			c, hop := tr.Receive(m, "next", "change", at)
			if c != nil || hop != -1 {
				t.Fatal("a skipped message was traced halfway")
			}
		} else {
			tagged++
		}
	}
	if tagged != 10 || skipped != 90 || tr.Stats().Started != 10 || tr.Stats().Skipped != 90 {
		t.Fatalf("%d traced, %d skipped, stats %+v", tagged, skipped, tr.Stats())
	}
	// The next second has its own ten.
	m := NewMsgWithPayload(0)
	tr.Outgoing(m, "src", "inject", 0, "", nil, -1, at.Add(time.Second))
	if m.Trace == untraced {
		t.Fatal("the limit didn't reset with the second")
	}
}
