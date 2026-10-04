// LinkRegistry.target is ported from Node-RED 5.0.7 @node-red/runtime
// lib/nodes/index.js (linkcallTargets.getTargetNode) (Apache-2.0), Copyright JS
// Foundation and other contributors, http://js.foundation. Modified. The rest
// of this file is HotLoop Flow's own.

package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/cron"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/jsonata"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Palette colours. Kept here rather than scattered as literals so the canvas
// stays coherent: neutrals, with one danger color for the node that catches
// errors, rather than Node-RED's pastels. Node bodies keep the same fill in both
// themes and the label on them is fixed dark ink, so every value here has to
// clear 4.5:1 against #1A1A1A.
const (
	colorCommon   = "#B3B3B3"
	colorInject   = "#A6BBCF"
	colorDebug    = "#87A980"
	colorCatch    = "#FF6B6B"
	colorStatus   = "#E6A03C"
	colorLink     = "#DDDDDD"
	colorFunction = "#FDD0A2"
	colorSequence = "#C0C0C0"
)

func init() {
	registerInject()
	registerDebug()
	registerJunction()
	registerComment()
	registerCatch()
	registerStatus()
	registerComplete()
	registerLinkIn()
	registerLinkOut()
}

// ---------------------------------------------------------------------------
// inject
// ---------------------------------------------------------------------------

// injectNode emits a message on a schedule, at startup, or on demand.
type injectNode struct {
	props    []injectProp
	repeat   time.Duration
	onceThen time.Duration
	once     bool
	topic    string

	// schedule is the crontab, when the node has one and no repeat interval.
	// Node-RED gives a repeat interval priority over a crontab, and so does
	// this.
	schedule *cron.Expr
	// clock and recheck are the wall clock the schedule reads and how often it
	// reads it again: the node clock's Now and scheduleRecheck, swapped only
	// by tests.
	clock   func() time.Time
	recheck time.Duration

	// timers is the node's clock: the wall clock in a flow, the test's under
	// a flow test. timer and ticker are what's pending on it, so a stop can
	// cancel them.
	timers node.Clock
	timer  node.Timer
	ticker *clockTicker

	svc node.Services
	mu  sync.Mutex
}

type injectProp struct {
	Prop string // message property to set
	TV   TypedValue
}

func registerInject() {
	node.MustRegister(node.Descriptor{
		Type:         "inject",
		Category:     node.CategoryCommon,
		Color:        colorInject,
		Icon:         "inject",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "inject",
		LabelProp:    "name",
		HasButton:    true,
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Interval, startup and cron-scheduled injection are supported. The crontab " +
				"is read the way Node-RED's own scheduler, cronosjs, reads it, in the process's " +
				"local time, including what it does with the hour that goes missing or repeats " +
				"when the clocks change. A manual inject from the admin API always sends the " +
				"configured properties: Node-RED's inject-with-these-values " +
				"(msg.__user_inject_props__) is not implemented.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "topic", Kind: node.PropString, Label: "Topic"},
			{Name: "repeat", Kind: node.PropString, Label: "Repeat (seconds)",
				Help: "Emit every N seconds. Leave empty to inject only manually or at startup."},
			{Name: "crontab", Kind: node.PropString, Label: "Cron schedule",
				Placeholder: "30 06 * * 1-5",
				Help: "Inject on a schedule instead of an interval: minute hour day month weekday, " +
					"with an optional leading seconds field. Ignored when Repeat is set."},
			{Name: "once", Kind: node.PropBool, Label: "Inject once at start"},
			{Name: "onceDelay", Kind: node.PropString, Label: "Startup delay (seconds)", Default: "0.1"},
			{Name: "props", Kind: node.PropList, Label: "Properties", Fields: []node.Prop{
				{Name: "p", Kind: node.PropString, Label: "Property", Default: "payload"},
				{Name: "v", Kind: node.PropTypedInput, Label: "Value", TypeProp: "vt"},
			}},
		},
		Help: "Emits a message, either manually from the button, at a repeating " +
			"interval, or once shortly after the flow starts.",
	}, newInject)
}

