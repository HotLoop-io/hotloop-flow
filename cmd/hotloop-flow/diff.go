package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowdiff"
)

// exitCode ends the process with a status and nothing printed, for commands
// whose exit status is the answer.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

const diffUsage = `usage:
  hotloop-flow diff [-json] <old flows.json> <new flows.json>
  hotloop-flow diff <path> <old-file> <old-hex> <old-mode> <new-file> <new-hex> <new-mode>

Compares two flow files node by node: what was added, removed or changed,
property by property, which wires moved, and what only moved on the canvas.
Exits 0 when nothing differs and 1 when something does, like diff(1).

The second form is git's external diff interface, so git diff shows the
semantic diff for flow files:

  git config diff.hotloop-flow.command 'hotloop-flow diff'
  echo 'flows.json diff=hotloop-flow' >> .gitattributes

Or run it as a difftool:

  git config difftool.hotloop-flow.cmd 'hotloop-flow diff "$LOCAL" "$REMOTE"'
  git difftool -t hotloop-flow
`

// cmdDiff compares two flow files.
func cmdDiff(args []string, stdout io.Writer) error {
	// Git's external diff driver calls with exactly seven arguments and no
	// flags. Recognised by shape, because git can't be told to pass a flag.
	if len(args) == 7 && !strings.HasPrefix(args[0], "-") {
		return gitExternalDiff(args, stdout)
	}

	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the diff as JSON")
	fs.Usage = func() { fmt.Fprint(fs.Output(), diffUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return errors.New("diff needs two flow files")
	}

	oldF, err := readFlowFile(fs.Arg(0))
	if err != nil {
		return err
	}
	newF, err := readFlowFile(fs.Arg(1))
	if err != nil {
		return err
	}
	res := flowdiff.Diff(oldF, newF)

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else {
		fmt.Fprint(stdout, flowdiff.Text(res, oldF, newF))
	}
	if !res.Empty() {
		return exitCode(1)
	}
	return nil
}

// gitExternalDiff answers git's GIT_EXTERNAL_DIFF call:
// path old-file old-hex old-mode new-file new-hex new-mode. It exits 0 whatever
// it finds, because git stops the whole diff when a driver exits otherwise.
func gitExternalDiff(args []string, stdout io.Writer) error {
	path, oldFile, newFile := args[0], args[1], args[4]
	oldF, err := readFlowFile(oldFile)
	if err != nil {
		return fmt.Errorf("%s (old side): %w", path, err)
	}
	newF, err := readFlowFile(newFile)
	if err != nil {
		return fmt.Errorf("%s (new side): %w", path, err)
	}
	fmt.Fprintf(stdout, "hotloop-flow diff %s\n", path)
	fmt.Fprint(stdout, flowdiff.Text(flowdiff.Diff(oldF, newF), oldF, newF))
	return nil
}

// readFlowFile parses a flow file. Git names a side that doesn't exist (an
// added or deleted file) /dev/null, which reads as no flows at all.
func readFlowFile(path string) (*engine.Flows, error) {
	if path == "/dev/null" || strings.EqualFold(path, "nul") {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := engine.ParseFlows(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}
