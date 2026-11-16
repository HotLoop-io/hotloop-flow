// Package kafkaw is a Kafka producer: the few requests it takes to put one record on
// a topic, and nothing else.
//
// It speaks the Kafka protocol's older, fixed-size message versions that every
// broker since 0.11 answers and that Kafka 4.0 still does (Metadata v4, Produce v3,
// SaslHandshake v1, SaslAuthenticate v0), sends one uncompressed record batch per
// call and waits for every in-sync replica (acks=-1). It finds the topic's partition
// leaders with Metadata, picks the partition the way Kafka's own Java client does
// (the murmur2 hash of the key, or round robin with no key), reconnects, refreshes
// its metadata and retries when a leader moves, and logs in with any
// github.com/twmb/franz-go/pkg/sasl mechanism (PLAIN, SCRAM-SHA-256, SCRAM-SHA-512).
//
// It exists because a full Kafka client library is more than five megabytes of this
// binary and a producer is the only thing Flow needs. It is not a consumer, does not
// batch across calls, compresses nothing and does no transactions. The SASL packages
// of franz-go (BSD-3-Clause, credited in NOTICE) are used for the login exchange and
// are the only part of it that is linked.
package kafkaw

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/sasl"
)

// The request kinds and versions this package uses.
const (
	apiProduce      = 0
	apiMetadata     = 3
	apiSaslHS       = 17
	apiSaslAuth     = 36
	verProduce      = 3
	verMetadata     = 4
	verSaslHS       = 1
	verSaslAuth     = 0
	clientID        = "hotloop-flow"
	maxFrame        = 100 << 20
	dialTimeout     = 10 * time.Second
	requestTimeout  = 15 * time.Second
	produceTimeout  = 10000 // ms the broker may take to collect the acks
	attempts        = 4
	errUnknownTopic = 3
	errLeaderNA     = 5
	errNotLeader    = 6
	errReqTimeout   = 7
	errMsgTooLarge  = 10
	errNotEnoughRep = 19
	errNotEnoughIn  = 20
	errSASLAuth     = 58
)

// Config is how to reach a cluster.
type Config struct {
	Brokers []string // host:port seeds
	SASL    sasl.Mechanism
	TLS     *tls.Config
}

// Client produces records. It is safe for concurrent use.
type Client struct {
	cfg Config

	mu      sync.Mutex
	conns   map[string]*conn // by host:port
	brokers map[int32]string
	topics  map[string][]int32 // partition -> leader node id (-1 when none)
	rr      atomic.Uint32
}

// New makes a client. Nothing is dialed until the first Produce.
func New(cfg Config) *Client {
	return &Client{cfg: cfg, conns: map[string]*conn{}, brokers: map[int32]string{}, topics: map[string][]int32{}}
}

// Close drops every connection.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for a, cn := range c.conns {
		cn.close()
		delete(c.conns, a)
	}
}

// Error is a broker's refusal, by its Kafka error code.
type Error struct{ Code int16 }

func (e *Error) Error() string { return "the broker answered " + codeName(e.Code) }

func codeName(c int16) string {
	switch c {
	case errUnknownTopic:
		return "UNKNOWN_TOPIC_OR_PARTITION (the topic does not exist and the broker does not create it)"
	case errLeaderNA:
		return "LEADER_NOT_AVAILABLE"
	case errNotLeader:
		return "NOT_LEADER_OR_FOLLOWER"
	case errReqTimeout:
		return "REQUEST_TIMED_OUT"
	case errMsgTooLarge:
		return "MESSAGE_TOO_LARGE"
	case errNotEnoughRep, errNotEnoughIn:
		return "NOT_ENOUGH_REPLICAS"
	case 29, 30:
		return "TOPIC_AUTHORIZATION_FAILED (the login may not write to this topic)"
	case errSASLAuth:
		return "SASL_AUTHENTICATION_FAILED"
	}
	return "error code " + strconv.Itoa(int(c))
}

func retriable(code int16) bool {
	switch code {
	case errUnknownTopic, errLeaderNA, errNotLeader, errReqTimeout, errNotEnoughRep, errNotEnoughIn:
		return true
	}
	return false
}

// Produce puts one record on a topic and returns once every in-sync replica has it.
// A nil key is no key.
func (c *Client) Produce(ctx context.Context, topic string, key, value []byte) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-time.After(time.Duration(i) * 250 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		last = c.once(ctx, topic, key, value)
		if last == nil {
			return nil
		}
		var ke *Error
		if errors.As(last, &ke) && !retriable(ke.Code) {
			return last
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A leader may have moved, or a connection died: ask again next time.
		c.mu.Lock()
		delete(c.topics, topic)
		c.mu.Unlock()
	}
	return last
}

