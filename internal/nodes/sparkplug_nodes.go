package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// ---------------------------------------------------------------------------
// Shared
// ---------------------------------------------------------------------------

func spTypeOptions() []node.Option {
	var opts []node.Option
	for _, n := range []string{"Boolean", "Int8", "Int16", "Int32", "Int64", "UInt8", "UInt16", "UInt32",
		"UInt64", "Float", "Double", "String", "Text", "UUID", "DateTime", "Bytes", "DataSet", "Template",
		"BooleanArray", "Int8Array", "Int16Array", "Int32Array", "Int64Array", "UInt8Array", "UInt16Array",
		"UInt32Array", "UInt64Array", "FloatArray", "DoubleArray", "StringArray", "DateTimeArray"} {
		opts = append(opts, node.Option{Value: n, Label: n})
	}
	return opts
}

func spServerOf(svc node.Services, id string) (*spServer, error) {
	cfg, ok := svc.ConfigNode(id)
	if !ok {
		return nil, fmt.Errorf("the Sparkplug config %s is not running. Open this node and pick a connection", id)
	}
	s, ok := cfg.(*spServer)
	if !ok {
		return nil, fmt.Errorf("config node %s is not a Sparkplug connection", id)
	}
	return s, nil
}

func spNoReserved(label, v string) error {
	if strings.ContainsAny(v, "+/#") {
		return fmt.Errorf("%s %q can't contain +, / or #, which Sparkplug reserves for topics", label, v)
	}
	return nil
}

// ---------------------------------------------------------------------------
// sparkplug out: an edge node's metrics, or a device's
// ---------------------------------------------------------------------------

type spDecl struct {
	name    string
	dt      uint32
	initial any
	alias   uint64
	props   map[string]any
}

type spUpdate struct {
	decl      *spDecl
	value     any
	timestamp uint64
	props     map[string]any
}

type spOutNode struct {
	cfgID  string
	device string
	decls  []*spDecl
	byName map[string]*spDecl
	svc    node.Services
	srv    *spServer
	out    node.Emitter

	// Guarded by the server's lock, because the birth that reads them runs
	// under it.
	values map[string]any
	props  map[string]map[string]any
	alive  bool
}

func registerSparkplugOut() {
	node.MustRegister(node.Descriptor{
		Type:         "sparkplug out",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "mqtt",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "sparkplug out",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Sparkplug B publisher, for an edge node or one of its devices.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Edge node", ConfigType: "hotloop-flow-sparkplug",
				Required: true, Help: "A Sparkplug connection that is an edge node."},
			{Name: "device", Kind: node.PropString, Label: "Device ID",
				Help: "Empty publishes the edge node's own metrics, as NBIRTH and NDATA. A device ID publishes " +
					"a device's, as DBIRTH and DDATA. One node for each."},
			{Name: "metrics", Kind: node.PropList, Label: "Metrics", Fields: []node.Prop{
				{Name: "name", Kind: node.PropString, Label: "Name", Placeholder: "Boiler/Temperature"},
				{Name: "dataType", Kind: node.PropSelect, Label: "Type", Default: "Double", Options: spTypeOptions()},
				{Name: "initial", Kind: node.PropString, Label: "Value at birth",
					Help: "Empty is null until the first message. For an array, a DataSet or a Template, JSON: " +
						"[1, 2, 3], or {\"columns\": [\"Name\"], \"types\": [\"String\"], \"rows\": [[\"a\"]]}, or " +
						"{\"isDefinition\": true, \"metrics\": [{\"name\": \"Speed\", \"type\": \"Double\"}]}."},
				{Name: "properties", Kind: node.PropString, Label: "Properties",
					Help: "Optional. A PropertySet for the metric, as JSON: {\"engUnit\": \"degC\", \"Quality\": " +
						"{\"type\": \"Int32\", \"value\": 192}}. A value with no type is a boolean, a string, an " +
						"Int32 or Int64 for a whole number or a Double. Spell out {\"type\", \"value\"} for anything else."},
			}, Help: "A birth has to list every metric before it is ever sent, so they are declared here."},
		},
		Help: "Publishes metrics to a Sparkplug host as an edge node, or as one of its devices. msg.payload is " +
			"an object of metric names and values, or a list of {name, value, timestamp}. Only the metrics in the " +
			"message are sent, as data. The node is born when the connection is, and is born again when a host " +
			"asks. A device says it has gone with msg.sparkplug.death set to true, and is born again by its " +
			"next message.\n\nWhat gets published is what the message said, no more. A metric the node " +
			"doesn't declare is an error, because a host can't be told about a metric that wasn't in the birth.",
	}, newSparkplugOut)
}

