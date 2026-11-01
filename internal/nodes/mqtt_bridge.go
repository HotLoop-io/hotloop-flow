package nodes

import (
	"context"
	"fmt"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// These are the way code that is not a message node, a FlowLab diagram, reaches
// a running mqtt-broker config node: the same connection, credentials and TLS
// the MQTT nodes use, so a diagram never holds a second set of them.

// PublishMQTT publishes through a running mqtt-broker config node. It fails when
// the connection is down rather than queueing behind the caller's back.
func PublishMQTT(ctx context.Context, cfg node.Node, topic string, qos byte, retain bool, payload []byte) error {
	b, ok := cfg.(MQTTBroker)
	if !ok {
		return fmt.Errorf("the config node is not an MQTT broker")
	}
	if topic == "" {
		return fmt.Errorf("no topic")
	}
	if qos > 2 {
		return fmt.Errorf("qos must be 0, 1 or 2, got %d", qos)
	}
	for _, c := range topic {
		if c == '+' || c == '#' {
			return fmt.Errorf("%q is a subscription filter, not a topic that can be published to", topic)
		}
	}
	// A connection that is down is an error now, never a message parked in the
	// client's memory: the caller decides whether to keep it, and a durable
	// output writes it to disk.
	if lu, ok := cfg.(interface{ LinkUp() bool }); ok && !lu.LinkUp() {
		return fmt.Errorf("broker link is down")
	}
	if !b.Connected() {
		return fmt.Errorf("broker is not connected")
	}
	if err := b.Publish(ctx, mqttMessage{Topic: topic, QoS: qos, Retain: retain, Payload: payload}); err != nil {
		return fmt.Errorf("publishing to %q: %w", topic, err)
	}
	return nil
}

// EncodeMQTTPayload renders a value for the wire the way the MQTT Out node
// does: text as it is, numbers without trailing zeros, and records and arrays as
// JSON.
func EncodeMQTTPayload(v any) ([]byte, error) { return encodeMQTTPayload(v) }
