package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
)

// ciInstance is a running instance with a CI deploy token and a read-only
// token configured, and the CLI pointed at it through the environment the way
// a pipeline would point it.
func ciInstance(t *testing.T) (*e2e, string, string) {
	t.Helper()
	app, _ := newApp(t)
	deployTok, deployHash, err := config.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	readTok, readHash, _ := config.NewToken()
	app.cfg.Auth.Tokens = []config.Token{
		{Name: "ci", Hash: deployHash, Permissions: config.DeployTokenPermissions},
		{Name: "board", Hash: readHash, Permissions: []string{"flows.read"}},
	}
	e := serveApp(t, app, map[string][]string{"admin": {"*"}})
	t.Setenv("HOTLOOP_FLOW_URL", e.URL)
	t.Setenv("HOTLOOP_FLOW_TOKEN", deployTok)
	return e, deployTok, readTok
}

func runCLI(t *testing.T, cmd func([]string, *bytes.Buffer) error, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := cmd(args, &out)
	return out.String(), err
}

func deployCLI(args []string, out *bytes.Buffer) error { return cmdDeploy(args, out) }
func exportCLI(args []string, out *bytes.Buffer) error { return cmdExport(args, out) }

// The roadmap's own test: a pipeline deploys from a file with a token, the
// history shows the note and the token as the deployer, the live file comes
// back out byte for byte, and the same file again deploys nothing.
func TestFlowsAsCodeFromCI(t *testing.T) {
	e, _, _ := ciInstance(t)
	dir := t.TempDir()
	file := writeFile(t, dir, "flows.json", lineThree)

	out, err := runCLI(t, deployCLI, "-file", file, "-note", "PR #12: line 3 pressure check")
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if !strings.Contains(out, `+ switch "Pressure check" (sw1)`) || !strings.Contains(out, "Deployed: deployment 1") {
		t.Fatalf("deploy output:\n%s", out)
	}
	rec, ok := e.app.history.Latest()
	if !ok || rec.User != "token:ci" || rec.Note != "PR #12: line 3 pressure check" {
		t.Fatalf("history says %+v", rec)
	}

	// Export is the live file, exactly.
	exported := filepath.Join(dir, "exported.json")
	if out, err := runCLI(t, exportCLI, "-o", exported); err != nil {
		t.Fatalf("export: %v %s", err, out)
	}
	got, _ := os.ReadFile(exported)
	onDisk, _ := os.ReadFile(e.app.cfg.FlowPath())
	if !bytes.Equal(got, onDisk) {
		t.Fatalf("export is not the live file:\n%s\nvs\n%s", got, onDisk)
	}

	// The same flows again: nothing to deploy, nothing in the history.
	out, err = runCLI(t, deployCLI, "-file", exported, "-note", "merge with no flow change")
	if err != nil || !strings.Contains(out, "Nothing to deploy.") {
		t.Fatalf("redeploying the same file: %v\n%s", err, out)
	}
	if n := len(e.app.history.List(0)); n != 1 {
		t.Fatalf("a no-op deploy added a record: %d records", n)
	}

	// A real change shows up in the pipeline's log as the change it is.
	changed := writeFile(t, dir, "flows.json", strings.Replace(string(got), `"7.5"`, `"8.0"`, 1))
	out, err = runCLI(t, deployCLI, "-file", changed, "-dry-run")
	if err != nil || !strings.Contains(out, `rules[0].v: "7.5" -> "8.0"`) || !strings.Contains(out, "Dry run") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if n := len(e.app.history.List(0)); n != 1 {
		t.Fatal("a dry run deployed")
	}
	out, err = runCLI(t, deployCLI, "-file", changed, "-note", "raise the threshold")
	if err != nil || !strings.Contains(out, "Deployed: deployment 2") {
		t.Fatalf("deploying the change: %v\n%s", err, out)
	}

	// The history's first deployment exports exactly as it was.
	first := filepath.Join(dir, "first.json")
	if _, err := runCLI(t, exportCLI, "-deployment", "1", "-o", first); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(first); !bytes.Equal(b, got) {
		t.Fatal("exporting deployment 1 didn't give back deployment 1")
	}

	// And the same token can roll back, which is what a pipeline reverting a
	// merge does.
	code, body := e.call(t, "POST", "/deployments/1/rollback", os.Getenv("HOTLOOP_FLOW_TOKEN"), []byte(`{"note":"revert PR #13"}`))
	if code != http.StatusOK {
		t.Fatalf("rollback with the CI token: %d %s", code, body)
	}
	if rec, _ := e.app.history.Latest(); rec.User != "token:ci" || rec.RollbackOf != 1 {
		t.Fatalf("rollback record: %+v", rec)
	}
}