func newInject(def *node.Definition) (node.Node, error) {
	timers := node.ClockOf(def.Services)
	n := &injectNode{
		clock:   timers.Now,
		timers:  timers,
		recheck: scheduleRecheck,
		svc:     def.Services,
		topic:   def.Node.PropString("topic", ""),
		once:    def.Node.PropBool("once", false),
	}

	if s := strings.TrimSpace(def.Node.PropString("repeat", "")); s != "" {
		secs, err := parseSeconds(s)
		if err != nil {
			return nil, fmt.Errorf("repeat: %w", err)
		}
		if secs <= 0 {
			return nil, fmt.Errorf("repeat must be greater than zero, got %v", secs)
		}
		n.repeat = time.Duration(secs * float64(time.Second))
	}
	if spec := strings.TrimSpace(def.Node.PropString("crontab", "")); spec != "" && n.repeat == 0 {
		expr, err := cron.Parse(spec)
		if err != nil {
			return nil, err
		}
		n.schedule = expr
	}

	// Node-RED defaults the startup delay to 0.1s so that downstream nodes have
	// finished starting before the first message arrives.
	n.onceThen = 100 * time.Millisecond
	if s := strings.TrimSpace(def.Node.PropString("onceDelay", "")); s != "" {
		if secs, err := parseSeconds(s); err == nil && secs >= 0 {
			n.onceThen = time.Duration(secs * float64(time.Second))
		}
	}

	raw, _ := def.Node.Prop("props")
	if arr, ok := raw.([]any); ok {
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			p, _ := m["p"].(string)
			if p == "" {
				continue
			}
			// Node-RED special-cases the topic row: it carries no explicit
			// value type and takes the node's own topic property.
			if p == engine.PropTopic {
				if _, hasV := m["v"]; !hasV {
					n.props = append(n.props, injectProp{
						Prop: engine.PropTopic,
						TV:   TypedValue{Type: node.TypeStr, Value: n.topic},
					})
					continue
				}
			}
			n.props = append(n.props, injectProp{
				Prop: p,
				TV:   ReadTypedValue(m, "v", "vt", node.TypeStr),
			})
		}
	}

	if len(n.props) == 0 {
		// A flow written before the props array existed sets payload and topic
		// directly on the node.
		n.props = append(n.props,
			injectProp{Prop: engine.PropPayload, TV: ReadTypedValue(def.Node.Raw, "payload", "payloadType", node.TypeDate)},
			injectProp{Prop: engine.PropTopic, TV: TypedValue{Type: node.TypeStr, Value: n.topic}},
		)
	}
	for _, p := range n.props {
		if err := p.TV.Check(); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Prop, err)
		}
	}

	return n, nil
}

// Receive lets an inject node be triggered by an inbound message, which is how
// the editor's manual button and a wired trigger both work.
func (n *injectNode) Receive(_ context.Context, _ *engine.Msg, out node.Emitter) error {
	return n.emit(out)
}

func (n *injectNode) emit(out node.Emitter) error {
	m := engine.NewMsg()
	ec := EvalContext{Msg: m, Services: n.svc}

	for _, p := range n.props {
		v, ok, err := p.TV.Eval(ec)
		if err != nil {
			return fmt.Errorf("evaluating %s: %w", p.Prop, err)
		}
		if !ok {
			continue
		}
		if err := m.Set(p.Prop, v); err != nil {
			return fmt.Errorf("setting %s: %w", p.Prop, err)
		}
	}
	out.Send(0, m)
	return nil
}

