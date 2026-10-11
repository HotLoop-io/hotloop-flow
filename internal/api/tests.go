package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/audit"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// TestSuite is this instance's flow tests: the YAML kept beside the flow file,
// and a way to run it.
type TestSuite interface {
	// Load returns the suite, or nil when there isn't one.
	Load() ([]byte, error)
	// Save replaces the suite. It has already been parsed and checked.
	Save(data []byte) error
	// Run runs a suite against a flow file and reports what happened, only
	// the tests whose names match matches when it isn't empty. An error means
	// the tests couldn't be run at all, not that they failed.
	Run(ctx context.Context, flows, tests []byte, match string) (flowtest.Report, error)
	// Gate reports whether a deploy has to pass the suite.
	Gate() bool
	// Name is the suite's file name, for the editor to show.
	Name() string
}

// TestsFailedError is a deploy the gate refused: the flows ran against the
// suite and something didn't pass. It carries the whole report, because "the
// tests failed" with no assertions is a refusal nobody can act on.
type TestsFailedError struct {
	Report flowtest.Report
}

func (e *TestsFailedError) Error() string {
	r := e.Report
	var b strings.Builder
	fmt.Fprintf(&b, "the deploy was refused: %d of %d flow tests didn't pass, and the flows already running are untouched",
		r.Failed+r.Errored, len(r.Tests))
	shown := 0
	for _, t := range r.Tests {
		if t.Status == flowtest.Pass {
			continue
		}
		if shown == 3 {
			b.WriteString("; and more in the report")
			break
		}
		shown++
		fmt.Fprintf(&b, "; %q: ", t.Name)
		if len(t.Problems) > 0 {
			first, _, _ := strings.Cut(t.Problems[0].String(), "\n")
			b.WriteString(first)
		} else {
			b.WriteString(string(t.Status))
		}
	}
	return b.String()
}

// handleGetTests returns the suite as it's kept, and whether deploys are gated
// on it.
func (s *Server) handleGetTests(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Tests == nil {
		writeError(w, http.StatusNotFound, "this instance keeps no flow tests")
		return
	}
	data, err := s.deps.Tests.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	count := 0
	if suite, err := flowtest.Parse(data); err == nil {
		count = len(suite.Tests)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tests": string(data),
		"count": count,
		"file":  s.deps.Tests.Name(),
		"gate":  s.deps.Tests.Gate(),
	})
}

// handlePutTests replaces the suite. The body is the YAML itself. A suite that
// doesn't parse is refused with the reason, so the editor never saves one the
// gate would then choke on.
func (s *Server) handlePutTests(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tests == nil {
		writeError(w, http.StatusNotFound, "this instance keeps no flow tests")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.Config.Server.MaxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the test suite is too large")
		return
	}
	suite, err := flowtest.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.deps.Tests.Save(body); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.record(r, audit.TestsSaved, requestUser(r), map[string]any{"tests": len(suite.Tests)})
	writeJSON(w, http.StatusOK, map[string]any{"tests": len(suite.Tests)})
}

// runRequest is POST /tests/run. Both halves are optional: no flows runs the
// suite against what's running, no tests runs the suite this instance keeps.
// The editor sends its working copy, so a test can be run against a change
// before anybody deploys it.
type runRequest struct {
	Flows json.RawMessage `json:"flows"`
	Tests *string         `json:"tests"`
	// Match runs only the tests whose names it matches, a regular expression.
	Match string `json:"match"`
}

func (s *Server) handleRunTests(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tests == nil {
		writeError(w, http.StatusNotFound, "this instance keeps no flow tests")
		return
	}
	var req runRequest
	if err := readJSON(r, s.deps.Config.Server.MaxRequestBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tests := []byte(nil)
	if req.Tests != nil {
		tests = []byte(*req.Tests)
	} else {
		data, err := s.deps.Tests.Load()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		tests = data
	}
	if len(strings.TrimSpace(string(tests))) == 0 {
		writeError(w, http.StatusNotFound, "there are no flow tests to run")
		return
	}
	if _, err := flowtest.Parse(tests); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	flows := []byte(req.Flows)
	if len(flows) == 0 {
		live, err := s.deps.Flows.Load()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if flows, err = live.MarshalJSON(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		// Wrapped or bare, the way a deploy takes it, and checked the same way
		// before a child process is spent on it.
		parsed, err := engine.ParseFlows(flows)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		parsed.StripCredentials()
		if flows, err = parsed.MarshalJSON(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	rep, err := s.deps.Tests.Run(r.Context(), flows, tests, req.Match)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// fromDebugRequest is POST /tests/from-debug: the Debug node and the message
// id the debug sidebar showed.
type fromDebugRequest struct {
	Node  string `json:"node"`
	MsgID string `json:"msgId"`
}

// handleTestFromDebug drafts a test from a message the debug sidebar showed:
// what came in at the source, and what the Debug node showed. It saves
// nothing; the editor shows the draft and the person decides.
func (s *Server) handleTestFromDebug(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tests == nil || s.deps.Capture == nil {
		writeError(w, http.StatusNotFound, "this instance keeps no flow tests")
		return
	}
	var req fromDebugRequest
	if err := readJSON(r, 4096, &req); err != nil || req.Node == "" || req.MsgID == "" {
		writeError(w, http.StatusBadRequest, "give the Debug node's id as node and the message's id as msgId")
		return
	}
	live, err := s.deps.Flows.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	doc, err := live.MarshalJSON()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, err := flowtest.FromDebug(doc, req.Node, s.deps.Capture.Find(req.MsgID))
	switch {
	case errors.Is(err, flowtest.ErrNotCaptured):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	text, err := flowtest.CaseYAML(c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": c.Name, "yaml": text})
}

// handleAddTests adds the tests in the body, a test file of their own, to the
// suite. A name the suite already has gets a number on the end rather than
// replacing the test somebody wrote.
func (s *Server) handleAddTests(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tests == nil {
		writeError(w, http.StatusNotFound, "this instance keeps no flow tests")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.Config.Server.MaxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the tests are too large")
		return
	}
	add, err := flowtest.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	suite, err := s.deps.Tests.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var names []string
	for _, c := range add.Tests {
		var name string
		if suite, name, err = flowtest.AppendCase(suite, c); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		names = append(names, name)
	}
	if err := s.deps.Tests.Save(suite); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.record(r, audit.TestsSaved, requestUser(r), map[string]any{"added": names})
	writeJSON(w, http.StatusOK, map[string]any{"added": names})
}
