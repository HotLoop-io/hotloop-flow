package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/history"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

const e2ePassword = "correct-horse-battery"

// e2e is the admin API wired to the real application, the way cmdServe wires
// it: a deploy over HTTP runs the real deploy, which writes the real flow file,
// credentials and deployment log.
type e2e struct {
	*httptest.Server
	app *application
}

func serveApp(t *testing.T, app *application, users map[string][]string) *e2e {
	t.Helper()
	hash, err := config.HashPassword(e2ePassword)
	if err != nil {
		t.Fatal(err)
	}
	app.cfg.Auth.Users = nil
	for name, perms := range users {
		app.cfg.Auth.Users = append(app.cfg.Auth.Users, config.User{Username: name, PasswordHash: hash, Permissions: perms})
	}
	srv := api.New(api.Deps{
		Config:      app.cfg,
		Registry:    node.Default,
		Flows:       app.flowStore,
		Credentials: app.creds,
		History:     app.history,
		Logger:      app.log,
		Runtime:     app.currentRuntime,
		Deploy:      app.deploy,
		Rollback:    app.rollback,
		Version:     "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &e2e{Server: ts, app: app}
}

func (e *e2e) login(t *testing.T, user string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": e2ePassword})
	res, err := http.Post(e.URL+"/auth/token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("login as %s: %d %v", user, res.StatusCode, err)
	}
	return tok.AccessToken
}

func (e *e2e) call(t *testing.T, method, path, token string, body []byte, hdr ...string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

type listing struct {
	Deployments []history.Record `json:"deployments"`
	Retain      int              `json:"retain"`
	Current     string           `json:"current"`
}

func (e *e2e) list(t *testing.T, token string) listing {
	t.Helper()
	code, out := e.call(t, "GET", "/deployments", token, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /deployments: %d %s", code, out)
	}
	var l listing
	if err := json.Unmarshal(out, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func tabFlow(label string) string {
	return `[{"id":"t1","type":"tab","label":"` + label + `"}]`
}

// The roadmap's own test: three deploys by two people, listed newest first,
// each with who, the note, and the revision it replaced.
func TestDeploysAreRecordedWithWhoWhyAndWhat(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"dana": {"*"}, "sam": {"flows.*"}})
	dana, sam := e.login(t, "dana"), e.login(t, "sam")

	var files [][]byte
	deploy := func(token string, body []byte, hdr ...string) {
		t.Helper()
		code, out := e.call(t, "POST", "/flows", token, body, hdr...)
		if code != http.StatusOK {
			t.Fatalf("deploy: %d %s", code, out)
		}
		if !strings.Contains(string(out), `"deployment":`) {
			t.Fatalf("the deploy response doesn't name its deployment: %s", out)
		}
		data, err := os.ReadFile(app.cfg.FlowPath())
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, data)
	}

	deploy(dana, []byte(tabFlow("one")))
	// The note rides in the document, which is how the editor sends it: a
	// browser won't put this in a header.
	deploy(sam, []byte(`{"flows":`+tabFlow("two")+`,"note":"Druckgrenze für Linie 3 angehoben"}`))
	deploy(dana, []byte(tabFlow("three")), "HotLoop-Flow-Deployment-Note", "raise the alarm delay")

	l := e.list(t, dana)
	if len(l.Deployments) != 3 {
		t.Fatalf("%d records, want 3", len(l.Deployments))
	}
	want := []struct {
		user, note string
	}{
		{"dana", "raise the alarm delay"},
		{"sam", "Druckgrenze für Linie 3 angehoben"},
		{"dana", ""},
	}
	for i, w := range want {
		r := l.Deployments[i]
		if r.User != w.user || r.Note != w.note || r.Kind != history.KindDeploy {
			t.Errorf("deployment %d = %s %q %s, want %s %q deploy", r.Seq, r.User, r.Note, r.Kind, w.user, w.note)
		}
		if r.Remote == "" {
			t.Errorf("deployment %d has no remote address", r.Seq)
		}
	}
	if l.Deployments[0].ParentRev != l.Deployments[1].Rev || l.Deployments[1].ParentRev != l.Deployments[2].Rev {
		t.Error("each deployment's parent is not the one before it")
	}
	if l.Current != l.Deployments[0].Rev {
		t.Errorf("current rev %s is not the newest deployment's %s", l.Current, l.Deployments[0].Rev)
	}

	// Each record holds the exact bytes that were on disk after its deploy.
	for i, rec := range []history.Record{l.Deployments[2], l.Deployments[1], l.Deployments[0]} {
		seq := strconv.FormatInt(rec.Seq, 10)
		code, out := e.call(t, "GET", "/deployments/"+seq+"/flows", sam, nil)
		if code != http.StatusOK {
			t.Fatalf("GET deployment %d flows: %d", rec.Seq, code)
		}
		if !bytes.Equal(out, files[i]) {
			t.Errorf("deployment %d flows are not the bytes that were written:\n%s\nvs\n%s", rec.Seq, out, files[i])
		}

		// The JSON view carries the same document, parsed.
		code, out = e.call(t, "GET", "/deployments/"+seq, sam, nil)
		if code != http.StatusOK {
			t.Fatalf("GET deployment %d: %d", rec.Seq, code)
		}
		var full struct {
			Seq   int64           `json:"seq"`
			Flows json.RawMessage `json:"flows"`
		}
		if err := json.Unmarshal(out, &full); err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		_ = json.Compact(&compact, files[i])
		if full.Seq != rec.Seq || !bytes.Equal(full.Flows, compact.Bytes()) {
			t.Errorf("deployment %d JSON view = seq %d %s", rec.Seq, full.Seq, full.Flows)
		}
	}
}

func TestReadingHistoryNeedsFlowsRead(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"admin": {"*"}, "board": {"status.read"}})
	admin, board := e.login(t, "admin"), e.login(t, "board")
	if code, _ := e.call(t, "POST", "/flows", admin, []byte(tabFlow("x"))); code != http.StatusOK {
		t.Fatal("deploy failed")
	}
	for _, path := range []string{"/deployments", "/deployments/1"} {
		if code, _ := e.call(t, "GET", path, board, nil); code != http.StatusForbidden {
			t.Errorf("GET %s without flows.read: %d, want 403", path, code)
		}
		if code, _ := e.call(t, "GET", path, admin, nil); code != http.StatusOK {
			t.Errorf("GET %s as admin: %d", path, code)
		}
	}
	if code, _ := e.call(t, "GET", "/deployments/99", admin, nil); code != http.StatusNotFound {
		t.Errorf("a deployment that never existed: %d, want 404", code)
	}
	if code, _ := e.call(t, "GET", "/deployments/abc", admin, nil); code != http.StatusBadRequest {
		t.Errorf("a deployment that isn't a number: %d, want 400", code)
	}
}

