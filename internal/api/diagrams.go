package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/instruments"
)

// diagramRoutes serves the diagrams of the flow file: what is there, the panel of
// each, and the controls for running and debugging one. The editor and the panel
// page are both clients of these, and so is anything else that can send JSON, which
// is how a diagram is run from a script or a CI job.
func (s *Server) diagramRoutes() {
	if s.deps.Diagrams == nil {
		return
	}
	read := func(h http.HandlerFunc) http.Handler { return s.auth(PermStatusRead, h) }
	run := func(h http.HandlerFunc) http.Handler { return s.auth(PermDiagramRun, h) }
	s.mux.Handle("GET "+s.path("/diagrams"), read(s.handleDiagrams))
	s.mux.Handle("GET "+s.path("/diagrams/{id}"), read(s.handleDiagram))
	s.mux.Handle("POST "+s.path("/diagrams/{id}/run"), run(s.handleDiagramRun))
	s.mux.Handle("POST "+s.path("/diagrams/{id}/stop"), run(s.handleDiagramStop))
	s.mux.Handle("POST "+s.path("/diagrams/{id}/debug"), run(s.handleDiagramDebug))
	s.mux.Handle("POST "+s.path("/diagrams/{id}/sub"), read(s.handleDiagramSub))
	s.mux.Handle("PUT "+s.path("/diagrams/{id}/controls/{node}"), run(s.handleDiagramControl))
	s.mux.Handle("PUT "+s.path("/diagrams/{id}/breakpoints"), run(s.handleDiagramBreakpoints))
	s.mux.Handle("PUT "+s.path("/diagrams/{id}/probes"), run(s.handleDiagramProbes))
	// The address an operator is given: it opens the editor's panel view. A
	// redirect, so the page is the one the editor ships, signed in the same way.
	s.mux.HandleFunc("GET "+s.path("/panel/{id}"), func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.path("/")+"#/panel/"+r.PathValue("id"), http.StatusSeeOther)
	})
}

// instrumentRoutes serves the per-instrument write switch: which instruments the
// deployed diagrams would write to, and whether each one's writes are on, and the
// way somebody holding instrument.write turns one on or off.
func (s *Server) instrumentRoutes() {
	if s.deps.Instruments == nil {
		return
	}
	s.mux.Handle("GET "+s.path("/instruments"), s.auth(PermStatusRead, s.handleInstruments))
	s.mux.Handle("PUT "+s.path("/instruments/writes"), s.auth(PermInstrumentWrite, s.handleInstrumentWrites))
}

type instrumentRow struct {
	Instrument string         `json:"instrument"`
	Allowed    bool           `json:"allowed"`
	By         string         `json:"by,omitempty"`
	At         string         `json:"at,omitempty"`
	Note       string         `json:"note,omitempty"`
	Diagrams   map[string]any `json:"diagrams,omitempty"`
}

func (s *Server) handleInstruments(w http.ResponseWriter, _ *http.Request) {
	rows := map[string]*instrumentRow{}
	for id, a := range s.deps.Instruments.List() {
		r := &instrumentRow{Instrument: id, Allowed: a.Allowed, By: a.By, Note: a.Note}
		if !a.At.IsZero() {
			r.At = a.At.Format(time.RFC3339)
		}
		rows[id] = r
	}
	if s.deps.Diagrams != nil {
		for _, sum := range s.deps.Diagrams.List() {
			if sum.Owner != "" && sum.Owner != sum.ID {
				continue
			}
			for _, t := range s.deps.Diagrams.Writes(sum.ID) {
				r := rows[t.Instrument]
				if r == nil {
					r = &instrumentRow{Instrument: t.Instrument, Allowed: s.deps.Instruments.Allowed(t.Instrument)}
					rows[t.Instrument] = r
				}
				if r.Diagrams == nil {
					r.Diagrams = map[string]any{}
				}
				r.Diagrams[sum.ID] = t.Nodes
			}
		}
	}
	out := make([]*instrumentRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument < out[j].Instrument })
	writeJSON(w, http.StatusOK, map[string]any{"instruments": out})
}

func (s *Server) handleInstrumentWrites(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Instrument string `json:"instrument"`
		Allowed    bool   `json:"allowed"`
		Note       string `json:"note"`
	}
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user := requestUser(r)
	if err := s.deps.Instruments.Set(body.Instrument, body.Allowed, user, body.Note); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	id := instruments.Normalize(body.Instrument)
	s.record(r, audit.InstrumentSwitch, user, map[string]any{"instrument": id, "allowed": body.Allowed, "note": body.Note})
	a := s.deps.Instruments.List()[id]
	writeJSON(w, http.StatusOK, instrumentRow{Instrument: id, Allowed: a.Allowed, By: a.By, At: a.At.Format(time.RFC3339), Note: a.Note})
}

