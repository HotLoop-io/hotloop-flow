package kafkaw

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// A fake broker that answers the four requests this package sends, from the
// protocol's own layout, and keeps what it was sent. The real one is in the lab
// (TestAgainstARealBroker below); this is what runs in CI.

type produced struct {
	topic     string
	partition int32
	key, val  []byte
	crcOK     bool
}

type fakeBroker struct {
	ln         net.Listener
	partitions int
	mu         sync.Mutex
	got        []produced
	logins     []string
	failFirst  int16 // an error code the first produce answers with
	failed     bool
	wantUser   string
	wantPass   string
}

func newFake(t *testing.T, partitions int) *fakeBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeBroker{ln: ln, partitions: partitions}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeBroker) addr() string { return f.ln.Addr().String() }

func (f *fakeBroker) serve(c net.Conn) {
	defer c.Close()
	for {
		var sz [4]byte
		if _, err := io.ReadFull(c, sz[:]); err != nil {
			return
		}
		req := make([]byte, binary.BigEndian.Uint32(sz[:]))
		if _, err := io.ReadFull(c, req); err != nil {
			return
		}
		r := rbuf{b: req}
		api, ver, corr := r.i16(), r.i16(), r.i32()
		r.str() // client id
		var w wbuf
		w.i32(corr)
		switch api {
		case apiSaslHS:
			mech := r.str()
			f.mu.Lock()
			f.logins = append(f.logins, mech)
			f.mu.Unlock()
			w.i16(0)
			w.i32(1)
			w.str(mech)
		case apiSaslAuth:
			tok := r.nbytes()
			parts := strings.Split(string(tok), "\x00")
			if f.wantUser != "" && (len(parts) != 3 || parts[1] != f.wantUser || parts[2] != f.wantPass) {
				w.i16(58)
				w.nstr("Authentication failed: Invalid username or password")
				w.bytes(nil)
			} else {
				w.i16(0)
				w.nstr("")
				w.bytes(nil)
			}
		case apiMetadata:
			_ = ver
			n := r.i32()
			topic := ""
			for ; n > 0; n-- {
				topic = r.str()
			}
			host, port, _ := net.SplitHostPort(f.addr())
			w.i32(0) // throttle
			w.i32(1)
			w.i32(7)
			w.str(host)
			p, _ := net.LookupPort("tcp", port)
			w.i32(int32(p))
			w.nstr("") // rack
			w.nstr("fake")
			w.i32(7)
			w.i32(1)
			w.i16(0)
			w.str(topic)
			w.u8(0)
			w.i32(int32(f.partitions))
			for i := 0; i < f.partitions; i++ {
				w.i16(0)
				w.i32(int32(i))
				w.i32(7)
				w.i32(1)
				w.i32(7)
				w.i32(1)
				w.i32(7)
			}
		case apiProduce:
			r.nstr() // transactional id
			if acks := r.i16(); acks != -1 {
				panic("not acks=-1")
			}
			r.i32()
			r.i32() // one topic
			topic := r.str()
			r.i32()
			part := r.i32()
			recs := r.take(int(r.i32()))
			code := int16(0)
			f.mu.Lock()
			if f.failFirst != 0 && !f.failed {
				f.failed = true
				code = f.failFirst
			} else {
				f.got = append(f.got, decodeBatch(topic, part, recs))
			}
			f.mu.Unlock()
			w.i32(1)
			w.str(topic)
			w.i32(1)
			w.i32(part)
			w.i16(code)
			w.i64(0)
			w.i64(-1)
			w.i32(0)
		}
		out := make([]byte, 4, 4+len(w.buf))
		binary.BigEndian.PutUint32(out, uint32(len(w.buf)))
		c.Write(append(out, w.buf...))
	}
}

// decodeBatch reads a record batch the way the protocol lays it out and checks its checksum.
func decodeBatch(topic string, part int32, b []byte) produced {
	r := rbuf{b: b}
	r.i64()
	length := int(r.i32())
	r.i32()
	magic := r.u8()
	crc := uint32(r.i32())
	body := r.b
	out := produced{topic: topic, partition: part, crcOK: magic == 2 && length == len(b)-12 && crc == crc32.Checksum(body, castagnoli)}
	r.i16()
	r.i32()
	r.i64()
	r.i64()
	r.i64()
	r.i16()
	r.i32()
	r.i32()
	readVar := func() int64 {
		var u uint64
		for s := uint(0); ; s += 7 {
			c := r.u8()
			u |= uint64(c&0x7f) << s
			if c < 0x80 {
				break
			}
		}
		return int64(u>>1) ^ -int64(u&1)
	}
	readVar() // record length
	r.u8()
	readVar()
	readVar()
	if kl := readVar(); kl >= 0 {
		out.key = append([]byte(nil), r.take(int(kl))...)
	}
	vl := readVar()
	out.val = append([]byte(nil), r.take(int(vl))...)
	return out
}

