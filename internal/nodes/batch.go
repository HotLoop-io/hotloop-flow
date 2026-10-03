// Ported from Node-RED 5.0.7 @node-red/nodes core/sequence/19-batch.js
// (Apache-2.0), Copyright JS Foundation and other contributors,
// http://js.foundation. Modified.

package nodes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Batch, ported from Node-RED 5's 19-batch.js.
//
// Three modes. By count: every N messages become a sequence, optionally
// overlapping, optionally ending early at the end of an incoming sequence. By
// interval: whatever arrived in each period becomes a sequence, which is how a
// burst of readings becomes one row a minute. And concatenate: complete
// sequences arriving under the listed topics are glued together in the order
// the topics are listed.
//
// A message keeps the msg.parts it came with, and gets this sequence's id,
// index and count written over it, which is what Node-RED does. The sequence id
// is the id of its first message.
//
// Two differences from Node-RED, both refusals of a configuration that cannot
// do anything useful: a count below one, which Node-RED silently reads as one,
// and an interval of zero, which Node-RED accepts and then holds messages
// forever.

type batchNode struct {
	mode          string // count, interval, concat
	count         int
	overlap       int
	honourParts   bool
	interval      time.Duration
	allowEmptySeq bool
	topics        []string

	mu      sync.Mutex
	pending []*engine.Msg
	// concat mode: per topic, the sequences in arrival order.
	byTopic map[string]*topicGroups

	out    node.Emitter
	ticker *time.Ticker
	stop   chan struct{}
}

type topicGroups struct {
	order  []string
	groups map[string]*concatGroup
}

type concatGroup struct {
	count int
	msgs  []*engine.Msg
}

func registerBatch() {
	node.MustRegister(node.Descriptor{
		Type:         "batch",
		Category:     node.CategorySequence,
		Color:        colorSequence,
		Icon:         "batch",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "batch",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Group by count with overlap and with the end of an incoming sequence " +
				"honoured, group by time interval with or without empty sequences, and " +
				"concatenate sequences by topic, with msg.reset, as Node-RED does. Two " +
				"configurations Node-RED accepts are refused: a count below one, which it " +
				"quietly reads as one, and an interval of zero, which holds every message " +
				"forever.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "mode", Kind: node.PropSelect, Label: "Mode", Default: "count", Options: []node.Option{
				{Value: "count", Label: "Group by message count"},
				{Value: "interval", Label: "Group by time interval"},
				{Value: "concat", Label: "Concatenate sequences"},
			}},
			{Name: "count", Kind: node.PropNumber, Label: "Messages per group", Default: 10},
			{Name: "overlap", Kind: node.PropNumber, Label: "Overlap", Default: 0},
			{Name: "honourParts", Kind: node.PropBool, Label: "End a group at the end of an incoming sequence"},
			{Name: "interval", Kind: node.PropNumber, Label: "Interval (seconds)", Default: 10},
			{Name: "allowEmptySequence", Kind: node.PropBool, Label: "Send an empty message when nothing arrived"},
			{Name: "topics", Kind: node.PropList, Label: "Topics", Fields: []node.Prop{
				{Name: "topic", Kind: node.PropString, Label: "Topic"},
			}},
		},
		Help: "Groups a stream of messages into sequences, ready for a Join node.",
	}, newBatch)
}

func newBatch(def *node.Definition) (node.Node, error) {
	n := &batchNode{
		mode:        orDefault(def.Node.PropString("mode", ""), "count"),
		honourParts: def.Node.PropBool("honourParts", false),
		byTopic:     map[string]*topicGroups{},
		stop:        make(chan struct{}),
	}
	switch n.mode {
	case "count":
		// Number(n.count || 1): absent or empty is one.
		n.count = int(jsNumberProp(def.Node.Raw["count"]))
		if raw, present := def.Node.Raw["count"]; !present || raw == "" || raw == nil {
			n.count = 1
		}
		n.overlap = int(jsNumberProp(def.Node.Raw["overlap"]))
		if n.count < 1 {
			return nil, fmt.Errorf("batch size must be at least 1, got %d", n.count)
		}
		if n.overlap < 0 || n.overlap >= n.count {
			return nil, fmt.Errorf("overlap must be between 0 and %d, got %d", n.count-1, n.overlap)
		}
	case "interval":
		secs := jsNumberProp(def.Node.Raw["interval"])
		if secs <= 0 {
			return nil, fmt.Errorf("an interval of %v seconds would never send a group", secs)
		}
		n.interval = time.Duration(secs * float64(time.Second))
		n.allowEmptySeq = def.Node.PropBool("allowEmptySequence", false)
	case "concat":
		if arr, ok := def.Node.Raw["topics"].([]any); ok {
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok {
					t, _ := m["topic"].(string)
					n.topics = append(n.topics, t)
				}
			}
		}
		if len(n.topics) == 0 {
			return nil, fmt.Errorf("concatenate mode needs at least one topic")
		}
	default:
		return nil, fmt.Errorf("unknown batch mode %q", n.mode)
	}
	return n, nil
}

