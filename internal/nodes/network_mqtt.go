package nodes

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

const colorNetwork = "#D8BFD8"

func init() {
	registerMQTTBroker()
	registerMQTTIn()
	registerMQTTOut()
}

// MQTT has two client libraries behind one interface. Versions 3.1 and 3.1.1
// run on paho.mqtt.golang, which is what this build has always used and which
// does not speak version 5. Version 5 runs on paho.golang, from the same
// Eclipse project, which speaks nothing else. Both are dual EPL-2.0 and EDL-1.0.
// A flow does not know which one it is on: the In and Out nodes talk to an
// MQTTBroker, and a v5 broker config just carries more on each message.

// MQTTBroker is how the In and Out nodes reach their shared connection.
type MQTTBroker interface {
	// Publish sends one message. It fails when the connection is down rather
	// than queueing behind the caller's back.
	Publish(ctx context.Context, m mqttMessage) error
	// Subscribe adds owner's handler for topic. Several nodes may subscribe
	// to the same topic on one connection, each with its own handler.
	Subscribe(owner, topic string, qos byte, opts mqttSubOptions, h func(mqttMessage)) error
	// Unsubscribe removes owner's handler, and the subscription itself once
	// nobody is left on it.
	Unsubscribe(owner, topic string) error
	Connected() bool
	V5() bool
}

// mqttMessage is one publish, in either direction.
type mqttMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
	// Props carries the version 5 properties. Nil on a 3.1.1 connection.
	Props *mqttProps
}

// mqttProps are the version 5 message properties a flow can set and read.
type mqttProps struct {
	ContentType     string
	ResponseTopic   string
	CorrelationData []byte
	MessageExpiry   *uint32
	PayloadFormat   *bool
	ReasonString    string
	// User keeps the order the properties were given in, because MQTT allows
	// a key more than once and a message can rely on that.
	User [][2]string
}

// mqttSubOptions are the version 5 subscription options: no local, retain as
// published and retain handling. Ignored on a 3.1.1 connection, which has
// none of them.
type mqttSubOptions struct {
	NoLocal           bool
	RetainAsPublished bool
	RetainHandling    byte
}

// mqttLWT is a birth, close or will message, as the broker config describes
// it. Node-RED calls all three last will and testament messages.
type mqttLWT struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
	Props   *mqttProps
	// Delay is the will delay interval in seconds, wills only.
	Delay *uint32
}

// mqttBrokerSettings is the broker config node read out of the flow, shared by
// both implementations.
type mqttBrokerSettings struct {
	nodeID        string
	host          string
	port          int
	useTLS        bool
	tlsConfig     *tls.Config
	clientID      string
	username      string
	password      string
	hasPassword   bool
	cleanSession  bool
	keepalive     time.Duration
	version       int // 3, 4 or 5
	sessionExpiry uint32
	userProps     [][2]string
	birth         *mqttLWT
	close         *mqttLWT
	will          *mqttLWT
}

