package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// poisonSink refuses one payload forever and takes everything else, which is a
// historian choking on one reading it will never accept.
type poisonSink struct {
	poison   float64
	mu       sync.Mutex
	got      []float64
	attempts atomic.Int64 // on the poisoned payload only
}

func (p *poisonSink) Receive(_ context.Context, m *engine.Msg, _ node.Emitter) error {
	n, _ := m.Payload().(float64)
	if n == p.poison {
		p.attempts.Add(1)
		return fmt.Errorf("column quality does not accept %v", n)
	}
	p.mu.Lock()
	p.got = append(p.got, n)
	p.mu.Unlock()
	return nil
}

func (p *poisonSink) values() []float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]float64(nil), p.got...)
}

// parked is what the Catch node's output leads to: the place a flow would put
// a message it could not deliver.
type parked struct {
	mu   sync.Mutex
	msgs []*engine.Msg
}

func (c *parked) Receive(_ context.Context, m *engine.Msg, _ node.Emitter) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
	return nil
}

func (c *parked) all() []*engine.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*engine.Msg(nil), c.msgs...)
}

// giveUpFlow has a Catch that parks what it catches. attempts goes into the
// source node verbatim, so a test can say nothing, a limit, or nonsense.
func giveUpFlow(attempts string) string {
	return `[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"a","type":"pass","z":"t1","x":1,"y":1,"ew_durable":["b"]` + attempts + `,"wires":[["b"]]},
	  {"id":"b","type":"poison","z":"t1","x":2,"y":1,"wires":[]},
	  {"id":"c","type":"catch","z":"t1","scope":["b"],"x":1,"y":3,"wires":[["p"]]},
	  {"id":"p","type":"parked","z":"t1","x":2,"y":3,"wires":[]}
	]`
}

func startGiveUp(t *testing.T, flow string) (*durableRig, *poisonSink, *parked) {
	t.Helper()
	sink := &poisonSink{poison: 1}
	park := &parked{}
	tr := newTestRegistry()
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("poison", 1, 0, func(string) node.Node { return sink })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("parked", 1, 0, func(string) node.Node { return park })
	db, sp := openState(t, filepath.Join(t.TempDir(), "state.db"), 0)
	rt := New(tr.Registry, mustFlows(t, flow), Options{})
	rt.SetSpool(sp)
	el := drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("Start failures: %v", fails)
	}
	return &durableRig{rt: rt, sp: sp, db: db, el: el}, sink, park
}

func injectValues(t *testing.T, rt *Runtime, values ...float64) {
	t.Helper()
	for _, v := range values {
		if err := rt.Inject("a", engine.NewMsgWithPayload(v)); err != nil {
			t.Fatal(err)
		}
	}
}