func (n *injectNode) Start(ctx context.Context, out node.Emitter) error {
	context.AfterFunc(ctx, n.stopTimers)
	if !n.once {
		n.startRepeating(ctx, out)
		return nil
	}
	n.setTimer(ctx, n.timers.AfterFunc(n.onceThen, func() {
		if ctx.Err() != nil {
			return
		}
		if err := n.emit(out); err != nil {
			out.Error(err, nil)
		}
		// The interval or the schedule starts after the startup injection, not
		// alongside it, the order Node-RED's own Inject node uses.
		n.startRepeating(ctx, out)
	}))
	return nil
}

// setTimer keeps the pending timer so a stop can cancel it, and cancels it
// itself if the stop already happened.
func (n *injectNode) setTimer(ctx context.Context, t node.Timer) {
	n.mu.Lock()
	n.timer = t
	n.mu.Unlock()
	if ctx.Err() != nil {
		t.Stop()
	}
}

func (n *injectNode) stopTimers() {
	n.mu.Lock()
	t, tk := n.timer, n.ticker
	n.mu.Unlock()
	if t != nil {
		t.Stop()
	}
	if tk != nil {
		tk.Stop()
	}
}

// startRepeating starts whichever of the interval or the crontab the node has.
func (n *injectNode) startRepeating(ctx context.Context, out node.Emitter) {
	switch {
	case n.repeat > 0:
		t := newClockTicker(n.timers, n.repeat, func() {
			if ctx.Err() != nil {
				return
			}
			if err := n.emit(out); err != nil {
				out.Error(err, nil)
			}
		})
		n.mu.Lock()
		n.ticker = t
		n.mu.Unlock()
		if ctx.Err() != nil {
			t.Stop()
		}
	case n.schedule != nil:
		n.scheduleNext(ctx, out)
	}
}

// scheduleRecheck is the longest the schedule sleeps before looking at the
// clock again.
//
// A timer measures elapsed time, and a schedule is about the time on the wall.
// They drift apart whenever the clock is set, and an edge box with no
// battery-backed clock is set every boot, when NTP finally answers, sometimes
// by hours. Sleeping a minute at a time means a schedule computed against the
// wrong clock is corrected within a minute instead of firing hours late.
const scheduleRecheck = time.Minute

// scheduleNext works out the next date on the crontab and waits for it.
func (n *injectNode) scheduleNext(ctx context.Context, out node.Emitter) {
	next, ok := n.schedule.Next(n.clock(), time.Local)
	if !ok {
		out.Status(node.Status{Fill: "grey", Shape: "ring", Text: "schedule has ended"})
		out.Log(node.LogWarn, "crontab %q has no more dates to fire on", n.schedule.String())
		return
	}
	n.waitUntil(ctx, out, next)
}

// waitUntil sleeps toward next a recheck at a time, looking at the clock again
// each time it wakes, and injects once the clock says it's time.
func (n *injectNode) waitUntil(ctx context.Context, out node.Emitter, next time.Time) {
	if ctx.Err() != nil {
		return
	}
	wait := next.Sub(n.clock())
	if wait <= 0 {
		if err := n.emit(out); err != nil {
			out.Error(err, nil)
		}
		n.scheduleNext(ctx, out)
		return
	}
	n.setTimer(ctx, n.timers.AfterFunc(min(wait, n.recheck), func() { n.waitUntil(ctx, out, next) }))
}

