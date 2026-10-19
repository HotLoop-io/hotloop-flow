package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// Durable wires.
//
// A wire is marked durable on the node it leaves, in a key other tools ignore:
//
//	"ew_durable": ["<id of the node the wire goes to>"]
//
// While the target keeps up, a durable wire is an ordinary wire: the message goes
// from memory into the target's inbox and nothing is written to disk. That is on
// purpose. Writing every message first would put a flash card through
// gigabytes a day for a failure that happens a few hours a year.
//
// The wire spills when the target stops taking messages: its handler returns an
// error, its inbox is full, or it isn't running. From that moment every message
// is committed to the state database before the sending node's Send returns,
// the ones already on their way to the target are diverted behind it in order,
// and a pump delivers from disk, oldest first, one at a time, retrying a
// message the target fails with a pause that grows to a few seconds. When the
// disk queue is empty the wire goes back to memory. An InfluxDB that is down for
// an hour holds that hour of readings on disk, in order, and writes them when it
// comes back.
//
// The cost is a window. While the wire is healthy, what is in memory, meaning
// the target's inbox and the call it is making, is lost to a kill -9 or a power
// cut. Once the wire is spilling there is no window: a message is on disk before
// the sender goes on.
//
// The target has taken a message when its handler returns without an error. For
// a node that finishes its work inside the handler, like influxdb out, that
// means the write succeeded. A node that hands the work to another goroutine
// and returns has promised less, and the wire can only be as durable as what it
// is wired to.

// durableKeyProp is the flow-file key that marks a node's wires durable.
const durableKeyProp = "ew_durable"

// queuedAtProp is the message property a durable wire stamps with the time the
// message entered it, as RFC 3339 with nanoseconds.
const queuedAtProp = "_queuedAt"

// retry pauses between attempts on a message the target failed.
const (
	durableRetryMin = 250 * time.Millisecond
	durableRetryMax = 5 * time.Second
)

// durableKey names one wire: the node it leaves, the port, and the node it goes
// to. It is the queue's name in the database and the label on its metrics.
func durableKey(from string, port int, to string) string {
	return fmt.Sprintf("%s:%d>%s", from, port, to)
}

type durableWire struct {
	rt   *Runtime
	key  string
	from string
	to   string
	q    *store.WireQueue

	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}

	// warned remembers the properties already reported as left out.
	warned sync.Map

	// mu guards the mode of the wire. spill is true from the first message the
	// target doesn't take until the disk queue is empty again. memInflight
	// counts messages sent straight to the target and not yet resolved, and
	// pendingAppends the messages decided for disk and not yet written. The
	// pump delivers from disk only when memInflight is zero, so a message that
	// failed in memory is on disk, in its place, before anything behind it is
	// delivered.
	mu             sync.Mutex
	spill          bool
	spilling       atomic.Bool
	memInflight    int
	pendingAppends int

	// lastStamp keeps the stamps on one wire strictly increasing, so two
	// readings that reach the wire in the same tick of a coarse clock still
	// get different times.
	stampMu   sync.Mutex
	lastStamp time.Time
}

func (w *durableWire) stampTime() time.Time {
	w.stampMu.Lock()
	defer w.stampMu.Unlock()
	now := w.rt.opts.Now().UTC()
	if !now.After(w.lastStamp) {
		now = w.lastStamp.Add(time.Nanosecond)
	}
	w.lastStamp = now
	return now
}

// durableTargets reads the ids a node's durable wires go to.
func durableTargets(n *engine.Node) map[string]bool {
	raw, ok := n.Raw[durableKeyProp]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			out[s] = true
		}
	}
	return out
}

