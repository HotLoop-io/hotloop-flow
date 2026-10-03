package nodes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Link Call.
//
// A Link Call sends a message to a Link In, the flow behind that Link In does
// its work, and a Link Out in return mode sends the result back out of the Link
// Call that asked. It is how one piece of logic, "look up this batch", "check
// this interlock", gets written once and used from every tab, which plain link
// wires can't do because they have no way back.
//
// The way back is a stack on the message, msg._linkSource, exactly as Node-RED
// keeps it: each call pushes an entry naming itself, and each return pops one.
// So a call can make calls of its own, and an imported flow that already does
// that keeps working.

func init() {
	registerLinkCall()
}

// linkSourceKey is the message property carrying the call stack.
const linkSourceKey = "_linkSource"

// defaultLinkCallTimeout is how long a call waits for its return, from
// Node-RED.
const defaultLinkCallTimeout = 30 * time.Second

type linkCallNode struct {
	id      string
	flow    string
	target  string // the static target's id
	dynamic bool   // aim at msg.target instead
	timeout time.Duration

	mu      sync.Mutex
	out     node.Emitter
	waiting map[string]*linkCallWait
}

// linkCallWait is one call waiting for its return.
type linkCallWait struct {
	original *engine.Msg
	out      node.Emitter
	timer    *time.Timer
}

func registerLinkCall() {
	node.MustRegister(node.Descriptor{
		Type:         "link call",
		Category:     node.CategoryCommon,
		Color:        colorLink,
		Icon:         "link",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "link call",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatFull,
			Notes: "Calls a Link In and sends on whatever a Link Out in return mode sends " +
				"back, including calls made from inside a call. A static target is chosen by " +
				"id. In dynamic mode msg.target is an id or a name, looked up on the calling " +
				"flow first and then across every flow, never inside a subflow instance. A call " +
				"with no return within the timeout raises an error with the original message; " +
				"a return that turns up after that still goes out, as it does in Node-RED.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "linkType", Kind: node.PropSelect, Label: "Link type", Default: "static",
				Options: []node.Option{
					{Value: "static", Label: "Fixed target"},
					{Value: "dynamic", Label: "Dynamic target, from msg.target"},
				}},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 30},
		},
		Help: "Calls a flow that starts with a Link In and ends with a Link Out in return " +
			"mode, and sends on what comes back.",
	}, newLinkCall)
}

func newLinkCall(def *node.Definition) (node.Node, error) {
	n := &linkCallNode{
		id:      def.Node.ID,
		flow:    def.Node.Z,
		dynamic: def.Node.PropString("linkType", "static") == "dynamic",
		timeout: defaultLinkCallTimeout,
		waiting: map[string]*linkCallWait{},
	}
	switch links := def.Node.Raw["links"].(type) {
	case string:
		n.target = links
	case []any:
		if len(links) > 0 {
			n.target, _ = links[0].(string)
		}
	}
	if !n.dynamic && n.target == "" {
		return nil, fmt.Errorf("no link in node selected to call")
	}
	// Node-RED reads the timeout with parseFloat and falls back to 30 seconds
	// when that is not a number.
	if raw := strings.TrimSpace(fmt.Sprint(def.Node.Raw["timeout"])); def.Node.Raw["timeout"] != nil && raw != "" {
		if secs, err := strconv.ParseFloat(raw, 64); err == nil {
			n.timeout = time.Duration(secs * float64(time.Second))
		}
	}
	Links.registerCall(n.id, n)
	return n, nil
}

// Start keeps the emitter for a return that arrives after its call gave up
// waiting, which still has to come out of this node.
func (n *linkCallNode) Start(_ context.Context, out node.Emitter) error {
	n.mu.Lock()
	n.out = out
	n.mu.Unlock()
	return nil
}

func (n *linkCallNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	target := n.target
	if n.dynamic {
		t, ok := m.Data["target"].(string)
		if !ok || t == "" {
			return fmt.Errorf("dynamic link call: msg.target names no link in node")
		}
		target = t
	}
	in, err := Links.target(n.flow, target, n.dynamic)
	if err != nil {
		return err
	}

	var raw [14]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("generating a call id: %w", err)
	}
	callID := hex.EncodeToString(raw[:])

	// The copy kept for a timeout error is taken before this call goes on the
	// stack, which is what Node-RED reports with.
	stack, _ := m.Data[linkSourceKey].([]any)
	if stack == nil {
		stack = []any{}
	}
	m.Data[linkSourceKey] = stack
	wait := &linkCallWait{original: m.Clone(), out: out}
	m.Data[linkSourceKey] = append(stack, map[string]any{"id": callID, "node": n.id})

	n.mu.Lock()
	n.waiting[callID] = wait
	wait.timer = time.AfterFunc(n.timeout, func() { n.timedOut(callID) })
	n.mu.Unlock()

	if !in.deliver(m) {
		n.mu.Lock()
		delete(n.waiting, callID)
		wait.timer.Stop()
		n.mu.Unlock()
		return fmt.Errorf("target link in node %q is not running", target)
	}
	return nil
}

func (n *linkCallNode) timedOut(callID string) {
	n.mu.Lock()
	wait, ok := n.waiting[callID]
	delete(n.waiting, callID)
	n.mu.Unlock()
	if ok {
		wait.out.Error(fmt.Errorf("link call timed out after %s with no return", n.timeout), wait.original)
	}
}

// returned sends a message that came back through a Link Out in return mode.
func (n *linkCallNode) returned(callID string, m *engine.Msg) {
	n.mu.Lock()
	wait, ok := n.waiting[callID]
	delete(n.waiting, callID)
	out := n.out
	n.mu.Unlock()
	if ok {
		wait.timer.Stop()
		out = wait.out
	}
	if out != nil {
		out.Send(0, m)
	}
}

// Close stops the timers of calls still waiting.
func (n *linkCallNode) Close(context.Context, bool) error {
	Links.unregisterCall(n.id, n)
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, w := range n.waiting {
		w.timer.Stop()
	}
	return nil
}

// returnToCaller is a Link Out in return mode: pop the newest call off the
// message's stack and send the message out of the Link Call that made it.
func returnToCaller(m *engine.Msg) error {
	stack, _ := m.Data[linkSourceKey].([]any)
	if len(stack) == 0 {
		return fmt.Errorf("link out in return mode: this message was not sent by a link call, so there is nowhere to return it")
	}
	top, _ := stack[len(stack)-1].(map[string]any)
	stack = stack[:len(stack)-1]
	if len(stack) == 0 {
		delete(m.Data, linkSourceKey)
	} else {
		m.Data[linkSourceKey] = stack
	}
	callID, _ := top["id"].(string)
	nodeID, _ := top["node"].(string)
	call, ok := Links.lookupCall(nodeID)
	if !ok {
		return fmt.Errorf("link out in return mode: link call node %q is not running", nodeID)
	}
	call.returned(callID, m)
	return nil
}
