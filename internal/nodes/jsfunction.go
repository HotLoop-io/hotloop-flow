package nodes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/js"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

func init() {
	registerFunction()
}

// functionNode runs JavaScript against each message.
type functionNode struct {
	prog    *js.Program
	outputs int
	svc     node.Services

	// id and flow place the node for node.linkcall, which finds its Link In
	// the way a dynamic Link Call does.
	id    string
	flow  string
	clock node.Clock

	mu      sync.Mutex
	waiting map[string]*functionLinkWait
}

// functionLinkWait is one node.linkcall waiting for its return.
type functionLinkWait struct {
	deliver func(*engine.Msg, error)
	timer   node.Timer
}

func registerFunction() {
	node.MustRegister(node.Descriptor{
		Type:         "function",
		Category:     node.CategoryFunction,
		Color:        colorFunction,
		Icon:         "function",
		Inputs:       1,
		Outputs:      1,
		OutputsProp:  "outputs",
		PaletteLabel: "function",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Runs on goja, a JavaScript interpreter written in Go, rather than " +
				"Node's vm module. The language is ES2023; the Node standard library is " +
				"not present. require() and npm modules do not work and cannot be made " +
				"to without embedding Node. There is always a CPU time limit, which " +
				"Node-RED leaves optional and off, and it applies to every timer callback " +
				"too. setTimeout, setInterval and their clear functions work, on the node's " +
				"clock, and every one is cleared when the flow stops or is redeployed. Buffer " +
				"is there (alloc, from, concat, the read and write methods for integers and " +
				"floats in both byte orders, slice, toString and toJSON). A byte slice " +
				"in a message arrives as a byte array, so wrap it with Buffer.from. await " +
				"works at the top level of the body, a promise the body returns is waited " +
				"for, a promise the Setup code returns is settled before the first message " +
				"runs, and node.linkcall(target, msg, {timeout, clone}) calls a Link In by " +
				"id or name and resolves with what its Link Out in return mode sends back " +
				"(5 second wait unless the call says otherwise).",
			UnsupportedProps: []string{"libs", "require"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "func", Kind: node.PropJS, Label: "On message",
				Default: "return msg;"},
			{Name: "initialize", Kind: node.PropJS, Label: "On start",
				Help: "Runs once when the flow starts."},
			{Name: "finalize", Kind: node.PropJS, Label: "On stop",
				Help: "Runs once when the flow stops or is redeployed."},
			{Name: "outputs", Kind: node.PropNumber, Label: "Outputs", Default: 1},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 5,
				Help: "A hard ceiling. An infinite loop is stopped rather than holding " +
					"a runtime goroutine forever."},
		},
		Help: "Runs JavaScript against each message. Return a message to send it on, " +
			"an array to address several outputs, or nothing to stop it here.\n\n" +
			"Available: msg, node (send, done, error, status, log, warn, debug, linkcall), " +
			"context, flow, global, env and util. Not available: require, npm modules, " +
			"the Node standard library.",
	}, newFunction)
}

func newFunction(def *node.Definition) (node.Node, error) {
	body := def.Node.PropString("func", "")
	if body == "" {
		body = "return msg;"
	}

	outputs := def.Node.PropInt("outputs", 1)
	if outputs < 0 {
		return nil, fmt.Errorf("outputs must not be negative, got %d", outputs)
	}
	// A function with zero declared outputs is a terminal node; keep one slot
	// so node.send() from it is a no-op rather than an index panic.
	slots := outputs
	if slots < 1 {
		slots = 1
	}

	timeout := time.Duration(def.Node.PropInt("timeout", 5)) * time.Second

	// Compiled once at deploy. goja's parser is the expensive part, and a flow
	// running a few thousand messages a second would otherwise spend most of
	// its time re-parsing the same source.
	name := def.Node.Name
	if name == "" {
		name = "function:" + def.Node.ID
	}
	prog, err := js.Compile(
		name,
		body,
		def.Node.PropString("initialize", ""),
		def.Node.PropString("finalize", ""),
		js.Limits{Timeout: timeout},
	)
	if err != nil {
		return nil, err
	}

	n := &functionNode{
		prog: prog, outputs: slots, svc: def.Services,
		id: def.Node.ID, flow: def.Node.Z, clock: node.ClockOf(def.Services),
		waiting: map[string]*functionLinkWait{},
	}
	if prog.UsesPromises() {
		Links.registerCall(n.id, n)
	}
	return n, nil
}

