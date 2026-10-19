package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Tracing answers one question: where did this message go, how long did each
// node sit on it, and what did it look like when it got there.
//
// A trace is everything that came out of one message entering the flow. It
// starts where the message comes in, at a source or at a node somebody pressed,
// and every node that handles the message or anything made from it adds a hop.
// Splits make several hops from one, a join makes one from several, a subflow is
// just more nodes, and an error that reaches a Catch is a hop with a different
// way in.
//
// The tag that carries a message from hop to hop lives on the Msg and not in
// its Data, for two reasons. A flow can't see it or break it with msg = {...},
// and it never shows up in a Debug node, a test or a payload on the wire.
//
// All of it is bounded, because it runs on the line it watches. The tracer keeps
// a fixed number of traces and a fixed number of bytes, a trace keeps a fixed
// number of hops, and a hop keeps a copy of its message only when the copy is
// small. Past any of those it forgets the oldest or stops writing down the
// newest. It never grows.

// TraceTag is what a message carries between nodes.
type TraceTag struct {
	Trace *Trace
	// Hop is the hop that consumed this message, or -1 while it is still on
	// its way to one.
	Hop int
	// Senders are the hops that sent it, valid while Hop is -1.
	Senders []int
	// Port is the output port of the sender it left by, -1 when it didn't
	// travel a wire.
	Port int
	// Via is how it got there when that wasn't a wire: "error" for a message
	// a Catch node was handed.
	Via string
}

// untraced marks a message whose trace was forgotten, or never could be kept,
// while it was still on its way. It stays untraced for the rest of its journey:
// starting a new trace halfway would only fill the buffer with the tails of
// journeys whose heads are gone.
var untraced = &TraceTag{Hop: -2, Port: -1}

// Hop is one node handling one message.
type Hop struct {
	ID int `json:"id"`
	// Parents are the hops that sent the message this hop handled. More than
	// one means a join: several messages became this one.
	Parents []int `json:"parents,omitempty"`
	// Port is the output port of the parent the message left by.
	Port int    `json:"port"`
	Via  string `json:"via,omitempty"`
	Node string `json:"node"`
	Type string `json:"type"`
	// Entry is set on the hop a trace starts with: "emit" when a source sent
	// the message into the flow, "receive" when a node was handed it.
	Entry string    `json:"entry,omitempty"`
	At    time.Time `json:"at"`
	// Nanos is how long the node took with the message.
	Nanos int64  `json:"nanos"`
	Done  bool   `json:"done"`
	MsgID string `json:"msgId"`
	// Msg is the message as the node was handed it. Nil when it was too big
	// to keep, which Cut says.
	Msg   map[string]any `json:"-"`
	Cut   bool           `json:"cut,omitempty"`
	Error string         `json:"error,omitempty"`
	// Sent is how many messages the node sent on from this one.
	Sent int `json:"sent"`
}

// TraceLimits bounds a tracer.
type TraceLimits struct {
	// Traces is how many traces are kept, newest.
	Traces int
	// MaxHops is how many hops one trace keeps. A flow with a loop in it
	// would otherwise write one down forever.
	MaxHops int
	// MaxMessageBytes is the biggest message a hop keeps a copy of.
	MaxMessageBytes int
	// MaxBytes is the most all the traces together may hold, roughly counted.
	MaxBytes int64
	// PerSecond is the most new traces started in a second. Past it, messages
	// entering the flow aren't traced until the next second, so a sensor
	// talking a hundred thousand times a second costs what a thousand does.
	// Zero means no limit.
	PerSecond int
}

// DefaultTraceLimits is what an instance runs with unless it is told
// otherwise.
func DefaultTraceLimits() TraceLimits {
	return TraceLimits{Traces: 256, MaxHops: 512, MaxMessageBytes: 16 << 10, MaxBytes: 16 << 20, PerSecond: 1000}
}

