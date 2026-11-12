package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/dashboard"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The dashboard node types a Node-RED flow uses: ui_base, ui_tab, ui_group,
// ui_link, ui_spacer and the widgets ui_text, ui_numeric, ui_button, ui_switch,
// ui_text_input, ui_led, ui_gauge, ui_chart and ui_template, with
// node-red-dashboard's property names. The page they make is served at /ui
// (HOTLOOP_FLOW_UI_PATH moves it) by internal/dashboard. Nothing is ported from
// node-red-dashboard.

const colorDashboard = "#7BB3A0"

// UIPathEnv names the variable that moves the dashboard off /ui.
const UIPathEnv = "HOTLOOP_FLOW_UI_PATH"

var ui struct {
	mu     sync.Mutex
	hub    *dashboard.Hub
	refs   int
	unbind []func()
}

// uiAcquire hands out the process's dashboard, mounting it on the flow routes
// the first time a node needs it.
//
// The hub lives as long as the process. A deploy stops every dashboard node and
// starts them again, and the browsers that have the page open are connected to
// this hub: if it were thrown away between the two they would sit connected to
// nothing and never hear that the page changed. Only the routes come and go.
func uiAcquire() (*dashboard.Hub, error) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.hub == nil {
		ui.hub = dashboard.New()
	}
	if ui.refs == 0 {
		path := strings.TrimRight(os.Getenv(UIPathEnv), "/")
		if path == "" {
			path = "/ui"
		}
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("%s is %q, and it has to be a path like /ui", UIPathEnv, path)
		}
		full := strings.TrimRight(Routes.Root(), "/") + path
		h := ui.hub.Handler(full)
		var unbind []func()
		for _, pat := range []string{path, path + "/*"} {
			u, err := Routes.Register("dashboard", flowMethodAny, pat, h)
			if err != nil {
				for _, f := range unbind {
					f()
				}
				return nil, fmt.Errorf("the dashboard cannot be served at %s: %w. Move it with %s", full, err, UIPathEnv)
			}
			unbind = append(unbind, u)
		}
		ui.unbind = unbind
	}
	ui.refs++
	return ui.hub, nil
}

// uiRelease lets go of the dashboard. The last one out unmounts the routes.
func uiRelease() {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.refs > 0 {
		ui.refs--
	}
	if ui.refs == 0 {
		for _, f := range ui.unbind {
			f()
		}
		ui.unbind = nil
	}
}

// flowMethodAny is every request method on a flow route.
const flowMethodAny = ""

func init() {
	registerUIConfig()
	for _, w := range uiWidgets {
		registerUIWidget(w)
	}
}

// ---------------------------------------------------------------------------
// the nodes
// ---------------------------------------------------------------------------

type uiSpec struct {
	typ      string
	label    string
	config   bool // a node without wires, which the flow file marks as a config node
	outputs  int
	inputs   int
	icon     string
	props    []node.Prop
	help     string
	notes    string
	unsupp   []string
	partial  bool
	hasGroup bool
}

func uiCommon() []node.Prop {
	return []node.Prop{
		{Name: "name", Kind: node.PropString, Label: "Name"},
		{Name: "group", Kind: node.PropConfigRef, Label: "Group", ConfigType: "ui_group", Required: true},
		{Name: "order", Kind: node.PropNumber, Label: "Order", Default: 0},
		{Name: "width", Kind: node.PropNumber, Label: "Width", Default: 0, Help: "In columns. 0 fills the group."},
		{Name: "height", Kind: node.PropNumber, Label: "Height", Default: 0, Help: "In rows. 0 is one row."},
	}
}

func uiTyped(name, label, dflt string, extra ...string) node.Prop {
	return node.Prop{Name: name, Kind: node.PropTypedInput, Label: label, Default: dflt, TypeProp: name + "Type",
		TypedInputTypes: []string{node.TypeStr, node.TypeNum, node.TypeBool, node.TypeJSON, node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeEnv, node.TypeDate}}
}

