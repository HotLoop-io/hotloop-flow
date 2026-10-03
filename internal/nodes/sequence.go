package nodes

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/jsonata"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

func init() {
	registerSplit()
	registerJoin()
	registerSort()
	registerBatch()
}

// msg.parts is how Node-RED tracks a message sequence: Split stamps it, Join
// and Sort and Batch read it. Reproducing the exact field names matters,
// because a flow may pass a sequence through a Function node that inspects
// them, and because a sequence produced here has to be joinable by a
// Node-RED-authored Join node and the reverse.
type partsInfo struct {
	ID    string // groups messages belonging to one sequence
	Index int    // position within the sequence
	Count int    // total, when known
	Type  string // "array", "object", "string", "buffer"
	Key   string // object key, for object splits
	Len   int    // chunk length, for string and buffer splits
}

func (p partsInfo) toMap() map[string]any {
	m := map[string]any{
		"id":    p.ID,
		"index": float64(p.Index),
		"type":  p.Type,
	}
	if p.Count > 0 {
		m["count"] = float64(p.Count)
	}
	if p.Key != "" {
		m["key"] = p.Key
	}
	if p.Len > 0 {
		m["len"] = float64(p.Len)
	}
	return m
}

func readParts(m *engine.Msg) (partsInfo, bool) {
	raw, ok := m.Data[engine.PropParts].(map[string]any)
	if !ok {
		return partsInfo{}, false
	}
	p := partsInfo{}
	p.ID, _ = raw["id"].(string)
	p.Type, _ = raw["type"].(string)
	p.Key, _ = raw["key"].(string)
	if f, ok := raw["index"].(float64); ok {
		p.Index = int(f)
	}
	if f, ok := raw["count"].(float64); ok {
		p.Count = int(f)
	}
	if f, ok := raw["len"].(float64); ok {
		p.Len = int(f)
	}
	return p, p.ID != ""
}

// ---------------------------------------------------------------------------
// sort
// ---------------------------------------------------------------------------

type sortNode struct {
	order    string // ascending, descending
	asNumber bool
	target   string
	seq      bool // sort a message sequence rather than an array payload

	// seqKey is the message property a sequence is sorted on, and key the
	// JSONata expression that replaces it (or the element itself, for an
	// array) when the flow asks for one.
	seqKey string
	key    *jsonata.Expr

	mu    sync.Mutex
	group map[string][]*engine.Msg
}

func registerSort() {
	node.MustRegister(node.Descriptor{
		Type:         "sort",
		Category:     node.CategorySequence,
		Color:        colorSequence,
		Icon:         "sort",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "sort",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatFull,
			Notes: "Sorts an array property by its elements or by a JSONata key evaluated " +
				"against each element, and a message sequence by a property or a JSONata " +
				"key evaluated against each message.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "target", Kind: node.PropString, Label: "Sort", Default: "payload"},
			{Name: "seqKey", Kind: node.PropString, Label: "Key", Default: "payload",
				Help: "When sorting a sequence, the property of each message to sort on."},
			{Name: "order", Kind: node.PropSelect, Label: "Direction", Default: "ascending",
				Options: []node.Option{
					{Value: "ascending", Label: "Ascending"},
					{Value: "descending", Label: "Descending"},
				}},
			{Name: "as_num", Kind: node.PropBool, Label: "Compare as numbers"},
		},
		Help: "Sorts an array payload, or a sequence of messages produced by Split.",
	}, newSort)
}

