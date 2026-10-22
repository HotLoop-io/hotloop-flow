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
	"github.com/HotLoop-io/hotloop-flow/internal/mcphost"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP endpoint through the whole admin server: a real token store, real API
// tokens from the config, the real audit log on disk, and the Go SDK's client.

type mcpFixture struct {
	url     string
	trail   *audit.Log
	agent   string // an API token with mcp.call
	deploy  string // an API token without it
	admin   string // a session token for a user with *
	reg     *mcphost.Registry
	handled chan mcphost.Call
}

func newMCPFixture(t *testing.T, mutate ...func(*config.Config)) *mcpFixture {
	t.Helper()
	hash, err := config.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Data.Dir = t.TempDir()
	cfg.Auth.Users = []config.User{{Username: "ryan", PasswordHash: hash, Permissions: []string{"*"}}}
	agent, agentHash, _ := config.NewToken()
	deploy, deployHash, _ := config.NewToken()
	cfg.Auth.Tokens = []config.Token{
		{Name: "agent", Hash: agentHash, Permissions: []string{"mcp.call"}},
		{Name: "ci", Hash: deployHash, Permissions: []string{"flows.read", "flows.write"}},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	trail, err := audit.Open(filepath.Join(cfg.Data.Dir, "audit.log"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { trail.Close() })

	f := &mcpFixture{trail: trail, agent: agent, deploy: deploy, reg: mcphost.New("test"), handled: make(chan mcphost.Call, 64)}
	_, err = f.reg.Register("node-line", mcphost.ToolSpec{
		Name: "line_status", Description: "Status of a line.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"line":{"type":"string"}},"required":["line"]}`),
	}, func(_ context.Context, c mcphost.Call) (mcphost.Outcome, error) {
		f.handled <- c
		return mcphost.Outcome{Text: "line " + c.Arguments["line"].(string) + " is running"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	s := New(Deps{
		Config: cfg, Registry: node.Default, Audit: trail, MCP: f.reg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Runtime: nilRuntime, Version: "test",
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	f.url = srv.URL

	if cfg.Auth.Enabled {
		body, _ := json.Marshal(map[string]string{"username": "ryan", "password": testPassword})
		res, err := http.Post(srv.URL+strings.TrimSuffix(cfg.Server.AdminRoot, "/")+"/auth/token", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var tok struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(res.Body).Decode(&tok)
		if res.StatusCode != http.StatusOK || tok.AccessToken == "" {
			t.Fatalf("login: %d", res.StatusCode)
		}
		f.admin = tok.AccessToken
	}
	return f
}

func (f *mcpFixture) rows(t *testing.T, prefix string) []audit.Entry {
	t.Helper()
	all, err := f.trail.Read(audit.Query{Event: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (f *mcpFixture) client(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.url + "/mcp", HTTPClient: &http.Client{Transport: bearerRT{token}}, DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

func TestMCPAnAPITokenWithMCPCallListsAndCallsAndIsAudited(t *testing.T) {
	f := newMCPFixture(t)
	s, err := f.client(t, f.agent)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var names []string
	for tool, err := range s.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	if len(names) != 1 || names[0] != "line_status" {
		t.Fatalf("tools = %v", names)
	}
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L3"}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "line L3 is running" {
		t.Fatalf("answer = %q", got)
	}
	if c := <-f.handled; c.Caller != "token:agent" {
		t.Errorf("the flow was told the caller is %q", c.Caller)
	}

	rows := f.rows(t, "mcp.")
	var started, ended *audit.Entry
	for i := range rows {
		switch rows[i].Event {
		case "mcp.call":
			started = &rows[i]
		case "mcp.result":
			ended = &rows[i]
		}
	}
	if started == nil || ended == nil {
		t.Fatalf("audit rows: %+v", rows)
	}
	if started.User != "token:agent" || started.Detail["tool"] != "line_status" || started.Remote == "" {
		t.Errorf("mcp.call = %+v", *started)
	}
	if ended.Detail["outcome"] != "ok" {
		t.Errorf("mcp.result = %+v", *ended)
	}
}

func TestMCPASessionTokenOfAUserWithEverythingCanCall(t *testing.T) {
	f := newMCPFixture(t)
	s, err := f.client(t, f.admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L1"}}); err != nil {
		t.Fatal(err)
	}
	rows := f.rows(t, "mcp.call")
	if len(rows) != 1 || rows[0].User != "ryan" {
		t.Fatalf("audit rows: %+v", rows)
	}
}

func TestMCPNoTokenIsRefusedAndNothingIsEnumerated(t *testing.T) {
	f := newMCPFixture(t)
	if _, err := f.client(t, ""); err == nil {
		t.Fatal("a client with no token connected")
	}
	req, _ := http.NewRequest("POST", f.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusUnauthorized || strings.Contains(string(body), "line_status") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if len(f.rows(t, "mcp.call")) != 0 {
		t.Errorf("a refused request was recorded as a call")
	}
}

func TestMCPAWrongTokenIsRefusedAndRecorded(t *testing.T) {
	f := newMCPFixture(t)
	if _, err := f.client(t, "hlf_"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("a made-up API token connected")
	}
	rows := f.rows(t, "mcp.refused")
	if len(rows) == 0 || rows[0].Detail["reason"] != "token is invalid or has expired" {
		t.Fatalf("audit rows: %+v", rows)
	}
}

func TestMCPATokenWithoutMCPCallIsForbidden(t *testing.T) {
	f := newMCPFixture(t)
	if _, err := f.client(t, f.deploy); err == nil {
		t.Fatal("a deploy token connected")
	}
	rows := f.rows(t, "mcp.refused")
	if len(rows) == 0 || rows[0].User != "token:ci" {
		t.Fatalf("audit rows: %+v", rows)
	}
}

func TestMCPIsRefusedWhenAuthenticationIsOff(t *testing.T) {
	f := newMCPFixture(t, func(c *config.Config) { c.Auth.Enabled = false; c.Auth.Insecure = true })
	req, _ := http.NewRequest("POST", f.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "needs authentication") {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
}

func TestMCPAdminRootMovesTheEndpoint(t *testing.T) {
	f := newMCPFixture(t, func(c *config.Config) { c.Server.AdminRoot = "/admin" })
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.url + "/admin/mcp", HTTPClient: &http.Client{Transport: bearerRT{f.agent}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect under the admin root: %v", err)
	}
	_ = s.Close()
}

func TestMCPATokenWithARateLimitInTheConfigIsHeldToIt(t *testing.T) {
	var limited, free string
	f := newMCPFixture(t, func(c *config.Config) {
		var lh, fh string
		limited, lh, _ = config.NewToken()
		free, fh, _ = config.NewToken()
		c.Auth.Tokens = append(c.Auth.Tokens,
			config.Token{Name: "metered", Hash: lh, Permissions: []string{"mcp.call"}, RateLimit: config.TokenRateLimit{Calls: 2}},
			config.Token{Name: "open", Hash: fh, Permissions: []string{"mcp.call"}})
	})
	call := func(token string) *mcp.CallToolResult {
		t.Helper()
		s, err := f.client(t, token)
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "line_status", Arguments: map[string]any{"line": "L3"}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for i := 0; i < 2; i++ {
		if res := call(limited); res.IsError {
			t.Fatalf("call %d of 2: %+v", i+1, res)
		}
	}
	res := call(limited)
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "rate limit exceeded: this token may make 2 calls per 1m0s") {
		t.Fatalf("third call: %+v", res)
	}
	for i := 0; i < 10; i++ {
		if res := call(free); res.IsError {
			t.Fatalf("a token with no limit was refused: %+v", res)
		}
	}
	// The agent token from the fixture has no rateLimit either.
	for i := 0; i < 5; i++ {
		if res := call(f.agent); res.IsError {
			t.Fatalf("agent refused: %+v", res)
		}
	}
	rows := f.rows(t, "mcp.refused")
	if len(rows) != 1 || rows[0].User != "token:metered" || rows[0].Detail["reason"] != "rate limit exceeded: 2 calls per 1m0s" {
		t.Fatalf("audit rows: %+v", rows)
	}
}
