package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// flakySink stands in for a historian: it takes messages in order, and while
// down is set it fails every one it is handed.
type flakySink struct {
	mu       sync.Mutex
	got      []float64
	down     atomic.Bool
	attempts atomic.Int64
	failFor  atomic.Int64 // fail this many more attempts, then work
	gate     chan struct{}
}

func (f *flakySink) Receive(_ context.Context, m *engine.Msg, _ node.Emitter) error {
	f.attempts.Add(1)
	if f.gate != nil {
		<-f.gate
	}
	if f.down.Load() {
		return fmt.Errorf("historian is down")
	}
	if f.failFor.Load() > 0 {
		f.failFor.Add(-1)
		return fmt.Errorf("historian said no")
	}
	n, _ := m.Payload().(float64)
	f.mu.Lock()
	f.got = append(f.got, n)
	f.mu.Unlock()
	return nil
}

func (f *flakySink) values() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.got...)
}

func (f *flakySink) count() int { return len(f.values()) }

func openState(t *testing.T, path string, max int64) (*store.SQLite, *store.Spool) {
	t.Helper()
	db, err := store.OpenSQLite(path, store.SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := store.OpenSpool(db, store.SpoolOptions{MaxBytes: max, AckFlush: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return db, sp
}

const durableFlow = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"a","type":"pass","z":"t1","x":1,"y":1,"ew_durable":["b"],"wires":[["b"]]},
  {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
]`

// spillFlow is durableFlow with a target whose inbox holds one message, so a
// target that is stuck or down pushes the wire onto disk at once.
const spillFlow = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"a","type":"pass","z":"t1","x":1,"y":1,"ew_durable":["b"],"wires":[["b"]]},
  {"id":"b","type":"sink","z":"t1","x":2,"y":1,"ew_inboxCapacity":1,"wires":[]}
]`

type durableRig struct {
	rt   *Runtime
	sink *flakySink
	sp   *store.Spool
	db   *store.SQLite
	el   *eventLog
}

func startDurable(t *testing.T, path string, flow string, opts Options, max int64, sink *flakySink) *durableRig {
	t.Helper()
	tr := newTestRegistry()
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	db, sp := openState(t, path, max)
	rt := New(tr.Registry, mustFlows(t, flow), opts)
	rt.SetSpool(sp)
	el := drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("Start failures: %v", fails)
	}
	return &durableRig{rt: rt, sink: sink, sp: sp, db: db, el: el}
}

// stop shuts the runtime and the store down the way the process does.
func (d *durableRig) stop(t *testing.T) {
	t.Helper()
	d.rt.Stop(context.Background())
	if err := d.sp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.db.Close(); err != nil {
		t.Fatal(err)
	}
}

func inject(t *testing.T, rt *Runtime, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		if err := rt.Inject("a", engine.NewMsgWithPayload(float64(i))); err != nil {
			t.Fatal(err)
		}
	}
}

func seq(from, to int) []float64 {
	out := make([]float64, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, float64(i))
	}
	return out
}

func sameSeq(t *testing.T, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d (first got %v)", len(got), len(want), head(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message %d is %v, want %v: order or content is wrong", i, got[i], want[i])
		}
	}
}

func head(v []float64) []float64 {
	if len(v) > 10 {
		return v[:10]
	}
	return v
}

// The point of the whole feature: the target is down, messages keep coming, the
// process stops, and a new process delivers every one of them, in order, once.
func TestDurableWireSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	down := &flakySink{}
	down.down.Store(true)
	rig := startDurable(t, path, durableFlow, Options{}, 0, down)
	inject(t, rig.rt, 0, 200)
	waitFor(t, "200 messages on disk", func() bool { return rig.sp.Wire("a:0>b").Depth() == 200 })
	if down.count() != 0 {
		t.Fatalf("a downed target took %d messages", down.count())
	}
	rig.stop(t)

	up := &flakySink{}
	rig = startDurable(t, path, durableFlow, Options{}, 0, up)
	defer rig.stop(t)
	waitFor(t, "the backlog to be delivered", func() bool { return up.count() == 200 })
	sameSeq(t, up.values(), seq(0, 200))
	waitFor(t, "the queue to empty", func() bool { return rig.sp.Wire("a:0>b").Depth() == 0 })
}