// parseSeconds reads a duration expressed in seconds, which is how Node-RED
// persists every interval field — sometimes as a number, sometimes as a string.
func parseSeconds(s string) (float64, error) {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f); err != nil {
		return 0, fmt.Errorf("%q is not a number of seconds", s)
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// debug
// ---------------------------------------------------------------------------

// debugNode publishes messages to the editor's debug sidebar.
type debugNode struct {
	active     bool
	toSidebar  bool
	toStatus   bool
	complete   string // property to show, or "true" for the whole message
	targetType string
	maxLength  int
	svc        node.Services
	nodeID     string
	nodeName   string

	// show and status are set when the flow gives a JSONata expression for
	// what to display (targetType jsonata) or for the status badge
	// (statusType jsonata) instead of a property.
	show   *jsonata.Expr
	status *jsonata.Expr
}

func registerDebug() {
	node.MustRegister(node.Descriptor{
		Type:          "debug",
		Category:      node.CategoryCommon,
		Color:         colorDebug,
		Icon:          "debug",
		Inputs:        1,
		Outputs:       0,
		Align:         "right",
		PaletteLabel:  "debug",
		LabelProp:     "name",
		HasButton:     true,
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "active", Kind: node.PropBool, Label: "Enabled", Default: true},
			{Name: "complete", Kind: node.PropString, Label: "Property", Default: "payload",
				Help: `Property to display, or "true" for the complete message.`},
			{Name: "tosidebar", Kind: node.PropBool, Label: "To debug sidebar", Default: true},
			{Name: "tostatus", Kind: node.PropBool, Label: "To node status"},
			{Name: "statusVal", Kind: node.PropString, Label: "Status property"},
		},
		Help: "Shows a message property in the debug sidebar, and optionally as " +
			"the node's own status badge.",
	}, newDebug)
}

func newDebug(def *node.Definition) (node.Node, error) {
	n := &debugNode{
		active:     def.Node.PropBool("active", true),
		toSidebar:  def.Node.PropBool("tosidebar", true),
		toStatus:   def.Node.PropBool("tostatus", false),
		complete:   def.Node.PropString("complete", "payload"),
		targetType: def.Node.PropString("targetType", "msg"),
		maxLength:  def.Node.PropInt("maxLength", 1000),
		svc:        def.Services,
		nodeID:     def.Node.ID,
		nodeName:   def.Node.Name,
	}
	if n.complete == "" {
		n.complete = "payload"
	}
	if n.targetType == node.TypeJSONata {
		x, err := jsonata.Compile(def.Node.PropString("complete", ""), def.Services)
		if err != nil {
			return nil, err
		}
		n.show = x
	}
	if def.Node.PropString("statusType", "") == node.TypeJSONata {
		x, err := jsonata.Compile(def.Node.PropString("statusVal", ""), def.Services)
		if err != nil {
			return nil, fmt.Errorf("status: %w", err)
		}
		n.status = x
	}
	return n, nil
}

func (n *debugNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	if !n.active {
		return nil
	}

	var shown any
	if n.show != nil {
		v, ok, err := n.show.EvalMsg(ctx, m, nil)
		if err != nil {
			return err
		}
		shown = v
		if !ok {
			shown = "(undefined)"
		}
	} else if n.complete == "true" || n.complete == "complete" {
		shown = m.Data
	} else {
		v, ok, err := m.Get(n.complete)
		if err != nil {
			return fmt.Errorf("debug property %q: %w", n.complete, err)
		}
		if !ok {
			// Node-RED shows "(undefined)" rather than nothing, so that a typo
			// in the property path is visible instead of looking like silence.
			shown = "(undefined)"
		} else {
			shown = v
		}
	}

	if n.toStatus {
		badge := shown
		if n.status != nil {
			v, ok, err := n.status.EvalMsg(ctx, m, nil)
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}
			badge = v
			if !ok {
				badge = "(undefined)"
			}
		}
		out.Status(node.Status{Fill: "grey", Shape: "dot", Text: truncate(stringify(badge), 32)})
	}

	if n.toSidebar {
		out.Publish("debug", map[string]any{
			"id":     n.nodeID,
			"name":   n.nodeName,
			"topic":  m.Topic(),
			"msgId":  m.ID(),
			"format": describeType(shown),
			"msg":    truncate(stringify(shown), n.maxLength),
		})
	}
	return nil
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "(undefined)"
	case []byte:
		return fmt.Sprintf("buffer[%d]", len(t))
	case engine.ImmutableBytes:
		return fmt.Sprintf("buffer[%d]", len(t))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