func newSparkplugOut(def *node.Definition) (node.Node, error) {
	p := def.Node
	o := &spOutNode{
		cfgID:  p.PropString("server", ""),
		device: strings.TrimSpace(p.PropString("device", "")),
		svc:    def.Services,
		byName: map[string]*spDecl{},
		values: map[string]any{},
		props:  map[string]map[string]any{},
	}
	if o.cfgID == "" {
		return nil, fmt.Errorf("no edge node selected: pick the Sparkplug connection to publish through")
	}
	if err := spNoReserved("device ID", o.device); err != nil {
		return nil, err
	}
	rows, _ := p.Raw["metrics"].([]any)
	for i, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(stringOr(m["name"], ""))
		if name == "" {
			continue
		}
		if name == spBdSeqName || name == spRebirthName {
			return nil, fmt.Errorf("metric %d: %q belongs to Sparkplug itself and is added to every birth. Choose another name", i+1, name)
		}
		tn := stringOr(m["dataType"], "Double")
		dt, ok := spTypeNames[tn]
		if !ok {
			return nil, fmt.Errorf("metric %q: type %q is not one of the types this node can publish", name, tn)
		}
		if _, dup := o.byName[name]; dup {
			return nil, fmt.Errorf("metric %q is declared twice", name)
		}
		d := &spDecl{name: name, dt: dt}
		if raw := strings.TrimSpace(stringOr(m["initial"], "")); raw != "" {
			v, err := spParseInitial(dt, raw)
			if err != nil {
				return nil, fmt.Errorf("metric %q: the value at birth %q: %w", name, raw, err)
			}
			d.initial = v
			o.values[name] = v
		}
		if raw := strings.TrimSpace(stringOr(m["properties"], "")); raw != "" {
			var props map[string]any
			if err := json.Unmarshal([]byte(raw), &props); err != nil {
				return nil, fmt.Errorf("metric %q: the properties %q aren't a JSON object: %v", name, raw, err)
			}
			if _, err := spPropsBytes(props); err != nil {
				return nil, fmt.Errorf("metric %q: properties: %w", name, err)
			}
			d.props = props
			o.props[name] = props
		}
		o.decls = append(o.decls, d)
		o.byName[name] = d
	}
	if len(o.decls) == 0 {
		return nil, fmt.Errorf("declare at least one metric. A birth lists every metric the node will ever publish")
	}
	return o, nil
}

// spParseInitial reads the text in the value at birth field as its type.
func spParseInitial(dt uint32, s string) (any, error) {
	switch dt {
	case spBoolean:
		return modbusBool(s)
	case spString, spText, spUUID:
		return s, nil
	case spBytes:
		return []byte(s), nil
	}
	if _, isArray := spArrayElem[dt]; isArray || dt == spDataSet || dt == spTemplate {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("isn't JSON: %v", err)
		}
		if _, err := spValueToRaw(dt, v); err != nil {
			return nil, err
		}
		return v, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("isn't a number")
	}
	if _, err := spValueToRaw(dt, f); err != nil {
		return nil, err
	}
	return f, nil
}

// birthMetrics are the metrics of a birth with the values they hold now. The
// caller holds the server's lock.
func (o *spOutNode) birthMetrics(now uint64, aliases bool) ([]*spMetric, error) {
	var out []*spMetric
	for _, d := range o.decls {
		m, err := spMetricFor(d.name, d.dt, o.values[d.name], true, now)
		if err != nil {
			return nil, fmt.Errorf("metric %q: %w", d.name, err)
		}
		if err := m.setProps(o.props[d.name]); err != nil {
			return nil, fmt.Errorf("metric %q: properties: %w", d.name, err)
		}
		if aliases && d.alias != 0 {
			m.Alias, m.HasAlias = d.alias, true
		}
		out = append(out, m)
	}
	return out, nil
}