func (c *Client) once(ctx context.Context, topic string, key, value []byte) error {
	leaders, err := c.leaders(ctx, topic)
	if err != nil {
		return err
	}
	part := int32(0)
	if key != nil {
		part = int32(murmur2(key)&0x7fffffff) % int32(len(leaders))
	} else {
		part = int32(c.rr.Add(1)-1) % int32(len(leaders))
	}
	c.mu.Lock()
	addr := c.brokers[leaders[part]]
	c.mu.Unlock()
	if addr == "" {
		return &Error{Code: errLeaderNA}
	}
	cn, err := c.conn(ctx, addr)
	if err != nil {
		return err
	}
	var b wbuf
	b.nstr("")            // no transactional id
	b.i16(-1)             // acks: every in-sync replica
	b.i32(produceTimeout) // timeout
	b.i32(1)              // one topic
	b.str(topic)          //
	b.i32(1)              // one partition
	b.i32(part)           //
	batch := recordBatch(key, value, time.Now())
	b.i32(int32(len(batch))) // records
	b.buf = append(b.buf, batch...)
	resp, err := cn.call(ctx, apiProduce, verProduce, b.buf)
	if err != nil {
		c.drop(addr, cn)
		return err
	}
	r := rbuf{b: resp}
	n := r.i32() // topics
	for ; n > 0 && r.err == nil; n-- {
		r.str()
		pn := r.i32()
		for ; pn > 0 && r.err == nil; pn-- {
			r.i32() // partition
			code := r.i16()
			r.i64() // base offset
			r.i64() // log append time
			if code != 0 {
				return &Error{Code: code}
			}
		}
	}
	if r.err != nil {
		c.drop(addr, cn)
		return fmt.Errorf("kafka: the produce answer was cut short")
	}
	return nil
}

func (c *Client) drop(addr string, cn *conn) {
	cn.close()
	c.mu.Lock()
	if c.conns[addr] == cn {
		delete(c.conns, addr)
	}
	c.mu.Unlock()
}

// leaders is the leader node id of every partition of the topic, asked of the cluster
// when it is not known.
func (c *Client) leaders(ctx context.Context, topic string) ([]int32, error) {
	c.mu.Lock()
	if l, ok := c.topics[topic]; ok {
		c.mu.Unlock()
		return l, nil
	}
	seeds := append([]string(nil), c.cfg.Brokers...)
	for _, a := range c.brokers {
		seeds = append(seeds, a)
	}
	c.mu.Unlock()

	var lastErr error
	for _, addr := range seeds {
		cn, err := c.conn(ctx, addr)
		if err != nil {
			lastErr = err
			continue
		}
		var b wbuf
		b.i32(1)
		b.str(topic)
		b.u8(1) // allow auto topic creation, if the broker's own policy does
		resp, err := cn.call(ctx, apiMetadata, verMetadata, b.buf)
		if err != nil {
			c.drop(addr, cn)
			lastErr = err
			continue
		}
		r := rbuf{b: resp}
		r.i32() // throttle
		nodes := map[int32]string{}
		for n := r.i32(); n > 0 && r.err == nil; n-- {
			id := r.i32()
			host := r.str()
			port := r.i32()
			r.nstr() // rack
			nodes[id] = net.JoinHostPort(host, strconv.Itoa(int(port)))
		}
		r.nstr() // cluster id
		r.i32()  // controller
		var out []int32
		var topicErr int16
		for n := r.i32(); n > 0 && r.err == nil; n-- {
			code := r.i16()
			name := r.str()
			r.u8() // internal
			if name == topic {
				topicErr = code
			}
			for pn := r.i32(); pn > 0 && r.err == nil; pn-- {
				r.i16() // partition error
				idx := r.i32()
				leader := r.i32()
				for k := r.i32(); k > 0 && r.err == nil; k-- {
					r.i32()
				}
				for k := r.i32(); k > 0 && r.err == nil; k-- {
					r.i32()
				}
				if name == topic {
					for int(idx) >= len(out) {
						out = append(out, -1)
					}
					out[idx] = leader
				}
			}
		}
		if r.err != nil {
			c.drop(addr, cn)
			lastErr = fmt.Errorf("kafka: the metadata answer was cut short")
			continue
		}
		if topicErr != 0 {
			return nil, &Error{Code: topicErr}
		}
		if len(out) == 0 {
			return nil, &Error{Code: errUnknownTopic}
		}
		for _, l := range out {
			if l < 0 {
				return nil, &Error{Code: errLeaderNA}
			}
		}
		c.mu.Lock()
		for id, a := range nodes {
			c.brokers[id] = a
		}
		c.topics[topic] = out
		c.mu.Unlock()
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no broker is configured")
	}
	return nil, lastErr
}

