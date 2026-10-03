package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

const testPassword = "correct-horse-battery"

// nilRuntime is a process whose flows never started. Handlers that need a
// runtime answer 503, which is past the permission check.
func nilRuntime() *runtime.Runtime { return nil }

// testServer is the admin API over a real flow store on a temp directory. The
// deploy function saves through the store exactly as the binary's does, so a
// 409 here is the store's revision check, not a canned answer.
type testServer struct {
	*httptest.Server
	flows *store.FlowStore
}

func newTestServer(t *testing.T, users map[string][]string, mutate ...func(*config.Config)) *testServer {
	t.Helper()
	hash, err := config.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	for name, perms := range users {
		cfg.Auth.Users = append(cfg.Auth.Users, config.User{Username: name, PasswordHash: hash, Permissions: perms})
	}
	for _, m := range mutate {
		m(&cfg)
	}

	fs := store.NewFlowStore(filepath.Join(cfg.Data.Dir, "flows.json"))
	if _, err := fs.Load(); err != nil {
		t.Fatal(err)
	}
	s := New(Deps{
		Config:   cfg,
		Registry: node.Default,
		Flows:    fs,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Runtime:  nilRuntime,
		Deploy: func(_ context.Context, f *engine.Flows, rev string) (DeployResult, error) {
			newRev, err := fs.Save(f, rev)
			return DeployResult{Rev: newRev}, err
		},
		Version: "test",
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &testServer{Server: srv, flows: fs}
}

func (ts *testServer) do(t *testing.T, method, path, token string, body []byte, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, out
}

func (ts *testServer) login(t *testing.T, user, pass string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	res, out := ts.do(t, "POST", "/auth/token", "", body)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(out, &tok)
	return res.StatusCode, tok.AccessToken
}

func (ts *testServer) mustLogin(t *testing.T, user string) string {
	t.Helper()
	code, tok := ts.login(t, user, testPassword)
	if code != http.StatusOK || tok == "" {
		t.Fatalf("login as %s: status %d, token %q", user, code, tok)
	}
	return tok
}

func TestLoginIssuesATokenThatWorks(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	tok := ts.mustLogin(t, "admin")

	res, _ := ts.do(t, "GET", "/flows", tok, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /flows with a fresh token: %d", res.StatusCode)
	}
	res, _ = ts.do(t, "GET", "/flows", "", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /flows with no token: %d, want 401", res.StatusCode)
	}
	res, _ = ts.do(t, "GET", "/flows", strings.Repeat("0", 64), nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /flows with a made-up token: %d, want 401", res.StatusCode)
	}
}

// A wrong password and an unknown user get the same answer, word for word. A
// different message for each is a username oracle.
func TestFailedLoginsLookTheSame(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})

	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "not-the-password"})
	wrongPass, wrongPassBody := ts.do(t, "POST", "/auth/token", "", body)
	body, _ = json.Marshal(map[string]string{"username": "nobody", "password": testPassword})
	noUser, noUserBody := ts.do(t, "POST", "/auth/token", "", body)

	if wrongPass.StatusCode != http.StatusUnauthorized || noUser.StatusCode != http.StatusUnauthorized {
		t.Fatalf("statuses %d and %d, want 401 for both", wrongPass.StatusCode, noUser.StatusCode)
	}
	if !bytes.Equal(wrongPassBody, noUserBody) {
		t.Fatalf("a wrong password says %s and an unknown user says %s", wrongPassBody, noUserBody)
	}
}

func TestLoginRejectsAGarbageBody(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	res, _ := ts.do(t, "POST", "/auth/token", "", []byte("{not json"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", res.StatusCode)
	}
}

func TestRevokedTokenStopsWorking(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	tok := ts.mustLogin(t, "admin")

	res, _ := ts.do(t, "POST", "/auth/revoke", tok, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", res.StatusCode)
	}
	res, _ = ts.do(t, "GET", "/flows", tok, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked token still reads flows: %d", res.StatusCode)
	}
}

func TestTokenExpires(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}}, func(c *config.Config) {
		c.Auth.SessionTTL = 200 * time.Millisecond
	})
	tok := ts.mustLogin(t, "admin")
	time.Sleep(300 * time.Millisecond)
	res, _ := ts.do(t, "GET", "/flows", tok, nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an expired token still works: %d", res.StatusCode)
	}
}

