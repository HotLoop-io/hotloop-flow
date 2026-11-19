package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/stations"
)

// StationsPushRequest is one push to the tenant's stations. Flows is the flow
// file to push; left out it is what this instance runs. There is no field for a
// station or a tenant: the stations are the tenant's own, from EmberNet.
type StationsPushRequest struct {
	Flows json.RawMessage `json:"flows,omitempty"`
	Note  string          `json:"note,omitempty"`
	// ConfirmRestart agrees that running loggers may be restarted. Window is a
	// time of day, HH:MM-HH:MM, in WindowZone (UTC when empty), in which that is
	// agreed in advance.
	ConfirmRestart bool   `json:"confirmRestart,omitempty"`
	Window         string `json:"window,omitempty"`
	WindowZone     string `json:"windowZone,omitempty"`
	DryRun         bool   `json:"dryRun,omitempty"`
}

// StationsService is what the station routes call. The server in cmd builds it
// from the stations settings.
type StationsService interface {
	Discover(ctx context.Context) (stations.Discovery, error)
	Push(ctx context.Context, req StationsPushRequest) (stations.Report, error)
}

func (s *Server) stationRoutes() {
	if s.deps.Stations == nil {
		return
	}
	s.mux.Handle("GET "+s.path("/stations"), s.auth(PermFlowsRead, s.handleStations))
	s.mux.Handle("POST "+s.path("/stations/push"), s.auth(PermFlowsWrite, s.handleStationsPush))
}

func (s *Server) handleStations(w http.ResponseWriter, r *http.Request) {
	d, err := s.deps.Stations.Discover(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleStationsPush(w http.ResponseWriter, r *http.Request) {
	var req StationsPushRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.Config.Server.MaxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the push request is too large")
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "parsing the push request: "+err.Error())
			return
		}
	}
	rep, err := s.deps.Stations.Push(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !req.DryRun {
		s.record(r, audit.StationsPush, requestUser(r), map[string]any{
			"tenant": rep.Tenant, "rev": rep.Rev, "ok": rep.OK(), "stopped": rep.Stopped, "stations": len(rep.Results),
		})
	}
	writeJSON(w, http.StatusOK, struct {
		stations.Report
		OK bool `json:"ok"`
	}{rep, rep.OK()})
}