func TestDeployTokenCanOnlyDoItsJob(t *testing.T) {
	e, deployTok, readTok := ciInstance(t)
	for _, path := range []string{"/settings", "/runtime/stats", "/nodes"} {
		if code, _ := e.call(t, "GET", path, deployTok, nil); code != http.StatusForbidden {
			t.Errorf("GET %s with the deploy token: %d, want 403", path, code)
		}
	}
	if code, _ := e.call(t, "POST", "/inject/x", deployTok, nil); code != http.StatusForbidden {
		t.Errorf("inject with the deploy token: %d, want 403", code)
	}

	file := writeFile(t, t.TempDir(), "flows.json", lineThree)

	t.Setenv("HOTLOOP_FLOW_TOKEN", readTok)
	_, err := runCLI(t, deployCLI, "-file", file)
	if err == nil || !strings.Contains(err.Error(), "isn't allowed") {
		t.Fatalf("deploy with a read-only token: %v", err)
	}

	t.Setenv("HOTLOOP_FLOW_TOKEN", "hlf_"+strings.Repeat("0", 64))
	_, err = runCLI(t, deployCLI, "-file", file)
	if err == nil || !strings.Contains(err.Error(), "didn't accept the token") {
		t.Fatalf("deploy with an unknown token: %v", err)
	}

	t.Setenv("HOTLOOP_FLOW_TOKEN", "")
	if _, err := runCLI(t, deployCLI, "-file", file); err == nil || !strings.Contains(err.Error(), "hotloop-flow token") {
		t.Fatalf("deploy with no token: %v", err)
	}
	if len(e.app.history.List(0)) != 0 {
		t.Fatal("a refused deploy deployed")
	}
}

func TestDeployRefusesARaceAndABrokenFile(t *testing.T) {
	e, _, _ := ciInstance(t)
	dir := t.TempDir()
	file := writeFile(t, dir, "flows.json", lineThree)
	if _, err := runCLI(t, deployCLI, "-file", file); err != nil {
		t.Fatal(err)
	}
	stale := e.app.flowStore.Rev()
	if _, err := runCLI(t, deployCLI, "-file", writeFile(t, dir, "b.json", strings.Replace(lineThree, "7.5", "7.6", 1))); err != nil {
		t.Fatal(err)
	}

	// A plan reviewed against a revision that is no longer live.
	_, err := runCLI(t, deployCLI, "-file", writeFile(t, dir, "c.json", strings.Replace(lineThree, "7.5", "9.9", 1)), "-expect-rev", stale)
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != http.StatusConflict || !strings.Contains(err.Error(), "somebody deployed in between") {
		t.Fatalf("a stale -expect-rev: %v", err)
	}

	// A file that doesn't parse never leaves the pipeline.
	_, err = runCLI(t, deployCLI, "-file", writeFile(t, dir, "bad.json", `[{"id":"t1"}]`))
	if err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("a broken file: %v", err)
	}
	if n := len(e.app.history.List(0)); n != 2 {
		t.Fatalf("%d records, want 2", n)
	}
}

func TestTokenCommandPrintsATokenAndItsHash(t *testing.T) {
	var out bytes.Buffer
	if err := cmdToken([]string{"-name", "ci"}, &out); err != nil {
		t.Fatal(err)
	}
	tok := regexp.MustCompile(`hlf_[0-9a-f]{64}`).FindString(out.String())
	if tok == "" {
		t.Fatalf("no token in:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "HOTLOOP_FLOW_DEPLOY_TOKEN_HASH="+config.HashToken(tok)) ||
		!strings.Contains(out.String(), "name: ci") {
		t.Fatalf("the hash or the name is missing:\n%s", out.String())
	}

	// A second run is a different token.
	var again bytes.Buffer
	_ = cmdToken(nil, &again)
	if strings.Contains(again.String(), tok) {
		t.Fatal("two runs printed the same token")
	}
}

// The export of an instance that has never deployed is an empty flow file,
// not an error, so the first commit of a new repository works.
func TestExportOfAnEmptyInstance(t *testing.T) {
	ciInstance(t)
	out, err := runCLI(t, exportCLI)
	if err != nil || out != "[]\n" {
		t.Fatalf("export of nothing: %v %q", err, out)
	}
	var v []any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
}
