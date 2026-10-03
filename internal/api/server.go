// Package api serves the admin REST API and the editor's event websocket.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowdiff"
	"github.com/HotLoop-io/hotloop-flow/internal/flowhttp"
	"github.com/HotLoop-io/hotloop-flow/internal/history"
	"github.com/HotLoop-io/hotloop-flow/internal/metrics"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
	"github.com/HotLoop-io/hotloop-flow/web"
)

// Permissions the API checks. Mirrors Node-RED's granular scheme so a read-only
// dashboard token cannot deploy a flow.
const (
	PermFlowsRead  = "flows.read"
	PermFlowsWrite = "flows.write"
	PermStatusRead = "status.read"
	PermNodesRead  = "nodes.read"
	PermSettings   = "settings.read"
	PermInject     = "inject.write"
)

// Deps is everything the server needs from the rest of the process.
type Deps struct {
	Config      config.Config
	Registry    *node.Registry
	Flows       *store.FlowStore
	Credentials *store.CredentialStore
	History     *history.Log
	Logger      *slog.Logger

	// Runtime returns the currently running runtime. It is a function rather
	// than a value because a deploy replaces the whole runtime, and every
	// handler needs the live one rather than the one that existed at startup.
	Runtime func() *runtime.Runtime

	// Deploy swaps in a new flow set. It owns stopping the old runtime,
	// persisting, recording the deployment, and starting the new one.
	Deploy func(ctx context.Context, req DeployRequest) (DeployResult, error)

	// FlowRoutes holds the paths the flow's HTTP In nodes have claimed. It is
	// consulted for anything the fixed routes below did not match, because a
	// flow's routes change on every deploy and http.ServeMux cannot unregister
	// a pattern.
	FlowRoutes *flowhttp.Router

	// Version identifies this build in /settings and the log.
	Version string
}

// DeployRequest is a deploy and everything the deployment log records about it.
type DeployRequest struct {
	Flows       *engine.Flows
	ExpectedRev string
	// User is whoever the token belongs to, empty when authentication is off.
	User string
	// Remote is the address the request came from.
	Remote string
	// Note is the deployer's sentence for whoever reads the history next.
	Note string
}

// DeployResult reports what a deploy did.
type DeployResult struct {
	Rev string `json:"rev"`
	// Deployment is the deployment log's sequence number for this deploy, or
	// zero if the record could not be written (which is also a warning).
	Deployment int64                `json:"deployment,omitempty"`
	Failures   []runtime.StartError `json:"-"`
	Warnings   []string             `json:"warnings,omitempty"`
}

// Server is the HTTP surface.
type Server struct {
	deps   Deps
	mux    *http.ServeMux
	tokens *tokenStore
	hub    *hub
	log    *slog.Logger
	root   string
}

// New builds the server and wires its routes.
func New(deps Deps) *Server {
	root := strings.TrimSuffix(deps.Config.Server.AdminRoot, "/")

	s := &Server{
		deps:   deps,
		mux:    http.NewServeMux(),
		tokens: newTokenStore(deps.Config.Auth.SessionTTL),
		hub:    newHub(deps.Logger),
		log:    deps.Logger,
		root:   root,
	}
	s.routes()
	return s
}

// Hub returns the websocket fan-out, so the process can pump runtime events
// into it.
func (s *Server) Hub() *hub { return s.hub }

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.recoverPanic(s.logRequests(s.mux))
}

func (s *Server) path(p string) string {
	if s.root == "" {
		return p
	}
	return s.root + p
}