// The same thing without a clean stop: the files as they are on disk at the
// moment of the kill are what the next process starts from.
func TestDurableWireSurvivesAKill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	down := &flakySink{}
	down.down.Store(true)
	rig := startDurable(t, path, durableFlow, Options{}, 0, down)
	defer rig.stop(t)
	inject(t, rig.rt, 0, 100)
	waitFor(t, "100 messages on disk", func() bool {
		// A flush request goes through the writer behind every append already
		// submitted, so when it returns they are committed.
		_ = rig.sp.FlushAcks()
		return rig.sp.Wire("a:0>b").Depth() == 100
	})

	// What a kill -9 leaves: the database and its log, copied as they stand
	// while the first process is still alive and has not closed anything.
	killed := filepath.Join(dir, "killed")
	if err := os.MkdirAll(killed, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		copyIfExists(t, path+suffix, filepath.Join(killed, "state.db"+suffix))
	}

	up := &flakySink{}
	after := startDurable(t, filepath.Join(killed, "state.db"), durableFlow, Options{}, 0, up)
	defer after.stop(t)
	waitFor(t, "the survivors to be delivered", func() bool { return up.count() == 100 })
	sameSeq(t, up.values(), seq(0, 100))
}

func copyIfExists(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}

// A target with a tiny inbox that is stuck would, on an ordinary wire, make the
// sender block or drop. On a durable wire the sender is never held up and
// nothing is dropped.
func TestDurableWireNeitherBlocksNorDrops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	flow := `[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"a","type":"pass","z":"t1","x":1,"y":1,"ew_durable":["b"],"wires":[["b"]]},
	  {"id":"b","type":"sink","z":"t1","x":2,"y":1,"ew_inboxCapacity":2,"ew_overflow":"drop-newest","wires":[]}
	]`
	sink := &flakySink{gate: make(chan struct{})}
	rig := startDurable(t, path, flow, Options{}, 0, sink)
	defer rig.stop(t)

	inject(t, rig.rt, 0, 500)
	processed(t, rig.rt, 500)
	if d := rig.sp.Wire("a:0>b").Depth(); d < 490 {
		t.Fatalf("only %d of 500 are on disk with the target stuck", d)
	}
	for _, s := range rig.rt.Snapshots() {
		if s.Dropped != 0 {
			t.Fatalf("node %s dropped %d messages on a durable wire", s.NodeID, s.Dropped)
		}
	}

	close(sink.gate)
	waitFor(t, "all 500 delivered", func() bool { return sink.count() == 500 })
	sameSeq(t, sink.values(), seq(0, 500))
}

// A message the target fails is sent again, nothing behind it overtakes it, and
// the flow's Catch nodes hear about it once and not on every retry.
func TestDurableWireRetriesInOrderAndReportsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	flow := `[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"a","type":"pass","z":"t1","x":1,"y":1,"ew_durable":["b"],"wires":[["b"]]},
	  {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[]},
	  {"id":"c","type":"catch","z":"t1","scope":null,"x":1,"y":3,"wires":[]}
	]`
	sink := &flakySink{}
	sink.failFor.Store(3)
	rig := startDurable(t, path, flow, Options{}, 0, sink)
	defer rig.stop(t)
	inject(t, rig.rt, 0, 20)
	waitFor(t, "all delivered", func() bool { return sink.count() == 20 })
	sameSeq(t, sink.values(), seq(0, 20))
	if got := sink.attempts.Load(); got != 23 {
		t.Errorf("target saw %d attempts, want 20 messages plus 3 retries", got)
	}
	// The first failure happened in memory and put the message on disk; the
	// other two were the wire trying again from there.
	if got := rig.sp.Wire("a:0>b").Retries(); got != 2 {
		t.Errorf("retry counter is %d, want 2", got)
	}
	if got := len(rig.el.byTopic(TopicError)); got != 1 {
		t.Errorf("%d errors reached the flow, want 1: the first failure, not every retry", got)
	}
}

func quotaFlow(overflow string) string {
	return fmt.Sprintf(`[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"a","type":"pass","z":"t1","x":1,"y":1,"ew_durable":["b"],"wires":[["b"]]},
	  {"id":"b","type":"sink","z":"t1","x":2,"y":1,"ew_inboxCapacity":1,"ew_overflow":%q,"wires":[]},
	  {"id":"c","type":"catch","z":"t1","scope":null,"x":1,"y":3,"wires":[]}
	]`, overflow)
}