func uiSelect(name, label, dflt string, opts ...string) node.Prop {
	o := make([]node.Option, len(opts))
	for i, v := range opts {
		o[i] = node.Option{Value: v, Label: v}
	}
	return node.Prop{Name: name, Kind: node.PropSelect, Label: label, Default: dflt, Options: o}
}

func uiBool(name, label string) node.Prop {
	return node.Prop{Name: name, Kind: node.PropBool, Label: label}
}
func uiStr(name, label string) node.Prop {
	return node.Prop{Name: name, Kind: node.PropString, Label: label}
}
func uiNum(name, label string, dflt any) node.Prop {
	return node.Prop{Name: name, Kind: node.PropNumber, Label: label, Default: dflt}
}

var uiWidgets = []uiSpec{
	{typ: "ui_text", label: "text", inputs: 1, icon: "text", hasGroup: true,
		props: []node.Prop{uiStr("label", "Label"), uiStr("format", "Value format"),
			uiSelect("layout", "Layout", "row-spread", "row-spread", "row-left", "row-right", "col-center", "col-left", "col-right"),
			uiBool("style", "Style the value"), uiStr("font", "Font"), uiNum("fontSize", "Font size", 16), uiStr("color", "Color")},
		help:  "Shows text on the dashboard. The value format is a template: {{msg.payload}} is the message's payload.",
		notes: "Shows msg.payload through the value format, with {{msg.x}} and {{value}} and the number filter ({{msg.payload | number:1}}). The label may contain <br>, and no other markup. Angular's other filters are not supported."},
	{typ: "ui_numeric", label: "numeric", inputs: 1, outputs: 1, icon: "number", hasGroup: true,
		props: []node.Prop{uiStr("label", "Label"), uiStr("tooltip", "Tooltip"), uiBool("wrap", "Wrap at the limits"), uiBool("passthru", "Pass the message on"),
			uiTyped("topic", "Topic", "topic"), uiStr("format", "Value format"), uiStr("min", "Min"), uiStr("max", "Max"), uiNum("step", "Step", 1)},
		help:  "A number the operator can change. A message sets it, and a change sends msg.payload.",
		notes: "A message with a numeric payload sets the value, and with passthru on it goes on unchanged. The operator's change sends {payload, topic}, within min and max. wrap is accepted and ignored: the value stops at the limits."},
	{typ: "ui_button", label: "button", inputs: 1, outputs: 1, icon: "button", hasGroup: true,
		props: []node.Prop{uiStr("label", "Label"), uiStr("tooltip", "Tooltip"), uiBool("passthru", "Pass the message on"), uiStr("color", "Text color"), uiStr("bgcolor", "Background"),
			uiStr("icon", "Icon"), uiTyped("payload", "Payload", "", ""), uiTyped("topic", "Topic", "topic")},
		help:   "A button. A press sends msg.payload and msg.topic as configured.",
		notes:  "A press sends the payload and topic, each by its type (a literal, a flow or global value, a property of the last message, the time). The icon is accepted and not drawn.",
		unsupp: []string{"icon"}},
	{typ: "ui_switch", label: "switch", inputs: 1, outputs: 1, icon: "switch", hasGroup: true,
		props: []node.Prop{uiStr("label", "Label"), uiStr("tooltip", "Tooltip"), uiBool("passthru", "Pass the message on"), uiBool("decouple", "Show only what a message says"),
			uiTyped("topic", "Topic", "topic"), uiStr("style", "Style"), uiTyped("onvalue", "On value", "true"), uiTyped("offvalue", "Off value", "false"),
			uiStr("oncolor", "On color"), uiStr("offcolor", "Off color"), uiBool("animate", "Animate")},
		help:   "A switch. The operator's click sends the on or the off value.",
		notes:  "A click sends the on or the off value. With decouple off the switch moves at once, and with it on it moves only when a message with the value arrives. A message whose payload equals the on or the off value sets the switch. The icons and animate are accepted and not drawn.",
		unsupp: []string{"onicon", "officon", "animate"}},
	{typ: "ui_text_input", label: "text input", inputs: 1, outputs: 1, icon: "input", hasGroup: true,
		props: []node.Prop{uiStr("label", "Label"), uiStr("tooltip", "Tooltip"), uiBool("passthru", "Pass the message on"),
			uiSelect("mode", "Mode", "text", "text", "email", "password", "number", "time", "date", "color"),
			uiStr("delay", "Delay (ms)"), uiBool("sendOnBlur", "Send when it loses focus"), uiTyped("topic", "Topic", "topic")},
		help:  "A text box. What the operator types goes out as msg.payload.",
		notes: "Sends the text, or the number in number mode, after delay milliseconds without typing, on Enter, or on leaving the box when sendOnBlur is on. A message sets the text."},
	{typ: "ui_led", label: "LED", inputs: 1, icon: "led", hasGroup: true,
		props: []node.Prop{uiStr("label", "Label"), uiSelect("labelPlacement", "Label placement", "left", "left", "right"), uiSelect("labelAlignment", "Label alignment", "left", "left", "center", "right"),
			uiSelect("shape", "Shape", "circle", "circle", "square", "rounded"), uiBool("showGlow", "Glow"), uiBool("allowColorForValueInMessage", "Take the color from msg.color")},
		help:  "A light. Its color follows msg.payload through the color-for-value list.",
		notes: "Draws a light whose color is the first entry of colorForValue that equals msg.payload, as a boolean, a number or text, or msg.color when allowed."},
	{typ: "ui_gauge", label: "gauge", inputs: 1, outputs: 0, icon: "gauge", hasGroup: true,
		props: []node.Prop{uiSelect("gtype", "Type", "gage", "gage", "donut", "compass", "wave"), uiStr("title", "Title"), uiStr("label", "Units"),
			uiStr("format", "Value format"), uiStr("min", "Min"), uiStr("max", "Max"), uiStr("seg1", "Sector 1 ends"), uiStr("seg2", "Sector 2 ends"), uiBool("diff", "Show the difference")},
		help:  "A gauge for a number between min and max.",
		notes: "Gauge, donut, compass and wave, drawn on a canvas, in the three sector colors. The difference mode of the donut is accepted and drawn as the plain donut."},
	{typ: "ui_chart", label: "chart", inputs: 1, outputs: 1, icon: "chart", hasGroup: true,
		props: []node.Prop{uiStr("label", "Title"), uiSelect("chartType", "Type", "line", "line", "bar", "horizontalBar", "pie", "polar-area", "radar"),
			uiStr("legend", "Legend"), uiStr("xformat", "X axis format"), uiSelect("interpolate", "Interpolate", "linear", "linear", "step", "bezier", "none"),
			uiStr("nodata", "Text with no data"), uiBool("dot", "Dots"), uiStr("ymin", "Y min"), uiStr("ymax", "Y max"),
			uiStr("removeOlder", "Keep for"), uiStr("removeOlderPoints", "Keep at most"), uiStr("removeOlderUnit", "Unit (seconds in one)"),
			uiBool("useUTC", "UTC"), uiBool("useOneColor", "One color")},
		help:   "A chart of msg.payload against time. msg.topic names the line.",
		notes:  "Line and bar charts of numbers over time, one line for each msg.topic, kept for removeOlder times its unit and at most removeOlderPoints points a line. A payload of [] clears it. Pie, polar, radar and horizontal bar are drawn as a line. Charts built from an array of series objects, and the output on a click of a point, are not provided.",
		unsupp: []string{"cutout", "useDifferentColor", "outputs"}},
	{typ: "ui_template", label: "template", inputs: 1, outputs: 1, icon: "template", hasGroup: true,
		props: []node.Prop{{Name: "format", Kind: node.PropText, Label: "Template", Language: "html"},
			uiSelect("templateScope", "Scope", "local", "local", "global"), uiBool("fwdInMessages", "Pass messages on"),
			uiBool("storeOutMessages", "Store what it sends"), uiBool("resendOnRefresh", "Resend on refresh")},
		help: "HTML for the dashboard, with {{msg.payload}}, ng-click, ng-class, ng-init, ng-show, ng-hide, ng-if, ng-bind, " +
			"scope.$watch and scope.send(msg).",
		notes: "HTML with a small AngularJS-style layer: {{ }}, ng-click, ng-class, ng-init, ng-show, ng-hide, ng-if, ng-bind, scope.msg, scope.$watch, scope.send and scope.$eval. Directives beyond those (ng-repeat, ng-model, ng-switch, controllers, Angular Material components) are not provided. The template runs in the operator's browser with the page's own permissions, so it is as trusted as the flow's author. Global scope is treated as local."},
	{typ: "ui_spacer", label: "spacer", config: true, icon: "spacer", hasGroup: true,
		props: []node.Prop{uiStr("name", "Name"), {Name: "group", Kind: node.PropConfigRef, Label: "Group", ConfigType: "ui_group", Required: true},
			uiNum("order", "Order", 0), uiNum("width", "Width", 1), uiNum("height", "Height", 1)},
		help: "An empty cell, to space a group out.", notes: "An empty cell."},
}

