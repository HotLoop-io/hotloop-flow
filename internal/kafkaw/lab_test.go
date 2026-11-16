package kafkaw

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"
)

// Tests that need a real broker and say which one in an environment variable. They
// skip without it, so CI (which has no broker) is unaffected; the lab runs them.

func envOr(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set", name)
	}
	return v
}

// HOTLOOP_FLOW_TEST_KAFKA_TLS=host:port,/path/ca.pem is a broker with a certificate
// from a lab CA. HOTLOOP_FLOW_TEST_KAFKA_TLS_IP is the same broker by an address its
// certificate does not name.
func TestTLSAgainstARealBroker(t *testing.T) {
	spec := strings.SplitN(envOr(t, "HOTLOOP_FLOW_TEST_KAFKA_TLS"), ",", 2)
	pem, err := os.ReadFile(spec[1])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("the CA file holds no certificate")
	}
	topic := "kafkaw-tls-" + time.Now().Format("150405")
	ok := New(Config{Brokers: []string{spec[0]}, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
	defer ok.Close()
	if err := ok.Produce(context.Background(), topic, []byte("k"), []byte("over tls")); err != nil {
		t.Fatalf("a trusted certificate was refused: %v", err)
	}
	// A client that does not hold the lab CA refuses the broker's certificate.
	for name, cfg := range map[string]*tls.Config{
		"no CA given (system roots)": {MinVersion: tls.VersionTLS12},
		"a different CA":             {RootCAs: otherCA(t), MinVersion: tls.VersionTLS12},
	} {
		bad := New(Config{Brokers: []string{spec[0]}, TLS: cfg})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err := bad.Produce(ctx, topic, nil, []byte("must not arrive"))
		cancel()
		bad.Close()
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("%s: %v", name, err)
			continue
		}
		t.Logf("%s: %v", name, err)
	}
	if ip := os.Getenv("HOTLOOP_FLOW_TEST_KAFKA_TLS_IP"); ip != "" {
		bad := New(Config{Brokers: []string{ip}, TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err := bad.Produce(ctx, topic, nil, []byte("must not arrive"))
		cancel()
		bad.Close()
		if err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Errorf("a name the certificate does not carry: %v", err)
		} else {
			t.Logf("wrong name: %v", err)
		}
	}
	// What the trusted client wrote is there, and nothing the refused ones tried to write.
	got := consumeAll(t, spec[0], topic, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, 1)
	if len(got) != 1 || string(got[0].Value) != "over tls" {
		t.Fatalf("topic holds %d records", len(got))
	}
}

// otherCA is a pool holding one certificate authority that did not sign the broker's.
func otherCA(t *testing.T) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "some other lab CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

func consumeAll(t *testing.T, addr, topic string, tc *tls.Config, want int) []*kgo.Record {
	t.Helper()
	opts := []kgo.Opt{kgo.SeedBrokers(addr), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart())}
	if tc != nil {
		opts = append(opts, kgo.DialTLSConfig(tc))
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var out []*kgo.Record
	idle := 0
	for len(out) < want || idle < 2 {
		f := cl.PollFetches(ctx)
		if f.IsClientClosed() || ctx.Err() != nil {
			break
		}
		n := 0
		f.EachRecord(func(r *kgo.Record) { out = append(out, r); n++ })
		if n == 0 {
			idle++
		} else {
			idle = 0
		}
		if len(out) >= want && n == 0 {
			break
		}
	}
	return out
}

// HOTLOOP_FLOW_TEST_KAFKA_SASL=host:port,user,password is a broker with PLAIN,
// SCRAM-SHA-256 and SCRAM-SHA-512 all on for that user.
func TestEveryLoginAgainstARealBroker(t *testing.T) {
	p := strings.SplitN(envOr(t, "HOTLOOP_FLOW_TEST_KAFKA_SASL"), ",", 3)
	topic := "kafkaw-sasl-" + time.Now().Format("150405")
	mechs := map[string]func(user, pass string) Config{
		"PLAIN": func(u, pw string) Config {
			return Config{Brokers: []string{p[0]}, SASL: plain.Auth{User: u, Pass: pw}.AsMechanism()}
		},
		"SCRAM-SHA-256": func(u, pw string) Config {
			return Config{Brokers: []string{p[0]}, SASL: scram.Auth{User: u, Pass: pw}.AsSha256Mechanism()}
		},
		"SCRAM-SHA-512": func(u, pw string) Config {
			return Config{Brokers: []string{p[0]}, SASL: scram.Auth{User: u, Pass: pw}.AsSha512Mechanism()}
		},
	}
	for name, mk := range mechs {
		c := New(mk(p[1], p[2]))
		if err := c.Produce(context.Background(), topic, []byte(name), []byte("by "+name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		c.Close()
		bad := New(mk(p[1], "not-the-password"))
		err := bad.Produce(context.Background(), topic, nil, []byte("must not arrive"))
		bad.Close()
		if err == nil || strings.Contains(err.Error(), "not-the-password") {
			t.Errorf("%s with a wrong password: %v", name, err)
		} else {
			t.Logf("%s wrong password: %v", name, err)
		}
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(p[0]), kgo.SASL(plain.Auth{User: p[1], Pass: p[2]}.AsMechanism()), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for len(seen) < 3 && ctx.Err() == nil {
		cl.PollFetches(ctx).EachRecord(func(r *kgo.Record) { seen[string(r.Key)] = true })
	}
	if len(seen) != 3 {
		t.Fatalf("the broker holds records for %v", seen)
	}
	// Keys go where Kafka's own hash puts them, on this broker too (a topic of three partitions).
	ptopic := "kafkaw-keys-" + time.Now().Format("150405")
	c := New(Config{Brokers: []string{p[0]}, SASL: plain.Auth{User: p[1], Pass: p[2]}.AsMechanism()})
	defer c.Close()
	keys := []string{"press4", "press5", "oven-1", "oven-2", "line-3", "line-4"}
	for _, k := range keys {
		if err := c.Produce(context.Background(), ptopic, []byte(k), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	pc, err := kgo.NewClient(kgo.SeedBrokers(p[0]), kgo.SASL(plain.Auth{User: p[1], Pass: p[2]}.AsMechanism()), kgo.ConsumeTopics(ptopic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	got := map[string]int32{}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	for len(got) < len(keys) && ctx2.Err() == nil {
		pc.PollFetches(ctx2).EachRecord(func(r *kgo.Record) { got[string(r.Key)] = r.Partition })
	}
	for _, k := range keys {
		want := kgo.StickyKeyPartitioner(nil).ForTopic(ptopic).Partition(&kgo.Record{Key: []byte(k)}, 3)
		if int(got[k]) != want {
			t.Errorf("key %s on partition %d, Kafka's hash says %d", k, got[k], want)
		}
	}
}

// HOTLOOP_FLOW_TEST_KAFKA_BIG=host:port[,user,password(PLAIN)] with the topics
// created by the harness: kafkaw-small has the broker's default max.message.bytes
// and kafkaw-large has it raised to 4 MiB.
func TestRecordsOverTheBrokersLimit(t *testing.T) {
	p := strings.SplitN(envOr(t, "HOTLOOP_FLOW_TEST_KAFKA_BIG"), ",", 3)
	cfg := Config{Brokers: []string{p[0]}}
	if len(p) == 3 {
		cfg.SASL = plain.Auth{User: p[1], Pass: p[2]}.AsMechanism()
	}
	c := New(cfg)
	defer c.Close()
	ctx := context.Background()
	blob := func(n int) []byte { return []byte(strings.Repeat("x", n)) }
	if err := c.Produce(ctx, "kafkaw-small", nil, blob(900<<10)); err != nil {
		t.Fatalf("900 KiB into a 1 MiB topic: %v", err)
	}
	err := c.Produce(ctx, "kafkaw-small", nil, blob(1536<<10))
	if err == nil {
		t.Fatal("1.5 MiB was accepted by a topic that holds 1 MiB")
	}
	var ke *Error
	if !asError(err, &ke) || ke.Code != errMsgTooLarge {
		t.Fatalf("1.5 MiB into a 1 MiB topic was not refused as too large: %v", err)
	}
	t.Logf("1.5 MiB into a 1 MiB topic: %v", err)
	if err := c.Produce(ctx, "kafkaw-large", nil, blob(3<<20)); err != nil {
		t.Fatalf("3 MiB into a 4 MiB topic: %v", err)
	}
	// And the next small one still goes: a refusal does not poison the connection.
	if err := c.Produce(ctx, "kafkaw-small", nil, blob(10)); err != nil {
		t.Fatalf("after a refusal: %v", err)
	}
	got := consumeSized(t, p, "kafkaw-large")
	if got != 3<<20 {
		t.Fatalf("the 3 MiB record reads back as %d bytes", got)
	}
}

func asError(err error, target **Error) bool {
	for e := err; e != nil; {
		if ke, ok := e.(*Error); ok {
			*target = ke
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func consumeSized(t *testing.T, p []string, topic string) int {
	t.Helper()
	opts := []kgo.Opt{kgo.SeedBrokers(p[0]), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.FetchMaxBytes(16 << 20), kgo.FetchMaxPartitionBytes(8 << 20)}
	if len(p) == 3 {
		opts = append(opts, kgo.SASL(plain.Auth{User: p[1], Pass: p[2]}.AsMechanism()))
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	size := 0
	for size == 0 && ctx.Err() == nil {
		cl.PollFetches(ctx).EachRecord(func(r *kgo.Record) { size = len(r.Value) })
	}
	return size
}

// HOTLOOP_FLOW_TEST_KAFKA_FAILOVER=host:port,host:port,host:port,topic names a three
// broker cluster and a topic of three partitions with three replicas. The test
// produces 900 records, 40 a second, and the harness stops a broker while it does:
// it writes a file named by HOTLOOP_FLOW_TEST_KAFKA_FAILOVER_SIGNAL when it is a
// third of the way through, and the test goes on whether or not a broker has gone.
// Afterwards every record the producer was told was stored is counted in the topic.
func TestALeaderMovesWhileProducing(t *testing.T) {
	p := strings.Split(envOr(t, "HOTLOOP_FLOW_TEST_KAFKA_FAILOVER"), ",")
	topic := p[len(p)-1]
	seeds := p[:len(p)-1]
	signal := os.Getenv("HOTLOOP_FLOW_TEST_KAFKA_FAILOVER_SIGNAL")
	c := New(Config{Brokers: seeds})
	defer c.Close()
	const total = 900
	var acked, failed []int
	var mu sync.Mutex
	var longest time.Duration
	start := time.Now()
	for i := 0; i < total; i++ {
		if i == total/3 && signal != "" {
			_ = os.WriteFile(signal, []byte(strconv.Itoa(i)), 0o644)
		}
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := c.Produce(ctx, topic, []byte("k"+strconv.Itoa(i%9)), []byte(strconv.Itoa(i)))
		cancel()
		if d := time.Since(t0); d > longest {
			longest = d
		}
		mu.Lock()
		if err == nil {
			acked = append(acked, i)
		} else {
			failed = append(failed, i)
			t.Logf("record %d failed after %s: %v", i, time.Since(t0).Round(time.Millisecond), err)
		}
		mu.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
	t.Logf("%d records in %s: %d acknowledged, %d failed, the slowest took %s", total, time.Since(start).Round(time.Millisecond), len(acked), len(failed), longest.Round(time.Millisecond))

	time.Sleep(3 * time.Second)
	cl, err := kgo.NewClient(kgo.SeedBrokers(seeds...), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	count := map[int]int{}
	quiet := 0
	for ctx.Err() == nil && quiet < 3 {
		n := 0
		cl.PollFetches(ctx).EachRecord(func(r *kgo.Record) {
			v, _ := strconv.Atoi(string(r.Value))
			count[v]++
			n++
		})
		if n == 0 {
			quiet++
		} else {
			quiet = 0
		}
	}
	lost, dups := 0, 0
	for _, i := range acked {
		if count[i] == 0 {
			lost++
		}
	}
	for _, n := range count {
		if n > 1 {
			dups += n - 1
		}
	}
	var present []int
	for i := range count {
		present = append(present, i)
	}
	sort.Ints(present)
	t.Logf("the topic holds %d distinct records, %d duplicates, %d acknowledged records missing", len(present), dups, lost)
	if lost != 0 {
		t.Fatalf("%d records the producer was told were stored are not in the topic", lost)
	}
	fmt.Printf("FAILOVER total=%d acked=%d failed=%d distinct=%d duplicates=%d lost=%d slowest=%s\n", total, len(acked), len(failed), len(present), dups, lost, longest.Round(time.Millisecond))
}

// HOTLOOP_FLOW_TEST_KAFKA=host:port: how fast one caller produces, one record at a
// time and waiting for each, against franz-go doing the same with ProduceSync.
func TestHowFastOneCallerProduces(t *testing.T) {
	addr := envOr(t, "HOTLOOP_FLOW_TEST_KAFKA")
	if os.Getenv("HOTLOOP_FLOW_TEST_KAFKA_SPEED") == "" {
		t.Skip("HOTLOOP_FLOW_TEST_KAFKA_SPEED is not set")
	}
	const n = 3000
	val := []byte(strings.Repeat("v", 200))
	ours := New(Config{Brokers: []string{addr}})
	defer ours.Close()
	t0 := time.Now()
	for i := 0; i < n; i++ {
		if err := ours.Produce(context.Background(), "kafkaw-speed", []byte("k"), val); err != nil {
			t.Fatal(err)
		}
	}
	d1 := time.Since(t0)
	fr, err := kgo.NewClient(kgo.SeedBrokers(addr), kgo.RequiredAcks(kgo.AllISRAcks()), kgo.AllowAutoTopicCreation())
	if err != nil {
		t.Fatal(err)
	}
	defer fr.Close()
	t0 = time.Now()
	for i := 0; i < n; i++ {
		if err := fr.ProduceSync(context.Background(), &kgo.Record{Topic: "kafkaw-speed2", Key: []byte("k"), Value: val}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	d2 := time.Since(t0)
	fmt.Printf("SPEED ours %.0f records/s (%s per record), franz-go ProduceSync %.0f records/s (%s per record), 200-byte value, acks all\n", n/d1.Seconds(), (d1 / n).Round(time.Microsecond), n/d2.Seconds(), (d2 / n).Round(time.Microsecond))
}