// Credentials go into every record, so a rollback can bring them back, and
// never in plaintext: not in the record on disk, not over the API.
func TestRecordsCarryCredentialsEncryptedOnly(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"admin": {"*"}})
	admin := e.login(t, "admin")
	doc := `[{"id":"t1","type":"tab","label":"x"},` +
		`{"id":"b1","type":"vendor-broker","credentials":{"password":"hunter2-but-longer"}}]`
	if code, out := e.call(t, "POST", "/flows", admin, []byte(doc)); code != http.StatusOK {
		t.Fatalf("deploy: %d %s", code, out)
	}

	files, _ := filepath.Glob(filepath.Join(app.cfg.HistoryDir(), "*.json"))
	if len(files) != 1 {
		t.Fatalf("%d record files, want 1", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2-but-longer") {
		t.Fatal("the deployment record holds the password in plaintext")
	}

	code, out := e.call(t, "GET", "/deployments/1", admin, nil)
	if code != http.StatusOK || strings.Contains(string(out), "hunter2") || strings.Contains(string(out), "aes256gcm") {
		t.Fatalf("the API handed out credentials: %s", out)
	}
	if !strings.Contains(string(out), `"hasCredentials":true`) {
		t.Fatalf("the record doesn't say it captured credentials: %s", out)
	}

	// And they really are in there, under the secret.
	rec, err := app.history.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.creds.Restore(rec.Credentials); err != nil {
		t.Fatal(err)
	}
	if got := app.creds.Get("b1")["password"]; got != "hunter2-but-longer" {
		t.Fatalf("restored password = %q", got)
	}
}