// syncDurable makes the running pumps match the wires the flow marks durable,
// and returns them by key.
//
// The flow definition is the truth, not what happens to be running. A target
// that failed to start leaves its wire in place and its queue filling, which is
// the point. A wire that is gone from the definition, or no longer marked, has
// its queue purged, counted and announced: those messages have nowhere to go,
// and holding them would fill the quota with a queue nobody can see.
func (rt *Runtime) syncDurable(g *graph) map[string]*durableWire {
	if rt.spool == nil {
		return nil
	}
	type spec struct{ from, to string }
	wanted := map[string]spec{}
	for _, id := range g.flows.Order {
		n := g.flows.Nodes[id]
		if n == nil || n.IsConfig {
			continue
		}
		targets := durableTargets(n)
		if len(targets) == 0 {
			continue
		}
		for port, ids := range n.Wires {
			for _, tid := range ids {
				if targets[tid] {
					wanted[durableKey(id, port, tid)] = spec{from: id, to: tid}
				}
			}
		}
	}

	rt.durableMu.Lock()
	defer rt.durableMu.Unlock()
	if rt.durable == nil {
		rt.durable = map[string]*durableWire{}
	}

	for key, sp := range wanted {
		if _, ok := rt.durable[key]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(rt.ctx)
		w := &durableWire{
			rt: rt, key: key, from: sp.from, to: sp.to,
			q:      rt.spool.Wire(key),
			wake:   make(chan struct{}, 1),
			cancel: cancel,
			done:   make(chan struct{}),
		}
		if w.q.Depth() > 0 {
			// Left from before the restart: deliver it first.
			w.setSpill(true)
		}
		rt.durable[key] = w
		go w.pump(ctx)
	}

	for key, w := range rt.durable {
		if _, ok := wanted[key]; ok {
			continue
		}
		w.cancel()
		<-w.done
		delete(rt.durable, key)
		rt.purgeDurable(w.q, w.to)
	}
	// Queues left on disk by a flow that is no longer what is deployed.
	for _, key := range rt.spool.Wires() {
		if _, ok := wanted[key]; ok {
			continue
		}
		q := rt.spool.Wire(key)
		if q.Depth() > 0 {
			rt.purgeDurable(q, "")
		}
	}
	return rt.durable
}

// purgeDurable drops a wire's queue and says so.
func (rt *Runtime) purgeDurable(q *store.WireQueue, to string) {
	n, err := q.Purge()
	if err != nil || n == 0 {
		return
	}
	rt.emit(Event{Topic: TopicDropped, Data: map[string]any{
		"nodeId": to, "type": "durable-wire", "policy": "wire-removed",
		"wire": q.ID(), "dropped": n,
	}})
}

// stopDurable ends every pump, letting a message that is mid-delivery finish, and
// writes the "done" marks so the next runtime does not send them again.
func (rt *Runtime) stopDurable(ctx context.Context) {
	rt.durableMu.Lock()
	ws := make([]*durableWire, 0, len(rt.durable))
	for _, w := range rt.durable {
		ws = append(ws, w)
	}
	rt.durable = nil
	rt.durableMu.Unlock()

	for _, w := range ws {
		w.cancel()
	}
	for _, w := range ws {
		select {
		case <-w.done:
		case <-ctx.Done():
		}
	}
	if rt.spool != nil {
		_ = rt.spool.FlushAcks()
	}
}

func (w *durableWire) notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// target is the runner the wire leads to in the graph running now, or nil while
// that node is not running.
func (w *durableWire) target() *runner {
	r, ok := w.rt.graph().runners[w.to]
	if !ok || r.exited.Load() {
		return nil
	}
	return r
}

func (w *durableWire) setSpill(on bool) {
	w.spill = on
	w.spilling.Store(on)
}

// offer sends a message down the wire. While the target keeps up it goes
// straight into its inbox and nothing is written. Otherwise it returns once the
// message is committed to disk, or has been refused and reported.
func (w *durableWire) offer(msg *engine.Msg, from *runner) {
	// When the message entered the wire, which is when the reading happened as
	// far as anything downstream can tell. A node that would stamp the time on
	// arrival can use this instead, so an hour held through an outage doesn't
	// all arrive stamped with the minute it came back.
	if _, ok := msg.Data[queuedAtProp]; !ok {
		msg.Data[queuedAtProp] = w.stampTime().Format(time.RFC3339Nano)
	}

	w.mu.Lock()
	seq := w.q.AllocSeq()
	if !w.spill {
		if t := w.target(); t != nil {
			// The node may change the message it is handed, so the copy kept
			// for a failure is made first.
			orig := msg.Clone()
			d := delivery{
				msg:    msg,
				divert: w.spilling.Load,
				ack:    func(err error) { w.memDone(seq, orig, err) },
			}
			w.memInflight++
			if t.tryPush(d) {
				w.mu.Unlock()
				return
			}
			w.memInflight--
		}
		w.setSpill(true)
	}
	w.pendingAppends++
	w.mu.Unlock()

	w.appendSpill(seq, msg, from)

	w.mu.Lock()
	w.pendingAppends--
	w.mu.Unlock()
	w.notify()
}