// Start runs the interval timer, which is independent of messages arriving.
func (n *batchNode) Start(ctx context.Context, out node.Emitter) error {
	n.mu.Lock()
	n.out = out
	if n.mode == "interval" {
		n.ticker = time.NewTicker(n.interval)
	}
	ticker := n.ticker
	n.mu.Unlock()
	if ticker == nil {
		return nil
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-n.stop:
				return
			case <-ticker.C:
				n.flushInterval()
			}
		}
	}()
	return nil
}

func (n *batchNode) flushInterval() {
	n.mu.Lock()
	group := n.pending
	n.pending = nil
	out := n.out
	n.mu.Unlock()
	if len(group) > 0 {
		sendBatch(out, group, false)
		return
	}
	if n.allowEmptySeq {
		m := engine.NewMsg()
		m.SetPayload(nil)
		m.Data[engine.PropParts] = map[string]any{"id": engine.GenerateID(), "index": 0.0, "count": 1.0}
		out.Send(0, m)
	}
}

// sendBatch stamps a group as a sequence and sends it. The sequence id is the
// first message's id. Overlapping groups share messages, so those are copied.
func sendBatch(out node.Emitter, msgs []*engine.Msg, copyMsgs bool) {
	id := msgs[0].EnsureID()
	for i, m := range msgs {
		if copyMsgs {
			m = m.Clone()
		}
		parts, _ := m.Data[engine.PropParts].(map[string]any)
		next := make(map[string]any, len(parts)+3)
		for k, v := range parts {
			next[k] = v
		}
		next["id"] = id
		next["index"] = float64(i)
		next["count"] = float64(len(msgs))
		m.Data[engine.PropParts] = next
		out.Send(0, m)
	}
}

func (n *batchNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	switch n.mode {
	case "count":
		return n.receiveCount(m, out)
	case "interval":
		return n.receiveInterval(m)
	default:
		return n.receiveConcat(m, out)
	}
}

func (n *batchNode) receiveCount(m *engine.Msg, out node.Emitter) error {
	n.mu.Lock()
	if _, reset := m.Data["reset"]; reset {
		n.pending = nil
		n.mu.Unlock()
		return nil
	}
	eof := false
	if n.honourParts {
		if p, ok := m.Data[engine.PropParts].(map[string]any); ok {
			if jsNumberProp(p["index"])+1 == jsNumberProp(p["count"]) {
				eof = true
			}
		}
	}
	n.pending = append(n.pending, m)
	if len(n.pending) < n.count && !eof {
		n.mu.Unlock()
		return nil
	}
	group := n.pending
	n.pending = nil
	if n.overlap > 0 {
		n.pending = append([]*engine.Msg(nil), group[len(group)-n.overlap:]...)
	}
	n.mu.Unlock()
	sendBatch(out, group, n.overlap > 0)
	return nil
}

func (n *batchNode) receiveInterval(m *engine.Msg) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, reset := m.Data["reset"]; reset {
		// The period starts again from the reset, as Node-RED's does.
		n.pending = nil
		if n.ticker != nil {
			n.ticker.Reset(n.interval)
		}
		return nil
	}
	n.pending = append(n.pending, m)
	return nil
}

func (n *batchNode) receiveConcat(m *engine.Msg, out node.Emitter) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, reset := m.Data["reset"]; reset {
		n.byTopic = map[string]*topicGroups{}
		return nil
	}
	topic := m.Topic()
	listed := false
	for _, t := range n.topics {
		if t == topic {
			listed = true
			break
		}
	}
	if !listed {
		return nil
	}
	p, ok := m.Data[engine.PropParts].(map[string]any)
	_, hasID := p["id"]
	_, hasIndex := p["index"]
	_, hasCount := p["count"]
	if !ok || !hasID || !hasIndex || !hasCount {
		return fmt.Errorf("concatenating needs msg.parts with an id, an index and a count")
	}
	gid := fmt.Sprint(p["id"])
	tg, ok := n.byTopic[topic]
	if !ok {
		tg = &topicGroups{groups: map[string]*concatGroup{}}
		n.byTopic[topic] = tg
	}
	g, ok := tg.groups[gid]
	if !ok {
		g = &concatGroup{count: int(jsNumberProp(p["count"]))}
		tg.groups[gid] = g
		tg.order = append(tg.order, gid)
	}
	g.msgs = append(g.msgs, m)

	// Every listed topic needs its oldest sequence complete before anything
	// goes out.
	var all []*engine.Msg
	for _, t := range n.topics {
		tg, ok := n.byTopic[t]
		if !ok || len(tg.order) == 0 {
			return nil
		}
		first := tg.groups[tg.order[0]]
		if first.count != len(first.msgs) {
			return nil
		}
	}
	for _, t := range n.topics {
		tg := n.byTopic[t]
		first := tg.groups[tg.order[0]]
		all = append(all, first.msgs...)
		delete(tg.groups, tg.order[0])
		tg.order = tg.order[1:]
	}
	sendBatch(out, all, true)
	return nil
}

// Close stops the interval timer.
func (n *batchNode) Close(context.Context, bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ticker != nil {
		n.ticker.Stop()
	}
	select {
	case <-n.stop:
	default:
		close(n.stop)
	}
	return nil
}