func registerMQTTBroker() {
	node.MustRegister(node.Descriptor{
		Type:     "mqtt-broker",
		Category: node.CategoryConfig,
		Color:    colorNetwork,
		Icon:     "mqtt",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "MQTT 3.1, 3.1.1 and 5. Connection, credentials, TLS, clean session, keepalive, " +
				"and birth, close and will messages with their QoS, retain flag and, on version 5, " +
				"their properties and the will delay. On version 5 also the session expiry interval " +
				"and connect user properties. The receive maximum, maximum packet size and topic " +
				"alias maximum settings are not implemented, and the broker's defaults apply. " +
				"TLS is Node-RED's usetls with a tls-config for the CA, client certificate " +
				"and server name; a broker saved by an earlier HotLoop Flow with tls: true " +
				"still connects over TLS with the system roots.",
			UnsupportedProps: []string{"receiveMaximum", "maximumPacketSize", "topicAliasMaximum"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "broker", Kind: node.PropString, Label: "Server", Required: true,
				Placeholder: "monster-mq.tenant-fireball.svc.cluster.local"},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Default: 1883},
			{Name: "protocolVersion", Kind: node.PropSelect, Label: "Protocol", Default: "4",
				Options: []node.Option{
					{Value: "5", Label: "MQTT 5"},
					{Value: "4", Label: "MQTT 3.1.1"},
					{Value: "3", Label: "MQTT 3.1"},
				}},
			{Name: "usetls", Kind: node.PropBool, Label: "Use TLS"},
			{Name: "tls", Kind: node.PropConfigRef, ConfigType: "tls-config", Label: "TLS settings",
				Help: "A CA, a client certificate or a server name. Leave empty to check the " +
					"broker against the system roots."},
			{Name: "clientid", Kind: node.PropString, Label: "Client ID",
				Help: "Leave empty to generate one. Two clients sharing an ID disconnect each other."},
			{Name: "user", Kind: node.PropString, Label: "Username"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
			{Name: "cleansession", Kind: node.PropBool, Label: "Use a clean session", Default: true},
			{Name: "sessionExpiry", Kind: node.PropNumber, Label: "Session expiry (seconds)",
				Help: "MQTT 5: how long the broker keeps the session after a disconnect."},
			{Name: "keepalive", Kind: node.PropNumber, Label: "Keep alive (seconds)", Default: 60},
			{Name: "birthTopic", Kind: node.PropString, Label: "Birth topic",
				Help: "Published on connect."},
			{Name: "birthPayload", Kind: node.PropString, Label: "Birth payload"},
			{Name: "birthQos", Kind: node.PropSelect, Label: "Birth QoS", Default: "0", Options: mqttQoSOptions()},
			{Name: "birthRetain", Kind: node.PropBool, Label: "Retain the birth message"},
			{Name: "closeTopic", Kind: node.PropString, Label: "Close topic",
				Help: "Published on a clean disconnect."},
			{Name: "closePayload", Kind: node.PropString, Label: "Close payload"},
			{Name: "closeQos", Kind: node.PropSelect, Label: "Close QoS", Default: "0", Options: mqttQoSOptions()},
			{Name: "closeRetain", Kind: node.PropBool, Label: "Retain the close message"},
			{Name: "willTopic", Kind: node.PropString, Label: "Will topic",
				Help: "Published by the broker if this connection drops without saying goodbye."},
			{Name: "willPayload", Kind: node.PropString, Label: "Will payload"},
			{Name: "willQos", Kind: node.PropSelect, Label: "Will QoS", Default: "0", Options: mqttQoSOptions()},
			{Name: "willRetain", Kind: node.PropBool, Label: "Retain the will message"},
		},
		Help: "Connection to an MQTT broker.",
	}, newMQTTBroker)
}

func mqttQoSOptions() []node.Option {
	return []node.Option{
		{Value: "0", Label: "0 — at most once"},
		{Value: "1", Label: "1 — at least once"},
		{Value: "2", Label: "2 — exactly once"},
	}
}

func newMQTTBroker(def *node.Definition) (node.Node, error) {
	s, err := readMQTTBrokerSettings(def)
	if err != nil {
		return nil, err
	}
	if s.version == 5 {
		return newMQTTv5Broker(s), nil
	}
	return newMQTTv3Broker(s), nil
}