// 40 messages of this size cost about 48 bytes each plus RowOverhead, so a
// quota of 4 KiB fits some and not all.
func quotaRig(t *testing.T, overflow string, opts Options) (*durableRig, *flakySink) {
	sink := &flakySink{gate: make(chan struct{})}
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), quotaFlow(overflow), opts, 4096, sink)
	return rig, sink
}

// processed waits until node a has handled every injected message, and has
// finished handing each one to its wire. Received is counted as a handler
// starts and the message is only on disk, or refused, when the handler returns,
// so Received with an empty inbox says the last message has begun, not that it
// has landed. A test that read the quota or the drop count on that word read it
// with the 200th message still on its way, and when the gate opened the
// freed room let that message in after the reading. Sent is counted inside the
// handler, so Sent == n means the last handler is running and idle means it
// has returned.
func processed(t *testing.T, rt *Runtime, n int64) {
	t.Helper()
	waitFor(t, "node a to handle every message", func() bool {
		r := rt.graph().runners["a"]
		s := r.Snapshot()
		return s.Received == n && s.Sent == n && s.QueueLen == 0 && r.idle()
	})
}

// quotaSlack is how far past the quota the spool may sit. A message already
// accepted into memory that the wire then diverts to disk goes in over the
// quota, because refusing it would drop an older message and keep newer ones
// (store.AppendSeqOver, and durableWire.memDone). The overshoot is bounded by
// what the target holds: its inbox, plus the one its goroutine has taken and
// not yet started.
func quotaSlack(q *store.WireQueue, sp *store.Spool, inbox int) int64 {
	return int64(inbox+1) * (sp.Bytes() / q.Depth())
}

func TestDurableQuotaDropNewest(t *testing.T) {
	rig, sink := quotaRig(t, "drop-newest", Options{})
	defer rig.stop(t)
	inject(t, rig.rt, 0, 200)
	processed(t, rig.rt, 200)
	q := rig.sp.Wire("a:0>b")
	if q.Dropped() == 0 {
		t.Fatal("the quota was never reached; the test proves nothing")
	}
	if limit := rig.sp.MaxBytes() + quotaSlack(q, rig.sp, 1); rig.sp.Bytes() > limit {
		t.Fatalf("quota used %d of %d, and the most it may run over is %d", rig.sp.Bytes(), rig.sp.MaxBytes(), limit-rig.sp.MaxBytes())
	}
	kept := 200 - int(q.Dropped())
	close(sink.gate)
	waitFor(t, "the kept messages", func() bool { return sink.count() == kept })
	// The oldest are the ones kept, in order.
	sameSeq(t, sink.values(), seq(0, kept))
}

func TestDurableQuotaDropOldest(t *testing.T) {
	rig, sink := quotaRig(t, "drop-oldest", Options{})
	defer rig.stop(t)
	inject(t, rig.rt, 0, 200)
	processed(t, rig.rt, 200)
	q := rig.sp.Wire("a:0>b")
	if q.Dropped() == 0 {
		t.Fatal("the quota was never reached; the test proves nothing")
	}
	if limit := rig.sp.MaxBytes() + quotaSlack(q, rig.sp, 1); rig.sp.Bytes() > limit {
		t.Fatalf("quota used %d of %d, and the most it may run over is %d", rig.sp.Bytes(), rig.sp.MaxBytes(), limit-rig.sp.MaxBytes())
	}
	close(sink.gate)
	waitFor(t, "the queue to drain", func() bool { return q.Depth() == 0 })
	got := sink.values()
	if got[len(got)-1] != 199 {
		t.Fatalf("the newest message is not the last one delivered: %v", got[len(got)-3:])
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("order broken at %d: %v then %v", i, got[i-1], got[i])
		}
	}
	if int64(len(got))+q.Dropped() != 200 {
		t.Fatalf("%d delivered plus %d dropped is not 200", len(got), q.Dropped())
	}
}

func TestDurableQuotaErrorReachesCatch(t *testing.T) {
	rig, sink := quotaRig(t, "error", Options{})
	defer rig.stop(t)
	defer close(sink.gate)
	inject(t, rig.rt, 0, 200)
	waitFor(t, "a quota error at the Catch", func() bool { return len(rig.el.byTopic(TopicError)) > 0 })
}

