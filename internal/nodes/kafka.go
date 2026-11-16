package nodes

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Kafka out: a producer, beside HotLoop core's Kafka event stream output, with the
// same client (franz-go, pure Go, BSD-3-Clause, credited in NOTICE) and the same
// settings: a comma separated list of brokers, SASL plain or SCRAM-SHA-256 or
// SCRAM-SHA-512, and TLS. Anything that speaks the Kafka API answers it: Apache
// Kafka, Redpanda and the rest.
//
// A message is sent when the node receives it and passed on once the broker has
// acknowledged it from every in-sync replica. A broker that refuses or cannot be
// reached is an error on that message, and the credentials are in none of it.

func init() { registerKafka() }

var kafkaTopicRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,249}$`)

type kafkaConn struct {
	mu      sync.Mutex
	brokers []string
	mech    sasl.Mechanism
	tls     *tls.Config
	secrets []string
	client  *kgo.Client
}

func (k *kafkaConn) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (k *kafkaConn) get() (*kgo.Client, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.client != nil {
		return k.client, nil
	}
	opts := []kgo.Opt{
		kgo.SeedBrokers(k.brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProduceRequestTimeout(10 * time.Second),
		kgo.RecordDeliveryTimeout(20 * time.Second),
		kgo.RecordRetries(3),
		// Whether a topic that is not there is made is the broker's policy, not ours.
		kgo.AllowAutoTopicCreation(),
	}
	if k.mech != nil {
		opts = append(opts, kgo.SASL(k.mech))
	}
	if k.tls != nil {
		opts = append(opts, kgo.DialTLSConfig(k.tls))
	}
	c, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka: %s", k.mask(err.Error()))
	}
	k.client = c
	return c, nil
}

func (k *kafkaConn) mask(s string) string {
	for _, sec := range k.secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

func (k *kafkaConn) Close(context.Context, bool) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.client != nil {
		k.client.Close()
		k.client = nil
	}
	return nil
}

func registerKafka() {
	node.MustRegister(node.Descriptor{
		Type: "hotloop-flow-kafka", Category: node.CategoryConfig, Color: colorStorage, Icon: "db", IsConfig: true,
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own Kafka connection, the same settings HotLoop core's Kafka output uses."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "brokers", Kind: node.PropString, Label: "Brokers", Required: true, Placeholder: "kafka-1:9092,kafka-2:9092", Help: "Comma separated host:port."},
			{Name: "sasl", Kind: node.PropSelect, Label: "Login", Default: "none", Options: []node.Option{
				{Value: "none", Label: "none"}, {Value: "plain", Label: "SASL PLAIN"}, {Value: "scram-sha-256", Label: "SCRAM-SHA-256"}, {Value: "scram-sha-512", Label: "SCRAM-SHA-512"}}},
			{Name: "username", Kind: node.PropString, Label: "Username"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
			{Name: "usetls", Kind: node.PropBool, Label: "Use TLS", Default: false},
			{Name: "tls", Kind: node.PropConfigRef, ConfigType: "tls-config", Label: "TLS settings", Help: "Empty verifies the broker against the system's certificate authorities."},
		},
		Help: "A Kafka, Redpanda or other Kafka API cluster.",
	}, func(def *node.Definition) (node.Node, error) {
		var brokers []string
		for _, b := range strings.Split(def.Node.PropString("brokers", ""), ",") {
			if b = strings.TrimSpace(b); b != "" {
				if !strings.Contains(b, ":") {
					return nil, fmt.Errorf("the broker %q needs a port", b)
				}
				brokers = append(brokers, b)
			}
		}
		if len(brokers) == 0 {
			return nil, fmt.Errorf("at least one broker is required")
		}
		k := &kafkaConn{brokers: brokers}
		pw, _ := def.Services.Credential("password")
		user := def.Node.PropString("username", "")
		switch m := def.Node.PropString("sasl", "none"); m {
		case "none", "":
		case "plain", "scram-sha-256", "scram-sha-512":
			if user == "" || pw == "" {
				return nil, fmt.Errorf("a username and a password are required for %s", m)
			}
			k.secrets = []string{pw}
			switch m {
			case "plain":
				k.mech = plain.Auth{User: user, Pass: pw}.AsMechanism()
			case "scram-sha-256":
				k.mech = scram.Auth{User: user, Pass: pw}.AsSha256Mechanism()
			default:
				k.mech = scram.Auth{User: user, Pass: pw}.AsSha512Mechanism()
			}
		default:
			return nil, fmt.Errorf("the login %q is not none, plain, scram-sha-256 or scram-sha-512", m)
		}
		if def.Node.PropBool("usetls", false) {
			if ref := def.Node.PropString("tls", ""); ref != "" {
				cfg, err := lookupTLSConfig(def.Services, ref)
				if err != nil {
					return nil, err
				}
				k.tls = cfg.clientConfig()
			} else {
				k.tls = &tls.Config{MinVersion: tls.VersionTLS12}
			}
		}
		return k, nil
	})

	node.MustRegister(node.Descriptor{
		Type: "kafka out", Category: node.CategoryNetwork, StandIn: true, Color: colorStorage, Icon: "db",
		Inputs: 1, Outputs: 1, PaletteLabel: "kafka", LabelProp: "name",
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own node."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "broker", Kind: node.PropConfigRef, Label: "Cluster", ConfigType: "hotloop-flow-kafka", Required: true},
			{Name: "topic", Kind: node.PropString, Label: "Topic", Help: "Empty takes msg.topic."},
			{Name: "key", Kind: node.PropString, Label: "Key", Help: "Empty takes msg.key. Messages with one key go to one partition, in order."},
		},
		Help: "Produces msg.payload to a Kafka topic, as text, or as JSON when it is not text, and passes the message on once the cluster has acknowledged it.",
	}, func(def *node.Definition) (node.Node, error) {
		id := def.Node.PropString("broker", "")
		if id == "" {
			return nil, fmt.Errorf("no cluster selected")
		}
		return &kafkaOut{def: def, cfgID: id}, nil
	})
}

type kafkaOut struct {
	def   *node.Definition
	cfgID string
}

func (n *kafkaOut) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	cfg, ok := n.def.Services.ConfigNode(n.cfgID)
	k, isK := cfg.(*kafkaConn)
	if !ok || !isK {
		return fmt.Errorf("cluster config node %s is not a Kafka cluster", n.cfgID)
	}
	topic := pick(n.def, m, "topic", "topic")
	if !kafkaTopicRe.MatchString(topic) || topic == "." || topic == ".." {
		err := fmt.Errorf("the topic is empty or not letters, digits, . _ and -")
		notifyStatus(out, err, "")
		return err
	}
	rec := &kgo.Record{Topic: topic, Value: []byte(kafkaValue(m))}
	if key := pick(n.def, m, "key", "key"); key != "" {
		rec.Key = []byte(key)
	}
	if ok, err := standIn(n.def.Services, "kafka", map[string]any{"topic": topic, "key": string(rec.Key), "value": string(rec.Value)}); ok {
		notifyStatus(out, err, "sent")
		if err != nil {
			return err
		}
		out.Send(0, m)
		return nil
	}
	c, err := k.get()
	if err == nil {
		err = c.ProduceSync(ctx, rec).FirstErr()
		if err != nil {
			err = fmt.Errorf("kafka: %s", k.mask(err.Error()))
		}
	}
	notifyStatus(out, err, "sent")
	if err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

func kafkaValue(m *engine.Msg) string {
	if s, ok := m.Payload().(string); ok {
		return s
	}
	b, err := json.Marshal(m.Payload())
	if err != nil {
		return fmt.Sprint(m.Payload())
	}
	return string(b)
}
