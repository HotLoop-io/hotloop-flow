package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/jsonata"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Join, ported from Node-RED 5's 17-split.js.
//
// Automatic mode undoes a Split from msg.parts: elements go back in their
// index order, a string is rejoined with the separator the Split recorded, an
// object gets its keys back, and an outer sequence comes back off the stack.
// Manual mode builds an array, string, buffer, object (keyed by a message
// property, msg.topic by default) or a merged object, and sends when it has
// enough messages, when a message says msg.complete, or when the timeout runs
// out with whatever it has. Reduce mode folds a sequence into one value with a
// JSONata expression, which is how a Split, some work on each piece and a Join
// add a column up without a Function node.
//
// One difference from Node-RED: an automatic Join handed a message with no
// msg.parts raises an error. Node-RED logs a warning, which reads the same as
// working until somebody looks at the log.

type joinNode struct {
	mode         string // auto, custom, reduce
	property     string
	propertyFull bool
	key          string
	timeout      time.Duration
	count        int
	countFromSeq bool // the count was left blank: take it from msg.parts
	joiner       any  // string or []byte
	joinerSet    bool
	build        string // array, object, string, buffer, merged
	accumulate   bool
	useParts     bool

	// Reduce mode.
	reduceExp   *jsonata.Expr
	reduceFixup *jsonata.Expr
	reduceInit  TypedValue
	reduceRight bool
	svc         node.Services

	mu       sync.Mutex
	inflight map[string]*joinGroup
	reducing map[string]*reduceGroup
}

type joinGroup struct {
	currentCount int
	items        []any
	object       map[string]any
	isObject     bool
	targetCount  int
	typ          string
	joinChar     any
	hasJoinChar  bool
	arrayLen     int
	msg          *engine.Msg
	out          node.Emitter
	timer        *time.Timer
	prop         string
}

type reduceGroup struct {
	count int
	msgs  []*engine.Msg
}

func registerJoin() {
	node.MustRegister(node.Descriptor{
		Type:         "join",
		Category:     node.CategorySequence,
		Color:        colorSequence,
		Icon:         "join",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "join",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Automatic, manual and reduce modes as Node-RED has them: placement by " +
				"msg.parts.index, the separator Split recorded, nested sequences, arrays, " +
				"strings, buffers, objects keyed by a message property, merged objects, " +
				"accumulate, msg.complete, msg.reset, msg.restartTimeout, the timeout, and a " +
				"reduce expression with $A, $I and $N and an optional fixup. One deliberate " +
				"difference: an automatic join given a message without msg.parts raises an " +
				"error rather than logging a warning.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "mode", Kind: node.PropSelect, Label: "Mode", Default: "auto", Options: []node.Option{
				{Value: "auto", Label: "Automatic — rejoin a split sequence"},
				{Value: "custom", Label: "Manual"},
				{Value: "reduce", Label: "Reduce a sequence"},
			}},
			{Name: "build", Kind: node.PropSelect, Label: "Combine into", Default: "array",
				Options: []node.Option{
					{Value: "array", Label: "Array"},
					{Value: "object", Label: "Key/value object"},
					{Value: "merged", Label: "Merged object"},
					{Value: "string", Label: "String"},
					{Value: "buffer", Label: "Buffer"},
				}},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
			{Name: "key", Kind: node.PropString, Label: "Key", Default: "topic",
				Help: "For a key/value object: the message property each value is keyed by."},
			{Name: "joiner", Kind: node.PropString, Label: "Join using", Default: `\n`},
			{Name: "count", Kind: node.PropNumber, Label: "After this many messages",
				Help: "Leave empty to take the count from msg.parts."},
			{Name: "timeout", Kind: node.PropNumber, Label: "After this many seconds",
				Help: "Send whatever has arrived when this runs out, counted from the first message."},
			{Name: "accumulate", Kind: node.PropBool, Label: "Keep adding to the result after it is sent"},
			{Name: "reduceExp", Kind: node.PropJSONata, Label: "Reduce expression",
				Help: "Evaluated for each message; $A is the result so far, $I the index, $N the count."},
			{Name: "reduceInit", Kind: node.PropTypedInput, Label: "Initial value", TypeProp: "reduceInitType"},
			{Name: "reduceFixup", Kind: node.PropJSONata, Label: "Fixup expression",
				Help: "Applied to the final result; $A is the result, $N the count."},
			{Name: "reduceRight", Kind: node.PropBool, Label: "Evaluate in reverse order"},
		},
		Help: "Combines a sequence of messages back into one. In automatic mode it " +
			"uses msg.parts, so it undoes exactly what Split did.",
	}, newJoin)
}