func (s *Server) routes() {
	// Unauthenticated. Health has to be reachable by the kubelet, which does
	// not carry a token, and the liveness probe failing because auth is on
	// would restart-loop the pod forever.
	s.mux.HandleFunc("GET "+s.path("/health"), s.handleHealth)
	s.mux.HandleFunc("GET "+s.path("/ready"), s.handleReady)
	s.mux.HandleFunc("POST "+s.path("/auth/token"), s.handleToken)
	s.mux.HandleFunc("POST "+s.path("/auth/revoke"), s.handleRevoke)

	// Metrics, unauthenticated like health. A Prometheus scraper carries no
	// bearer token, and requiring one would mean either handing a credential to
	// the monitoring stack or having no monitoring. The endpoint exposes counts
	// and node ids, never message contents or configuration.
	if s.deps.Config.Metrics.Enabled {
		path := s.deps.Config.Metrics.Path
		if path == "" {
			path = "/metrics"
		}
		s.mux.Handle("GET "+s.path(path), metrics.NewHandler(s.deps.Version, func() []metrics.NodeStat {
			rt := s.deps.Runtime()
			if rt == nil {
				return nil
			}
			snaps := rt.Snapshots()
			out := make([]metrics.NodeStat, 0, len(snaps))
			for _, sn := range snaps {
				out = append(out, metrics.NodeStat{
					NodeID: sn.NodeID, Type: sn.Type,
					Received: sn.Received, Sent: sn.Sent, Errors: sn.Errors,
					Dropped: sn.Dropped, Blocked: sn.Blocked,
					QueueLen: sn.QueueLen, QueueCap: sn.QueueCap, QueueHigh: sn.QueueHigh,
				})
			}
			return out
		}))
	}

	// Authenticated.
	s.mux.Handle("GET "+s.path("/settings"), s.auth(PermSettings, s.handleSettings))
	s.mux.Handle("GET "+s.path("/nodes"), s.auth(PermNodesRead, s.handleNodes))
	s.mux.Handle("GET "+s.path("/flows"), s.auth(PermFlowsRead, s.handleGetFlows))
	s.mux.Handle("POST "+s.path("/flows"), s.auth(PermFlowsWrite, s.handlePostFlows))
	s.mux.Handle("GET "+s.path("/deployments"), s.auth(PermFlowsRead, s.handleListDeployments))
	s.mux.Handle("GET "+s.path("/deployments/{seq}"), s.auth(PermFlowsRead, s.handleGetDeployment))
	s.mux.Handle("GET "+s.path("/deployments/{seq}/flows"), s.auth(PermFlowsRead, s.handleGetDeploymentFlows))
	s.mux.Handle("GET "+s.path("/deployments/{from}/diff/{to}"), s.auth(PermFlowsRead, s.handleDiffDeployments))
	s.mux.Handle("POST "+s.path("/flows/diff"), s.auth(PermFlowsRead, s.handleDiffPending))
	s.mux.Handle("GET "+s.path("/runtime/stats"), s.auth(PermStatusRead, s.handleStats))
	s.mux.Handle("POST "+s.path("/inject/{id}"), s.auth(PermInject, s.handleInject))
	s.mux.Handle("GET "+s.path("/comms"), s.auth(PermStatusRead, s.handleComms))

	// The editor. Unauthenticated on purpose: it is a static bundle that renders
	// a login form, and every call it makes afterwards carries a token. Putting
	// auth in front of the login page itself would be a loop.
	//
	// Registered last and on the bare prefix, so it only catches what the API
	// routes above did not. ServeMux prefers the most specific pattern, so every
	// route registered above still wins over this one and a flow cannot shadow
	// the admin API by claiming its path.
	editorRoot := s.root + "/"
	editor := web.Handler(s.root)

	flowRoot := "/"
	if s.deps.FlowRoutes != nil {
		flowRoot = s.deps.FlowRoutes.Root()
	}
	if flowRoot != "/" {
		flowRoot = strings.TrimSuffix(flowRoot, "/") + "/"
	}

	if flowRoot == editorRoot {
		// Both live at the same prefix, which is the default. One handler tries
		// the flow's routes and falls back to the editor. Registering two
		// patterns would be a duplicate-registration panic.
		s.mux.Handle(editorRoot, s.flowThenEditor(editor))
		return
	}
	s.mux.Handle(flowRoot, http.HandlerFunc(s.serveFlowRoute))
	s.mux.Handle("GET "+editorRoot, editor)
}

// flowThenEditor dispatches to a flow's HTTP In node if one claimed the path,
// and serves the editor otherwise.
//
// The order matters and is this way round because the editor's handler answers
// everything — it serves index.html for any unknown path so the single-page app
// can route client-side. Asking it first would mean no flow route ever ran.
func (s *Server) flowThenEditor(editor http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.dispatchFlowRoute(w, r) {
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// The editor only answers GET. Without this, a POST to a path with
			// no flow route would be served index.html with a 200, which reads
			// as success to whatever sent it.
			writeError(w, http.StatusNotFound, "no flow serves "+r.Method+" "+r.URL.Path)
			return
		}
		editor.ServeHTTP(w, r)
	})
}

func (s *Server) serveFlowRoute(w http.ResponseWriter, r *http.Request) {
	if s.dispatchFlowRoute(w, r) {
		return
	}
	writeError(w, http.StatusNotFound, "no flow serves "+r.Method+" "+r.URL.Path)
}

