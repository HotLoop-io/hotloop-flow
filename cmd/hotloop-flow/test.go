package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

const testUsage = `usage:
  hotloop-flow test [-junit report.xml] [-json] [-run regexp] [-flows flows.json] [-v] <tests.yaml>...

Runs flow tests: the real runtime and the real nodes, in this process, no
server needed. Each test file names the flow file it tests ("flows:", relative
to the test file), or tests flows.json next to it; -flows overrides both.

A test puts messages into the flow and says what has to come out:

  tests:
    - name: a high reading trips the alarm
      inject:
        - node: temperature            # by name or id
          msg: {topic: line3/temp, payload: 92.5}
      expect:
        - node: over limit?            # first output unless port says otherwise
          port: 1
          msg: {payload: 92.5}
          within: 500ms
        - node: over limit?
          port: 2
          nothing: true
        - node: errors                 # a Catch node
          error: "out of range"

Exits 0 when every test passes and 1 when anything fails or can't run.
`

// TestBundle is what the deploy gate hands a child process to test: one flow
// file, one suite, and the scheduler settings the flows would really run
// with. It goes in on stdin, so nothing about a deploy that might be refused
// is ever written to the disk.
type TestBundle struct {
	Flows   json.RawMessage `json:"flows"`
	Tests   string          `json:"tests"`
	Runtime BundleRuntime   `json:"runtime"`
	Match   string          `json:"match,omitempty"`
	File    string          `json:"file,omitempty"`
}

// BundleRuntime is the part of runtime.Options a bundle carries: the queues
// the flows will really have, so a test sees an overflow the deploy would.
type BundleRuntime struct {
	InboxCapacity int           `json:"inboxCapacity,omitempty"`
	Overflow      string        `json:"overflow,omitempty"`
	BlockTimeout  time.Duration `json:"blockTimeout,omitempty"`
}

// cmdTest runs flow test files.
func cmdTest(args []string, stdout io.Writer) error {
	return cmdTestFrom(args, os.Stdin, stdout)
}

func cmdTestFrom(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	junit := fs.String("junit", "", "also write a JUnit XML report to this file")
	asJSON := fs.Bool("json", false, "print the reports as JSON instead of text")
	match := fs.String("run", "", "run only the tests whose names match this regular expression")
	flowsPath := fs.String("flows", "", "test this flow file instead of the one each test file names")
	verbose := fs.Bool("v", false, "print the flow's log under every test, not only under failures")
	bundle := fs.Bool("stdin", false, "read one flow file and one suite as a JSON bundle on stdin and print the report as JSON; what the deploy gate runs")
	fs.Usage = func() { fmt.Fprint(fs.Output(), testUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bundle {
		if fs.NArg() > 0 {
			return errors.New("-stdin reads its tests from stdin; give no test files with it")
		}
		return testBundle(stdin, stdout)
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("test needs at least one test file")
	}

	opts := flowtest.Options{Registry: node.Default}
	if *match != "" {
		re, err := regexp.Compile(*match)
		if err != nil {
			return fmt.Errorf("-run: %w", err)
		}
		opts.Match = re
	}

	// Everything is read and checked before anything runs, so a typo in the
	// third file is found in a second rather than after the first two have
	// run for a minute.
	type job struct {
		file  string
		suite *flowtest.Suite
		flows []byte
	}
	var jobs []job
	for _, file := range fs.Args() {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		suite, err := flowtest.Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		path := *flowsPath
		if path == "" {
			path = suite.Flows
			if path == "" {
				path = "flows.json"
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(file), path)
			}
		}
		flows, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: the flow file it tests: %w", file, err)
		}
		jobs = append(jobs, job{file: file, suite: suite, flows: flows})
	}

	ctx := context.Background()
	reports := make([]flowtest.Report, 0, len(jobs))
	ok := true
	for _, j := range jobs {
		r := flowtest.Run(ctx, j.flows, j.suite, opts)
		r.File = j.file
		reports = append(reports, r)
		ok = ok && r.OK()
		if !*asJSON {
			if err := flowtest.WriteText(stdout, &r, *verbose); err != nil {
				return err
			}
		}
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			return err
		}
	}
	if *junit != "" {
		if err := writeJUnit(*junit, reports); err != nil {
			return err
		}
	}
	if !ok {
		return exitCode(1)
	}
	return nil
}

// testBundle runs a bundle from stdin and prints the report as JSON. The exit
// status is 0 whether the tests pass or fail: the report says which, and a
// non-zero status is kept for a bundle that couldn't be run at all.
func testBundle(stdin io.Reader, stdout io.Writer) error {
	var b TestBundle
	dec := json.NewDecoder(stdin)
	if err := dec.Decode(&b); err != nil {
		return fmt.Errorf("reading the test bundle: %w", err)
	}
	suite, err := flowtest.Parse([]byte(b.Tests))
	if err != nil {
		return err
	}
	opts := flowtest.Options{Registry: node.Default, Runtime: runtime.Options{
		InboxCapacity: b.Runtime.InboxCapacity,
		Overflow:      runtime.OverflowPolicy(b.Runtime.Overflow),
		BlockTimeout:  b.Runtime.BlockTimeout,
	}}
	if b.Match != "" {
		if opts.Match, err = regexp.Compile(b.Match); err != nil {
			return fmt.Errorf("match: %w", err)
		}
	}
	r := flowtest.Run(context.Background(), b.Flows, suite, opts)
	r.File = b.File
	return json.NewEncoder(stdout).Encode([]flowtest.Report{r})
}

func writeJUnit(path string, reports []flowtest.Report) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("-junit: %w", err)
	}
	if err := flowtest.WriteJUnit(f, reports); err != nil {
		f.Close()
		return fmt.Errorf("-junit: %w", err)
	}
	return f.Close()
}