func (o *spOutNode) statusSink(s node.Status) {
	if o.out != nil {
		o.out.Status(s)
	}
}

func (o *spOutNode) deliver(*spEvent) {}

func (o *spOutNode) Start(_ context.Context, out node.Emitter) error {
	o.out = out
	if node.StandInOf(o.svc) != nil {
		return nil
	}
	srv, err := spServerOf(o.svc, o.cfgID)
	if err != nil {
		return err
	}
	o.srv = srv
	if err := srv.registerOut(o); err != nil {
		return err
	}
	srv.acquire(o)
	return nil
}

func (o *spOutNode) Close(_ context.Context, removed bool) error {
	if o.srv != nil {
		o.srv.unregisterOut(o, removed)
		o.srv.release(o)
	}
	return nil
}

func (o *spOutNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (o *spOutNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	death := false
	if sp, ok := m.Data["sparkplug"].(map[string]any); ok {
		death, _ = sp["death"].(bool)
	}
	si := node.StandInOf(o.svc)
	if death {
		if o.device == "" {
			return o.fail(out, fmt.Errorf("an edge node's own metrics can't die by themselves. A death is for a device. Disconnect the edge node to end it"))
		}
		if si != nil {
			if _, err := si.Call("sparkplug", map[string]any{"op": "ddeath", "device": o.device}); err != nil {
				return o.fail(out, err)
			}
		} else if o.srv == nil {
			return o.fail(out, fmt.Errorf("this node isn't running"))
		} else if err := o.srv.publishDeviceDeath(o); err != nil {
			return o.fail(out, err)
		}
		out.Status(node.Status{Fill: "yellow", Shape: "dot", Text: "device died"})
		out.Send(0, m)
		return nil
	}

	updates, err := o.parse(m.Payload())
	if err != nil {
		return o.fail(out, err)
	}
	if si != nil {
		sent := make([]any, len(updates))
		for i, u := range updates {
			sent[i] = map[string]any{"name": u.decl.name, "type": spTypeName(u.decl.dt), "value": u.value}
		}
		// What would have been published, in the form it takes on the wire,
		// after the value has been checked against its type.
		for _, u := range updates {
			if _, err := spMetricFor(u.decl.name, u.decl.dt, u.value, false, 0); err != nil {
				return o.fail(out, fmt.Errorf("metric %q: %w", u.decl.name, err))
			}
		}
		if _, err := si.Call("sparkplug", map[string]any{"op": "data", "device": o.device, "metrics": sent}); err != nil {
			return o.fail(out, err)
		}
	} else {
		if o.srv == nil {
			return o.fail(out, fmt.Errorf("this node isn't running"))
		}
		if err := o.srv.publishData(o, updates, o.device != ""); err != nil {
			return o.fail(out, err)
		}
	}
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(fmt.Sprintf("sent %d", len(updates)), 32)})
	out.Send(0, m)
	return nil
}

