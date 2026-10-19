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

// TestADebugMessageLightsItsPathAndReplaysIntoADraft is the editor's trace view
// with the browser taken out, against the running application: a message goes
// through a deployed flow, the id the debug sidebar shows finds its trace, the
// trace becomes a test, and the same message goes into a draft in a child
// process and comes back with the one property the draft changed.
func TestADebugMessageLightsItsPathAndReplaysIntoADraft(t *testing.T) {
	app, rec := newApp(t)
	app.tracer = engine.NewTracer(engine.TraceLimits{})
	app.capture = runtime.NewCapture(runtime.DefaultCaptureSize, runtime.DefaultCaptureMaxBytes)
	app.tests = newTestSuite(app.cfg)
	app.tests.command = testBinary(t)
	e := serveAppWith(t, app, map[string][]string{"dana": {"flows.read", "flows.write"}},
		func(d *api.Deps) {
			d.Tests = app.tests
			d.Capture = app.capture
			d.Tracer = app.tracer
			d.Replay = app.tests.Replay
		})
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

	// The id the debug sidebar shows finds the trace, and the path to the node
	// that showed it.
	code, out := e.call(t, "GET", "/traces/find?msgId="+msgID+"&node=out", dana, nil)
	var found struct {
		Trace engine.TraceView `json:"trace"`
		Focus int              `json:"focus"`
		Path  []int            `json:"path"`
	}
	if code != http.StatusOK || json.Unmarshal(out, &found) != nil {
		t.Fatalf("finding the trace: %d %s", code, out)
	}
	var nodes []string
	for _, id := range found.Path {
		nodes = append(nodes, found.Trace.Hops[id].Node)
	}
	if strings.Join(nodes, ",") != "reading,label,out" {
		t.Fatalf("the path is %v", nodes)
	}
	last := found.Trace.Hops[found.Focus]
	if last.Node != "out" || last.MsgID != msgID || !last.Done {
		t.Fatalf("the focus hop is %+v", last)
	}
	if m, _ := last.Msg.(map[string]any); m["payload"] != "LINE 3" {
		t.Fatalf("the Debug node's hop kept %v", last.Msg)
	}

	// The same trace by its id, and in the list.
	if code, _ := e.call(t, "GET", "/traces/"+found.Trace.ID, dana, nil); code != http.StatusOK {
		t.Fatalf("GET /traces/{id}: %d", code)
	}
	code, out = e.call(t, "GET", "/traces", dana, nil)
	if code != http.StatusOK || !strings.Contains(string(out), found.Trace.ID) {
		t.Fatalf("GET /traces: %d %s", code, out)
	}
	if code, _ := e.call(t, "GET", "/traces/ffffffffffffffffffffffffffffffff", dana, nil); code != http.StatusNotFound {
		t.Fatalf("a trace that was never kept: %d", code)
	}

	// The trace becomes a test.
	code, out = e.call(t, "POST", "/traces/"+found.Trace.ID+"/test", dana, nil)
	var draft struct{ Name, YAML string }
	if code != http.StatusOK || json.Unmarshal(out, &draft) != nil {
		t.Fatalf("a test from the trace: %d %s", code, out)
	}
	if !strings.Contains(draft.YAML, "node: reading") || !strings.Contains(draft.YAML, "payload: LINE 3") {
		t.Fatalf("the test doesn't put the reading in and expect the label:\n%s", draft.YAML)
	}

	// Replayed into what's running it comes out the same; into a draft that
	// changes the label it says which property differs.
	replay := func(doc string) flowtest.ReplayResult {
		t.Helper()
		var body []byte
		if doc != "" {
			body, _ = json.Marshal(map[string]any{"flows": json.RawMessage(doc)})
		} else {
			body = []byte(`{}`)
		}
		code, out := e.call(t, "POST", "/traces/"+found.Trace.ID+"/replay", dana, body)
		var res flowtest.ReplayResult
		if code != http.StatusOK || json.Unmarshal(out, &res) != nil {
			t.Fatalf("replay: %d %s", code, out)
		}
		return res
	}
	if res := replay(""); !res.Same || res.Trace == nil || len(res.Trace.Hops) != len(found.Trace.Hops) {
		t.Fatalf("a replay into the running flow: %+v", res)
	}
	res := replay(pressedFlow("LINE 9"))
	if res.Same || len(res.Differences) != 1 || res.Differences[0].Node != "out" ||
		!strings.Contains(strings.Join(res.Differences[0].Changes, "\n"), `"LINE 3" -> "LINE 9"`) {
		t.Fatalf("a replay into a draft: %+v", res)
	}
	if res.Trace == nil || res.Trace.Hops[len(res.Trace.Hops)-1].Node != "out" {
		t.Fatalf("the replay's own trace: %+v", res.Trace)
	}
}

// TestTracingTurnedOffAnswersNotFound: no tracer, no routes that pretend.
func TestTracingTurnedOffAnswersNotFound(t *testing.T) {
	app, _ := newApp(t)
	e := serveAppWith(t, app, map[string][]string{"dana": {"flows.read"}}, func(d *api.Deps) {})
	dana := e.login(t, "dana")
	if code, _ := e.call(t, "GET", "/traces", dana, nil); code != http.StatusNotFound {
		t.Fatalf("GET /traces with tracing off: %d", code)
	}
}
