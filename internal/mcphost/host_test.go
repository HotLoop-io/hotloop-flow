package mcphost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests talk to the host with the Go SDK's own client, over real HTTP, in
// both eras of the protocol. Nothing here calls a handler directly.

const echoSchema = `{
  "type": "object",
  "properties": {
    "line": {"type": "string", "minLength": 2},
    "count": {"type": "integer", "minimum": 1, "maximum": 5}
  },
  "required": ["line"],
  "additionalProperties": false
}`

type auditLog struct {
	mu sync.Mutex
	ev []Event
}

func (a *auditLog) add(e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev = append(a.ev, e)
}

func (a *auditLog) named(name string) []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Event
	for _, e := range a.ev {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// identities maps a test token to who it is and what it may do.
func identities() map[string]Identity {
	allow := func(perms ...string) func(string) bool {
		return func(p string) bool {
			for _, have := range perms {
				if have == "*" || have == p {
					return true
				}
			}
			return false
		}
	}
	return map[string]Identity{
		"caller":  {Name: "token:agent", Allows: allow("mcp.call")},
		"admin":   {Name: "ryan", Allows: allow("*")},
		"nomcp":   {Name: "token:deploy", Allows: allow("flows.read", "flows.write")},
		"special": {Name: "token:line3", Allows: allow("mcp.call", "mcp.line3")},
		"limited": {Name: "token:slow", Allows: allow("mcp.call"), Limit: Limit{Calls: 2, Per: time.Minute}},
		"other":   {Name: "token:slow2", Allows: allow("mcp.call"), Limit: Limit{Calls: 2, Per: time.Minute}},
	}
}

func authenticate(r *http.Request) (Identity, *Refusal) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return Identity{}, &Refusal{Status: http.StatusUnauthorized, Message: "no bearer token"}
	}
	id, ok := identities()[tok]
	if !ok {
		return Identity{}, &Refusal{Status: http.StatusUnauthorized, Message: "token is invalid or has expired", Audit: true}
	}
	return id, nil
}

