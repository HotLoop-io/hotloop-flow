package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// recorder stands in for the websocket hub. It is the real interface the
// application pumps events into, so a debug message arriving here is a debug
// message an editor would have seen.
type recorder struct {
	mu     sync.Mutex
	events []runtime.Event
}

func (r *recorder) Broadcast(e runtime.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// debugFrom waits for a debug event from a node and returns what it showed.
func (r *recorder) debugFrom(t *testing.T, nodeID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, e := range r.events {
			if e.Topic == runtime.TopicDebug && e.Data["id"] == nodeID {
				r.mu.Unlock()
				s, _ := e.Data["msg"].(string)
				return s
			}
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no debug message from %s within 5s", nodeID)
	return ""
}

func (r *recorder) reset() {
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
}

// newApp builds the application exactly as cmdServe does, on a temp data dir.
func newApp(t *testing.T) (*application, *recorder) {
	t.Helper()
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	cfg.Runtime.CloseTimeout = 2 * time.Second

	flowStore := store.NewFlowStore(cfg.FlowPath())
	flowStore.SetBackupGenerations(cfg.Data.BackupGenerations)
	rec := &recorder{}
	app := &application{
		cfg:       cfg,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		flowStore: flowStore,
		creds:     store.NewCredentialStore(cfg.CredentialsPath(), "a-secret-long-enough-to-count"),
		registry:  node.Default,
		contexts:  store.NewScopedContexts(),
		hub:       rec,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		app.stop(ctx)
	})
	return app, rec
}

func parse(t *testing.T, doc string) *engine.Flows {
	t.Helper()
	f, err := engine.ParseFlows([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// One debug node on one tab. The name rides along in the debug event, so it
// says which deploy is running.
func debugFlow(name string) string {
	return `[{"id":"t1","type":"tab","label":"Line 3"},` +
		`{"id":"d1","type":"debug","z":"t1","name":"` + name + `","complete":"payload","wires":[]}]`
}

func injectPayload(t *testing.T, app *application, nodeID, payload string) {
	t.Helper()
	rt := app.currentRuntime()
	if rt == nil {
		t.Fatal("no runtime is running")
	}
	if err := rt.Inject(nodeID, engine.WrapMsg(map[string]any{"payload": payload})); err != nil {
		t.Fatal(err)
	}
}

// The README promises this, and it's the only thing between a full disk and a
// stopped line: a deploy writes the flow file before it stops the old runtime,
// so a save that fails leaves the old flows running. Do it the obvious way
// round (stop, then save) and a save that fails takes everything down with it.
func TestDeployThatCannotSaveLeavesTheOldFlowsRunning(t *testing.T) {
	app, rec := newApp(t)
	ctx := context.Background()

	if _, err := app.deploy(ctx, parse(t, debugFlow("first")), ""); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	before := app.currentRuntime()

	// Sabotage the save: the flow file is now a directory, which no write
	// can replace. Works the same for root, which a permissions trick doesn't.
	path := app.cfg.FlowPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := app.deploy(ctx, parse(t, debugFlow("second")), "")
	if err == nil {
		t.Fatal("deploy succeeded with the flow file replaced by a directory")
	}

	if app.currentRuntime() != before {
		t.Fatal("a deploy that failed to save replaced the running runtime")
	}
	// The old flows have to still be moving messages, not just still be
	// referenced: a stopped runtime keeps its runner table.
	rec.reset()
	injectPayload(t, app, "d1", "still here")
	if got := rec.debugFrom(t, "d1"); got != "still here" {
		t.Fatalf("old flow answered %q, want %q", got, "still here")
	}
}

// The same promise from the other side: a deploy refused for a stale revision
// is a save that never happened, so nothing may stop.
func TestStaleDeployLeavesTheOldFlowsRunning(t *testing.T) {
	app, _ := newApp(t)
	ctx := context.Background()

	res, err := app.deploy(ctx, parse(t, debugFlow("first")), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.deploy(ctx, parse(t, debugFlow("second")), res.Rev); err != nil {
		t.Fatal(err)
	}
	running := app.currentRuntime()

	// A third editor still holding the first revision.
	_, err = app.deploy(ctx, parse(t, debugFlow("third")), res.Rev)
	if !errors.Is(err, store.ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
	if app.currentRuntime() != running {
		t.Fatal("a refused deploy disturbed the running flows")
	}
	data, err := os.ReadFile(app.cfg.FlowPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"second"`) || strings.Contains(string(data), `"third"`) {
		t.Fatalf("flow file after a refused deploy:\n%s", data)
	}
}

// And a deploy that does work: the file on disk is the new flows, the old
// runtime is gone, and the new one answers.
func TestDeployReplacesTheFileAndTheRuntime(t *testing.T) {
	app, rec := newApp(t)
	ctx := context.Background()

	if _, err := app.deploy(ctx, parse(t, debugFlow("first")), ""); err != nil {
		t.Fatal(err)
	}
	old := app.currentRuntime()
	res, err := app.deploy(ctx, parse(t, debugFlow("second")), "")
	if err != nil {
		t.Fatal(err)
	}
	if app.currentRuntime() == old {
		t.Fatal("deploy kept the old runtime")
	}

	data, err := os.ReadFile(app.cfg.FlowPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"second"`) {
		t.Fatalf("flow file does not hold the deployed flows:\n%s", data)
	}
	if res.Rev != app.flowStore.Rev() {
		t.Fatalf("deploy returned rev %s, store holds %s", res.Rev, app.flowStore.Rev())
	}
	if _, err := os.Stat(app.cfg.FlowPath() + ".bak.1"); err != nil {
		t.Fatalf("the previous flow file was not kept as a backup: %v", err)
	}

	rec.reset()
	injectPayload(t, app, "d1", "new")
	if got := rec.debugFrom(t, "d1"); got != "new" {
		t.Fatalf("new flow answered %q", got)
	}
}

// Credentials posted inline with a deploy never reach the flow file, and they
// are on disk, encrypted, by the time the deploy returns.
func TestDeploySplitsCredentialsOutOfTheFlowFile(t *testing.T) {
	app, _ := newApp(t)
	// A config node type this build doesn't know, so nothing dials out. Its
	// credentials are handled exactly like a broker's.
	doc := `[{"id":"t1","type":"tab","label":"Line 3"},` +
		`{"id":"b1","type":"vendor-broker","credentials":{"user":"line3","password":"hunter2-but-longer"}}]`
	if _, err := app.deploy(context.Background(), parse(t, doc), ""); err != nil {
		t.Fatal(err)
	}
	flowFile, err := os.ReadFile(app.cfg.FlowPath())
	if err != nil {
		t.Fatal(err)
	}
	credFile, err := os.ReadFile(filepath.Join(app.cfg.Data.Dir, app.cfg.Data.CredentialsFile))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"flow file": flowFile, "credential file": credFile} {
		if strings.Contains(string(data), "hunter2-but-longer") {
			t.Fatalf("the password is in the %s in plaintext", name)
		}
	}
	if got := app.creds.Get("b1")["password"]; got != "hunter2-but-longer" {
		t.Fatalf("credential store holds %q", got)
	}
}