func (s *Server) diagramError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, diagram.ErrNoDiagram):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, diagram.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	}
}

func (s *Server) handleDiagrams(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"diagrams": s.deps.Diagrams.List()})
}

func (s *Server) handleDiagram(w http.ResponseWriter, r *http.Request) {
	d, err := s.deps.Diagrams.Describe(r.PathValue("id"))
	if err != nil {
		s.diagramError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleDiagramRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		HighlightMs float64  `json:"highlightMs"`
		Pause       bool     `json:"pause"`
		Breakpoints []string `json:"breakpoints"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	run, err := s.deps.Diagrams.Start(id, diagram.StartOptions{
		Highlight: time.Duration(body.HighlightMs * float64(time.Millisecond)), Pause: body.Pause, Breakpoints: body.Breakpoints,
	})
	if err != nil {
		s.diagramError(w, err)
		return
	}
	s.record(r, audit.DiagramRun, requestUser(r), map[string]any{"diagram": id, "run": run.ID})
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run.ID, "diagram": id})
}

func (s *Server) handleDiagramStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.deps.Diagrams.Stop(id); err != nil {
		s.diagramError(w, err)
		return
	}
	s.record(r, audit.DiagramStop, requestUser(r), map[string]any{"diagram": id})
	writeJSON(w, http.StatusOK, map[string]any{"stopped": id})
}

func (s *Server) handleDiagramDebug(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Action      string  `json:"action"`
		HighlightMs float64 `json:"highlightMs"`
	}
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	run := s.deps.Diagrams.Current(id)
	if run == nil {
		writeError(w, http.StatusConflict, "this diagram has not been run")
		return
	}
	switch body.Action {
	case "pause":
		run.Controller().Pause()
	case "resume":
		run.Controller().Resume()
	case "step-into":
		run.Controller().Step(diagram.StepInto)
	case "step-over":
		run.Controller().Step(diagram.StepOver)
	case "step-out":
		run.Controller().Step(diagram.StepOut)
	case "highlight":
		run.SetHighlight(time.Duration(body.HighlightMs * float64(time.Millisecond)))
	default:
		writeError(w, http.StatusBadRequest, `action must be pause, resume, step-into, step-over, step-out or highlight`)
		return
	}
	s.record(r, audit.DiagramDebug, requestUser(r), map[string]any{"diagram": id, "action": body.Action})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDiagramControl(w http.ResponseWriter, r *http.Request) {
	id, node := r.PathValue("id"), r.PathValue("node")
	var body struct {
		Value any `json:"value"`
	}
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := s.deps.Diagrams.Panel(id)
	if err != nil {
		s.diagramError(w, err)
		return
	}
	v, err := p.SetControl(node, body.Value)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// A control is how a person commands the thing the diagram drives, so who set
	// it, and to what, is on the record like an inject is.
	s.record(r, audit.DiagramControl, requestUser(r), map[string]any{"diagram": id, "control": node, "value": v})
	writeJSON(w, http.StatusOK, map[string]any{"control": node, "value": v})
}

func (s *Server) handleDiagramBreakpoints(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Keys []string `json:"keys"`
	}
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.deps.Diagrams.Diagram(id); !ok {
		writeError(w, http.StatusNotFound, diagram.ErrNoDiagram.Error())
		return
	}
	// Kept for the next run too, so breakpoints can be set before pressing run.
	s.deps.Diagrams.SetBreakpoints(id, body.Keys)
	s.record(r, audit.DiagramDebug, requestUser(r), map[string]any{"diagram": id, "breakpoints": body.Keys})
	writeJSON(w, http.StatusOK, map[string]any{"keys": body.Keys})
}

func (s *Server) handleDiagramProbes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Wires []string `json:"wires"`
		Depth int      `json:"depth"`
	}
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := s.deps.Diagrams.Diagram(id); !ok {
		writeError(w, http.StatusNotFound, diagram.ErrNoDiagram.Error())
		return
	}
	s.deps.Diagrams.SetProbes(id, body.Wires, body.Depth)
	writeJSON(w, http.StatusOK, map[string]any{"wires": body.Wires})
}

// handleDiagramSub makes a sub-diagram from some nodes of the flow file the
// editor sends and sends the new flow file back. Nothing is saved or deployed:
// the editor loads the answer as an edit, so it can be undone like any other. It
// reads and computes only, so it needs no more than a view of the status.
func (s *Server) handleDiagramSub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Flows json.RawMessage `json:"flows"`
		Nodes []string        `json:"nodes"`
		Name  string          `json:"name"`
	}
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := diagram.ExtractSub(body.Flows, r.PathValue("id"), body.Nodes, body.Name)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flows": json.RawMessage(out)})
}
