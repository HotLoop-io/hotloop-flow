package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Calling tools on other MCP servers.
//
// A flow that needs something an MCP server offers, HotLoop's own tools
// included, calls it with an "mcp client" node. The connection is a config node
// that holds the server's address and the bearer token to send, so every client
// node that points at it shares one session.
//
// The protocol is the Go SDK's, which opens with whichever revision of the
// specification the server speaks.

func init() {
	registerMCPServerConfig()
	registerMCPClient()
}

// mcpTarget is how the client nodes reach their connection config node.
type mcpTarget interface {
	callTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)
	listTools(ctx context.Context) ([]*mcp.Tool, error)
	describe() string
	timeoutFor() time.Duration
}

// mcpServerConn is the config node: one MCP server, one session, shared.
type mcpServerConn struct {
	endpoint string
	token    string
	timeout  time.Duration
	http     *http.Client

	mu      sync.Mutex
	client  *mcp.Client
	session *mcp.ClientSession
}

func registerMCPServerConfig() {
	node.MustRegister(node.Descriptor{
		Type:     "hotloop-flow-mcp-server",
		Category: node.CategoryConfig,
		Color:    colorNetwork,
		Icon:     "network",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own connection to a Model Context Protocol server over Streamable HTTP. " +
				"It sends a bearer token if it has one, connects on the first call and reconnects on the call after a failure.",
		},
		LabelProp: "name",
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "url", Kind: node.PropString, Label: "URL", Required: true,
				Placeholder: "http://hotloop.plant.local:8080/mcp",
				Help:        "The server's MCP endpoint, path included. HotLoop serves it at /mcp, and Flow serves its own at /mcp too."},
			{Name: "token", Kind: node.PropCredential, Label: "Bearer token",
				Help: "Sent as Authorization: Bearer on every request. For HotLoop it is the mcp-token in its release Secret. " +
					"Leave it empty for a server that takes none."},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 30,
				Help: "How long one call waits for the server. A tool that takes longer than this is reported as failed, " +
					"and it may still have run."},
		},
		Help: "One MCP server. Every MCP client node that points at it shares the connection.\n\n" +
			"The server decides what the token may do, and an MCP server that takes no token takes a call from anyone " +
			"who can reach it. Flow never sends the token anywhere but this address, and does not follow a redirect with it.",
	}, newMCPServerConn)
}

func newMCPServerConn(def *node.Definition) (node.Node, error) {
	raw := strings.TrimSpace(def.Node.PropString("url", ""))
	if raw == "" {
		return nil, fmt.Errorf("url is required: the server's MCP endpoint, like http://hotloop.plant.local:8080/mcp")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("url %q has no host", raw)
	}
	if u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("url %q should be just the endpoint, with no user or fragment. The token goes in the Bearer token field", raw)
	}
	secs := def.Node.PropFloat("timeout", 30)
	if secs <= 0 {
		return nil, fmt.Errorf("timeout has to be more than zero seconds")
	}
	tok, _ := def.Services.Credential("token")
	c := &mcpServerConn{
		endpoint: raw,
		token:    strings.TrimSpace(tok),
		timeout:  time.Duration(secs * float64(time.Second)),
	}
	c.http = &http.Client{
		Transport: bearerTransport{token: c.token, next: http.DefaultTransport},
		// A redirect is not followed. The token is for this address and no other.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// bearerTransport adds the token to every request.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token == "" {
		return b.next.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func (c *mcpServerConn) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (c *mcpServerConn) describe() string { return c.endpoint }

func (c *mcpServerConn) timeoutFor() time.Duration { return c.timeout }

// Close ends the session when the flow stops.
func (c *mcpServerConn) Close(context.Context, bool) error {
	c.mu.Lock()
	s := c.session
	c.session = nil
	c.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
	return nil
}

// open returns the live session, connecting first when there is none.
func (c *mcpServerConn) open(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		return c.session, nil
	}
	if c.client == nil {
		c.client = mcp.NewClient(&mcp.Implementation{Name: "hotloop-flow", Version: "flow"}, nil)
	}
	s, err := c.client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   c.endpoint,
		HTTPClient: c.http,
		// Nothing here listens for the server to speak first, so no standing
		// connection is held open for it.
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("could not connect to the MCP server at %s: %w", c.endpoint, scrubURLError(err))
	}
	c.session = s
	return s, nil
}

// drop forgets a session that failed, so the next call connects again. It only
// forgets the one it was given: another call may already have replaced it.
func (c *mcpServerConn) drop(s *mcp.ClientSession) {
	c.mu.Lock()
	if c.session == s {
		c.session = nil
	}
	c.mu.Unlock()
	_ = s.Close()
}