// dispatchFlowRoute runs a flow route if one matches, reporting whether it did.
func (s *Server) dispatchFlowRoute(w http.ResponseWriter, r *http.Request) bool {
	if s.deps.FlowRoutes == nil {
		return false
	}
	h, params, ok := s.deps.FlowRoutes.Match(r.Method, r.URL.Path)
	if !ok {
		return false
	}
	h.ServeHTTP(w, flowhttp.WithRouteParams(r, params))
	return true
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// recoverPanic keeps a bug in one handler from taking the process down. The
// runtime is still moving messages for a production line while this HTTP server
// runs; an editor request must not be able to stop it.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic serving request",
					"method", r.Method, "path", r.URL.Path, "panic", p)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying ResponseWriter so the websocket upgrade can
// hijack the connection through the wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// auth requires a valid token carrying the given permission.
func (s *Server) auth(perm string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.deps.Config.Auth.Enabled {
			// Only reachable when the operator set HOTLOOP_FLOW_INSECURE, which
			// config.Validate makes them do deliberately.
			h(w, r)
			return
		}

		tok := bearerToken(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "no bearer token")
			return
		}
		user, ok := s.tokens.lookup(tok)
		if !ok {
			writeError(w, http.StatusUnauthorized, "token is invalid or has expired")
			return
		}
		if !user.Allows(perm) {
			writeError(w, http.StatusForbidden, "token lacks the "+perm+" permission")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user.Username)))
	})
}

// userKey carries the authenticated username on a request's context.
type userKey struct{}

// requestUser is who made the request, or "" when authentication is off. It is
// what the deployment log writes down, so it comes from the token, never from
// anything the client says about itself.
func requestUser(r *http.Request) string {
	u, _ := r.Context().Value(userKey{}).(string)
	return u
}

// bearerToken reads the token from the Authorization header, or from a query
// parameter for the websocket — browsers cannot set headers on a WebSocket
// handshake.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, found := strings.CutPrefix(h, "Bearer "); found {
			return strings.TrimSpace(after)
		}
	}
	if r.Header.Get("Upgrade") == "websocket" {
		return r.URL.Query().Get("access_token")
	}
	return ""
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.deps.Version,
	})
}

// handleReady reports whether the runtime is up. Distinct from health so a
// runtime that failed to start takes the pod out of the Service without the
// kubelet restarting it in a loop.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	rt := s.deps.Runtime()
	if rt == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "starting"})
		return
	}
	recovered, from := s.deps.Flows.Recovered()
	body := map[string]any{"status": "ok"}
	if recovered {
		// Surfaced rather than buried in a log line: the operator needs to know
		// their flow file was corrupt and which backup is running.
		body["recoveredFromBackup"] = from
	}
	writeJSON(w, http.StatusOK, body)
}

type tokenRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	user, found := s.deps.Config.FindUser(req.Username)
	// Always run the hash comparison, even for an unknown user, so that response
	// timing does not reveal which usernames exist.
	valid := config.CheckPassword(user.PasswordHash, req.Password)
	if !found || !valid {
		s.log.Warn("failed login", "username", req.Username, "remote", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	tok, expires := s.tokens.issue(user)
	s.log.Info("login", "username", user.Username, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"token_type":   "Bearer",
		"expires_in":   int(time.Until(expires).Seconds()),
	})
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if tok := bearerToken(r); tok != "" {
		s.tokens.revoke(tok)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSettings(w http.ResponseWriter, _ *http.Request) {
	cfg := s.deps.Config
	// Deliberately narrow. The credential secret, the password hashes and the
	// filesystem layout are not the editor's business, and this response goes
	// to a browser.
	writeJSON(w, http.StatusOK, map[string]any{
		"version":   s.deps.Version,
		"adminRoot": cfg.Server.AdminRoot,
		"runtime": map[string]any{
			"inboxCapacity": cfg.Runtime.InboxCapacity,
			"overflow":      cfg.Runtime.Overflow,
		},
		// The editor has no other way to know it may skip the login screen, and
		// a login screen with no users behind it is a locked door.
		"auth":      map[string]any{"enabled": cfg.Auth.Enabled},
		"discovery": map[string]any{"enabled": cfg.Discovery.Enabled},
		"metrics":   map[string]any{"enabled": cfg.Metrics.Enabled, "path": cfg.Metrics.Path},
		"editor":    map[string]any{"theme": "hotloop"},
	})
}

// handleNodes returns every registered node type, which is what the editor
// renders its palette and every edit dialog from. Node-RED serves an HTML
// document here containing a hand-written dialog per node; this is JSON.
func (s *Server) handleNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Registry.Descriptors())
}

func (s *Server) handleGetFlows(w http.ResponseWriter, _ *http.Request) {
	flows, err := s.deps.Flows.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	raw, err := flows.MarshalJSON()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	// Assembled by hand so the flow array goes out as the raw bytes the store
	// produced, preserving key order rather than being re-encoded through a map.
	fmt.Fprintf(w, `{"rev":%q,"flows":%s`, flows.Rev, raw)
	if len(flows.Warnings) > 0 {
		wb, _ := json.Marshal(flows.Warnings)
		fmt.Fprintf(w, `,"warnings":%s`, wb)
	}
	fmt.Fprint(w, "}")
}

