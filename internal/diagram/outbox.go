package diagram

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// outbox is a durable path to something that can be down: a broker, a database.
// A message goes straight out while the target keeps up. When it doesn't, the
// message is committed to disk before the node finishes and a pump hands the
// oldest to the target once it is back, in order. A message put in while others
// wait goes behind them, so nothing overtakes. The queue is Flow's own durable
// wire store, so it survives a restart and counts against the same disk quota.
//
// Delivery from the queue is at least once: the one message that was in flight
// when the process died can arrive twice.
type outbox struct {
	q    *store.WireQueue
	send func(ctx context.Context, body []byte) error
	// sendBatch, when the target takes several at once, delivers the leading
	// messages of bodies in order and says how many it took. It takes them all or
	// the leading ones only, never a later one without an earlier one.
	sendBatch func(ctx context.Context, bodies [][]byte) (int, error)
	// onReject is told of a message the target refused for what is in it, which
	// would be refused again, before the message is let go.
	onReject func(body []byte, err error)

	mu     sync.Mutex // one send at a time, so the order holds
	cursor uint64
	kick   chan struct{}

	direct      atomic.Int64
	spilled     atomic.Int64
	unconfirmed atomic.Int64 // sends the client took and did not confirm, each of which may still arrive
	drained     atomic.Int64
	rejected    atomic.Int64 // messages the target refused for what was in them
	limit       int          // when set, the next claim is this many, to close in on a message that is refused
	lastErr     atomic.Value // string
}

// sendWait is how long one send may take before the message goes to disk
// instead. A broker that has gone away must not hold the output for as long as
// the client would wait for it. The client may still send the message when the
// broker is back, so a message that timed out and was queued can arrive twice.
const sendWait = 2 * time.Second

// drainBatch is the most messages one send takes from the queue when the target
// takes batches, and batchWait is how long such a send may take.
const (
	drainBatch = 256
	batchWait  = 10 * time.Second
)

func newOutbox(q *store.WireQueue, send func(ctx context.Context, body []byte) error) *outbox {
	return &outbox{q: q, send: send, kick: make(chan struct{}, 1)}
}

// Put sends body now if nothing waits ahead of it and the target answers, and
// otherwise queues it. It returns whether it went straight out. An error means
// the message could be neither sent nor queued, which is the disk quota.
func (o *outbox) Put(ctx context.Context, body []byte) (sent bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.q.Depth() == 0 {
		sctx, cancel := context.WithTimeout(ctx, sendWait)
		err := o.send(sctx, body)
		cancel()
		if err == nil {
			o.direct.Add(1)
			return true, nil
		}
		o.lastErr.Store(err.Error())
		if errors.Is(err, nodes.ErrRowRejected) {
			// Refused for what is in it: queueing it would only refuse it again later.
			o.rejected.Add(1)
			return false, err
		}
		o.noteFailure(err)
	}
	if _, err := o.q.Append(body); err != nil {
		return false, err
	}
	o.spilled.Add(1)
	select {
	case o.kick <- struct{}{}:
	default:
	}
	return false, nil
}

// noteFailure counts a failed send as a possible duplicate only when the client
// or the database had taken the message, which is the only way it can arrive after being queued.
func (o *outbox) noteFailure(err error) {
	if errors.Is(err, nodes.ErrPublishUnconfirmed) || errors.Is(err, nodes.ErrInsertUnconfirmed) {
		o.unconfirmed.Add(1)
	}
}

// Depth is how many messages wait.
func (o *outbox) Depth() int64 { return o.q.Depth() }

// pump delivers the queue until ctx ends.
func (o *outbox) pump(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		for o.deliverOne(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-o.kick:
		case <-t.C:
		}
	}
}

// deliverOne sends the oldest waiting message and reports whether to go on.
func (o *outbox) deliverOne(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sendBatch != nil {
		return o.deliverBatch(ctx)
	}
	e, ok, err := o.q.Claim(o.cursor)
	if err != nil {
		o.lastErr.Store(err.Error())
		return false
	}
	if !ok {
		return false
	}
	defer o.q.SetInflight(0)
	sctx, cancel := context.WithTimeout(ctx, sendWait)
	err = o.send(sctx, e.Body)
	cancel()
	if err != nil {
		o.lastErr.Store(err.Error())
		o.noteFailure(err)
		o.q.CountRetry()
		return false
	}
	o.q.Ack(e)
	o.cursor = e.Seq
	o.drained.Add(1)
	return true
}

// deliverBatch sends up to drainBatch of the oldest waiting messages in one go.
// What the target took is acknowledged and the rest stays. A batch the target
// refuses for what is in it is sent again in halves, so the message at fault is
// found: it goes to onReject and is let go, and the ones behind it are
// delivered. Any other failure leaves everything where it was.
func (o *outbox) deliverBatch(ctx context.Context) bool {
	n := drainBatch
	if o.limit > 0 {
		n = o.limit
	}
	es, err := o.q.ClaimN(o.cursor, n)
	if err != nil {
		o.lastErr.Store(err.Error())
		return false
	}
	if len(es) == 0 {
		return false
	}
	defer o.q.SetInflight(0)
	bodies := make([][]byte, len(es))
	for i, e := range es {
		bodies[i] = e.Body
	}
	sctx, cancel := context.WithTimeout(ctx, batchWait)
	took, err := o.sendBatch(sctx, bodies)
	cancel()
	if took > len(es) {
		took = len(es)
	}
	for _, e := range es[:took] {
		o.q.Ack(e)
		o.cursor = e.Seq
		o.drained.Add(1)
	}
	if err == nil {
		o.limit = 0
		return true
	}
	o.lastErr.Store(err.Error())
	if errors.Is(err, nodes.ErrRowRejected) {
		if len(es) > 1 {
			// Half of it, then half of that, until the one at fault is alone.
			o.limit = (len(es) + 1) / 2
			return true
		}
		if o.onReject != nil {
			o.onReject(es[0].Body, err)
		}
		o.rejected.Add(1)
		o.q.Ack(es[0])
		o.cursor = es[0].Seq
		o.limit = 0
		return true
	}
	o.noteFailure(err)
	o.q.CountRetry()
	return false
}
