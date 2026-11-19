package api

import "net/http"

// The tools for diagrams and instruments: what the editor's panel view and the
// instruments page do, for an agent. Like the rest of Flow's own tools each is
// one row over one admin route, run through that route's own handler as the
// caller the MCP endpoint authenticated, with the permission the route asks for
// (status.read to look, diagram.run to run, stop, debug and set a control,
// instrument.write to turn an instrument's writes on or off). The run, the
// control and the instrument switch land in the audit log under the agent's name
// as they do for a person.
//
// A server built without diagrams or instruments has neither the routes nor the
// tools, so they are offered only where the route is served.

var diagramTools = []routeTool{
	{name: "list_diagrams", desc: "The diagrams in the flow file: each tab that runs as a diagram, with its id, whether it is running, its problems and its panel.",
		perm: PermStatusRead, method: "GET", path: "/diagrams", handler: func(s *Server) http.HandlerFunc { return s.handleDiagrams }},
	{name: "get_diagram", desc: "One diagram: its nodes, wires, panel controls and indicators, and any problems that stop it from running. The id comes from list_diagrams.",
		perm: PermStatusRead, method: "GET", path: "/diagrams/{id}", pathArgs: []string{"id"}, handler: func(s *Server) http.HandlerFunc { return s.handleDiagram }},
	{name: "run_diagram", desc: "RUN a diagram: start it, as the editor's Run button does. What its nodes write to equipment goes through HotLoop's write gate, and who started the run is in the audit trail. Optional body: {highlightMs, pause, breakpoints}.",
		perm: PermDiagramRun, method: "POST", path: "/diagrams/{id}/run", pathArgs: []string{"id"}, body: "optional: {highlightMs: how long a wire stays lit, pause: start paused, breakpoints: wire keys to stop at}",
		handler: func(s *Server) http.HandlerFunc { return s.handleDiagramRun }},
	{name: "stop_diagram", desc: "STOP a running diagram. Nothing it already wrote to equipment is undone.",
		perm: PermDiagramRun, method: "POST", path: "/diagrams/{id}/stop", pathArgs: []string{"id"}, handler: func(s *Server) http.HandlerFunc { return s.handleDiagramStop }},
	{name: "debug_diagram", desc: "Drive a running diagram's debugger: pause, resume, step-into, step-over, step-out, or highlight. The body is {action, highlightMs}.",
		perm: PermDiagramRun, method: "POST", path: "/diagrams/{id}/debug", pathArgs: []string{"id"}, body: "{action: pause, resume, step-into, step-over, step-out or highlight; highlightMs: for highlight}",
		handler: func(s *Server) http.HandlerFunc { return s.handleDiagramDebug }},
	{name: "set_diagram_control", desc: "Set a control on a diagram's panel, the way an operator moves a slider or flips a switch. It is checked against the control's kind, range and options. A control is how a person commands what the diagram drives, so who set it and to what is in the audit trail. The body is {value}.",
		perm: PermDiagramRun, method: "PUT", path: "/diagrams/{id}/controls/{node}", pathArgs: []string{"id", "node"}, body: "{value: what to set the control to}",
		handler: func(s *Server) http.HandlerFunc { return s.handleDiagramControl }},
	{name: "set_diagram_breakpoints", desc: "Set the wires a diagram stops at when it runs, kept for the next run too. The body is {keys}.",
		perm: PermDiagramRun, method: "PUT", path: "/diagrams/{id}/breakpoints", pathArgs: []string{"id"}, body: "{keys: the wire keys to stop at}",
		handler: func(s *Server) http.HandlerFunc { return s.handleDiagramBreakpoints }},
	{name: "set_diagram_probes", desc: "Choose the wires a diagram records values on, and how many it keeps. The body is {wires, depth}.",
		perm: PermDiagramRun, method: "PUT", path: "/diagrams/{id}/probes", pathArgs: []string{"id"}, body: "{wires: the wire keys to probe, depth: how many values to keep}",
		handler: func(s *Server) http.HandlerFunc { return s.handleDiagramProbes }},
	{name: "extract_sub_diagram", desc: "Make a sub-diagram out of some nodes of a flow file and answer the new flow file. Nothing is saved or deployed: send the answer to deploy_flows to keep it. The body is {flows, nodes, name}.",
		perm: PermStatusRead, method: "POST", path: "/diagrams/{id}/sub", pathArgs: []string{"id"}, body: "{flows: the flow file, nodes: the ids to move into the sub-diagram, name: its name}",
		handler: func(s *Server) http.HandlerFunc { return s.handleDiagramSub }},
}

var instrumentTools = []routeTool{
	{name: "list_instruments", desc: "The instruments the deployed diagrams would write to, and whether each one's writes are on. Every instrument starts with writes off.",
		perm: PermStatusRead, method: "GET", path: "/instruments", handler: func(s *Server) http.HandlerFunc { return s.handleInstruments }},
	{name: "set_instrument_writes", desc: "Turn one instrument's writes on or off. Until they are on, a diagram's write to that instrument is refused, so this is what lets a diagram command equipment. Who did it and the note are in the audit trail. The body is {instrument, allowed, note}.",
		perm: PermInstrumentWrite, method: "PUT", path: "/instruments/writes", body: "{instrument: the instrument's name, allowed: true or false, note: why}",
		handler: func(s *Server) http.HandlerFunc { return s.handleInstrumentWrites }},
}

// toolNeeds holds back the tools whose route is only served when the server was
// built with what the route needs.
var toolNeeds = func() map[string]func(*Server) bool {
	m := map[string]func(*Server) bool{}
	for _, t := range diagramTools {
		m[t.name] = func(s *Server) bool { return s.deps.Diagrams != nil }
	}
	for _, t := range instrumentTools {
		m[t.name] = func(s *Server) bool { return s.deps.Instruments != nil }
	}
	return m
}()
