// Package mcphost serves flows as Model Context Protocol tools.
//
// A flow's "mcp tool" node declares a name, a description and an input schema
// and registers itself here when it starts. This package owns everything on the
// wire: it speaks MCP over Streamable HTTP at one endpoint, answers tools/list
// and tools/call, and refuses everything that has not been authenticated. It
// does not run flows. A call is handed to the function the node registered, which
// puts a message into the flow and waits for the flow's answer.
//
// The protocol is the Go SDK's (github.com/modelcontextprotocol/go-sdk, Apache
// 2.0). It speaks both eras of the specification on the one endpoint: the
// stateless revision, 2026-07-28, where every request carries its own version
// and capabilities, and the handshake revisions through 2025-11-25, where a
// client opens with initialize. A request is routed by its MCP-Protocol-Version
// header, which the stateless revision requires and the earlier ones send after
// the handshake.
//
// A caller is authenticated the way the admin API authenticates one: a session
// token or an API token. What the caller may do is what that identity may do,
// and every call and every refusal goes in the audit log under the caller's
// name. There is no anonymous call.
package mcphost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/schema"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Permission is what a caller needs to reach any tool. A tool can ask for more.
const Permission = "mcp.call"

// Audit events this package records.
const (
	// EventCall is a tool call that was accepted, written before the flow runs.
	EventCall = "mcp.call"
	// EventResult is how that call ended.
	EventResult = "mcp.result"
	// EventRefused is a call or a connection turned away.
	EventRefused = "mcp.refused"
)

// protocolStateless is the first revision with no handshake. A request carrying
// it or a later one is served by the stateless handler.
const protocolStateless = "2026-07-28"

// Identity is who is calling.
type Identity struct {
	// Name is what the audit log calls the caller: a username, or token:<name>.
	Name string
	// Allows reports whether the caller holds a permission.
	Allows func(perm string) bool
	// Limit bounds this caller's tool calls. The zero value is no limit.
	Limit Limit
}

// Limit is a number of tool calls in a window. The window starts with the
// caller's first call and, once it has passed, the count starts again.
type Limit struct {
	Calls int
	Per   time.Duration
}

type window struct {
	start time.Time
	n     int
}

// Refusal is why a request was turned away before it reached the protocol.
type Refusal struct {
	Status  int
	Message string
	// User is the name to audit the refusal under, when there is one.
	User string
	// Audit asks for the refusal to be written to the audit log. A request with
	// no token at all is noise and is not written; one with a bad token, or a
	// good token that lacks the permission, is.
	Audit bool
}

// Event is one row for the audit log.
type Event struct {
	Name         string
	User         string
	Remote       string
	ForwardedFor string
	Detail       map[string]any
}

// Options is what the host needs from the process around it.
type Options struct {
	// Authenticate identifies the caller of a request. It must refuse every
	// request it cannot identify.
	Authenticate func(r *http.Request) (Identity, *Refusal)
	// Audit records an event. Nil records nothing, which only tests that are
	// not about it should want.
	Audit func(Event)
	// Logger receives the SDK's own log lines. Nil discards them.
	Logger *slog.Logger
	// SessionTimeout closes a session of the handshake revisions that has been
	// idle this long. Zero means thirty minutes.
	SessionTimeout time.Duration
}

// ToolSpec declares one tool.
type ToolSpec struct {
	// Name is the tool's name, as a client calls it.
	Name        string
	Title       string
	Description string
	// InputSchema is a JSON Schema whose root is an object.
	InputSchema json.RawMessage
	// Permission is an extra permission the caller needs on top of Permission.
	// Empty means Permission alone is enough.
	Permission string
}

// Call is one tool call, after the caller was identified and the arguments were
// checked against the tool's schema.
type Call struct {
	// ID identifies this call in the audit log and on the message.
	ID        string
	Tool      string
	Arguments map[string]any
	// Caller is the name the audit log has for the caller.
	Caller string
}

// Outcome is what a tool answers with.
type Outcome struct {
	// Text is the answer for a client that reads text.
	Text string
	// Structured is the answer as a JSON object, when it is one.
	Structured map[string]any
	// IsError says the tool ran and the work it was asked to do failed. The
	// client's model is shown the text and may try again.
	IsError bool
}

// Func runs one call. An error is a failure of the host's own, not of the work:
// the call is answered as a tool error carrying the error's words.
type Func func(ctx context.Context, c Call) (Outcome, error)

