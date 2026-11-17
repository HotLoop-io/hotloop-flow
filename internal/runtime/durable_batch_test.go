package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// batchSink is a target that takes batches. It records what it took, in order,
// and fails on request: a whole batch, or one poisoned message that it will not
// take for its first few tries.
type batchSink struct {
	flakySink
	failBatches atomic.Int64 // fail this many more batch calls, whole
	poison      float64
	poisonFails atomic.Int64
	batches     atomic.Int64
	biggest     atomic.Int64
	singles     atomic.Int64
	bmu         sync.Mutex
}

var errPoison = errors.New("the historian will not take this one yet")

func (b *batchSink) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	b.singles.Add(1)
	if v, _ := m.Payload().(float64); v == b.poison && b.poisonFails.Load() > 0 {
		b.poisonFails.Add(-1)
		return errPoison
	}
	return b.flakySink.Receive(ctx, m, out)
}

func (b *batchSink) ReceiveBatch(_ context.Context, msgs []*engine.Msg, _ []node.Emitter) []error {
	b.batches.Add(1)
	if n := int64(len(msgs)); n > b.biggest.Load() {
		b.biggest.Store(n)
	}
	errs := make([]error, len(msgs))
	if b.down.Load() {
		for i := range errs {
			errs[i] = errors.New("historian is down")
		}
		return errs
	}
	if b.failBatches.Load() > 0 {
		b.failBatches.Add(-1)
		for i := range errs {
			errs[i] = errors.New("the historian is down")
		}
		return errs
	}
	for i, m := range msgs {
		v, _ := m.Payload().(float64)
		if v == b.poison && b.poisonFails.Load() > 0 {
			b.poisonFails.Add(-1)
			errs[i] = errPoison
			for j := i + 1; j < len(errs); j++ {
				errs[j] = errors.New("held behind an earlier message")
			}
			return errs
		}
		b.mu.Lock()
		b.got = append(b.got, v)
		b.mu.Unlock()
	}
	return errs
}

// startBatchDurableWith runs the durable flow with the batch sink as the target.
func startBatchDurableWith(t *testing.T, path string, sink *batchSink) *durableRig {
	t.Helper()
	tr := newTestRegistry()
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	db, sp := openState(t, path, 0)
	rt := New(tr.Registry, mustFlows(t, durableFlow), Options{})
	rt.SetSpool(sp)
	el := drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("Start failures: %v", fails)
	}
	return &durableRig{rt: rt, sink: &sink.flakySink, sp: sp, db: db, el: el}
}

// A backlog is delivered in batches, in order, and once.
func TestDurableDrainTakesABacklogInBatchesInOrder(t *testing.T) {
	sink := &batchSink{poison: -1}
	sink.down.Store(true)
	rig := startBatchDurableWith(t, filepath.Join(t.TempDir(), "state.db"), sink)
	defer rig.stop(t)
	inject(t, rig.rt, 0, 1000)
	waitFor(t, "the backlog on disk", func() bool { return rig.sp.Wire("a:0>b").Depth() == 1000 })
	sink.down.Store(false)
	waitFor(t, "the backlog delivered", func() bool { return sink.count() == 1000 })
	sameSeq(t, sink.values(), seq(0, 1000))
	if sink.batches.Load() == 0 || sink.biggest.Load() < 2 {
		t.Fatalf("the drain made %d batch calls, the largest %d: it did not batch", sink.batches.Load(), sink.biggest.Load())
	}
	waitFor(t, "the queue to empty", func() bool { return rig.sp.Wire("a:0>b").Depth() == 0 })
}

// A target that refuses whole batches for a while, and then one message in the
// middle, loses nothing and reorders nothing: the messages before the bad one are
// kept, the bad one is retried alone, and the ones after it follow.
func TestDurableDrainKeepsOrderThroughFailures(t *testing.T) {
	sink := &batchSink{poison: 437}
	sink.down.Store(true)
	rig := startBatchDurableWith(t, filepath.Join(t.TempDir(), "state.db"), sink)
	defer rig.stop(t)
	inject(t, rig.rt, 0, 900)
	waitFor(t, "the backlog on disk", func() bool { return rig.sp.Wire("a:0>b").Depth() == 900 })
	sink.failBatches.Store(3)
	sink.poisonFails.Store(4)
	sink.down.Store(false)
	deadline := time.Now().Add(30 * time.Second)
	for sink.count() != 900 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("batches %d singles %d got %d", sink.batches.Load(), sink.singles.Load(), sink.count())
	sameSeq(t, sink.values(), seq(0, 900))
	if sink.poisonFails.Load() != 0 {
		t.Fatalf("the poisoned message was never refused: %d refusals left", sink.poisonFails.Load())
	}
	if sink.singles.Load() == 0 {
		t.Fatal("a refused message was not retried on its own")
	}
	waitFor(t, "the queue to empty", func() bool { return rig.sp.Wire("a:0>b").Depth() == 0 })
}

// A kill in the middle of a batched drain: the next process finishes the queue,
// every message at least once and in order, with a repeat only for what was in
// flight.
func TestDurableDrainSurvivesARestartMidDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	sink := &batchSink{poison: -1}
	sink.down.Store(true)
	rig := startBatchDurableWith(t, path, sink)
	inject(t, rig.rt, 0, 600)
	waitFor(t, "the backlog on disk", func() bool { return rig.sp.Wire("a:0>b").Depth() == 600 })
	rig.stop(t)

	// A fresh process, a target that takes a little and then stops taking.
	first := &batchSink{poison: -1}
	first.failBatches.Store(0)
	rig = startBatchDurableWith(t, path, first)
	waitFor(t, "some of the backlog delivered", func() bool { return first.count() > 0 })
	rig.stop(t)
	taken := first.count()

	second := &batchSink{poison: -1}
	rig = startBatchDurableWith(t, path, second)
	defer rig.stop(t)
	_ = taken
	waitFor(t, "the rest delivered", func() bool {
		v := second.values()
		return len(v) > 0 && v[len(v)-1] == 599
	})
	all := append(first.values(), second.values()...)
	// At-least-once: what was in flight when the first process stopped may come
	// again. Walking the two runs together, no message may arrive after a later
	// one, and none may be missing.
	t.Logf("first took %d (head %v), second took %d (head %v)", len(first.values()), head(first.values()), len(second.values()), head(second.values()))
	next := float64(0)
	for _, v := range all {
		switch {
		case v == next:
			next++
		case v < next:
			// a repeat of something already taken
		default:
			t.Fatalf("message %v arrived while %v was still owed", v, next)
		}
	}
	if next != 600 {
		t.Fatalf("delivered up to %v of 600", next)
	}
}