func registerUIConfig() {
	cfg := func(typ, label string, props []node.Prop, notes, help string) {
		node.MustRegister(node.Descriptor{
			Type: typ, Category: node.CategoryConfig, Color: colorDashboard, Icon: "dashboard", IsConfig: true,
			LabelProp: "name", Props: props, Help: help, StandIn: true,
			Compatibility: node.Compatibility{Level: node.CompatPartial, Notes: notes},
		}, func(def *node.Definition) (node.Node, error) { return newUIConfig(typ, def) })
	}
	cfg("ui_base", "dashboard", []node.Prop{uiStr("name", "Name")},
		"The dashboard's theme: its name (dark or light) and base color are used. Custom fonts, sizes, widget colors and the site title are accepted and not applied.",
		"The dashboard's theme.")
	cfg("ui_tab", "tab", []node.Prop{uiStr("name", "Name"), uiStr("icon", "Icon"), uiNum("order", "Order", 0), uiBool("disabled", "Disabled"), uiBool("hidden", "Hidden")},
		"A page of the dashboard, in the menu by its order. The icon is accepted and not drawn. A disabled or hidden tab is left out.",
		"A tab of the dashboard.")
	cfg("ui_group", "group", []node.Prop{uiStr("name", "Name"), {Name: "tab", Kind: node.PropConfigRef, Label: "Tab", ConfigType: "ui_tab", Required: true},
		uiNum("order", "Order", 0), uiBool("disp", "Show the name"), uiNum("width", "Width", 6), uiBool("collapse", "Collapsible")},
		"A card on a tab, width columns wide, in the tab's order. Its widgets are laid out in a grid of that many columns.",
		"A group of widgets on a tab.")
	cfg("ui_link", "link", []node.Prop{uiStr("name", "Name"), uiStr("link", "Link"), uiStr("icon", "Icon"), uiSelect("target", "Open in", "thistab", "thistab", "newtab"), uiNum("order", "Order", 0)},
		"A menu entry that opens another address, http or https only. The icon is accepted and not drawn.",
		"A link in the dashboard's menu.")
	// Dashboard 2 is a different dashboard, and its nodes are accepted so a flow that carries them loads.
	for _, t := range []string{"ui-base", "ui-theme", "ui-page", "ui-group"} {
		typ := t
		node.MustRegister(node.Descriptor{
			Type: typ, Category: node.CategoryConfig, Color: colorDashboard, Icon: "dashboard", IsConfig: true,
			LabelProp: "name", Props: []node.Prop{uiStr("name", "Name")},
			Help: "A Dashboard 2 configuration node, kept so the flow loads. Nothing draws it.",
			Compatibility: node.Compatibility{Level: node.CompatPartial,
				Notes: "Dashboard 2 (FlowFuse) configuration, accepted so a flow that carries it loads and saves unchanged. Dashboard 2's widgets (ui-text, ui-button and the rest) are not provided, so nothing draws it. Use the ui_ nodes."},
		}, func(*node.Definition) (node.Node, error) { return &nrPassiveConfig{}, nil })
	}
}