func describeType(v any) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case string:
		return fmt.Sprintf("string[%d]", len(t))
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	case []any:
		return fmt.Sprintf("array[%d]", len(t))
	case []byte:
		return fmt.Sprintf("buffer[%d]", len(t))
	case engine.ImmutableBytes:
		return fmt.Sprintf("buffer[%d]", len(t))
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d more bytes)", len(s)-max)
}

// ---------------------------------------------------------------------------
// junction, comment
// ---------------------------------------------------------------------------

type passThrough struct{}

func (passThrough) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	out.Send(0, m)
	return nil
}

func registerJunction() {
	node.MustRegister(node.Descriptor{
		Type:          "junction",
		Category:      node.CategoryCommon,
		Color:         colorCommon,
		Icon:          "junction",
		Inputs:        1,
		Outputs:       1,
		PaletteLabel:  "junction",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Help:          "A wiring convenience. Passes messages straight through so wires can be routed tidily.",
	}, func(*node.Definition) (node.Node, error) { return passThrough{}, nil })
}

type noopNode struct{}

func (noopNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func registerComment() {
	node.MustRegister(node.Descriptor{
		Type:          "comment",
		Category:      node.CategoryCommon,
		Color:         colorCommon,
		Icon:          "comment",
		Inputs:        0,
		Outputs:       0,
		PaletteLabel:  "comment",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Title"},
			{Name: "info", Kind: node.PropText, Label: "Note", Language: "markdown"},
		},
		Help: "A note on the canvas. Does nothing at runtime.",
	}, func(*node.Definition) (node.Node, error) { return noopNode{}, nil })
}

// ---------------------------------------------------------------------------
// catch, status, complete
// ---------------------------------------------------------------------------
//
// These three have no logic of their own. The runtime routes to them — see
// Runtime.raiseError, onStatus and onComplete, which handle scope filtering and
// the group-distance rule — and all they do is forward what arrives.

func registerCatch() {
	node.MustRegister(node.Descriptor{
		Type:          "catch",
		Category:      node.CategoryCommon,
		Color:         colorCatch,
		Icon:          "alert",
		Inputs:        0,
		Outputs:       1,
		PaletteLabel:  "catch",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "uncaught", Kind: node.PropBool, Label: "Only catch unhandled errors"},
		},
		Help: "Catches errors raised by other nodes on the same flow. When several " +
			"Catch nodes are eligible, the one closest in the group hierarchy wins.",
	}, func(*node.Definition) (node.Node, error) { return passThrough{}, nil })
}

func registerStatus() {
	node.MustRegister(node.Descriptor{
		Type:          "status",
		Category:      node.CategoryCommon,
		Color:         colorStatus,
		Icon:          "status",
		Inputs:        0,
		Outputs:       1,
		PaletteLabel:  "status",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
		},
		Help: "Reports status changes from other nodes on the same flow.",
	}, func(*node.Definition) (node.Node, error) { return passThrough{}, nil })
}

func registerComplete() {
	node.MustRegister(node.Descriptor{
		Type:         "complete",
		Category:     node.CategoryCommon,
		Color:        colorCommon,
		Icon:         "complete",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "complete",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatFull,
			Notes: "Watches only the nodes explicitly selected in its scope, as Node-RED does.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
		},
		Help: "Fires when a selected node finishes handling a message.",
	}, func(*node.Definition) (node.Node, error) { return passThrough{}, nil })
}

// ---------------------------------------------------------------------------
// link in / link out
// ---------------------------------------------------------------------------
//
// Link nodes are virtual wires: a Link Out names Link In nodes by id and
// delivers to them without a drawn connection, which is how a flow spanning
// several tabs stays readable.

// LinkRegistry resolves link targets across the whole runtime. Link wires cross
// tab boundaries, so they cannot be resolved from the flow graph alone.
type LinkRegistry struct {
	mu      sync.RWMutex
	inputs  map[string]*linkInNode
	pending map[string][]*linkOutNode
	// calls holds the Link Call nodes by id, so a Link Out in return mode can
	// find the one a message came from.
	calls map[string]*linkCallNode
}

