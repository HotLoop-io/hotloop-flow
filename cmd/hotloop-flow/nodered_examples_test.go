package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// Node-RED's own example flows, as the Node-RED project ships them (Apache-2.0,
// testdata/nodered/examples), imported the way a customer would and run. The
// expected output of each is what real Node-RED 5.0.10 printed for the same flow
// with the same Inject pressed (testdata/nodered/oracle.json). Only flows whose
// output does not depend on a clock are listed there; the rest are the known
// gaps in docs/node-red-types.md.
func TestNodeREDExampleFlowsGiveWhatNodeREDGives(t *testing.T) {
	holdFlowToNodeRED(t, "../../testdata/nodered/oracle.json", "../../testdata/nodered/examples", false)
}

// The same comparison for flows published in the Node-RED flow library (flows.nodered.org), which carries
// no license that lets this repository keep a copy: they are fetched, run through
// testdata/nodered/oracle/oracle.js and kept outside the repository. HOTLOOP_NODERED_LIBRARY is the folder
// holding the flow files, with oracle.json beside them.
func TestPublishedLibraryFlowsGiveWhatNodeREDGives(t *testing.T) {
	dir := os.Getenv("HOTLOOP_NODERED_LIBRARY")
	if dir == "" {
		t.Skip("HOTLOOP_NODERED_LIBRARY is not set")
	}
	holdFlowToNodeRED(t, filepath.Join(dir, "oracle.json"), dir, true)
}