// parse reads a message's payload as metric updates.
func (o *spOutNode) parse(payload any) ([]spUpdate, error) {
	var ups []spUpdate
	add := func(name string, v any, ts uint64) error {
		d, ok := o.byName[name]
		if !ok {
			names := make([]string, 0, len(o.decls))
			for _, d := range o.decls {
				names = append(names, d.name)
			}
			return fmt.Errorf("metric %q isn't declared on this node, which declares %s. A birth has to list every metric "+
				"before it is sent, so add it to the node's metrics", name, strings.Join(names, ", "))
		}
		if v != nil {
			if _, err := spValueToRaw(d.dt, v); err != nil {
				return fmt.Errorf("metric %q: %w", name, err)
			}
		}
		ups = append(ups, spUpdate{decl: d, value: v, timestamp: ts})
		return nil
	}
	switch t := payload.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := add(k, t[k], 0); err != nil {
				return nil, err
			}
		}
	case []any:
		for i, it := range t {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("item %d of the list is %s, and has to be an object like {name, value}", i, describeType(it))
			}
			name, _ := m["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("item %d of the list has no name", i)
			}
			var ts uint64
			if f, ok := asFloat(m["timestamp"]); ok && m["timestamp"] != nil {
				if f < 0 || f != math.Trunc(f) {
					return nil, fmt.Errorf("item %d: the timestamp has to be milliseconds since 1970", i)
				}
				ts = uint64(f)
			}
			v := m["value"]
			if n, _ := m["isNull"].(bool); n {
				v = nil
			}
			if err := add(name, v, ts); err != nil {
				return nil, err
			}
			if pr, ok := m["properties"]; ok && pr != nil {
				pm, isMap := pr.(map[string]any)
				if !isMap {
					return nil, fmt.Errorf("item %d: the properties are %s, and have to be an object", i, describeType(pr))
				}
				if _, err := spPropsBytes(pm); err != nil {
					return nil, fmt.Errorf("metric %q: properties: %w", name, err)
				}
				ups[len(ups)-1].props = pm
			}
		}
	default:
		return nil, fmt.Errorf("msg.payload is %s, and has to be an object of metric names and values, like {\"Temperature\": 21.5}, or a list of {name, value}", describeType(payload))
	}
	if len(ups) == 0 {
		return nil, fmt.Errorf("msg.payload names no metrics, so there is nothing to publish")
	}
	return ups, nil
}

// ---------------------------------------------------------------------------
// sparkplug in: what a host sees, or what an edge node is told
// ---------------------------------------------------------------------------

type spInNode struct {
	cfgID                      string
	group, edge, device        string
	births, deaths, data, cmds bool
	state                      bool
	svc                        node.Services
	srv                        *spServer
	out                        node.Emitter
}

func registerSparkplugIn() {
	node.MustRegister(node.Descriptor{
		Type:         "sparkplug in",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "mqtt",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "sparkplug in",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Sparkplug B listener. On a host connection it follows every edge node " +
				"it can see. On an edge connection it hands over the commands sent to it.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Connection", ConfigType: "hotloop-flow-sparkplug",
				Required: true},
			{Name: "group", Kind: node.PropString, Label: "Group ID", Help: "Empty is every group. Host only."},
			{Name: "edgeNode", Kind: node.PropString, Label: "Edge node ID", Help: "Empty is every edge node. Host only."},
			{Name: "device", Kind: node.PropString, Label: "Device ID",
				Help: "Empty is every device and the edge node's own metrics. On an edge node, empty is every command."},
			{Name: "births", Kind: node.PropBool, Label: "Births", Default: true, Help: "NBIRTH and DBIRTH. Host only."},
			{Name: "deaths", Kind: node.PropBool, Label: "Deaths", Default: true, Help: "NDEATH and DDEATH. Host only."},
			{Name: "data", Kind: node.PropBool, Label: "Data", Default: true, Help: "NDATA and DDATA. Host only."},
			{Name: "commands", Kind: node.PropBool, Label: "Commands to other edge nodes", Help: "NCMD and DCMD seen on the namespace. Host only."},
			{Name: "state", Kind: node.PropBool, Label: "Host STATE", Help: "The online and offline messages of host applications. Host only."},
		},
		Help: "On a host application connection, sends a message for each Sparkplug message it receives: " +
			"msg.topic is the MQTT topic, msg.payload an object of metric names and values, and " +
			"msg.sparkplug the group, edge node, device, message type, sequence number, timestamp, and " +
			"every metric with its datatype. A death carries the list of metrics that went stale. A " +
			"message that arrives out of sequence is marked, and a gap that isn't filled makes the host " +
			"ask for a rebirth.\n\nOn an edge node connection, sends the commands a host sends this node, " +
			"which is how a flow acts on a write. A rebirth request is answered by the node itself and " +
			"isn't passed on.\n\nNull values arrive as null. A DateTime is milliseconds since 1970, and a " +
			"64 bit integer past 2^53 is a string of its exact digits. DataSet, Template and PropertySet " +
			"values aren't carried: the metric arrives with its name and datatype, marked unsupported.",
	}, newSparkplugIn)
}

