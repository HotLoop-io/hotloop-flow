package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// Replayer puts a captured message into a flow somewhere it can't touch the
// plant, and says how it came out. The command does it in a child process, the
// way it runs flow tests, for the same reasons.
type Replayer func(ctx context.Context, flows []byte, trace engine.TraceView) (flowtest.ReplayResult, error)

// traceSummary is a trace in the list: enough to pick one.
type traceSummary struct {
	ID        string    `json:"id"`
	Started   time.Time `json:"started"`
	Node      string    `json:"node"`
	Hops      int       `json:"hops"`
	Errors    int       `json:"errors"`
	Truncated bool      `json:"truncated,omitempty"`
}

func (s *Server) tracesOr404(w http.ResponseWriter) *engine.Tracer {
	if s.deps.Tracer == nil {
		writeError(w, http.StatusNotFound, "tracing is turned off on this instance")
		return nil
	}
	return s.deps.Tracer
}

// canvasNode is the node on the canvas to light for a hop at this id. A node
// inside a subflow instance runs as a copy with a derived id, and the canvas
// shows the instance.
func canvasNode(flows *engine.Flows, id string) string {
	for range 32 {
		if _, ok := flows.Nodes[id]; ok {
			return id
		}
		scope, _, ok := engine.SplitDerivedID(id)
		if !ok {
			return id
		}
		id = scope
	}
	return id
}

func viewOf(snap engine.TraceSnapshot, flows *engine.Flows) engine.TraceView {
	v := snap.View()
	if flows != nil {
		for i := range v.Hops {
			if c := canvasNode(flows, v.Hops[i].Node); c != v.Hops[i].Node {
				v.Hops[i].Canvas = c
			}
		}
	}
	return v
}

// handleListTraces lists the traces kept, newest first.
func (s *Server) handleListTraces(w http.ResponseWriter, r *http.Request) {
	tr := s.tracesOr404(w)
	if tr == nil {
		return
	}
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	out := []traceSummary{}
	for _, t := range tr.Traces() {
		if len(out) == limit {
			break
		}
		snap := t.Snapshot()
		sum := traceSummary{ID: snap.ID, Started: snap.Started, Hops: len(snap.Hops), Truncated: snap.Truncated}
		if len(snap.Hops) > 0 {
			sum.Node = snap.Hops[0].Node
		}
		for _, h := range snap.Hops {
			if h.Error != "" {
				sum.Errors++
			}
		}
		out = append(out, sum)
	}
	writeJSON(w, http.StatusOK, map[string]any{"traces": out, "stats": tr.Stats(), "limits": tr.Limits()})
}

// handleGetTrace returns one trace, every hop with its timing and message.
func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	tr := s.tracesOr404(w)
	if tr == nil {
		return
	}
	t, ok := tr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "that trace is no longer kept: the newest "+strconv.Itoa(tr.Limits().Traces)+" are")
		return
	}
	live, _ := s.deps.Flows.Load()
	writeJSON(w, http.StatusOK, map[string]any{"trace": viewOf(t.Snapshot(), live)})
}

// handleFindTrace finds the trace a message was in, from the message id the
// debug sidebar shows and the Debug node that showed it. focus is the hop the
// message was at, which is where the path the canvas lights ends.
func (s *Server) handleFindTrace(w http.ResponseWriter, r *http.Request) {
	tr := s.tracesOr404(w)
	if tr == nil {
		return
	}
	q := r.URL.Query()
	t, hop, ok := tr.Find(q.Get("msgId"), q.Get("node"))
	if !ok {
		writeError(w, http.StatusNotFound, "that message's trace is no longer kept: send it again and look at the new one")
		return
	}
	snap := t.Snapshot()
	live, _ := s.deps.Flows.Load()
	writeJSON(w, http.StatusOK, map[string]any{
		"trace": viewOf(snap, live),
		"focus": hop,
		"path":  snap.PathTo(hop),
	})
}

// handleTraceToTest drafts a flow test from a trace. It saves nothing; the
// editor shows the draft and the person decides.
func (s *Server) handleTraceToTest(w http.ResponseWriter, r *http.Request) {
	tr := s.tracesOr404(w)
	if tr == nil {
		return
	}
	t, ok := tr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "that trace is no longer kept")
		return
	}
	live, err := s.deps.Flows.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	doc, err := live.MarshalJSON()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, err := flowtest.FromTrace(doc, t.Snapshot())
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	text, err := flowtest.CaseYAML(c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": c.Name, "yaml": text})
}

// replayRequest is POST /traces/{id}/replay. No flows replays into what's
// running; the editor sends its working copy to replay into a draft.
type replayRequest struct {
	Flows json.RawMessage `json:"flows"`
}

// handleReplay puts the message that started a trace into a draft of the flow
// and compares where it ends up with where it ended up the first time.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	tr := s.tracesOr404(w)
	if tr == nil {
		return
	}
	if s.deps.Replay == nil {
		writeError(w, http.StatusNotFound, "this instance can't replay")
		return
	}
	t, ok := tr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "that trace is no longer kept")
		return
	}
	var req replayRequest
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var parsed *engine.Flows
	var err error
	if len(req.Flows) == 0 {
		if parsed, err = s.deps.Flows.Load(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else if parsed, err = engine.ParseFlows(req.Flows); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed.StripCredentials()
	doc, err := parsed.MarshalJSON()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.deps.Replay(r.Context(), doc, t.Snapshot().View())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res.Trace != nil {
		for i := range res.Trace.Hops {
			if c := canvasNode(parsed, res.Trace.Hops[i].Node); c != res.Trace.Hops[i].Node {
				res.Trace.Hops[i].Canvas = c
			}
		}
	}
	writeJSON(w, http.StatusOK, res)
}