// failed handles an error from a request on a session. The server answering
// with a protocol error means the connection is fine. Anything else may mean it
// is not, and a connection that is not is not reused.
//
// A call is never repeated on a new connection here: a tool may move equipment,
// and an error does not say whether the server ran it. The next message
// connects again.
func (c *mcpServerConn) failed(s *mcp.ClientSession, err error) error {
	var je *jsonrpc.Error
	if !errors.As(err, &je) {
		c.drop(s)
	}
	return fmt.Errorf("the MCP server at %s: %w", c.endpoint, scrubURLError(err))
}

func (c *mcpServerConn) callTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	s, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, c.failed(s, err)
	}
	return res, nil
}

func (c *mcpServerConn) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	s, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	var out []*mcp.Tool
	for t, err := range s.Tools(ctx, nil) {
		if err != nil {
			return nil, c.failed(s, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// mcp client
// ---------------------------------------------------------------------------

type mcpClientNode struct {
	cfgID   string
	op      string
	tool    TypedValue
	args    TypedValue
	svc     node.Services
	target  mcpTarget
	standIn node.StandIn
}

func registerMCPClient() {
	node.MustRegister(node.Descriptor{
		Type:         "mcp client",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "mcp client",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own node. It calls a tool on a Model Context Protocol server, or lists the tools it has.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "MCP server", ConfigType: "hotloop-flow-mcp-server", Required: true},
			{Name: "operation", Kind: node.PropSelect, Label: "Operation", Default: "call", Options: []node.Option{
				{Value: "call", Label: "Call a tool"},
				{Value: "list", Label: "List the tools"},
			}},
			{Name: "tool", Kind: node.PropTypedInput, Label: "Tool", Default: "",
				TypeProp: "toolType", TypedInputTypes: hotloopStringTypes(), Placeholder: "get_plant_status",
				Help: "The tool's name, as the server lists it. Not used when listing."},
			{Name: "arguments", Kind: node.PropTypedInput, Label: "Arguments", Default: "{}",
				TypeProp: "argumentsType", TypedInputTypes: hotloopObjectTypes(),
				Help: "An object with a value for each argument the tool takes, like {\"deviceId\": \"press-01\"}. " +
					"The server checks them and says which one is wrong."},
		},
		Help: "Calls a tool on an MCP server when a message arrives, and sends the message on with the tool's answer in " +
			"msg.payload. An answer that is structured is sent as an object; text is sent as text, and several pieces of " +
			"text are joined with a newline. msg.mcp holds the server, the tool, and the answer's content exactly as the " +
			"server sent it.\n\n" +
			"A tool that reports an error raises one, with the server's words, so a Catch node sees what the model would " +
			"have seen. A call is never sent twice: if the connection fails halfway, the error says so and the next " +
			"message connects again.\n\n" +
			"List the tools returns them in msg.payload, each with its name, description and input schema.",
	}, newMCPClient)
}

func newMCPClient(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &mcpClientNode{
		cfgID:   p.PropString("server", ""),
		op:      p.PropString("operation", "call"),
		tool:    ReadTypedValue(p.Raw, "tool", "toolType", node.TypeStr),
		args:    ReadTypedValue(p.Raw, "arguments", "argumentsType", node.TypeJSON),
		svc:     def.Services,
		standIn: node.StandInOf(def.Services),
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no MCP server selected: pick the server to call")
	}
	switch n.op {
	case "call":
		if n.tool.Type == node.TypeStr && strings.TrimSpace(n.tool.Value) == "" {
			return nil, fmt.Errorf("a tool is required, like get_plant_status")
		}
	case "list":
	default:
		return nil, fmt.Errorf("operation %q is not one of call and list", n.op)
	}
	if err := n.tool.Check(); err != nil {
		return nil, err
	}
	if err := n.args.Check(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *mcpClientNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *mcpClientNode) resolveTarget() (mcpTarget, error) {
	if n.target != nil {
		return n.target, nil
	}
	cfg, ok := n.svc.ConfigNode(n.cfgID)
	if !ok {
		return nil, fmt.Errorf("the MCP server connection %s is not running. Open this node and pick an MCP server", n.cfgID)
	}
	t, ok := cfg.(mcpTarget)
	if !ok {
		return nil, fmt.Errorf("config node %s is not an MCP server connection", n.cfgID)
	}
	n.target = t
	return t, nil
}

// contentItems is the server's content as plain JSON values, for msg.mcp.
func contentItems(content []mcp.Content) []any {
	b, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	var items []any
	if json.Unmarshal(b, &items) != nil {
		return nil
	}
	return items
}

// toolPayload is what a tool's answer becomes on msg.payload: the structured
// answer when there is one, the text when everything it said was text, and the
// content as it came when it was anything else.
func toolPayload(structured any, items []any) any {
	if structured != nil {
		return structured
	}
	texts := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil || m["type"] != "text" {
			return items
		}
		s, _ := m["text"].(string)
		texts = append(texts, s)
	}
	return strings.Join(texts, "\n")
}

// itemsText is the text in a list of content items, for an error message.
func itemsText(items []any) string {
	var parts []string
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if s, ok := m["text"].(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
	}
	if len(parts) == 0 {
		return "no reason given"
	}
	return strings.Join(parts, " ")
}

func (n *mcpClientNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}

	var tool string
	var args map[string]any
	if n.op == "call" {
		var err error
		if tool, err = hotloopID(n.tool, ec, "tool"); err != nil {
			return n.fail(out, err)
		}
		v, ok, err := n.args.Eval(ec)
		if err != nil {
			return n.fail(out, fmt.Errorf("arguments: %w", err))
		}
		switch t := v.(type) {
		case nil:
			args = map[string]any{}
		case map[string]any:
			args = t
		default:
			if !ok {
				args = map[string]any{}
				break
			}
			return n.fail(out, fmt.Errorf("arguments has to be an object, and it resolved to %s", describeType(v)))
		}
	}

	if n.standIn != nil {
		return n.standInCall(m, out, tool, args)
	}

	tgt, err := n.resolveTarget()
	if err != nil {
		return n.fail(out, err)
	}
	cctx, cancel := context.WithTimeout(ctx, tgt.timeoutFor())
	defer cancel()

	if n.op == "list" {
		tools, err := tgt.listTools(cctx)
		if err != nil {
			return n.fail(out, err)
		}
		list := make([]any, 0, len(tools))
		for _, t := range tools {
			list = append(list, map[string]any{
				"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		m.SetPayload(list)
		m.Data["mcp"] = map[string]any{"server": tgt.describe(), "tools": len(list)}
		out.Status(node.Status{Fill: "green", Shape: "dot", Text: fmt.Sprintf("%d tools", len(list))})
		out.Send(0, m)
		return nil
	}

	res, err := tgt.callTool(cctx, tool, args)
	if err != nil {
		return n.fail(out, fmt.Errorf("calling %s: %w", tool, err))
	}
	return n.answer(m, out, tgt.describe(), tool, res.IsError, contentItems(res.Content), res.StructuredContent)
}

// answer puts a tool's answer on the message, or raises its error.
func (n *mcpClientNode) answer(m *engine.Msg, out node.Emitter, server, tool string, isError bool, items []any, structured any) error {
	if isError {
		return n.fail(out, fmt.Errorf("the tool %s on %s reported an error: %s", tool, server, itemsText(items)))
	}
	m.SetPayload(toolPayload(structured, items))
	info := map[string]any{"server": server, "tool": tool, "isError": false, "content": items}
	if structured != nil {
		info["structuredContent"] = structured
	}
	m.Data["mcp"] = info
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(tool, 32)})
	out.Send(0, m)
	return nil
}

// standInCall is the node under a flow test: it does its own work up to the wire
// and hands what it would have sent to the test, which says what came back.
func (n *mcpClientNode) standInCall(m *engine.Msg, out node.Emitter, tool string, args map[string]any) error {
	sent := map[string]any{"op": n.op}
	if n.op == "call" {
		sent["tool"] = tool
		sent["arguments"] = args
	}
	reply, err := n.standIn.Call("mcp", sent)
	if err != nil {
		return n.fail(out, fmt.Errorf("calling %s: %w", tool, err))
	}
	if reply == nil {
		return n.fail(out, fmt.Errorf("the test scripted no answer for the MCP %s", n.op))
	}
	if n.op == "list" {
		m.SetPayload(reply["tools"])
		m.Data["mcp"] = map[string]any{"server": "stand-in"}
		out.Send(0, m)
		return nil
	}
	isError, _ := reply["isError"].(bool)
	var items []any
	if s, ok := reply["text"].(string); ok {
		items = []any{map[string]any{"type": "text", "text": s}}
	}
	return n.answer(m, out, "stand-in", tool, isError, items, reply["structuredContent"])
}