func (l TraceLimits) withDefaults() TraceLimits {
	d := DefaultTraceLimits()
	if l.Traces <= 0 {
		l.Traces = d.Traces
	}
	if l.MaxHops <= 0 {
		l.MaxHops = d.MaxHops
	}
	if l.MaxMessageBytes <= 0 {
		l.MaxMessageBytes = d.MaxMessageBytes
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = d.MaxBytes
	}
	return l
}

// Span is a hop that has finished, handed to the exporter. Trace names the
// hop's parents' spans, since a span id is the trace's to make.
type Span struct {
	TraceID string
	Hop     Hop
	Trace   *Trace
}

// Tracer keeps the traces.
type Tracer struct {
	lim TraceLimits

	mu    sync.Mutex
	ring  []*Trace
	head  int
	count int

	bytes atomic.Int64

	window   atomic.Int64
	used     atomic.Int64
	skipped  atomic.Int64
	started  atomic.Int64
	evicted  atomic.Int64
	hopsCut  atomic.Int64
	onFinish atomic.Pointer[func(Span)]
}

// NewTracer returns a tracer with the limits given; zero fields get the
// defaults.
func NewTracer(lim TraceLimits) *Tracer {
	lim = lim.withDefaults()
	return &Tracer{lim: lim, ring: make([]*Trace, lim.Traces)}
}

// Limits returns what the tracer is bounded by.
func (t *Tracer) Limits() TraceLimits { return t.lim }

// OnFinish installs the function called with every hop as it finishes, for an
// exporter. It must not block. Nil removes it.
func (t *Tracer) OnFinish(fn func(Span)) {
	if fn == nil {
		t.onFinish.Store(nil)
		return
	}
	t.onFinish.Store(&fn)
}

// TraceStats is what the tracer has done, for the metrics and for the soak.
type TraceStats struct {
	Kept        int   `json:"kept"`
	Bytes       int64 `json:"bytes"`
	Started     int64 `json:"started"`
	Evicted     int64 `json:"evicted"`
	Skipped     int64 `json:"skipped"`
	HopsDropped int64 `json:"hopsDropped"`
}

// Stats reports the tracer's counters.
func (t *Tracer) Stats() TraceStats {
	t.mu.Lock()
	n := t.count
	t.mu.Unlock()
	return TraceStats{Kept: n, Bytes: t.bytes.Load(), Started: t.started.Load(),
		Evicted: t.evicted.Load(), Skipped: t.skipped.Load(), HopsDropped: t.hopsCut.Load()}
}

// Trace is one message's journey.
type Trace struct {
	ID      string
	Started time.Time
	t       *Tracer
	salt    [8]byte

	mu        sync.Mutex
	hops      []Hop
	bytes     int64
	truncated bool
	gone      atomic.Bool
}

// SpanID is the id a hop's span has in an exporter: stable, and unique within
// the trace because the salt is random per trace.
func (tr *Trace) SpanID(hop int) string {
	var b [8]byte
	copy(b[:4], tr.salt[:4])
	n := uint32(hop + 1)
	b[4], b[5], b[6], b[7] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	return hex.EncodeToString(b[:])
}

// admit says whether a new trace may start now, under the per-second limit.
func (t *Tracer) admit(now time.Time) bool {
	if t.lim.PerSecond <= 0 {
		return true
	}
	sec := now.Unix()
	if w := t.window.Load(); w != sec && t.window.CompareAndSwap(w, sec) {
		t.used.Store(0)
	}
	if t.used.Add(1) > int64(t.lim.PerSecond) {
		t.skipped.Add(1)
		return false
	}
	return true
}

func (t *Tracer) begin(node, typ, entry string, m *Msg, now time.Time) (*Trace, int) {
	if !t.admit(now) {
		return nil, -1
	}
	var id [16]byte
	tr := &Trace{t: t, Started: now}
	if _, err := rand.Read(id[:]); err != nil {
		n := now.UnixNano()
		for i := range id {
			id[i] = byte(n >> (8 * (i % 8)))
		}
	}
	copy(tr.salt[:], id[8:])
	tr.ID = hex.EncodeToString(id[:])

	t.mu.Lock()
	if t.count == len(t.ring) {
		t.evictOldestLocked()
	}
	t.ring[(t.head+t.count)%len(t.ring)] = tr
	t.count++
	t.mu.Unlock()
	t.started.Add(1)

	hop := tr.add(Hop{Node: node, Type: typ, Entry: entry, Port: -1, At: now}, m)
	return tr, hop
}

