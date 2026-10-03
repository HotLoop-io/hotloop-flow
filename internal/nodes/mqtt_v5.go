package nodes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// mqttV5Broker is an MQTT 5 connection on paho.golang's autopaho, which
// reconnects on its own.
//
// Incoming messages are routed by subscription identifier. Every subscription
// gets its own, so when two subscriptions overlap, a/# and a/b say, and the
// broker sends one copy per subscription, each copy goes only to the nodes on
// the subscription it was sent for. Without that, both sets of nodes would get
// both copies. A broker that does not support identifiers gets topic
// matching instead.
type mqttV5Broker struct {
	settings mqttBrokerSettings

	mu        sync.Mutex
	cm        *autopaho.ConnectionManager
	cancel    context.CancelFunc
	connected bool
	nextSubID int
	subs      map[string]*mqttV5Sub // by topic filter
	byID      map[int]*mqttV5Sub
}

type mqttV5Sub struct {
	id       int
	topic    string
	qos      byte
	opts     mqttSubOptions
	handlers map[string]func(mqttMessage)
}

func newMQTTv5Broker(s mqttBrokerSettings) *mqttV5Broker {
	return &mqttV5Broker{settings: s, subs: map[string]*mqttV5Sub{}, byID: map[int]*mqttV5Sub{}}
}

func (b *mqttV5Broker) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (b *mqttV5Broker) V5() bool { return true }

func (b *mqttV5Broker) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startLocked()
	return b.connected
}

// startLocked starts the connection manager on first use, for the same reason
// the 3.1.1 connection is lazy: a broker that is not up yet must not stop the
// flow from starting.
func (b *mqttV5Broker) startLocked() {
	if b.cm != nil {
		return
	}
	s := b.settings
	scheme := "mqtt"
	if s.useTLS {
		scheme = "tls"
	}
	u, err := url.Parse(fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(s.host, strconv.Itoa(s.port))))
	if err != nil {
		return
	}
	cfg := autopaho.ClientConfig{
		ServerUrls:                    []*url.URL{u},
		TlsCfg:                        s.tlsConfig,
		KeepAlive:                     uint16(s.keepalive / time.Second),
		CleanStartOnInitialConnection: s.cleanSession,
		SessionExpiryInterval:         s.sessionExpiry,
		ConnectTimeout:                10 * time.Second,
		// Doubling from one second to thirty, the same ceiling the 3.1.1
		// connection reconnects under.
		ReconnectBackoff: func(attempt int) time.Duration {
			d := time.Second << min(attempt, 5)
			return min(d, 30*time.Second)
		},
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			b.mu.Lock()
			b.connected = true
			subs := make([]*mqttV5Sub, 0, len(b.subs))
			for _, sub := range b.subs {
				subs = append(subs, sub)
			}
			b.mu.Unlock()
			// Not from inside this callback, which must not block.
			go func() {
				if birth := s.birth; birth != nil {
					_, _ = cm.Publish(context.Background(), lwtPublish(birth))
				}
				for _, sub := range subs {
					_ = b.sendSubscribe(context.Background(), cm, sub)
				}
			}()
		},
		OnConnectionDown: func() bool {
			b.mu.Lock()
			b.connected = false
			b.mu.Unlock()
			return true
		},
		ClientConfig: paho.ClientConfig{
			ClientID:          s.clientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){b.route},
		},
	}
	if s.username != "" {
		cfg.ConnectUsername = s.username
		if s.hasPassword {
			cfg.ConnectPassword = []byte(s.password)
		}
	}
	if w := s.will; w != nil {
		cfg.WillMessage = &paho.WillMessage{Topic: w.Topic, Payload: w.Payload, QoS: w.QoS, Retain: w.Retain}
		wp := &paho.WillProperties{WillDelayInterval: w.Delay}
		if p := w.Props; p != nil {
			wp.ContentType = p.ContentType
			wp.ResponseTopic = p.ResponseTopic
			wp.CorrelationData = p.CorrelationData
			wp.MessageExpiry = p.MessageExpiry
			wp.User = toPahoUser(p.User)
		}
		cfg.WillProperties = wp
		if willNeedsFix(w) {
			cfg.AttemptConnection = willFixDialer(w, s.tlsConfig)
		}
	}
	if len(s.userProps) > 0 {
		user := toPahoUser(s.userProps)
		cfg.ConnectPacketBuilder = func(c *paho.Connect, _ *url.URL) (*paho.Connect, error) {
			if c.Properties == nil {
				c.Properties = &paho.ConnectProperties{}
			}
			c.Properties.User = append(c.Properties.User, user...)
			return c, nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		cancel()
		return
	}
	b.cm, b.cancel = cm, cancel
}

func toPahoUser(kv [][2]string) paho.UserProperties {
	var up paho.UserProperties
	for _, p := range kv {
		up = append(up, paho.UserProperty{Key: p[0], Value: p[1]})
	}
	return up
}

func lwtPublish(m *mqttLWT) *paho.Publish {
	p := &paho.Publish{Topic: m.Topic, Payload: m.Payload, QoS: m.QoS, Retain: m.Retain}
	if m.Props != nil {
		p.Properties = pahoPublishProps(m.Props)
	}
	return p
}

func pahoPublishProps(p *mqttProps) *paho.PublishProperties {
	out := &paho.PublishProperties{
		ContentType:     p.ContentType,
		ResponseTopic:   p.ResponseTopic,
		CorrelationData: p.CorrelationData,
		MessageExpiry:   p.MessageExpiry,
		User:            toPahoUser(p.User),
	}
	if p.PayloadFormat != nil {
		var f byte
		if *p.PayloadFormat {
			f = 1
		}
		out.PayloadFormat = &f
	}
	return out
}

