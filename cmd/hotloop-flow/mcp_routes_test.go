package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/mcphost"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// The tools that stand for the rest of the admin API: each runs the route's own
// handler for the caller, asks for the permission the route asks for, and writes
// its audit row, through the whole application and the Go SDK's client.

var routeToolPerm = map[string]string{
	"get_settings": "settings.read", "list_nodes": "nodes.read", "list_plugins": "nodes.read",
	"list_audit": "audit.read", "get_deployment": "flows.read", "get_deployment_flows": "flows.read",
	"diff_deployments": "flows.read", "diff_pending": "flows.read", "export_flows": "flows.read",
	"check_flow_types": "flows.read", "get_tests": "flows.read", "put_tests": "flows.write",
	"add_test_cases": "flows.write", "run_tests": "flows.read", "test_from_debug": "flows.read",
	"list_traces": "flows.read", "reset_mfa": "auth.admin", "find_trace": "flows.read", "get_trace": "flows.read",
	"trace_to_test": "flows.read", "replay_trace": "flows.read", "get_runtime_stats": "status.read",
}

func TestEveryRouteToolIsOfferedOnlyWithTheRoutesPermission(t *testing.T) {
	app, _ := newApp(t)
	all := []string{"mcp.call", "flows.read", "flows.write", "inject.write", "settings.read", "nodes.read", "audit.read", "status.read", "auth.admin"}
	users := map[string][]string{"all": all}
	for _, p := range all[1:] {
		var without []string
		for _, q := range all {
			if q != p {
				without = append(without, q)
			}
		}
		users["no-"+p] = without
	}
	e := serveAppWith(t, app, users, func(d *api.Deps) { d.MCP = mcphost.New("test") })
	offered := func(user string) map[string]bool {
		s := mcpSession(t, e, e.login(t, user))
		l, err := s.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, tl := range l.Tools {
			m[tl.Name] = true
		}
		return m
	}
	full := offered("all")
	for name := range routeToolPerm {
		if !full[name] {
			t.Errorf("%s is not offered to a caller with every permission", name)
		}
	}
	for _, p := range all[1:] {
		got := offered("no-" + p)
		for name, need := range routeToolPerm {
			if got[name] == (need == p) {
				t.Errorf("without %s, %s offered=%v, wanted %v", p, name, got[name], need != p)
			}
		}
	}
	// A tool called past what the listing offers is refused in the route's words.
	s := mcpSession(t, e, e.login(t, "no-settings.read"))
	if text, _, isErr := callTool(t, s, "get_settings", nil); !isErr || !strings.Contains(text, "settings.read") {
		t.Errorf("get_settings without settings.read: %v %s", isErr, text)
	}
}