func TestProduceSendsOneChecksummedRecordToTheLeader(t *testing.T) {
	f := newFake(t, 1)
	c := New(Config{Brokers: []string{f.addr()}})
	defer c.Close()
	if err := c.Produce(context.Background(), "readings", []byte("press4"), []byte(`{"psi":41.5}`)); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 1 {
		t.Fatalf("%d records", len(f.got))
	}
	g := f.got[0]
	if g.topic != "readings" || string(g.key) != "press4" || string(g.val) != `{"psi":41.5}` || !g.crcOK {
		t.Fatalf("%+v", g)
	}
}

func TestAKeyGoesToThePartitionKafkasOwnHashPicks(t *testing.T) {
	f := newFake(t, 8)
	c := New(Config{Brokers: []string{f.addr()}})
	defer c.Close()
	for _, key := range []string{"a", "press4", "line-3/oven", "the quick brown fox"} {
		if err := c.Produce(context.Background(), "t", []byte(key), []byte("v")); err != nil {
			t.Fatal(err)
		}
		g := f.got[len(f.got)-1]
		// franz-go's partitioner is Kafka's, written independently of this package.
		want := kgo.StickyKeyPartitioner(nil).ForTopic("t").Partition(&kgo.Record{Key: []byte(key)}, 8)
		if int(g.partition) != want {
			t.Errorf("key %q went to partition %d, Kafka's hash says %d", key, g.partition, want)
		}
	}
}

