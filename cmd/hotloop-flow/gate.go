package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// testSuite is the flow test suite an instance keeps beside its flow file, and
// the way it's run: always in a child process, this same binary running
// `hotloop-flow test -stdin`.
//
// A child process rather than a second runtime in this one, on purpose. A test
// starts every node in the flow it tests, and some of them register in places
// the whole process shares: a Link In by its id, an HTTP route by its path. A
// candidate flow built in here would answer for the running one's Link In the
// moment it started. In a child it can't touch anything the running flows use,
// a node that panics on a goroutine of its own takes down the test and not the
// plant, and whatever memory the run needed goes back when it exits.
type testSuite struct {
	path    string
	gate    bool
	timeout time.Duration
	runtime BundleRuntime

	// command is the executable a run is made with, and its environment:
	// this binary in the field, the test binary under go test.
	command func() (string, []string, error)

	mu sync.Mutex
}

var _ api.TestSuite = (*testSuite)(nil)

func newTestSuite(cfg config.Config) *testSuite {
	return &testSuite{
		path:    cfg.TestsPath(),
		gate:    cfg.Tests.Gate,
		timeout: cfg.Tests.Timeout,
		runtime: BundleRuntime{
			InboxCapacity: cfg.Runtime.InboxCapacity,
			Overflow:      cfg.Runtime.Overflow,
			BlockTimeout:  cfg.Runtime.BlockTimeout,
		},
		command: func() (string, []string, error) {
			exe, err := os.Executable()
			return exe, os.Environ(), err
		},
	}
}

func (s *testSuite) Name() string { return filepath.Base(s.path) }
func (s *testSuite) Gate() bool   { return s.gate }

func (s *testSuite) Load() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the flow tests: %w", err)
	}
	return data, nil
}

// Save replaces the suite atomically: written beside it, flushed, renamed over
// it. A half-written suite is one the gate would refuse every deploy over.
func (s *testSuite) Save(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tests-*.yaml")
	if err != nil {
		return fmt.Errorf("saving the flow tests: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("saving the flow tests: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("saving the flow tests: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("saving the flow tests: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("saving the flow tests: %w", err)
	}
	return nil
}

// Run runs a suite against a flow file in a child process.
func (s *testSuite) Run(ctx context.Context, flows, tests []byte, match string) (flowtest.Report, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	bundle, err := json.Marshal(TestBundle{
		Flows: flows, Tests: string(tests), Runtime: s.runtime, Match: match, File: s.Name(),
	})
	if err != nil {
		return flowtest.Report{}, err
	}
	exe, env, err := s.command()
	if err != nil {
		return flowtest.Report{}, fmt.Errorf("finding this binary to run the flow tests with: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, "test", "-stdin")
	cmd.Env = env
	// Killing the child is not the end of waiting for it if anything it
	// started still holds its output open. This bounds that, so a stuck
	// test run costs a deploy tests.timeout and a second, not forever.
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(bundle)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &capped{buf: &stderr, max: 8 << 10}
	err = cmd.Run()
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return flowtest.Report{}, fmt.Errorf("the flow tests took longer than tests.timeout (%s) and were stopped", s.timeout)
	}
	if err != nil {
		return flowtest.Report{}, fmt.Errorf("running the flow tests: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var reports []flowtest.Report
	if err := json.Unmarshal(stdout.Bytes(), &reports); err != nil || len(reports) != 1 {
		return flowtest.Report{}, fmt.Errorf("the flow tests answered with something that isn't a report: %v", err)
	}
	return reports[0], nil
}

// capped keeps the first max bytes written to it and drops the rest, so a
// child that writes a novel to stderr can't grow this process.
type capped struct {
	buf *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// gate runs the suite against the flows a deploy is about to put in place, and
// refuses the deploy if anything in it doesn't pass. It runs before anything is
// written, saved or stopped, so a refused deploy leaves the running flows, the
// flow file, the credentials and the history exactly as they were.
func (a *application) gate(ctx context.Context, flows *engine.Flows) error {
	if a.tests == nil || !a.tests.Gate() {
		return nil
	}
	tests, err := a.tests.Load()
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(tests)) == 0 {
		// The gate is on and there's nothing to gate on. Nothing can fail,
		// so the deploy goes, and the log says why it wasn't checked.
		a.log.Warn("tests.gate is on but there are no flow tests", "file", a.tests.Name())
		return nil
	}
	doc, err := flows.MarshalJSON()
	if err != nil {
		return err
	}
	rep, err := a.tests.Run(ctx, doc, tests, "")
	if err != nil {
		// A gate that couldn't run the tests didn't see them pass.
		return fmt.Errorf("the deploy was refused because the flow tests couldn't be run: %w", err)
	}
	if !rep.OK() {
		a.log.Warn("a deploy was refused by its flow tests", "failed", rep.Failed, "errored", rep.Errored)
		return &api.TestsFailedError{Report: rep}
	}
	a.log.Info("the flow tests passed", "tests", len(rep.Tests))
	return nil
}