func readMQTTBrokerSettings(def *node.Definition) (mqttBrokerSettings, error) {
	n := def.Node
	s := mqttBrokerSettings{
		nodeID:       n.ID,
		host:         n.PropString("broker", ""),
		port:         n.PropInt("port", 1883),
		useTLS:       n.PropBool("usetls", false) || n.PropBool("tls", false),
		clientID:     n.PropString("clientid", ""),
		cleanSession: n.PropBool("cleansession", true),
		keepalive:    time.Duration(n.PropInt("keepalive", 60)) * time.Second,
		version:      4,
	}
	if s.host == "" {
		return s, fmt.Errorf("server is required")
	}
	if s.clientID == "" {
		// Derived from the node id rather than random, so a reconnect after a
		// restart resumes the same session instead of orphaning the old one.
		s.clientID = "hotloop-flow-" + n.ID
	}
	switch strings.TrimSpace(fmt.Sprint(n.Raw["protocolVersion"])) {
	case "5":
		s.version = 5
	case "3":
		s.version = 3
	}
	// Node-RED's older compatibility switch means 3.1.
	if n.PropBool("compatmode", false) {
		s.version = 3
	}
	if user := n.PropString("user", ""); user != "" {
		s.username = user
		s.password, s.hasPassword = def.Services.Credential("password")
	}
	if ref := tlsConfigRef(n); s.useTLS && ref != "" && ref != "true" && ref != "false" {
		// Node-RED's shape: usetls, and the tls-config it points at.
		cfg, err := lookupTLSConfig(def.Services, ref)
		if err != nil {
			return s, err
		}
		s.tlsConfig = cfg.clientConfig()
	} else if s.useTLS {
		s.tlsConfig = &tls.Config{
			// Defaults to verifying. Turning it off is a deliberate act because
			// an OT network with a self-signed broker is common, and quietly
			// defaulting to "trust anything" is how that becomes permanent.
			InsecureSkipVerify: !n.PropBool("verifyservercert", true),
			MinVersion:         tls.VersionTLS12,
		}
	}
	if secs := n.PropFloat("sessionExpiry", 0); secs > 0 {
		s.sessionExpiry = uint32(secs)
	} else if secs := n.PropFloat("sessionExpiryInterval", 0); secs > 0 {
		s.sessionExpiry = uint32(secs)
	}
	s.userProps = mqttUserPropsFromJSON(n.Raw["userProps"])

	var err error
	if s.birth, err = readMQTTLWT(n.Raw, "birth"); err != nil {
		return s, err
	}
	if s.close, err = readMQTTLWT(n.Raw, "close"); err != nil {
		return s, err
	}
	if s.will, err = readMQTTLWT(n.Raw, "will"); err != nil {
		return s, err
	}
	return s, nil
}

// readMQTTLWT reads a birth, close or will message the way Node-RED's
// createLWT does: no topic means no message, the QoS is a number, retain is
// true or "true", and on version 5 a <kind>Msg object carries the properties
// under Node-RED's short names.
func readMQTTLWT(raw map[string]any, kind string) (*mqttLWT, error) {
	topic, _ := raw[kind+"Topic"].(string)
	if topic == "" {
		return nil, nil
	}
	m := &mqttLWT{Topic: topic, Retain: raw[kind+"Retain"] == true || raw[kind+"Retain"] == "true"}
	if p, ok := raw[kind+"Payload"].(string); ok {
		m.Payload = []byte(p)
	}
	if q := int(mqttNumber(raw[kind+"Qos"])); q >= 0 && q <= 2 {
		m.QoS = byte(q)
	} else {
		return nil, fmt.Errorf("%s QoS must be 0, 1 or 2, got %d", kind, q)
	}
	v5, _ := raw[kind+"Msg"].(map[string]any)
	if v5 == nil {
		return m, nil
	}
	props := &mqttProps{}
	props.ContentType, _ = v5["contentType"].(string)
	props.ResponseTopic, _ = v5["respTopic"].(string)
	if c, ok := v5["correl"].(string); ok && c != "" {
		props.CorrelationData = []byte(c)
	}
	if e := mqttNumber(v5["expiry"]); e > 0 {
		exp := uint32(e)
		props.MessageExpiry = &exp
	}
	props.User = mqttUserPropsFromJSON(v5["userProps"])
	m.Props = props
	if d := mqttNumber(v5["delay"]); d > 0 && kind == "will" {
		delay := uint32(d)
		m.Delay = &delay
	}
	return m, nil
}

// mqttUserPropsFromJSON reads user properties from config: an object, or the
// JSON text of one, which is how the editor stores them. A value that is not
// a string is sent as its JSON, as Node-RED does. Keys are sorted, because a
// flow file holds them in an object and an object has no order here.
func mqttUserPropsFromJSON(v any) [][2]string {
	obj, ok := v.(map[string]any)
	if !ok {
		s, isStr := v.(string)
		if !isStr || !strings.HasPrefix(strings.TrimSpace(s), "{") {
			return nil
		}
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return nil
		}
	}
	return mqttUserPropsFromObject(obj)
}

