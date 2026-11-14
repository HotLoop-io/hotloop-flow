package nodes

import (
	"context"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// After a reconnect paho replays every publish still in its store, and it takes
// the snapshot of the store some time after it marks the connection up. A publish
// handed to it in between is already in the store when the snapshot is taken, so
// it goes out twice: once on its own and once in the replay. The outbox promises
// that every duplicate is covered by a send that was not confirmed, and this
// send is confirmed, so that promise breaks.
//
// This test makes the gap as wide as it likes. A store whose snapshot waits holds
// paho's replay, and a proxy that keeps the broker's acknowledgements back keeps
// the publish in the store until the snapshot is taken. LinkUp is what the outbox
// asks before it sends, so it is the thing under test: it must not say the link is
// up until the replay has taken its snapshot.

// slowSnapshotStore holds All, the call the replay starts with, until released.
type slowSnapshotStore struct {
	*mqtt.MemoryStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *slowSnapshotStore) All() []string {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.MemoryStore.All()
}

func (s *slowSnapshotStore) Put(key string, p packets.ControlPacket) { s.MemoryStore.Put(key, p) }

// holdProxy forwards to the broker. Every connection after the first has the
// broker's first four bytes (the CONNACK) passed on, and the rest held back until
// released.
type holdProxy struct {
	ln      net.Listener
	target  string
	mu      sync.Mutex
	conns   []net.Conn
	n       int
	release chan struct{}
}

func newHoldProxy(t *testing.T, target string) *holdProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &holdProxy{ln: ln, target: target, release: make(chan struct{})}
	t.Cleanup(func() { ln.Close(); p.cut() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.n++
			hold := p.n > 1
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = copyAll(up, c, nil); up.Close(); c.Close() }()
			go func() {
				var gate chan struct{}
				if hold {
					gate = p.release
				}
				_, _ = copyAll(c, up, gate)
				up.Close()
				c.Close()
			}()
		}
	}()
	return p
}

// copyAll copies src to dst. With a gate, the first four bytes pass and the rest
// waits for the gate to close.
func copyAll(dst, src net.Conn, gate chan struct{}) (int, error) {
	buf := make([]byte, 4096)
	total, passed := 0, 0
	for {
		n, err := src.Read(buf)
		b := buf[:n]
		for len(b) > 0 {
			if gate != nil && passed >= 4 {
				<-gate
				gate = nil
			}
			chunk := b
			if gate != nil && passed+len(chunk) > 4 {
				chunk = b[:4-passed]
			}
			if _, werr := dst.Write(chunk); werr != nil {
				return total, werr
			}
			passed += len(chunk)
			total += len(chunk)
			b = b[len(chunk):]
		}
		if err != nil {
			return total, err
		}
	}
}

func (p *holdProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func TestAPublishDuringPahoReplayIsSentOnce(t *testing.T) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_MQTT")
	if addr == "" {
		t.Skip("set HOTLOOP_FLOW_TEST_MQTT=host:port to run this against a real broker")
	}
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	topic := "hotloop-flow/test/" + run + "/replay"

	var mu sync.Mutex
	arrived := 0
	sub := mqtt.NewClient(mqtt.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("replay-sub-" + run))
	if tok := sub.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("subscriber: %v", tok.Error())
	}
	defer sub.Disconnect(100)
	if tok := sub.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) { mu.Lock(); arrived++; mu.Unlock() }); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe: %v", tok.Error())
	}

	proxy := newHoldProxy(t, addr)
	host, port, _ := net.SplitHostPort(proxy.ln.Addr().String())
	portN, _ := strconv.Atoi(port)
	b := newMQTTv3Broker(mqttBrokerSettings{host: host, port: portN, clientID: "replay-" + run, cleanSession: true, keepalive: 5 * time.Second, version: 4})
	store := &slowSnapshotStore{MemoryStore: mqtt.NewMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	b.opts.SetStore(store)
	defer b.Close(context.Background(), false)

	deadline := time.Now().Add(15 * time.Second)
	for !b.LinkUp() {
		if time.Now().After(deadline) {
			t.Fatal("the first connection never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The first connection with a clean session resets the store and never calls
	// All, so the store has not been entered yet. Drop the link: paho reconnects,
	// the CONNACK gets through, and the replay stops at its snapshot.
	proxy.cut()
	select {
	case <-store.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("paho never started its replay after the reconnect")
	}

	// The outbox sends only when the link is up. While the replay waits for its
	// snapshot, a link reported up is the gap this test is about.
	sent := make(chan error, 1)
	publish := func() {
		sent <- PublishMQTT(context.Background(), b, topic, 1, false, []byte("X"))
	}
	published := false
	for i := 0; i < 20 && !published; i++ {
		if b.LinkUp() {
			go publish()
			published = true
		}
		time.Sleep(50 * time.Millisecond)
	}
	if published {
		// Wait until the publish is in paho's store, so the snapshot will hold it.
		time.Sleep(500 * time.Millisecond)
	}

	close(store.release)
	close(proxy.release)
	for !published {
		if time.Now().After(deadline.Add(30 * time.Second)) {
			t.Fatal("the link never came up after the replay")
		}
		if b.LinkUp() {
			go publish()
			published = true
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("the publish failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the publish never finished")
	}
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if arrived != 1 {
		t.Fatalf("one confirmed publish arrived %d times; a publish handed over during paho's replay is sent twice", arrived)
	}
}