func TestRouteToolsDoWhatTheirRoutesDo(t *testing.T) {
	app, _ := newApp(t)
	e := serveAppWith(t, app, map[string][]string{
		"agent": {"mcp.call", "flows.read", "flows.write", "inject.write", "settings.read", "nodes.read", "audit.read", "status.read", "auth.admin"},
	}, func(d *api.Deps) {
		d.MCP = mcphost.New("test")
		suite := newTestSuite(app.cfg)
		d.Tests, d.Capture, d.Replay = suite, runtime.NewCapture(runtime.DefaultCaptureSize, runtime.DefaultCaptureMaxBytes), suite.Replay
	})
	agent := mcpSession(t, e, e.login(t, "agent"))

	for _, name := range []string{"get_settings", "list_nodes", "list_plugins", "export_flows", "get_tests"} {
		if text, _, isErr := callTool(t, agent, name, nil); isErr {
			t.Errorf("%s: %s", name, text)
		}
	}
	if text, st, _ := callTool(t, agent, "list_nodes", nil); !strings.Contains(text, "inject") || st == nil && !strings.Contains(text, "[") {
		t.Errorf("list_nodes does not show the palette: %.200s", text)
	}

	// Two deployments, then the detail, the flows at each and the diff between.
	_, doc, _ := callTool(t, agent, "get_flows", nil)
	rev, _ := doc["rev"].(string)
	var flows []any
	if err := json.Unmarshal([]byte(agentFlow), &flows); err != nil {
		t.Fatal(err)
	}
	if text, _, isErr := callTool(t, agent, "deploy_flows", map[string]any{"flows": flows, "rev": rev, "note": "one"}); isErr {
		t.Fatalf("deploy: %s", text)
	}
	_, doc, _ = callTool(t, agent, "get_flows", nil)
	rev, _ = doc["rev"].(string)
	changed := strings.Replace(agentFlow, "ran for the agent", "ran twice for the agent", 1)
	var flows2 []any
	_ = json.Unmarshal([]byte(changed), &flows2)

	// A pending diff is read only: nothing deploys.
	text, _, isErr := callTool(t, agent, "diff_pending", map[string]any{"body": flows2})
	if isErr || !strings.Contains(text, "chg") {
		t.Fatalf("diff_pending: %v %s", isErr, text)
	}
	if _, d, _ := callTool(t, agent, "get_flows", nil); d["rev"] != rev {
		t.Fatal("diff_pending changed what is deployed")
	}
	if text, _, isErr := callTool(t, agent, "check_flow_types", map[string]any{"body": flows2}); isErr {
		t.Errorf("check_flow_types: %s", text)
	}
	if text, _, isErr := callTool(t, agent, "deploy_flows", map[string]any{"flows": flows2, "rev": rev, "note": "two"}); isErr {
		t.Fatalf("deploy two: %s", text)
	}
	if text, st, isErr := callTool(t, agent, "get_deployment", map[string]any{"seq": 1}); isErr || st["note"] != "one" {
		t.Errorf("get_deployment: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "get_deployment_flows", map[string]any{"seq": 2}); isErr || !strings.Contains(text, "ran twice") {
		t.Errorf("get_deployment_flows: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "diff_deployments", map[string]any{"from": 1, "to": 2}); isErr || !strings.Contains(text, "chg") {
		t.Errorf("diff_deployments: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "get_deployment", map[string]any{"seq": 99}); !isErr {
		t.Errorf("a deployment that is not there was found: %s", text)
	}
	if text, _, isErr := callTool(t, agent, "get_deployment", nil); !isErr || !strings.Contains(text, "seq") {
		t.Errorf("get_deployment without a number: %v %s", isErr, text)
	}

	// The audit log says who deployed, and the tools' own calls.
	text, _, isErr = callTool(t, agent, "list_audit", map[string]any{"limit": 50})
	if isErr || !strings.Contains(text, "agent") {
		t.Errorf("list_audit: %v %.300s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "list_audit", nil); isErr {
		t.Errorf("list_audit with no limit: %s", text)
	}
	if text, _, isErr := callTool(t, agent, "get_runtime_stats", nil); isErr {
		t.Errorf("get_runtime_stats with flows running: %s", text)
	}

	// Tests: replace the suite, read it back, run it, add a case, and a document
	// that is not a suite is refused whole in the route's words.
	var suite any
	_ = json.Unmarshal([]byte(`{"tests":[{"name":"the change node sets the payload","inject":[{"node":"chg","msg":{"payload":"x"}}],"expect":[{"node":"dbg","msg":{"payload":"ran twice for the agent"}}]}]}`), &suite)
	if text, st, isErr := callTool(t, agent, "put_tests", map[string]any{"body": suite}); isErr || st["tests"] != float64(1) {
		t.Fatalf("put_tests: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "get_tests", nil); isErr || !strings.Contains(text, "the change node sets the payload") {
		t.Errorf("get_tests after put_tests: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "put_tests", map[string]any{"body": map[string]any{"nonsense": true}}); !isErr || !strings.Contains(text, "refused (400)") {
		t.Errorf("put_tests with nonsense: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "get_tests", nil); isErr || !strings.Contains(text, "the change node sets the payload") {
		t.Errorf("a refused put_tests changed the suite: %v %s", isErr, text)
	}
	var more any
	_ = json.Unmarshal([]byte(`{"tests":[{"name":"a second case","inject":[{"node":"chg","msg":{"payload":"y"}}],"expect":[{"node":"dbg","msg":{"payload":"ran twice for the agent"}}]}]}`), &more)
	if text, _, isErr := callTool(t, agent, "add_test_cases", map[string]any{"body": more}); isErr {
		t.Fatalf("add_test_cases: %s", text)
	}
	if text, _, _ := callTool(t, agent, "get_tests", nil); !strings.Contains(text, "a second case") || !strings.Contains(text, "the change node sets the payload") {
		t.Errorf("add_test_cases lost a case: %s", text)
	}
	// run_tests starts the test binary as the flow-test child, which a test
	// process cannot do, so this test does not run it. It is the route's own handler, run the same way as the others.
	text, _, _ = callTool(t, agent, "list_audit", map[string]any{"event": "tests.saved"})
	if !strings.Contains(text, "tests") {
		t.Errorf("saving the tests left no audit row: %s", text)
	}

	// A second-factor reset asks for auth.admin and answers in the route's words
	// when two-factor sign-in is not set up.
	if text, _, isErr := callTool(t, agent, "reset_mfa", map[string]any{"body": map[string]any{"username": "nobody"}}); !isErr || !strings.Contains(text, "refused") {
		t.Errorf("reset_mfa for an account with no second factor: %v %s", isErr, text)
	}

	// Traces: none yet, and a missing one is the route's 404.
	if text, _, isErr := callTool(t, agent, "list_traces", map[string]any{"limit": 5}); isErr && !strings.Contains(text, "404") {
		t.Errorf("list_traces: %s", text)
	}
	if text, _, isErr := callTool(t, agent, "get_trace", map[string]any{"id": "nope"}); !isErr {
		t.Errorf("get_trace for a trace that is not there: %s", text)
	}
}
