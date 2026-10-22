package api

import (
	"io"
	"net/http"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/wiretype"
)

// handleFlowTypes is POST /flows/types: the typed-wire check a deploy would
// make, run on a working copy and answered without deploying anything. The
// editor draws the answer on the wires, and it is the same code the deploy
// refuses with, so what the canvas shows and what the deploy does can't
// disagree.
//
// The body is the flow document, bare or wrapped, the way a deploy takes it.
// The answer is {"wires": [...]}, empty when nothing is wrong.
func (s *Server) handleFlowTypes(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.Config.Server.MaxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "flow document is too large")
		return
	}
	flows, err := engine.ParseFlows(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	problems := wiretype.Check(flows)
	if problems == nil {
		problems = []wiretype.Problem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"wires": problems})
}