func errorText(m *engine.Msg) string {
	e, _ := m.Data["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

// With no limit set the wire does what it always did: it holds on the refused
// message and tries it again, and the ones behind it wait. Nothing is dropped
// and nothing is given up on.
func TestDurableWireWithoutALimitKeepsRetrying(t *testing.T) {
	rig, sink, park := startGiveUp(t, giveUpFlow(""))
	defer rig.stop(t)
	injectValues(t, rig.rt, 0, 1, 2)

	// The pauses run 250ms, 500ms, 1s, so four tries are in by about two seconds.
	waitFor(t, "four tries at the refused message", func() bool { return sink.attempts.Load() >= 4 })
	q := rig.sp.Wire("a:0>b")
	if q.GaveUp() != 0 || q.Dropped() != 0 {
		t.Fatalf("gave up on %d and dropped %d with no limit set", q.GaveUp(), q.Dropped())
	}
	if q.Depth() < 1 {
		t.Fatalf("the refused message left the queue (depth %d) with no limit set", q.Depth())
	}
	if got := sink.values(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("the target took %v; message 2 must wait behind the refused one", got)
	}
	// Only the first refusal, in memory, reached the flow.
	if n := len(park.all()); n != 1 {
		t.Fatalf("the Catch parked %d messages, want the first refusal only", n)
	}
}

// A limit lets the wire get out: after that many tries the message goes to the
// Catch with the error, the wire moves on, and the ones behind it are delivered
// in order.
func TestDurableWireGivesUpAfterTheLimitAndMovesOn(t *testing.T) {
	rig, sink, park := startGiveUp(t, giveUpFlow(`,"ew_durable_attempts":{"b":3}`))
	defer rig.stop(t)
	injectValues(t, rig.rt, 0, 1, 2, 3)

	waitFor(t, "the others delivered", func() bool { return len(sink.values()) == 3 })
	sameSeq(t, sink.values(), []float64{0, 2, 3})

	q := rig.sp.Wire("a:0>b")
	waitFor(t, "the queue to empty", func() bool { return q.Depth() == 0 })
	if q.GaveUp() != 1 {
		t.Fatalf("gave up on %d messages, want 1", q.GaveUp())
	}
	// One try in memory, then three from the queue.
	if got := sink.attempts.Load(); got != 4 {
		t.Fatalf("the target saw the refused message %d times, want 1 in memory and 3 from the queue", got)
	}

	// Two messages reached the Catch: the first refusal, and the one that says
	// the wire gave up. The last carries the message that was refused.
	waitFor(t, "the Catch to park both", func() bool { return len(park.all()) == 2 })
	last := park.all()[1]
	if text := errorText(last); !strings.Contains(text, "gave up") || !strings.Contains(text, "3 attempts") || !strings.Contains(text, "column quality") {
		t.Fatalf("the error is %q, want it to say the wire gave up after 3 attempts and why", text)
	}
	if last.Payload() != float64(1) {
		t.Fatalf("the Catch got payload %v, want the refused message (1)", last.Payload())
	}
	src, _ := last.Data["error"].(map[string]any)["source"].(map[string]any)
	if src["id"] != "b" {
		t.Fatalf("the error came from %v, want the node that refused it (b)", src["id"])
	}
}

func TestDurableGiveUpsAreAMetricNextToTheDepth(t *testing.T) {
	rig, sink, _ := startGiveUp(t, giveUpFlow(`,"ew_durable_attempts":{"b":2}`))
	defer rig.stop(t)
	injectValues(t, rig.rt, 1, 5, 1)
	waitFor(t, "both refused messages given up on", func() bool { return rig.sp.Wire("a:0>b").GaveUp() == 2 })
	waitFor(t, "the good one delivered", func() bool { return len(sink.values()) == 1 })

	var depth, gaveUp bool
	for _, f := range rig.sp.Families() {
		for _, s := range f.Collect() {
			if len(s.Labels) != 1 || s.Labels[0] != [2]string{"wire", "a:0>b"} {
				continue
			}
			switch f.Name {
			case "hotloop_flow_durable_wire_depth":
				depth = true
			case "hotloop_flow_durable_wire_given_up_total":
				gaveUp = true
				if s.Value != 2 {
					t.Errorf("given-up metric is %v, want 2", s.Value)
				}
			}
		}
	}
	if !depth || !gaveUp {
		t.Fatalf("depth sample %v, given-up sample %v: both belong to the wire", depth, gaveUp)
	}
}

// Setting a limit on a running flow is wiring: the node is not restarted, and
// the message the wire is already holding is given up on under the new limit.
func TestAttemptLimitIsWiringAndAppliesToTheHeldMessage(t *testing.T) {
	rig, sink, _ := startGiveUp(t, giveUpFlow(""))
	defer rig.stop(t)
	injectValues(t, rig.rt, 1, 2)
	waitFor(t, "two tries at the refused message", func() bool { return sink.attempts.Load() >= 2 })

	res, err := rig.rt.Update(context.Background(), mustFlows(t, giveUpFlow(`,"ew_durable_attempts":{"b":2}`)), UpdateOptions{Mode: DeployNodes})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restarted) != 0 {
		t.Fatalf("setting an attempt limit restarted %v", res.Restarted)
	}
	waitFor(t, "the wire to give up on the held message", func() bool { return rig.sp.Wire("a:0>b").GaveUp() == 1 })
	waitFor(t, "the next message delivered", func() bool { return len(sink.values()) == 1 })
}

// A limit that is not a whole number of 1 or more is said out loud, and the wire
// stays on the default, which is to keep trying.
func TestAttemptLimitThatMakesNoSenseIsReportedAndIgnored(t *testing.T) {
	for _, bad := range []string{`{"b":0}`, `{"b":2.5}`, `{"b":"ten"}`} {
		t.Run(bad, func(t *testing.T) {
			rig, sink, _ := startGiveUp(t, giveUpFlow(`,"ew_durable_attempts":`+bad))
			defer rig.stop(t)
			injectValues(t, rig.rt, 1)
			waitFor(t, "a warning about the limit", func() bool {
				for _, e := range rig.el.byTopic(TopicLog) {
					if m, _ := e.Data["message"].(string); strings.Contains(m, "attempt limit") {
						return true
					}
				}
				return false
			})
			waitFor(t, "a second try", func() bool { return sink.attempts.Load() >= 2 })
			if g := rig.sp.Wire("a:0>b").GaveUp(); g != 0 {
				t.Fatalf("gave up %d times on a limit that makes no sense", g)
			}
		})
	}
}
