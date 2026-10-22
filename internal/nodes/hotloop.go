package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// HotLoop.
//
// Flow runs next to HotLoop and talks to its HTTP API from the outside, which is
// the whole relationship (docs/ROADMAP.md, Decided 2026-10-03). Nothing here
// reaches into HotLoop and nothing here is shared with it: a login, then the
// documented calls, with the answer's own words passed back when HotLoop says no.
//
// The calls are the ones in HotLoop's docs/API.md at 4.18.10:
//
//	POST /api/auth/login               a bearer token, 12 hours by default
//	GET  /api/stream                   server-sent events: snapshot, then entities
//	GET  /api/states/{entity_id}       one entity's state
//	POST /api/write                    one value to one tag, through the write gate
//	POST /api/scripts/{id}/run         run a script
//	POST /api/recipes/{id}/apply       apply a recipe
//
// A flow that never selects a HotLoop connection never makes any of them.

func init() {
	registerHotloopConfig()
}

// hotloopTarget is how the HotLoop nodes reach their connection config node.
type hotloopTarget interface {
	// call makes one request and answers its status and body. A transport
	// failure is an error; a status HotLoop chose to answer with is not.
	call(ctx context.Context, method, path string, body any) (int, []byte, error)
	// openStream opens GET /api/stream. The caller closes the body.
	openStream(ctx context.Context) (*http.Response, error)
	// describe is the address, for messages.
	describe() string
}

// hotloopConn is the config node: one HotLoop, one login, shared by every node
// that points at it.
type hotloopConn struct {
	base    string
	user    string
	pass    string
	timeout time.Duration

	client *http.Client
	// streamClient has no overall timeout, because the live feed is open for as
	// long as the node is.
	streamClient *http.Client

	mu    sync.Mutex
	token string
}

func registerHotloopConfig() {
	node.MustRegister(node.Descriptor{
		Type:     "hotloop-flow-hotloop",
		Category: node.CategoryConfig,
		Color:    colorNetwork,
		Icon:     "network",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own connection to a HotLoop. Logs in with an account and sends " +
				"the bearer token HotLoop hands back, and logs in again when it expires.",
		},
		LabelProp: "name",
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "url", Kind: node.PropString, Label: "URL", Required: true,
				Placeholder: "http://hotloop.plant.local:8080",
				Help: "Where HotLoop answers, with no path after it. If HotLoop is served under a " +
					"path, put the path here, like https://plant.example/hotloop."},
			{Name: "username", Kind: node.PropString, Label: "Username",
				Help: "An operator account is enough: it can read states, write values, run scripts and " +
					"apply recipes. Leave it empty only for a HotLoop that runs with no login at all."},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 15,
				Help: "How long one request waits for HotLoop. A script that waits for its run to end " +
					"is held to this too, so raise it for a script that takes longer."},
		},
		Help: "One HotLoop. Every HotLoop node that points at it shares the login.\n\n" +
			"Flow signs in with this account, and every write, script run and recipe application " +
			"HotLoop receives from Flow is recorded under it, with the reason `flow:<node id>`.",
	}, newHotloopConn)
}

func newHotloopConn(def *node.Definition) (node.Node, error) {
	base := strings.TrimRight(strings.TrimSpace(def.Node.PropString("url", "")), "/")
	if base == "" {
		return nil, fmt.Errorf("url is required: where HotLoop answers, like http://hotloop.plant.local:8080")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("url %q has no host", base)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("url %q should be just the address, with no user, query or fragment", base)
	}
	secs := def.Node.PropFloat("timeout", 15)
	if secs <= 0 {
		return nil, fmt.Errorf("timeout has to be more than zero seconds")
	}
	user := strings.TrimSpace(def.Node.PropString("username", ""))
	pass, _ := def.Services.Credential("password")
	timeout := time.Duration(secs * float64(time.Second))
	return &hotloopConn{
		base:         base,
		user:         user,
		pass:         pass,
		timeout:      timeout,
		client:       &http.Client{Timeout: timeout},
		streamClient: &http.Client{},
	}, nil
}

func (c *hotloopConn) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (c *hotloopConn) describe() string { return c.base }

// maxHotloopBody is HotLoop's own ceiling on a request body, and a generous one
// on what comes back: a trace of a long script run is the biggest answer there is.
const maxHotloopBody = 8 << 20

// login signs in and keeps the token. Callers hold c.mu.
func (c *hotloopConn) login(ctx context.Context) error {
	if c.pass == "" {
		// Not checked when the node is built, because a flow test never signs in
		// and has no password to give it.
		return fmt.Errorf("the HotLoop connection has the username %q and no password. Open it and set the password", c.user)
	}
	body, _ := json.Marshal(map[string]string{"username": c.user, "password": c.pass})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach HotLoop at %s to sign in: %w", c.base, scrubURLError(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxHotloopBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HotLoop at %s refused the login for %q: %s (HTTP %d)",
			c.base, c.user, hotloopErrorText(b), resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Token == "" {
		return fmt.Errorf("HotLoop at %s answered the login with no token", c.base)
	}
	c.token = out.Token
	return nil
}