// memDone is the outcome of a message sent straight to the target. A success
// needs nothing. A failure puts the wire into spilling and writes the message
// to disk under its own sequence number.
func (w *durableWire) memDone(seq uint64, orig *engine.Msg, err error) {
	if err == nil {
		w.mu.Lock()
		w.memInflight--
		w.mu.Unlock()
		w.notify()
		return
	}
	w.mu.Lock()
	w.setSpill(true)
	w.mu.Unlock()

	// This runs on the target's own goroutine, so it can't wait for room in a
	// full quota the way a sender can, and the message is older than anything
	// the quota has since refused. It goes in over the quota, by at most what
	// fits in the target's inbox.
	body, skipped, eerr := store.EncodeMsg(orig)
	if eerr == nil {
		w.reportSkipped(skipped)
		_, eerr = w.q.AppendSeqOver(seq, body)
	}
	if eerr != nil {
		w.dropped(orig, "spill-failed")
	}
	w.mu.Lock()
	w.memInflight--
	w.mu.Unlock()
	w.notify()
}

func (w *durableWire) reportSkipped(skipped []string) {
	for _, k := range skipped {
		if _, seen := w.warned.LoadOrStore(k, true); !seen {
			w.logf("durable wire %s does not carry msg.%s to disk: it can't be stored, so it is left out of every message the wire spills", w.key, k)
		}
	}
}

// appendSpill writes a message to the disk queue, applying the target's
// overflow policy if the quota is full.
func (w *durableWire) appendSpill(seq uint64, msg *engine.Msg, from *runner) {
	rt := w.rt
	body, skipped, err := store.EncodeMsg(msg)
	if err != nil {
		rt.raiseError(from, fmt.Errorf("durable wire %s: %w", w.key, err), msg)
		return
	}
	w.reportSkipped(skipped)

	var deadline <-chan time.Time
	for {
		space := rt.spool.Space()
		_, err = w.q.AppendSeq(seq, body)
		if err == nil {
			return
		}
		if !errors.Is(err, store.ErrQuota) {
			rt.raiseError(from, fmt.Errorf("durable wire %s: %w", w.key, err), msg)
			return
		}

		// The disk quota is full. The wire's target decides what gives, by the
		// same policy it uses when its own inbox fills.
		policy := rt.opts.Overflow
		if t := w.target(); t != nil {
			policy = t.overflow
		}
		switch policy {
		case OverflowDropNewest:
			w.dropped(msg, "drop-newest")
			return
		case OverflowError:
			w.dropped(msg, "error")
			rt.raiseError(from, fmt.Errorf("%w: durable wire %s, %d of %d bytes used",
				store.ErrQuota, w.key, rt.spool.Bytes(), rt.spool.MaxBytes()), msg)
			return
		case OverflowDropOldest:
			evicted, eerr := w.q.EvictOldest()
			if eerr != nil {
				rt.raiseError(from, fmt.Errorf("durable wire %s: %w", w.key, eerr), msg)
				return
			}
			if !evicted {
				// The quota is held by other wires, or by the one message
				// this wire is delivering. Nothing here to make room with.
				w.dropped(msg, "drop-newest")
				return
			}
			rt.emit(Event{Topic: TopicDropped, Data: map[string]any{
				"nodeId": w.to, "type": "durable-wire", "policy": "drop-oldest", "wire": w.key,
			}})
		default:
			if deadline == nil {
				t := time.NewTimer(rt.opts.BlockTimeout)
				defer t.Stop()
				deadline = t.C
			}
			select {
			case <-space:
			case <-deadline:
				w.dropped(msg, "block-timeout")
				rt.raiseError(from, fmt.Errorf("%w: durable wire %s after %s",
					store.ErrQuota, w.key, rt.opts.BlockTimeout), msg)
				return
			case <-rt.ctx.Done():
				w.dropped(msg, "shutdown")
				return
			}
		}
	}
}