// Links is the process-wide link registry.
var Links = &LinkRegistry{
	inputs:  map[string]*linkInNode{},
	pending: map[string][]*linkOutNode{},
	calls:   map[string]*linkCallNode{},
}

// Reset clears the registry. Called on redeploy, and by tests.
func (lr *LinkRegistry) Reset() {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.inputs = map[string]*linkInNode{}
	lr.pending = map[string][]*linkOutNode{}
	lr.calls = map[string]*linkCallNode{}
}

func (lr *LinkRegistry) registerCall(id string, n *linkCallNode) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.calls[id] = n
}

// unregisterCall removes a Link Call, only if the registry still points at that
// instance, for the same reason unregisterIn checks.
func (lr *LinkRegistry) unregisterCall(id string, n *linkCallNode) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.calls[id] == n {
		delete(lr.calls, id)
	}
}

func (lr *LinkRegistry) lookupCall(id string) (*linkCallNode, bool) {
	lr.mu.RLock()
	defer lr.mu.RUnlock()
	n, ok := lr.calls[id]
	return n, ok
}

// target resolves what a Link Call is aimed at, the way Node-RED's runtime
// does. An id always works. In dynamic mode a name works too: a Link In of that
// name on the caller's own flow wins, and failing that one somewhere on a
// regular flow, never inside a subflow instance. Two of them is an error,
// because picking one would be a guess.
func (lr *LinkRegistry) target(callerFlow, target string, dynamic bool) (*linkInNode, error) {
	lr.mu.RLock()
	defer lr.mu.RUnlock()
	if n, ok := lr.inputs[target]; ok {
		return n, nil
	}
	if dynamic && target != "" {
		var here, anywhere []*linkInNode
		for _, n := range lr.inputs {
			if n.name != target {
				continue
			}
			if n.flow == callerFlow {
				here = append(here, n)
			}
			if !n.inSubflow {
				anywhere = append(anywhere, n)
			}
		}
		switch {
		case len(here) == 1:
			return here[0], nil
		case len(anywhere) == 1:
			return anywhere[0], nil
		case len(anywhere) > 1:
			return nil, fmt.Errorf("multiple link in nodes are named %q", target)
		}
	}
	return nil, fmt.Errorf("target link in node %q is not running", target)
}

func (lr *LinkRegistry) registerIn(id string, n *linkInNode) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.inputs[id] = n
}

// unregisterIn removes a Link In, but only if the registry still points at that
// instance. A partial deploy builds the replacement after the old one closes,
// but a node must never be able to unregister the one that replaced it.
func (lr *LinkRegistry) unregisterIn(id string, n *linkInNode) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.inputs[id] == n {
		delete(lr.inputs, id)
	}
}

func (lr *LinkRegistry) lookup(id string) (*linkInNode, bool) {
	lr.mu.RLock()
	defer lr.mu.RUnlock()
	n, ok := lr.inputs[id]
	return n, ok
}

// linkInNode receives from Link Out nodes and emits into its own flow.
type linkInNode struct {
	id string
	// name is what a dynamic Link Call can address it by: its own name, or
	// its id when it has none, as in Node-RED.
	name string
	// flow is the tab or subflow instance it runs in, and inSubflow whether
	// that is an instance. A Link Call by name looks in its own flow first and
	// never reaches into a subflow instance from outside.
	flow      string
	inSubflow bool

	mu  sync.Mutex
	out node.Emitter
}

func registerLinkIn() {
	node.MustRegister(node.Descriptor{
		Type:          "link in",
		Category:      node.CategoryCommon,
		Color:         colorLink,
		Icon:          "link",
		Inputs:        1,
		Outputs:       1,
		PaletteLabel:  "link in",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
		},
		Help: "Receives messages from Link Out nodes anywhere in the runtime.",
	}, newLinkIn)
}