// evictOldestLocked forgets the oldest trace. t.mu is held.
func (t *Tracer) evictOldestLocked() {
	if t.count == 0 {
		return
	}
	old := t.ring[t.head]
	t.ring[t.head] = nil
	t.head = (t.head + 1) % len(t.ring)
	t.count--
	old.mu.Lock()
	old.gone.Store(true)
	t.bytes.Add(-old.bytes)
	old.bytes = 0
	old.hops = nil
	old.mu.Unlock()
	t.evicted.Add(1)
}

// shrink forgets the oldest traces until the tracer is under its byte budget.
// The newest trace is never forgotten by it: one trace over the budget on its
// own stops writing instead.
func (t *Tracer) shrink() {
	t.mu.Lock()
	for t.bytes.Load() > t.lim.MaxBytes && t.count > 1 {
		t.evictOldestLocked()
	}
	t.mu.Unlock()
}

const hopOverhead = 160

// add writes a hop down and returns its id, or -1 when the trace is full or
// gone. The message is copied only if it fits.
func (tr *Trace) add(h Hop, m *Msg) int {
	t := tr.t
	if tr.gone.Load() {
		return -1
	}
	if m != nil {
		h.MsgID = m.ID()
	}
	size := int64(hopOverhead + len(h.Node) + len(h.Type) + 8*len(h.Parents))
	if m != nil {
		if n, ok := messageSize(m.Data, t.lim.MaxMessageBytes); ok {
			h.Msg = m.Clone().Data
			size += int64(n)
		} else {
			h.Cut = true
		}
	}
	tr.mu.Lock()
	if tr.gone.Load() || len(tr.hops) >= t.lim.MaxHops || tr.bytes+size > t.lim.MaxBytes {
		tr.truncated = true
		tr.mu.Unlock()
		t.hopsCut.Add(1)
		return -1
	}
	h.ID = len(tr.hops)
	tr.hops = append(tr.hops, h)
	tr.bytes += size
	tr.mu.Unlock()
	if t.bytes.Add(size) > t.lim.MaxBytes {
		t.shrink()
	}
	return h.ID
}

func (tr *Trace) nodeOf(hop int) string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if hop < 0 || hop >= len(tr.hops) {
		return ""
	}
	return tr.hops[hop].Node
}

// Receive is called as a node is about to handle a message. It writes the hop
// down and re-tags the message as consumed by it. The trace and hop come back
// so the node's emitter can say which hop its sends belong to; a nil trace
// means this message isn't being traced.
func (t *Tracer) Receive(m *Msg, node, typ string, now time.Time) (*Trace, int) {
	if t == nil || m == nil {
		return nil, -1
	}
	m.EnsureID()
	tag := m.Trace
	if tag == nil {
		tr, id := t.begin(node, typ, "receive", m, now)
		if id >= 0 {
			m.Trace = &TraceTag{Trace: tr, Hop: id, Port: -1}
			return tr, id
		}
		m.Trace = untraced
		return nil, -1
	}
	tr := tag.Trace
	if tr == nil || tr.gone.Load() {
		m.Trace = untraced
		return nil, -1
	}
	h := Hop{Node: node, Type: typ, At: now, Port: tag.Port, Via: tag.Via}
	if tag.Hop >= 0 {
		h.Parents = []int{tag.Hop}
	} else {
		h.Parents = tag.Senders
	}
	id := tr.add(h, m)
	if id < 0 {
		return nil, -1
	}
	m.Trace = &TraceTag{Trace: tr, Hop: id, Port: -1}
	return tr, id
}