func mqttUserPropsFromObject(obj map[string]any) [][2]string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out [][2]string
	for _, k := range keys {
		switch v := obj[k].(type) {
		case string:
			out = append(out, [2]string{k, v})
		case nil:
		default:
			b, err := json.Marshal(v)
			if err == nil {
				out = append(out, [2]string{k, string(b)})
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// mqtt in
// ---------------------------------------------------------------------------

type mqttInNode struct {
	id      string
	cfgID   string
	topic   string
	qos     byte
	output  string // auto, auto-detect, buffer, base64, utf8, json
	subOpts mqttSubOptions
	svc     node.Services
	broker  MQTTBroker
	started bool
}

func registerMQTTIn() {
	node.MustRegister(node.Descriptor{
		Type:         "mqtt in",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "mqtt",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "mqtt in",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Topic subscription with QoS and payload decoding, and on MQTT 5 the no " +
				"local, retain as published and retain handling options, with the message's " +
				"properties on msg.userProperties, msg.contentType, msg.responseTopic, " +
				"msg.correlationData and msg.messageExpiryInterval, and its content type " +
				"steering the auto-detect decoding the way Node-RED's does. Dynamic " +
				"subscription via a control message is not implemented.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "broker", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "mqtt-broker", Required: true},
			{Name: "topic", Kind: node.PropString, Label: "Topic", Required: true,
				Placeholder: "press/+/raw"},
			{Name: "qos", Kind: node.PropSelect, Label: "QoS", Default: "0", Options: mqttQoSOptions()},
			{Name: "datatype", Kind: node.PropSelect, Label: "Output", Default: "auto",
				Options: []node.Option{
					{Value: "auto", Label: "Auto — parse a JSON object or array, else a string"},
					{Value: "auto-detect", Label: "Auto-detect — parse any JSON, else a string"},
					{Value: "utf8", Label: "A string"},
					{Value: "json", Label: "A parsed JSON object"},
					{Value: "buffer", Label: "A binary buffer"},
					{Value: "base64", Label: "A base64 string"},
				}},
			{Name: "nl", Kind: node.PropBool, Label: "MQTT 5: ignore messages this connection published"},
			{Name: "rap", Kind: node.PropBool, Label: "MQTT 5: keep the retain flag as published", Default: true},
			{Name: "rh", Kind: node.PropSelect, Label: "MQTT 5: retained messages", Default: "0",
				Options: []node.Option{
					{Value: "0", Label: "Send them on every subscribe"},
					{Value: "1", Label: "Send them only on a new subscription"},
					{Value: "2", Label: "Never send them"},
				}},
		},
		Help: "Subscribes to an MQTT topic and emits a message for each one received.",
	}, newMQTTIn)
}

func newMQTTIn(def *node.Definition) (node.Node, error) {
	n := &mqttInNode{
		id:     def.Node.ID,
		cfgID:  def.Node.PropString("broker", ""),
		topic:  def.Node.PropString("topic", ""),
		output: def.Node.PropString("datatype", "auto"),
		svc:    def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected")
	}
	if n.topic == "" {
		return nil, fmt.Errorf("topic is required")
	}
	q := def.Node.PropInt("qos", 0)
	if q < 0 || q > 2 {
		return nil, fmt.Errorf("qos must be 0, 1 or 2, got %d", q)
	}
	n.qos = byte(q)
	// Node-RED reads these loosely: true or "true", and a retain handling
	// outside 0 to 2 is 0.
	n.subOpts.NoLocal = def.Node.PropBool("nl", false)
	n.subOpts.RetainAsPublished = def.Node.PropBool("rap", false)
	if rh := int(mqttNumber(def.Node.Raw["rh"])); rh >= 0 && rh <= 2 {
		n.subOpts.RetainHandling = byte(rh)
	}
	return n, nil
}

func (n *mqttInNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *mqttInNode) Start(ctx context.Context, out node.Emitter) error {
	cfg, ok := n.svc.ConfigNode(n.cfgID)
	if !ok {
		return fmt.Errorf("server config node %s is not running", n.cfgID)
	}
	b, ok := cfg.(MQTTBroker)
	if !ok {
		return fmt.Errorf("config node %s is not an MQTT broker", n.cfgID)
	}
	n.broker = b

	out.Status(node.Status{Fill: "yellow", Shape: "ring", Text: "connecting"})

	handler := func(pm mqttMessage) {
		m, err := mqttInMessage(pm, n.output)
		if err != nil {
			out.Error(err, m)
			return
		}
		out.Status(node.Status{Fill: "green", Shape: "dot", Text: "connected"})
		out.Send(0, m)
	}

	if err := n.broker.Subscribe(n.id, n.topic, n.qos, n.subOpts, handler); err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "subscribe failed"})
		return err
	}
	n.started = true
	return nil
}

