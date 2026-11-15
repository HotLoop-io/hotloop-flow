package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/instruments"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// The switch is turned on by someone holding instrument.write and by no one else,
// and the change is audited under their name.
func TestOnlyInstrumentWriteTurnsAnInstrumentOn(t *testing.T) {
	hash, err := config.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	cfg.Auth.Users = []config.User{
		{Username: "lee", PasswordHash: hash, Permissions: []string{PermStatusRead, PermInstrumentWrite}},
		{Username: "viewer", PasswordHash: hash, Permissions: []string{PermStatusRead}},
	}
	fs := store.NewFlowStore(filepath.Join(cfg.Data.Dir, "flows.json"))
	if _, err := fs.Load(); err != nil {
		t.Fatal(err)
	}
	sw, err := instruments.Open(cfg.InstrumentsPath())
	if err != nil {
		t.Fatal(err)
	}
	trail, err := audit.Open(cfg.AuditPath(), 1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer trail.Close()
	ts := &testServer{flows: fs}
	s := New(Deps{Config: cfg, Registry: node.Default, Flows: fs, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Runtime: nilRuntime, Version: "test", Audit: trail, Instruments: sw,
		Deploy: func(context.Context, DeployRequest) (DeployResult, error) { return DeployResult{}, nil }})
	ts.Server = httptest.NewServer(s.Handler())
	t.Cleanup(ts.Server.Close)

	_, lee := ts.login(t, "lee", testPassword)
	_, viewer := ts.login(t, "viewer", testPassword)
	body, _ := json.Marshal(map[string]any{"instrument": "10.0.0.5:5025", "allowed": true, "note": "bench 2"})

	if res, _ := ts.do(t, "PUT", "/instruments/writes", viewer, body); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a viewer turned a switch on: %d", res.StatusCode)
	}
	if sw.Allowed("10.0.0.5:5025") {
		t.Fatal("the refused request turned it on")
	}
	res, out := ts.do(t, "PUT", "/instruments/writes", lee, body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(out), `"instrument":"tcp://10.0.0.5:5025"`) || !sw.Allowed("10.0.0.5:5025") {
		t.Fatalf("holder: %d %s", res.StatusCode, out)
	}
	// Anyone who can see the status can see the switches.
	if res, out := ts.do(t, "GET", "/instruments", viewer, nil); res.StatusCode != http.StatusOK || !strings.Contains(string(out), `"allowed":true`) {
		t.Fatalf("list: %d %s", res.StatusCode, out)
	}

	rows, err := trail.Read(audit.Query{Event: audit.InstrumentSwitch})
	if err != nil || len(rows) != 1 || rows[0].User != "lee" || rows[0].Detail["instrument"] != "tcp://10.0.0.5:5025" || rows[0].Detail["allowed"] != true {
		t.Fatalf("audit %v %v", rows, err)
	}
}