func TestDurableQuotaBlockTimesOutInsteadOfWedging(t *testing.T) {
	rig, sink := quotaRig(t, "block", Options{BlockTimeout: 150 * time.Millisecond})
	defer rig.stop(t)
	defer close(sink.gate)
	inject(t, rig.rt, 0, 200)
	waitFor(t, "a block timeout at the Catch", func() bool { return len(rig.el.byTopic(TopicError)) > 0 })
	if rig.sp.Wire("a:0>b").Dropped() == 0 {
		t.Fatal("a timed-out message was not counted as dropped")
	}
}

// Blocking is a wait, not a drop: when the target frees space inside the
// timeout, the message goes in.
func TestDurableQuotaBlockWaitsForSpace(t *testing.T) {
	rig, sink := quotaRig(t, "block", Options{BlockTimeout: 20 * time.Second})
	defer rig.stop(t)
	inject(t, rig.rt, 0, 200)
	waitFor(t, "the quota to fill", func() bool { return rig.sp.Bytes() > 3500 })
	time.Sleep(200 * time.Millisecond)
	for _, s := range rig.rt.Snapshots() {
		if s.NodeID == "a" && s.Received == 200 && s.QueueLen == 0 {
			t.Fatal("node a got through 200 messages with the quota full: the sender was not held")
		}
	}
	close(sink.gate)
	processed(t, rig.rt, 200)
	waitFor(t, "all 200 delivered", func() bool { return sink.count() == 200 })
	sameSeq(t, sink.values(), seq(0, 200))
	if d := rig.sp.Wire("a:0>b").Dropped(); d != 0 {
		t.Fatalf("%d dropped while blocking", d)
	}
}

func TestDurableDepthIsAMetric(t *testing.T) {
	sink := &flakySink{gate: make(chan struct{})}
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), spillFlow, Options{}, 0, sink)
	defer rig.stop(t)
	defer close(sink.gate)
	inject(t, rig.rt, 0, 25)
	processed(t, rig.rt, 25)
	q := rig.sp.Wire("a:0>b")
	waitFor(t, "the backlog on disk", func() bool { return q.Depth() >= 23 })

	var found bool
	for _, f := range rig.sp.Families() {
		if f.Name != "hotloop_flow_durable_wire_depth" {
			continue
		}
		for _, s := range f.Collect() {
			if len(s.Labels) == 1 && s.Labels[0] == [2]string{"wire", "a:0>b"} {
				found = true
				if s.Value != float64(q.Depth()) {
					t.Errorf("depth metric is %v, the queue holds %d", s.Value, q.Depth())
				}
			}
		}
	}
	if !found {
		t.Fatal("no depth sample for the wire")
	}
}

// A wire that stops being durable has nowhere for its queue to go, so the queue
// is purged and counted rather than left to fill the quota unseen.
func TestDurableWireRemovedPurgesItsQueue(t *testing.T) {
	sink := &flakySink{gate: make(chan struct{})}
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), spillFlow, Options{}, 0, sink)
	defer rig.stop(t)
	defer close(sink.gate)
	const sent = 30
	inject(t, rig.rt, 0, sent)
	processed(t, rig.rt, sent)
	q := rig.sp.Wire("a:0>b")
	waitFor(t, "the backlog on disk", func() bool { return q.Depth() >= 28 })
	// Only a floor. Depth() is still moving when it is read: the spool's writer
	// counts a message after its commit, on its own goroutine, and a message
	// the target's goroutine was holding is written when it gets to it. How
	// many of the 30 are on disk by the time the wire goes depends on the
	// machine (28 to 30 in the stress runs), so the purge can't be held to the
	// number seen here, which is what this test did and why it failed once on a
	// slow runner: 30 dropped against the 29 sampled.
	queued := q.Depth()

	if _, err := rig.rt.Update(context.Background(), mustFlows(t, notDurable(spillFlow)), UpdateOptions{Mode: DeployNodes}); err != nil {
		t.Fatal(err)
	}
	if q.Depth() != 0 || rig.sp.Bytes() != 0 {
		t.Fatalf("queue depth %d and %d bytes charged after the wire stopped being durable", q.Depth(), rig.sp.Bytes())
	}
	// What was on disk at the sample must all be counted, and the purge can't
	// count more than was sent. A queue that kept rows, or a purge that missed
	// them, drops below the first; double counting goes above the second.
	if d := q.Dropped(); d < queued || d > sent {
		t.Fatalf("%d counted dropped, want between the %d queued when sampled and the %d sent", d, queued, sent)
	}
	if n := sink.count(); n != 0 {
		t.Fatalf("%d delivered to a target that never took one", n)
	}
}

