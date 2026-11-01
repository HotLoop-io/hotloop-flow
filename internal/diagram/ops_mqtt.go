package diagram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// A message for the broker, as it waits on disk: where it goes and the bytes.
type mqttEnvelope struct {
	Topic   string `json:"topic"`
	QoS     byte   `json:"qos"`
	Retain  bool   `json:"retain"`
	Payload []byte `json:"payload"`
}

func mqttDeliver(m *Manager, raw map[string]any) func(ctx context.Context, body []byte) error {
	broker := strProp(raw, "broker", "")
	return func(ctx context.Context, body []byte) error {
		var e mqttEnvelope
		if err := json.Unmarshal(body, &e); err != nil {
			return fmt.Errorf("a queued message is not readable: %w", err)
		}
		cfg, ok := m.configNode(broker)
		if !ok {
			return fmt.Errorf("the broker %s is not running", broker)
		}
		return nodes.PublishMQTT(ctx, cfg, e.Topic, e.QoS, e.Retain, e.Payload)
	}
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-mqtt-out", Color: "#bfd9c9", Icon: "mqtt", PaletteLabel: "mqtt out", LabelProp: "topic",
			InputPorts: []string{"value", "topic?", "err?"}, OutputPorts: []string{"sent", "err"},
			Props: []node.Prop{
				{Name: "broker", Label: "Server", Kind: node.PropConfigRef, ConfigType: "mqtt-broker", Required: true},
				{Name: "topic", Label: "Topic", Kind: node.PropString, Help: "Used when no topic is wired in."},
				choice("qos", "QoS", "0", "0", "1", "2"),
				prop("retain", "Retain", node.PropBool, false),
				{Name: "durable", Label: "Keep what the broker misses", Kind: node.PropBool, Default: false,
					Help: "When the broker is down, messages are written to disk and sent, in order, when it is back. They survive a restart. Without this, a message the broker misses is an error on the error wire and is gone."},
			},
			Help: "Publishes its value through an MQTT server that the flow file configures, the same connection the MQTT nodes use. Text goes as it is, a number without trailing zeros, and a record or array as JSON. The sent output is true when the message went straight out and false when it is waiting on disk.",
		},
		Durable: func(raw map[string]any) bool { d, _ := raw["durable"].(bool); return d },
		Deliver: mqttDeliver,
		New: func(raw map[string]any) (Op, error) {
			broker := strProp(raw, "broker", "")
			if broker == "" {
				return nil, fmt.Errorf("no server selected")
			}
			topic := strProp(raw, "topic", "")
			qos := intOr(raw["qos"], 0)
			if qos < 0 || qos > 2 {
				return nil, fmt.Errorf("qos must be 0, 1 or 2, got %d", qos)
			}
			retain, _ := raw["retain"].(bool)
			durable, _ := raw["durable"].(bool)
			return opFunc(func(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				t := topic
				if s, ok := in["topic"].(string); ok && s != "" {
					t = s
				}
				if t == "" {
					return nil, &Fault{Code: CodeBadInput, Source: "no topic: set one on the node or wire one in"}
				}
				if strings.ContainsAny(t, "+#") {
					return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("%q is a subscription filter, not a topic that can be published to", t)}
				}
				payload, err := nodes.EncodeMQTTPayload(jsonValue(in["value"]))
				if err != nil {
					return nil, &Fault{Code: CodeBadInput, Source: err.Error()}
				}
				m := rc.Run.mgr
				if !durable {
					cfg, ok := m.configNode(broker)
					if !ok {
						return nil, &Fault{Code: CodeDevice, Source: fmt.Sprintf("the broker %s is not running", broker)}
					}
					if err := nodes.PublishMQTT(ctx, cfg, t, byte(qos), retain, payload); err != nil {
						return nil, &Fault{Code: CodeDevice, Source: err.Error()}
					}
					return map[string]any{"sent": true}, nil
				}
				box := m.outboxFor(rc.Diagram.ID, rc.Node.ID)
				if box == nil {
					return nil, &Fault{Code: CodeDevice, Source: "a durable output needs the state store, and this runtime has none"}
				}
				body, _ := json.Marshal(mqttEnvelope{Topic: t, QoS: byte(qos), Retain: retain, Payload: payload})
				sent, err := box.Put(ctx, body)
				if err != nil {
					code := CodeDevice
					if err == store.ErrQuota {
						code = CodeFile
					}
					return nil, &Fault{Code: code, Source: err.Error()}
				}
				return map[string]any{"sent": sent}, nil
			}), nil
		},
	})
}
