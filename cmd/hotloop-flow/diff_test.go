package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain lets git run this test binary as hotloop-flow itself. With the
// variable set it is main(), arguments and all, which is exactly what git's
// external diff driver will call in the field.
func TestMain(m *testing.M) {
	if os.Getenv("HOTLOOP_FLOW_TEST_RUN_AS_MAIN") == "1" {
		os.Args = append([]string{"hotloop-flow"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const lineThree = `[
    {"id":"t1","type":"tab","label":"Line 3"},
    {"id":"sw1","type":"switch","z":"t1","name":"Pressure check","property":"payload","rules":[{"t":"gte","v":"7.5","vt":"num"}],"x":300,"y":80,"wires":[[]]}
]
`

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiffCommand(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.json", lineThree)
	b := writeFile(t, dir, "b.json", strings.Replace(lineThree, `"v":"7.5"`, `"v":"8.0"`, 1))
	moved := writeFile(t, dir, "moved.json", strings.Replace(lineThree, `"x":300`, `"x":340`, 1))

	var out bytes.Buffer
	err := cmdDiff([]string{a, b}, &out)
	var code exitCode
	if !errors.As(err, &code) || code != 1 {
		t.Fatalf("err = %v, want exit status 1 for a difference", err)
	}
	if !strings.Contains(out.String(), `rules[0].v: "7.5" -> "8.0"`) {
		t.Fatalf("output:\n%s", out.String())
	}

	out.Reset()
	if err := cmdDiff([]string{a, a}, &out); err != nil {
		t.Fatalf("a file against itself: %v", err)
	}
	if out.String() != "No changes.\n" {
		t.Fatalf("output: %q", out.String())
	}

	out.Reset()
	_ = cmdDiff([]string{a, moved}, &out)
	if !strings.Contains(out.String(), "Layout only") {
		t.Fatalf("a drag reads as:\n%s", out.String())
	}

	out.Reset()
	_ = cmdDiff([]string{"-json", a, b}, &out)
	var res struct {
		Summary struct{ Changed int } `json:"summary"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Summary.Changed != 1 {
		t.Fatalf("-json: %v %s", err, out.String())
	}

	if err := cmdDiff([]string{a}, &out); err == nil {
		t.Fatal("one file was accepted")
	}
	if err := cmdDiff([]string{a, writeFile(t, dir, "bad.json", "{oops")}, &out); err == nil {
		t.Fatal("an unparseable file was accepted")
	}
}

// Real git, calling this binary as its external diff driver, through
// .gitattributes, the way the docs say to set it up.
func TestDiffAsGitsExternalDiff(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git is needed for this test and isn't on the PATH")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"HOTLOOP_FLOW_TEST_RUN_AS_MAIN=1",
			"GIT_CONFIG_GLOBAL="+filepath.Join(repo, ".no-global"),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=line3", "GIT_AUTHOR_EMAIL=line3@example.invalid",
			"GIT_COMMITTER_NAME=line3", "GIT_COMMITTER_EMAIL=line3@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	git("init", "-q")
	git("config", "diff.hotloop-flow.command", "'"+filepath.ToSlash(self)+"' diff")
	writeFile(t, repo, ".gitattributes", "flows.json diff=hotloop-flow\n")
	writeFile(t, repo, "flows.json", lineThree)
	git("add", ".")
	git("commit", "-q", "-m", "line 3")

	writeFile(t, repo, "flows.json", strings.Replace(lineThree, `"v":"7.5"`, `"v":"8.0"`, 1))
	out := git("diff")
	for _, want := range []string{"hotloop-flow diff flows.json", `~ switch "Pressure check" (sw1)`, `rules[0].v: "7.5" -> "8.0"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("git diff output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "@@") {
		t.Fatalf("git fell back to a line diff:\n%s", out)
	}

	// The same thing as a difftool, which hands over two whole files.
	git("config", "difftool.hotloop-flow.cmd", "'"+filepath.ToSlash(self)+`' diff "$LOCAL" "$REMOTE"`)
	if out := git("difftool", "-y", "-t", "hotloop-flow"); !strings.Contains(out, `rules[0].v: "7.5" -> "8.0"`) {
		t.Fatalf("git difftool output:\n%s", out)
	}

	// A deleted flow file is /dev/null on the new side.
	if err := os.Remove(filepath.Join(repo, "flows.json")); err != nil {
		t.Fatal(err)
	}
	if out := git("diff"); !strings.Contains(out, "2 removed") {
		t.Fatalf("a deleted flow file reads as:\n%s", out)
	}
}

func TestDiffOverTheAPI(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"admin": {"*"}, "board": {"status.read"}})
	admin, board := e.login(t, "admin"), e.login(t, "board")

	for _, doc := range []string{lineThree, strings.Replace(lineThree, `"v":"7.5"`, `"v":"8.0"`, 1)} {
		if code, out := e.call(t, "POST", "/flows", admin, []byte(doc)); code != http.StatusOK {
			t.Fatalf("deploy: %d %s", code, out)
		}
	}

	code, out := e.call(t, "GET", "/deployments/1/diff/2", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("diff: %d %s", code, out)
	}
	var d struct {
		From, To string
		Summary  struct{ Changed int } `json:"summary"`
		Entries  []struct {
			ID    string `json:"id"`
			Props []struct {
				Path     string
				Old, New any
			} `json:"props"`
		} `json:"entries"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatal(err)
	}
	if d.Summary.Changed != 1 || len(d.Entries) != 1 || d.Entries[0].ID != "sw1" ||
		d.Entries[0].Props[0].Path != "rules[0].v" || d.Entries[0].Props[0].New != "8.0" {
		t.Fatalf("diff = %s", out)
	}
	if !strings.Contains(d.Text, `"7.5" -> "8.0"`) || d.From == "" || d.From == d.To {
		t.Fatalf("diff text or revs: %s", out)
	}

	// What review-and-deploy shows: live against a document nobody deployed.
	pending := strings.Replace(lineThree, `"v":"7.5"`, `"v":"9.5"`, 1)
	code, out = e.call(t, "POST", "/flows/diff", admin, []byte(pending))
	if code != http.StatusOK || !strings.Contains(string(out), `\"8.0\" -> \"9.5\"`) {
		t.Fatalf("pending diff: %d %s", code, out)
	}
	if app.flowStore.Bytes() == nil || strings.Contains(string(app.flowStore.Bytes()), "9.5") {
		t.Fatal("diffing a pending document deployed it")
	}

	if code, _ := e.call(t, "GET", "/deployments/1/diff/2", board, nil); code != http.StatusForbidden {
		t.Errorf("diff without flows.read: %d", code)
	}
	if code, _ := e.call(t, "POST", "/flows/diff", board, []byte(pending)); code != http.StatusForbidden {
		t.Errorf("pending diff without flows.read: %d", code)
	}
	if code, _ := e.call(t, "GET", "/deployments/1/diff/9", admin, nil); code != http.StatusNotFound {
		t.Errorf("diff against a deployment that doesn't exist: %d", code)
	}
	if code, _ := e.call(t, "POST", "/flows/diff", admin, []byte("{oops")); code != http.StatusBadRequest {
		t.Errorf("pending diff of garbage: %d", code)
	}
}