// A flow file that was there before the log, or changed by hand while the
// process was down, becomes a baseline record at startup. A restart with
// nothing changed adds nothing.
func TestStartupRecordsWhatIsOnDisk(t *testing.T) {
	app, _ := newApp(t)
	cfg := app.cfg
	if err := os.WriteFile(cfg.FlowPath(), []byte(tabFlow("by hand")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	start := func() *application {
		a, _ := newAppIn(t, cfg)
		a.recordBaseline(a.flowStore.Rev())
		return a
	}

	a := start()
	l := a.history.List(0)
	if len(l) != 1 || l[0].Kind != history.KindBaseline || l[0].Rev != a.flowStore.Rev() {
		t.Fatalf("after the first start: %+v", l)
	}
	rec, _ := a.history.Get(l[0].Seq)
	if !bytes.Equal(rec.Flows, []byte(tabFlow("by hand")+"\n")) {
		t.Fatalf("baseline flows = %q", rec.Flows)
	}

	if l := start().history.List(0); len(l) != 1 {
		t.Fatalf("a restart with nothing changed added a record: %+v", l)
	}

	if err := os.WriteFile(cfg.FlowPath(), []byte(tabFlow("edited")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l = start().history.List(0)
	if len(l) != 2 || l[0].Kind != history.KindBaseline || !strings.Contains(l[0].Note, "outside Flow") {
		t.Fatalf("a hand edit was not recorded: %+v", l)
	}
	if l[0].ParentRev != l[1].Rev {
		t.Fatal("the hand edit's record doesn't point at what it replaced")
	}
}

// A fresh volume has nothing to record.
func TestFreshStartRecordsNothing(t *testing.T) {
	app, _ := newApp(t)
	app.recordBaseline(app.flowStore.Rev())
	if l := app.history.List(0); len(l) != 0 {
		t.Fatalf("recorded %+v on an empty volume", l)
	}
}

// The deploy goes ahead when the log can't be written, because the flow file
// is already saved. But it says so, out loud, in the response.
func TestAnUnwritableLogIsAWarningNotAStoppedLine(t *testing.T) {
	app, rec := newApp(t)
	dir := app.cfg.HistoryDir()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := app.deploy(context.Background(), api.DeployRequest{Flows: parse(t, debugFlow("first"))})
	if err != nil {
		t.Fatalf("the deploy failed because the log couldn't be written: %v", err)
	}
	if res.Deployment != 0 {
		t.Fatalf("deployment = %d for a record that wasn't written", res.Deployment)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "deployment log could not be written") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning about the missing record: %v", res.Warnings)
	}
	injectPayload(t, app, "d1", "running")
	if got := rec.debugFrom(t, "d1"); got != "running" {
		t.Fatalf("the new flows aren't running: %q", got)
	}
}

func TestRetentionComesFromConfig(t *testing.T) {
	app, _ := newApp(t)
	cfg := app.cfg
	cfg.History.Retain = 2
	app, _ = newAppIn(t, cfg)
	for _, label := range []string{"a", "b", "c", "d"} {
		if _, err := app.deploy(context.Background(), api.DeployRequest{Flows: parse(t, tabFlow(label))}); err != nil {
			t.Fatal(err)
		}
	}
	l := app.history.List(0)
	if len(l) != 2 || l[0].Seq != 4 || l[1].Seq != 3 {
		t.Fatalf("kept %+v, want 4 and 3", l)
	}
}

func TestAnOverlongNoteIsRefusedBeforeAnythingDeploys(t *testing.T) {
	app, _ := newApp(t)
	e := serveApp(t, app, map[string][]string{"admin": {"*"}})
	admin := e.login(t, "admin")
	body, _ := json.Marshal(map[string]any{"flows": json.RawMessage(tabFlow("x")), "note": strings.Repeat("n", history.MaxNoteLength+1)})
	if code, _ := e.call(t, "POST", "/flows", admin, body); code != http.StatusBadRequest {
		t.Fatalf("an overlong note: %d, want 400", code)
	}
	if app.flowStore.Bytes() != nil || len(app.history.List(0)) != 0 {
		t.Fatal("a refused deploy wrote something")
	}
}
