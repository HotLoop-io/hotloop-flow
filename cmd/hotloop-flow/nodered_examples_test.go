package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	var oracle struct {
		Outputs map[string][]string `json:"outputs"`
		// Gaps are example flows Flow does not yet give Node-RED's output for. Each is
		// still run and compared: one that now matches fails the test until it is taken
		// off this list, so the list only shrinks.
		Gaps []string `json:"gaps"`
	}
	raw, err := os.ReadFile("../../testdata/nodered/oracle.json")
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
		want := oracle.Outputs[name]
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("../../testdata/nodered/examples", name))
			if err != nil {
				t.Fatal(err)
			}
			flows, err := engine.ParseFlows(withTabForLooseNodes(src))
			if err != nil {
				t.Fatal(err)
			}
			app, rec := newApp(t)
			if _, err := app.deploy(context.Background(), api.DeployRequest{Flows: flows}); err != nil {
				t.Fatal(err)
			}
			for _, id := range flows.Order {
				if n := flows.Nodes[id]; n != nil && n.Type == "inject" {
					if err := app.currentRuntime().Inject(id, engine.NewMsg()); err != nil {
						t.Fatal(err)
					}
				}
			}
			time.Sleep(1500 * time.Millisecond)
			var got []string
			rec.mu.Lock()
			for _, e := range rec.events {
				if e.Topic == runtime.TopicDebug {
					got = append(got, canonicalDebug(e.Data["msg"]))
				}
			}
			rec.mu.Unlock()
			sort.Strings(got)
			differs := len(got) != len(want)
			for i := 0; !differs && i < len(want); i++ {
				differs = got[i] != want[i]
			}
			switch {
			case gap[name] && !differs:
				t.Errorf("this flow now gives what Node-RED gives: take it off the gaps list in oracle.json")
			case gap[name]:
				t.Logf("known gap: %d outputs, Node-RED gave %d", len(got), len(want))
			case differs:
				t.Errorf("Flow gave %v\n Node-RED gave %v", got, want)
			}
		})
	}
}

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