func registerUIWidget(s uiSpec) {
	props := s.props
	if s.hasGroup && !s.config {
		props = append(uiCommon(), s.props...)
	}
	d := node.Descriptor{
		Type: s.typ, Category: node.CategoryFunction, Color: colorDashboard, Icon: "dashboard",
		Inputs: s.inputs, Outputs: s.outputs, PaletteLabel: s.label, LabelProp: "name",
		Props: props, Help: s.help, StandIn: true,
		Compatibility: node.Compatibility{Level: node.CompatPartial, Notes: s.notes, UnsupportedProps: s.unsupp},
	}
	if s.config {
		d.Category, d.IsConfig = node.CategoryConfig, true
	}
	if s.typ == "ui_gauge" || s.typ == "ui_text" || s.typ == "ui_led" {
		d.Align = "right"
	}
	node.MustRegister(d, func(def *node.Definition) (node.Node, error) { return newUIWidget(s, def) })
}

// uiProps copies a node's entry for the page, without the editor's bookkeeping.
func uiProps(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		switch k {
		case "id", "type", "z", "g", "x", "y", "wires", "d":
			continue
		}
		out[k] = v
	}
	return out
}

// jsonable keeps what a browser can be sent: a message's values that survive
// JSON and are not huge. A byte buffer from a Modbus read is neither.
func jsonable(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		switch t := v.(type) {
		case []byte, engine.ImmutableBytes:
			continue
		case string:
			if len(t) > 16<<10 {
				continue
			}
		}
		b, err := json.Marshal(v)
		if err != nil || len(b) > 16<<10 {
			continue
		}
		var back any
		if json.Unmarshal(b, &back) == nil {
			out[k] = back
		}
	}
	return out
}

