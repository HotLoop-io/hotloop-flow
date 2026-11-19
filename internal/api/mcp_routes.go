package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/mcphost"
)

// The rest of the admin API that a token may call, as tools. The seven tools in
// mcp_builtin.go cover the flows themselves; these cover what the editor shows
// around them: the settings, the palette, the audit log, deployment detail and
// diffs, the export, the flow tests, the message traces and the runtime's
// numbers. Flow's MCP reads and writes like every MCP in the HotLoop family, so
// nothing the admin API lets a token do is left out unless the route itself wants
// a person (see docs/MCP.md for those).
//
// Each one is the route's own handler, run for the caller the MCP endpoint
// authenticated, and asks for the permission the route asks for, so the same
// answers come back as from the REST API. A tool here is one row: nothing is
// reimplemented.

type routeTool struct {
	name, desc, perm string
	method, path     string
	// pathArgs name the {wildcards} in path, in order; each is a required string or
	// integer argument.
	pathArgs []string
	// query names optional arguments sent as the query string.
	query []string
	// body says the route takes a JSON document, sent as the argument "body".
	body    string
	handler func(*Server) http.HandlerFunc
}

var routeTools = []routeTool{
	{name: "get_settings", desc: "The settings the editor reads: the version, the admin root, the inbox capacity and overflow policy, whether sign-in, discovery and metrics are on. Secrets and the filesystem layout are not in it.",
		perm: PermSettings, method: "GET", path: "/settings", handler: func(s *Server) http.HandlerFunc { return s.handleSettings }},
	{name: "list_nodes", desc: "Every node type this build has, as the editor builds its palette and edit dialogs from it.",
		perm: PermNodesRead, method: "GET", path: "/nodes", handler: func(s *Server) http.HandlerFunc { return s.handleNodes }},
	{name: "list_plugins", desc: "The plugins that loaded, and the ones that were refused and why.",
		perm: PermNodesRead, method: "GET", path: "/plugins", handler: func(s *Server) http.HandlerFunc { return s.handlePlugins }},
	{name: "list_audit", desc: "The audit log: who deployed, rolled back, pressed an inject, saved tests or called a tool, and when. Narrow it with event, user and since (RFC 3339); limit is 1 to 10000.",
		perm: PermAuditRead, method: "GET", path: "/audit", query: []string{"event", "user", "since", "limit"}, handler: func(s *Server) http.HandlerFunc { return s.handleAudit }},
	{name: "get_deployment", desc: "One deployment record: who, when, the note, and what it changed. The number comes from list_deployments.",
		perm: PermFlowsRead, method: "GET", path: "/deployments/{seq}", pathArgs: []string{"seq"}, handler: func(s *Server) http.HandlerFunc { return s.handleGetDeployment }},
	{name: "get_deployment_flows", desc: "The whole flow set as it was at one deployment.",
		perm: PermFlowsRead, method: "GET", path: "/deployments/{seq}/flows", pathArgs: []string{"seq"}, handler: func(s *Server) http.HandlerFunc { return s.handleGetDeploymentFlows }},
	{name: "diff_deployments", desc: "What changed between two deployments, node by node, with a text summary.",
		perm: PermFlowsRead, method: "GET", path: "/deployments/{from}/diff/{to}", pathArgs: []string{"from", "to"}, handler: func(s *Server) http.HandlerFunc { return s.handleDiffDeployments }},
	{name: "diff_pending", desc: "What deploying a flow set would change against what is deployed now, without deploying it. The body is the flow document, as deploy_flows takes it. Nothing is changed.",
		perm: PermFlowsRead, method: "POST", path: "/flows/diff", body: "the flow document: an array of nodes, or an object with a flows array", handler: func(s *Server) http.HandlerFunc { return s.handleDiffPending }},
	{name: "export_flows", desc: "The deployed flow set as an export file, the way the editor's Export does it.",
		perm: PermFlowsRead, method: "GET", path: "/flows/export", handler: func(s *Server) http.HandlerFunc { return s.handleExport }},
	{name: "check_flow_types", desc: "Whether the wires of a flow set agree about what travels on them, with every problem named. The body is the flow document. Nothing is changed.",
		perm: PermFlowsRead, method: "POST", path: "/flows/types", body: "the flow document: an array of nodes, or an object with a flows array", handler: func(s *Server) http.HandlerFunc { return s.handleFlowTypes }},
	{name: "get_tests", desc: "The flow tests this instance keeps, as the suite document.",
		perm: PermFlowsRead, method: "GET", path: "/tests", handler: func(s *Server) http.HandlerFunc { return s.handleGetTests }},
	{name: "put_tests", desc: "REPLACE the whole flow test suite with the one you send. It is parsed first and refused whole if it does not parse. Read it with get_tests, change it, send it back.",
		perm: PermFlowsWrite, method: "PUT", path: "/tests", body: "the suite document, as get_tests returns it", handler: func(s *Server) http.HandlerFunc { return s.handlePutTests }},
	{name: "add_test_cases", desc: "Add test cases to the suite without replacing it. Cases whose names are already there are the route's to refuse or replace, in its own words.",
		perm: PermFlowsWrite, method: "POST", path: "/tests/cases", body: "a suite document holding only the cases to add", handler: func(s *Server) http.HandlerFunc { return s.handleAddTests }},
	{name: "run_tests", desc: "Run the flow tests against the deployed flows, or against a draft if flows is in the body, and say which passed. Optional body fields: flows, tests (a suite document as a string) and match (a regular expression on test names).",
		perm: PermFlowsRead, method: "POST", path: "/tests/run", body: "optional: {flows, tests, match}", handler: func(s *Server) http.HandlerFunc { return s.handleRunTests }},
	{name: "test_from_debug", desc: "Draft a test from a message a Debug node showed. It saves nothing: use add_test_cases to keep the draft. The body is {node, msgId}.",
		perm: PermFlowsRead, method: "POST", path: "/tests/from-debug", body: "{node: the Debug node's id, msgId: the message's id}", handler: func(s *Server) http.HandlerFunc { return s.handleTestFromDebug }},
	{name: "list_traces", desc: "The message traces Flow keeps, newest first: each is one message's path through the flows. limit is 1 to 500, 50 when left out.",
		perm: PermFlowsRead, method: "GET", path: "/traces", query: []string{"limit"}, handler: func(s *Server) http.HandlerFunc { return s.handleListTraces }},
	{name: "find_trace", desc: "Find the trace a message belongs to, by the message's id and optionally the node it was seen at.",
		perm: PermFlowsRead, method: "GET", path: "/traces/find", query: []string{"msgId", "node"}, handler: func(s *Server) http.HandlerFunc { return s.handleFindTrace }},
	{name: "get_trace", desc: "One trace with every hop. The id comes from list_traces.",
		perm: PermFlowsRead, method: "GET", path: "/traces/{id}", pathArgs: []string{"id"}, handler: func(s *Server) http.HandlerFunc { return s.handleGetTrace }},
	{name: "trace_to_test", desc: "Draft a flow test from a trace. It saves nothing: use add_test_cases to keep the draft.",
		perm: PermFlowsRead, method: "POST", path: "/traces/{id}/test", pathArgs: []string{"id"}, handler: func(s *Server) http.HandlerFunc { return s.handleTraceToTest }},
	{name: "replay_trace", desc: "Put the message that started a trace into the flows (the deployed ones, or a draft if flows is in the body) and compare where it ends up with where it ended up the first time. It runs in a sandbox the way the editor's replay does, and the route says in its own words when this instance cannot replay.",
		perm: PermFlowsRead, method: "POST", path: "/traces/{id}/replay", pathArgs: []string{"id"}, body: "optional: {flows}", handler: func(s *Server) http.HandlerFunc { return s.handleReplay }},
	{name: "reset_mfa", desc: "TURN OFF a user's two-factor sign-in so they can enrol again, for someone who lost their device. The body is {username}. It needs auth.admin, the permission the route asks for, and the audit log records who did it.",
		perm: PermAuthAdmin, method: "POST", path: "/auth/mfa/reset", body: "{username: the account to reset}", handler: func(s *Server) http.HandlerFunc { return s.handleMFAReset }},
	{name: "get_runtime_stats", desc: "The running flows' numbers: per-node received, sent, errors, dropped and queue depth.",
		perm: PermStatusRead, method: "GET", path: "/runtime/stats", handler: func(s *Server) http.HandlerFunc { return s.handleStats }},
}