func newSparkplugIn(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &spInNode{
		cfgID:  p.PropString("server", ""),
		group:  strings.TrimSpace(p.PropString("group", "")),
		edge:   strings.TrimSpace(p.PropString("edgeNode", "")),
		device: strings.TrimSpace(p.PropString("device", "")),
		births: p.PropBool("births", true),
		deaths: p.PropBool("deaths", true),
		data:   p.PropBool("data", true),
		cmds:   p.PropBool("commands", false),
		state:  p.PropBool("state", false),
		svc:    def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no connection selected: pick the Sparkplug connection to listen on")
	}
	for l, v := range map[string]string{"group ID": n.group, "edge node ID": n.edge, "device ID": n.device} {
		if err := spNoReserved(l, v); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *spInNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *spInNode) statusSink(s node.Status) {
	if n.out != nil {
		n.out.Status(s)
	}
}

func (n *spInNode) Start(_ context.Context, out node.Emitter) error {
	n.out = out
	if node.StandInOf(n.svc) != nil {
		// A flow test listens to nothing. What the broker would have
		// delivered is whatever the test injects here.
		return nil
	}
	srv, err := spServerOf(n.svc, n.cfgID)
	if err != nil {
		return err
	}
	n.srv = srv
	out.Status(node.Status{Fill: "yellow", Shape: "ring", Text: "connecting"})
	srv.acquire(n)
	return nil
}

func (n *spInNode) Close(context.Context, bool) error {
	if n.srv != nil {
		n.srv.release(n)
	}
	return nil
}

func (n *spInNode) wants(ev *spEvent) bool {
	if n.srv == nil {
		return false
	}
	if n.srv.role == spRoleEdge {
		// An edge node's In node is its command line.
		return (ev.Type == "NCMD" || ev.Type == "DCMD") && (n.device == "" || n.device == ev.Device)
	}
	switch ev.Type {
	case "NBIRTH", "DBIRTH":
		if !n.births {
			return false
		}
	case "NDEATH", "DDEATH":
		if !n.deaths {
			return false
		}
	case "NDATA", "DDATA":
		if !n.data {
			return false
		}
	case "NCMD", "DCMD":
		if !n.cmds {
			return false
		}
	case "STATE":
		return n.state
	}
	if n.group != "" && n.group != ev.Group {
		return false
	}
	if n.edge != "" && n.edge != ev.Edge {
		return false
	}
	if n.device != "" && n.device != ev.Device {
		return false
	}
	return true
}

func (n *spInNode) deliver(ev *spEvent) {
	if !n.wants(ev) {
		return
	}
	m := engine.NewMsg()
	m.SetTopic(ev.Topic)
	m.SetPayload(spEventMsg(m, ev))
	n.out.Send(0, m)
}

// spEventMsg fills msg.sparkplug for an event and returns the payload.
func spEventMsg(m *engine.Msg, ev *spEvent) any {
	if ev.Type == "STATE" {
		m.Data["sparkplug"] = map[string]any{"type": "STATE", "host": ev.Edge}
		return ev.State
	}
	payload := map[string]any{}
	metrics := make([]any, 0, len(ev.Metrics))
	for _, mt := range ev.Metrics {
		if mt.Name != "" {
			payload[mt.Name] = mt.Value
		}
		e := map[string]any{"name": mt.Name, "value": mt.Value}
		if mt.HasAlias {
			e["alias"] = float64(mt.Alias)
		}
		if mt.HasDataType {
			e["datatype"] = spTypeName(mt.DataType)
		}
		if mt.HasTimestamp {
			e["timestamp"] = float64(mt.Timestamp)
		}
		if mt.IsNull {
			e["isNull"] = true
		}
		if mt.IsHistorical {
			e["isHistorical"] = true
		}
		if mt.IsTransient {
			e["isTransient"] = true
		}
		if mt.Properties != nil {
			e["properties"] = mt.Properties
		}
		if mt.Unsupported != "" {
			e["unsupported"] = mt.Unsupported
		}
		metrics = append(metrics, e)
	}
	sp := map[string]any{
		"group":    ev.Group,
		"edgeNode": ev.Edge,
		"type":     ev.Type,
		"metrics":  metrics,
	}
	if ev.Device != "" {
		sp["device"] = ev.Device
	}
	if ev.Seq >= 0 {
		sp["seq"] = float64(ev.Seq)
	}
	if ev.Payload != nil && ev.Payload.HasTimestamp {
		sp["timestamp"] = float64(ev.Payload.Timestamp)
	}
	if ev.BdSeq != nil {
		sp["bdSeq"] = *ev.BdSeq
	}
	if ev.Out {
		sp["outOfOrder"] = true
	}
	if len(ev.Stale) > 0 {
		stale := make([]any, len(ev.Stale))
		for i, s := range ev.Stale {
			stale[i] = s
		}
		sp["stale"] = stale
	}
	if len(ev.Unsolved) > 0 {
		u := make([]any, len(ev.Unsolved))
		for i, s := range ev.Unsolved {
			u[i] = s
		}
		sp["unresolved"] = u
	}
	m.Data["sparkplug"] = sp
	return payload
}

// ---------------------------------------------------------------------------
// sparkplug command: a host writing to an edge node or a device
// ---------------------------------------------------------------------------

type spCommandNode struct {
	cfgID               string
	group, edge, device TypedValue
	svc                 node.Services
	srv                 *spServer
}

func registerSparkplugCommand() {
	node.MustRegister(node.Descriptor{
		Type:         "sparkplug command",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "mqtt",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "sparkplug command",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Sparkplug B command sender, as a host application.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Host application", ConfigType: "hotloop-flow-sparkplug",
				Required: true, Help: "A Sparkplug connection that is a host application."},
			{Name: "group", Kind: node.PropTypedInput, Label: "Group ID", Required: true, TypeProp: "groupType",
				TypedInputTypes: spAddrTypes()},
			{Name: "edgeNode", Kind: node.PropTypedInput, Label: "Edge node ID", Required: true, TypeProp: "edgeNodeType",
				TypedInputTypes: spAddrTypes()},
			{Name: "device", Kind: node.PropTypedInput, Label: "Device ID", TypeProp: "deviceType",
				TypedInputTypes: spAddrTypes(), Help: "Empty sends an NCMD to the edge node. A device ID sends a DCMD to it."},
		},
		Help: "Sends a command to an edge node, or to one of its devices, when a message arrives, then passes " +
			"the message on. msg.payload is an object of metric names and values, or a list of " +
			"{name, value, type}.\n\nA metric is sent as the type the host saw in the birth. For one the host " +
			"hasn't seen born, give the type in the list form. This changes equipment. There is no " +
			"confirmation step: the edge node's next data message is the answer.\n\n" +
			"{\"Node Control/Rebirth\": true} asks an edge node to be born again.",
	}, newSparkplugCommand)
}

