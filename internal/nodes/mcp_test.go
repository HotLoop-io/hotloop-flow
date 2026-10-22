package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/mcphost"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP nodes against the Go SDK's client and its server, over real HTTP. The
// tool and result nodes are started the way the runtime starts them, and what
// the flow in between does is a few lines of the test.

const mcpToolSchema = `{"type":"object","properties":{"line":{"type":"string"},"minutes":{"type":"integer","minimum":1}},"required":["line"],"additionalProperties":false}`

// mcpWorld is a registry, an HTTP server in front of it that knows two tokens,
// and a record of what was audited.
type mcpWorld struct {
	reg   *mcphost.Registry
	srv   *httptest.Server
	mu    sync.Mutex
	audit []mcphost.Event
}

func newMCPWorld(t *testing.T) *mcpWorld {
	t.Helper()
	w := &mcpWorld{reg: mcphost.New("test")}
	prev := MCPTools
	MCPTools = w.reg
	t.Cleanup(func() { MCPTools = prev })

	id := mcphost.Identity{Name: "token:agent", Allows: func(p string) bool { return p == "mcp.call" }}
	w.srv = httptest.NewServer(w.reg.Handler(mcphost.Options{
		Authenticate: func(r *http.Request) (mcphost.Identity, *mcphost.Refusal) {
			if r.Header.Get("Authorization") != "Bearer good" {
				return mcphost.Identity{}, &mcphost.Refusal{Status: 401, Message: "no good"}
			}
			return id, nil
		},
		Audit: func(e mcphost.Event) { w.mu.Lock(); w.audit = append(w.audit, e); w.mu.Unlock() },
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *mcpWorld) session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             w.srv.URL,
		HTTPClient:           &http.Client{Transport: tokenRT{"good"}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type tokenRT struct{ tok string }

func (b tokenRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func toolConfig(extra string) string {
	schema, _ := json.Marshal(mcpToolSchema)
	cfg := fmt.Sprintf(`{"toolName":"line_status","description":"Status of a line","schema":%s`, schema)
	if extra != "" {
		cfg += "," + extra
	}
	return cfg + "}"
}

// theFlow answers every call that comes out of the tool node by running it
// through the given nodes in turn, the way wires would.
func theFlow(t *testing.T, e *testEmitter, steps func(m *engine.Msg)) {
	t.Helper()
	go func() {
		seen := 0
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			msgs := e.on(0)
			for ; seen < len(msgs); seen++ {
				steps(msgs[seen])
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
}

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestMCPToolServesAFlowAndTheResultNodeAnswersIt(t *testing.T) {
	w := newMCPWorld(t)
	tool := build(t, "mcp tool", toolConfig(""), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, tool, e)
	defer cancel()

	result := build(t, "mcp result", `{}`, newTestServices())
	var got *engine.Msg
	theFlow(t, e, func(m *engine.Msg) {
		got = m.Clone()
		args := m.Payload().(map[string]any)
		m.SetPayload(map[string]any{"line": args["line"], "running": true})
		if err := result.Receive(context.Background(), m, newTestEmitter()); err != nil {
			t.Errorf("result: %v", err)
		}
	})

	s := w.session(t)
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L3", "minutes": 5}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %q", err, resultText(res))
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc["line"] != "L3" || sc["running"] != true {
		t.Errorf("structured = %#v", res.StructuredContent)
	}
	if !strings.Contains(resultText(res), `"running":true`) {
		t.Errorf("text = %q, want the object's JSON", resultText(res))
	}

	args, _ := got.Payload().(map[string]any)
	if args["line"] != "L3" || args["minutes"] != float64(5) {
		t.Errorf("msg.payload = %#v, want the call's arguments", got.Payload())
	}
	if got.Topic() != "line_status" {
		t.Errorf("msg.topic = %q", got.Topic())
	}
	info, _ := got.Data["mcp"].(map[string]any)
	if info["caller"] != "token:agent" || info["tool"] != "line_status" || info["callId"] == "" {
		t.Errorf("msg.mcp = %#v", info)
	}
}

func TestMCPToolRefusesArgumentsThatDoNotFitBeforeTheFlowRuns(t *testing.T) {
	w := newMCPWorld(t)
	tool := build(t, "mcp tool", toolConfig(""), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, tool, e)
	defer cancel()

	s := w.session(t)
	for _, bad := range []map[string]any{{}, {"line": 3}, {"line": "L3", "minutes": 0}, {"line": "L3", "nope": 1}} {
		res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: bad})
		if err != nil || !res.IsError || !strings.Contains(resultText(res), "input schema") {
			t.Errorf("%v: err=%v isError=%v %q", bad, err, res != nil && res.IsError, resultText(res))
		}
	}
	if n := e.total(); n != 0 {
		t.Fatalf("%d messages reached the flow for arguments that do not fit", n)
	}
}

func TestMCPToolTimesOutWhenNothingAnswers(t *testing.T) {
	w := newMCPWorld(t)
	tool := build(t, "mcp tool", toolConfig(`"timeout":0.2`), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, tool, e)
	defer cancel()

	res, err := w.session(t).CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L3"}})
	if err != nil || !res.IsError || !strings.Contains(resultText(res), "did not answer") {
		t.Fatalf("err=%v %q", err, resultText(res))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.errs) != 1 || !strings.Contains(e.errs[0].Error(), "no mcp result node answered") {
		t.Fatalf("errors raised: %v", e.errs)
	}
}

func TestMCPResultFromACatchNodeIsAToolError(t *testing.T) {
	w := newMCPWorld(t)
	tool := build(t, "mcp tool", toolConfig(""), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, tool, e)
	defer cancel()
	result := build(t, "mcp result", `{}`, newTestServices())
	theFlow(t, e, func(m *engine.Msg) {
		m.Data["error"] = map[string]any{"message": "the PLC would not answer", "source": map[string]any{"id": "x"}}
		_ = result.Receive(context.Background(), m, newTestEmitter())
	})
	res, err := w.session(t).CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L3"}})
	if err != nil || !res.IsError || resultText(res) != "the PLC would not answer" {
		t.Fatalf("err=%v isError=%v %q", err, res != nil && res.IsError, resultText(res))
	}
}

func TestMCPResultNodeWithTheFailedBoxOnAnswersAnError(t *testing.T) {
	w := newMCPWorld(t)
	tool := build(t, "mcp tool", toolConfig(""), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, tool, e)
	defer cancel()
	result := build(t, "mcp result", `{"isError":true}`, newTestServices())
	theFlow(t, e, func(m *engine.Msg) {
		m.SetPayload("line 3 is down for changeover")
		_ = result.Receive(context.Background(), m, newTestEmitter())
	})
	res, _ := w.session(t).CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L3"}})
	if !res.IsError || resultText(res) != "line 3 is down for changeover" {
		t.Fatalf("isError=%v %q", res.IsError, resultText(res))
	}
}

func TestMCPResultPayloads(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload any
		text    string
		object  bool
		wantErr string
	}{
		{"text", "running", "running", false, ""},
		{"a number", 3.5, "3.5", false, ""},
		{"a list", []any{1.0, 2.0}, "[1,2]", false, ""},
		{"nothing", nil, "", false, ""},
		{"utf8 bytes", []byte("ok"), "ok", false, ""},
		{"an object", map[string]any{"a": 1.0}, `{"a":1}`, true, ""},
		{"binary", []byte{0xff, 0xfe, 0x00}, "", false, "binary"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := engine.NewMsg()
			m.SetPayload(c.payload)
			o, err := toolOutcome(m, false)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || o.Text != c.text || (o.Structured != nil) != c.object {
				t.Fatalf("outcome = %+v, err = %v", o, err)
			}
		})
	}
}

func TestMCPResultNodeRefusesAMessageThatIsNotAToolCall(t *testing.T) {
	result := build(t, "mcp result", `{}`, newTestServices())
	_, err := send(t, result, msg(t, `{"payload":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "msg.mcpCall") {
		t.Fatalf("err = %v", err)
	}
}

func TestMCPOnlyOneResultNodeMayAnswerACall(t *testing.T) {
	w := newMCPWorld(t)
	tool := build(t, "mcp tool", toolConfig(""), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, tool, e)
	defer cancel()
	result := build(t, "mcp result", `{}`, newTestServices())
	second := make(chan error, 1)
	theFlow(t, e, func(m *engine.Msg) {
		m.SetPayload("first")
		_ = result.Receive(context.Background(), m.Clone(), newTestEmitter())
		m.SetPayload("second")
		second <- result.Receive(context.Background(), m, newTestEmitter())
	})
	res, _ := w.session(t).CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L3"}})
	if resultText(res) != "first" {
		t.Fatalf("answered %q", resultText(res))
	}
	if err := <-second; err == nil || !strings.Contains(err.Error(), "already answered") {
		t.Fatalf("second answer: %v", err)
	}
}

func TestMCPToolNodesRefuseWhatCannotBeServed(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ cfg, want string }{
		{`{"schema":"{\"type\":\"object\"}"}`, "tool name is required"},
		{`{"toolName":"a b"}`, "tool name"},
		{`{"toolName":"a","schema":"{nope"}`, "not a JSON Schema"},
		{`{"toolName":"a","schema":"{\"type\":\"array\"}"}`, "object"},
		{`{"toolName":"a","schema":"{\"type\":\"object\",\"properties\":{\"x\":{\"type\":\"nonsense\"}}}"}`, "value must be one of"},
		{`{"toolName":"a","permission":"Not Valid"}`, "permission"},
	} {
		err := buildErr(t, "mcp tool", c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.cfg, err, c.want)
		}
	}
	// A schema written as an object in a flow file, which is how flows as code hold it.
	if err := buildErr(t, "mcp tool", `{"toolName":"a","schema":{"type":"object","properties":{"x":{"type":"string"}}}}`, svc); err != nil {
		t.Errorf("a schema given as an object was refused: %v", err)
	}
}

func TestMCPTwoNodesMayNotServeTheSameTool(t *testing.T) {
	newMCPWorld(t)
	a := build(t, "mcp tool", toolConfig(""), newTestServices())
	b := build(t, "mcp tool", toolConfig(""), newTestServices())
	_, cancel := startNode(t, a, newTestEmitter())
	defer cancel()
	err := b.(node.Starter).Start(context.Background(), newTestEmitter())
	if err == nil || !strings.Contains(err.Error(), "already served by node") {
		t.Fatalf("err = %v", err)
	}
}

func TestMCPToolIsGoneOnceItsNodeCloses(t *testing.T) {
	w := newMCPWorld(t)
	tool := build(t, "mcp tool", toolConfig(""), newTestServices())
	ctx, cancel := startNode(t, tool, newTestEmitter())
	defer cancel()
	if names := w.reg.Names(); len(names) != 1 {
		t.Fatalf("names = %v", names)
	}
	_ = ctx
	if err := tool.(node.Closer).Close(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if names := w.reg.Names(); len(names) != 0 {
		t.Fatalf("names after close = %v", names)
	}
	// And it can start again, which is what a redeploy does.
	if err := tool.(node.Starter).Start(context.Background(), newTestEmitter()); err != nil {
		t.Fatalf("restart: %v", err)
	}
}

func TestMCPToolReportsPendingCallsSoARedeployWaits(t *testing.T) {
	w := newMCPWorld(t)
	tool := build(t, "mcp tool", toolConfig(""), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, tool, e)
	defer cancel()
	result := build(t, "mcp result", `{}`, newTestServices())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = w.session(t).CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L3"}})
	}()
	if !waitForCount(e, 1, 5*time.Second) {
		t.Fatal("the call never reached the flow")
	}
	if n := tool.(node.Deferrer).Pending(); n != 1 {
		t.Fatalf("Pending = %d, want 1 while the call is open", n)
	}
	m := e.on(0)[0]
	m.SetPayload("done")
	_ = result.Receive(context.Background(), m, newTestEmitter())
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for tool.(node.Deferrer).Pending() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := tool.(node.Deferrer).Pending(); n != 0 {
		t.Fatalf("Pending = %d after the answer", n)
	}
}

func TestMCPToolAndResultUnderAFlowTestServeNothing(t *testing.T) {
	w := newMCPWorld(t)
	si := &recordingStandIn{}
	svc := newTestServices()
	svc.standIn = si
	tool := build(t, "mcp tool", toolConfig(""), svc)
	if err := tool.(node.Starter).Start(context.Background(), newTestEmitter()); err != nil {
		t.Fatal(err)
	}
	if names := w.reg.Names(); len(names) != 0 {
		t.Fatalf("a flow test registered %v with the real server", names)
	}
	result := build(t, "mcp result", `{}`, svc)
	m := engine.NewMsg()
	m.SetPayload(map[string]any{"ok": true})
	if _, err := send(t, result, m); err != nil {
		t.Fatal(err)
	}
	if len(si.calls) != 1 || si.calls[0]["kind"] != "mcp result" {
		t.Fatalf("stand-in calls: %#v", si.calls)
	}
	sent := si.calls[0]["sent"].(map[string]any)
	if sent["text"] != `{"ok":true}` || sent["isError"] != false {
		t.Errorf("sent = %#v", sent)
	}
}

// ---------------------------------------------------------------------------
// The client node
// ---------------------------------------------------------------------------

// clientServices has the connection under the name "srv", with a token.
func (w *mcpWorld) clientServices(t *testing.T, token string) *credServices {
	t.Helper()
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.creds["token"] = token
	svc.configs["srv"] = build(t, "hotloop-flow-mcp-server", fmt.Sprintf(`{"url":%q,"timeout":5}`, w.srv.URL), svc)
	t.Cleanup(func() { _ = svc.configs["srv"].(node.Closer).Close(context.Background(), false) })
	return svc
}

func (w *mcpWorld) serveTool(t *testing.T, name, schema string, fn mcphost.Func) {
	t.Helper()
	unbind, err := w.reg.Register("node-"+name, mcphost.ToolSpec{Name: name, InputSchema: json.RawMessage(schema)}, fn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unbind)
}

func TestMCPClientCallsAToolAndTheAnswerIsThePayload(t *testing.T) {
	w := newMCPWorld(t)
	w.serveTool(t, "add", `{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`,
		func(_ context.Context, c mcphost.Call) (mcphost.Outcome, error) {
			sum := c.Arguments["a"].(float64) + c.Arguments["b"].(float64)
			return mcphost.Outcome{Text: fmt.Sprint(sum), Structured: map[string]any{"sum": sum}}, nil
		})
	w.serveTool(t, "say", `{"type":"object"}`, func(context.Context, mcphost.Call) (mcphost.Outcome, error) {
		return mcphost.Outcome{Text: "hello"}, nil
	})

	svc := w.clientServices(t, "good")
	n := build(t, "mcp client", `{"server":"srv","tool":"add","arguments":"payload","argumentsType":"msg"}`, svc)
	e, err := send(t, n, msg(t, `{"payload":{"a":2,"b":3.5}}`))
	if err != nil {
		t.Fatal(err)
	}
	out := e.on(0)
	if len(out) != 1 {
		t.Fatalf("sent %d messages", len(out))
	}
	pl, _ := out[0].Payload().(map[string]any)
	if pl["sum"] != 5.5 {
		t.Errorf("payload = %#v, want the structured answer", out[0].Payload())
	}
	info, _ := out[0].Data["mcp"].(map[string]any)
	if info["tool"] != "add" || info["server"] != w.srv.URL || info["isError"] != false {
		t.Errorf("msg.mcp = %#v", info)
	}

	// A second call reuses the connection, and a text answer is the text.
	n2 := build(t, "mcp client", `{"server":"srv","tool":"say"}`, svc)
	e2, err := send(t, n2, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := e2.on(0)[0].Payload(); got != "hello" {
		t.Errorf("payload = %#v", got)
	}
}

func TestMCPClientRaisesAToolsErrorWithItsWords(t *testing.T) {
	w := newMCPWorld(t)
	w.serveTool(t, "fails", `{"type":"object"}`, func(context.Context, mcphost.Call) (mcphost.Outcome, error) {
		return mcphost.Outcome{Text: "the press is not ready", IsError: true}, nil
	})
	svc := w.clientServices(t, "good")
	n := build(t, "mcp client", `{"server":"srv","tool":"fails"}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err == nil || !strings.Contains(err.Error(), "the press is not ready") || !strings.Contains(err.Error(), "fails") {
		t.Fatalf("err = %v", err)
	}
	if e.total() != 0 {
		t.Fatalf("a failed call sent a message on")
	}
}

func TestMCPClientSendsTheTokenAndSaysWhenItIsRefused(t *testing.T) {
	w := newMCPWorld(t)
	w.serveTool(t, "say", `{"type":"object"}`, func(context.Context, mcphost.Call) (mcphost.Outcome, error) {
		return mcphost.Outcome{Text: "hello"}, nil
	})
	svc := w.clientServices(t, "wrong")
	n := build(t, "mcp client", `{"server":"srv","tool":"say"}`, svc)
	_, err := send(t, n, msg(t, `{}`))
	if err == nil || !strings.Contains(err.Error(), "could not connect") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong") && strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("the error repeats the token: %v", err)
	}
	// Once the server takes it, the same node works: a failed connection is not remembered.
	svc2 := w.clientServices(t, "good")
	n2 := build(t, "mcp client", `{"server":"srv","tool":"say"}`, svc2)
	if _, err := send(t, n2, msg(t, `{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestMCPClientListsTheTools(t *testing.T) {
	w := newMCPWorld(t)
	w.serveTool(t, "say", `{"type":"object"}`, func(context.Context, mcphost.Call) (mcphost.Outcome, error) {
		return mcphost.Outcome{Text: "hello"}, nil
	})
	n := build(t, "mcp client", `{"server":"srv","operation":"list"}`, w.clientServices(t, "good"))
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	list, _ := e.on(0)[0].Payload().([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "say" {
		t.Fatalf("payload = %#v", e.on(0)[0].Payload())
	}
}

func TestMCPClientSaysWhenTheServerIsNotThere(t *testing.T) {
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.configs["srv"] = build(t, "hotloop-flow-mcp-server", `{"url":"http://127.0.0.1:1/mcp","timeout":2}`, svc)
	n := build(t, "mcp client", `{"server":"srv","tool":"say"}`, svc)
	_, err := send(t, n, msg(t, `{}`))
	if err == nil || !strings.Contains(err.Error(), "could not connect to the MCP server at http://127.0.0.1:1/mcp") {
		t.Fatalf("err = %v", err)
	}
}

func TestMCPClientNodesRefuseWhatCannotWork(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ typ, cfg, want string }{
		{"hotloop-flow-mcp-server", `{}`, "url is required"},
		{"hotloop-flow-mcp-server", `{"url":"ftp://x"}`, "scheme must be http or https"},
		{"hotloop-flow-mcp-server", `{"url":"http://"}`, "has no host"},
		{"hotloop-flow-mcp-server", `{"url":"http://u:p@host/mcp"}`, "no user or fragment"},
		{"hotloop-flow-mcp-server", `{"url":"http://h/mcp","timeout":0}`, "more than zero"},
		{"mcp client", `{"tool":"x"}`, "no MCP server selected"},
		{"mcp client", `{"server":"srv"}`, "a tool is required"},
		{"mcp client", `{"server":"srv","tool":"x","operation":"nope"}`, "not one of call and list"},
	} {
		err := buildErr(t, c.typ, c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: got %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

func TestMCPClientNeverSendsTheTokenToARedirectTarget(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked = true
		}
	}))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.creds["token"] = "secret-token"
	svc.configs["srv"] = build(t, "hotloop-flow-mcp-server", fmt.Sprintf(`{"url":%q,"timeout":2}`, redirector.URL), svc)
	n := build(t, "mcp client", `{"server":"srv","tool":"say"}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err == nil {
		t.Fatal("a redirect was followed to a server")
	}
	if leaked {
		t.Fatal("the bearer token was sent to the redirect target")
	}
}