type fixture struct {
	reg   *Registry
	srv   *httptest.Server
	audit *auditLog
	calls chan Call
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{reg: New("test"), audit: &auditLog{}, calls: make(chan Call, 16)}
	f.srv = httptest.NewServer(f.reg.Handler(Options{Authenticate: authenticate, Audit: f.audit.add}))
	t.Cleanup(f.srv.Close)

	_, err := f.reg.Register("node-echo", ToolSpec{
		Name: "echo_line", Description: "Echo a line back.", InputSchema: json.RawMessage(echoSchema),
	}, func(_ context.Context, c Call) (Outcome, error) {
		f.calls <- c
		return Outcome{Text: "line " + c.Arguments["line"].(string), Structured: map[string]any{"line": c.Arguments["line"]}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.reg.Register("node-secret", ToolSpec{
		Name: "line3_status", Description: "Only for the line 3 token.", Permission: "mcp.line3",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, Call) (Outcome, error) { return Outcome{Text: "running"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.reg.Register("node-broken", ToolSpec{
		Name: "breaks", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, Call) (Outcome, error) { return Outcome{}, errors.New("the flow fell over") })
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.reg.Register("node-fails", ToolSpec{
		Name: "fails", InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, Call) (Outcome, error) {
		return Outcome{Text: "the press is not ready", IsError: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

var eras = []struct {
	name    string
	version string
}{
	{"stateless 2026-07-28", "2026-07-28"},
	{"handshake 2025-11-25", "2025-11-25"},
}

func (f *fixture) connect(t *testing.T, token, version string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             f.srv.URL,
		HTTPClient:           &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("connect (%s): %v", version, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func toolNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	var names []string
	for tool, err := range s.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		names = append(names, tool.Name)
	}
	return names
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func text(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestListShowsOnlyWhatTheCallerMayCall(t *testing.T) {
	for _, era := range eras {
		t.Run(era.name, func(t *testing.T) {
			f := newFixture(t)

			plain := toolNames(t, f.connect(t, "caller", era.version))
			if !has(plain, "echo_line") {
				t.Errorf("a caller with mcp.call does not see echo_line: %v", plain)
			}
			if has(plain, "line3_status") {
				t.Errorf("a caller without mcp.line3 is offered line3_status: %v", plain)
			}

			special := toolNames(t, f.connect(t, "special", era.version))
			if !has(special, "line3_status") {
				t.Errorf("a caller with mcp.line3 does not see line3_status: %v", special)
			}
		})
	}
}

func TestCallRunsTheFunctionAndAnswersWithItsResult(t *testing.T) {
	for _, era := range eras {
		t.Run(era.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.connect(t, "caller", era.version)

			res, err := s.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "echo_line", Arguments: map[string]any{"line": "L3", "count": 2},
			})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if res.IsError {
				t.Fatalf("answered as an error: %s", text(t, res))
			}
			if got := text(t, res); got != "line L3" {
				t.Errorf("text = %q", got)
			}
			sc, _ := res.StructuredContent.(map[string]any)
			if sc["line"] != "L3" {
				t.Errorf("structuredContent = %#v", res.StructuredContent)
			}

			c := <-f.calls
			if c.Caller != "token:agent" {
				t.Errorf("the function was told the caller is %q, want token:agent", c.Caller)
			}
			if c.ID == "" {
				t.Errorf("the call has no id")
			}
		})
	}
}

func TestArgumentsThatDoNotFitTheSchemaNeverReachTheFunction(t *testing.T) {
	for _, era := range eras {
		t.Run(era.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.connect(t, "caller", era.version)

			for _, bad := range []map[string]any{
				{},                               // line is required
				{"line": "L"},                    // too short
				{"line": "L3", "count": 9},       // over the maximum
				{"line": "L3", "extra": true},    // not allowed
				{"line": "L3", "count": "three"}, // wrong type
			} {
				res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo_line", Arguments: bad})
				if err != nil {
					t.Fatalf("%v: a protocol error, want a tool error the model can read: %v", bad, err)
				}
				if !res.IsError {
					t.Errorf("%v was accepted", bad)
				}
				if !strings.Contains(text(t, res), "input schema") {
					t.Errorf("%v: the refusal does not say why: %q", bad, text(t, res))
				}
			}
			select {
			case c := <-f.calls:
				t.Fatalf("the function ran for arguments that do not fit: %#v", c.Arguments)
			default:
			}
			if n := len(f.audit.named(EventCall)); n != 0 {
				t.Errorf("%d mcp.call rows for calls that were refused", n)
			}
			if n := len(f.audit.named(EventRefused)); n != 5 {
				t.Errorf("%d mcp.refused rows, want 5", n)
			}
		})
	}
}

func TestACallerWithoutTheToolsPermissionIsRefusedEvenIfItNamesTheTool(t *testing.T) {
	for _, era := range eras {
		t.Run(era.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.connect(t, "caller", era.version)
			res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "line3_status", Arguments: map[string]any{}})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !res.IsError || !strings.Contains(text(t, res), "mcp.line3") {
				t.Fatalf("not refused for the missing permission: isError=%v %q", res.IsError, text(t, res))
			}
			refused := f.audit.named(EventRefused)
			if len(refused) != 1 || refused[0].User != "token:agent" || refused[0].Detail["tool"] != "line3_status" {
				t.Fatalf("audit rows: %#v", refused)
			}
			if n := len(f.audit.named(EventCall)); n != 0 {
				t.Errorf("an mcp.call row for a refused call")
			}
		})
	}
}

func TestEveryCallIsAuditedUnderTheCallersName(t *testing.T) {
	for _, era := range eras {
		t.Run(era.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.connect(t, "admin", era.version)
			if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo_line", Arguments: map[string]any{"line": "L3"}}); err != nil {
				t.Fatal(err)
			}
			calls, results := f.audit.named(EventCall), f.audit.named(EventResult)
			if len(calls) != 1 || len(results) != 1 {
				t.Fatalf("%d mcp.call and %d mcp.result rows, want one each", len(calls), len(results))
			}
			if calls[0].User != "ryan" || calls[0].Detail["tool"] != "echo_line" || calls[0].Detail["node"] != "node-echo" {
				t.Errorf("mcp.call = %#v", calls[0])
			}
			if calls[0].Remote == "" {
				t.Errorf("mcp.call has no remote address")
			}
			if results[0].Detail["outcome"] != "ok" || results[0].Detail["callId"] != calls[0].Detail["callId"] {
				t.Errorf("mcp.result = %#v does not match %#v", results[0], calls[0])
			}
			if _, leaked := calls[0].Detail["arguments"]; leaked {
				t.Errorf("the arguments are in the audit log")
			}
		})
	}
}

func TestAToolThatFailsIsAnErrorTheModelCanRead(t *testing.T) {
	for _, era := range eras {
		t.Run(era.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.connect(t, "admin", era.version)

			res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "fails", Arguments: map[string]any{}})
			if err != nil || !res.IsError || text(t, res) != "the press is not ready" {
				t.Fatalf("fails: err=%v isError=%v %q", err, res != nil && res.IsError, text(t, res))
			}
			res, err = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "breaks", Arguments: map[string]any{}})
			if err != nil || !res.IsError || text(t, res) != "the flow fell over" {
				t.Fatalf("breaks: err=%v %q", err, text(t, res))
			}
			results := f.audit.named(EventResult)
			if len(results) != 2 || results[0].Detail["outcome"] != "error" || results[1].Detail["error"] != "the flow fell over" {
				t.Fatalf("mcp.result rows: %#v", results)
			}
		})
	}
}

