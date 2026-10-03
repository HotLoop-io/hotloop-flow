package nodes

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/eclipse/paho.golang/paho"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// MQTT 5 and last will, against a real broker. Every test talks to the broker
// named by HOTLOOP_FLOW_TEST_MQTT, a Mosquitto 2 locally and in CI, and the
// will tests put a TCP proxy in the middle so a connection can be cut the way
// a pulled cable cuts it: no DISCONNECT, just gone.

// cutProxy forwards TCP to the broker until it is cut.
type cutProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
	paused bool
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(func() { p.cut(); ln.Close() })
	return p
}

func (p *cutProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.paused {
			p.mu.Unlock()
			c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			p.mu.Unlock()
			c.Close()
			continue
		}
		p.conns = append(p.conns, c, up)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(up, c); up.Close() }()
		go func() { _, _ = io.Copy(c, up); c.Close() }()
	}
}

// cut drops every connection without a word, and refuses new ones until
// resume.
func (p *cutProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = true
	for _, c := range p.conns {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		c.Close()
	}
	p.conns = nil
}

func (p *cutProxy) resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
}

func (p *cutProxy) port() string {
	_, port, _ := net.SplitHostPort(p.ln.Addr().String())
	return port
}

// v5Watcher is an outside MQTT 5 client recording everything on its topics.
type v5Watcher struct {
	mu   sync.Mutex
	got  []*paho.Publish
	conn *paho.Client
}