// notDurable is the same flow with the wire no longer marked. It has to be the
// same in every other way: a node whose settings differ is a node the deploy
// replaces, and the deploy waits for a replaced node to finish its message.
// This test once swapped in a flow without the sink's inbox capacity, so the
// sink was replaced while the test held it inside a message, and every run
// spent the 15 second close timeout there. Sixty runs of it hit go test's ten
// minute limit.
func notDurable(flow string) string {
	return strings.Replace(flow, `"ew_durable":["b"],`, "", 1)
}

// A message the target hands back to a wire that has since been taken out of
// the flow has nowhere to go. It used to be written to the queue anyway, after
// the purge, and sat there charged to the quota with nothing to read it. This
// is that message, handed back after the wire is gone: it is counted dropped
// and nothing is written.
func TestDurableWireRemovedRefusesWhatComesBackLate(t *testing.T) {
	sink := &flakySink{gate: make(chan struct{})}
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), spillFlow, Options{}, 0, sink)
	defer rig.stop(t)
	defer close(sink.gate)
	rig.rt.durableMu.Lock()
	w := rig.rt.durable["a:0>b"]
	rig.rt.durableMu.Unlock()
	if w == nil {
		t.Fatal("the durable wire is not running")
	}
	if _, err := rig.rt.Update(context.Background(), mustFlows(t, notDurable(spillFlow)), UpdateOptions{Mode: DeployNodes}); err != nil {
		t.Fatal(err)
	}
	q := w.q
	before := q.Dropped()

	// What the target's goroutine does when it finds the wire spilling, and
	// what a sender does that was already inside offer when the wire went.
	w.memDone(w.q.AllocSeq(), engine.NewMsgWithPayload(1.0), errDropped)
	w.appendSpill(w.q.AllocSeq(), engine.NewMsgWithPayload(2.0), nil)

	if q.Depth() != 0 || rig.sp.Bytes() != 0 {
		t.Fatalf("queue depth %d and %d bytes charged after messages came back to a removed wire", q.Depth(), rig.sp.Bytes())
	}
	if got := q.Dropped() - before; got != 2 {
		t.Fatalf("%d counted dropped, want the 2 that came back", got)
	}
}

// Taking a durable wire out of a flow waits for a delivery in flight only as
// long as it takes a healthy target to answer. A target that is stuck in a
// message cost the whole close timeout, 15 seconds, with the deploy held, to
// protect a message that is purged a moment later anyway.
func TestDurableWireRemovedDoesNotWaitOnAStuckTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	down := &flakySink{}
	down.down.Store(true)
	rig := startDurable(t, path, durableFlow, Options{}, 0, down)
	inject(t, rig.rt, 0, 10)
	waitFor(t, "10 messages on disk", func() bool { return rig.sp.Wire("a:0>b").Depth() == 10 })
	rig.stop(t)

	// The backlog is on disk before this runtime starts, so its pump delivers
	// from disk at once, into a target that takes the first message and never
	// finishes it.
	stuck := &flakySink{gate: make(chan struct{})}
	rig = startDurable(t, path, durableFlow, Options{}, 0, stuck)
	defer rig.stop(t)
	defer close(stuck.gate)
	waitFor(t, "the target to be inside a message", func() bool { return stuck.attempts.Load() == 1 })

	start := time.Now()
	if _, err := rig.rt.Update(context.Background(), mustFlows(t, notDurable(durableFlow)), UpdateOptions{Mode: DeployNodes}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("removing the wire held the deploy for %s", took)
	}
	if q := rig.sp.Wire("a:0>b"); q.Depth() != 0 || rig.sp.Bytes() != 0 {
		t.Fatalf("queue depth %d and %d bytes charged after the wire was removed", q.Depth(), rig.sp.Bytes())
	}
}