func (w *durableWire) dropped(msg *engine.Msg, policy string) {
	w.q.CountDropped()
	w.rt.emit(Event{Topic: TopicDropped, Data: map[string]any{
		"nodeId": w.to, "type": "durable-wire", "policy": policy,
		"wire": w.key, "msgId": msg.ID(),
	}})
}

// pump delivers the wire's disk queue to its target, in order, whenever the
// wire is spilling.
func (w *durableWire) pump(ctx context.Context) {
	defer close(w.done)
	var cursor uint64
	idle := func() bool {
		select {
		case <-w.wake:
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
			// A wake is never missed, but a poll costs nothing and means a bug
			// there cannot strand a queue.
		}
		return true
	}
	for {
		if ctx.Err() != nil {
			return
		}
		w.mu.Lock()
		spill, mem := w.spill, w.memInflight
		w.mu.Unlock()
		if !spill {
			if !idle() {
				return
			}
			continue
		}
		if mem > 0 {
			// Messages sent straight to the target are still being resolved,
			// and a failed one is about to land on disk ahead of everything
			// the pump would read now.
			if !sleepCtx(ctx, 5*time.Millisecond) {
				return
			}
			continue
		}
		e, ok, err := w.q.Next(cursor)
		if err != nil {
			w.logf("reading the queue for durable wire %s: %v", w.key, err)
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if !ok {
			w.mu.Lock()
			if w.memInflight == 0 && w.pendingAppends == 0 && w.q.Depth() == 0 {
				w.setSpill(false)
			}
			w.mu.Unlock()
			if !idle() {
				return
			}
			continue
		}
		if !w.deliver(ctx, e) {
			return
		}
		cursor = e.Seq
	}
}

// deliver hands one queued message to the target and does not return until it
// has been taken, or the runtime is stopping. It reports whether to carry on.
func (w *durableWire) deliver(ctx context.Context, e store.Entry) bool {
	w.q.SetInflight(e.Seq)
	defer w.q.SetInflight(0)

	pause := durableRetryMin
	for {
		msg, err := store.DecodeMsg(e.Body)
		if err != nil {
			// Not something a retry will fix. Counted and announced, and the
			// queue moves on; stalling behind one unreadable row would hold
			// every message after it.
			w.logf("durable wire %s dropped an unreadable message: %v", w.key, err)
			w.q.CountDropped()
			w.q.Ack(e)
			return true
		}

		t := w.target()
		if t == nil {
			if !sleepCtx(ctx, 100*time.Millisecond) {
				return false
			}
			continue
		}

		result := make(chan error, 1)
		var once sync.Once
		// Whatever failure got a message on disk has already been reported to
		// the flow, so a failure here goes back to the wire and not to Catch.
		d := delivery{
			msg:   msg,
			retry: true,
			ack:   func(err error) { once.Do(func() { result <- err }) },
		}
		if !t.pushDurable(ctx, d) {
			if ctx.Err() != nil {
				return false
			}
			// The node was replaced between the lookup and the push.
			continue
		}

		var herr error
		select {
		case herr = <-result:
		case <-ctx.Done():
			// Stopping. The message is in the node's inbox and the runtime is
			// about to drain it, so wait for how that goes rather than leave
			// it queued on disk as well and send it twice.
			select {
			case herr = <-result:
			case <-time.After(w.rt.opts.CloseTimeout):
				return false
			}
		}
		if herr == nil {
			w.q.Ack(e)
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		w.q.CountRetry()
		if !sleepCtx(ctx, pause) {
			return false
		}
		if pause *= 2; pause > durableRetryMax {
			pause = durableRetryMax
		}
	}
}

func (w *durableWire) logf(format string, args ...any) {
	w.rt.emit(Event{Topic: TopicLog, Data: map[string]any{
		"nodeId": w.from, "type": "durable-wire",
		"level": "warn", "message": fmt.Sprintf(format, args...),
	}})
}

// sleepCtx waits, and reports false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