func (n *mqttInNode) Close(context.Context, bool) error {
	if n.broker != nil && n.started {
		return n.broker.Unsubscribe(n.id, n.topic)
	}
	return nil
}

// mqttInMessage turns a received publish into a message: the topic, the
// decoded payload, QoS and retain, and on version 5 the properties under the
// names Node-RED gives them.
func mqttInMessage(pm mqttMessage, output string) (*engine.Msg, error) {
	m := engine.NewMsg()
	m.SetTopic(pm.Topic)
	m.Data["qos"] = float64(pm.QoS)
	m.Data["retain"] = pm.Retain
	var p *mqttProps
	if pm.Props != nil {
		p = pm.Props
		if p.ResponseTopic != "" {
			m.Data["responseTopic"] = p.ResponseTopic
		}
		if p.CorrelationData != nil {
			m.Data["correlationData"] = append([]byte(nil), p.CorrelationData...)
		}
		if p.ContentType != "" {
			m.Data["contentType"] = p.ContentType
		}
		if p.MessageExpiry != nil {
			m.Data["messageExpiryInterval"] = float64(*p.MessageExpiry)
		}
		if p.PayloadFormat != nil {
			m.Data["payloadFormatIndicator"] = *p.PayloadFormat
		}
		if p.ReasonString != "" {
			m.Data["reasonString"] = p.ReasonString
		}
		if len(p.User) > 0 {
			up := map[string]any{}
			for _, kv := range p.User {
				up[kv[0]] = kv[1]
			}
			m.Data["userProperties"] = up
		}
	}
	payload, err := decodeMQTTPayload(pm.Payload, output, p)
	if err != nil {
		m.SetPayload(string(pm.Payload))
		return m, err
	}
	m.SetPayload(payload)
	return m, nil
}

// knownMediaTypes is Node-RED's table of what a version 5 content type means
// for auto-detect decoding.
var knownMediaTypes = map[string]string{
	"text/css": "string", "text/html": "string", "text/plain": "string",
	"application/json": "json", "application/xml": "string",
	"application/octet-stream": "buffer", "application/pdf": "buffer",
	"application/x-gtar": "buffer", "application/x-gzip": "buffer",
	"application/x-tar": "buffer", "application/zip": "buffer",
	"audio/aac": "buffer", "audio/ac3": "buffer", "audio/basic": "buffer",
	"audio/mp4": "buffer", "audio/ogg": "buffer",
	"image/bmp": "buffer", "image/gif": "buffer", "image/jpeg": "buffer",
	"image/tiff": "buffer", "image/png": "buffer",
}

// decodeMQTTPayload turns bytes into what the flow asked for.
//
// "auto" is this build's original mode: a JSON object or array is parsed, and
// anything else is a string, or a buffer when it is not valid UTF-8, because
// silently producing a string full of replacement characters destroys binary
// data. "auto-detect" is Node-RED's: any JSON value is parsed, and on version 5
// a content type, or a payload format saying the payload is text, settles it
// instead of guessing. A message whose content type says JSON and is not is an
// error, as it is in Node-RED.
func decodeMQTTPayload(raw []byte, mode string, props *mqttProps) (any, error) {
	switch mode {
	case "buffer":
		return raw, nil
	case "base64":
		return base64.StdEncoding.EncodeToString(raw), nil
	case "utf8":
		return string(raw), nil
	case "json":
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			// Returning the raw string rather than an error keeps one
			// malformed publish from stopping the subscription.
			return string(raw), nil
		}
		return v, nil
	case "auto-detect":
		if props != nil && ((props.PayloadFormat != nil && *props.PayloadFormat) || props.ContentType != "") {
			switch knownMediaTypes[strings.ToLower(props.ContentType)] {
			case "string":
				return string(raw), nil
			case "buffer":
				return raw, nil
			case "json":
				var v any
				if err := json.Unmarshal(raw, &v); err != nil {
					return nil, fmt.Errorf("the content type says JSON and the payload is not: %w", err)
				}
				return v, nil
			}
			utf8OK := (props.PayloadFormat != nil && *props.PayloadFormat) || utf8.Valid(raw)
			if !utf8OK {
				return raw, nil
			}
			var v any
			if err := json.Unmarshal(raw, &v); err == nil {
				return v, nil
			}
			return string(raw), nil
		}
		if !utf8.Valid(raw) {
			return raw, nil
		}
		var v any
		if err := json.Unmarshal(raw, &v); err == nil {
			return v, nil
		}
		return string(raw), nil
	default: // auto
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var v any
			if err := json.Unmarshal(raw, &v); err == nil {
				return v, nil
			}
		}
		if !utf8.Valid(raw) {
			return raw, nil
		}
		return string(raw), nil
	}
}