// linkCall is node.linkcall: send a copy of the message to a Link In and hand
// the message a Link Out in return mode sends back to deliver. Node-RED's
// RED.nodes.linkcallTargets finds the target the same way a dynamic Link Call
// does, by id or by name.
func (n *functionNode) linkCall(target string, m *engine.Msg, timeout time.Duration, deliver func(*engine.Msg, error)) error {
	in, err := Links.target(n.flow, target, true)
	if err != nil {
		return err
	}
	var raw [14]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("generating a call id: %w", err)
	}
	callID := hex.EncodeToString(raw[:])
	stack, _ := m.Data[linkSourceKey].([]any)
	m.Data[linkSourceKey] = append(stack, map[string]any{"id": callID, "node": n.id})

	w := &functionLinkWait{deliver: deliver}
	n.mu.Lock()
	n.waiting[callID] = w
	w.timer = n.clock.AfterFunc(timeout, func() {
		n.mu.Lock()
		_, ok := n.waiting[callID]
		delete(n.waiting, callID)
		n.mu.Unlock()
		if ok {
			deliver(nil, errors.New("timeout"))
		}
	})
	n.mu.Unlock()

	if !in.deliver(m) {
		n.mu.Lock()
		delete(n.waiting, callID)
		w.timer.Stop()
		n.mu.Unlock()
		return fmt.Errorf("target link in node %q is not running", target)
	}
	return nil
}

// returned is the way back for a call this node made.
func (n *functionNode) returned(callID string, m *engine.Msg) {
	n.mu.Lock()
	w, ok := n.waiting[callID]
	delete(n.waiting, callID)
	n.mu.Unlock()
	if !ok {
		return
	}
	w.timer.Stop()
	w.deliver(m, nil)
}

func (n *functionNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	res, err := n.prog.Run(ctx, js.Sandbox{
		Msg:      m,
		Services: n.svc,
		Outputs:  n.outputs,
		LinkCall: n.linkCall,
	})

	// Emit whatever the function produced before reporting the failure. A
	// function that sends two messages and then throws should not have those
	// two messages discarded — they already happened.
	n.emit(res, out)

	if err != nil {
		return err
	}
	if res.Done {
		out.Done(m, nil)
	}
	return nil
}

// emit applies a result's side effects.
func (n *functionNode) emit(res *js.Result, out node.Emitter) {
	if res == nil {
		return
	}
	for _, l := range res.Logs {
		out.Log(l.Level, "%s", l.Message)
	}
	if res.Status != nil {
		out.Status(*res.Status)
	}
	for _, e := range res.Errors {
		out.Error(fmt.Errorf("%s", e), nil)
	}
	if len(res.ByPort) > 0 {
		out.SendAll(res.ByPort)
	}
}

// Start runs the setup code.
func (n *functionNode) Start(ctx context.Context, out node.Emitter) error {
	// A timer callback's node.send leaves through the emitter, from whatever
	// goroutine the clock fires it on.
	n.prog.SetAsync(func(res *js.Result) { n.emit(res, out) })
	res, err := n.prog.RunLifecycle(ctx, js.Sandbox{
		Services: n.svc,
		Outputs:  n.outputs,
		LinkCall: n.linkCall,
	}, "start")
	n.emit(res, out)
	if err != nil {
		return fmt.Errorf("setup code: %w", err)
	}
	return nil
}

// Close runs the cleanup code.
//
// Bounded by the context the runtime provides, so a cleanup function that hangs
// cannot wedge a deploy — which is the failure Node-RED only bounded in 0.17
// and still allows fifteen seconds of.
func (n *functionNode) Close(ctx context.Context, _ bool) error {
	defer n.prog.Stop()
	Links.unregisterCall(n.id, n)
	n.mu.Lock()
	for _, w := range n.waiting {
		w.timer.Stop()
	}
	n.mu.Unlock()
	res, err := n.prog.RunLifecycle(ctx, js.Sandbox{
		Services: n.svc,
		Outputs:  n.outputs,
	}, "stop")
	_ = res
	if err != nil {
		return fmt.Errorf("cleanup code: %w", err)
	}
	return nil
}
