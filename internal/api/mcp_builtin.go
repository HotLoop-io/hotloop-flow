package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/mcphost"
)

// The tools Flow serves itself, beside the ones its flows serve: list and read
// the flows, deploy them, look at the deployments and roll one back, and press an
// inject node. An agent that can only call what a flow author wired up cannot see
// or change the flows, and Flow's MCP reads and writes like every MCP in the
// HotLoop family.
//
// None of them is a second implementation. Each runs the admin API's own handler
// for the same job, as the caller the MCP endpoint authenticated, so the deploy
// gate, the revision check, the typed-wire and plugin refusals, the deployment
// history and the audit trail are the route's. The permission each asks for is the
// route's own (flows.read, flows.write, inject.write), checked by the MCP host on
// top of mcp.call before the tool runs, and every call and its outcome is in the
// audit log under the caller's name like any other tool's.

// builtinTool is one tool and the route it stands for.
type builtinTool struct {
	name, desc, perm string
	schema           string
	run              func(s *Server, ctx context.Context, caller string, args map[string]any) (mcphost.Outcome, error)
}

var builtinTools = []builtinTool{
	{"list_flows", "The flows on this instance: each tab and subflow with its id, label, whether it is disabled and how many nodes it has, and the revision the deployed set is at.",
		"flows.read", `{"type":"object","additionalProperties":false}`, (*Server).toolListFlows},
	{"get_flows", "The whole deployed flow set as the editor reads it: the revision and every node. deploy_flows replaces the whole set, so read it here first and send it back changed, with the revision.",
		"flows.read", `{"type":"object","additionalProperties":false}`, (*Server).toolGetFlows},
	{"get_flow", "One flow (a tab or a subflow) with its nodes, by id.",
		"flows.read", `{"type":"object","properties":{"id":{"type":"string","description":"the tab or subflow id, from list_flows"}},"required":["id"],"additionalProperties":false}`, (*Server).toolGetFlow},
	{"deploy_flows", "DEPLOY: replace the whole flow set with the one you send. It goes through the deploy the editor uses, so a document that does not parse, flows whose wires disagree about what travels on them, plugins that are not allowed, and flow tests that fail are refused, in the route's words, and nothing changes. A deployed flow starts running: a flow can move equipment. Send the revision get_flows gave you, and the deploy is refused if somebody deployed since; an empty revision overwrites whatever is there, on purpose. Give a note: it is recorded in the deployment history with who deployed.",
		"flows.write", `{"type":"object","properties":{"flows":{"type":"array","items":{"type":"object"},"description":"every node of the flow set, as get_flows returns them"},"rev":{"type":"string","description":"the revision from get_flows; empty overwrites"},"mode":{"type":"string","description":"how much to restart: full, flows, nodes or reload. Left out, only what changed restarts."},"note":{"type":"string","description":"why, for the deployment history"}},"required":["flows","rev"],"additionalProperties":false}`, (*Server).toolDeployFlows},
	{"list_deployments", "The deployment history, newest first: the number, who, when, the note, and what each changed.",
		"flows.read", `{"type":"object","properties":{"limit":{"type":"integer","minimum":1}},"additionalProperties":false}`, (*Server).toolListDeployments},
	{"rollback_deployment", "ROLL BACK: deploy the flow set as it was at an earlier deployment, as a new deployment with your note. The flows start running.",
		"flows.write", `{"type":"object","properties":{"deployment":{"type":"integer","minimum":1,"description":"the deployment number from list_deployments"},"note":{"type":"string"}},"required":["deployment"],"additionalProperties":false}`, (*Server).toolRollback},
	{"inject_node", "Press an inject node, starting its flow by hand with the message you give. A flow can move equipment, and who pressed the button is in the audit trail.",
		"inject.write", `{"type":"object","properties":{"id":{"type":"string","description":"the inject node's id"},"payload":{"type":"object","description":"fields of the message to send; left out, the node sends what it is set to"}},"required":["id"],"additionalProperties":false}`, (*Server).toolInject},
}