// uiIntProp reads an order, a width or a height, which the editor stores as
// either a number or text.
func uiIntProp(raw map[string]any, key string) int {
	switch v := raw[key].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return 0
}

// --- config nodes -----------------------------------------------------------

type uiConfig struct {
	typ  string
	id   string
	svc  node.Services
	raw  map[string]any
	hub  *dashboard.Hub
	held bool
}

func newUIConfig(typ string, def *node.Definition) (node.Node, error) {
	return &uiConfig{typ: typ, id: def.Node.ID, svc: def.Services, raw: def.Node.Raw}, nil
}

func (c *uiConfig) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (c *uiConfig) Start(context.Context, node.Emitter) error {
	if node.StandInOf(c.svc) != nil {
		return nil // a flow test serves no page
	}
	hub, err := uiAcquire()
	if err != nil {
		return err
	}
	c.hub, c.held = hub, true
	m := &dashboard.Member{ID: c.id, Type: c.typ, Props: uiProps(c.raw), Order: uiIntProp(c.raw, "order")}
	switch c.typ {
	case "ui_base":
		m.Kind = dashboard.KindBase
	case "ui_tab":
		m.Kind = dashboard.KindTab
	case "ui_group":
		tab, _ := c.raw["tab"].(string)
		m.Kind, m.Parent = dashboard.KindGroup, tab
	case "ui_link":
		m.Kind = dashboard.KindLink
	}
	hub.Add(m)
	return nil
}

func (c *uiConfig) Close(context.Context, bool) error {
	if c.held {
		c.hub.Remove(c.id)
		c.held = false
		uiRelease()
	}
	return nil
}

// --- widgets ----------------------------------------------------------------

type uiWidget struct {
	spec uiSpec
	id   string
	raw  map[string]any
	svc  node.Services

	mu   sync.Mutex
	hub  *dashboard.Hub
	out  node.Emitter
	last *engine.Msg
	held bool

	passthru bool
	onv, off TypedValue
	payload  TypedValue
	topic    TypedValue
	keep     time.Duration
	maxPts   int
	min, max float64
	hasMin   bool
	hasMax   bool
}

