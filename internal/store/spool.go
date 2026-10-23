package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/metrics"
)

// Spool is the disk behind durable wires: one ordered queue per wire, all in the
// same SQLite file as persistent context.
//
// It is only written when a wire's target stops taking messages. While the
// target keeps up, messages go straight to it from memory and the file is not
// touched. When it doesn't, a message is committed here before the sending
// node's call returns, and a pump hands the oldest one to the target when it
// recovers and, once the target has taken it, marks it done. Marking is lazy and
// rides along with the next write, because a delete per message would double the
// writes for nothing.
//
// That makes delivery from the queue at-least-once. The only message that can
// arrive twice is one the target had already taken when the process died and
// whose "done" had not reached the file yet.
type Spool struct {
	db       *SQLite
	maxBytes int64
	ackFlush time.Duration
	syncAcks bool

	total   atomic.Int64
	nextSeq atomic.Uint64
	commits atomic.Int64

	mu      sync.Mutex
	wires   map[string]*WireQueue
	pending map[*WireQueue]uint64 // newest finished seq per wire, not yet deleted
	space   chan struct{}

	sendMu sync.RWMutex
	closed bool
	reqs   chan spoolReq
	kick   chan struct{}
	done   chan struct{}
}

// ErrQuota means the disk quota for durable wires is used up.
var ErrQuota = errors.New("the durable wire disk quota is full")

// ErrSpoolClosed means the spool has been shut down.
var ErrSpoolClosed = errors.New("the durable wire store is closed")

// RowOverhead is what the quota charges per queued message on top of its body:
// the key, the wire name and SQLite's own bookkeeping, rounded up.
const RowOverhead = 64

// DefaultSpoolMaxBytes is 256 MiB.
const DefaultSpoolMaxBytes = 256 << 20

// DefaultAckFlush is how long a finished message waits for a write to ride
// along on before one is made for it.
const DefaultAckFlush = 100 * time.Millisecond

// SpoolOptions configures a Spool.
type SpoolOptions struct {
	// MaxBytes is the quota for every durable wire together: the bodies of the
	// messages waiting plus RowOverhead each. The file on disk runs somewhat
	// larger, by SQLite's page overhead and the write-ahead log.
	MaxBytes int64

	// AckFlush is how long a finished message waits for a write to ride along
	// on before one is made for it. Messages finished inside the window share a
	// write, which is cheaper on the disk, and are the ones a kill can send a
	// second time.
	AckFlush time.Duration

	// SyncAcks writes each finished message before the next is sent, so a kill
	// can repeat at most the one that was in flight. Costs a write per message.
	SyncAcks bool
}

type reqKind int

const (
	reqAppend reqKind = iota
	reqEvict
	reqPurge
	reqFlush
	reqStop
)

type spoolReq struct {
	kind  reqKind
	wire  *WireQueue
	seq   uint64
	body  []byte
	reply chan spoolReply
}

type spoolReply struct {
	err  error
	size int64
	n    int64
}

