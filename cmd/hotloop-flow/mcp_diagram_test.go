package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/instruments"
	"github.com/HotLoop-io/hotloop-flow/internal/mcphost"
)

// An agent works a diagram through MCP the way a person works its panel: it
// deploys the diagram, reads it, runs it, sets a control on the panel and
// switches an instrument's writes, with the same permissions the routes ask for,
// and every one of them is in the audit trail under its name.

const agentDiagram = `[
 {"id":"main","type":"tab","label":"Gain","ew_diagram":{}},
 {"id":"gain","type":"dia-control","z":"main","x":100,"y":100,"label":"Gain","kind":"number","default":"2","min":"0","max":"10","wires":[["twice"]],"ew_in":[["b"]]},
 {"id":"two","type":"dia-const","z":"main","x":100,"y":160,"value":"21","wires":[["twice"]],"ew_in":[["a"]]},
 {"id":"twice","type":"dia-math","z":"main","x":300,"y":120,"op":"mul","wires":[["out"]],"ew_in":[["value"]]},
 {"id":"out","type":"dia-indicator","z":"main","x":500,"y":120,"label":"Result","wires":[],"ew_in":[["value"]]}
]`

func TestAnAgentRunsAndControlsADiagramThroughMCP(t *testing.T) {
	app, _ := newApp(t)
	reg := mcphost.New("test")
	app.diagrams = diagram.NewManager(nil)
	t.Cleanup(app.diagrams.Close)
	switches, err := instruments.Open(filepath.Join(t.TempDir(), "instruments.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := serveAppWith(t, app, map[string][]string{
		"agent":    {"mcp.call", "flows.read", "flows.write", "status.read", "diagram.run", "instrument.write"},
		"watcher":  {"mcp.call", "status.read"},
		"operator": {"mcp.call", "status.read", "diagram.run"},
	}, func(d *api.Deps) { d.MCP = reg; d.Diagrams = app.diagrams; d.Instruments = switches })
	agent := mcpSession(t, e, e.login(t, "agent"))

	// Deploy a diagram through MCP, then find it.
	_, doc, _ := callTool(t, agent, "get_flows", nil)
	rev, _ := doc["rev"].(string)
	var flows []any
	if err := json.Unmarshal([]byte(agentDiagram), &flows); err != nil {
		t.Fatal(err)
	}
	if text, _, isErr := callTool(t, agent, "deploy_flows", map[string]any{"flows": flows, "rev": rev, "note": "a gain diagram"}); isErr {
		t.Fatalf("deploy_flows: %s", text)
	}
	text, _, isErr := callTool(t, agent, "list_diagrams", nil)
	if isErr || !strings.Contains(text, `"main"`) {
		t.Fatalf("list_diagrams: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "get_diagram", map[string]any{"id": "main"}); isErr || !strings.Contains(text, "Gain") {
		t.Fatalf("get_diagram: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "get_diagram", map[string]any{"id": "no-such"}); !isErr || !strings.Contains(text, "404") {
		t.Errorf("a diagram that is not there: %v %s", isErr, text)
	}

	// Run it, set a control on its panel, and stop it.
	if text, st, isErr := callTool(t, agent, "run_diagram", map[string]any{"id": "main"}); isErr || st["diagram"] != "main" {
		t.Fatalf("run_diagram: %v %s", isErr, text)
	}
	// What the run did is a tool too, the recording the editor replays.
	if text, st, isErr := callTool(t, agent, "get_diagram_trace", map[string]any{"id": "main"}); isErr || st["diagram"] != "main" || !strings.Contains(text, `"events"`) {
		t.Fatalf("get_diagram_trace: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "get_diagram_trace", map[string]any{"id": "no-such"}); !isErr || !strings.Contains(text, "404") {
		t.Errorf("the trace of a diagram that is not there: %v %s", isErr, text)
	}
	if text, st, isErr := callTool(t, agent, "set_diagram_control", map[string]any{"id": "main", "node": "gain", "body": map[string]any{"value": 4}}); isErr || st["value"] != float64(4) {
		t.Fatalf("set_diagram_control: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "set_diagram_control", map[string]any{"id": "main", "node": "gain", "body": map[string]any{"value": 400}}); !isErr {
		t.Errorf("a control was set past its range: %s", text)
	}
	if text, _, isErr := callTool(t, agent, "set_diagram_control", map[string]any{"id": "main", "node": "out", "body": map[string]any{"value": 1}}); !isErr || !strings.Contains(text, "not a control") {
		t.Errorf("an indicator was set as a control: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, agent, "set_diagram_breakpoints", map[string]any{"id": "main", "body": map[string]any{"keys": []any{}}}); isErr {
		t.Errorf("set_diagram_breakpoints: %s", text)
	}
	if text, _, isErr := callTool(t, agent, "set_diagram_probes", map[string]any{"id": "main", "body": map[string]any{"wires": []any{}, "depth": 5}}); isErr {
		t.Errorf("set_diagram_probes: %s", text)
	}
	if text, _, isErr := callTool(t, agent, "stop_diagram", map[string]any{"id": "main"}); isErr {
		t.Errorf("stop_diagram: %s", text)
	}

	// Instruments: read them, turn one's writes on, and read it back.
	if text, _, isErr := callTool(t, agent, "set_instrument_writes", map[string]any{"body": map[string]any{"instrument": "scope-1", "allowed": true, "note": "bench"}}); isErr {
		t.Fatalf("set_instrument_writes: %s", text)
	}
	if text, _, isErr := callTool(t, agent, "list_instruments", nil); isErr || !strings.Contains(text, "scope-1") {
		t.Errorf("list_instruments: %v %s", isErr, text)
	}
	if !switches.Allowed("scope-1") {
		t.Error("the instrument's writes were not turned on")
	}

	// The permissions are the routes': a caller who may only watch is not offered
	// what runs a diagram, and one who may run is not offered the instrument switch.
	watcher := mcpSession(t, e, e.login(t, "watcher"))
	if text, _, isErr := callTool(t, watcher, "run_diagram", map[string]any{"id": "main"}); !isErr || !strings.Contains(text, "diagram.run") {
		t.Errorf("a watcher ran a diagram: %v %s", isErr, text)
	}
	if text, _, isErr := callTool(t, watcher, "list_diagrams", nil); isErr {
		t.Errorf("a watcher could not look: %s", text)
	}
	operator := mcpSession(t, e, e.login(t, "operator"))
	if text, _, isErr := callTool(t, operator, "set_instrument_writes", map[string]any{"body": map[string]any{"instrument": "scope-2", "allowed": true}}); !isErr || !strings.Contains(text, "instrument.write") {
		t.Errorf("an operator turned on an instrument's writes: %v %s", isErr, text)
	}
	if switches.Allowed("scope-2") {
		t.Error("a refused call changed an instrument")
	}
	listed, err := watcher.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range listed.Tools {
		if tl.Name == "run_diagram" || tl.Name == "set_diagram_control" || tl.Name == "set_instrument_writes" {
			t.Errorf("a watcher is offered %s", tl.Name)
		}
	}

	// The audit trail names the agent for each.
	for _, ev := range []string{audit.DiagramRun, audit.DiagramControl, audit.InstrumentSwitch, audit.DiagramStop} {
		rows, _ := e.trail.Read(audit.Query{Event: ev})
		if len(rows) == 0 || rows[0].User != "agent" {
			t.Errorf("audit rows for %s: %+v", ev, rows)
		}
	}
}
