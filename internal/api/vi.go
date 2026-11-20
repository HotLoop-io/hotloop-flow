package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"

	"github.com/HotLoop-io/hotloop-flow/internal/vi"
)

// handleImportVI reads a LabVIEW VI and answers what it holds and the flow file
// that carries it, with every part of the VI that did not come across. It reads
// and computes only: nothing is saved or deployed, and the editor shows the list
// before it adds anything. The body is {name, data} with the file base64.
func (s *Server) handleImportVI(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, vi.MaxFile*2))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the VI is too large")
		return
	}
	var req struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Data == "" {
		writeError(w, http.StatusBadRequest, "send {\"name\": the file's name, \"data\": the VI, base64}")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "data is not base64")
		return
	}
	in, err := vi.Read(raw, req.Name)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": in, "flows": in.Flows(), "unmapped": in.Unmapped()})
}