// OpenSpool loads the queues already on disk and starts the writer.
func OpenSpool(db *SQLite, opts SpoolOptions) (*Spool, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultSpoolMaxBytes
	}
	if opts.AckFlush <= 0 {
		opts.AckFlush = DefaultAckFlush
	}
	s := &Spool{
		db:       db,
		maxBytes: opts.MaxBytes,
		ackFlush: opts.AckFlush,
		syncAcks: opts.SyncAcks,
		wires:    map[string]*WireQueue{},
		pending:  map[*WireQueue]uint64{},
		space:    make(chan struct{}),
		reqs:     make(chan spoolReq, 1024),
		kick:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}

	var maxSeq uint64
	if err := db.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM wire_queue`).Scan(&maxSeq); err != nil {
		return nil, fmt.Errorf("reading the durable queue: %w", err)
	}
	s.nextSeq.Store(maxSeq)

	rows, err := db.db.Query(`SELECT wire, COUNT(*), COALESCE(SUM(LENGTH(body)), 0) FROM wire_queue GROUP BY wire`)
	if err != nil {
		return nil, fmt.Errorf("reading the durable queue: %w", err)
	}
	for rows.Next() {
		var id string
		var n, bytes int64
		if err := rows.Scan(&id, &n, &bytes); err != nil {
			rows.Close()
			return nil, err
		}
		q := s.wireLocked(id)
		q.depth.Store(n)
		q.bytes.Store(bytes + n*RowOverhead)
		s.total.Add(bytes + n*RowOverhead)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	go s.run()
	return s, nil
}

// Wire returns the queue for a wire, creating it empty if it has none.
func (s *Spool) Wire(id string) *WireQueue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wireLocked(id)
}

func (s *Spool) wireLocked(id string) *WireQueue {
	if q, ok := s.wires[id]; ok {
		return q
	}
	q := &WireQueue{s: s, id: id}
	s.wires[id] = q
	return q
}

// Wires lists every wire with a queue, sorted.
func (s *Spool) Wires() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.wires))
	for id := range s.wires {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Commits is how many transactions the spool has written. A durable wire whose
// target keeps up never adds to it.
func (s *Spool) Commits() int64 { return s.commits.Load() }

// AllocSeq reserves the next sequence number. A wire takes its number when the
// message arrives and may write it later, and the numbers, not the order the
// writes land in, are the order messages are delivered in.
func (s *Spool) AllocSeq() uint64 { return s.nextSeq.Add(1) }

// Bytes is what the quota is charged right now.
func (s *Spool) Bytes() int64 { return s.total.Load() }

// MaxBytes is the quota.
func (s *Spool) MaxBytes() int64 { return s.maxBytes }

// Space returns a channel that is closed the next time room is freed, for a
// sender waiting out a full quota.
func (s *Spool) Space() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.space
}

func (s *Spool) signalSpace() {
	s.mu.Lock()
	close(s.space)
	s.space = make(chan struct{})
	s.mu.Unlock()
}

// FlushAcks writes every "done" mark that is waiting. A runtime calls it when it
// stops, so the next one does not send again what this one already delivered.
func (s *Spool) FlushAcks() error {
	_, err := s.submit(spoolReq{kind: reqFlush})
	return err
}

// Close writes what is waiting and stops the writer. The database stays open.
func (s *Spool) Close() error {
	s.sendMu.Lock()
	if s.closed {
		s.sendMu.Unlock()
		<-s.done
		return nil
	}
	s.closed = true
	s.sendMu.Unlock()
	reply := make(chan spoolReply, 1)
	s.reqs <- spoolReq{kind: reqStop, reply: reply}
	r := <-reply
	<-s.done
	return r.err
}

func (s *Spool) submit(r spoolReq) (spoolReply, error) {
	r.reply = make(chan spoolReply, 1)
	s.sendMu.RLock()
	if s.closed {
		s.sendMu.RUnlock()
		return spoolReply{}, ErrSpoolClosed
	}
	s.reqs <- r
	s.sendMu.RUnlock()
	rep := <-r.reply
	return rep, rep.err
}

// run is the only goroutine that writes. Everything that arrives while one
// transaction is committing is gathered into the next, so concurrent senders
// share a commit and the cost of a write is split between them.
func (s *Spool) run() {
	defer close(s.done)
	var ackTimer <-chan time.Time
	for {
		select {
		case r := <-s.reqs:
			batch := []spoolReq{r}
		gather:
			for len(batch) < 512 {
				select {
				case more := <-s.reqs:
					batch = append(batch, more)
				default:
					break gather
				}
			}
			ackTimer = nil
			if stop := s.commit(batch); stop {
				return
			}
		case <-s.kick:
			if ackTimer == nil {
				ackTimer = time.After(s.ackFlush)
			}
		case <-ackTimer:
			ackTimer = nil
			s.commit(nil)
		}
	}
}

// commit applies a batch and every pending "done" mark in one transaction. It
// reports whether the batch held the stop request.
func (s *Spool) commit(batch []spoolReq) (stop bool) {
	s.mu.Lock()
	acks := s.pending
	s.pending = map[*WireQueue]uint64{}
	s.mu.Unlock()

	writes := len(acks) > 0
	for _, r := range batch {
		if r.kind == reqAppend || r.kind == reqEvict || r.kind == reqPurge {
			writes = true
		}
	}
	replies := make([]spoolReply, len(batch))
	err := func() error {
		if !writes {
			// A flush with nothing to flush, or a stop: no transaction, because
			// even an empty one is a call into the file.
			return nil
		}
		tx, err := s.db.db.Begin()
		if err != nil {
			return err
		}
		for i, r := range batch {
			switch r.kind {
			case reqAppend:
				if _, err := tx.Exec(`INSERT INTO wire_queue (seq, wire, body) VALUES (?, ?, ?)`, r.seq, r.wire.id, r.body); err != nil {
					tx.Rollback()
					return err
				}
			case reqEvict:
				var seq uint64
				var size int64
				// Not the message in flight, and not one that has been
				// acknowledged and is only waiting for this transaction to
				// delete it: Ack has already given its space back, so taking
				// it again would give the same space back twice and leave the
				// wire's depth and the quota short of what is on disk.
				floor := r.seq
				if a := acks[r.wire]; a > floor {
					floor = a
				}
				row := tx.QueryRow(`SELECT seq, LENGTH(body) FROM wire_queue WHERE wire = ? AND seq > ? ORDER BY seq LIMIT 1`,
					r.wire.id, floor)
				if err := row.Scan(&seq, &size); err != nil {
					// Nothing to evict is an answer, not a failure.
					continue
				}
				if _, err := tx.Exec(`DELETE FROM wire_queue WHERE wire = ? AND seq = ?`, r.wire.id, seq); err != nil {
					tx.Rollback()
					return err
				}
				replies[i].size = size + RowOverhead
				replies[i].n = 1
			case reqPurge:
				var n, size int64
				_ = tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(LENGTH(body)), 0) FROM wire_queue WHERE wire = ?`, r.wire.id).Scan(&n, &size)
				if _, err := tx.Exec(`DELETE FROM wire_queue WHERE wire = ?`, r.wire.id); err != nil {
					tx.Rollback()
					return err
				}
				replies[i].n = n
				replies[i].size = size + n*RowOverhead
			}
		}
		for q, seq := range acks {
			if _, err := tx.Exec(`DELETE FROM wire_queue WHERE wire = ? AND seq <= ?`, q.id, seq); err != nil {
				tx.Rollback()
				return err
			}
		}
		return tx.Commit()
	}()
	if err == nil && writes {
		s.commits.Add(1)
		// The rows are in the file now, so this is the moment they count as
		// queued. It happens before any sender is told, so a sender that has
		// been answered sees its own message in Depth().
		for _, r := range batch {
			if r.kind == reqAppend {
				r.wire.depth.Add(1)
				r.wire.bytes.Add(int64(len(r.body)) + RowOverhead)
			}
		}
	}

	if err != nil {
		// Nothing was written, so the marks go back to be tried again.
		s.mu.Lock()
		for q, seq := range acks {
			if seq > s.pending[q] {
				s.pending[q] = seq
			}
		}
		s.mu.Unlock()
		select {
		case s.kick <- struct{}{}:
		default:
		}
		for i := range replies {
			replies[i] = spoolReply{err: fmt.Errorf("writing the durable queue: %w", err)}
		}
	}
	for i, r := range batch {
		if r.kind == reqStop {
			stop = true
		}
		r.reply <- replies[i]
	}
	return stop
}