// Every authenticated route against every kind of grant. A read-only account
// for a status board must not be able to deploy, and a prefix grant has to
// cover exactly its prefix.
func TestPermissionsAreEnforcedPerRoute(t *testing.T) {
	ts := newTestServer(t, map[string][]string{
		"admin":    {"*"},
		"viewer":   {"flows.read", "status.read"},
		"flowsall": {"flows.*"},
		"injector": {"inject.write"},
	})
	tokens := map[string]string{}
	for _, u := range []string{"admin", "viewer", "flowsall", "injector"} {
		tokens[u] = ts.mustLogin(t, u)
	}

	// 503 means the permission check passed and the handler ran into the
	// runtime not being up, which this test server never starts.
	cases := []struct {
		user, method, path string
		allowed            bool
	}{
		{"admin", "GET", "/settings", true},
		{"viewer", "GET", "/settings", false},
		{"viewer", "GET", "/nodes", false},
		{"viewer", "GET", "/flows", true},
		{"viewer", "POST", "/flows", false},
		{"viewer", "GET", "/runtime/stats", true},
		{"viewer", "POST", "/inject/n1", false},
		{"flowsall", "GET", "/flows", true},
		{"flowsall", "POST", "/flows", true},
		{"flowsall", "GET", "/runtime/stats", false},
		{"injector", "POST", "/inject/n1", true},
		{"injector", "GET", "/flows", false},
	}
	for _, c := range cases {
		var body []byte
		if c.method == "POST" && c.path == "/flows" {
			body = []byte(`[]`)
		}
		res, out := ts.do(t, c.method, c.path, tokens[c.user], body)
		if c.allowed && res.StatusCode == http.StatusForbidden {
			t.Errorf("%s %s as %s: 403 (%s), want allowed", c.method, c.path, c.user, out)
		}
		if !c.allowed && res.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as %s: %d, want 403", c.method, c.path, c.user, res.StatusCode)
		}
	}
}

// The 409 that stops two editors overwriting each other. The second editor
// still holds the revision the first one replaced.
func TestStaleDeployGets409AndChangesNothing(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	tok := ts.mustLogin(t, "admin")

	res, out := ts.do(t, "GET", "/flows", tok, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /flows: %d", res.StatusCode)
	}
	var got struct {
		Rev string `json:"rev"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	staleRev := got.Rev

	first := []byte(`[{"id":"t1","type":"tab","label":"first"}]`)
	res, out = ts.do(t, "POST", "/flows", tok, first, "HotLoop-Flow-Deployment-Rev", staleRev)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first deploy: %d %s", res.StatusCode, out)
	}

	second := []byte(`[{"id":"t1","type":"tab","label":"second"}]`)
	res, out = ts.do(t, "POST", "/flows", tok, second, "HotLoop-Flow-Deployment-Rev", staleRev)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale deploy: %d %s, want 409", res.StatusCode, out)
	}

	// The rev can also ride in the wrapped document, the way the editor sends it.
	wrapped := []byte(`{"rev":"` + staleRev + `","flows":[{"id":"t1","type":"tab","label":"third"}]}`)
	res, _ = ts.do(t, "POST", "/flows", tok, wrapped)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale deploy in a wrapped document: %d, want 409", res.StatusCode)
	}

	_, out = ts.do(t, "GET", "/flows", tok, nil)
	if !strings.Contains(string(out), `"first"`) {
		t.Fatalf("a refused deploy changed the flows: %s", out)
	}

	// No rev at all is the deliberate "overwrite anyway".
	res, _ = ts.do(t, "POST", "/flows", tok, second)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("forced deploy: %d", res.StatusCode)
	}
}

func TestDeployRefusesAnUnparseableDocument(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	tok := ts.mustLogin(t, "admin")
	res, _ := ts.do(t, "POST", "/flows", tok, []byte(`[{"type":"tab"}]`))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an entry with no id: %d, want 400", res.StatusCode)
	}
}

func TestDeployIsBoundedByMaxRequestBytes(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}}, func(c *config.Config) {
		c.Server.MaxRequestBytes = 1024
	})
	tok := ts.mustLogin(t, "admin")
	big := []byte(`[{"id":"t1","type":"tab","label":"` + strings.Repeat("x", 4096) + `"}]`)
	res, _ := ts.do(t, "POST", "/flows", tok, big)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized deploy: %d, want 413", res.StatusCode)
	}
}

// A token in the query string is only for the websocket, which can't set a
// header. Accepting it anywhere else invites it into every access log.
func TestQueryTokenOnlyCountsOnAWebsocketUpgrade(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	tok := ts.mustLogin(t, "admin")
	res, _ := ts.do(t, "GET", "/flows?access_token="+tok, "", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query token on a plain request: %d, want 401", res.StatusCode)
	}
}

// Health and readiness answer without a token, because the kubelet carries
// none. Readiness says 503 until there is a runtime.
func TestHealthAndReadyNeedNoToken(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	res, _ := ts.do(t, "GET", "/health", "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/health: %d", res.StatusCode)
	}
	res, _ = ts.do(t, "GET", "/ready", "", nil)
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/ready with no runtime: %d, want 503", res.StatusCode)
	}
}