func newSort(def *node.Definition) (node.Node, error) {
	n := &sortNode{
		order:    def.Node.PropString("order", "ascending"),
		asNumber: def.Node.PropBool("as_num", false),
		target:   orDefault(def.Node.PropString("target", ""), engine.PropPayload),
		seq:      def.Node.PropString("targetType", "msg") == "seq",
		seqKey:   orDefault(def.Node.PropString("seqKey", ""), engine.PropPayload),
		group:    map[string][]*engine.Msg{},
	}
	// The key is an expression when its type says so: msgKeyType for an
	// array, seqKeyType for a sequence, same as Node-RED.
	keyType, keySrc := def.Node.PropString("msgKeyType", ""), def.Node.PropString("msgKey", "")
	if n.seq {
		keyType, keySrc = def.Node.PropString("seqKeyType", ""), def.Node.PropString("seqKey", "")
	}
	if keyType == node.TypeJSONata {
		x, err := jsonata.Compile(keySrc, def.Services)
		if err != nil {
			return nil, fmt.Errorf("sort key: %w", err)
		}
		n.key = x
	}
	return n, nil
}

func (n *sortNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	if n.seq {
		return n.sortSequence(ctx, m, out)
	}

	value, ok, err := m.Get(n.target)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no property %q", n.target)
	}
	arr, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s is %T, which is not an array", n.target, value)
	}

	var cp []any
	if n.key != nil {
		// Each element is the expression's input, not the message.
		keys := make([]any, len(arr))
		for i, e := range arr {
			k, _, err := n.key.EvalValue(ctx, e, nil)
			if err != nil {
				return fmt.Errorf("sort key for element %d: %w", i, err)
			}
			keys[i] = k
		}
		cp = sortByKeys(arr, keys, n.less)
	} else {
		cp = append([]any(nil), arr...)
		sort.SliceStable(cp, func(i, j int) bool { return n.less(cp[i], cp[j]) })
	}

	if err := m.Set(n.target, cp); err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

func (n *sortNode) sortSequence(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	parts, ok := readParts(m)
	if !ok {
		return fmt.Errorf("sorting a sequence needs msg.parts, which this message does not carry")
	}

	n.mu.Lock()
	n.group[parts.ID] = append(n.group[parts.ID], m)
	batch := n.group[parts.ID]
	complete := parts.Count > 0 && len(batch) >= parts.Count
	if complete {
		delete(n.group, parts.ID)
	}
	n.mu.Unlock()

	if !complete {
		return nil
	}

	// Every key is worked out before anything moves: an expression that fails
	// on one message fails the batch, rather than leaving half of it sorted.
	keys := make([]any, len(batch))
	for i, bm := range batch {
		if n.key != nil {
			k, _, err := n.key.EvalMsg(ctx, bm, nil)
			if err != nil {
				return fmt.Errorf("sort key for message %d of the sequence: %w", i, err)
			}
			keys[i] = k
			continue
		}
		keys[i], _, _ = bm.Get(n.seqKey)
	}
	order := make([]int, len(batch))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return n.less(keys[order[i]], keys[order[j]]) })
	sorted := make([]*engine.Msg, len(batch))
	for i, idx := range order {
		sorted[i] = batch[idx]
	}
	batch = sorted

	// Renumber so a downstream Join reassembles them in the new order.
	for i, bm := range batch {
		p, _ := readParts(bm)
		p.Index = i
		bm.Data[engine.PropParts] = p.toMap()
		out.Send(0, bm)
	}
	return nil
}

// sortByKeys returns items in the order of their keys, stable for equal keys.
func sortByKeys(items, keys []any, less func(a, b any) bool) []any {
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return less(keys[order[i]], keys[order[j]]) })
	out := make([]any, len(items))
	for i, idx := range order {
		out[i] = items[idx]
	}
	return out
}

func (n *sortNode) less(a, b any) bool {
	asc := n.order != "descending"

	if n.asNumber {
		af, aOK := asFloat(a)
		bf, bOK := asFloat(b)
		if aOK && bOK {
			if asc {
				return af < bf
			}
			return af > bf
		}
	}

	if cmp, ok := compare(a, b); ok {
		if asc {
			return cmp < 0
		}
		return cmp > 0
	}

	// Not comparable: fall back to the string form so the sort is at least
	// deterministic rather than arbitrary.
	as, bs := fmt.Sprint(a), fmt.Sprint(b)
	if asc {
		return as < bs
	}
	return as > bs
}