func newUIWidget(s uiSpec, def *node.Definition) (node.Node, error) {
	p := def.Node
	group, _ := p.Raw["group"].(string)
	if group == "" {
		return nil, fmt.Errorf("no group: pick the dashboard group this %s sits in", s.label)
	}
	w := &uiWidget{spec: s, id: p.ID, raw: p.Raw, svc: def.Services, passthru: p.PropBool("passthru", false)}
	w.topic = ReadTypedValue(p.Raw, "topic", "topicType", node.TypeMsg)
	switch s.typ {
	case "ui_button":
		w.payload = ReadTypedValue(p.Raw, "payload", "payloadType", node.TypeStr)
	case "ui_switch":
		w.onv = ReadTypedValue(p.Raw, "onvalue", "onvalueType", node.TypeBool)
		w.off = ReadTypedValue(p.Raw, "offvalue", "offvalueType", node.TypeBool)
		if w.onv.Value == "" {
			w.onv = TypedValue{Type: node.TypeBool, Value: "true"}
		}
		if w.off.Value == "" {
			w.off = TypedValue{Type: node.TypeBool, Value: "false"}
		}
		for _, tv := range []TypedValue{w.onv, w.off} {
			if err := tv.Check(); err != nil {
				return nil, err
			}
		}
	case "ui_numeric":
		w.min, w.hasMin = uiLimit(p.Raw["min"])
		w.max, w.hasMax = uiLimit(p.Raw["max"])
	case "ui_chart":
		unit := p.PropFloat("removeOlderUnit", 3600)
		w.keep = time.Duration(p.PropFloat("removeOlder", 1) * unit * float64(time.Second))
		w.maxPts = p.PropInt("removeOlderPoints", 0)
	}
	for _, tv := range []TypedValue{w.topic, w.payload} {
		if tv.Type != "" {
			if err := tv.Check(); err != nil {
				return nil, err
			}
		}
	}
	return w, nil
}

func (w *uiWidget) Start(ctx context.Context, out node.Emitter) error {
	if node.StandInOf(w.svc) != nil {
		return nil // a flow test serves no page, and a widget still passes its messages on
	}
	hub, err := uiAcquire()
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.hub, w.out, w.held = hub, out, true
	w.mu.Unlock()
	group, _ := w.raw["group"].(string)
	hub.Add(&dashboard.Member{
		ID: w.id, Kind: dashboard.KindWidget, Type: w.spec.typ, Props: uiProps(w.raw),
		Parent: group, Order: uiIntProp(w.raw, "order"), Flow: node.TabLabelOf(w.svc),
		OnEvent: w.onEvent,
	})
	return nil
}

func (w *uiWidget) Close(context.Context, bool) error {
	w.mu.Lock()
	held, hub := w.held, w.hub
	w.held, w.out = false, nil
	w.mu.Unlock()
	if held {
		hub.Remove(w.id)
		uiRelease()
	}
	return nil
}

func (w *uiWidget) now() time.Time { return node.ClockOf(w.svc).Now() }

// eval reads a typed value against the last message this widget saw.
func (w *uiWidget) eval(tv TypedValue) (any, bool) {
	w.mu.Lock()
	last := w.last
	w.mu.Unlock()
	if last == nil {
		last = engine.NewMsg()
	}
	v, ok, err := tv.Eval(EvalContext{Msg: last, Services: w.svc, Now: w.now})
	if err != nil || !ok {
		return nil, false
	}
	return v, true
}

func sameValue(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	af, aok := asFloat(a)
	bf, bok := asFloat(b)
	if aok && bok {
		if _, ab := a.(bool); !ab {
			if _, bb := b.(bool); !bb {
				return af == bf
			}
		}
	}
	return false
}