func newJoin(def *node.Definition) (node.Node, error) {
	n := &joinNode{
		mode:     orDefault(def.Node.PropString("mode", ""), "auto"),
		property: orDefault(def.Node.PropString("property", ""), engine.PropPayload),
		key:      orDefault(def.Node.PropString("key", ""), engine.PropTopic),
		build:    orDefault(def.Node.PropString("build", ""), "array"),
		svc:      def.Services,
		inflight: map[string]*joinGroup{},
		reducing: map[string]*reduceGroup{},
	}
	if def.Node.PropString("propertyType", "msg") == "full" {
		n.propertyFull = true
		n.property = engine.PropPayload
	}
	if n.mode != "auto" {
		n.timeout = time.Duration(jsNumberProp(def.Node.Raw["timeout"]) * float64(time.Second))
	}
	n.count = int(jsNumberProp(def.Node.Raw["count"]))
	// Node-RED takes the count from msg.parts only when the field was left
	// empty, not when it says 0.
	n.countFromSeq = def.Node.Raw["count"] == ""
	n.accumulate, _ = def.Node.Raw["accumulate"].(bool)
	n.useParts = true
	if v, present := def.Node.Raw["useparts"]; present {
		n.useParts, _ = v.(bool)
	}

	joiner := def.Node.PropString("joiner", "")
	switch def.Node.PropString("joinerType", "str") {
	case "bin":
		var arr []any
		if strings.TrimSpace(joiner) == "" {
			joiner = "[]"
		}
		if err := json.Unmarshal([]byte(joiner), &arr); err != nil {
			return nil, fmt.Errorf("join using: a byte sequence is a JSON array of byte values, got %q", joiner)
		}
		b := make([]byte, 0, len(arr))
		for _, v := range arr {
			f, _ := v.(float64)
			b = append(b, byte(int(f)))
		}
		n.joiner = b
	default:
		n.joiner = unescapeSequenceString(joiner)
	}
	n.joinerSet = true

	switch n.mode {
	case "auto", "custom":
	case "reduce":
		x, err := jsonata.Compile(def.Node.PropString("reduceExp", ""), def.Services)
		if err != nil {
			return nil, fmt.Errorf("reduce expression: %w", err)
		}
		n.reduceExp = x
		if fix := def.Node.PropString("reduceFixup", ""); fix != "" {
			if n.reduceFixup, err = jsonata.Compile(fix, def.Services); err != nil {
				return nil, fmt.Errorf("fixup expression: %w", err)
			}
		}
		n.reduceInit = ReadTypedValue(def.Node.Raw, "reduceInit", "reduceInitType", node.TypeStr)
		if _, present := def.Node.Raw["reduceInit"]; !present {
			n.reduceInit = TypedValue{}
		}
		if err := n.reduceInit.Check(); err != nil {
			return nil, fmt.Errorf("initial value: %w", err)
		}
		n.reduceRight = def.Node.PropBool("reduceRight", false)
	default:
		return nil, fmt.Errorf("unknown join mode %q", n.mode)
	}
	return n, nil
}

// jsNumberProp reads a property the way Number(x || 0) does.
func jsNumberProp(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return 0
}

