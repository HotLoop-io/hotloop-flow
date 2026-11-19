package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/fence"
	"github.com/HotLoop-io/hotloop-flow/internal/lease"
)

// failoverHandle is the running lease and what closing it undoes.
type failoverHandle struct {
	*lease.Manager
	db      *lease.Postgres
	restore func()
}

// Close gives the lease up, so a standby takes over at once, and stops fencing.
func (f *failoverHandle) Close() {
	f.Manager.Close()
	f.restore()
	f.db.Close()
}

// startFailover turns failover on when the settings ask for it. It opens the
// lease database, takes or fails to take the lease once, and installs the fence
// every equipment write asks. It returns nil when failover is off.
func startFailover(ctx context.Context, cfg config.Config, log *slog.Logger) (*failoverHandle, error) {
	f := cfg.Failover
	if !f.Enabled() {
		return nil, nil
	}
	raw, err := os.ReadFile(f.URLFile)
	if err != nil {
		return nil, fmt.Errorf("reading failover.urlFile: %w", err)
	}
	octx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	db, err := lease.OpenPostgres(octx, strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("opening the lease database: %w", err)
	}
	renew := f.RenewEvery
	if renew <= 0 {
		renew = f.TTL / 3
	}
	holder := f.Holder
	if holder == "" {
		holder = lease.Hostname()
	}
	m, err := lease.New(lease.Config{
		Store: db, Name: f.Name, Holder: holder, TTL: f.TTL, Renew: renew, Margin: f.WriteMargin,
		Attempt: 2 * time.Second, Log: log,
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	restore := fence.Set(m)
	m.Start(ctx)
	st := m.Status()
	log.Info("failover is on", "lease", f.Name, "holder", holder, "role", st.Role, "owner", st.Owner, "ttl", f.TTL.String(), "writeMargin", f.WriteMargin.String())
	return &failoverHandle{Manager: m, db: db, restore: restore}, nil
}

// failoverStatus is the handle as the API wants it, or nothing when failover is
// off, which leaves the route out.
func failoverStatus(f *failoverHandle) api.FailoverStatus {
	if f == nil {
		return nil
	}
	return f.Manager
}