func post(t *testing.T, url, token, version, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

const listBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

func TestNoTokenAndABadTokenAreRefusedBeforeTheProtocolSeesThem(t *testing.T) {
	f := newFixture(t)

	res, body := post(t, f.srv.URL, "", "2026-07-28", listBody)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d %s", res.StatusCode, body)
	}
	if res.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("a 401 with no WWW-Authenticate")
	}
	if !strings.Contains(body, `"error"`) || strings.Contains(body, "echo_line") {
		t.Errorf("body = %q: it must be an error and must not name a tool", body)
	}
	if n := len(f.audit.named(EventRefused)); n != 0 {
		t.Errorf("a request with no token at all wrote %d audit rows", n)
	}

	res, body = post(t, f.srv.URL, "wrong", "2026-07-28", listBody)
	if res.StatusCode != http.StatusUnauthorized || strings.Contains(body, "echo_line") {
		t.Fatalf("bad token: %d %s", res.StatusCode, body)
	}
	if n := len(f.audit.named(EventRefused)); n != 1 {
		t.Errorf("a bad token wrote %d audit rows, want 1", n)
	}

	// The handshake era's opening request is refused the same way.
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	res, body = post(t, f.srv.URL, "", "", init)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("initialize with no token: %d %s", res.StatusCode, body)
	}
}

func TestATokenWithoutMCPCallIsForbiddenAndAudited(t *testing.T) {
	f := newFixture(t)
	res, body := post(t, f.srv.URL, "nomcp", "2026-07-28", listBody)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body, Permission) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	rows := f.audit.named(EventRefused)
	if len(rows) != 1 || rows[0].User != "token:deploy" {
		t.Fatalf("audit rows: %#v", rows)
	}
}

func TestACrossOriginBrowserRequestIsRefusedWithAValidToken(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest("POST", f.srv.URL, strings.NewReader(listBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-origin call got %d, want 403", res.StatusCode)
	}
}

func TestTwoToolsMayNotShareAName(t *testing.T) {
	f := newFixture(t)
	_, err := f.reg.Register("node-other", ToolSpec{Name: "echo_line", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, Call) (Outcome, error) { return Outcome{}, nil })
	if err == nil || !strings.Contains(err.Error(), "node-echo") {
		t.Fatalf("err = %v, want the first owner named", err)
	}
}

func TestAToolIsGoneFromTheListWhenItsNodeStops(t *testing.T) {
	for _, era := range eras {
		t.Run(era.name, func(t *testing.T) {
			f := newFixture(t)
			unbind, err := f.reg.Register("node-temp", ToolSpec{Name: "temp", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(context.Context, Call) (Outcome, error) { return Outcome{Text: "hi"}, nil })
			if err != nil {
				t.Fatal(err)
			}
			s := f.connect(t, "admin", era.version)
			if !has(toolNames(t, s), "temp") {
				t.Fatalf("temp is not listed while its node runs")
			}
			unbind()
			if has(toolNames(t, s), "temp") {
				t.Fatalf("temp is still listed after its node stopped")
			}
			// The name is free again.
			if _, err := f.reg.Register("node-temp2", ToolSpec{Name: "temp", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(context.Context, Call) (Outcome, error) { return Outcome{}, nil }); err != nil {
				t.Fatalf("the name was not released: %v", err)
			}
		})
	}
}

func TestResetDropsEveryTool(t *testing.T) {
	f := newFixture(t)
	f.reg.Reset()
	if names := f.reg.Names(); len(names) != 0 {
		t.Fatalf("still serving %v", names)
	}
	if names := toolNames(t, f.connect(t, "admin", "2026-07-28")); len(names) != 0 {
		t.Fatalf("still listing %v", names)
	}
}

func TestCompileRefusesWhatCannotBeServed(t *testing.T) {
	obj := json.RawMessage(`{"type":"object"}`)
	for _, c := range []struct {
		name string
		spec ToolSpec
		want string
	}{
		{"empty name", ToolSpec{InputSchema: obj}, "tool name"},
		{"a space in the name", ToolSpec{Name: "get status", InputSchema: obj}, "tool name"},
		{"a name that is too long", ToolSpec{Name: strings.Repeat("a", 129), InputSchema: obj}, "tool name"},
		{"no schema", ToolSpec{Name: "x"}, "schema is empty"},
		{"not JSON", ToolSpec{Name: "x", InputSchema: json.RawMessage(`{nope`)}, "not a JSON Schema"},
		{"not an object", ToolSpec{Name: "x", InputSchema: json.RawMessage(`{"type":"string"}`)}, "object"},
		{"a bad keyword value", ToolSpec{Name: "x", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"nonsense"}}}`)}, "not valid"},
		{"a remote ref", ToolSpec{Name: "x", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}}`)}, "not valid"},
		{"a bad permission", ToolSpec{Name: "x", InputSchema: obj, Permission: "Has Spaces"}, "permission"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Compile(c.spec)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, c.want)
			}
		})
	}
	if _, err := Compile(ToolSpec{Name: "ok.name-1_x", InputSchema: obj, Permission: "mcp.line3"}); err != nil {
		t.Fatalf("a good spec was refused: %v", err)
	}
}