func (n *joinNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	if n.mode == "reduce" {
		return n.reduce(ctx, m, out)
	}

	var property any
	propertyOK := true
	if n.propertyFull {
		property = m.Data
	} else {
		v, ok, err := m.Get(n.property)
		if err != nil {
			return err
		}
		property, propertyOK = v, ok
	}

	rawParts, hasParts := m.Data[engine.PropParts].(map[string]any)
	if n.mode == "auto" {
		if _, hasID := rawParts["id"]; !hasParts || !hasID {
			if _, reset := m.Data["reset"]; reset {
				n.mu.Lock()
				for _, g := range n.inflight {
					stopTimer(g)
				}
				n.inflight = map[string]*joinGroup{}
				n.mu.Unlock()
				return nil
			}
			return fmt.Errorf("automatic join needs msg.parts, which this message does not carry; " +
				"use manual mode or put a Split node upstream")
		}
	}
	if n.mode == "custom" && hasParts && !n.useParts {
		if inner, ok := rawParts["parts"]; ok {
			m.Data[engine.PropParts] = map[string]any{"parts": inner}
		} else {
			delete(m.Data, engine.PropParts)
		}
		rawParts, hasParts = m.Data[engine.PropParts].(map[string]any)
	}

	partID := "_"
	var payloadType string
	var propertyKey string
	hasKey := false
	targetCount := 0
	var joinChar any
	hasJoinChar := false
	arrayLen := 0
	propertyIndex := -1

	if n.mode == "auto" {
		partID = fmt.Sprint(rawParts["id"])
		payloadType, _ = rawParts["type"].(string)
		targetCount = int(jsNumberProp(rawParts["count"]))
		joinChar, hasJoinChar = rawParts["ch"]
		if k, ok := rawParts["key"]; ok && k != nil {
			propertyKey, hasKey = fmt.Sprint(k), true
		}
		arrayLen = int(jsNumberProp(rawParts["len"]))
		if f, ok := rawParts["index"].(float64); ok && !math.IsNaN(f) {
			propertyIndex = int(f)
		}
		prop := engine.PropPayload
		if p, ok := rawParts["property"].(string); ok && p != "" {
			prop = p
		}
		v, ok, err := m.Get(prop)
		if err != nil {
			return err
		}
		property, propertyOK = v, ok
	} else {
		payloadType = n.build
		targetCount = n.count
		joinChar, hasJoinChar = n.joiner, true
		if n.countFromSeq && hasParts {
			targetCount = int(jsNumberProp(rawParts["count"]))
			if id, ok := rawParts["id"]; ok {
				partID = fmt.Sprint(id)
			}
		}
		if n.build == "object" {
			if k, ok, _ := m.Get(n.key); ok && k != nil {
				propertyKey, hasKey = fmt.Sprint(k), true
			}
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if _, restart := m.Data["restartTimeout"]; restart {
		if g, ok := n.inflight[partID]; ok {
			stopTimer(g)
			n.armTimer(partID, g)
		}
	}
	if _, reset := m.Data["reset"]; reset {
		if g, ok := n.inflight[partID]; ok {
			stopTimer(g)
			delete(n.inflight, partID)
		}
		return nil
	}

	_, complete := m.Data["complete"]
	if payloadType == "object" && (!hasKey || propertyKey == "") {
		if n.mode == "auto" {
			return fmt.Errorf("this message has no msg.parts.key, so it cannot be added to an object")
		}
		if complete {
			if g, ok := n.inflight[partID]; ok {
				g.msg.Data["complete"] = m.Data["complete"]
				g.out = out
				n.completeSend(partID)
			}
			return nil
		}
		return fmt.Errorf("this message has no msg.%s, so it cannot be added to an object", n.key)
	}

	g, exists := n.inflight[partID]
	if !exists {
		g = &joinGroup{targetCount: targetCount, typ: payloadType, msg: m.Clone()}
		if payloadType == "object" || payloadType == "merged" {
			g.isObject, g.typ, g.object = true, "object", map[string]any{}
		}
		switch payloadType {
		case "string", "buffer":
			g.joinChar, g.hasJoinChar = joinChar, hasJoinChar
		case "array":
			g.arrayLen = arrayLen
		}
		if n.mode == "auto" {
			g.prop, _ = rawParts["property"].(string)
		} else {
			g.prop = n.property
		}
		n.inflight[partID] = g
		n.armTimer(partID, g)
	}

	if payloadType == "buffer" && propertyOK {
		switch property.(type) {
		case []byte, engine.ImmutableBytes, string, []any:
		default:
			return fmt.Errorf("a buffer join cannot take a value of type %T", property)
		}
	}

	var notMerged error
	switch payloadType {
	case "object":
		g.object[propertyKey] = property
		g.currentCount = len(g.object)
	case "merged":
		obj, isObj := property.(map[string]any)
		if !isObj {
			// Node-RED warns and carries on: the message still lays its
			// properties over the result and can still complete it.
			if !complete {
				notMerged = fmt.Errorf("a merged join can only merge objects, not %T", property)
			}
		} else {
			for k, v := range obj {
				if k != engine.PropMsgID {
					g.object[k] = v
				}
			}
			g.currentCount = len(g.object)
		}
	default:
		if propertyIndex >= 0 {
			for len(g.items) <= propertyIndex {
				g.items = append(g.items, nil)
			}
			// Counted only the first time an index is filled, so a
			// duplicate does not complete the sequence early.
			if g.items[propertyIndex] == nil {
				g.currentCount++
			}
			g.items[propertyIndex] = property
		} else if propertyOK {
			g.items = append(g.items, property)
			g.currentCount++
		}
	}

	// The message sent is the first one with every later message's
	// properties laid over it, as Object.assign does in Node-RED.
	for k, v := range m.Data {
		g.msg.Data[k] = v
	}
	g.out = out
	if hasParts {
		if g.targetCount == 0 {
			g.targetCount = int(jsNumberProp(rawParts["count"]))
		}
	}
	if (g.targetCount > 0 && g.currentCount >= g.targetCount) || complete {
		n.completeSend(partID)
	}
	return notMerged
}

func stopTimer(g *joinGroup) {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// armTimer starts the timeout on a group, when the node has one. Called with
// n.mu held.
func (n *joinNode) armTimer(partID string, g *joinGroup) {
	if n.timeout <= 0 {
		return
	}
	g.timer = time.AfterFunc(n.timeout, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		// The group may have completed, or been replaced, since the timer
		// was armed.
		if cur, ok := n.inflight[partID]; ok && cur == g {
			n.completeSend(partID)
		}
	})
}

// completeSend builds the joined value and sends it. Called with n.mu held.
func (n *joinNode) completeSend(partID string) {
	g := n.inflight[partID]
	stopTimer(g)
	_, completeFlag := g.msg.Data["complete"]
	if n.mode == "auto" || !n.accumulate || completeFlag {
		delete(n.inflight, partID)
	}

	var value any
	switch {
	case g.isObject:
		value = cloneValueShallow(g.object)
	case g.typ == "array" && g.arrayLen > 1:
		var flat []any
		for _, item := range g.items {
			if arr, ok := item.([]any); ok {
				flat = append(flat, arr...)
			} else {
				flat = append(flat, item)
			}
		}
		value = flat
	case g.typ == "buffer":
		value = joinBuffers(g)
	case g.typ == "string":
		sep := ""
		if g.hasJoinChar {
			sep = jsToString(g.joinChar)
		}
		strs := make([]string, len(g.items))
		for i, item := range g.items {
			strs[i] = jsToString(item)
		}
		value = strings.Join(strs, sep)
	default:
		value = append([]any(nil), g.items...)
	}

	res := g.msg
	if n.propertyFull {
		res = res.Clone()
	}
	prop := g.prop
	if prop == "" {
		prop = engine.PropPayload
	}
	if err := res.Set(prop, value); err != nil {
		g.out.Error(err, res)
		return
	}
	if p, ok := res.Data[engine.PropParts].(map[string]any); ok {
		if inner, nested := p["parts"]; nested {
			res.Data[engine.PropParts] = inner
		} else {
			delete(res.Data, engine.PropParts)
		}
	}
	delete(res.Data, "complete")
	out := res.Clone()
	if n.accumulate && n.mode != "auto" {
		// The group keeps going, and keeps its own copy of the message.
		g.msg = res
	}
	g.out.Send(0, out)
}

func cloneValueShallow(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// joinBuffers concatenates a buffer group, with the separator between pieces
// when there is one. Strings go in as their UTF-8 bytes and arrays as byte
// values, which is what Buffer.from does with them.
func joinBuffers(g *joinGroup) []byte {
	var sep []byte
	if g.hasJoinChar {
		sep = toBytes(g.joinChar)
	}
	var out []byte
	for i, item := range g.items {
		if i > 0 && g.hasJoinChar {
			out = append(out, sep...)
		}
		out = append(out, toBytes(item)...)
	}
	return out
}

func toBytes(v any) []byte {
	switch t := v.(type) {
	case []byte:
		return t
	case engine.ImmutableBytes:
		return t
	case string:
		return []byte(t)
	case []any:
		b := make([]byte, 0, len(t))
		for _, e := range t {
			f, _ := e.(float64)
			b = append(b, byte(int(f)))
		}
		return b
	}
	return nil
}

// jsToString renders a value the way JavaScript's String() does, which is
// what Array.join applies to each element: null and undefined become nothing,
// a number keeps its shortest form, an array joins its elements with commas, an
// object becomes [object Object] and a buffer its UTF-8 text.
func jsToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return jsNumberString(t)
	case int:
		return strconv.Itoa(t)
	case []byte:
		return string(t)
	case engine.ImmutableBytes:
		return string(t)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsToString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	}
	return fmt.Sprint(v)
}

// jsNumberString formats a number the way JavaScript prints it: plain
// notation from 1e-7 up to 1e21, exponent notation outside it.
func jsNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	if a := math.Abs(f); a >= 1e21 || a < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[0]
		exp = strings.TrimLeft(exp[1:], "0")
		return mant + "e" + string(sign) + exp
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// reduce collects a sequence and folds it into one value once it is complete.
// A message that is not part of a sequence passes straight through.
func (n *joinNode) reduce(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	parts, ok := m.Data[engine.PropParts].(map[string]any)
	if !ok {
		out.Send(0, m)
		return nil
	}
	gid := fmt.Sprint(parts["id"])
	n.mu.Lock()
	g, exists := n.reducing[gid]
	if !exists {
		g = &reduceGroup{}
		n.reducing[gid] = g
	}
	if c, has := parts["count"]; has && g.count == 0 {
		g.count = int(jsNumberProp(c))
	}
	g.msgs = append(g.msgs, m)
	done := g.count > 0 && len(g.msgs) == g.count
	if done {
		delete(n.reducing, gid)
	}
	n.mu.Unlock()
	if !done {
		return nil
	}

	index := func(m *engine.Msg) float64 {
		p, _ := m.Data[engine.PropParts].(map[string]any)
		return jsNumberProp(p["index"])
	}
	sort.SliceStable(g.msgs, func(i, j int) bool {
		if n.reduceRight {
			return index(g.msgs[i]) > index(g.msgs[j])
		}
		return index(g.msgs[i]) < index(g.msgs[j])
	})

	var acc any
	if n.reduceInit.Type != "" {
		v, ok, err := n.reduceInit.Eval(EvalContext{Msg: engine.NewMsg(), Services: n.svc, Ctx: ctx})
		if err != nil {
			return fmt.Errorf("initial value: %w", err)
		}
		if ok {
			acc = v
		}
	}
	last := g.msgs[len(g.msgs)-1]
	for _, gm := range g.msgs {
		v, _, err := n.reduceExp.EvalMsg(ctx, gm, map[string]any{
			"I": index(gm), "N": float64(g.count), "A": acc,
		})
		if err != nil {
			return err
		}
		acc = v
	}
	if n.reduceFixup != nil {
		v, _, err := n.reduceFixup.EvalValue(ctx, map[string]any{}, map[string]any{
			"N": float64(g.count), "A": acc,
		})
		if err != nil {
			return fmt.Errorf("fixup: %w", err)
		}
		acc = v
	}
	last.SetPayload(acc)
	out.Send(0, last)
	return nil
}

// Close stops the timeouts of groups still waiting.
func (n *joinNode) Close(context.Context, bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, g := range n.inflight {
		stopTimer(g)
	}
	return nil
}