// bearer returns the token to send, signing in first when there is none. A
// connection with no username sends nothing.
func (c *hotloopConn) bearer(ctx context.Context) (string, error) {
	if c.user == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		if err := c.login(ctx); err != nil {
			return "", err
		}
	}
	return c.token, nil
}

// expire forgets a token HotLoop refused, so the next request signs in again.
// It only forgets the one it was refused with: another request may already have
// signed in again.
func (c *hotloopConn) expire(used string) {
	c.mu.Lock()
	if c.token == used {
		c.token = ""
	}
	c.mu.Unlock()
}

// send makes a request with the token, and once more with a fresh one when
// HotLoop says the session is gone.
func (c *hotloopConn) send(ctx context.Context, hc *http.Client, method, path string, payload []byte, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.bearer(ctx)
		if err != nil {
			return nil, err
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
		if err != nil {
			return nil, err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("could not reach HotLoop at %s: %w", c.base, scrubURLError(err))
		}
		if resp.StatusCode == http.StatusUnauthorized && tok != "" && attempt == 0 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			c.expire(tok)
			continue
		}
		return resp, nil
	}
}

func (c *hotloopConn) call(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("the request can't be written as JSON: %w", err)
		}
		payload = b
	}
	resp, err := c.send(ctx, c.client, method, path, payload, "application/json")
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxHotloopBody))
	if err != nil {
		return 0, nil, fmt.Errorf("reading HotLoop's answer: %w", scrubURLError(err))
	}
	if resp.StatusCode == http.StatusUnauthorized && c.user == "" {
		return resp.StatusCode, b, fmt.Errorf("HotLoop at %s wants a login (%s). Set a username and password on the HotLoop connection",
			c.base, hotloopErrorText(b))
	}
	return resp.StatusCode, b, nil
}

func (c *hotloopConn) openStream(ctx context.Context) (*http.Response, error) {
	resp, err := c.send(ctx, c.streamClient, http.MethodGet, "/api/stream", nil, "text/event-stream")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("HotLoop at %s would not open its live feed: %s (HTTP %d)",
			c.base, hotloopErrorText(b), resp.StatusCode)
	}
	return resp, nil
}

// scrubURLError drops the URL net/http wraps around a transport error, which
// repeats what the message already says.
func scrubURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// hotloopErrorText is what HotLoop said, in its own words. Every failure the API
// produces is {"error": "..."}; anything else is shown as it came, trimmed.
func hotloopErrorText(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "no reason given"
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// hotloopTarget0 finds the connection a node points at, once.
func hotloopTarget0(svc node.Services, cfgID string, cached *hotloopTarget) (hotloopTarget, error) {
	if *cached != nil {
		return *cached, nil
	}
	cfg, ok := svc.ConfigNode(cfgID)
	if !ok {
		return nil, fmt.Errorf("the HotLoop connection %s is not running. Open this node and pick a HotLoop", cfgID)
	}
	t, ok := cfg.(hotloopTarget)
	if !ok {
		return nil, fmt.Errorf("config node %s is not a HotLoop connection", cfgID)
	}
	*cached = t
	return t, nil
}

// hotloopReason is what Flow tells HotLoop it is doing, which HotLoop records
// beside the write, the script run or the recipe application: flow:<node id>,
// and the operator's own words after it when the node has some.
func hotloopReason(nodeID, extra string) string {
	r := "flow:" + nodeID
	if extra = strings.TrimSpace(extra); extra != "" {
		r += ": " + extra
	}
	return r
}

// hotloopObject decodes a JSON object answer.
func hotloopObject(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("HotLoop's answer is not JSON: %w", err)
	}
	return m, nil
}

// hotloopPathEscape makes one id safe to put in a path. Ids are letters, digits
// and punctuation HotLoop allows, but a value that arrives on a message is
// whatever it is.
func hotloopPathEscape(id string) string { return url.PathEscape(id) }

// hotloopValue is a state as HotLoop sends it (always a string) or, with numeric
// on, a number when the text is one. "on", "unknown" and the rest stay text.
func hotloopValue(state any, numeric bool) any {
	s, ok := state.(string)
	if !ok || !numeric {
		return state
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return s
	}
	return f
}

// hotloopInfo is what the HotLoop nodes leave on msg.hotloop, so the next node
// knows where the answer came from.
func hotloopInfo(server string, fields map[string]any) map[string]any {
	m := map[string]any{"server": server}
	for k, v := range fields {
		m[k] = v
	}
	return m
}
