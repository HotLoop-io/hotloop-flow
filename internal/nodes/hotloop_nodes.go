package nodes

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The HotLoop nodes: entity changes in, an entity's state, a script run and a
// recipe application. The connection and the calls are in hotloop.go, and the
// gated write that the equipment nodes use is in hotloop_gate.go.

func init() {
	registerHotloopIn()
	registerHotloopState()
	registerHotloopScript()
	registerHotloopRecipe()
}

// hotloopStringTypes is what a node's id fields may be set from.
func hotloopStringTypes() []string {
	return []string{node.TypeStr, node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeEnv, node.TypeJSONata}
}

// hotloopObjectTypes is what a node's fields and params may be set from.
func hotloopObjectTypes() []string {
	return []string{node.TypeJSON, node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeEnv, node.TypeJSONata}
}

// ---------------------------------------------------------------------------
// hotloop in
// ---------------------------------------------------------------------------

// hotloopIdle is how long the live feed may say nothing before the node decides
// it is gone. HotLoop sends a comment every 20 seconds, so three missed ones is
// a connection that is dead with the socket still open.
const hotloopIdle = 60 * time.Second

type hotloopSeen struct {
	state       string
	lastChanged string
}

type hotloopInNode struct {
	id       string
	cfgID    string
	patterns []string
	initial  bool
	numeric  bool
	svc      node.Services
	target   hotloopTarget

	cancel context.CancelFunc
	done   chan struct{}

	// seen is the last state of every entity this node follows, kept across
	// reconnects so that what changed while the link was down is sent when it
	// comes back, and what did not change is not.
	seen    map[string]hotloopSeen
	started bool
}

func registerHotloopIn() {
	node.MustRegister(node.Descriptor{
		Type:         "hotloop in",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "hotloop in",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own node. It follows HotLoop's live feed (GET /api/stream), which has no Node-RED counterpart.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "HotLoop", ConfigType: "hotloop-flow-hotloop", Required: true},
			{Name: "entities", Kind: node.PropList, Label: "Entities", Fields: []node.Prop{
				{Name: "entity", Kind: node.PropString, Label: "Entity id", Placeholder: "sensor.tank_1_level"},
			}, Help: "An entity id, or a pattern: sensor.tank_* follows every sensor whose id starts with tank_, " +
				"and * follows every entity. A plant with thousands of entities sends a message for every change of every " +
				"one that matches, so name what the flow needs."},
			{Name: "initial", Kind: node.PropBool, Label: "Send the current state when it connects", Default: false,
				Help: "Off, only changes after the node connects are sent."},
			{Name: "numeric", Kind: node.PropBool, Label: "Numbers as numbers", Default: false,
				Help: "HotLoop sends every state as text. On, a state that is a number becomes one. on, off and unknown stay text."},
		},
		Help: "Sends a message each time a HotLoop entity changes state. msg.topic is the entity id, msg.payload " +
			"the new state, and msg.hotloop the entity id, the state it had before when this node saw it, its " +
			"attributes, its quality and its timestamps.\n\n" +
			"HotLoop's own screens use the same feed. If the link drops the node says so on its badge and raises an " +
			"error once, then keeps trying with a longer wait after each failure up to thirty seconds. When it is " +
			"back, an entity that changed in the meantime is sent, and one that didn't isn't.",
	}, newHotloopIn)
}

func newHotloopIn(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &hotloopInNode{
		id:      p.ID,
		cfgID:   p.PropString("server", ""),
		initial: p.PropBool("initial", false),
		numeric: p.PropBool("numeric", false),
		svc:     def.Services,
		seen:    map[string]hotloopSeen{},
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no HotLoop selected: pick the HotLoop to follow")
	}
	rows, _ := p.Raw["entities"].([]any)
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		s := strings.TrimSpace(stringOr(m["entity"], ""))
		if s == "" {
			continue
		}
		if _, err := path.Match(s, "x"); err != nil {
			return nil, fmt.Errorf("entity pattern %q is not valid: %w", s, err)
		}
		n.patterns = append(n.patterns, s)
	}
	if len(n.patterns) == 0 {
		return nil, fmt.Errorf("add at least one entity id or pattern to follow, or * for every entity")
	}
	return n, nil
}

func (n *hotloopInNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *hotloopInNode) matches(id string) bool {
	for _, p := range n.patterns {
		if ok, _ := path.Match(p, id); ok {
			return true
		}
	}
	return false
}