// route hands an incoming publish to the nodes subscribed to it.
func (b *mqttV5Broker) route(pr paho.PublishReceived) (bool, error) {
	pub := pr.Packet
	pm := mqttMessage{Topic: pub.Topic, Payload: pub.Payload, QoS: pub.QoS, Retain: pub.Retain, Props: &mqttProps{}}
	var subID *int
	if pp := pub.Properties; pp != nil {
		pm.Props.ContentType = pp.ContentType
		pm.Props.ResponseTopic = pp.ResponseTopic
		pm.Props.CorrelationData = pp.CorrelationData
		pm.Props.MessageExpiry = pp.MessageExpiry
		if pp.PayloadFormat != nil {
			utf8 := *pp.PayloadFormat == 1
			pm.Props.PayloadFormat = &utf8
		}
		for _, u := range pp.User {
			pm.Props.User = append(pm.Props.User, [2]string{u.Key, u.Value})
		}
		subID = pp.SubscriptionIdentifier
	}

	b.mu.Lock()
	var targets []*mqttV5Sub
	if subID != nil {
		if sub, ok := b.byID[*subID]; ok {
			targets = append(targets, sub)
		}
	} else {
		for _, sub := range b.subs {
			if topicMatches(sub.topic, pub.Topic) {
				targets = append(targets, sub)
			}
		}
	}
	var hs []func(mqttMessage)
	for _, sub := range targets {
		for _, h := range sub.handlers {
			hs = append(hs, h)
		}
	}
	b.mu.Unlock()

	for _, h := range hs {
		h(pm)
	}
	return len(hs) > 0, nil
}

func (b *mqttV5Broker) sendSubscribe(ctx context.Context, cm *autopaho.ConnectionManager, sub *mqttV5Sub) error {
	id := sub.id
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ack, err := cm.Subscribe(ctx, &paho.Subscribe{
		Properties: &paho.SubscribeProperties{SubscriptionIdentifier: &id},
		Subscriptions: []paho.SubscribeOptions{{
			Topic:             sub.topic,
			QoS:               sub.qos,
			NoLocal:           sub.opts.NoLocal,
			RetainAsPublished: sub.opts.RetainAsPublished,
			RetainHandling:    sub.opts.RetainHandling,
		}},
	})
	if err != nil {
		return err
	}
	if ack != nil && len(ack.Reasons) > 0 && ack.Reasons[0] >= 0x80 {
		return fmt.Errorf("the broker refused the subscription to %q (reason code 0x%02x)", sub.topic, ack.Reasons[0])
	}
	return nil
}

func (b *mqttV5Broker) Subscribe(owner, topic string, qos byte, opts mqttSubOptions, h func(mqttMessage)) error {
	b.mu.Lock()
	b.startLocked()
	sub, ok := b.subs[topic]
	if !ok {
		b.nextSubID++
		sub = &mqttV5Sub{id: b.nextSubID, topic: topic, handlers: map[string]func(mqttMessage){}}
		b.subs[topic] = sub
		b.byID[sub.id] = sub
	}
	sub.handlers[owner] = h
	sub.qos = max(sub.qos, qos)
	sub.opts = opts
	cm, connected := b.cm, b.connected
	b.mu.Unlock()

	if cm == nil || !connected {
		// Recorded and sent by OnConnectionUp once the link comes up.
		return nil
	}
	return b.sendSubscribe(context.Background(), cm, sub)
}

func (b *mqttV5Broker) Unsubscribe(owner, topic string) error {
	b.mu.Lock()
	sub, ok := b.subs[topic]
	if ok {
		delete(sub.handlers, owner)
	}
	last := ok && len(sub.handlers) == 0
	if last {
		delete(b.subs, topic)
		delete(b.byID, sub.id)
	}
	cm, connected := b.cm, b.connected
	b.mu.Unlock()
	if !last || cm == nil || !connected {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cm.Unsubscribe(ctx, &paho.Unsubscribe{Topics: []string{topic}})
	return err
}

func (b *mqttV5Broker) Publish(ctx context.Context, m mqttMessage) error {
	b.mu.Lock()
	b.startLocked()
	cm := b.cm
	b.mu.Unlock()
	if cm == nil {
		return errors.New("broker is not connected")
	}
	p := &paho.Publish{Topic: m.Topic, Payload: m.Payload, QoS: m.QoS, Retain: m.Retain}
	if m.Props != nil {
		p.Properties = pahoPublishProps(m.Props)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := cm.Publish(ctx, p)
	if err != nil {
		return err
	}
	if res != nil && res.ReasonCode >= 0x80 {
		return fmt.Errorf("the broker refused the publish (reason code 0x%02x)", res.ReasonCode)
	}
	return nil
}

func (b *mqttV5Broker) Close(ctx context.Context, _ bool) error {
	b.mu.Lock()
	cm, cancel, connected := b.cm, b.cancel, b.connected
	b.cm, b.cancel, b.connected = nil, nil, false
	b.mu.Unlock()
	if cm == nil {
		return nil
	}
	if c := b.settings.close; c != nil && connected {
		pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = cm.Publish(pctx, lwtPublish(c))
		pcancel()
	}
	// A clean disconnect is what tells the broker not to send the will.
	dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
	defer dcancel()
	err := cm.Disconnect(dctx)
	cancel()
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