// Families are the queue metrics.
func (s *Spool) Families() []metrics.Family {
	each := func(f func(q *WireQueue) float64) func() []metrics.Sample {
		return func() []metrics.Sample {
			s.mu.Lock()
			qs := make([]*WireQueue, 0, len(s.wires))
			for _, q := range s.wires {
				qs = append(qs, q)
			}
			s.mu.Unlock()
			sort.Slice(qs, func(i, j int) bool { return qs[i].id < qs[j].id })
			out := make([]metrics.Sample, 0, len(qs))
			for _, q := range qs {
				out = append(out, metrics.Sample{Labels: [][2]string{{"wire", q.id}}, Value: f(q)})
			}
			return out
		}
	}
	return []metrics.Family{
		{Name: "hotloop_flow_durable_wire_depth", Type: "gauge",
			Help:    "Messages waiting on a durable wire, on disk.",
			Collect: each(func(q *WireQueue) float64 { return float64(q.depth.Load()) })},
		{Name: "hotloop_flow_durable_wire_bytes", Type: "gauge",
			Help:    "Quota charged to a durable wire's waiting messages.",
			Collect: each(func(q *WireQueue) float64 { return float64(q.bytes.Load()) })},
		{Name: "hotloop_flow_durable_wire_delivered_total", Type: "counter",
			Help:    "Messages a durable wire has handed to its target and had taken.",
			Collect: each(func(q *WireQueue) float64 { return float64(q.acked.Load()) })},
		{Name: "hotloop_flow_durable_wire_retries_total", Type: "counter",
			Help:    "Times a durable wire sent a message again because its target failed it.",
			Collect: each(func(q *WireQueue) float64 { return float64(q.retries.Load()) })},
		{Name: "hotloop_flow_durable_wire_dropped_total", Type: "counter",
			Help:    "Messages a durable wire gave up on: the quota policy evicted or refused them.",
			Collect: each(func(q *WireQueue) float64 { return float64(q.dropped.Load()) })},
		{Name: "hotloop_flow_durable_wire_given_up_total", Type: "counter",
			Help:    "Messages a durable wire stopped retrying because the wire has an attempt limit and the target refused the message that many times. Each one was raised to the flow as an error with the message attached.",
			Collect: each(func(q *WireQueue) float64 { return float64(q.gaveUp.Load()) })},
		{Name: "hotloop_flow_durable_commits_total", Type: "counter",
			Help:    "Transactions written to the durable queue. Flat while every durable wire's target keeps up, because nothing is written until one doesn't.",
			Collect: func() []metrics.Sample { return []metrics.Sample{{Value: float64(s.commits.Load())}} }},
		{Name: "hotloop_flow_durable_bytes", Type: "gauge",
			Help:    "Quota used by every durable wire together.",
			Collect: func() []metrics.Sample { return []metrics.Sample{{Value: float64(s.total.Load())}} }},
		{Name: "hotloop_flow_durable_max_bytes", Type: "gauge",
			Help:    "The durable wire disk quota.",
			Collect: func() []metrics.Sample { return []metrics.Sample{{Value: float64(s.maxBytes)}} }},
	}
}

