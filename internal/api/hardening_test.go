package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
	"github.com/coder/websocket"
)

// process is one run of the server over a data directory. Building a second
// one over the same directory is a restart.
type process struct {
	*httptest.Server
	srv   *Server
	trail *audit.Log
}

func hashFor(t *testing.T) string {
	t.Helper()
	h, err := config.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func start(t *testing.T, dir string, users []config.User, mutate ...func(*config.Config)) *process {
	t.Helper()
	cfg := config.Default()
	cfg.Data.Dir = dir
	cfg.Auth.Users = users
	for _, m := range mutate {
		m(&cfg)
	}
	fs := store.NewFlowStore(filepath.Join(dir, "flows.json"))
	if _, err := fs.Load(); err != nil {
		t.Fatal(err)
	}
	trail, err := audit.Open(filepath.Join(dir, "audit.log"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { trail.Close() })
	s := New(Deps{
		Config: cfg, Registry: node.Default, Flows: fs, Audit: trail,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Runtime:      nilRuntime,
		SessionsPath: filepath.Join(dir, "sessions.json"),
		Version:      "test",
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &process{Server: ts, srv: s, trail: trail}
}

func (p *process) login(t *testing.T, user, pass string) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	res, err := http.Post(p.URL+"/auth/token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out.AccessToken
}

func (p *process) get(t *testing.T, path, tok string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", p.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

// A restart, a reschedule or an upgrade used to sign everybody out. Now a
// session lives through it, and the file it lives in is useless to whoever
// copies it.
func TestSessionsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	users := []config.User{{Username: "dana", PasswordHash: hashFor(t), Permissions: []string{"*"}}}

	first := start(t, dir, users)
	res, tok := first.login(t, "dana", testPassword)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", res.StatusCode)
	}
	first.Close()

	second := start(t, dir, users)
	if code := second.get(t, "/flows", tok); code != http.StatusOK {
		t.Fatalf("after a restart the session gives %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tok) {
		t.Fatal("the sessions file holds the token itself")
	}
	if os.PathSeparator != '\\' {
		if info, _ := os.Stat(filepath.Join(dir, "sessions.json")); info.Mode().Perm() != 0o600 {
			t.Fatalf("sessions file mode %o", info.Mode().Perm())
		}
	}

	// Signing out is remembered across a restart too.
	req, _ := http.NewRequest("POST", second.URL+"/auth/revoke", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if r, err := http.DefaultClient.Do(req); err == nil {
		r.Body.Close()
	}
	second.Close()
	if code := start(t, dir, users).get(t, "/flows", tok); code != http.StatusUnauthorized {
		t.Fatalf("a revoked session after a restart: %d", code)
	}
}

// A session is a username, not a frozen set of permissions. Take a permission
// away, or the user, and the next request knows.
func TestSessionsFollowTheConfiguration(t *testing.T) {
	dir := t.TempDir()
	h := hashFor(t)
	p := start(t, dir, []config.User{{Username: "sam", PasswordHash: h, Permissions: []string{"*"}}})
	_, tok := p.login(t, "sam", testPassword)
	p.Close()

	readOnly := start(t, dir, []config.User{{Username: "sam", PasswordHash: h, Permissions: []string{"status.read"}}})
	if code := readOnly.get(t, "/flows", tok); code != http.StatusForbidden {
		t.Fatalf("after losing flows.read: %d", code)
	}
	readOnly.Close()

	gone := start(t, dir, []config.User{{Username: "dana", PasswordHash: h, Permissions: []string{"*"}}})
	if code := gone.get(t, "/flows", tok); code != http.StatusUnauthorized {
		t.Fatalf("after the user was removed: %d", code)
	}
}

func TestExpiredSessionsDontSurvive(t *testing.T) {
	dir := t.TempDir()
	users := []config.User{{Username: "dana", PasswordHash: hashFor(t), Permissions: []string{"*"}}}
	short := func(c *config.Config) { c.Auth.SessionTTL = 150 * time.Millisecond }
	p := start(t, dir, users, short)
	_, tok := p.login(t, "dana", testPassword)
	p.Close()
	time.Sleep(250 * time.Millisecond)
	if code := start(t, dir, users, short).get(t, "/flows", tok); code != http.StatusUnauthorized {
		t.Fatalf("an expired session after a restart: %d", code)
	}
}

func events(t *testing.T, p *process, prefix string) []audit.Entry {
	t.Helper()
	got, err := p.trail.Read(audit.Query{Event: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Five wrong passwords and the right one doesn't work either, for a while,
// from there. The audit trail says when it locked and every attempt it turned
// away.
func TestRepeatedFailuresLockTheAccountFromThatAddress(t *testing.T) {
	p := start(t, t.TempDir(), []config.User{{Username: "dana", PasswordHash: hashFor(t), Permissions: []string{"*"}}})
	now := time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)
	p.srv.limiter.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if res, _ := p.login(t, "dana", "not-it"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong password %d: %d", i+1, res.StatusCode)
		}
	}
	res, tok := p.login(t, "dana", testPassword)
	if res.StatusCode != http.StatusTooManyRequests || tok != "" {
		t.Fatalf("the right password while locked: %d", res.StatusCode)
	}
	if ra := res.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q", ra)
	}
	if n := len(events(t, p, audit.LoginLocked)); n != 1 {
		t.Fatalf("%d lock entries, want 1", n)
	}
	if n := len(events(t, p, audit.LoginThrottled)); n != 1 {
		t.Fatalf("%d throttled entries, want 1", n)
	}

	now = now.Add(16 * time.Minute)
	if res, tok := p.login(t, "dana", testPassword); res.StatusCode != http.StatusOK || tok == "" {
		t.Fatalf("after the lock ran out: %d", res.StatusCode)
	}
	// And a success clears the count, so the next typo starts again from one:
	// four typos, a good sign-in, four more, and still not locked.
	for round := 0; round < 2; round++ {
		for i := 0; i < 4; i++ {
			p.login(t, "dana", "not-it")
		}
		if res, _ := p.login(t, "dana", testPassword); res.StatusCode != http.StatusOK {
			t.Fatalf("round %d: four typos after a good sign-in locked it: %d", round+1, res.StatusCode)
		}
	}
}

// One address walking a list of usernames hits the per-address limit even
// though no single account reached five.
func TestOneAddressCantWalkAListOfUsernames(t *testing.T) {
	p := start(t, t.TempDir(), []config.User{{Username: "dana", PasswordHash: hashFor(t), Permissions: []string{"*"}}})
	p.srv.limiter.now = func() time.Time { return time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC) }
	for _, u := range []string{"admin", "root", "operator", "line3", "plc"} {
		for i := 0; i < 4; i++ {
			p.login(t, u, "password1")
		}
	}
	if res, _ := p.login(t, "dana", testPassword); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after twenty failures across five names: %d", res.StatusCode)
	}
}

func TestLockoutCantBeTurnedOff(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Users = []config.User{{Username: "dana", PasswordHash: hashFor(t), Permissions: []string{"*"}}}
	cfg.Data.CredentialSecret = "x"
	for _, lo := range []config.Lockout{
		{Attempts: 0, PerAddress: 20, Window: time.Minute, Duration: time.Minute},
		{Attempts: 5, PerAddress: 2, Window: time.Minute, Duration: time.Minute},
		{Attempts: 5, PerAddress: 20, Window: 0, Duration: time.Minute},
	} {
		c := cfg
		c.Auth.Lockout = lo
		if err := c.Validate(); err == nil {
			t.Errorf("accepted %+v", lo)
		}
	}
}

// The websocket's token rides as a subprotocol. In the query string, where
// access logs keep it, it doesn't count, on the websocket or anywhere else.
func TestWebsocketTokenRidesAsASubprotocol(t *testing.T) {
	p := start(t, t.TempDir(), []config.User{{Username: "dana", PasswordHash: hashFor(t), Permissions: []string{"*"}}})
	_, tok := p.login(t, "dana", testPassword)
	wsURL := "ws" + strings.TrimPrefix(p.URL, "http") + "/comms"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"hotloop-flow", "hotloop-flow.bearer." + tok},
	})
	if err != nil {
		t.Fatalf("dial with the token as a subprotocol: %v", err)
	}
	if conn.Subprotocol() != "hotloop-flow" {
		t.Fatalf("the server picked %q", conn.Subprotocol())
	}
	conn.Close(websocket.StatusNormalClosure, "")

	_, res, err := websocket.Dial(ctx, wsURL+"?access_token="+tok, &websocket.DialOptions{Subprotocols: []string{"hotloop-flow"}})
	if err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a token in the query string: %v %v", err, res)
	}
	_, res, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"hotloop-flow", "hotloop-flow.bearer.0000"},
	})
	if err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a made-up token: %v %v", err, res)
	}
}