func (n *hotloopInNode) Start(ctx context.Context, out node.Emitter) error {
	tgt, err := hotloopTarget0(n.svc, n.cfgID, &n.target)
	if err != nil {
		return err
	}
	if node.StandInOf(n.svc) != nil {
		// A flow test follows nothing. What HotLoop would have sent is whatever
		// the test injects here.
		return nil
	}
	rctx, cancel := context.WithCancel(ctx)
	n.cancel = cancel
	n.done = make(chan struct{})
	out.Status(node.Status{Fill: "yellow", Shape: "ring", Text: "connecting"})
	go func() {
		defer close(n.done)
		n.run(rctx, tgt, out)
	}()
	return nil
}

func (n *hotloopInNode) Close(ctx context.Context, _ bool) error {
	if n.cancel != nil {
		n.cancel()
		select {
		case <-n.done:
		case <-ctx.Done():
		}
	}
	return nil
}

// run keeps the feed open for as long as the node is.
func (n *hotloopInNode) run(ctx context.Context, tgt hotloopTarget, out node.Emitter) {
	wait := time.Second
	reported := false
	for ctx.Err() == nil {
		up := time.Now()
		err := n.session(ctx, tgt, out, &reported)
		if ctx.Err() != nil {
			return
		}
		if time.Since(up) > time.Minute {
			wait = time.Second
		}
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: truncate(err.Error(), 32)})
		if !reported {
			// Once per outage, or a Catch node gets the same sentence every
			// few seconds.
			out.Error(err, nil)
			reported = true
		}
		out.Log(node.LogWarn, "HotLoop feed lost, trying again in %s: %v", wait, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > 30*time.Second {
			wait = 30 * time.Second
		}
	}
}

