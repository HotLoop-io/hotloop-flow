package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/mcphost"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Flows as MCP tools.
//
// An "mcp tool" node declares a tool: a name, a description and an input schema.
// While the flow is deployed, Flow serves it at /mcp, and a call from an MCP
// client becomes a message that comes out of the node. An "mcp result" node on
// the same flow sends the answer back. The pair is the shape of http in and
// http response, for the same reason: the flow in between is whatever it needs
// to be.
//
// The wire, the authentication and the audit log are mcphost's. What a call is
// allowed to do is decided before the message is made, from the caller's token.

func init() {
	registerMCPTool()
	registerMCPResult()
}

// MCPTools is the process-wide set of tools the flows are serving. main installs
// it before any flow starts; the zero value serves nothing over the wire but
// still refuses a duplicate name, so a node can never register against a table
// nobody is serving.
var MCPTools = mcphost.New("")

// defaultToolTimeout is how long a call waits for an mcp result node before the
// client is told the flow did not answer. Node-RED has nothing like this node,
// so there is no behavior to match: a call that nothing answers would hold a
// connection open for as long as the client waits, and that is the failure the
// http in node already refuses.
const defaultToolTimeout = 30 * time.Second

// defaultToolSchema takes any object, which is the schema a node starts with.
const defaultToolSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

// pendingToolCall is the handle an mcp tool node puts on msg.mcpCall.
//
// Like http in's, it is a plain struct, which is what makes it survive a
// message clone: two branches of a flow hold the same call, and the once guard
// turns the second reply into a reported error instead of a double answer.
type pendingToolCall struct {
	done chan struct{}
	once sync.Once
	out  mcphost.Outcome
	// nodeID is the mcp tool node, for the error when nobody answers.
	nodeID string
}

// reply records the answer exactly once and says whether this caller did.
func (p *pendingToolCall) reply(o mcphost.Outcome) bool {
	replied := false
	p.once.Do(func() {
		replied = true
		p.out = o
		close(p.done)
	})
	return replied
}

// abandon releases the call without an answer: the client went away, or the flow
// is stopping. It reports false when an answer got there first.
func (p *pendingToolCall) abandon() bool {
	abandoned := false
	p.once.Do(func() {
		abandoned = true
		close(p.done)
	})
	return abandoned
}

// ---------------------------------------------------------------------------
// mcp tool
// ---------------------------------------------------------------------------

type mcpToolNode struct {
	nodeID  string
	spec    mcphost.ToolSpec
	timeout time.Duration
	standIn bool

	unbind func()

	mu      sync.Mutex
	pending int
}

func registerMCPTool() {
	node.MustRegister(node.Descriptor{
		Type:         "mcp tool",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "mcp tool",
		LabelProp:    "toolName",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own node. It serves a flow as a Model Context Protocol tool at /mcp, " +
				"for a caller authenticated like the admin API's, and it has no Node-RED counterpart.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "toolName", Kind: node.PropString, Label: "Tool name", Required: true,
				Placeholder: "get_line_status",
				Help: "What an MCP client calls the tool: letters, digits, underscores, hyphens and dots, up to 128. " +
					"Two nodes may not serve the same name."},
			{Name: "description", Kind: node.PropText, Label: "Description",
				Help: "What the tool does, written for the model that reads it. It is how an agent decides to call it."},
			{Name: "schema", Kind: node.PropJSON, Label: "Input schema", Default: defaultToolSchema, Required: true,
				Help: "A JSON Schema, 2020-12, whose root is an object. A call whose arguments do not fit it is " +
					"refused with the path of the field that does not, and never reaches the flow."},
			{Name: "permission", Kind: node.PropString, Label: "Extra permission", Placeholder: "mcp.line3",
				Help: "Every caller needs mcp.call. Name another permission here and a caller also needs that one, " +
					"and does not see this tool in the list without it. A token with mcp.* has any of the mcp. ones."},
			{Name: "timeout", Kind: node.PropNumber, Label: "Reply timeout (seconds)", Default: 30,
				Help: "How long a call waits for an mcp result node. Past it the client is told the flow did not answer."},
		},
		Help: "Serves this flow as an MCP tool. A call from an MCP client sends a message out of this node: " +
			"msg.payload is the call's arguments, msg.topic the tool name, and msg.mcp holds the caller's name and the " +
			"call's id. Every message must reach an mcp result node, or the client is told the flow did not answer " +
			"once the reply timeout passes.\n\n" +
			"A caller is authenticated the way the admin API's are, with a session token or an API token that has the " +
			"mcp.call permission. The call runs with that identity's rights, and is written to the audit log with the " +
			"tool's name and the caller's before this flow runs.",
	}, newMCPTool)
}

