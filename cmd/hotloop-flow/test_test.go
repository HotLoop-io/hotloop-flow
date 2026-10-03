package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

const testCmdFlows = `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"chk","type":"switch","z":"t1","name":"over limit?","property":"payload","propertyType":"msg","rules":[{"t":"gt","v":"80","vt":"num"},{"t":"else"}],"checkall":"false","outputs":2,"x":300,"y":100,"wires":[[],[]]}
]`

const testCmdSuite = `
tests:
  - name: high goes out of port 1
    inject: [{node: "over limit?", msg: {payload: 92}}]
    expect:
      - {node: "over limit?", port: 1, msg: {payload: 92}}
      - {node: "over limit?", port: 2, nothing: true}
`

const testCmdFailing = `
flows: line3.json
tests:
  - name: low goes out of port 1, which it doesn't
    timeout: 200ms
    inject: [{node: "over limit?", msg: {payload: 3}}]
    expect:
      - {node: "over limit?", port: 1}
`

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestTheTestCommand runs the command the way CI does: test files on the
// command line, text on stdout, a JUnit file beside it, and the exit status as
// the verdict.
func TestTheTestCommand(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"flows.json":      testCmdFlows,
		"line3.json":      testCmdFlows,
		"pass.test.yaml":  testCmdSuite,
		"fails.test.yaml": testCmdFailing,
	})
	junit := filepath.Join(dir, "report.xml")

	// One passing file: exit 0, and the flow file is the flows.json next to it.
	var out bytes.Buffer
	if err := cmdTest([]string{"-junit", junit, filepath.Join(dir, "pass.test.yaml")}, &out); err != nil {
		t.Fatalf("a passing run returned %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "--- PASS: high goes out of port 1") ||
		!strings.Contains(out.String(), "ok\t") {
		t.Errorf("the text report:\n%s", out.String())
	}
	if b, err := os.ReadFile(junit); err != nil || !strings.Contains(string(b), `<testcase name="high goes out of port 1"`) {
		t.Errorf("the JUnit file: %v\n%s", err, b)
	}

	// Add a failing file, which names its own flow file: exit 1.
	out.Reset()
	err := cmdTest([]string{"-junit", junit, filepath.Join(dir, "pass.test.yaml"), filepath.Join(dir, "fails.test.yaml")}, &out)
	var code exitCode
	if !errors.As(err, &code) || code != 1 {
		t.Fatalf("a failing run returned %v, want exit status 1\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "--- FAIL: low goes out of port 1") || !strings.Contains(out.String(), "FAIL\t") {
		t.Errorf("the text report:\n%s", out.String())
	}
	if b, _ := os.ReadFile(junit); !strings.Contains(string(b), `failures="1"`) {
		t.Errorf("the JUnit file doesn't count the failure:\n%s", b)
	}

	// -run picks tests by name, and -json is the same report for a program.
	out.Reset()
	err = cmdTest([]string{"-json", "-run", "^high", filepath.Join(dir, "pass.test.yaml"), filepath.Join(dir, "fails.test.yaml")}, &out)
	if err != nil {
		t.Fatalf("-run should have skipped the failing test: %v\n%s", err, out.String())
	}
	var reports []flowtest.Report
	if err := json.Unmarshal(out.Bytes(), &reports); err != nil {
		t.Fatalf("-json printed something that isn't JSON: %v\n%s", err, out.String())
	}
	if len(reports) != 2 || reports[0].Passed != 1 || len(reports[1].Tests) != 0 {
		t.Errorf("-run didn't select: %+v", reports)
	}

	// A test file that names a flow file that isn't there is refused before
	// anything runs.
	err = cmdTest([]string{"-flows", filepath.Join(dir, "nope.json"), filepath.Join(dir, "pass.test.yaml")}, &out)
	if err == nil || !strings.Contains(err.Error(), "the flow file it tests") {
		t.Errorf("a missing flow file: %v", err)
	}
}
