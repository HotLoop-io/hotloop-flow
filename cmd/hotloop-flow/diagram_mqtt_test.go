package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// gate is a TCP forwarder to the real broker that can be cut and restored, so
// the broker "goes down" from Flow's side while it stays up for the test's own
// subscriber. Real broker, real client, real dropped connections.
type gate struct {
	mu     sync.Mutex
	ln     net.Listener
	conns  []net.Conn
	target string
	addr   string
	silent bool // bytes are read and thrown away, and no connection is closed
}

func newGate(t *testing.T, target string) *gate {
	t.Helper()
	g := &gate{target: target}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.addr = ln.Addr().String()
	g.serve(ln)
	t.Cleanup(g.cut)
	return g
}

func (g *gate) serve(ln net.Listener) {
	g.mu.Lock()
	g.ln = ln
	g.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", g.target)
			if err != nil {
				c.Close()
				continue
			}
			g.mu.Lock()
			g.conns = append(g.conns, c, up)
			g.mu.Unlock()
			go func() { g.copy(up, c); up.Close(); c.Close() }()
			go func() { g.copy(c, up); up.Close(); c.Close() }()
		}
	}()
}

// copy forwards bytes until the connection ends, or throws them away while the
// gate is silent, which is a link that has died without telling either end.
func (g *gate) copy(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			g.mu.Lock()
			silent := g.silent
			g.mu.Unlock()
			if !silent {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// stall makes the link dead without closing it: neither side sees a close.
func (g *gate) stall() { g.mu.Lock(); g.silent = true; g.mu.Unlock() }

// cut closes the listener and every connection through it.
func (g *gate) cut() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ln != nil {
		g.ln.Close()
		g.ln = nil
	}
	for _, c := range g.conns {
		c.Close()
	}
	g.conns = nil
	g.silent = false
}

// restore listens on the same address again.
func (g *gate) restore(t *testing.T) {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", g.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	g.serve(ln)
}

// B7: a logging target that stops accepting loses nothing. A diagram publishes a
// counter to a real broker through a connection that is cut for a while in the
// middle of the run, and every number arrives, once and in order.
func TestADiagramLosesNoMessagesAcrossABrokerOutage(t *testing.T) {
	outageRun(t, false)
}

// The same run, but the link dies without a close, so the client still believes
// it is up. Publishes are handed to it, time out, and are queued on disk, and
// the client delivers them when it reconnects: several messages arrive twice,
// each one covered by a send that was not confirmed.
func TestADiagramDuplicatesAreCoveredByUnconfirmedSends(t *testing.T) {
	outageRun(t, true)
}

func outageRun(t *testing.T, stalled bool) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_MQTT")
	if addr == "" {
		t.Skip("set HOTLOOP_FLOW_TEST_MQTT=host:port to run this against a real broker")
	}
	const total = 300
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	topic := "hotloop-flow/test/" + run + "/count"

	got := &counter{}
	outside := mqtt.NewClient(mqtt.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("dia-outside-" + run).SetOrderMatters(true))
	if tok := outside.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("connecting the test client to %s: %v", addr, tok.Error())
	}
	defer outside.Disconnect(100)
	if tok := outside.Subscribe(topic, 1, got.add); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribing: %v", tok.Error())
	}

	g := newGate(t, addr)
	host, port, _ := net.SplitHostPort(g.addr)

	app := newTestApp(t)
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "state.db"), store.SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	spool, err := store.OpenSpool(db, store.SpoolOptions{AckFlush: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	app.diagrams = diagram.NewManager(nil)
	defer app.diagrams.Close()
	app.diagrams.SetServices(diagram.Services{
		ConfigNode: func(id string) (node.Node, bool) { return app.currentRuntime().ConfigNode(id) },
		Spool:      spool,
	})

	flow := fmt.Sprintf(`[
  {"id":"brk","type":"mqtt-broker","broker":%q,"port":%s,"protocolVersion":"4","clientid":"dia-%s","cleansession":true,"keepalive":5},
  {"id":"main","type":"tab","label":"Count","ew_diagram":{}},
  {"id":"body","type":"tab","label":"Count body","ew_diagram":{"role":"body"}},
  {"id":"loop","type":"dia-while","z":"main","x":200,"y":100,"body":"Count body","carried":"","ins":"","outs":"","start":"{}","maxIterations":0,"wires":[]},
  {"id":"iter","type":"dia-body-in","z":"body","x":100,"y":100,"name":"iter","ew_in":[["value","a"],[]],"wires":[["pub","last"],[]]},
  {"id":"pub","type":"dia-mqtt-out","z":"body","x":400,"y":100,"broker":"brk","topic":%q,"qos":"1","retain":false,"durable":true,"wires":[]},
  {"id":"last","type":"dia-compare","z":"body","x":300,"y":200,"op":"ge","ew_in":[["value"],[]],"wires":[["stop"],[]]},
  {"id":"top","type":"dia-const","z":"body","x":100,"y":200,"value":"%d","ew_in":[["b"],[]],"wires":[["last"],[]]},
  {"id":"stop","type":"dia-body-out","z":"body","x":500,"y":200,"name":"stop","wires":[]},
  {"id":"pace","type":"dia-wait","z":"body","x":300,"y":300,"ms":20,"wires":[]}
]`, host, port, run, topic, total-1)
	// iter feeds the publisher's value and the compare's a; the constant feeds b.
	flowJSON := flow
	deployJSON(t, app, flowJSON, runtime.DeployFull)

	waitState := func(want string) {
		eventually(t, "the diagram to be "+want, 60*time.Second, func() bool {
			r := app.diagrams.Current("main")
			if r == nil {
				return false
			}
			st, _ := r.State()
			return string(st) == want
		})
	}
	d, _ := app.diagrams.Diagram("main")
	if len(d.Problems) > 0 {
		t.Fatalf("the diagram has problems: %v", d.Problems)
	}
	eventually(t, "the broker config to connect", 15*time.Second, func() bool {
		cfg, ok := app.currentRuntime().ConfigNode("brk")
		if !ok {
			return false
		}
		type conn interface{ Connected() bool }
		c, isConn := cfg.(conn)
		return isConn && c.Connected()
	})
	if _, err := app.diagrams.Start("main", diagram.StartOptions{}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "forty messages", 20*time.Second, func() bool { return len(got.all()) >= 40 })
	if stalled {
		g.stall()
		time.Sleep(8 * time.Second) // four send timeouts
		g.cut()
		time.Sleep(time.Second)
	} else {
		g.cut() // the broker goes away
		time.Sleep(4 * time.Second)
	}
	g.restore(t)
	waitState("done")

	eventually(t, "every message to arrive", 60*time.Second, func() bool { return len(got.all()) >= total })
	time.Sleep(500 * time.Millisecond)
	msgs := got.all()
	next := 0
	dups := 0
	var dupAt []int
	for _, m := range msgs {
		n, err := strconv.Atoi(m)
		if err != nil {
			t.Fatalf("a message that is not a number: %q", m)
		}
		switch {
		case n == next:
			next++
		case n == next-1:
			dupAt = append(dupAt, n)
			dups++ // delivered twice around the cut: at least once, never lost
		default:
			t.Fatalf("out of order or missing: got %d after %d", n, next-1)
		}
	}
	if next != total {
		t.Fatalf("got %d distinct messages in order, want %d", next, total)
	}
	// At least once: a message can arrive twice only if a send of it was an
	// unconfirmed send, because the client may still deliver it after the outbox
	// stopped waiting. How many sends time out depends on how slow the machine
	// is, so the bound is the number of unconfirmed sends, not a fixed one.
	unconfirmed := app.diagrams.OutboxUnconfirmedSends("body", "pub")
	if int64(dups) > unconfirmed {
		t.Fatalf("%d messages arrived twice (%v) but only %d sends were unconfirmed; a duplicate with no unconfirmed send is a bug", dups, dupAt, unconfirmed)
	}
	direct, spilled, drained, depth, ok := app.diagrams.OutboxStats("body", "pub")
	if !ok || spilled == 0 || drained == 0 || depth != 0 {
		t.Fatalf("outbox: ok=%v direct=%d spilled=%d drained=%d depth=%d; the outage should have spilled to disk and drained", ok, direct, spilled, drained, depth)
	}
	t.Logf("%d sent straight out, %d went through disk, during the outage and behind it while the queue drained, %d arrived twice, %d sends were unconfirmed", direct, spilled, dups, unconfirmed)
}