// schemaText reads the schema property, which the editor stores as text and a
// flow written as code may hold as an object.
func schemaText(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return defaultToolSchema, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return defaultToolSchema, nil
		}
		return t, nil
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "", fmt.Errorf("the input schema can't be written as JSON: %w", err)
		}
		return string(b), nil
	}
}

func newMCPTool(def *node.Definition) (node.Node, error) {
	p := def.Node
	schema, err := schemaText(p.Raw["schema"])
	if err != nil {
		return nil, err
	}
	n := &mcpToolNode{
		nodeID:  p.ID,
		timeout: defaultToolTimeout,
		standIn: node.StandInOf(def.Services) != nil,
		spec: mcphost.ToolSpec{
			Name:        strings.TrimSpace(p.PropString("toolName", "")),
			Description: strings.TrimSpace(p.PropString("description", "")),
			InputSchema: json.RawMessage(schema),
			Permission:  strings.TrimSpace(p.PropString("permission", "")),
		},
	}
	if secs := p.PropFloat("timeout", 0); secs > 0 {
		n.timeout = time.Duration(secs * float64(time.Second))
	}
	if n.spec.Name == "" {
		return nil, fmt.Errorf("a tool name is required, like get_line_status")
	}
	// Refused here, at deploy, so a schema that cannot be served is found by
	// whoever wrote it and not by the first agent to call.
	if _, err := mcphost.Compile(n.spec); err != nil {
		return nil, err
	}
	return n, nil
}

// Receive is unreachable in a normal flow, since the node has no input.
func (n *mcpToolNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *mcpToolNode) Start(ctx context.Context, out node.Emitter) error {
	if n.standIn {
		// A flow test serves nothing. What a client would have sent is whatever
		// the test injects here.
		return nil
	}
	unbind, err := MCPTools.Register(n.nodeID, n.spec, func(callCtx context.Context, c mcphost.Call) (mcphost.Outcome, error) {
		return n.serve(ctx, callCtx, c, out)
	})
	if err != nil {
		return err
	}
	n.unbind = unbind
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "serving " + truncate(n.spec.Name, 24)})
	return nil
}

// serve turns one call into a message and waits for the flow's answer. The
// goroutine stays here until it has one, because the call's connection does.
func (n *mcpToolNode) serve(flowCtx, callCtx context.Context, c mcphost.Call, out node.Emitter) (mcphost.Outcome, error) {
	pending := &pendingToolCall{done: make(chan struct{}), nodeID: n.nodeID}

	m := engine.NewMsg()
	m.SetTopic(c.Tool)
	m.SetPayload(c.Arguments)
	m.Data["mcp"] = map[string]any{"tool": c.Tool, "caller": c.Caller, "callId": c.ID}
	m.Data["mcpCall"] = pending

	n.mu.Lock()
	n.pending++
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.pending--
		n.mu.Unlock()
	}()

	out.Send(0, m)

	timer := time.NewTimer(n.timeout)
	defer timer.Stop()
	select {
	case <-pending.done:
	case <-timer.C:
		if pending.abandon() {
			err := fmt.Errorf("no mcp result node answered the call to %s within %s", c.Tool, n.timeout)
			out.Error(err, m)
			return mcphost.Outcome{
				Text:    fmt.Sprintf("the flow did not answer within %s", n.timeout),
				IsError: true,
			}, nil
		}
	case <-flowCtx.Done():
		if pending.abandon() {
			return mcphost.Outcome{Text: "the flow is restarting, so the call was not answered. Try it again.", IsError: true}, nil
		}
	case <-callCtx.Done():
		// The client gave up. The result node has to be told it lost the race
		// rather than finding a call nobody is waiting for.
		pending.abandon()
		return mcphost.Outcome{Text: "the call was cancelled", IsError: true}, nil
	}
	return pending.out, nil
}