// Marking a wire durable on a running flow is a wiring change: the node is not
// restarted for it, and the new wire carries the next message.
func TestMarkingAWireDurableDoesNotRestartTheNode(t *testing.T) {
	sink := &flakySink{}
	plain := `[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"a","type":"pass","z":"t1","x":1,"y":1,"wires":[["b"]]},
	  {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
	]`
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), plain, Options{}, 0, sink)
	defer rig.stop(t)
	inject(t, rig.rt, 0, 5)
	waitFor(t, "5 plain", func() bool { return sink.count() == 5 })

	res, err := rig.rt.Update(context.Background(), mustFlows(t, durableFlow), UpdateOptions{Mode: DeployNodes})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restarted) != 0 {
		t.Fatalf("marking a wire durable restarted %v", res.Restarted)
	}
	inject(t, rig.rt, 5, 10)
	waitFor(t, "5 more", func() bool { return sink.count() == 10 })
	sameSeq(t, sink.values(), seq(0, 10))
}

func TestMessageTypesSurviveTheQueue(t *testing.T) {
	m := engine.NewMsgWithPayload(map[string]any{"b": []byte{1, 2, 3}, "t": time.Unix(1700000000, 5).UTC()})
	m.Data["topic"] = "plant/line1"
	body, skipped, err := store.EncodeMsg(m)
	if len(skipped) != 0 {
		t.Fatalf("skipped %v", skipped)
	}
	if err != nil {
		t.Fatal(err)
	}
	back, err := store.DecodeMsg(body)
	if err != nil {
		t.Fatal(err)
	}
	p := back.Payload().(map[string]any)
	if b, ok := p["b"].([]byte); !ok || len(b) != 3 || b[2] != 3 {
		t.Errorf("bytes came back as %#v", p["b"])
	}
	if tm, ok := p["t"].(time.Time); !ok || !tm.Equal(time.Unix(1700000000, 5)) {
		t.Errorf("time came back as %#v", p["t"])
	}
	if back.Data["topic"] != "plant/line1" {
		t.Errorf("topic came back as %#v", back.Data["topic"])
	}
}

// A live handle on the message, like the response an HTTP In node attaches, is
// left out and reported, not the reason the whole message is refused. The
// payload is never left out.
func TestDurableWireLeavesOutHandlesAndNeverThePayload(t *testing.T) {
	m := engine.NewMsgWithPayload(1.0)
	m.Data["res"] = make(chan int)
	body, skipped, err := store.EncodeMsg(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "res" {
		t.Fatalf("skipped %v, want [res]", skipped)
	}
	back, err := store.DecodeMsg(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := back.Data["res"]; ok || back.Payload() != 1.0 {
		t.Fatalf("decoded %#v", back.Data)
	}

	bad := engine.NewMsgWithPayload(make(chan int))
	if _, _, err := store.EncodeMsg(bad); err == nil {
		t.Fatal("a payload that can't be stored was accepted")
	}
}

// Each message gets the time it entered the wire, and no two on one wire share
// one, so a sink that writes by time can tell them apart and can tell a repeat
// from a new reading.
func TestDurableWireStampsWhenAMessageQueued(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	sink := &flakySink{gate: make(chan struct{})}
	rig := startDurable(t, path, spillFlow, Options{Now: func() time.Time { return time.Unix(1700000000, 0) }}, 0, sink)
	defer rig.stop(t)
	defer close(sink.gate)
	inject(t, rig.rt, 0, 50)
	processed(t, rig.rt, 50)
	q := rig.sp.Wire("a:0>b")
	waitFor(t, "the backlog on disk", func() bool { return q.Depth() >= 48 })

	var cursor uint64
	var last time.Time
	rows := 0
	for {
		e, ok, err := q.Next(cursor)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		m, err := store.DecodeMsg(e.Body)
		if err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339Nano, m.Data["_queuedAt"].(string))
		if err != nil {
			t.Fatal(err)
		}
		// The clock is frozen, so the wire alone makes them differ.
		if !at.After(last) {
			t.Fatalf("row %d is stamped %v, not after %v", rows, at, last)
		}
		last, cursor = at, e.Seq
		rows++
	}
	if rows < 48 {
		t.Fatalf("read %d rows", rows)
	}
}

// The design rule: while the target keeps up, nothing is written. A thousand
// messages through a healthy wire commit no transaction and leave the queue empty.
func TestDurableWireWritesNothingWhileTheTargetKeepsUp(t *testing.T) {
	sink := &flakySink{}
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), durableFlow, Options{}, 0, sink)
	defer rig.stop(t)
	before := rig.sp.Commits()
	inject(t, rig.rt, 0, 1000)
	waitFor(t, "1000 delivered", func() bool { return sink.count() == 1000 })
	sameSeq(t, sink.values(), seq(0, 1000))
	if got := rig.sp.Commits() - before; got != 0 {
		t.Fatalf("%d transactions written for a healthy wire, want none", got)
	}
	if d := rig.sp.Wire("a:0>b").Depth(); d != 0 {
		t.Fatalf("%d messages on disk for a healthy wire", d)
	}
}

