package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/fence"
	"github.com/HotLoop-io/hotloop-flow/internal/lease"
)

type stubFailover struct{}

func (stubFailover) Status() lease.Status {
	return lease.Status{Role: lease.Standby, Holder: "b", Name: "flow", Owner: "a", Current: 3, Refused: map[string]int64{"modbus write": 2}}
}

// The status route says who writes, and is not there at all without failover.
func TestTheFailoverRouteSaysWhoWrites(t *testing.T) {
	app, _ := newApp(t)
	e := serveAppWith(t, app, map[string][]string{"admin": {"*"}, "nobody": {"flows.read"}}, func(d *api.Deps) { d.Failover = stubFailover{} })
	code, out := e.call(t, "GET", "/failover", e.login(t, "admin"), nil)
	var st lease.Status
	if code != 200 || json.Unmarshal(out, &st) != nil || st.Role != lease.Standby || st.Owner != "a" || st.Refused["modbus write"] != 2 {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ := e.call(t, "GET", "/failover", e.login(t, "nobody"), nil); code != http.StatusForbidden {
		t.Errorf("without status.read: %d", code)
	}
	app2, _ := newApp(t)
	bare := serveApp(t, app2, map[string][]string{"admin": {"*"}})
	if code, _ := bare.call(t, "GET", "/failover", bare.login(t, "admin"), nil); code != http.StatusNotFound {
		t.Errorf("without failover the route answered %d", code)
	}
}

// Failover starts from the settings against a real PostgreSQL: the first install is
// the writer, the fence is installed while it runs, and closing gives the lease up
// and takes the fence away.
func TestFailoverStartsFromTheSettingsAndCloseGivesTheLeaseUp(t *testing.T) {
	addr := os.Getenv("HOTLOOP_FLOW_TEST_PG")
	if addr == "" {
		t.Skip("set HOTLOOP_FLOW_TEST_PG=host:port to run this against a real PostgreSQL (user flow, password flowtest, database flowtest)")
	}
	dir := t.TempDir()
	urlFile := writeFile(t, dir, "lease.url", "postgres://flow:flowtest@"+addr+"/flowtest?sslmode=disable\n")
	name := "start-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	mk := func(holder string) config.Config {
		cfg := config.Default()
		cfg.Failover = config.Failover{URLFile: urlFile, Name: name, Holder: holder, TTL: 15 * time.Second, WriteMargin: 5 * time.Second}
		return cfg
	}
	a, err := startFailover(context.Background(), mk("a"), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if st := a.Status(); st.Role != lease.Active || st.Epoch != 1 {
		t.Fatalf("the first install: %+v", st)
	}
	if err := fence.Check("modbus write"); err != nil {
		t.Fatalf("the writer was refused: %v", err)
	}
	a.Close()
	if fence.On() {
		t.Fatal("closing left the fence set")
	}
	// It gave the lease up, so the next install has it at once and the number goes up.
	b, err := startFailover(context.Background(), mk("b"), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if st := b.Status(); st.Role != lease.Active || st.Epoch != 2 {
		t.Fatalf("after a clean release: %+v", st)
	}
	_ = filepath.Join
}