// A fake clock, so a window can pass without the test waiting for it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func callEcho(t *testing.T, s *mcp.ClientSession) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo_line", Arguments: map[string]any{"line": "L3"}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return res
}

func TestARateLimitedTokenIsRefusedOverItsLimitAndTheWindowResets(t *testing.T) {
	for _, era := range eras {
		t.Run(era.name, func(t *testing.T) {
			f := newFixture(t)
			clock := &fakeClock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
			f.reg.now = clock.Now
			s := f.connect(t, "limited", era.version)

			// Under the limit passes.
			for i := 1; i <= 2; i++ {
				if res := callEcho(t, s); res.IsError {
					t.Fatalf("call %d of 2 was refused: %s", i, text(t, res))
				}
			}
			<-f.calls
			<-f.calls

			// Over it is refused, with the way back, and the flow never runs.
			clock.Advance(20 * time.Second)
			res := callEcho(t, s)
			if !res.IsError || !strings.Contains(text(t, res), "rate limit exceeded") || !strings.Contains(text(t, res), "2 calls per 1m0s") ||
				!strings.Contains(text(t, res), "Try again in 40 seconds") {
				t.Fatalf("third call: isError=%v %q", res.IsError, text(t, res))
			}
			select {
			case c := <-f.calls:
				t.Fatalf("the function ran for a call over the limit: %#v", c)
			default:
			}
			refused := f.audit.named(EventRefused)
			if len(refused) != 1 || refused[0].User != "token:slow" ||
				refused[0].Detail["reason"] != "rate limit exceeded: 2 calls per 1m0s" || refused[0].Detail["tool"] != "echo_line" {
				t.Fatalf("audit rows: %#v", refused)
			}
			if n := len(f.audit.named(EventCall)); n != 2 {
				t.Errorf("%d mcp.call rows, want only the 2 that ran", n)
			}

			// Another limited token has a count of its own.
			if res := callEcho(t, f.connect(t, "other", era.version)); res.IsError {
				t.Fatalf("a different token was throttled by the first one's calls: %s", text(t, res))
			}
			<-f.calls

			// The window ends and the count starts again.
			clock.Advance(41 * time.Second)
			if res := callEcho(t, s); res.IsError {
				t.Fatalf("still refused after the window passed: %s", text(t, res))
			}
			<-f.calls
			if res := callEcho(t, s); res.IsError {
				t.Fatalf("second call of the new window refused: %s", text(t, res))
			}
			<-f.calls
			if res := callEcho(t, s); !res.IsError {
				t.Fatalf("the new window let a third call through")
			}
		})
	}
}

func TestATokenWithNoLimitIsNeverThrottled(t *testing.T) {
	f := newFixture(t)
	s := f.connect(t, "caller", "2026-07-28")
	for i := 0; i < 50; i++ {
		if res := callEcho(t, s); res.IsError {
			t.Fatalf("call %d was refused: %s", i+1, text(t, res))
		}
		<-f.calls
	}
	if n := len(f.audit.named(EventRefused)); n != 0 {
		t.Fatalf("%d refusals for a token with no limit", n)
	}
}
