package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// TestADebugMessageBecomesATestOverTheAPI is the editor's two clicks with the
// browser taken out: a message goes through the running flow, the debug
// sidebar shows it, the draft made from it is added to the suite, and the
// suite run against the working copy passes, then fails on the Debug node once
// the working copy changes.
// pressedFlow is a reading from an Inject button, labelled and shown.
func pressedFlow(label string) string {
	return `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"reading","type":"inject","z":"t1","name":"reading","props":[{"p":"payload","v":"50","vt":"num"}],"x":1,"y":1,"wires":[["label"]]},
  {"id":"label","type":"change","z":"t1","name":"label","rules":[{"t":"set","p":"payload","pt":"msg","to":"` + label + `","tot":"str"}],"x":3,"y":1,"wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","x":4,"y":1,"wires":[]}
]`
}

func TestADebugMessageBecomesATestOverTheAPI(t *testing.T) {
	app, rec := newApp(t)
	app.capture = runtime.NewCapture(runtime.DefaultCaptureSize, runtime.DefaultCaptureMaxBytes)
	app.tests = newTestSuite(app.cfg)
	app.tests.command = testBinary(t)
	e := serveAppWith(t, app, map[string][]string{"dana": {"flows.read", "flows.write"}},
		func(d *api.Deps) { d.Tests = app.tests; d.Capture = app.capture })
	dana := e.login(t, "dana")

	if _, err := deployDoc(t, app, pressedFlow("LINE 3")); err != nil {
		t.Fatal(err)
	}
	rec.reset()
	if err := app.currentRuntime().Inject("reading", engine.NewMsg()); err != nil {
		t.Fatal(err)
	}
	rec.debugFrom(t, "out")
	msgID := ""
	deadline := time.Now().Add(5 * time.Second)
	for msgID == "" && time.Now().Before(deadline) {
		rec.mu.Lock()
		for _, ev := range rec.events {
			if ev.Topic == runtime.TopicDebug && ev.Data["id"] == "out" {
				msgID, _ = ev.Data["msgId"].(string)
			}
		}
		rec.mu.Unlock()
	}

	body, _ := json.Marshal(map[string]string{"node": "out", "msgId": msgID})
	code, out := e.call(t, "POST", "/tests/from-debug", dana, body)
	var draft struct {
		Name string `json:"name"`
		YAML string `json:"yaml"`
	}
	if code != http.StatusOK || json.Unmarshal(out, &draft) != nil {
		t.Fatalf("drafting a test: %d %s", code, out)
	}
	if !strings.Contains(draft.YAML, "node: reading") || !strings.Contains(draft.YAML, "payload: 50") || !strings.Contains(draft.YAML, "payload: LINE 3") {
		t.Fatalf("the draft doesn't put the reading in at the Inject and expect the label:\n%s", draft.YAML)
	}

	if code, out := e.call(t, "POST", "/tests/cases", dana, []byte(draft.YAML)); code != http.StatusOK {
		t.Fatalf("adding the draft: %d %s", code, out)
	}
	code, out = e.call(t, "POST", "/tests/cases", dana, []byte(draft.YAML))
	var added struct {
		Added []string `json:"added"`
	}
	if code != http.StatusOK || json.Unmarshal(out, &added) != nil || len(added.Added) != 1 || added.Added[0] != draft.Name+" (2)" {
		t.Fatalf("adding it twice should number the second: %d %s", code, out)
	}
	code, out = e.call(t, "GET", "/tests", dana, nil)
	if code != http.StatusOK || !strings.Contains(string(out), `"count":2`) {
		t.Fatalf("GET /tests after adding: %d %s", code, out)
	}

	run := func(doc string) flowtest.Report {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"flows": json.RawMessage(doc)})
		code, out := e.call(t, "POST", "/tests/run", dana, body)
		var rep flowtest.Report
		if code != http.StatusOK || json.Unmarshal(out, &rep) != nil {
			t.Fatalf("POST /tests/run: %d %s", code, out)
		}
		return rep
	}
	if rep := run(pressedFlow("LINE 3")); rep.Passed != 2 || !contains(rep.Tests[0].Nodes, "out") {
		t.Fatalf("the drafted tests against the flow they came from: %+v", rep)
	}
	rep := run(pressedFlow("LINE 9"))
	if rep.Failed != 2 || rep.Tests[0].Problems[0].Node != "out" {
		t.Fatalf("the drafted tests against a changed flow: %+v", rep)
	}

	// A message that was never kept says so.
	body, _ = json.Marshal(map[string]string{"node": "out", "msgId": "not-a-message"})
	if code, out := e.call(t, "POST", "/tests/from-debug", dana, body); code != http.StatusNotFound {
		t.Fatalf("a message that isn't kept: %d %s", code, out)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