func newLinkIn(def *node.Definition) (node.Node, error) {
	n := &linkInNode{id: def.Node.ID, name: def.Node.Name, flow: def.Node.Z}
	if n.name == "" {
		n.name = n.id
	}
	_, _, n.inSubflow = engine.SplitDerivedID(def.Node.ID)
	Links.registerIn(def.Node.ID, n)
	return n, nil
}

// Close takes the node out of the registry and forgets its emitter.
//
// A full deploy resets the whole registry, but a partial deploy closes one Link
// In while every Link Out stays running. Without this a Link Out naming a
// deleted Link In keeps delivering through the emitter of a runner that has
// gone, and the message disappears with nothing said. With it, the Link Out
// finds no target and raises the error that says which one.
func (n *linkInNode) Close(context.Context, bool) error {
	Links.unregisterIn(n.id, n)
	n.mu.Lock()
	n.out = nil
	n.mu.Unlock()
	return nil
}

func (n *linkInNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	// Remember the emitter so a Link Out on another tab can deliver here.
	n.mu.Lock()
	n.out = out
	n.mu.Unlock()
	out.Send(0, m)
	return nil
}

// deliver is called by a Link Out node.
func (n *linkInNode) deliver(m *engine.Msg) bool {
	n.mu.Lock()
	out := n.out
	n.mu.Unlock()
	if out == nil {
		return false
	}
	out.Send(0, m)
	return true
}

// Start captures the emitter before any message arrives, so a Link Out can
// deliver to a Link In that has not itself received anything yet.
func (n *linkInNode) Start(_ context.Context, out node.Emitter) error {
	n.mu.Lock()
	n.out = out
	n.mu.Unlock()
	return nil
}

// linkOutNode sends to named Link In nodes.
type linkOutNode struct {
	targets []string
	// mode "link" delivers to the named targets; mode "return" sends the
	// message back to the Link Call node that initiated it.
	returnMode bool
}

func registerLinkOut() {
	node.MustRegister(node.Descriptor{
		Type:         "link out",
		Category:     node.CategoryCommon,
		Color:        colorLink,
		Icon:         "link",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "link out",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Both modes are supported: send to Link In nodes, and return to the Link " +
				"Call that sent the message. One deliberate difference: a Link In that is not " +
				"running, and a return with no Link Call to go back to, raise an error a Catch " +
				"node can see. Node-RED drops the first quietly and only logs a warning for the " +
				"second, which makes a deleted or mistyped link very hard to find.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "mode", Kind: node.PropSelect, Label: "Mode", Default: "link", Options: []node.Option{
				{Value: "link", Label: "Send to link in nodes"},
				{Value: "return", Label: "Return to calling link call node"},
			}},
		},
		Help: "Sends messages to Link In nodes without a drawn wire.",
	}, newLinkOut)
}

func newLinkOut(def *node.Definition) (node.Node, error) {
	n := &linkOutNode{returnMode: def.Node.PropString("mode", "link") == "return"}
	if raw, ok := def.Node.Prop("links"); ok {
		if arr, ok := raw.([]any); ok {
			for _, e := range arr {
				if s, ok := e.(string); ok {
					n.targets = append(n.targets, s)
				}
			}
		}
	}
	return n, nil
}

func (n *linkOutNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	if n.returnMode {
		return returnToCaller(m)
	}
	var missing []string
	for _, id := range n.targets {
		target, ok := Links.lookup(id)
		if !ok || !target.deliver(m.Clone()) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		// A link pointing at a node that is not running is a broken flow, not a
		// silent no-op. Node-RED drops these quietly, which makes a mistyped or
		// deleted link target very hard to find.
		return fmt.Errorf("link targets not running: %s", strings.Join(missing, ", "))
	}
	return nil
}
