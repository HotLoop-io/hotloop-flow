package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/gitmirror"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// A git server that takes ten seconds to say no. The mirror is asynchronous so
// that this costs a deploy nothing: the deploy returns, the line runs, and the
// status says the mirror is behind.
func TestASlowBrokenMirrorNeverHoldsUpADeploy(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(10 * time.Second):
		case <-r.Context().Done():
		}
		http.Error(w, "the git server is having a day", http.StatusServiceUnavailable)
	}))
	t.Cleanup(slow.Close)

	app, rec := newApp(t)
	m, err := gitmirror.New(gitmirror.Config{URL: slow.URL + "/line3.git"},
		filepath.Join(app.cfg.Data.Dir, "git-mirror"), app.history, app.log)
	if err != nil {
		t.Fatal(err)
	}
	app.mirror = m
	ctx, cancel := context.WithCancel(context.Background())
	// Stopped and waited for before the data directory is removed: the push
	// loop writes its clone there, and a loop still mid-clone during cleanup
	// is how this test was flaky on its first run in CI.
	done := make(chan struct{})
	go func() {
		m.Run(ctx, time.Minute)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	start := time.Now()
	res, err := app.deploy(context.Background(), api.DeployRequest{Flows: parse(t, debugFlow("first"))})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("a deploy took %v with a slow git server behind it", took)
	}
	if res.Deployment != 1 {
		t.Fatalf("deployment = %d", res.Deployment)
	}
	injectPayload(t, app, "d1", "running")
	if got := rec.debugFrom(t, "d1"); got != "running" {
		t.Fatalf("flows after the deploy: %q", got)
	}

	// The API and the metrics say where the mirror is.
	srv := api.New(api.Deps{
		Config: app.cfg, Registry: node.Default, Flows: app.flowStore, Credentials: app.creds,
		History: app.history, Logger: app.log, Runtime: app.currentRuntime, Deploy: app.deploy,
		Mirror: func() any { return m.Status() }, Metrics: m.Families(), Version: "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	res2, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(res2.Body)
	for _, name := range []string{"hotloop_flow_git_mirror_behind_deployments 1", "hotloop_flow_git_mirror_push_failures_total"} {
		if !strings.Contains(b.String(), name) {
			t.Fatalf("/metrics lacks %q", name)
		}
	}
}

func TestDeploymentsShowTheMirror(t *testing.T) {
	app, _ := newApp(t)
	m, err := gitmirror.New(gitmirror.Config{URL: "https://git.example/plant/line3.git", Branch: "flows"},
		filepath.Join(app.cfg.Data.Dir, "git-mirror"), app.history, app.log)
	if err != nil {
		t.Fatal(err)
	}
	app.mirror = m
	e := serveAppWith(t, app, map[string][]string{"admin": {"*"}}, func(d *api.Deps) {
		d.Mirror = func() any { return m.Status() }
	})
	_, out := e.call(t, "GET", "/deployments", e.login(t, "admin"), nil)
	var body struct {
		Mirror gitmirror.Status `json:"mirror"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if body.Mirror.URL != "https://git.example/plant/line3.git" || body.Mirror.Branch != "flows" {
		t.Fatalf("mirror status: %s", out)
	}
}