// Finish records how long the node took and whether it failed.
func (tr *Trace) Finish(hop int, d time.Duration, err error) {
	if tr == nil || hop < 0 {
		return
	}
	tr.mu.Lock()
	if tr.gone.Load() || hop >= len(tr.hops) {
		tr.mu.Unlock()
		return
	}
	h := &tr.hops[hop]
	h.Nanos = int64(d)
	h.Done = true
	if err != nil {
		h.Error = err.Error()
	}
	done := *h
	tr.mu.Unlock()
	if fn := tr.t.onFinish.Load(); fn != nil {
		(*fn)(Span{TraceID: tr.ID, Hop: done, Trace: tr})
	}
}

// Outgoing is called as a node sends a message that has somewhere to go. It
// decides which hops the message came from and tags it so the node that gets
// it can write the right parents down. ec and eh are the trace and hop of the
// Receive the sending node is in the middle of, if it is.
func (t *Tracer) Outgoing(m *Msg, node, typ string, port int, via string, ec *Trace, eh int, now time.Time) {
	if t == nil || m == nil {
		return
	}
	m.EnsureID()
	tag := m.Trace
	var tr *Trace
	var senders []int

	switch {
	case tag != nil:
		tr = tag.Trace
		if tr == nil || tr.gone.Load() {
			m.Trace = untraced
			return
		}
		if tag.Hop >= 0 && tr.nodeOf(tag.Hop) == node {
			senders = []int{tag.Hop}
			if ec == tr && eh >= 0 && eh != tag.Hop && tr.nodeOf(eh) == node {
				// The message that went out is one this node took in earlier
				// and held, and it went out while the node was handling
				// another: a join. Everything the node took in for it and
				// hasn't sent anything for is part of it.
				senders = tr.joined(node, tag.Hop, eh)
			}
		} else {
			// Not made here: it arrived by some way other than a wire, a
			// Link Out handing it to a Link In. The node is a stop on the
			// path of its own.
			parents := tag.Senders
			if tag.Hop >= 0 {
				parents = []int{tag.Hop}
			}
			id := tr.add(Hop{Node: node, Type: typ, At: now, Port: tag.Port, Via: tag.Via, Parents: parents}, m)
			tr.Finish(id, 0, nil)
			if id >= 0 {
				senders = []int{id}
			}
		}
	case ec != nil && eh >= 0 && !ec.gone.Load() && ec.nodeOf(eh) == node:
		tr = ec
		senders = []int{eh}
	default:
		var id int
		tr, id = t.begin(node, typ, "emit", m, now)
		if tr == nil {
			m.Trace = untraced
			return
		}
		tr.Finish(id, 0, nil)
		if id >= 0 {
			senders = []int{id}
		}
	}
	tr.sent(senders)
	m.Trace = &TraceTag{Trace: tr, Hop: -1, Senders: senders, Port: port, Via: via}
}

// joined returns the hops a held message went out on: the one it was taken in
// at, the one that let it go, and any other the node took in and sent nothing
// for.
func (tr *Trace) joined(node string, first, last int) []int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := []int{first}
	for i := range tr.hops {
		h := &tr.hops[i]
		if h.Node == node && h.Entry == "" && h.Sent == 0 && h.ID != first && h.ID != last {
			out = append(out, h.ID)
		}
	}
	return append(out, last)
}

func (tr *Trace) sent(hops []int) {
	if len(hops) == 0 {
		return
	}
	tr.mu.Lock()
	for _, id := range hops {
		if id >= 0 && id < len(tr.hops) {
			tr.hops[id].Sent++
		}
	}
	tr.mu.Unlock()
}

// TraceSnapshot is a trace as it stood when it was read.
type TraceSnapshot struct {
	ID        string
	Started   time.Time
	Hops      []Hop
	Truncated bool
}

// Snapshot copies the trace. The messages in it are shared, and nothing
// changes them once they're written.
func (tr *Trace) Snapshot() TraceSnapshot {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return TraceSnapshot{ID: tr.ID, Started: tr.Started, Hops: append([]Hop(nil), tr.hops...), Truncated: tr.truncated}
}

