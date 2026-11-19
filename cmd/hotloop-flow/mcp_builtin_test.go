package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/instruments"
	"github.com/HotLoop-io/hotloop-flow/internal/mcphost"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Flow's own MCP tools against the whole application: a real runtime, the real
// deploy, the real deployment history and audit trail, and the Go SDK's client
// over HTTP. An agent deploys a flow through MCP, presses its inject node through
// MCP, and the flow runs.

type agentRT struct{ token string }

func (a agentRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}

func mcpSession(t *testing.T, e *e2e, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: e.URL + "/mcp", HTTPClient: &http.Client{Transport: agentRT{token}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (string, map[string]any, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	st, _ := res.StructuredContent.(map[string]any)
	return b.String(), st, res.IsError
}

const agentFlow = `[
 {"id":"t1","type":"tab","label":"Line 3"},
 {"id":"inj","type":"inject","z":"t1","x":100,"y":100,"props":[{"p":"payload","v":"start","vt":"str"}],"repeat":"","once":false,"wires":[["chg"]]},
 {"id":"chg","type":"change","z":"t1","x":300,"y":100,"rules":[{"t":"set","p":"payload","pt":"msg","to":"ran for the agent","tot":"str"}],"wires":[["dbg"]]},
 {"id":"dbg","type":"debug","z":"t1","x":500,"y":100,"active":true,"tosidebar":true,"complete":"payload","targetType":"msg","wires":[]}
]`

func TestAnAgentDeploysAFlowThroughMCPAndItRuns(t *testing.T) {
	app, rec := newApp(t)
	reg := mcphost.New("test")
	app.diagrams = diagram.NewManager(nil)
	t.Cleanup(app.diagrams.Close)
	switches, err := instruments.Open(filepath.Join(t.TempDir(), "instruments.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The station tools are served only where the stations settings are set.
	app.cfg.Stations = config.Stations{DashboardURL: "http://127.0.0.1:1", Tenant: "t", DashboardTokenFile: "dash.token", DeployTokenFile: "deploy.token"}
	e := serveAppWith(t, app, map[string][]string{
		"agent":  {"mcp.call", "flows.read", "flows.write", "inject.write", "settings.read", "nodes.read", "audit.read", "status.read", "auth.admin", "diagram.run", "instrument.write"},
		"viewer": {"mcp.call", "flows.read"},
		"nomcp":  {"flows.read", "flows.write"},
	}, func(d *api.Deps) {
		d.MCP = reg
		d.Diagrams = app.diagrams
		d.Instruments = switches
		d.Stations = app.stationsService()
		d.Failover = stubFailover{}
	})
	agent := mcpSession(t, e, e.login(t, "agent"))

	// The tools are offered to the agent, and the deploying ones are not offered to a
	// caller who may only read.
	tools, err := agent.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tl := range tools.Tools {
		have[tl.Name] = true
	}
	for _, want := range api.BuiltinToolNames() {
		if !have[want] {
			t.Errorf("built-in tool %s is not offered to an agent that may use it", want)
		}
	}
	viewer := mcpSession(t, e, e.login(t, "viewer"))
	vt, _ := viewer.ListTools(context.Background(), nil)
	for _, tl := range vt.Tools {
		if tl.Name == "deploy_flows" || tl.Name == "rollback_deployment" || tl.Name == "inject_node" {
			t.Errorf("a read-only caller is offered %s", tl.Name)
		}
	}
	if text, _, isErr := callTool(t, viewer, "deploy_flows", map[string]any{"flows": []any{}, "rev": ""}); !isErr || !strings.Contains(text, "flows.write") {
		t.Errorf("a read-only caller deploying: %v %s", isErr, text)
	}
	if _, err := mcpSessionErr(e, e.login(t, "nomcp")); err == nil {
		t.Error("a caller without mcp.call got a session")
	}

	// Nothing deployed yet.
	text, st, isErr := callTool(t, agent, "list_flows", nil)
	if isErr || len(st["flows"].([]any)) != 0 {
		t.Fatalf("list_flows on an empty instance: %v %s", isErr, text)
	}
	_, doc, _ := callTool(t, agent, "get_flows", nil)
	rev, _ := doc["rev"].(string)

	// Deploy through MCP, and the flow runs: the agent presses the inject node and
	// the debug node shows what the change node made.
	var flows []any
	if err := json.Unmarshal([]byte(agentFlow), &flows); err != nil {
		t.Fatal(err)
	}
	text, st, isErr = callTool(t, agent, "deploy_flows", map[string]any{"flows": flows, "rev": rev, "note": "line 3 from an agent"})
	if isErr || st["deployment"] != float64(1) {
		t.Fatalf("deploy_flows: %v %s", isErr, text)
	}
	if rt := app.currentRuntime(); rt == nil {
		t.Fatal("nothing is running after the deploy")
	}
	if _, _, isErr := callTool(t, agent, "inject_node", map[string]any{"id": "inj"}); isErr {
		t.Fatal("inject_node was refused")
	}
	if got := rec.debugFrom(t, "dbg"); got != "ran for the agent" {
		t.Fatalf("the deployed flow showed %q", got)
	}

	// What it deployed reads back the way the editor would read it.
	text, st, _ = callTool(t, agent, "list_flows", nil)
	if rows := st["flows"].([]any); len(rows) != 1 || rows[0].(map[string]any)["nodes"] != float64(3) || !strings.Contains(text, "Line 3") {
		t.Errorf("list_flows after the deploy: %s", text)
	}
	text, st, isErr = callTool(t, agent, "get_flow", map[string]any{"id": "t1"})
	if isErr || len(st["flow"].([]any)) != 4 {
		t.Errorf("get_flow: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "get_flow", map[string]any{"id": "nope"}); !isErr || !strings.Contains(text, "no flow") {
		t.Errorf("get_flow of a flow that is not there: %v %s", isErr, text)
	}

	// The deployment history and the audit trail name the agent, and the note is kept.
	if r, ok := app.history.Latest(); !ok || r.User != "agent" || r.Note != "line 3 from an agent" {
		t.Errorf("deployment record %+v", r)
	}
	if rows, _ := e.trail.Read(audit.Query{Event: audit.Deploy}); len(rows) != 1 || rows[0].User != "agent" {
		t.Errorf("audit rows for the deploy: %+v", rows)
	}
	if rows, _ := e.trail.Read(audit.Query{Event: audit.Inject}); len(rows) != 1 || rows[0].User != "agent" {
		t.Errorf("audit rows for the inject: %+v", rows)
	}
	if rows, _ := e.trail.Read(audit.Query{Event: "mcp.call"}); len(rows) < 6 {
		t.Errorf("the MCP calls are not all in the trail: %d rows", len(rows))
	}

	// A deploy over somebody else's change is refused by the route's revision check, in its words.
	if text, _, isErr := callTool(t, agent, "deploy_flows", map[string]any{"flows": []any{}, "rev": rev, "note": "stale"}); !isErr {
		t.Errorf("a stale revision was deployed over: %s", text)
	}
	// A document the deploy refuses changes nothing.
	_, doc, _ = callTool(t, agent, "get_flows", nil)
	rev, _ = doc["rev"].(string)
	if text, _, isErr := callTool(t, agent, "deploy_flows", map[string]any{
		"flows": []any{map[string]any{"id": "x", "type": "tab"}, map[string]any{"id": "y", "type": "inject", "z": "x", "wires": []any{[]any{"ghost"}}}},
		"rev":   rev, "mode": "nonsense"}); !isErr || !strings.Contains(text, "refused") {
		t.Errorf("a bad deploy mode: %v %s", isErr, text)
	}

	// Deployments are listed and an earlier one comes back.
	text, st, isErr = callTool(t, agent, "list_deployments", nil)
	if isErr || !strings.Contains(text, "line 3 from an agent") {
		t.Errorf("list_deployments: %v %s", isErr, text)
	}
	empty := []any{map[string]any{"id": "t2", "type": "tab", "label": "Empty"}}
	_, doc, _ = callTool(t, agent, "get_flows", nil)
	rev, _ = doc["rev"].(string)
	if text, st, isErr := callTool(t, agent, "deploy_flows", map[string]any{"flows": empty, "rev": rev, "note": "empty it"}); isErr || st["deployment"] != float64(2) {
		t.Fatalf("second deploy: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "rollback_deployment", map[string]any{"deployment": 1, "note": "put line 3 back"}); isErr {
		t.Fatalf("rollback_deployment: %s", text)
	}
	if text, st, _ := callTool(t, agent, "list_flows", nil); len(st["flows"].([]any)) != 1 || !strings.Contains(text, "Line 3") {
		t.Errorf("after the rollback: %s", text)
	}
	if r, _ := app.history.Latest(); r.User != "agent" || r.RollbackOf != 1 {
		t.Errorf("rollback record %+v", r)
	}

	// A flow's own tools are dropped when a runtime is torn down; Flow's are not.
	reg.Reset()
	again, _ := agent.ListTools(context.Background(), nil)
	n := 0
	for _, tl := range again.Tools {
		for _, want := range api.BuiltinToolNames() {
			if tl.Name == want {
				n++
			}
		}
	}
	if n != len(api.BuiltinToolNames()) {
		t.Errorf("Reset took Flow's own tools with it: %d of %d left", n, len(api.BuiltinToolNames()))
	}
}

func mcpSessionErr(e *e2e, token string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: e.URL + "/mcp", HTTPClient: &http.Client{Transport: agentRT{token}}, DisableStandaloneSSE: true,
	}, nil)
}
