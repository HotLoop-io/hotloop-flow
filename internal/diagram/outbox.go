package diagram

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

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

	mu     sync.Mutex // one send at a time, so the order holds
	cursor uint64
	kick   chan struct{}

	direct  atomic.Int64
	spilled atomic.Int64
	drained atomic.Int64
	lastErr atomic.Value // string
}

// sendWait is how long one send may take before the message goes to disk
// instead. A broker that has gone away must not hold the output for as long as
// the client would wait for it. The client may still send the message when the
// broker is back, so a message that timed out and was queued can arrive twice.
const sendWait = 2 * time.Second

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
		o.q.CountRetry()
		return false
	}
	o.q.Ack(e)
	o.cursor = e.Seq
	o.drained.Add(1)
	return true
}
