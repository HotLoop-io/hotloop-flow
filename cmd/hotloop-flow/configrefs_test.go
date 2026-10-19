package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
)

// A flow was deleted and took its flow-scoped MQTT broker with it. The subflow
// that used the broker is still there, and so is a plain node on another tab
// that pointed at a second one. Both are in a hand-edited file as ids that name
// nothing.
//
// mqtt out is the one that matters most: it only looks its broker up when a
// message arrives, so before this the deploy said nothing and the node sat
// there doing nothing.
const danglingFlow = `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"sf","type":"subflow","name":"Publish","in":[{"x":40,"y":40,"wires":[{"id":"m1"}]}],"out":[]},
  {"id":"m1","type":"mqtt out","z":"sf","name":"to the broker","broker":"deleted-broker","topic":"line3/out","wires":[]},
  {"id":"i1","type":"subflow:sf","z":"t1","name":"publisher","wires":[]},
  {"id":"m2","type":"mqtt in","z":"t1","broker":"also-gone","topic":"line3/in","wires":[["d1"]]},
  {"id":"d1","type":"debug","z":"t1","name":"alive","complete":"payload","wires":[]}
]`

func TestADeployWithADanglingConfigReferenceSaysSoAndKeepsRunning(t *testing.T) {
	app, rec := newApp(t)

	res, err := app.deploy(context.Background(), api.DeployRequest{Flows: parse(t, danglingFlow)})
	if err != nil {
		t.Fatalf("a dangling config reference refused the whole deploy: %v", err)
	}

	// Each node that points at nothing is a failure on that node, in words that
	// say what is missing, including the copy of the subflow's node that the
	// instance runs.
	failed := map[string]string{}
	for _, f := range res.Failures {
		failed[f.NodeID] = f.Err.Error()
	}
	var sawSubflowCopy bool
	for id, msg := range failed {
		if strings.Contains(msg, `"deleted-broker"`) {
			sawSubflowCopy = true
			if id == "m1" {
				t.Errorf("the template's own node ran: %s", msg)
			}
		}
	}
	if !sawSubflowCopy {
		t.Errorf("the subflow's mqtt out with a deleted broker was not reported: %v", failed)
	}
	for _, id := range []string{"m2"} {
		if msg := failed[id]; !strings.Contains(msg, `no node with that id is in the file`) || !strings.Contains(msg, `"also-gone"`) {
			t.Errorf("%s: %q does not say the config node it points at does not exist", id, msg)
		}
	}

	// The same two are in the warnings, so an API caller that reads only those
	// sees them too, one line per node in the file, however many instances.
	var lines []string
	for _, w := range res.Warnings {
		if strings.Contains(w, "no node with that id is in the file") {
			lines = append(lines, w)
		}
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "m1") || !strings.Contains(lines[0], `"deleted-broker"`) || !strings.Contains(lines[1], "m2") {
		t.Errorf("warnings = %q", res.Warnings)
	}

	// And the runtime is up: the rest of the flow still takes a message.
	rec.reset()
	injectPayload(t, app, "d1", "still here")
	if got := rec.debugFrom(t, "d1"); !strings.Contains(got, "still here") {
		t.Errorf("the debug node on the same tab showed %q", got)
	}
}

func TestTheEditorIsToldAboutADanglingConfigReferenceWhenItLoads(t *testing.T) {
	app, _ := newApp(t)
	if _, err := app.deploy(context.Background(), api.DeployRequest{Flows: parse(t, danglingFlow)}); err != nil {
		t.Fatal(err)
	}
	e := serveApp(t, app, map[string][]string{"admin": {"*"}})
	code, out := e.call(t, "GET", "/flows", e.login(t, "admin"), nil)
	if code != http.StatusOK {
		t.Fatalf("GET /flows: %d %s", code, out)
	}
	var doc struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	var got int
	for _, w := range doc.Warnings {
		if strings.Contains(w, "no node with that id is in the file") {
			got++
		}
	}
	if got != 2 {
		t.Errorf("GET /flows warned %d times about config nodes, want 2: %q", got, doc.Warnings)
	}
}

func TestImportNamesTheNodesWhoseConfigNodeIsNotInTheFile(t *testing.T) {
	file := writeFile(t, t.TempDir(), "flows.json", danglingFlow)
	out := runImport(t, file)
	for _, want := range []string{
		"Config nodes that are not in the file or are the wrong kind, so these nodes will not start",
		`mqtt out m1 (to the broker)`, `"deleted-broker"`,
		`mqtt in m2`, `"also-gone"`,
		"2 node(s) point at config nodes that are not in the file or are the wrong kind",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the import report is missing %q:\n%s", want, out)
		}
	}
}

func TestImportOfAFlowWithNoDanglingReferenceStaysQuiet(t *testing.T) {
	file := writeFile(t, t.TempDir(), "flows.json", `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"b","type":"mqtt-broker","name":"plant","broker":"localhost","port":"1883"},
  {"id":"m","type":"mqtt in","z":"t1","broker":"b","topic":"x","wires":[[]]}
]`)
	out := runImport(t, file)
	if strings.Contains(out, "Config nodes that are not in the file") || strings.Contains(out, "wrong kind") {
		t.Errorf("a flow whose broker is in the file was reported:\n%s", out)
	}
}

// runImport runs the import report into a buffer. The command writes to the
// writer it is given, so nothing here touches standard output.
func runImport(t *testing.T, file string) string {
	t.Helper()
	var out bytes.Buffer
	if err := cmdImport([]string{file}, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// A config reference can also name something that is in the file and is not
// what the setting takes: a hand edit that pasted the wrong id. The report says
// so in the same list as the ones that name nothing.
func TestImportNamesTheNodesWhoseConfigReferenceIsTheWrongKind(t *testing.T) {
	file := writeFile(t, t.TempDir(), "flows.json", `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"b","type":"mqtt-broker","name":"plant","broker":"localhost","port":"1883"},
  {"id":"d","type":"debug","z":"t1","name":"alive","complete":"payload","wires":[]},
  {"id":"m1","type":"mqtt in","z":"t1","name":"on a debug","broker":"d","topic":"x","wires":[[]]},
  {"id":"m2","type":"mqtt in","z":"t1","name":"on a tab","broker":"t1","topic":"x","wires":[[]]},
  {"id":"m3","type":"mqtt in","z":"t1","name":"fine","broker":"b","topic":"x","wires":[[]]}
]`)
	out := runImport(t, file)
	for _, want := range []string{
		"mqtt in m1 (on a debug)", `"d", which is a debug and not a config node`,
		"mqtt in m2 (on a tab)", `"t1", which is a tab and not a config node`,
		"2 node(s) point at config nodes that are not in the file or are the wrong kind",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the import report is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mqtt in m3") {
		t.Errorf("the node with a good broker was reported:\n%s", out)
	}
}
