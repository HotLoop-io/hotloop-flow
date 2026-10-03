package gitmirror

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/history"
)

const (
	gitUser = "flow"
	gitPass = "a-token-from-the-git-server"
)

// gitServer is a real git server: git's own smart-HTTP backend behind basic
// auth, serving one bare repository. Pushes go through the same protocol a
// hosted git server speaks, and the repository is read back with git itself.
type gitServer struct {
	url  string
	bare string
	git  string
}

func newGitServer(t *testing.T) *gitServer {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git is needed for this test and isn't on the PATH")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "line3.git")
	g := &gitServer{bare: bare, git: gitBin}
	g.run(t, "", "init", "-q", "--bare", "-b", "main", bare)
	g.run(t, bare, "config", "http.receivepack", "true")

	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != gitUser || p != gitPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	g.url = srv.URL + "/line3.git"
	return g
}

func (g *gitServer) run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(g.git, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "none"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// commits lists the branch on the server, oldest first, as author|email|subject.
func (g *gitServer) commits(t *testing.T) []string {
	t.Helper()
	out := strings.TrimSpace(g.run(t, g.bare, "log", "--reverse", "--format=%an|%ae|%s", "main"))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// reject makes the server refuse every push, the way a protected branch or a
// full disk would.
func (g *gitServer) reject(t *testing.T, on bool) {
	t.Helper()
	hook := filepath.Join(g.bare, "hooks", "pre-receive")
	if !on {
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'branch is protected' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func openLog(t *testing.T, retain int) *history.Log {
	t.Helper()
	l, _, err := history.Open(t.TempDir(), retain)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func deploy(t *testing.T, l *history.Log, user, note, flows string) history.Record {
	t.Helper()
	r, err := l.Append(history.Record{Kind: history.KindDeploy, User: user, Note: note, Rev: "r", Remote: "10.0.0.7:5100", Flows: []byte(flows)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newMirror(t *testing.T, g *gitServer, l *history.Log, dir string, pass string) *Mirror {
	t.Helper()
	m, err := New(Config{URL: g.url, Username: gitUser, Password: pass, EmailDomain: "line3.example"},
		dir, l, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func flowsWith(threshold string) string {
	return "[\n    {\n        \"id\": \"sw1\",\n        \"type\": \"switch\",\n        \"v\": \"" + threshold + "\"\n    }\n]\n"
}

// The roadmap's test: three deploys make three commits on a real git server,
// each authored by the person who deployed it, with their note, and the file
// in the repository is the flow file byte for byte.
func TestEveryDeployIsACommitByWhoeverDeployedIt(t *testing.T) {
	g := newGitServer(t)
	l := openLog(t, 0)
	m := newMirror(t, g, l, filepath.Join(t.TempDir(), "clone"), gitPass)

	deploy(t, l, "dana", "line 3 goes live", flowsWith("7.5"))
	deploy(t, l, "sam", "raise the threshold", flowsWith("8.0"))
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := deploy(t, l, "token:ci", "PR #12", flowsWith("8.5"))
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"dana|dana@line3.example|line 3 goes live",
		"sam|sam@line3.example|raise the threshold",
		"token:ci|token-ci@line3.example|PR #12",
	}
	if got := g.commits(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commits on the server:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := g.run(t, g.bare, "show", "main:flows.json"); got != string(last.Flows) {
		t.Fatalf("the file in the repository is not the flow file:\n%q\nvs\n%q", got, last.Flows)
	}
	body := g.run(t, g.bare, "log", "-1", "--format=%B", "main")
	if !strings.Contains(body, "Flow-Deployment: 3") || !strings.Contains(body, "Deployed from 10.0.0.7:5100.") {
		t.Fatalf("commit message:\n%s", body)
	}
	// And the history between them is a real diff.
	if p := g.run(t, g.bare, "log", "-p", "-1", "main"); !strings.Contains(p, `-        "v": "8.0"`) || !strings.Contains(p, `+        "v": "8.5"`) {
		t.Fatalf("the last commit's diff:\n%s", p)
	}

	st := m.Status()
	if st.Pushed != 3 || st.Committed != 3 || st.LastError != "" || m.behind() != 0 {
		t.Fatalf("status = %+v, behind %d", st, m.behind())
	}
}

// A remote that refuses the push doesn't lose anything: the status and the
// metric say so, the commits wait in the clone, and they go out on the next
// push that works.
func TestARejectedPushWaitsAndCatchesUp(t *testing.T) {
	g := newGitServer(t)
	l := openLog(t, 0)
	m := newMirror(t, g, l, filepath.Join(t.TempDir(), "clone"), gitPass)

	deploy(t, l, "dana", "first", flowsWith("1"))
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	g.reject(t, true)
	deploy(t, l, "dana", "second", flowsWith("2"))
	deploy(t, l, "sam", "third", flowsWith("3"))
	if err := m.Sync(context.Background()); err == nil {
		t.Fatal("a refused push reported success")
	}
	st := m.Status()
	if st.LastError == "" || st.Pushed != 1 || st.Committed != 3 || m.behind() != 2 {
		t.Fatalf("after a refused push: %+v, behind %d", st, m.behind())
	}
	if got := metricValue(m, "hotloop_flow_git_mirror_push_failures_total"); got != 1 {
		t.Fatalf("failures metric = %v", got)
	}
	if got := metricValue(m, "hotloop_flow_git_mirror_behind_deployments"); got != 2 {
		t.Fatalf("behind metric = %v", got)
	}
	if n := len(g.commits(t)); n != 1 {
		t.Fatalf("the server has %d commits while refusing pushes", n)
	}

	g.reject(t, false)
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(g.commits(t)); n != 3 {
		t.Fatalf("after the server came back it has %d commits, want 3", n)
	}
	if st := m.Status(); st.LastError != "" || m.behind() != 0 {
		t.Fatalf("after catching up: %+v", st)
	}
}

func TestWrongCredentialsAreAnErrorNotAHang(t *testing.T) {
	g := newGitServer(t)
	l := openLog(t, 0)
	m := newMirror(t, g, l, filepath.Join(t.TempDir(), "clone"), "not-the-token")
	deploy(t, l, "dana", "first", flowsWith("1"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := m.Sync(ctx)
	if err == nil {
		t.Fatal("a push with the wrong token succeeded")
	}
	if strings.Contains(err.Error(), gitPass) || strings.Contains(m.Status().LastError, "not-the-token") {
		t.Fatal("the error carries a credential")
	}
}

// A restart, or a clone wiped off the volume, picks up where the repository
// says it got to: no duplicated commits, nothing skipped.
func TestPicksUpWhereTheRepositorySaysItGotTo(t *testing.T) {
	g := newGitServer(t)
	l := openLog(t, 0)
	clone := filepath.Join(t.TempDir(), "clone")
	deploy(t, l, "dana", "first", flowsWith("1"))
	if err := newMirror(t, g, l, clone, gitPass).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	deploy(t, l, "dana", "second", flowsWith("2"))
	if err := newMirror(t, g, l, clone, gitPass).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(clone); err != nil {
		t.Fatal(err)
	}
	deploy(t, l, "sam", "third", flowsWith("3"))
	if err := newMirror(t, g, l, clone, gitPass).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	subjects := []string{}
	for _, c := range g.commits(t) {
		subjects = append(subjects, c[strings.LastIndex(c, "|")+1:])
	}
	if strings.Join(subjects, ",") != "first,second,third" {
		t.Fatalf("commits: %v", subjects)
	}
}

// Retention can remove records before the mirror gets to them. The mirror
// commits what's left and doesn't stall on what isn't.
func TestRecordsRetentionRemovedAreSkipped(t *testing.T) {
	g := newGitServer(t)
	l := openLog(t, 1)
	m := newMirror(t, g, l, filepath.Join(t.TempDir(), "clone"), gitPass)
	deploy(t, l, "dana", "one", flowsWith("1"))
	deploy(t, l, "dana", "two", flowsWith("2"))
	deploy(t, l, "dana", "three", flowsWith("3"))
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := g.commits(t); len(got) != 1 || !strings.HasSuffix(got[0], "|three") {
		t.Fatalf("commits: %v", got)
	}
}

func TestRollbackAndBaselineSayWhatTheyAre(t *testing.T) {
	g := newGitServer(t)
	l := openLog(t, 0)
	m := newMirror(t, g, l, filepath.Join(t.TempDir(), "clone"), gitPass)
	if _, err := l.Append(history.Record{Kind: history.KindBaseline, Rev: "a", Flows: []byte(flowsWith("1"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(history.Record{Kind: history.KindRollback, RollbackOf: 1, User: "dana", Rev: "a",
		Note: "rollback to deployment 1: the new one broke the printer", Flows: []byte(flowsWith("1"))}); err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := g.run(t, g.bare, "log", "--format=%an|%s%n%b", "main")
	for _, want := range []string{"nobody signed in|baseline 1", "recorded at startup",
		"dana|rollback to deployment 1: the new one broke the printer", "Rollback to deployment 1."} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestValidateRefusesWhatCantWork(t *testing.T) {
	for _, bad := range []Config{
		{URL: "file:///srv/git/line3.git"},
		{URL: "ssh://git@git.example/line3.git"},
		{URL: "git@git.example:line3.git"},
		{URL: "https://dana:hunter2@git.example/line3.git"},
		{URL: "https://git.example/line3.git", Path: "../../etc/passwd"},
		{URL: "https://git.example/line3.git", Path: "/flows.json"},
	} {
		if err := Validate(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the refusal repeats the password: %v", err)
		}
	}
	if err := Validate(Config{URL: "https://git.example/plant/line3.git", Path: "flows/line3.json"}); err != nil {
		t.Fatalf("refused a good one: %v", err)
	}
}

// Notify never blocks, however many deploys pile up while a push is slow.
func TestNotifyNeverBlocks(t *testing.T) {
	l := openLog(t, 0)
	m, err := New(Config{URL: "https://git.example/line3.git"}, t.TempDir(), l, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			m.Notify()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked")
	}
}

func metricValue(m *Mirror, name string) float64 {
	for _, f := range m.Families() {
		if f.Name == name {
			s := f.Collect()
			if len(s) == 1 {
				return s[0].Value
			}
		}
	}
	return -1
}