// whole says the Debug nodes show what they are given whole. A Debug node cuts a long message at its Length
// setting (1000) in what the sidebar shows, and Flow cuts the message's text where Node-RED cuts each string
// and each long array inside it, so a long message shows differently in the two sidebars, which is a
// matter of the display and not of what the flow does. The library flows run with the cut off.
func holdFlowToNodeRED(t *testing.T, oracleFile, examplesDir string, whole bool) {
	var oracle struct {
		Outputs map[string][]string `json:"outputs"`
		// Gaps are example flows Flow does not yet give Node-RED's output for. Each is
		// still run and compared, and the list is what is left to fix.
		Gaps []string `json:"gaps"`
	}
	raw, err := os.ReadFile(oracleFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	gap := map[string]bool{}
	for _, g := range oracle.Gaps {
		gap[g] = true
		if _, ok := oracle.Outputs[g]; !ok {
			t.Fatalf("%q is listed as a gap and has no recorded output", g)
		}
	}
	if len(oracle.Outputs) < 20 {
		t.Fatalf("only %d recorded outputs", len(oracle.Outputs))
	}
	names := make([]string, 0, len(oracle.Outputs))
	for n := range oracle.Outputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		want := make([]string, 0, len(oracle.Outputs[name])+1)
		for _, w := range oracle.Outputs[name] {
			// Flow words its own errors (a schema's violations, a parser's position), so
			// of an error line only that it appeared is compared. A line that is a message
			// is compared as the Debug node's canonical form, which leaves out what is the
			// engine's own (see canonicalDebug).
			if strings.HasPrefix(w, errorLine+" ") {
				w = errorLine
			} else {
				w = canonicalDebug(w)
			}
			want = append(want, w)
		}
		if _, ok := deliberate[name]; ok {
			want = append(want, errorLine)
		}
		sort.Strings(want)
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(examplesDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if whole {
				src = debugWithoutCut(src)
			}
			flows, err := engine.ParseFlows(withTabForLooseNodes(src))
			if err != nil {
				t.Fatal(err)
			}
			app, rec := newApp(t)
			res, err := app.deploy(context.Background(), api.DeployRequest{Flows: flows})
			if err != nil {
				t.Fatal(err)
			}
			// A node that cannot be built (a Function with a syntax error) is a line Node-RED
			// logs, which its debug sidebar shows and the oracle records as "[error]". Flow tells
			// the editor which node is broken, which is the same report.
			failures := len(res.Failures)
			for _, id := range flows.Order {
				if n := flows.Nodes[id]; n != nil && n.Type == "inject" {
					if err := app.currentRuntime().Inject(id, engine.NewMsg()); err != nil {
						t.Fatal(err)
					}
					// Node-RED presses them one after another; so do we, or two flows that share a
					// node's state see the presses in whatever order the scheduler picks.
					time.Sleep(60 * time.Millisecond)
				}
			}
			// What the flow has printed so far, in the oracle's form.
			collect := func() []string {
				var out []string
				rec.mu.Lock()
				for _, e := range rec.events {
					switch {
					case e.Topic == runtime.TopicDebug:
						out = append(out, canonicalDebug(e.Data["msg"]))
					case e.Topic == runtime.TopicError && e.Data["handled"] == false:
						// A node's error that no Catch node took is a line in Node-RED's debug
						// sidebar, and the oracle records it as "[error]".
						out = append(out, errorLine)
					}
				}
				rec.mu.Unlock()
				for i := 0; i < failures; i++ {
					out = append(out, errorLine)
				}
				sort.Strings(out)
				return out
			}
			same := func(a, b []string) bool {
				if len(a) != len(b) {
					return false
				}
				for i := range a {
					if a[i] != b[i] {
						return false
					}
				}
				return true
			}
			// Node-RED was read 1.5 s after the last Inject. Waiting a fixed 1.5 s here makes
			// a flow whose answer comes a second in (a Trigger, a Delay) pass or fail with the
			// runner's load, so the wait is for the recorded output to appear, up to 10 s, and
			// then for a quiet 400 ms, and for 1.5 s in all, that nothing more comes. A flow that gives more than the
			// oracle did is seen at once, and one that never gives it runs out the 10 s.
			var got []string
			start := time.Now()
			deadline := start.Add(10 * time.Second)
			for {
				got = collect()
				if len(got) > len(want) || time.Now().After(deadline) {
					break
				}
				if same(got, want) {
					// Never less than the 1.5 s Node-RED was given, so a flow that should
					// print nothing more is still watched for that long.
					time.Sleep(max(400*time.Millisecond, time.Until(start.Add(1500*time.Millisecond))))
					got = collect()
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			differs := !same(got, want)
			switch {
			case gap[name] && !differs:
				// Several gaps are timing flows that match or miss by a few milliseconds on a
				// loaded runner, so a match is said, not enforced.
				t.Logf("%s: this flow gave what Node-RED gives this time: if it does every time, take it off the gaps list in oracle.json", name)
			case gap[name]:
				t.Logf("known gap %s: %d outputs, Node-RED gave %d\n Flow gave %v\n Node-RED gave %v", name, len(got), len(want), got, want)
			case differs:
				t.Errorf("Flow gave %v\n Node-RED gave %v", got, want)
			}
		})
	}
}

// deliberate lists the example flows where Flow differs from Node-RED on purpose, and says why.
// Each one is still compared, with the one difference expected, so a change in it is noticed.
var deliberate = map[string]string{
	"common__link__02 - Link across tabs.json": "its Link Out names a Link In that is not in the file; " +
		"Node-RED drops the message without a word and Flow raises an error a Catch node can see " +
		"(docs/compatibility.md, link out)",
}

// errorLine stands for an error no Catch node took, as the oracle records it.
const errorLine = "[error]"

var (
	msgIDKey = regexp.MustCompile(`"_msgid":\s*"[0-9a-f]+",?`)
	spaces   = regexp.MustCompile(`\s+`)
)

// canonicalDebug is a Debug line with the message id dropped, whitespace removed
// and object keys sorted, so two runtimes that order keys differently compare equal.
func canonicalDebug(v any) string {
	s, _ := v.(string)
	s = spaces.ReplaceAllString(msgIDKey.ReplaceAllString(s, ""), "")
	var j any
	if json.Unmarshal([]byte(s), &j) == nil {
		// What a Catch node's message carries that is the engine's own and not the flow's:
		// Node-RED keeps the Error object itself in msg._error and a stack that names its
		// own source files; Flow keeps a hop count in msg._errorDepth to break error loops.
		// None of them is something a flow reads, so none is compared.
		if m, ok := j.(map[string]any); ok {
			delete(m, "_error")
			delete(m, "_errorDepth")
			if e, ok := m["error"].(map[string]any); ok {
				delete(e, "stack")
			}
		}
		if b, err := json.Marshal(j); err == nil {
			return string(b)
		}
	}
	return s
}

// withTabForLooseNodes does what the editor's Import does with nodes whose flow
// is not in the file: they go on a flow of their own.
func withTabForLooseNodes(raw []byte) []byte {
	var d []map[string]any
	if json.Unmarshal(raw, &d) != nil {
		return raw
	}
	tabs := map[string]bool{}
	for _, n := range d {
		if n["type"] == "tab" || n["type"] == "subflow" {
			tabs[n["id"].(string)] = true
		}
	}
	loose := false
	for _, n := range d {
		if z, ok := n["z"].(string); ok && !tabs[z] {
			n["z"], loose = "imported-tab", true
		}
	}
	if loose {
		d = append([]map[string]any{{"id": "imported-tab", "type": "tab", "label": "Imported"}}, d...)
	}
	out, _ := json.Marshal(d)
	return out
}

// debugWithoutCut sets every Debug node's Length to 0, which shows a message whole.
func debugWithoutCut(raw []byte) []byte {
	var d []map[string]any
	if json.Unmarshal(raw, &d) != nil {
		return raw
	}
	for _, n := range d {
		if n["type"] == "debug" {
			n["maxLength"] = 0
		}
	}
	out, _ := json.Marshal(d)
	return out
}