func (s *Server) handlePostFlows(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.Config.Server.MaxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "flow document is too large")
		return
	}

	flows, err := engine.ParseFlows(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The editor sends the revision it last read. An empty one forces the
	// write, which is the "overwrite anyway" path.
	expectedRev := flows.Rev
	if h := r.Header.Get("HotLoop-Flow-Deployment-Rev"); h != "" {
		expectedRev = h
	}

	note, err := deployNote(r, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.deps.Deploy(r.Context(), DeployRequest{
		Flows:       flows,
		ExpectedRev: expectedRev,
		User:        requestUser(r),
		Remote:      r.RemoteAddr,
		Note:        note,
	})
	switch {
	case errors.Is(err, store.ErrRevisionConflict):
		// 409 rather than 500: the client can resolve this by reloading, and
		// the editor shows a merge prompt.
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Per-node start failures are reported, not fatal. One unknown node type
	// must not take a whole deploy down.
	failures := make([]map[string]string, 0, len(res.Failures))
	for _, f := range res.Failures {
		failures = append(failures, map[string]string{
			"id": f.NodeID, "type": f.Type, "error": f.Err.Error(),
		})
	}
	out := map[string]any{
		"rev":      res.Rev,
		"warnings": res.Warnings,
		"failures": failures,
	}
	if res.Deployment > 0 {
		out["deployment"] = res.Deployment
	}
	writeJSON(w, http.StatusOK, out)
}

// deployNote reads the note for the deployment log. It can ride in the
// wrapped document as "note", which is how the editor sends it, because a
// browser refuses to put anything outside Latin-1 in a header and people write
// notes in their own language. A CI job can use the header instead.
func deployNote(r *http.Request, body []byte) (string, error) {
	note := r.Header.Get("HotLoop-Flow-Deployment-Note")
	if trimmed := strings.TrimLeft(string(body), " \t\r\n"); strings.HasPrefix(trimmed, "{") {
		var wrapper struct {
			Note *string `json:"note"`
		}
		if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Note != nil {
			note = *wrapper.Note
		}
	}
	note = strings.TrimSpace(note)
	if len(note) > history.MaxNoteLength {
		return "", fmt.Errorf("the deploy note is %d bytes; the limit is %d", len(note), history.MaxNoteLength)
	}
	return note, nil
}

// handleListDeployments is the deployment log, newest first, without the flow
// bytes. ?limit=N bounds it.
func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	if s.deps.History == nil {
		writeError(w, http.StatusServiceUnavailable, "the deployment log is not available")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = n
	}
	list := s.deps.History.List(limit)
	if list == nil {
		list = []history.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deployments": list,
		"retain":      s.deps.History.Retain(),
		"current":     s.deps.Flows.Rev(),
	})
}

// handleGetDeployment returns one record with its flows, exactly as they were
// written. The credentials stay on the server: they are encrypted, they are
// only ever needed for a rollback, and a history endpoint is not a way to walk
// off with them.
func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.deploymentFromPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, deploymentView(rec))
}