// WireQueue is one durable wire's queue.
type WireQueue struct {
	s  *Spool
	id string

	depth atomic.Int64
	bytes atomic.Int64

	appended atomic.Int64
	acked    atomic.Int64
	retries  atomic.Int64
	dropped  atomic.Int64
	gaveUp   atomic.Int64

	// inflight is the message being delivered right now. Evicting the oldest
	// message must never take the one a target is in the middle of.
	inflight atomic.Uint64

	// claimMu makes picking the next message and marking it in flight one step
	// as far as EvictOldest can tell. Without it an eviction between the two
	// takes the message the pump has just read, the pump delivers it anyway,
	// and its Ack gives the space back a second time.
	claimMu sync.Mutex
}

// Entry is a queued message.
type Entry struct {
	Seq  uint64
	Body []byte
	// Size is what the quota is charged for it.
	Size int64
}

// ID is the wire's name.
func (q *WireQueue) ID() string { return q.id }

// Depth is how many messages are waiting.
func (q *WireQueue) Depth() int64 { return q.depth.Load() }

// Bytes is the quota charged to this wire.
func (q *WireQueue) Bytes() int64 { return q.bytes.Load() }

// Retries counts messages sent again after a failure.
func (q *WireQueue) Retries() int64 { return q.retries.Load() }

// Dropped counts messages given up on.
func (q *WireQueue) Dropped() int64 { return q.dropped.Load() }

// GaveUp counts messages given up on after the wire's attempt limit.
func (q *WireQueue) GaveUp() int64 { return q.gaveUp.Load() }

// CountGaveUp is kept by the pump when it stops retrying a message.
func (q *WireQueue) CountGaveUp() { q.gaveUp.Add(1) }

// CountRetry and CountDropped let the pump and the sender keep the counters.
func (q *WireQueue) CountRetry()   { q.retries.Add(1) }
func (q *WireQueue) CountDropped() { q.dropped.Add(1) }

// SetInflight records the message being delivered, 0 for none.
func (q *WireQueue) SetInflight(seq uint64) { q.inflight.Store(seq) }

// Append adds a message and returns once it is committed to the file. It
// returns ErrQuota, having written nothing, if the message would not fit.
func (q *WireQueue) Append(body []byte) (uint64, error) {
	return q.AppendSeq(q.s.AllocSeq(), body)
}