var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// permissionName is the shape of a permission the admin API already uses:
// letters, digits and dots, with an optional trailing .* in a grant.
var permissionName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)*$`)

// Compiled is a tool's schema, resolved and ready to check arguments with.
type Compiled struct {
	schema *schema.Schema
	raw    json.RawMessage
}

// Compile checks a spec and prepares its schema. It is what a node runs when
// the flow is deployed, so a tool that cannot be served is refused then and not
// when an agent first calls it.
//
// The schema is checked by internal/schema, the validator the json node uses,
// so a schema means the same thing in both places and an error names the bad
// field the same way. A schema that points at another URL is refused there.
func Compile(spec ToolSpec) (*Compiled, error) {
	if !toolName.MatchString(spec.Name) {
		return nil, fmt.Errorf("the tool name %q is not one MCP allows: 1 to 128 letters, digits, underscores, hyphens and dots", spec.Name)
	}
	if spec.Permission != "" && !permissionName.MatchString(spec.Permission) {
		return nil, fmt.Errorf("the permission %q is not valid: use letters, digits, underscores and dots, like mcp.line3", spec.Permission)
	}
	if len(strings.TrimSpace(string(spec.InputSchema))) == 0 {
		return nil, errors.New("the input schema is empty. Use {\"type\":\"object\"} for a tool that takes anything, or {\"type\":\"object\",\"additionalProperties\":false} for one that takes nothing")
	}
	var root map[string]any
	if err := json.Unmarshal(spec.InputSchema, &root); err != nil {
		return nil, fmt.Errorf("the input schema is not a JSON Schema: %w", err)
	}
	if root["type"] != "object" {
		return nil, errors.New(`the input schema has to describe an object: its "type" must be "object"`)
	}
	compiled, err := schema.Compile(spec.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("the input schema is not valid: %w", err)
	}
	return &Compiled{schema: compiled, raw: spec.InputSchema}, nil
}

// Check reports why arguments do not fit the schema, or nil. The error names
// the bad field, like arguments.line.
func (c *Compiled) Check(args map[string]any) error {
	if args == nil {
		args = map[string]any{}
	}
	return c.schema.Validate("arguments", args)
}

type tool struct {
	owner    string
	spec     ToolSpec
	compiled *Compiled
	fn       Func
}

// Registry is the set of tools flows are serving, and the MCP server in front
// of them.
type Registry struct {
	version string

	mu     sync.Mutex
	server *mcp.Server
	tools  map[string]*tool
	opts   Options

	// windows is each limited caller's count, by name, and now is the clock it
	// is read against, swapped only by tests.
	windows map[string]*window
	now     func() time.Time
}

// allow counts a call against the caller's limit. It reports whether the call
// may go ahead and, when it may not, how long until the window ends.
func (r *Registry) allow(id Identity) (bool, time.Duration) {
	if id.Limit.Calls <= 0 {
		return true, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	w := r.windows[id.Name]
	if w == nil || now.Sub(w.start) >= id.Limit.Per {
		w = &window{start: now}
		r.windows[id.Name] = w
	}
	if w.n >= id.Limit.Calls {
		return false, w.start.Add(id.Limit.Per).Sub(now)
	}
	w.n++
	return true, 0
}

// New makes an empty registry. version is what the server reports as its own.
func New(version string) *Registry {
	r := &Registry{version: version, tools: map[string]*tool{}, windows: map[string]*window{}, now: time.Now}
	r.server = r.newServer()
	return r
}

func (r *Registry) newServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hotloop-flow", Version: r.version}, &mcp.ServerOptions{
		Instructions: "Each tool here is a flow in HotLoop Flow. Calling one runs that flow with your arguments " +
			"and answers with what the flow answers. You run with the permissions of the token you connected " +
			"with, and every call is written to Flow's audit log under that token's name.",
		// A tools capability even before the first tool is registered, so a
		// client that connects ahead of a deploy is told this server has tools.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
		Logger:       r.logger(),
	})
	s.AddReceivingMiddleware(r.filterList)
	return s
}

func (r *Registry) logger() *slog.Logger {
	if r.opts.Logger != nil {
		return r.opts.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Register serves a tool until the returned function is called. Two tools may
// not share a name: the second is refused, and the flow that has it fails to
// start with the owner of the first named in the error.
func (r *Registry) Register(owner string, spec ToolSpec, fn Func) (func(), error) {
	compiled, err := Compile(spec)
	if err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, errors.New("no function to run")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.tools[spec.Name]; ok {
		return nil, fmt.Errorf("the tool %q is already served by node %s", spec.Name, existing.owner)
	}
	t := &tool{owner: owner, spec: spec, compiled: compiled, fn: fn}
	r.tools[spec.Name] = t
	r.server.AddTool(&mcp.Tool{
		Name:        spec.Name,
		Title:       spec.Title,
		Description: spec.Description,
		InputSchema: compiled.raw,
	}, r.handle(t))
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.tools[spec.Name] == t {
			delete(r.tools, spec.Name)
			r.server.RemoveTools(spec.Name)
		}
	}, nil
}

// Reset drops every tool. A runtime that is torn down calls it, so a node that
// failed to close cannot leave a tool pointing at a flow that is gone.
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.tools) == 0 {
		return
	}
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	r.tools = map[string]*tool{}
	r.server.RemoveTools(names...)
}

// Names lists the tools being served, sorted.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) lookup(name string) *tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tools[name]
}

func (r *Registry) audit(ev Event) {
	r.mu.Lock()
	fn := r.opts.Audit
	r.mu.Unlock()
	if fn != nil {
		fn(ev)
	}
}

// ---------------------------------------------------------------------------
// The caller
// ---------------------------------------------------------------------------

// callerKey is where a request's identity travels, from the HTTP handler to the
// token verifier the SDK runs.
type callerKey struct{}

type callerInfo struct {
	id           Identity
	remote       string
	forwardedFor string
}

func callerOf(extra *mcp.RequestExtra) (callerInfo, bool) {
	if extra == nil || extra.TokenInfo == nil {
		return callerInfo{}, false
	}
	ci, ok := extra.TokenInfo.Extra["caller"].(callerInfo)
	return ci, ok && ci.id.Allows != nil
}

func (ci callerInfo) event(name string, detail map[string]any) Event {
	return Event{Name: name, User: ci.id.Name, Remote: ci.remote, ForwardedFor: ci.forwardedFor, Detail: detail}
}

// permitted reports whether the caller may use a tool: the endpoint's
// permission, and the tool's own when it has one.
func permitted(id Identity, t *tool) (string, bool) {
	if !id.Allows(Permission) {
		return Permission, false
	}
	if t != nil && t.spec.Permission != "" && !id.Allows(t.spec.Permission) {
		return t.spec.Permission, false
	}
	return "", true
}

// filterList leaves out of tools/list the tools the caller may not call, so an
// agent is not offered what it will be refused.
func (r *Registry) filterList(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if method != "tools/list" || err != nil {
			return res, err
		}
		lr, ok := res.(*mcp.ListToolsResult)
		if !ok {
			return res, err
		}
		ci, ok := callerOf(req.GetExtra())
		if !ok {
			// Cannot happen behind Handler. If it does, say nothing.
			cp := *lr
			cp.Tools = []*mcp.Tool{}
			return &cp, nil
		}
		cp := *lr
		cp.Tools = make([]*mcp.Tool, 0, len(lr.Tools))
		for _, t := range lr.Tools {
			if _, ok := permitted(ci.id, r.lookup(t.Name)); ok {
				cp.Tools = append(cp.Tools, t)
			}
		}
		return &cp, nil
	}
}

func newCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// handle is the SDK-facing side of one tool: who is calling, whether they may,
// whether the arguments fit, then the flow, and the audit rows around it.
func (r *Registry) handle(t *tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ci, ok := callerOf(req.GetExtra())
		if !ok {
			// A call with no identity is refused, whatever put it here.
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "this call has no authenticated caller"}
		}
		start := time.Now()
		if perm, ok := permitted(ci.id, t); !ok {
			r.audit(ci.event(EventRefused, map[string]any{"tool": t.spec.Name, "node": t.owner, "reason": "missing permission " + perm}))
			return errorResult("the token lacks the " + perm + " permission"), nil
		}

		if ok, wait := r.allow(ci.id); !ok {
			secs := int(wait.Round(time.Second).Seconds())
			if secs < 1 {
				secs = 1
			}
			r.audit(ci.event(EventRefused, map[string]any{"tool": t.spec.Name, "node": t.owner,
				"reason": fmt.Sprintf("rate limit exceeded: %d calls per %s", ci.id.Limit.Calls, ci.id.Limit.Per)}))
			return errorResult(fmt.Sprintf("rate limit exceeded: this token may make %d calls per %s. Try again in %d seconds.",
				ci.id.Limit.Calls, ci.id.Limit.Per, secs)), nil
		}

		var args map[string]any
		if raw := req.Params.Arguments; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &args); err != nil {
				return errorResult("the arguments are not a JSON object: " + err.Error()), nil
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		if err := t.compiled.Check(args); err != nil {
			r.audit(ci.event(EventRefused, map[string]any{"tool": t.spec.Name, "node": t.owner, "reason": "arguments do not match the input schema"}))
			return errorResult("the arguments do not match the tool's input schema: " + err.Error()), nil
		}

		id := newCallID()
		// Written before the flow runs: a call that moves equipment is on the
		// record whether or not it finishes.
		r.audit(ci.event(EventCall, map[string]any{"tool": t.spec.Name, "node": t.owner, "callId": id}))

		out, err := t.fn(ctx, Call{ID: id, Tool: t.spec.Name, Arguments: args, Caller: ci.id.Name})
		detail := map[string]any{
			"tool": t.spec.Name, "node": t.owner, "callId": id,
			"ms": time.Since(start).Milliseconds(),
		}
		switch {
		case err != nil:
			detail["outcome"] = "error"
			detail["error"] = err.Error()
			out = Outcome{Text: err.Error(), IsError: true}
		case out.IsError:
			detail["outcome"] = "error"
		default:
			detail["outcome"] = "ok"
		}
		r.audit(ci.event(EventResult, detail))

		res := &mcp.CallToolResult{IsError: out.IsError, Content: []mcp.Content{&mcp.TextContent{Text: out.Text}}}
		if out.Structured != nil {
			res.StructuredContent = out.Structured
		}
		return res, nil
	}
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// Handler serves the MCP endpoint. Mount it at one path for POST, GET and
// DELETE: the handshake revisions use GET and DELETE as well as POST.
func (r *Registry) Handler(o Options) http.Handler {
	if o.SessionTimeout <= 0 {
		o.SessionTimeout = 30 * time.Minute
	}
	r.mu.Lock()
	r.opts = o
	r.mu.Unlock()

	get := func(*http.Request) *mcp.Server { return r.server }
	logger := r.logger()
	stateless := mcp.NewStreamableHTTPHandler(get, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		Logger:                       logger,
		PropagateRequestCancellation: true,
	})
	stateful := mcp.NewStreamableHTTPHandler(get, &mcp.StreamableHTTPOptions{
		Logger:         logger,
		SessionTimeout: o.SessionTimeout,
	})

	// The SDK takes the identity from a token verifier. Ours does not verify
	// anything: the caller was identified below, before the SDK saw the request,
	// and this hands that identity to the protocol layer as the request's token.
	verify := func(ctx context.Context, _ string, req *http.Request) (*auth.TokenInfo, error) {
		ci, ok := req.Context().Value(callerKey{}).(callerInfo)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{
			UserID:     ci.id.Name,
			Expiration: time.Now().Add(time.Hour),
			Extra:      map[string]any{"caller": ci},
		}, nil
	}
	protocol := auth.RequireBearerToken(verify, nil)(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if v := req.Header.Get("MCP-Protocol-Version"); v >= protocolStateless {
			stateless.ServeHTTP(w, req)
			return
		}
		stateful.ServeHTTP(w, req)
	}))
	// A browser page that does not share the endpoint's origin does not get to
	// call it, with or without a token.
	protocol = http.NewCrossOriginProtection().Handler(protocol)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id, ref := o.Authenticate(req)
		if ref != nil {
			if ref.Audit && o.Audit != nil {
				o.Audit(Event{Name: EventRefused, User: ref.User, Remote: req.RemoteAddr,
					ForwardedFor: req.Header.Get("X-Forwarded-For"),
					Detail:       map[string]any{"reason": ref.Message, "status": ref.Status}})
			}
			if ref.Status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="hotloop-flow"`)
			}
			writeJSONError(w, ref.Status, ref.Message)
			return
		}
		if id.Allows == nil || !id.Allows(Permission) {
			if o.Audit != nil {
				o.Audit(Event{Name: EventRefused, User: id.Name, Remote: req.RemoteAddr,
					ForwardedFor: req.Header.Get("X-Forwarded-For"),
					Detail:       map[string]any{"reason": "missing permission " + Permission, "status": http.StatusForbidden}})
			}
			writeJSONError(w, http.StatusForbidden, "token lacks the "+Permission+" permission")
			return
		}
		ci := callerInfo{id: id, remote: req.RemoteAddr, forwardedFor: req.Header.Get("X-Forwarded-For")}
		protocol.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), callerKey{}, ci)))
	})
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