// handleGetDeploymentFlows returns a deployment's flow file byte for byte, the
// way it sat on disk, so what you download is what ran and a diff against it
// shows nothing that didn't change.
func (s *Server) handleGetDeploymentFlows(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.deploymentFromPath(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="flows-%d.json"`, rec.Seq))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rec.Flows)
}

// diffJSON is a semantic diff as the API returns it: the structured result for
// a program, the text a person would read, and the two revisions compared.
type diffJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
	flowdiff.Result
	Text string `json:"text"`
}

// handleDiffDeployments compares two deployments, node by node.
func (s *Server) handleDiffDeployments(w http.ResponseWriter, r *http.Request) {
	from, ok := s.deploymentNamed(w, r.PathValue("from"))
	if !ok {
		return
	}
	to, ok := s.deploymentNamed(w, r.PathValue("to"))
	if !ok {
		return
	}
	oldF, err := engine.ParseFlows(from.Flows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("deployment %d: %v", from.Seq, err))
		return
	}
	newF, err := engine.ParseFlows(to.Flows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("deployment %d: %v", to.Seq, err))
		return
	}
	res := flowdiff.Diff(oldF, newF)
	writeJSON(w, http.StatusOK, diffJSON{From: from.Rev, To: to.Rev, Result: res, Text: flowdiff.Text(res, oldF, newF)})
}

// handleDiffPending compares what is live with a document that hasn't been
// deployed yet: what "review and deploy" shows before anybody presses the
// button.
func (s *Server) handleDiffPending(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.Config.Server.MaxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "flow document is too large")
		return
	}
	pending, err := engine.ParseFlows(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	live, err := s.deps.Flows.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := flowdiff.Diff(live, pending)
	writeJSON(w, http.StatusOK, diffJSON{From: live.Rev, To: "pending", Result: res, Text: flowdiff.Text(res, live, pending)})
}

// deploymentFromPath reads the {seq} in the path and loads that record,
// writing the error response itself when it can't.
func (s *Server) deploymentFromPath(w http.ResponseWriter, r *http.Request) (history.Record, bool) {
	return s.deploymentNamed(w, r.PathValue("seq"))
}

// deploymentNamed loads a record by its sequence number as it appeared in a
// path, writing the error response itself when it can't.
func (s *Server) deploymentNamed(w http.ResponseWriter, name string) (history.Record, bool) {
	if s.deps.History == nil {
		writeError(w, http.StatusServiceUnavailable, "the deployment log is not available")
		return history.Record{}, false
	}
	seq, err := strconv.ParseInt(name, 10, 64)
	if err != nil || seq < 1 {
		writeError(w, http.StatusBadRequest, "a deployment is a positive sequence number")
		return history.Record{}, false
	}
	rec, err := s.deps.History.Get(seq)
	switch {
	case errors.Is(err, history.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
		return history.Record{}, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return history.Record{}, false
	}
	return rec, true
}

// deploymentJSON is a record as the API shows it: the metadata, the flows as
// JSON rather than base64, and whether credentials were captured, never what
// they are.
type deploymentJSON struct {
	history.Record
	Flows          json.RawMessage `json:"flows"`
	Credentials    *struct{}       `json:"credentials,omitempty"`
	HasCredentials bool            `json:"hasCredentials"`
}

func deploymentView(rec history.Record) deploymentJSON {
	flows := json.RawMessage(rec.Flows)
	if len(rec.Flows) == 0 {
		flows = json.RawMessage("[]")
	}
	return deploymentJSON{Record: rec.Meta(), Flows: flows, HasCredentials: len(rec.Credentials) > 0}
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	rt := s.deps.Runtime()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime is not started")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": rt.Snapshots()})
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	rt := s.deps.Runtime()
	if rt == nil {
		writeError(w, http.StatusServiceUnavailable, "runtime is not started")
		return
	}
	id := r.PathValue("id")

	m := engine.NewMsg()
	if r.ContentLength > 0 {
		var payload map[string]any
		if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &payload); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		m = engine.WrapMsg(payload)
	}

	if err := rt.Inject(id, m); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"injected": id, "msgId": m.ID()})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, maxBytes int64, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes))
	if err != nil {
		return fmt.Errorf("reading request body: %w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("request body is empty")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("parsing request body: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

type tokenStore struct {
	mu     sync.RWMutex
	ttl    time.Duration
	issued map[string]tokenEntry
}

type tokenEntry struct {
	user    config.User
	expires time.Time
}

func newTokenStore(ttl time.Duration) *tokenStore {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &tokenStore{ttl: ttl, issued: map[string]tokenEntry{}}
}

func (t *tokenStore) issue(u config.User) (string, time.Time) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform we ship to. If it somehow
		// did, issuing a predictable token would be far worse than panicking.
		panic("hotloop-flow: crypto/rand failed while issuing a token: " + err.Error())
	}
	tok := hex.EncodeToString(b[:])
	expires := time.Now().Add(t.ttl)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	t.issued[tok] = tokenEntry{user: u, expires: expires}
	return tok, expires
}

func (t *tokenStore) lookup(tok string) (config.User, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// Constant-time comparison against each candidate would be ideal, but a map
	// lookup on a 256-bit random token leaks nothing useful: there is no
	// prefix to walk when the whole key must match to hash to the right bucket.
	e, ok := t.issued[tok]
	if !ok || time.Now().After(e.expires) {
		return config.User{}, false
	}
	return e.user, true
}

func (t *tokenStore) revoke(tok string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.issued {
		if subtle.ConstantTimeCompare([]byte(k), []byte(tok)) == 1 {
			delete(t.issued, k)
		}
	}
}

// sweepLocked drops expired tokens. Called on issue rather than on a timer, so
// an idle instance holds no goroutine for it.
func (t *tokenStore) sweepLocked() {
	now := time.Now()
	for k, e := range t.issued {
		if now.After(e.expires) {
			delete(t.issued, k)
		}
	}
}