func (c *Client) conn(ctx context.Context, addr string) (*conn, error) {
	c.mu.Lock()
	if cn, ok := c.conns[addr]; ok {
		c.mu.Unlock()
		return cn, nil
	}
	c.mu.Unlock()
	cn, err := dial(ctx, addr, c.cfg)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if old, ok := c.conns[addr]; ok {
		c.mu.Unlock()
		cn.close()
		return old, nil
	}
	c.conns[addr] = cn
	c.mu.Unlock()
	return cn, nil
}

// ---- one connection

type conn struct {
	mu   sync.Mutex
	nc   net.Conn
	corr int32
}

func dial(ctx context.Context, addr string, cfg Config) (*conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	var nc net.Conn
	var err error
	if cfg.TLS != nil {
		t := cfg.TLS.Clone()
		if t.ServerName == "" {
			if h, _, e := net.SplitHostPort(addr); e == nil {
				t.ServerName = h
			}
		}
		nc, err = (&tls.Dialer{NetDialer: &d, Config: t}).DialContext(ctx, "tcp", addr)
	} else {
		nc, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("kafka: cannot reach %s: %w", addr, unwrapDial(err))
	}
	cn := &conn{nc: nc}
	if cfg.SASL != nil {
		if err := cn.login(ctx, addr, cfg.SASL); err != nil {
			cn.close()
			return nil, err
		}
	}
	return cn, nil
}

func unwrapDial(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err
	}
	return err
}

func (cn *conn) close() { _ = cn.nc.Close() }

func (cn *conn) login(ctx context.Context, addr string, m sasl.Mechanism) error {
	host, _, _ := net.SplitHostPort(addr)
	var b wbuf
	b.str(m.Name())
	resp, err := cn.call(ctx, apiSaslHS, verSaslHS, b.buf)
	if err != nil {
		return err
	}
	r := rbuf{b: resp}
	if code := r.i16(); code != 0 {
		return &Error{Code: code}
	}
	sess, first, err := m.Authenticate(ctx, host)
	if err != nil {
		return fmt.Errorf("kafka: login: %w", err)
	}
	msg := first
	for {
		var ab wbuf
		ab.bytes(msg)
		resp, err := cn.call(ctx, apiSaslAuth, verSaslAuth, ab.buf)
		if err != nil {
			return err
		}
		r := rbuf{b: resp}
		code := r.i16()
		text := r.nstr()
		reply := r.nbytes()
		if code != 0 {
			if text != "" {
				return fmt.Errorf("kafka: login refused: %s", text)
			}
			return &Error{Code: code}
		}
		done, next, err := sess.Challenge(reply)
		if err != nil {
			return fmt.Errorf("kafka: login: %w", err)
		}
		if done {
			return nil
		}
		msg = next
	}
}

// call sends one request and returns the body of its answer.
func (cn *conn) call(ctx context.Context, api, ver int16, body []byte) ([]byte, error) {
	cn.mu.Lock()
	defer cn.mu.Unlock()
	cn.corr++
	var h wbuf
	h.i16(api)
	h.i16(ver)
	h.i32(cn.corr)
	h.str(clientID)
	h.buf = append(h.buf, body...)
	frame := make([]byte, 4, 4+len(h.buf))
	binary.BigEndian.PutUint32(frame, uint32(len(h.buf)))
	frame = append(frame, h.buf...)
	dl := time.Now().Add(requestTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl.Add(5*time.Second)) {
		dl = d
	}
	_ = cn.nc.SetDeadline(dl)
	if _, err := cn.nc.Write(frame); err != nil {
		return nil, fmt.Errorf("kafka: %w", unwrapDial(err))
	}
	var sz [4]byte
	if _, err := io.ReadFull(cn.nc, sz[:]); err != nil {
		return nil, fmt.Errorf("kafka: the broker closed the connection: %w", unwrapDial(err))
	}
	n := binary.BigEndian.Uint32(sz[:])
	if n < 4 || n > maxFrame {
		return nil, fmt.Errorf("kafka: an answer of %d bytes is not a Kafka answer", n)
	}
	resp := make([]byte, n)
	if _, err := io.ReadFull(cn.nc, resp); err != nil {
		return nil, fmt.Errorf("kafka: the answer was cut short: %w", unwrapDial(err))
	}
	if got := int32(binary.BigEndian.Uint32(resp)); got != cn.corr {
		return nil, fmt.Errorf("kafka: an answer to another request arrived")
	}
	return resp[4:], nil
}