// ---------------------------------------------------------------------------
// mqtt out
// ---------------------------------------------------------------------------

type mqttOutNode struct {
	cfgID  string
	topic  string
	qos    byte
	retain bool
	svc    node.Services
	broker MQTTBroker

	// Version 5 properties set on the node, which win over the message's,
	// as they do in Node-RED.
	contentType   string
	responseTopic string
	correlation   []byte
	expiry        *uint32
	userProps     [][2]string
}

func registerMQTTOut() {
	node.MustRegister(node.Descriptor{
		Type:         "mqtt out",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "mqtt",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "mqtt out",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Publishing with topic, QoS and retain from the node or the message, and on " +
				"MQTT 5 the content type, response topic, correlation data, message expiry and " +
				"user properties, from the node or from msg.contentType, msg.responseTopic, " +
				"msg.correlationData, msg.messageExpiryInterval and msg.userProperties, with " +
				"msg.responseTopic as the topic when there is no other. Topic aliases are not " +
				"used: the full topic always goes. The connect and disconnect control messages " +
				"are not implemented.",
			UnsupportedProps: []string{"topicAlias"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "broker", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "mqtt-broker", Required: true},
			{Name: "topic", Kind: node.PropString, Label: "Topic",
				Help: "Leave empty to use msg.topic."},
			{Name: "qos", Kind: node.PropSelect, Label: "QoS", Default: "0", Options: mqttQoSOptions()},
			{Name: "retain", Kind: node.PropBool, Label: "Retain"},
			{Name: "contentType", Kind: node.PropString, Label: "MQTT 5: content type"},
			{Name: "respTopic", Kind: node.PropString, Label: "MQTT 5: response topic"},
			{Name: "correl", Kind: node.PropString, Label: "MQTT 5: correlation data"},
			{Name: "expiry", Kind: node.PropNumber, Label: "MQTT 5: message expiry (seconds)"},
			{Name: "userProps", Kind: node.PropJSON, Label: "MQTT 5: user properties"},
		},
		Help: "Publishes msg.payload to an MQTT topic.",
	}, newMQTTOut)
}

func newMQTTOut(def *node.Definition) (node.Node, error) {
	n := &mqttOutNode{
		cfgID:         def.Node.PropString("broker", ""),
		topic:         def.Node.PropString("topic", ""),
		retain:        def.Node.PropBool("retain", false),
		svc:           def.Services,
		contentType:   def.Node.PropString("contentType", ""),
		responseTopic: def.Node.PropString("respTopic", ""),
		userProps:     mqttUserPropsFromJSON(def.Node.Raw["userProps"]),
	}
	if c := def.Node.PropString("correl", ""); c != "" {
		n.correlation = []byte(c)
	}
	if e := mqttNumber(def.Node.Raw["expiry"]); e > 0 {
		exp := uint32(e)
		n.expiry = &exp
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected")
	}
	q := def.Node.PropInt("qos", 0)
	if q < 0 || q > 2 {
		return nil, fmt.Errorf("qos must be 0, 1 or 2, got %d", q)
	}
	n.qos = byte(q)
	return n, nil
}