// BuiltinToolNames is the names of the tools Flow serves itself, for the docs and the tests.
func BuiltinToolNames() []string {
	out := make([]string, 0, len(builtinTools))
	for _, t := range builtinTools {
		out = append(out, t.name)
	}
	return out
}

// registerMCPTools serves the built-in tools on the registry. They outlive a
// runtime teardown, which only drops the flows' own tools.
func (s *Server) registerMCPTools(reg *mcphost.Registry) {
	for _, t := range builtinTools {
		t := t
		if need := toolNeeds[t.name]; need != nil && !need(s) {
			continue
		}
		err := reg.RegisterBuiltin(mcphost.ToolSpec{
			Name: t.name, Description: t.desc, InputSchema: json.RawMessage(t.schema), Permission: t.perm,
		}, func(ctx context.Context, c mcphost.Call) (mcphost.Outcome, error) {
			return t.run(s, ctx, c.Caller, c.Arguments)
		})
		if err != nil {
			s.log.Error("a built-in MCP tool could not be served", "tool", t.name, "error", err)
		}
	}
}

// recorder is the smallest ResponseWriter that keeps what a handler wrote.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int)        { r.status = code }

// runHandler runs one of the admin API's handlers for the caller, as a request
// the way the editor would send it, and answers what the handler answered. The
// caller is what the audit trail and the deployment history name; the MCP host
// has already identified them and checked their permissions.
func (s *Server) runHandler(ctx context.Context, caller, method, target string, hdr map[string]string, body []byte,
	pathValues map[string]string, h http.HandlerFunc) (int, []byte) {

	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return http.StatusBadRequest, []byte(err.Error())
	}
	req.RemoteAddr = "mcp"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	req = req.WithContext(withUser(req, caller))
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	h(rec, req)
	return rec.status, rec.body.Bytes()
}

// outcome turns a handler's answer into a tool's: JSON as structured content when
// it is an object, and a refusal in the route's own words as a tool error.
func outcome(status int, body []byte) mcphost.Outcome {
	text := strings.TrimSpace(string(body))
	var e struct {
		Error string `json:"error"`
	}
	if status >= http.StatusBadRequest {
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			text = e.Error
		}
		return mcphost.Outcome{Text: fmt.Sprintf("refused (%d): %s", status, text), IsError: true, Structured: objectOf(body)}
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") == nil {
		text = pretty.String()
	}
	return mcphost.Outcome{Text: text, Structured: objectOf(body)}
}

func objectOf(body []byte) map[string]any {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		return m
	}
	return nil
}

func (s *Server) toolGetFlows(ctx context.Context, caller string, _ map[string]any) (mcphost.Outcome, error) {
	return outcome(s.runHandler(ctx, caller, http.MethodGet, "/flows", nil, nil, nil, s.handleGetFlows)), nil
}

type flowDoc struct {
	Rev      string           `json:"rev"`
	Flows    []map[string]any `json:"flows"`
	Warnings []string         `json:"warnings"`
}

func (s *Server) readFlows(ctx context.Context, caller string) (flowDoc, *mcphost.Outcome) {
	status, body := s.runHandler(ctx, caller, http.MethodGet, "/flows", nil, nil, nil, s.handleGetFlows)
	if status != http.StatusOK {
		o := outcome(status, body)
		return flowDoc{}, &o
	}
	var d flowDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return flowDoc{}, &mcphost.Outcome{Text: "the flow set could not be read: " + err.Error(), IsError: true}
	}
	return d, nil
}