// Down, then back: the wire spills in order, drains from disk, and goes back to
// writing nothing.
func TestDurableWireSpillsThenReturnsToMemory(t *testing.T) {
	sink := &flakySink{}
	sink.down.Store(true)
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), durableFlow, Options{}, 0, sink)
	defer rig.stop(t)
	q := rig.sp.Wire("a:0>b")

	inject(t, rig.rt, 0, 100)
	waitFor(t, "100 on disk", func() bool { _ = rig.sp.FlushAcks(); return q.Depth() == 100 })

	sink.down.Store(false)
	waitFor(t, "the backlog to drain", func() bool { return sink.count() == 100 && q.Depth() == 0 })
	// Messages sent while it drains, and after, still arrive in order.
	inject(t, rig.rt, 100, 200)
	waitFor(t, "all 200", func() bool { return sink.count() == 200 })
	sameSeq(t, sink.values(), seq(0, 200))

	// Back in memory: more traffic, no more writes.
	waitFor(t, "the wire to go back to memory", func() bool {
		rig.sp.FlushAcks()
		before := rig.sp.Commits()
		inject(t, rig.rt, 1000, 1001)
		time.Sleep(50 * time.Millisecond)
		return rig.sp.Commits() == before
	})
}

// A drop-oldest durable wire failed its drain once in a few thousand runs, and
// the one dump caught showed the pump idle with messages queued. A rerun hides
// that, so this loop runs the scenario again and again and holds each pass to
// what a healthy wire does: every message delivered or counted dropped, the
// queue empty in well under the pump's five second poll (a wake that was lost
// and found by the poll would otherwise pass), and the stranded counter at
// zero. A failure prints the wire's counters beside its table.
//
// HOTLOOP_FLOW_STRESS sets the passes; the default is small enough for CI.
func TestDurableDropOldestStress(t *testing.T) {
	passes := 150
	if v := os.Getenv("HOTLOOP_FLOW_STRESS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("HOTLOOP_FLOW_STRESS=%q is not a count", v)
		}
		passes = n
	}
	for i := 0; i < passes; i++ {
		if !dropOldestPass(t, i) {
			return
		}
	}
}

func dropOldestPass(t *testing.T, pass int) bool {
	sink := &flakySink{gate: make(chan struct{})}
	rig := startDurable(t, filepath.Join(t.TempDir(), "state.db"), quotaFlow("drop-oldest"), Options{}, 4096, sink)
	defer rig.stop(t)
	inject(t, rig.rt, 0, 200)
	processed(t, rig.rt, 200)
	q := rig.sp.Wire("a:0>b")
	close(sink.gate)
	start := time.Now()
	for q.Depth() != 0 || q.Dropped()+int64(sink.count()) != 200 {
		if time.Since(start) > time.Second {
			p, _ := q.Probe()
			t.Errorf("pass %d: not drained after %s: delivered %d, dropped %d, stranded %d, probe %+v",
				pass, time.Since(start), sink.count(), q.Dropped(), q.Stranded(), p)
			return false
		}
		time.Sleep(time.Millisecond)
	}
	if q.Stranded() != 0 {
		t.Errorf("pass %d: stranded counted %d", pass, q.Stranded())
		return false
	}
	got := sink.values()
	for j := 1; j < len(got); j++ {
		if got[j] <= got[j-1] {
			t.Errorf("pass %d: order broken at %d: %v then %v", pass, j, got[j-1], got[j])
			return false
		}
	}
	return true
}