func (n *mqttOutNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	if n.broker == nil {
		cfg, ok := n.svc.ConfigNode(n.cfgID)
		if !ok {
			return fmt.Errorf("server config node %s is not running", n.cfgID)
		}
		b, ok := cfg.(MQTTBroker)
		if !ok {
			return fmt.Errorf("config node %s is not an MQTT broker", n.cfgID)
		}
		n.broker = b
	}

	pm := mqttMessage{Topic: n.topic, QoS: n.qos, Retain: n.retain}
	if pm.Topic == "" {
		pm.Topic = m.Topic()
	}
	if v, ok, _ := m.Get("qos"); ok {
		if f, isNum := asFloat(v); isNum && f >= 0 && f <= 2 {
			pm.QoS = byte(f)
		}
	}
	if v, ok, _ := m.Get("retain"); ok {
		if b, isBool := v.(bool); isBool {
			pm.Retain = b
		}
	}
	if n.broker.V5() {
		pm.Props = n.publishProps(m)
		if pm.Topic == "" && pm.Props.ResponseTopic != "" {
			// Node-RED replies to a request on its response topic when the
			// message names no other.
			pm.Topic = pm.Props.ResponseTopic
		}
	}
	if pm.Topic == "" {
		return fmt.Errorf("no topic: set one on the node or on msg.topic")
	}
	if strings.ContainsAny(pm.Topic, "+#") {
		return fmt.Errorf("%q is a subscription filter, not a topic that can be published to", pm.Topic)
	}

	payload, err := encodeMQTTPayload(m.Payload())
	if err != nil {
		return err
	}
	pm.Payload = payload

	if !n.broker.Connected() {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "not connected"})
		return fmt.Errorf("broker is not connected")
	}
	if err := n.broker.Publish(ctx, pm); err != nil {
		return fmt.Errorf("publishing to %q: %w", pm.Topic, err)
	}
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "published"})
	return nil
}

// publishProps works out the version 5 properties for a publish: the
// message's own, with any set on the node laid over them.
func (n *mqttOutNode) publishProps(m *engine.Msg) *mqttProps {
	p := &mqttProps{}
	if s, ok := m.Data["contentType"].(string); ok {
		p.ContentType = s
	}
	if s, ok := m.Data["responseTopic"].(string); ok {
		p.ResponseTopic = s
	}
	switch c := m.Data["correlationData"].(type) {
	case []byte:
		p.CorrelationData = append([]byte(nil), c...)
	case engine.ImmutableBytes:
		p.CorrelationData = append([]byte(nil), c...)
	case string:
		p.CorrelationData = []byte(c)
	}
	if e, ok := asFloat(m.Data["messageExpiryInterval"]); ok && e >= 0 {
		exp := uint32(e)
		p.MessageExpiry = &exp
	}
	if b, ok := m.Data["payloadFormatIndicator"].(bool); ok {
		p.PayloadFormat = &b
	}
	if up, ok := m.Data["userProperties"].(map[string]any); ok {
		p.User = mqttUserPropsFromObject(up)
	}
	if n.contentType != "" {
		p.ContentType = n.contentType
	}
	if n.responseTopic != "" {
		p.ResponseTopic = n.responseTopic
	}
	if n.correlation != nil {
		p.CorrelationData = n.correlation
	}
	if n.expiry != nil {
		p.MessageExpiry = n.expiry
	}
	if len(n.userProps) > 0 {
		p.User = n.userProps
	}
	return p
}

// encodeMQTTPayload renders a payload for the wire. Objects and arrays go as
// JSON, which is what every consumer on an industrial bus expects.
func encodeMQTTPayload(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return t, nil
	case engine.ImmutableBytes:
		return t, nil
	case string:
		return []byte(t), nil
	case bool:
		if t {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	default:
		if f, ok := asFloat(v); ok {
			// -1 precision so 21 publishes as "21" rather than "21.000000".
			return []byte(strconv.FormatFloat(f, 'f', -1, 64)), nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encoding payload: %w", err)
		}
		return b, nil
	}
}

// topicMatches reports whether a topic matches a subscription filter,
// including + and # and a shared subscription's $share/<group>/ prefix.
func topicMatches(filter, topic string) bool {
	if strings.HasPrefix(filter, "$share/") {
		parts := strings.SplitN(filter, "/", 3)
		if len(parts) < 3 {
			return false
		}
		filter = parts[2]
	}
	// A filter starting with a wildcard does not match a $SYS style topic.
	if strings.HasPrefix(topic, "$") && (strings.HasPrefix(filter, "+") || strings.HasPrefix(filter, "#")) {
		return false
	}
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")
	for i, seg := range f {
		if seg == "#" {
			return true
		}
		if i >= len(t) {
			return false
		}
		if seg != "+" && seg != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}

// mqttNumber reads a number out of config the way Number(x || 0) does: a
// number, a numeric string, or 0.
func mqttNumber(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return 0
}
