package nodes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// mqttV3Broker is an MQTT 3.1 or 3.1.1 connection on paho.mqtt.golang, shared
// by every node that references the config.
type mqttV3Broker struct {
	opts  *mqtt.ClientOptions
	close *mqttLWT

	mu     sync.Mutex
	client mqtt.Client
	// subs is replayed on reconnect. Paho resubscribes automatically only when
	// clean session is off; keeping our own record makes the behaviour the same
	// either way, which matters because an edge link drops constantly.
	subs map[string]*mqttV3Sub
}

// mqttV3Sub is one topic subscription and every node listening on it.
type mqttV3Sub struct {
	qos      byte
	handlers map[string]func(mqttMessage)
}

func newMQTTv3Broker(s mqttBrokerSettings) *mqttV3Broker {
	scheme := "tcp"
	if s.useTLS {
		scheme = "ssl"
	}
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("%s://%s:%d", scheme, s.host, s.port))
	opts.SetClientID(s.clientID)
	opts.SetCleanSession(s.cleanSession)
	opts.SetKeepAlive(s.keepalive)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(30 * time.Second)
	opts.SetConnectTimeout(10 * time.Second)
	// Paho drops messages silently when its internal channel fills. Ordering
	// matters more than throughput for control traffic, so handlers run in
	// order on one goroutine and the scheduler's own bounded inbox provides
	// the back-pressure.
	opts.SetOrderMatters(true)
	if s.version == 3 {
		opts.SetProtocolVersion(3)
	}
	if s.username != "" {
		opts.SetUsername(s.username)
		if s.hasPassword {
			opts.SetPassword(s.password)
		}
	}
	if s.tlsConfig != nil {
		opts.SetTLSConfig(s.tlsConfig)
	}
	if w := s.will; w != nil {
		// The broker publishes this if the connection drops without a clean
		// disconnect, which is how everything else on the bus finds out the
		// line's flow went away instead of waiting on a timeout.
		opts.SetBinaryWill(w.Topic, w.Payload, w.QoS, w.Retain)
	}

	b := &mqttV3Broker{opts: opts, close: s.close, subs: map[string]*mqttV3Sub{}}
	birth := s.birth
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		if birth != nil {
			client.Publish(birth.Topic, birth.QoS, birth.Retain, birth.Payload)
		}
		b.replaySubscriptions(client)
	})
	return b
}

func (b *mqttV3Broker) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (b *mqttV3Broker) V5() bool { return false }

// clientLocked returns the client, connecting on first use.
//
// Lazily, for the same reason the PostgreSQL pool is lazy: a broker that is not
// up yet must not stop the flow from starting. On an edge box the broker and
// the app usually boot together.
func (b *mqttV3Broker) clientLocked() mqtt.Client {
	if b.client == nil {
		b.client = mqtt.NewClient(b.opts)
	}
	if !b.client.IsConnected() {
		// Fire and forget: paho's auto-reconnect keeps trying, and blocking a
		// node's goroutine on a broker that is down would stall its inbox.
		b.client.Connect()
	}
	return b.client
}

func (b *mqttV3Broker) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clientLocked().IsConnected()
}

func (b *mqttV3Broker) Publish(_ context.Context, m mqttMessage) error {
	b.mu.Lock()
	client := b.clientLocked()
	b.mu.Unlock()
	tok := client.Publish(m.Topic, m.QoS, m.Retain, m.Payload)
	// QoS 0 is fire-and-forget by definition; waiting for it would add latency
	// for a confirmation the protocol never sends.
	if m.QoS == 0 {
		return nil
	}
	if !tok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("timed out waiting for the broker to acknowledge")
	}
	return tok.Error()
}

// handlerFor fans one paho subscription out to every node on the topic.
func (b *mqttV3Broker) handlerFor(topic string) mqtt.MessageHandler {
	return func(_ mqtt.Client, msg mqtt.Message) {
		b.mu.Lock()
		sub, ok := b.subs[topic]
		var hs []func(mqttMessage)
		if ok {
			for _, h := range sub.handlers {
				hs = append(hs, h)
			}
		}
		b.mu.Unlock()
		pm := mqttMessage{Topic: msg.Topic(), Payload: msg.Payload(), QoS: msg.Qos(), Retain: msg.Retained()}
		for _, h := range hs {
			h(pm)
		}
	}
}

func (b *mqttV3Broker) Subscribe(owner, topic string, qos byte, _ mqttSubOptions, h func(mqttMessage)) error {
	b.mu.Lock()
	sub, ok := b.subs[topic]
	if !ok {
		sub = &mqttV3Sub{qos: qos, handlers: map[string]func(mqttMessage){}}
		b.subs[topic] = sub
	}
	sub.handlers[owner] = h
	if qos > sub.qos {
		sub.qos = qos
	}
	client := b.clientLocked()
	qos = sub.qos
	b.mu.Unlock()

	if !client.IsConnected() {
		// Recorded and replayed by the connect handler once the link comes up.
		return nil
	}
	tok := client.Subscribe(topic, qos, b.handlerFor(topic))
	if !tok.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("timed out subscribing to %q", topic)
	}
	return tok.Error()
}

func (b *mqttV3Broker) Unsubscribe(owner, topic string) error {
	b.mu.Lock()
	sub, ok := b.subs[topic]
	if ok {
		delete(sub.handlers, owner)
	}
	last := ok && len(sub.handlers) == 0
	if last {
		delete(b.subs, topic)
	}
	client := b.client
	b.mu.Unlock()

	// The subscription stays while another node still listens on it.
	if !last || client == nil || !client.IsConnected() {
		return nil
	}
	tok := client.Unsubscribe(topic)
	tok.WaitTimeout(5 * time.Second)
	return tok.Error()
}

func (b *mqttV3Broker) replaySubscriptions(client mqtt.Client) {
	b.mu.Lock()
	type entry struct {
		topic string
		qos   byte
	}
	var subs []entry
	for t, s := range b.subs {
		subs = append(subs, entry{t, s.qos})
	}
	b.mu.Unlock()

	for _, s := range subs {
		client.Subscribe(s.topic, s.qos, b.handlerFor(s.topic))
	}
}

func (b *mqttV3Broker) Close(context.Context, bool) error {
	b.mu.Lock()
	client := b.client
	b.client = nil
	b.mu.Unlock()

	if client == nil {
		return nil
	}
	if b.close != nil && client.IsConnected() {
		tok := client.Publish(b.close.Topic, b.close.QoS, b.close.Retain, b.close.Payload)
		tok.WaitTimeout(2 * time.Second)
	}
	// Give in-flight publishes a moment to leave before the socket closes. A
	// clean disconnect is also what tells the broker not to send the will.
	client.Disconnect(500)
	return nil
}