func TestARecordWithNoKeyGoesRoundTheTopic(t *testing.T) {
	f := newFake(t, 3)
	c := New(Config{Brokers: []string{f.addr()}})
	defer c.Close()
	for i := 0; i < 6; i++ {
		if err := c.Produce(context.Background(), "t", nil, []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int32]int{}
	for _, g := range f.got {
		seen[g.partition]++
		if g.key != nil {
			t.Fatal("a key appeared")
		}
	}
	if len(seen) != 3 || seen[0] != 2 {
		t.Fatalf("%v", seen)
	}
}

func TestALeaderThatMovedIsAskedForAgainAndTheRecordSentOnce(t *testing.T) {
	f := newFake(t, 1)
	f.failFirst = errNotLeader
	c := New(Config{Brokers: []string{f.addr()}})
	defer c.Close()
	if err := c.Produce(context.Background(), "t", nil, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 1 {
		t.Fatalf("%d records stored", len(f.got))
	}
}

func TestARefusalThatCannotBeRetriedIsReturnedAtOnce(t *testing.T) {
	f := newFake(t, 1)
	f.failFirst = errMsgTooLarge
	c := New(Config{Brokers: []string{f.addr()}})
	defer c.Close()
	err := c.Produce(context.Background(), "t", nil, []byte("v"))
	var ke *Error
	if !errors.As(err, &ke) || ke.Code != errMsgTooLarge || !strings.Contains(err.Error(), "MESSAGE_TOO_LARGE") {
		t.Fatalf("%v", err)
	}
	if len(f.got) != 0 {
		t.Fatal("a refused record was stored")
	}
}

func TestLoginWithPlainAndTheBrokersWordsWhenItRefuses(t *testing.T) {
	f := newFake(t, 1)
	f.wantUser, f.wantPass = "ops", "hunter2pw"
	ok := New(Config{Brokers: []string{f.addr()}, SASL: plain.Auth{User: "ops", Pass: "hunter2pw"}.AsMechanism()})
	defer ok.Close()
	if err := ok.Produce(context.Background(), "t", nil, []byte("v")); err != nil || len(f.got) != 1 {
		t.Fatalf("%v", err)
	}
	if len(f.logins) == 0 || f.logins[0] != "PLAIN" {
		t.Fatalf("%v", f.logins)
	}
	bad := New(Config{Brokers: []string{f.addr()}, SASL: plain.Auth{User: "ops", Pass: "wrongpass"}.AsMechanism()})
	defer bad.Close()
	err := bad.Produce(context.Background(), "t", nil, []byte("v"))
	if err == nil || !strings.Contains(err.Error(), "Invalid username or password") || strings.Contains(err.Error(), "wrongpass") {
		t.Fatalf("%v", err)
	}
}

func TestNothingListeningIsAnErrorWithTheReason(t *testing.T) {
	c := New(Config{Brokers: []string{"127.0.0.1:1"}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Produce(ctx, "t", nil, []byte("v")); err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("%v", err)
	}
}

// HOTLOOP_FLOW_TEST_KAFKA=host:port names a real broker (Redpanda in the lab). A
// consumer written by someone else reads back what this package produced, with its
// key and the partition Kafka's own hash picks.
func TestAgainstARealBroker(t *testing.T) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_KAFKA")
	if addr == "" {
		t.Skip("HOTLOOP_FLOW_TEST_KAFKA is not set")
	}
	topic := "kafkaw-" + time.Now().Format("150405")
	c := New(Config{Brokers: []string{addr}})
	defer c.Close()
	keys := []string{"press4", "press5", "oven-1", "oven-2", "line-3"}
	for _, k := range keys {
		if err := c.Produce(context.Background(), topic, []byte(k), []byte("v-"+k)); err != nil {
			t.Fatal(err)
		}
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got := map[string]*kgo.Record{}
	for len(got) < len(keys) {
		f := cl.PollFetches(ctx)
		if err := f.Err(); err != nil {
			t.Fatal(err)
		}
		f.EachRecord(func(r *kgo.Record) { got[string(r.Key)] = r })
	}
	parts := map[int32]bool{}
	for _, k := range keys {
		r := got[k]
		if r == nil || string(r.Value) != "v-"+k {
			t.Fatalf("key %s: %+v", k, r)
		}
		parts[r.Partition] = true
	}
	if _, hasMany := os.LookupEnv("HOTLOOP_FLOW_TEST_KAFKA_PARTITIONS"); hasMany {
		for _, k := range keys {
			n, _ := strconvAtoi(os.Getenv("HOTLOOP_FLOW_TEST_KAFKA_PARTITIONS"))
			want := kgo.StickyKeyPartitioner(nil).ForTopic(topic).Partition(&kgo.Record{Key: []byte(k)}, n)
			if int(got[k].Partition) != want {
				t.Errorf("key %s on partition %d, Kafka's hash says %d", k, got[k].Partition, want)
			}
		}
	}
}

// HOTLOOP_FLOW_TEST_KAFKA_SCRAM=host:port,user,password names a broker that needs
// SCRAM-SHA-256.
func TestScramAgainstARealBroker(t *testing.T) {
	spec := os.Getenv("HOTLOOP_FLOW_TEST_KAFKA_SCRAM")
	if spec == "" {
		t.Skip("HOTLOOP_FLOW_TEST_KAFKA_SCRAM is not set")
	}
	p := strings.SplitN(spec, ",", 3)
	topic := "kafkaw-scram-" + time.Now().Format("150405")
	c := New(Config{Brokers: []string{p[0]}, SASL: scram.Auth{User: p[1], Pass: p[2]}.AsSha256Mechanism()})
	defer c.Close()
	if err := c.Produce(context.Background(), topic, []byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	bad := New(Config{Brokers: []string{p[0]}, SASL: scram.Auth{User: p[1], Pass: "not-the-password"}.AsSha256Mechanism()})
	defer bad.Close()
	err := bad.Produce(context.Background(), topic, nil, []byte("v"))
	if err == nil || strings.Contains(err.Error(), "not-the-password") {
		t.Fatalf("%v", err)
	}
	t.Logf("a wrong password says: %v", err)
}

func strconvAtoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// Many callers at once, so the race detector has something to look at: one client,
// one connection, every record stored exactly once.
func TestManyCallersAtOnceStoreEveryRecordOnce(t *testing.T) {
	f := newFake(t, 4)
	c := New(Config{Brokers: []string{f.addr()}})
	defer c.Close()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				key := []byte("k" + string(rune('a'+g)))
				if i%2 == 0 {
					key = nil
				}
				if err := c.Produce(context.Background(), "t", key, []byte("v")); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) != 320 {
		t.Fatalf("%d records stored, want 320", len(f.got))
	}
	for _, g := range f.got {
		if !g.crcOK {
			t.Fatal("a record arrived with a bad checksum")
		}
	}
}