func watchV5(t *testing.T, addr string, topics ...string) *v5Watcher {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	w := &v5Watcher{}
	c := paho.NewClient(paho.ClientConfig{
		Conn: conn,
		OnPublishReceived: []func(paho.PublishReceived) (bool, error){func(pr paho.PublishReceived) (bool, error) {
			w.mu.Lock()
			w.got = append(w.got, pr.Packet)
			w.mu.Unlock()
			return true, nil
		}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Connect(ctx, &paho.Connect{ClientID: "flowgaps-watch-" + engine.GenerateID(), CleanStart: true, KeepAlive: 30}); err != nil {
		t.Fatalf("watcher connect: %v", err)
	}
	for _, topic := range topics {
		if _, err := c.Subscribe(ctx, &paho.Subscribe{Subscriptions: []paho.SubscribeOptions{{Topic: topic, QoS: 1}}}); err != nil {
			t.Fatalf("watcher subscribe %s: %v", topic, err)
		}
	}
	w.conn = c
	t.Cleanup(func() { _ = c.Disconnect(&paho.Disconnect{}) })
	return w
}

func (w *v5Watcher) all() []*paho.Publish {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*paho.Publish(nil), w.got...)
}

func (w *v5Watcher) publish(t *testing.T, p *paho.Publish) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := w.conn.Publish(ctx, p); err != nil {
		t.Fatalf("watcher publish: %v", err)
	}
}

// brokerNode builds an mqtt-broker config node and returns it with services
// that resolve it, closing it when the test ends.
func brokerNode(t *testing.T, cfg map[string]any) (*credServices, node.Node) {
	t.Helper()
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	raw, _ := json.Marshal(cfg)
	b := build(t, "mqtt-broker", string(raw), svc)
	svc.configs["brk"] = b
	t.Cleanup(func() { _ = b.(node.Closer).Close(context.Background(), false) })
	return svc, b
}

func waitConnected(t *testing.T, b node.Node) {
	t.Helper()
	waitFor(t, 15*time.Second, "the broker connection", func() bool { return b.(MQTTBroker).Connected() })
}

func mqttAddr(t *testing.T) (string, string, int) {
	t.Helper()
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_MQTT")
	host, port := splitHostPort(t, addr)
	p, _ := strconv.Atoi(port)
	return addr, host, p
}

// The will is the broker's job, and it only does it when a connection dies
// without saying goodbye. On both protocol versions, a cut connection gets the
// will out, with its QoS, retain flag and, on version 5, its properties and
// its delay.
func TestMQTTWillOnAConnectionThatDies(t *testing.T) {
	addr, _, _ := mqttAddr(t)
	for _, version := range []string{"4", "5"} {
		t.Run("protocol "+version, func(t *testing.T) {
			proxy := newCutProxy(t, addr)
			will := "flowgaps/will/" + engine.GenerateID()
			w := watchV5(t, addr, will)
			cfg := map[string]any{
				"broker": "127.0.0.1", "port": proxy.port(), "protocolVersion": version,
				"clientid": "flowgaps-will-" + engine.GenerateID(), "keepalive": 30,
				"willTopic": will, "willPayload": "line 3 flow is gone", "willQos": "1", "willRetain": "false",
			}
			if version == "5" {
				cfg["willMsg"] = map[string]any{"contentType": "text/plain", "respTopic": "line/3/status",
					"correl": "shift-b", "expiry": 600, "userProps": `{"line":"3"}`, "delay": 2}
			}
			_, b := brokerNode(t, cfg)
			waitConnected(t, b)

			cutAt := time.Now()
			proxy.cut()
			waitFor(t, 15*time.Second, "the will", func() bool { return len(w.all()) == 1 })
			got := w.all()[0]
			if string(got.Payload) != "line 3 flow is gone" || got.QoS != 1 {
				t.Errorf("will = %q at QoS %d", got.Payload, got.QoS)
			}
			if version == "5" {
				// Every property, including the five paho.golang 0.23.0 leaves
				// out of a will on its own.
				p := got.Properties
				if p == nil || p.ContentType != "text/plain" || p.ResponseTopic != "line/3/status" ||
					string(p.CorrelationData) != "shift-b" || p.MessageExpiry == nil || *p.MessageExpiry > 600 ||
					p.User.Get("line") != "3" {
					t.Errorf("will properties = %+v", p)
				}
				// Mosquitto runs will delays on a one second tick, so a two
				// second delay lands somewhere past one.
				if took := time.Since(cutAt); took < time.Second {
					t.Errorf("the will came %s after the cut, before its two second delay", took)
				}
			}
		})
	}
}

// A clean close publishes the close message and tells the broker the will is
// not needed.
func TestMQTTCleanCloseSendsTheCloseMessageAndNoWill(t *testing.T) {
	addr, host, port := mqttAddr(t)
	for _, version := range []string{"4", "5"} {
		t.Run("protocol "+version, func(t *testing.T) {
			will := "flowgaps/will/" + engine.GenerateID()
			closeTopic := "flowgaps/close/" + engine.GenerateID()
			w := watchV5(t, addr, will, closeTopic)
			_, b := brokerNode(t, map[string]any{
				"broker": host, "port": port, "protocolVersion": version,
				"clientid":  "flowgaps-close-" + engine.GenerateID(),
				"willTopic": will, "willPayload": "gone",
				"closeTopic": closeTopic, "closePayload": "stopping", "closeQos": "1", "closeRetain": "true",
			})
			waitConnected(t, b)
			if err := b.(node.Closer).Close(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			waitFor(t, 10*time.Second, "the close message", func() bool { return len(w.all()) >= 1 })
			time.Sleep(time.Second)
			for _, p := range w.all() {
				if p.Topic == will {
					t.Fatal("the broker sent the will after a clean close")
				}
			}
			// Retained, so a client arriving later still sees the line stopped.
			late := watchV5(t, addr, closeTopic)
			waitFor(t, 10*time.Second, "the retained close message", func() bool { return len(late.all()) == 1 })
			if string(late.all()[0].Payload) != "stopping" {
				t.Errorf("retained close = %q", late.all()[0].Payload)
			}
			late.publish(t, &paho.Publish{Topic: closeTopic, Retain: true, QoS: 1}) // clear it
		})
	}
}

// Properties set on the node or the message reach the wire, and properties on
// the wire reach the message, under Node-RED's names.
func TestMQTTv5PropertiesBothWays(t *testing.T) {
	addr, host, port := mqttAddr(t)
	topic := "flowgaps/props/" + engine.GenerateID()
	w := watchV5(t, addr, topic+"/out")
	svc, b := brokerNode(t, map[string]any{"broker": host, "port": port, "protocolVersion": "5",
		"clientid": "flowgaps-props-" + engine.GenerateID()})

	in := build(t, "mqtt in", `{"broker":"brk","topic":"`+topic+`/in","qos":"1","datatype":"auto-detect"}`, svc)
	ie := newTestEmitter()
	_, cancel := startNode(t, in, ie)
	defer cancel()
	waitConnected(t, b)
	time.Sleep(300 * time.Millisecond)

	// Out: the node's content type wins over the message's, the rest come
	// from the message.
	out := build(t, "mqtt out", `{"broker":"brk","topic":"`+topic+`/out","qos":"1","contentType":"application/json"}`, svc)
	m := msg(t, `{"payload":{"rpm":1450},"contentType":"text/plain","responseTopic":"`+topic+`/reply",
        "messageExpiryInterval":60,"userProperties":{"line":"3","shift":2}}`)
	m.Data["correlationData"] = []byte("req-7")
	if _, err := send(t, out, m); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the published message", func() bool { return len(w.all()) == 1 })
	p := w.all()[0]
	pp := p.Properties
	if string(p.Payload) != `{"rpm":1450}` || pp == nil || pp.ContentType != "application/json" ||
		pp.ResponseTopic != topic+"/reply" || string(pp.CorrelationData) != "req-7" ||
		pp.MessageExpiry == nil || *pp.MessageExpiry > 60 || pp.User.Get("line") != "3" || pp.User.Get("shift") != "2" {
		t.Fatalf("published %q with %+v", p.Payload, pp)
	}

	// In: properties land on the message, and a JSON content type decodes.
	exp := uint32(30)
	w.publish(t, &paho.Publish{Topic: topic + "/in", QoS: 1, Payload: []byte(`{"temp":21.5}`), Properties: &paho.PublishProperties{
		ContentType: "application/json", ResponseTopic: topic + "/back", CorrelationData: []byte("c1"),
		MessageExpiry: &exp, User: paho.UserProperties{{Key: "site", Value: "north"}},
	}})
	waitFor(t, 10*time.Second, "the subscribed message", func() bool { return ie.total() == 1 })
	got := ie.on(0)[0]
	if !reflect.DeepEqual(got.Payload(), map[string]any{"temp": 21.5}) || got.Data["contentType"] != "application/json" ||
		got.Data["responseTopic"] != topic+"/back" || !reflect.DeepEqual(got.Data["correlationData"], []byte("c1")) ||
		!reflect.DeepEqual(got.Data["userProperties"], map[string]any{"site": "north"}) {
		t.Fatalf("received %v", got.Data)
	}
	if e, _ := got.Data["messageExpiryInterval"].(float64); e < 1 || e > 30 {
		t.Errorf("messageExpiryInterval = %v", got.Data["messageExpiryInterval"])
	}

	// A plain text content type keeps JSON-looking text as text.
	w.publish(t, &paho.Publish{Topic: topic + "/in", QoS: 1, Payload: []byte(`{"temp":1}`),
		Properties: &paho.PublishProperties{ContentType: "text/plain"}})
	waitFor(t, 10*time.Second, "the text message", func() bool { return ie.total() == 2 })
	if got := ie.on(0)[1].Payload(); got != `{"temp":1}` {
		t.Errorf("text/plain decoded to %#v", got)
	}

	// No topic anywhere: the response topic is the topic.
	reply := build(t, "mqtt out", `{"broker":"brk","qos":"1"}`, svc)
	w2 := watchV5(t, addr, topic+"/reply2")
	r := msg(t, `{"payload":"pong","responseTopic":"`+topic+`/reply2"}`)
	if _, err := send(t, reply, r); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the reply on the response topic", func() bool { return len(w2.all()) == 1 })
}

// The three subscription options: no local, retain as published, and retain
// handling.
func TestMQTTv5SubscriptionOptions(t *testing.T) {
	addr, host, port := mqttAddr(t)
	topic := "flowgaps/subopts/" + engine.GenerateID()
	w := watchV5(t, addr)
	svc, b := brokerNode(t, map[string]any{"broker": host, "port": port, "protocolVersion": "5",
		"clientid": "flowgaps-subopts-" + engine.GenerateID()})
	waitConnected(t, b)

	// A retained message waiting on two topics.
	w.publish(t, &paho.Publish{Topic: topic + "/rh0", QoS: 1, Retain: true, Payload: []byte("kept")})
	w.publish(t, &paho.Publish{Topic: topic + "/rh2", QoS: 1, Retain: true, Payload: []byte("kept")})
	defer w.publish(t, &paho.Publish{Topic: topic + "/rh0", QoS: 1, Retain: true})
	defer w.publish(t, &paho.Publish{Topic: topic + "/rh2", QoS: 1, Retain: true})

	start := func(cfg string) *testEmitter {
		n := build(t, "mqtt in", cfg, svc)
		e := newTestEmitter()
		_, cancel := startNode(t, n, e)
		t.Cleanup(cancel)
		return e
	}
	rh0 := start(`{"broker":"brk","topic":"` + topic + `/rh0","qos":"1","rh":"0"}`)
	rh2 := start(`{"broker":"brk","topic":"` + topic + `/rh2","qos":"1","rh":"2"}`)
	noLocal := start(`{"broker":"brk","topic":"` + topic + `/nl","qos":"1","nl":true}`)
	rapOn := start(`{"broker":"brk","topic":"` + topic + `/rap1","qos":"1","rap":true}`)
	rapOff := start(`{"broker":"brk","topic":"` + topic + `/rap0","qos":"1","rap":false}`)
	time.Sleep(500 * time.Millisecond)

	waitFor(t, 10*time.Second, "the retained message with retain handling 0", func() bool { return rh0.total() == 1 })
	if rh2.total() != 0 {
		t.Error("retain handling 2 still delivered the retained message")
	}

	// No local: this connection's own publish does not come back, a
	// publish from anyone else does.
	out := build(t, "mqtt out", `{"broker":"brk","qos":"1"}`, svc)
	if _, err := send(t, out, msg(t, `{"topic":"`+topic+`/nl","payload":"mine"}`)); err != nil {
		t.Fatal(err)
	}
	w.publish(t, &paho.Publish{Topic: topic + "/nl", QoS: 1, Payload: []byte("theirs")})
	waitFor(t, 10*time.Second, "the other client's message", func() bool { return noLocal.total() >= 1 })
	time.Sleep(300 * time.Millisecond)
	if got := mqttPayloads(noLocal.on(0)); !reflect.DeepEqual(got, []any{"theirs"}) {
		t.Errorf("no local delivered %v", got)
	}

	// Retain as published: a live message published retained keeps its flag
	// only when asked to.
	w.publish(t, &paho.Publish{Topic: topic + "/rap1", QoS: 1, Retain: true, Payload: []byte("x")})
	w.publish(t, &paho.Publish{Topic: topic + "/rap0", QoS: 1, Retain: true, Payload: []byte("x")})
	defer w.publish(t, &paho.Publish{Topic: topic + "/rap1", QoS: 1, Retain: true})
	defer w.publish(t, &paho.Publish{Topic: topic + "/rap0", QoS: 1, Retain: true})
	waitFor(t, 10*time.Second, "both retained publishes", func() bool { return rapOn.total() == 1 && rapOff.total() == 1 })
	if rapOn.on(0)[0].Data["retain"] != true || rapOff.on(0)[0].Data["retain"] != false {
		t.Errorf("retain flags: as published %v, cleared %v", rapOn.on(0)[0].Data["retain"], rapOff.on(0)[0].Data["retain"])
	}
}

// A session that outlives its connection: messages published while the cable
// was out arrive when it comes back. With no session expiry they are gone, and
// that is the control that proves the first half means something.
func TestMQTTv5SessionExpiryKeepsMessagesAcrossADrop(t *testing.T) {
	addr, _, _ := mqttAddr(t)
	for _, tc := range []struct {
		expiry int
		want   int
	}{{60, 1}, {0, 0}} {
		t.Run("expiry "+strconv.Itoa(tc.expiry), func(t *testing.T) {
			proxy := newCutProxy(t, addr)
			topic := "flowgaps/session/" + engine.GenerateID()
			w := watchV5(t, addr)
			svc, b := brokerNode(t, map[string]any{"broker": "127.0.0.1", "port": proxy.port(), "protocolVersion": "5",
				"clientid": "flowgaps-session-" + engine.GenerateID(), "cleansession": false, "sessionExpiry": tc.expiry})
			in := build(t, "mqtt in", `{"broker":"brk","topic":"`+topic+`","qos":"1"}`, svc)
			e := newTestEmitter()
			_, cancel := startNode(t, in, e)
			defer cancel()
			waitConnected(t, b)
			time.Sleep(300 * time.Millisecond)

			proxy.cut()
			waitFor(t, 10*time.Second, "the drop to be noticed", func() bool { return !b.(MQTTBroker).Connected() })
			w.publish(t, &paho.Publish{Topic: topic, QoS: 1, Payload: []byte("while you were out")})
			proxy.resume()
			waitConnected(t, b)
			time.Sleep(time.Second)
			if e.total() != tc.want {
				t.Fatalf("with a session expiry of %d, %d messages arrived after the reconnect, want %d",
					tc.expiry, e.total(), tc.want)
			}
		})
	}
}

// Two MQTT In nodes on the same topic both get every message, and closing one
// leaves the other subscribed. This build used to key subscriptions by topic
// alone, so the second node replaced the first and closing either one
// unsubscribed both.
func TestMQTTTwoNodesOnOneTopic(t *testing.T) {
	addr, host, port := mqttAddr(t)
	for _, version := range []string{"4", "5"} {
		t.Run("protocol "+version, func(t *testing.T) {
			topic := "flowgaps/shared/" + engine.GenerateID()
			w := watchV5(t, addr)
			svc, b := brokerNode(t, map[string]any{"broker": host, "port": port, "protocolVersion": version,
				"clientid": "flowgaps-shared-" + engine.GenerateID()})
			mk := func(id string) (node.Node, *testEmitter) {
				n := build(t, "mqtt in", `{"broker":"brk","topic":"`+topic+`","qos":"1"}`, svc)
				n.(*mqttInNode).id = id
				e := newTestEmitter()
				_, cancel := startNode(t, n, e)
				t.Cleanup(cancel)
				return n, e
			}
			first, e1 := mk("in-1")
			_, e2 := mk("in-2")
			waitConnected(t, b)
			time.Sleep(300 * time.Millisecond)

			w.publish(t, &paho.Publish{Topic: topic, QoS: 1, Payload: []byte("one")})
			waitFor(t, 10*time.Second, "both nodes", func() bool { return e1.total() == 1 && e2.total() == 1 })

			if err := first.(node.Closer).Close(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			w.publish(t, &paho.Publish{Topic: topic, QoS: 1, Payload: []byte("two")})
			waitFor(t, 10*time.Second, "the remaining node", func() bool { return e2.total() == 2 })
			time.Sleep(300 * time.Millisecond)
			if e1.total() != 1 {
				t.Error("the closed node still received")
			}
		})
	}
}

// Overlapping subscriptions on one MQTT 5 connection: the broker sends a copy
// per subscription, and the subscription identifier sends each copy only to
// its own subscribers.
func TestMQTTv5OverlappingSubscriptionsDeliverOnce(t *testing.T) {
	addr, host, port := mqttAddr(t)
	base := "flowgaps/overlap/" + engine.GenerateID()
	w := watchV5(t, addr)
	svc, b := brokerNode(t, map[string]any{"broker": host, "port": port, "protocolVersion": "5",
		"clientid": "flowgaps-overlap-" + engine.GenerateID()})
	mk := func(id, topic string) *testEmitter {
		n := build(t, "mqtt in", `{"broker":"brk","topic":"`+topic+`","qos":"1"}`, svc)
		n.(*mqttInNode).id = id
		e := newTestEmitter()
		_, cancel := startNode(t, n, e)
		t.Cleanup(cancel)
		return e
	}
	wide := mk("wide", base+"/#")
	exact := mk("exact", base+"/a")
	waitConnected(t, b)
	time.Sleep(300 * time.Millisecond)

	w.publish(t, &paho.Publish{Topic: base + "/a", QoS: 1, Payload: []byte("x")})
	waitFor(t, 10*time.Second, "both subscriptions", func() bool { return wide.total() >= 1 && exact.total() >= 1 })
	time.Sleep(500 * time.Millisecond)
	if wide.total() != 1 || exact.total() != 1 {
		t.Errorf("wide got %d, exact got %d; each should get exactly one", wide.total(), exact.total())
	}
}

// The 3.1.1 path still talks to a plain 3.1.1 client the way it always has.
func TestMQTTv311StillRoundTrips(t *testing.T) {
	addr, host, port := mqttAddr(t)
	topic := "flowgaps/v311/" + engine.GenerateID()
	ext := mqtt.NewClient(mqtt.NewClientOptions().AddBroker("tcp://" + addr).SetClientID("flowgaps-ext-" + engine.GenerateID()))
	if tok := ext.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer ext.Disconnect(100)
	svc, b := brokerNode(t, map[string]any{"broker": host, "port": port,
		"clientid": "flowgaps-v311-" + engine.GenerateID()})
	in := build(t, "mqtt in", `{"broker":"brk","topic":"`+topic+`","qos":"1"}`, svc)
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	defer cancel()
	waitConnected(t, b)
	time.Sleep(300 * time.Millisecond)
	ext.Publish(topic, 1, false, `{"a":1}`).WaitTimeout(5 * time.Second)
	waitFor(t, 10*time.Second, "the message", func() bool { return e.total() == 1 })
	got := e.on(0)[0]
	if !reflect.DeepEqual(got.Payload(), map[string]any{"a": 1.0}) {
		t.Errorf("payload = %#v", got.Payload())
	}
	if _, has := got.Data["userProperties"]; has {
		t.Error("a 3.1.1 message grew MQTT 5 properties")
	}
}

func mqttPayloads(ms []*engine.Msg) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m.Payload()
	}
	return out
}
