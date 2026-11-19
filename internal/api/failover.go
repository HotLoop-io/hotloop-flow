package api

import (
	"net/http"

	"github.com/HotLoop-io/hotloop-flow/internal/lease"
)

// FailoverStatus is what this install says about the lease it shares with its
// standby: who writes to equipment, under which number, and what it has held back.
type FailoverStatus interface {
	Status() lease.Status
}

func (s *Server) failoverRoutes() {
	if s.deps.Failover == nil {
		return
	}
	s.mux.Handle("GET "+s.path("/failover"), s.auth(PermStatusRead, s.handleFailover))
}

func (s *Server) handleFailover(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Failover.Status())
}