func (t routeTool) schema() string {
	props := map[string]any{}
	var req []string
	for _, p := range t.pathArgs {
		props[p] = map[string]any{"type": []string{"string", "integer"}, "description": "the " + p + " from the listing tool"}
		req = append(req, p)
	}
	for _, q := range t.query {
		props[q] = map[string]any{"type": []string{"string", "integer"}}
	}
	if t.body != "" {
		props["body"] = map[string]any{"type": []string{"object", "array"}, "description": t.body}
		if !strings.HasPrefix(t.body, "optional") {
			req = append(req, "body")
		}
	}
	doc := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(req) > 0 {
		doc["required"] = req
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func (t routeTool) run(s *Server, ctx context.Context, caller string, args map[string]any) (mcphost.Outcome, error) {
	target := t.path
	values := map[string]string{}
	for _, p := range t.pathArgs {
		v, ok := args[p]
		if !ok || fmt.Sprint(v) == "" {
			return mcphost.Outcome{Text: fmt.Sprintf("%s needs %s", t.name, p), IsError: true}, nil
		}
		// A number arrives as a float64; print it without the exponent.
		str := fmt.Sprint(v)
		if f, ok := v.(float64); ok {
			str = fmt.Sprint(int64(f))
		}
		values[p] = str
		target = strings.ReplaceAll(target, "{"+p+"}", url.PathEscape(str))
	}
	if len(t.query) > 0 {
		q := url.Values{}
		keys := append([]string(nil), t.query...)
		sort.Strings(keys)
		for _, k := range keys {
			if v, ok := args[k]; ok && fmt.Sprint(v) != "" {
				if f, isNum := v.(float64); isNum {
					q.Set(k, fmt.Sprint(int64(f)))
				} else {
					q.Set(k, fmt.Sprint(v))
				}
			}
		}
		if len(q) > 0 {
			target += "?" + q.Encode()
		}
	}
	var body []byte
	if t.body != "" {
		if b, ok := args["body"]; ok && b != nil {
			var err error
			if body, err = json.Marshal(b); err != nil {
				return mcphost.Outcome{Text: "the body could not be sent: " + err.Error(), IsError: true}, nil
			}
		}
	}
	hdr := map[string]string{}
	if body != nil {
		hdr["Content-Type"] = "application/json"
	}
	return outcome(s.runHandler(ctx, caller, t.method, target, hdr, body, values, t.handler(s))), nil
}

// allRouteTools is every tool that is one row over one admin route.
func allRouteTools() []routeTool {
	return append(append(append(append([]routeTool(nil), routeTools...), diagramTools...), instrumentTools...), stationTools...)
}

func init() {
	for _, t := range allRouteTools() {
		t := t
		builtinTools = append(builtinTools, builtinTool{
			name: t.name, desc: t.desc, perm: t.perm, schema: t.schema(),
			run: func(s *Server, ctx context.Context, caller string, args map[string]any) (mcphost.Outcome, error) {
				return t.run(s, ctx, caller, args)
			},
		})
	}
}