func spAddrTypes() []string {
	return []string{node.TypeStr, node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeEnv, node.TypeJSONata}
}

func newSparkplugCommand(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &spCommandNode{
		cfgID:  p.PropString("server", ""),
		group:  ReadTypedValue(p.Raw, "group", "groupType", node.TypeStr),
		edge:   ReadTypedValue(p.Raw, "edgeNode", "edgeNodeType", node.TypeStr),
		device: ReadTypedValue(p.Raw, "device", "deviceType", node.TypeStr),
		svc:    def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no host application selected: pick the Sparkplug connection to send from")
	}
	for l, tv := range map[string]TypedValue{"group ID": n.group, "edge node ID": n.edge} {
		if strings.TrimSpace(tv.Value) == "" {
			return nil, fmt.Errorf("%s is required", l)
		}
	}
	for _, tv := range []TypedValue{n.group, n.edge, n.device} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
		if tv.Type == node.TypeStr {
			if err := spNoReserved("ID", tv.Value); err != nil {
				return nil, err
			}
		}
	}
	return n, nil
}

func (n *spCommandNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *spCommandNode) id(tv TypedValue, ec EvalContext, label string, required bool) (string, error) {
	v, ok, err := tv.Eval(ec)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	s := ""
	if ok && v != nil {
		s = strings.TrimSpace(fmt.Sprint(v))
	}
	if s == "" && required {
		return "", fmt.Errorf("%s resolved to nothing", label)
	}
	return s, spNoReserved(label, s)
}