func (w *uiWidget) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	w.mu.Lock()
	w.last = m
	hub := w.hub
	w.mu.Unlock()
	if hub == nil {
		// No page (a flow test): the widget still passes the message on when it is set to.
		if (w.passthru && w.spec.outputs > 0) || (w.spec.typ == "ui_template" && w.raw["fwdInMessages"] == true) {
			out.Send(0, m)
		}
		return nil
	}
	data := jsonable(m.Data)
	switch w.spec.typ {
	case "ui_switch":
		var value any
		if v, ok := w.eval(w.onv); ok && sameValue(m.Payload(), v) {
			value = true
		} else if v, ok := w.eval(w.off); ok && sameValue(m.Payload(), v) {
			value = false
		}
		if value == nil {
			if b, ok := m.Payload().(bool); ok {
				value = b
			}
		}
		hub.SetState(w.id, data, value)
	case "ui_numeric":
		f, ok := asFloat(m.Payload())
		if s, isStr := m.Payload().(string); isStr {
			g, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			f, ok = g, err == nil
		}
		if !ok {
			out.Status(node.Status{Fill: "yellow", Shape: "ring", Text: "not a number"})
			return nil
		}
		hub.SetState(w.id, data, f)
	case "ui_chart":
		return w.chart(hub, m, out)
	default:
		hub.SetState(w.id, data, nil)
	}
	if (w.passthru && w.spec.outputs > 0) || (w.spec.typ == "ui_template" && w.raw["fwdInMessages"] == true) {
		out.Send(0, m)
	}
	return nil
}

func (w *uiWidget) chart(hub *dashboard.Hub, m *engine.Msg, out node.Emitter) error {
	if l, ok := m.Payload().([]any); ok {
		if len(l) == 0 {
			hub.ClearChart(w.id)
			return nil
		}
		out.Status(node.Status{Fill: "yellow", Shape: "ring", Text: "series objects not supported"})
		return nil
	}
	var v float64
	switch t := m.Payload().(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return nil // text is not a point
		}
		v = f
	case bool:
		return nil
	default:
		f, ok := asFloat(t)
		if !ok {
			return nil
		}
		v = f
	}
	at := w.now()
	if ts, ok := asFloat(m.Data["timestamp"]); ok && ts > 1e11 {
		at = time.UnixMilli(int64(ts))
	}
	series := m.Topic()
	hub.AddPoint(w.id, series, at, v, w.keep, w.maxPts)
	return nil
}

// onEvent is what the operator did to the widget in a browser.
func (w *uiWidget) onEvent(ev map[string]any) {
	w.mu.Lock()
	out, hub := w.out, w.hub
	w.mu.Unlock()
	if out == nil {
		return
	}
	msg := engine.NewMsg()
	if tv := w.topic; tv.Value != "" || tv.Type != node.TypeMsg {
		if t, ok := w.eval(tv); ok && t != nil && t != "" {
			msg.SetTopic(fmt.Sprint(t))
		}
	}
	switch w.spec.typ {
	case "ui_button":
		if ev["event"] != "click" {
			return
		}
		if v, ok := w.eval(w.payload); ok {
			msg.SetPayload(v)
		}
	case "ui_switch":
		on, _ := ev["value"].(bool)
		tv := w.off
		if on {
			tv = w.onv
		}
		v, _ := w.eval(tv)
		msg.SetPayload(v)
		if fmt.Sprint(w.raw["decouple"]) != "true" {
			hub.SetState(w.id, nil, on)
		}
	case "ui_numeric":
		f, ok := ev["value"].(float64)
		if !ok {
			return
		}
		if w.hasMin && f < w.min {
			f = w.min
		}
		if w.hasMax && f > w.max {
			f = w.max
		}
		msg.SetPayload(f)
		hub.SetState(w.id, w.lastData(), f)
	case "ui_text_input":
		msg.SetPayload(ev["value"])
		if s, ok := ev["value"].(string); ok {
			hub.SetState(w.id, w.lastData(), s)
		}
	case "ui_template":
		m, ok := ev["msg"].(map[string]any)
		if !ok {
			return
		}
		msg = engine.WrapMsg(m)
	default:
		return
	}
	out.Send(0, msg)
}

func (w *uiWidget) lastData() map[string]any {
	msg, _ := w.hub.State(w.id)
	return msg
}

// uiLimit reads a numeric widget's min or max, which the editor stores as a
// number or as text, and which may be empty.
func uiLimit(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}