// session is one connection: open the feed, take the picture HotLoop sends
// first, then relay changes until the connection is no good.
func (n *hotloopInNode) session(ctx context.Context, tgt hotloopTarget, out node.Emitter, reported *bool) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := tgt.openStream(cctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Quiet for too long is a dead connection, and it produces no error
	// anywhere, only the absence of lines.
	var quiet atomic.Bool
	idle := time.AfterFunc(hotloopIdle, func() { quiet.Store(true); cancel() })
	defer idle.Stop()

	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "connected"})
	*reported = false

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	var event, data string
	pictured := false
	for sc.Scan() {
		idle.Reset(hotloopIdle)
		line := sc.Text()
		switch {
		case line == "":
			if event == "entities" && data != "" {
				if err := n.entities(data, !pictured, out, tgt); err != nil {
					return err
				}
				pictured = true
			}
			event, data = "", ""
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			if data != "" {
				data += "\n"
			}
			data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if quiet.Load() {
		return fmt.Errorf("HotLoop's live feed went quiet for %s, so the node is reconnecting", hotloopIdle)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("HotLoop's live feed broke: %w", scrubURLError(err))
	}
	return fmt.Errorf("HotLoop closed its live feed")
}

// entities handles one entities event. The first one on a connection is the
// picture of everything, and what is sent from it depends on whether this node
// has been connected before.
func (n *hotloopInNode) entities(data string, picture bool, out node.Emitter, tgt hotloopTarget) error {
	var ev struct {
		Changed []map[string]any `json:"changed"`
		Removed []string         `json:"removed"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return fmt.Errorf("HotLoop sent an entities event that is not JSON: %w", err)
	}
	for _, id := range ev.Removed {
		delete(n.seen, id)
	}
	for _, st := range ev.Changed {
		id, _ := st["entity_id"].(string)
		if id == "" || !n.matches(id) {
			continue
		}
		state, _ := st["state"].(string)
		lc, _ := st["last_changed"].(string)
		prev, known := n.seen[id]
		n.seen[id] = hotloopSeen{state: state, lastChanged: lc}

		send := false
		switch {
		case picture && !n.started:
			send = n.initial
		case !known:
			send = true
		default:
			// An attribute that moved is not a state that changed.
			send = prev.state != state || prev.lastChanged != lc
		}
		if !send {
			continue
		}
		m := engine.NewMsg()
		var before any
		if known {
			before = prev.state
		}
		hotloopStateOnMsg(m, tgt.describe(), st, n.numeric, before)
		out.Send(0, m)
	}
	if picture {
		n.started = true
	}
	return nil
}

// hotloopStateOnMsg puts an entity's state on a message the way every HotLoop
// node does: the id as the topic, the state as the payload, and the rest on
// msg.hotloop.
func hotloopStateOnMsg(m *engine.Msg, server string, st map[string]any, numeric bool, previous any) {
	id, _ := st["entity_id"].(string)
	m.SetTopic(id)
	m.SetPayload(hotloopValue(st["state"], numeric))
	info := map[string]any{
		"entity_id":  id,
		"state":      st["state"],
		"attributes": st["attributes"],
		"quality":    st["quality"],
	}
	for _, k := range []string{"last_changed", "last_updated", "last_reported"} {
		if v, ok := st[k]; ok {
			info[k] = v
		}
	}
	if previous != nil {
		info["previous"] = previous
	}
	m.Data["hotloop"] = hotloopInfo(server, info)
}

// ---------------------------------------------------------------------------
// hotloop state
// ---------------------------------------------------------------------------

type hotloopStateNode struct {
	cfgID   string
	entity  TypedValue
	numeric bool
	svc     node.Services
	target  hotloopTarget
}

func registerHotloopState() {
	node.MustRegister(node.Descriptor{
		Type:         "hotloop state",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "hotloop state",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own node. It reads one entity's state from HotLoop (GET /api/states/{entity_id}).",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "HotLoop", ConfigType: "hotloop-flow-hotloop", Required: true},
			{Name: "entity", Kind: node.PropTypedInput, Label: "Entity", Required: true, Default: "",
				TypeProp: "entityType", TypedInputTypes: hotloopStringTypes(), Placeholder: "sensor.tank_1_level"},
			{Name: "numeric", Kind: node.PropBool, Label: "Numbers as numbers", Default: false,
				Help: "HotLoop sends every state as text. On, a state that is a number becomes one."},
		},
		Help: "Reads an entity's state from HotLoop when a message arrives, and sends the message on with " +
			"msg.payload set to the state, msg.topic to the entity id and msg.hotloop holding the attributes, the " +
			"quality and the timestamps.\n\n" +
			"An entity that doesn't exist, or is disabled, raises an error with HotLoop's own words, which say " +
			"which. A reading HotLoop doesn't trust comes through: look at msg.hotloop.quality.",
	}, newHotloopState)
}

func newHotloopState(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &hotloopStateNode{
		cfgID:   p.PropString("server", ""),
		entity:  ReadTypedValue(p.Raw, "entity", "entityType", node.TypeStr),
		numeric: p.PropBool("numeric", false),
		svc:     def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no HotLoop selected: pick the HotLoop to read from")
	}
	if n.entity.Type == node.TypeStr && strings.TrimSpace(n.entity.Value) == "" {
		return nil, fmt.Errorf("an entity is required, for example sensor.tank_1_level")
	}
	if err := n.entity.Check(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *hotloopStateNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

// hotloopID evaluates a field that names one thing and makes sure it did.
func hotloopID(tv TypedValue, ec EvalContext, what string) (string, error) {
	v, ok, err := tv.Eval(ec)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	s, isStr := v.(string)
	if !ok || v == nil || !isStr || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s resolved to nothing. Check the %s field, which reads %s %q",
			what, what, tv.Type, tv.Value)
	}
	return strings.TrimSpace(s), nil
}

func (n *hotloopStateNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	id, err := hotloopID(n.entity, ec, "entity")
	if err != nil {
		return n.fail(out, err)
	}
	var st map[string]any
	server := "stand-in"
	if si := node.StandInOf(n.svc); si != nil {
		reply, err := si.Call("hotloop", map[string]any{"op": "state", "entity_id": id})
		if err != nil {
			return n.fail(out, fmt.Errorf("reading %s: %w", id, err))
		}
		st = reply
		if st == nil {
			return n.fail(out, fmt.Errorf("reading %s: the test scripted no state for it", id))
		}
		if _, ok := st["entity_id"]; !ok {
			st = map[string]any{"entity_id": id, "state": st["state"], "attributes": st["attributes"], "quality": st["quality"]}
		}
	} else {
		tgt, err := hotloopTarget0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		server = tgt.describe()
		status, body, err := tgt.call(ctx, http.MethodGet, "/api/states/"+hotloopPathEscape(id), nil)
		if err != nil {
			return n.fail(out, err)
		}
		if status != http.StatusOK {
			return n.fail(out, fmt.Errorf("reading %s: %s (HTTP %d)", id, hotloopErrorText(body), status))
		}
		st, err = hotloopObject(body)
		if err != nil {
			return n.fail(out, fmt.Errorf("reading %s: %w", id, err))
		}
	}
	hotloopStateOnMsg(m, server, st, n.numeric, nil)
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(fmt.Sprint(st["state"]), 32)})
	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// hotloop script
// ---------------------------------------------------------------------------

type hotloopScriptNode struct {
	id     string
	cfgID  string
	script TypedValue
	fields TypedValue
	wait   bool
	reason string
	svc    node.Services
	target hotloopTarget
}

func registerHotloopScript() {
	node.MustRegister(node.Descriptor{
		Type:         "hotloop script",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "hotloop script",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own node. It runs a HotLoop script (POST /api/scripts/{id}/run).",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "HotLoop", ConfigType: "hotloop-flow-hotloop", Required: true},
			{Name: "script", Kind: node.PropTypedInput, Label: "Script", Required: true, Default: "",
				TypeProp: "scriptType", TypedInputTypes: hotloopStringTypes(), Placeholder: "cip_cycle",
				Help: "The script's id, as it is in HotLoop."},
			{Name: "fields", Kind: node.PropTypedInput, Label: "Fields", Default: "{}",
				TypeProp: "fieldsType", TypedInputTypes: hotloopObjectTypes(),
				Help: "An object with a value for each field the script takes, like {\"minutes\": 20}. HotLoop checks " +
					"every one and never moves a value to fit."},
			{Name: "wait", Kind: node.PropBool, Label: "Wait for the run to end", Default: true,
				Help: "On, the message is sent on when the run has ended, and a run that fails raises an error. " +
					"Off, it is sent on as soon as the run has started."},
			{Name: "reason", Kind: node.PropString, Label: "Reason",
				Help: "Added after flow:<node id> in what HotLoop records beside the run."},
		},
		Help: "Runs a HotLoop script when a message arrives. HotLoop starts it as the account on the HotLoop " +
			"connection, and every write the script makes goes through HotLoop's write gate, which can refuse it. " +
			"HotLoop records the run with the reason flow:<node id>.\n\n" +
			"The message is sent on with msg.payload set to the run: its status, its fields and, when the node waited, " +
			"the trace of every step. A script HotLoop won't start (busy, a field out of range, a script that doesn't " +
			"exist), or a run that ends in anything but succeeded, raises an error with HotLoop's reason.",
	}, newHotloopScript)
}

func newHotloopScript(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &hotloopScriptNode{
		id:     p.ID,
		cfgID:  p.PropString("server", ""),
		script: ReadTypedValue(p.Raw, "script", "scriptType", node.TypeStr),
		fields: ReadTypedValue(p.Raw, "fields", "fieldsType", node.TypeJSON),
		wait:   p.PropBool("wait", true),
		reason: p.PropString("reason", ""),
		svc:    def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no HotLoop selected: pick the HotLoop that has the script")
	}
	if n.script.Type == node.TypeStr && strings.TrimSpace(n.script.Value) == "" {
		return nil, fmt.Errorf("a script is required: the id of the HotLoop script to run, like cip_cycle")
	}
	if strings.TrimSpace(n.fields.Value) == "" {
		n.fields = TypedValue{Type: node.TypeJSON, Value: "{}"}
	}
	for _, tv := range []TypedValue{n.script, n.fields} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *hotloopScriptNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

// hotloopObjectArg evaluates a field that has to be an object, or nothing.
func hotloopObjectArg(tv TypedValue, ec EvalContext, what string) (map[string]any, error) {
	v, ok, err := tv.Eval(ec)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if !ok || v == nil {
		return map[string]any{}, nil
	}
	obj, isObj := v.(map[string]any)
	if !isObj {
		return nil, fmt.Errorf("%s has to be an object, like {\"minutes\": 20}, and it is %s. "+
			"Check the field, which reads %s %q", what, describeType(v), tv.Type, tv.Value)
	}
	return obj, nil
}

func (n *hotloopScriptNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	id, err := hotloopID(n.script, ec, "script")
	if err != nil {
		return n.fail(out, err)
	}
	fields, err := hotloopObjectArg(n.fields, ec, "fields")
	if err != nil {
		return n.fail(out, err)
	}
	reason := hotloopReason(n.id, n.reason)

	var run map[string]any
	server := "stand-in"
	if si := node.StandInOf(n.svc); si != nil {
		reply, err := si.Call("hotloop", map[string]any{"op": "script", "script": id, "fields": fields,
			"wait": n.wait, "reason": reason})
		if err != nil {
			return n.fail(out, fmt.Errorf("running %s: %w", id, err))
		}
		run = reply
		if run == nil {
			run = map[string]any{"scriptId": id, "status": "succeeded"}
		}
	} else {
		tgt, err := hotloopTarget0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		server = tgt.describe()
		status, body, err := tgt.call(ctx, http.MethodPost, "/api/scripts/"+hotloopPathEscape(id)+"/run",
			map[string]any{"fields": fields, "reason": reason, "wait": n.wait})
		if err != nil {
			return n.fail(out, err)
		}
		if status != http.StatusOK && status != http.StatusAccepted {
			return n.fail(out, fmt.Errorf("running %s: %s (HTTP %d)", id, hotloopErrorText(body), status))
		}
		obj, err := hotloopObject(body)
		if err != nil {
			return n.fail(out, fmt.Errorf("running %s: %w", id, err))
		}
		run, _ = obj["run"].(map[string]any)
		if run == nil {
			return n.fail(out, fmt.Errorf("running %s: HotLoop answered %d with no run in it", id, status))
		}
	}

	st, _ := run["status"].(string)
	if n.wait && st != "succeeded" {
		why := stringOr(run["error"], "")
		if why == "" {
			why = hotloopFailedStep(run)
		}
		if why != "" {
			why = ": " + why
		}
		return n.fail(out, fmt.Errorf("script %s ended %s%s", id, orUnknown(st), why))
	}
	m.SetPayload(run)
	m.Data["hotloop"] = hotloopInfo(server, map[string]any{"script": id, "run_id": run["id"], "status": st})
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(id+" "+orUnknown(st), 32)})
	out.Send(0, m)
	return nil
}

// hotloopFailedStep is what the first step that did not go through said, for a
// run that ended without saying why itself.
func hotloopFailedStep(run map[string]any) string {
	steps, _ := run["steps"].([]any)
	for _, s := range steps {
		sm, _ := s.(map[string]any)
		if e := stringOr(sm["error"], ""); e != "" {
			return fmt.Sprintf("step %v (%v): %s", sm["index"], sm["type"], e)
		}
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "with no status"
	}
	return s
}

// ---------------------------------------------------------------------------
// hotloop recipe
// ---------------------------------------------------------------------------

type hotloopRecipeNode struct {
	id      string
	cfgID   string
	recipe  TypedValue
	params  TypedValue
	version int
	reason  string
	svc     node.Services
	target  hotloopTarget
}

func registerHotloopRecipe() {
	node.MustRegister(node.Descriptor{
		Type:         "hotloop recipe",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "hotloop recipe",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own node. It applies a HotLoop recipe (POST /api/recipes/{id}/apply).",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "HotLoop", ConfigType: "hotloop-flow-hotloop", Required: true},
			{Name: "recipe", Kind: node.PropTypedInput, Label: "Recipe", Required: true, Default: "",
				TypeProp: "recipeType", TypedInputTypes: hotloopStringTypes(), Placeholder: "grade_a",
				Help: "The recipe's id, as it is in HotLoop."},
			{Name: "params", Kind: node.PropTypedInput, Label: "Parameters", Default: "{}",
				TypeProp: "paramsType", TypedInputTypes: hotloopObjectTypes(),
				Help: "An object with a value for each parameter the recipe takes, like {\"speed\": 250}."},
			{Name: "version", Kind: node.PropNumber, Label: "Version", Default: 0,
				Help: "0 applies the current version. Anything else applies that older version on purpose."},
			{Name: "reason", Kind: node.PropString, Label: "Reason",
				Help: "Added after flow:<node id> in what HotLoop records beside the application."},
		},
		Help: "Applies a HotLoop recipe when a message arrives. HotLoop checks the whole recipe first and refuses it " +
			"whole, writes every target in order through its write gate, and reads each value back from the device. " +
			"The application is recorded under the account on the HotLoop connection with the reason flow:<node id>.\n\n" +
			"The message is sent on only when every target was written, with msg.payload set to the application, " +
			"including whether each value was verified. A recipe HotLoop refuses, or one that stopped halfway, raises " +
			"an error with HotLoop's reason, which names the target. Nothing that was written is undone.",
	}, newHotloopRecipe)
}

func newHotloopRecipe(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &hotloopRecipeNode{
		id:      p.ID,
		cfgID:   p.PropString("server", ""),
		recipe:  ReadTypedValue(p.Raw, "recipe", "recipeType", node.TypeStr),
		params:  ReadTypedValue(p.Raw, "params", "paramsType", node.TypeJSON),
		version: p.PropInt("version", 0),
		reason:  p.PropString("reason", ""),
		svc:     def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no HotLoop selected: pick the HotLoop that has the recipe")
	}
	if n.recipe.Type == node.TypeStr && strings.TrimSpace(n.recipe.Value) == "" {
		return nil, fmt.Errorf("a recipe is required: the id of the HotLoop recipe to apply, like grade_a")
	}
	if n.version < 0 {
		return nil, fmt.Errorf("version %d is negative. Use 0 for the current version", n.version)
	}
	if strings.TrimSpace(n.params.Value) == "" {
		n.params = TypedValue{Type: node.TypeJSON, Value: "{}"}
	}
	for _, tv := range []TypedValue{n.recipe, n.params} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *hotloopRecipeNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *hotloopRecipeNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	id, err := hotloopID(n.recipe, ec, "recipe")
	if err != nil {
		return n.fail(out, err)
	}
	params, err := hotloopObjectArg(n.params, ec, "parameters")
	if err != nil {
		return n.fail(out, err)
	}
	reason := hotloopReason(n.id, n.reason)
	req := map[string]any{"params": params, "reason": reason}
	if n.version > 0 {
		req["version"] = n.version
	}

	var app map[string]any
	server := "stand-in"
	if si := node.StandInOf(n.svc); si != nil {
		sent := map[string]any{"op": "recipe", "recipe": id, "params": params, "reason": reason}
		if n.version > 0 {
			sent["version"] = float64(n.version)
		}
		reply, err := si.Call("hotloop", sent)
		if err != nil {
			return n.fail(out, fmt.Errorf("applying %s: %w", id, err))
		}
		app = reply
		if app == nil {
			app = map[string]any{"recipe_id": id, "status": "applied"}
		}
	} else {
		tgt, err := hotloopTarget0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		server = tgt.describe()
		status, body, err := tgt.call(ctx, http.MethodPost, "/api/recipes/"+hotloopPathEscape(id)+"/apply", req)
		if err != nil {
			return n.fail(out, err)
		}
		obj, derr := hotloopObject(body)
		if derr != nil {
			return n.fail(out, fmt.Errorf("applying %s: %s (HTTP %d)", id, hotloopErrorText(body), status))
		}
		app, _ = obj["application"].(map[string]any)
		if status != http.StatusOK {
			why := hotloopErrorText(body)
			if app != nil && stringOr(obj["error"], "") == "" {
				why = fmt.Sprintf("the application ended %s, and %s", orUnknown(stringOr(app["status"], "")), hotloopTargetSummary(app))
			}
			return n.fail(out, fmt.Errorf("applying %s: %s (HTTP %d)", id, why, status))
		}
		if app == nil {
			return n.fail(out, fmt.Errorf("applying %s: HotLoop answered 200 with no application in it", id))
		}
	}

	st, _ := app["status"].(string)
	if st != "applied" {
		return n.fail(out, fmt.Errorf("applying %s: the application ended %s, and %s", id, orUnknown(st), hotloopTargetSummary(app)))
	}
	m.SetPayload(app)
	m.Data["hotloop"] = hotloopInfo(server, map[string]any{"recipe": id, "application_id": app["id"], "status": st})
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(id+" applied", 32)})
	out.Send(0, m)
	return nil
}

// hotloopTargetSummary says how far an application got, in the application's
// own numbers.
func hotloopTargetSummary(app map[string]any) string {
	targets, _ := app["targets"].([]any)
	written := 0
	for _, t := range targets {
		if tm, _ := t.(map[string]any); tm != nil && tm["status"] == "written" {
			written++
		}
	}
	return fmt.Sprintf("%d of %d targets were written", written, len(targets))
}