func (n *spCommandNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	group, err := n.id(n.group, ec, "group ID", true)
	if err != nil {
		return n.fail(out, err)
	}
	edge, err := n.id(n.edge, ec, "edge node ID", true)
	if err != nil {
		return n.fail(out, err)
	}
	device, err := n.id(n.device, ec, "device ID", false)
	if err != nil {
		return n.fail(out, err)
	}

	si := node.StandInOf(n.svc)
	var srv *spServer
	if si == nil {
		srv, err = spServerOf(n.svc, n.cfgID)
		if err != nil {
			return n.fail(out, err)
		}
		n.srv = srv
	}
	metrics, err := n.parse(m.Payload(), func(name string) (uint32, bool) {
		if srv == nil {
			return 0, false
		}
		return srv.knownType(group, edge, device, name)
	}, func(name string) (uint64, bool) {
		if srv == nil {
			return 0, false
		}
		return srv.knownAlias(group, edge, device, name)
	}, si != nil)
	if err != nil {
		return n.fail(out, err)
	}

	if si != nil {
		sent := make([]any, len(metrics))
		for i, mt := range metrics {
			sent[i] = map[string]any{"name": mt.Name, "type": spTypeName(mt.DataType), "value": mt.Value}
		}
		if _, err := si.Call("sparkplug", map[string]any{"op": "command", "group": group, "edgeNode": edge,
			"device": device, "metrics": sent}); err != nil {
			return n.fail(out, err)
		}
	} else if err := srv.publishCommand(group, edge, device, metrics); err != nil {
		return n.fail(out, err)
	}
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(fmt.Sprintf("sent %d", len(metrics)), 32)})
	out.Send(0, m)
	return nil
}

// parse reads the payload as metrics with datatypes: the type given, else the
// one the host learned from the birth, else, for a flow test only, one guessed
// from the value.
func (n *spCommandNode) parse(payload any, known func(string) (uint32, bool), aliasOf func(string) (uint64, bool), guess bool) ([]*spMetric, error) {
	type item struct {
		name  string
		value any
		typ   string
	}
	var items []item
	switch t := payload.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, item{name: k, value: t[k]})
		}
	case []any:
		for i, it := range t {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("item %d of the list is %s, and has to be an object like {name, value}", i, describeType(it))
			}
			name, _ := m["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("item %d of the list has no name", i)
			}
			typ, _ := m["type"].(string)
			items = append(items, item{name: name, value: m["value"], typ: typ})
		}
	default:
		return nil, fmt.Errorf("msg.payload is %s, and has to be an object of metric names and values, or a list of {name, value, type}", describeType(payload))
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("msg.payload names no metrics, so there is nothing to send")
	}
	var out []*spMetric
	for _, it := range items {
		var dt uint32
		var ok bool
		switch {
		case it.typ != "":
			dt, ok = spTypeNames[it.typ]
			if !ok {
				return nil, fmt.Errorf("metric %q: type %q is not one of the types this node can send", it.name, it.typ)
			}
		default:
			dt, ok = known(it.name)
			if !ok && guess {
				switch it.value.(type) {
				case bool:
					dt, ok = spBoolean, true
				case string:
					dt, ok = spString, true
				default:
					dt, ok = spDouble, true
				}
			}
			if !ok {
				return nil, fmt.Errorf("the host hasn't seen metric %q born, so it doesn't know its datatype. Give it, "+
					"as {\"name\": %q, \"value\": ..., \"type\": \"Double\"}, or wait for the edge node to be born", it.name, it.name)
			}
		}
		mt, err := spMetricFor(it.name, dt, it.value, false, 0)
		if err != nil {
			return nil, fmt.Errorf("metric %q: %w", it.name, err)
		}
		// A command names its datatype even though a data message wouldn't:
		// the edge node has no birth of the host's to look it up in.
		mt.DataType, mt.HasDataType = dt, true
		if alias, ok := aliasOf(it.name); ok {
			// An edge node that used aliases in its birth is commanded by number.
			mt.Name, mt.Alias, mt.HasAlias = "", alias, true
		}
		out = append(out, mt)
	}
	return out, nil
}
