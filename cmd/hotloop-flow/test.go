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

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
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

// cmdTest runs flow test files.
func cmdTest(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	junit := fs.String("junit", "", "also write a JUnit XML report to this file")
	asJSON := fs.Bool("json", false, "print the reports as JSON instead of text")
	match := fs.String("run", "", "run only the tests whose names match this regular expression")
	flowsPath := fs.String("flows", "", "test this flow file instead of the one each test file names")
	verbose := fs.Bool("v", false, "print the flow's log under every test, not only under failures")
	fs.Usage = func() { fmt.Fprint(fs.Output(), testUsage) }
	if err := fs.Parse(args); err != nil {
		return err
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