// AllocSeq reserves a sequence number for AppendSeq.
func (q *WireQueue) AllocSeq() uint64 { return q.s.AllocSeq() }

// AppendSeq is Append with a sequence number the caller reserved.
func (q *WireQueue) AppendSeq(seq uint64, body []byte) (uint64, error) {
	return q.append(seq, body, false)
}

// AppendSeqOver is AppendSeq without the quota check, for a message that was
// already accepted and is in memory. Refusing it would drop an older message
// while keeping newer ones. The overshoot is bounded by what fits in the
// target's inbox.
func (q *WireQueue) AppendSeqOver(seq uint64, body []byte) (uint64, error) {
	return q.append(seq, body, true)
}

func (q *WireQueue) append(seq uint64, body []byte, over bool) (uint64, error) {
	size := int64(len(body)) + RowOverhead
	s := q.s
	if s.total.Add(size) > s.maxBytes && !over {
		s.total.Add(-size)
		return 0, ErrQuota
	}
	// depth and bytes are counted by the writer once the row is committed, not
	// here. Counting a message before it is on disk made Depth() say a wire had
	// 100 messages queued while the hundredth was still waiting its turn, and a
	// file copied on that word was missing it. Only the quota is held up front,
	// so concurrent senders can't all squeeze in past the limit together.
	if _, err := s.submit(spoolReq{kind: reqAppend, wire: q, seq: seq, body: body}); err != nil {
		s.total.Add(-size)
		return 0, err
	}
	q.appended.Add(1)
	return seq, nil
}

// Next returns the oldest message after seq, or false if there is none.
func (q *WireQueue) Next(after uint64) (Entry, bool, error) {
	var e Entry
	err := q.s.db.db.QueryRow(`SELECT seq, body FROM wire_queue WHERE wire = ? AND seq > ? ORDER BY seq LIMIT 1`,
		q.id, after).Scan(&e.Seq, &e.Body)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	e.Size = int64(len(e.Body)) + RowOverhead
	return e, true, nil
}

// Claim is Next for the one reader that delivers the queue: it returns the oldest
// message after seq and marks it in flight, so EvictOldest can't take it between
// the two. The reader clears the mark with SetInflight(0) when it is done.
func (q *WireQueue) Claim(after uint64) (Entry, bool, error) {
	q.claimMu.Lock()
	defer q.claimMu.Unlock()
	e, ok, err := q.Next(after)
	if ok {
		q.inflight.Store(e.Seq)
	}
	return e, ok, err
}

// Ack marks a message, and so everything before it, delivered. The space is
// free at once; the rows are deleted with the next write, or before Ack returns
// when the spool was opened with SyncAcks.
func (q *WireQueue) Ack(e Entry) {
	s := q.s
	q.depth.Add(-1)
	q.bytes.Add(-e.Size)
	s.total.Add(-e.Size)
	q.acked.Add(1)
	s.mu.Lock()
	if e.Seq > s.pending[q] {
		s.pending[q] = e.Seq
	}
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	s.signalSpace()
	if s.syncAcks {
		_ = s.FlushAcks()
	}
}

// EvictOldest removes the oldest message that is not in flight, for a wire whose
// policy is to make room by dropping. It reports whether there was one.
func (q *WireQueue) EvictOldest() (bool, error) {
	q.claimMu.Lock()
	rep, err := q.s.submit(spoolReq{kind: reqEvict, wire: q, seq: q.inflight.Load()})
	q.claimMu.Unlock()
	if err != nil {
		return false, err
	}
	if rep.n == 0 {
		return false, nil
	}
	q.depth.Add(-1)
	q.bytes.Add(-rep.size)
	q.s.total.Add(-rep.size)
	q.dropped.Add(1)
	q.s.signalSpace()
	return true, nil
}

// Purge deletes everything queued on the wire, for a wire taken out of the flow.
// It returns how many messages went.
func (q *WireQueue) Purge() (int64, error) {
	rep, err := q.s.submit(spoolReq{kind: reqPurge, wire: q})
	if err != nil {
		return 0, err
	}
	q.depth.Add(-rep.n)
	q.bytes.Add(-rep.size)
	q.s.total.Add(-rep.size)
	q.dropped.Add(rep.n)
	q.s.signalSpace()
	return rep.n, nil
}