// Pending reports calls still waiting for an answer, so a redeploy does not
// tear the graph down around a call that is halfway through a flow.
func (n *mcpToolNode) Pending() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pending
}

func (n *mcpToolNode) Close(context.Context, bool) error {
	if n.unbind != nil {
		n.unbind()
		n.unbind = nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// mcp result
// ---------------------------------------------------------------------------

type mcpResultNode struct {
	isError bool
	standIn node.StandIn
}

func registerMCPResult() {
	node.MustRegister(node.Descriptor{
		Type:         "mcp result",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "mcp result",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own node. It answers the call an mcp tool node started.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "isError", Kind: node.PropBool, Label: "The work failed", Default: false,
				Help: "On, the call is answered as a tool error, which tells the model it can look at the text and try " +
					"again. A message that comes from a Catch node is answered that way whatever this says."},
		},
		Help: "Answers the call an mcp tool node started, with msg.payload. Text goes back as it is, an object goes back " +
			"as an object and as its JSON text, and anything else as its JSON text. The message must still carry " +
			"msg.mcpCall, which means it has to come from the same flow rather than being rebuilt along the way.\n\n" +
			"A message from a Catch node, which has msg.error, is answered as a tool error with that error's text.",
	}, newMCPResult)
}

func newMCPResult(def *node.Definition) (node.Node, error) {
	return &mcpResultNode{
		isError: def.Node.PropBool("isError", false),
		standIn: node.StandInOf(def.Services),
	}, nil
}

// toolOutcome is a message's answer to the client.
func toolOutcome(m *engine.Msg, isError bool) (mcphost.Outcome, error) {
	o := mcphost.Outcome{IsError: isError}
	if e, ok := m.Data[engine.PropError].(map[string]any); ok {
		o.IsError = true
		if msg, _ := e["message"].(string); msg != "" {
			o.Text = msg
			return o, nil
		}
	}
	switch v := m.Payload().(type) {
	case nil:
		// A flow that has nothing to say still answers.
	case string:
		o.Text = v
	case []byte:
		if !utf8.Valid(v) {
			return o, fmt.Errorf("msg.payload is %d bytes of binary, which an MCP tool result can't carry as text. "+
				"Encode it first, with a base64 string for instance", len(v))
		}
		o.Text = string(v)
	case engine.ImmutableBytes:
		if !utf8.Valid(v) {
			return o, fmt.Errorf("msg.payload is %d bytes of binary, which an MCP tool result can't carry as text. "+
				"Encode it first, with a base64 string for instance", len(v))
		}
		o.Text = string(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return o, fmt.Errorf("msg.payload can't be written as JSON: %w", err)
		}
		o.Text = string(b)
		if obj, ok := v.(map[string]any); ok {
			o.Structured = obj
		}
	}
	return o, nil
}

func (n *mcpResultNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	pending, ok := m.Data["mcpCall"].(*pendingToolCall)
	if !ok && n.standIn == nil {
		return fmt.Errorf("the message carries no msg.mcpCall, so there is no tool call to answer; " +
			"it must come from an mcp tool node on this flow")
	}
	o, err := toolOutcome(m, n.isError)
	if err != nil {
		return err
	}
	if n.standIn != nil {
		sent := map[string]any{"isError": o.IsError, "text": o.Text}
		if o.Structured != nil {
			sent["structuredContent"] = o.Structured
		}
		_, err := n.standIn.Call("mcp result", sent)
		return err
	}
	if !pending.reply(o) {
		return fmt.Errorf("the call was already answered, or the client went away; " +
			"only one mcp result node may answer a given call")
	}
	return nil
}