func (s *Server) toolListFlows(ctx context.Context, caller string, _ map[string]any) (mcphost.Outcome, error) {
	d, bad := s.readFlows(ctx, caller)
	if bad != nil {
		return *bad, nil
	}
	nodes := map[string]int{}
	for _, n := range d.Flows {
		if z, _ := n["z"].(string); z != "" {
			nodes[z]++
		}
	}
	type row struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Label    string `json:"label,omitempty"`
		Disabled bool   `json:"disabled"`
		Nodes    int    `json:"nodes"`
	}
	rows := []row{}
	var b strings.Builder
	fmt.Fprintf(&b, "Revision %s.\n", d.Rev)
	for _, n := range d.Flows {
		typ, _ := n["type"].(string)
		if typ != "tab" && typ != "subflow" {
			continue
		}
		id, _ := n["id"].(string)
		label, _ := n["label"].(string)
		if typ == "subflow" {
			label, _ = n["name"].(string)
		}
		dis, _ := n["disabled"].(bool)
		rows = append(rows, row{ID: id, Type: typ, Label: label, Disabled: dis, Nodes: nodes[id]})
		state := ""
		if dis {
			state = " (disabled)"
		}
		fmt.Fprintf(&b, "%s  %s  %q%s, %d nodes\n", id, typ, label, state, nodes[id])
	}
	if len(rows) == 0 {
		b.WriteString("No flows.\n")
	}
	return mcphost.Outcome{Text: b.String(), Structured: map[string]any{"rev": d.Rev, "flows": rows}}, nil
}

func (s *Server) toolGetFlow(ctx context.Context, caller string, args map[string]any) (mcphost.Outcome, error) {
	id, _ := args["id"].(string)
	d, bad := s.readFlows(ctx, caller)
	if bad != nil {
		return *bad, nil
	}
	var picked []map[string]any
	found := false
	for _, n := range d.Flows {
		nid, _ := n["id"].(string)
		z, _ := n["z"].(string)
		if nid == id && (n["type"] == "tab" || n["type"] == "subflow") {
			found = true
			picked = append([]map[string]any{n}, picked...)
		} else if z == id {
			picked = append(picked, n)
		}
	}
	if !found {
		return mcphost.Outcome{Text: fmt.Sprintf("no flow with the id %q: list_flows has them", id), IsError: true}, nil
	}
	raw, _ := json.MarshalIndent(map[string]any{"rev": d.Rev, "flow": picked}, "", "  ")
	return mcphost.Outcome{Text: string(raw), Structured: map[string]any{"rev": d.Rev, "flow": picked}}, nil
}

func (s *Server) toolDeployFlows(ctx context.Context, caller string, args map[string]any) (mcphost.Outcome, error) {
	rev, _ := args["rev"].(string)
	doc := map[string]any{"rev": rev, "flows": args["flows"]}
	if note, ok := args["note"].(string); ok {
		doc["note"] = note
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return mcphost.Outcome{}, err
	}
	hdr := map[string]string{"HotLoop-Flow-Deployment-Rev": rev}
	if mode, _ := args["mode"].(string); mode != "" {
		hdr["HotLoop-Flow-Deployment-Type"] = mode
	}
	return outcome(s.runHandler(ctx, caller, http.MethodPost, "/flows", hdr, body, nil, s.handlePostFlows)), nil
}

func (s *Server) toolListDeployments(ctx context.Context, caller string, args map[string]any) (mcphost.Outcome, error) {
	target := "/deployments"
	if n, ok := args["limit"].(float64); ok && n >= 1 {
		target += "?limit=" + url.QueryEscape(fmt.Sprint(int(n)))
	}
	return outcome(s.runHandler(ctx, caller, http.MethodGet, target, nil, nil, nil, s.handleListDeployments)), nil
}

func (s *Server) toolRollback(ctx context.Context, caller string, args map[string]any) (mcphost.Outcome, error) {
	seq, _ := args["deployment"].(float64)
	note, _ := args["note"].(string)
	body, _ := json.Marshal(map[string]string{"note": note})
	return outcome(s.runHandler(ctx, caller, http.MethodPost, "/deployments/x/rollback", nil, body,
		map[string]string{"seq": fmt.Sprint(int64(seq))}, s.handleRollback)), nil
}

func (s *Server) toolInject(ctx context.Context, caller string, args map[string]any) (mcphost.Outcome, error) {
	id, _ := args["id"].(string)
	var body []byte
	if p, ok := args["payload"].(map[string]any); ok && len(p) > 0 {
		body, _ = json.Marshal(p)
	}
	return outcome(s.runHandler(ctx, caller, http.MethodPost, "/inject/x", nil, body,
		map[string]string{"id": id}, s.handleInject)), nil
}