// Traces returns the kept traces, newest first.
func (t *Tracer) Traces() []*Trace {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Trace, 0, t.count)
	for i := t.count - 1; i >= 0; i-- {
		out = append(out, t.ring[(t.head+i)%len(t.ring)])
	}
	return out
}

// Get returns a trace by id.
func (t *Tracer) Get(id string) (*Trace, bool) {
	for _, tr := range t.Traces() {
		if tr.ID == id {
			return tr, true
		}
	}
	return nil, false
}

// Find returns the newest trace with a hop that handled the message with this
// id, at this node when node isn't empty, and that hop's id. A message keeps its
// id until a node makes a new one, so the id a Debug node shows finds the
// hop it was shown at.
func (t *Tracer) Find(msgID, node string) (*Trace, int, bool) {
	if t == nil || msgID == "" {
		return nil, -1, false
	}
	for _, tr := range t.Traces() {
		tr.mu.Lock()
		found := -1
		for i := len(tr.hops) - 1; i >= 0; i-- {
			h := &tr.hops[i]
			if h.MsgID == msgID && (node == "" || h.Node == node) {
				found = i
				break
			}
		}
		tr.mu.Unlock()
		if found >= 0 {
			return tr, found, true
		}
	}
	return nil, -1, false
}

// Ends are the hops where the message stopped: nothing was sent on from them.
// A Debug node, an MQTT Out, a Switch that matched nothing, a Join waiting for
// the rest.
func (s TraceSnapshot) Ends() []Hop {
	var out []Hop
	for _, h := range s.Hops {
		if h.Sent == 0 && h.Entry == "" {
			out = append(out, h)
		}
	}
	return out
}

// PathTo returns the ids of the hops the one given came by: itself, its
// parents, theirs, back to where the trace started. Oldest first.
func (s TraceSnapshot) PathTo(hop int) []int {
	seen := map[int]bool{}
	var walk func(int)
	walk = func(id int) {
		if id < 0 || id >= len(s.Hops) || seen[id] {
			return
		}
		seen[id] = true
		for _, p := range s.Hops[id].Parents {
			walk(p)
		}
	}
	walk(hop)
	out := make([]int, 0, len(seen))
	for i := range s.Hops {
		if seen[i] {
			out = append(out, i)
		}
	}
	return out
}

// messageSize counts a message roughly, giving up the moment it is over
// budget rather than walking a huge one to the end.
func messageSize(v any, budget int) (int, bool) {
	left := budget
	var walk func(v any, depth int) bool
	walk = func(v any, depth int) bool {
		if depth > 32 {
			return false
		}
		switch t := v.(type) {
		case string:
			left -= len(t) + 16
		case []byte:
			left -= len(t) + 24
		case ImmutableBytes:
			left -= len(t) + 24
		case map[string]any:
			left -= 48
			for k, e := range t {
				left -= len(k) + 16
				if left < 0 || !walk(e, depth+1) {
					return false
				}
			}
		case []any:
			left -= 24
			for _, e := range t {
				if left < 0 || !walk(e, depth+1) {
					return false
				}
			}
		default:
			left -= 16
		}
		return left >= 0
	}
	if !walk(v, 0) {
		return 0, false
	}
	return budget - left, true
}

// JSONSafe turns a kept message into something that encodes as JSON the way a
// person would expect to read it: bytes that are text become text, times become
// RFC 3339, and what can't be shown is named by its type instead of failing
// the whole response.
func JSONSafe(v any) any {
	return jsonSafe(v, 0)
}

func jsonSafe(v any, depth int) any {
	if depth > 64 {
		return nil
	}
	switch t := v.(type) {
	case nil, bool, string, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = jsonSafe(e, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = jsonSafe(e, depth+1)
		}
		return out
	case []byte:
		return bytesText(t)
	case ImmutableBytes:
		return bytesText(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case error:
		return t.Error()
	case fmt.Stringer:
		return t.String()
	}
	return fmt.Sprintf("<%T>", v)
}

func bytesText(b []byte) any {
	if utf8.Valid(b) {
		return string(b)
	}
	return fmt.Sprintf("<%d bytes>", len(b))
}