// ---- the record batch

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// recordBatch is one version 2 record batch holding one uncompressed record.
func recordBatch(key, value []byte, now time.Time) []byte {
	var rec wbuf
	rec.u8(0)     // attributes
	rec.varint(0) // timestamp delta
	rec.varint(0) // offset delta
	if key == nil {
		rec.varint(-1)
	} else {
		rec.varint(int64(len(key)))
		rec.buf = append(rec.buf, key...)
	}
	rec.varint(int64(len(value)))
	rec.buf = append(rec.buf, value...)
	rec.varint(0) // headers

	var body wbuf // everything the checksum covers
	body.i16(0)   // attributes: no compression, create time
	body.i32(0)   // last offset delta
	ms := now.UnixMilli()
	body.i64(ms) // first timestamp
	body.i64(ms) // max timestamp
	body.i64(-1) // producer id
	body.i16(-1) // producer epoch
	body.i32(-1) // base sequence
	body.i32(1)  // records
	var r1 wbuf
	r1.varint(int64(len(rec.buf)))
	body.buf = append(body.buf, r1.buf...)
	body.buf = append(body.buf, rec.buf...)

	var out wbuf
	out.i64(0)                                // base offset
	out.i32(int32(4 + 1 + 4 + len(body.buf))) // batch length: from the leader epoch on
	out.i32(-1)                               // partition leader epoch
	out.u8(2)                                 // magic
	out.i32(int32(crc32.Checksum(body.buf, castagnoli)))
	out.buf = append(out.buf, body.buf...)
	return out.buf
}

// murmur2 is the hash Kafka's Java client picks a partition with, so a key goes to
// the partition every other producer would send it to.
func murmur2(data []byte) uint32 {
	const m = 0x5bd1e995
	h := uint32(0x9747b28c) ^ uint32(len(data))
	n := len(data) / 4
	for i := 0; i < n; i++ {
		k := binary.LittleEndian.Uint32(data[i*4:])
		k *= m
		k ^= k >> 24
		k *= m
		h *= m
		h ^= k
	}
	tail := data[n*4:]
	switch len(tail) {
	case 3:
		h ^= uint32(tail[2]) << 16
		fallthrough
	case 2:
		h ^= uint32(tail[1]) << 8
		fallthrough
	case 1:
		h ^= uint32(tail[0])
		h *= m
	}
	h ^= h >> 13
	h *= m
	h ^= h >> 15
	return h
}

// ---- encoding

type wbuf struct{ buf []byte }

func (w *wbuf) u8(v byte)   { w.buf = append(w.buf, v) }
func (w *wbuf) i16(v int16) { w.buf = binary.BigEndian.AppendUint16(w.buf, uint16(v)) }
func (w *wbuf) i32(v int32) { w.buf = binary.BigEndian.AppendUint32(w.buf, uint32(v)) }
func (w *wbuf) i64(v int64) { w.buf = binary.BigEndian.AppendUint64(w.buf, uint64(v)) }
func (w *wbuf) str(s string) {
	w.i16(int16(len(s)))
	w.buf = append(w.buf, s...)
}

// nstr writes a null string: length -1.
func (w *wbuf) nstr(s string) {
	if s == "" {
		w.i16(-1)
		return
	}
	w.str(s)
}
func (w *wbuf) bytes(b []byte) {
	w.i32(int32(len(b)))
	w.buf = append(w.buf, b...)
}
func (w *wbuf) varint(v int64) {
	u := uint64(v<<1) ^ uint64(v>>63)
	for u >= 0x80 {
		w.buf = append(w.buf, byte(u)|0x80)
		u >>= 7
	}
	w.buf = append(w.buf, byte(u))
}

type rbuf struct {
	b   []byte
	err error
}

func (r *rbuf) take(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		r.err = io.ErrUnexpectedEOF
		return make([]byte, n&0xff)
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}
func (r *rbuf) i16() int16 { return int16(binary.BigEndian.Uint16(r.take(2))) }
func (r *rbuf) i32() int32 { return int32(binary.BigEndian.Uint32(r.take(4))) }
func (r *rbuf) i64() int64 { return int64(binary.BigEndian.Uint64(r.take(8))) }
func (r *rbuf) u8() byte   { return r.take(1)[0] }
func (r *rbuf) str() string {
	n := int(r.i16())
	return string(r.take(n))
}

// nstr reads a nullable string: length -1 is null, which reads as empty.
func (r *rbuf) nstr() string {
	n := int(r.i16())
	if n < 0 {
		return ""
	}
	return string(r.take(n))
}
func (r *rbuf) nbytes() []byte {
	n := int(r.i32())
	if n < 0 {
		return nil
	}
	return append([]byte(nil), r.take(n)...)
}
