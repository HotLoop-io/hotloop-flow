package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/api"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// gatedFlow is line 3 with a label the tests care about. Version decides what
// the label says. A Link Out on the tab feeds a Link In, which matters: the
// candidate a gate tests has the same Link In, by the same id, and a test run
// in this process would take that id over from the running one.
func gatedFlow(label string) string {
	return `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"lo","type":"link out","z":"t1","name":"send","links":["li"],"x":1,"y":1,"wires":[]},
  {"id":"li","type":"link in","z":"t1","name":"feed","x":2,"y":1,"wires":[["label"]]},
  {"id":"label","type":"change","z":"t1","name":"label","rules":[{"t":"set","p":"payload","pt":"msg","to":"` + label + `","tot":"str"}],"x":3,"y":1,"wires":[["out"]]},
  {"id":"out","type":"debug","z":"t1","name":"out","x":4,"y":1,"wires":[]}
]`
}

const gateSuite = `
tests:
  - name: the label says LINE 3
    inject: [{node: send, msg: {payload: x}}]
    expect:
      - {node: out, msg: {payload: LINE 3}}
`

// gatedApp is an application with the gate on, running its tests in this test
// binary acting as hotloop-flow, the way the real one runs itself.
func gatedApp(t *testing.T) (*application, *recorder) {
	t.Helper()
	app, rec := newApp(t)
	app.cfg.Tests.Gate = true
	app.tests = newTestSuite(app.cfg)
	app.tests.command = testBinary(t)
	if err := app.tests.Save([]byte(gateSuite)); err != nil {
		t.Fatal(err)
	}
	return app, rec
}

func testBinary(t *testing.T) func() (string, []string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return func() (string, []string, error) {
		return exe, append(os.Environ(), "HOTLOOP_FLOW_TEST_RUN_AS_MAIN=1"), nil
	}
}

func deployDoc(t *testing.T, app *application, doc string) (api.DeployResult, error) {
	t.Helper()
	flows, err := engine.ParseFlows([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return app.deploy(context.Background(), api.DeployRequest{Flows: flows, ExpectedRev: app.flowStore.Rev(), User: "dana"})
}

// TestTheGateRefusesAFailingDeployAndNothingNotices is the roadmap's own test.
// A deploy that fails its tests is refused with the failing assertion, and the
// line that's running carries on as if nobody had tried: the same runtime, the
// same flow file and credentials, no record in the history, and a message
// through its links still arrives.
func TestTheGateRefusesAFailingDeployAndNothingNotices(t *testing.T) {
	app, rec := gatedApp(t)

	// What's running passes its tests, so it deploys through the gate.
	if _, err := deployDoc(t, app, gatedFlow("LINE 3")); err != nil {
		t.Fatalf("a passing deploy was refused: %v", err)
	}
	running := app.currentRuntime()
	file := append([]byte(nil), app.flowStore.Bytes()...)
	rev := app.flowStore.Rev()
	latest, _ := app.history.Latest()

	// The change breaks the label, and carries a credential, which must not
	// land either.
	bad := strings.Replace(gatedFlow("LINE 4"), `"name":"send",`, `"name":"send","credentials":{"secret":"never-saved"},`, 1)
	_, err := deployDoc(t, app, bad)
	var failed *api.TestsFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("the failing deploy came back %v, want it refused by its tests", err)
	}
	if failed.Report.Failed != 1 || !strings.Contains(err.Error(), `"the label says LINE 3"`) ||
		!strings.Contains(err.Error(), `msg.payload: wanted "LINE 3", got "LINE 4"`) {
		t.Errorf("the refusal doesn't say which assertion failed: %v", err)
	}

	if app.currentRuntime() != running {
		t.Error("the runtime was replaced by a refused deploy")
	}
	if string(app.flowStore.Bytes()) != string(file) || app.flowStore.Rev() != rev {
		t.Error("the flow file changed under a refused deploy")
	}
	if got, _ := app.history.Latest(); got.Seq != latest.Seq {
		t.Errorf("a refused deploy was recorded as deployment %d", got.Seq)
	}
	if len(app.creds.Get("lo")) != 0 {
		t.Error("a refused deploy's credentials were kept")
	}

	// The line still runs, through the very link the candidate had too.
	rec.reset()
	if err := running.Inject("lo", engine.NewMsgWithPayload("x")); err != nil {
		t.Fatal(err)
	}
	if got := rec.debugFrom(t, "out"); got != "LINE 3" {
		t.Fatalf("the running flow said %q after a refused deploy, want LINE 3", got)
	}

	// And a change that keeps the tests passing goes straight through.
	if _, err := deployDoc(t, app, strings.Replace(gatedFlow("LINE 3"), `"x":4,"y":1`, `"x":40,"y":10`, 1)); err != nil {
		t.Fatalf("a passing change was refused: %v", err)
	}
}

func TestTheGateOnlyGatesWhenAskedAndRefusesWhatItCantRun(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		app, _ := gatedApp(t)
		app.tests.gate = false
		if _, err := deployDoc(t, app, gatedFlow("LINE 4")); err != nil {
			t.Fatalf("with the gate off a failing deploy was refused: %v", err)
		}
	})
	t.Run("on with no tests", func(t *testing.T) {
		app, _ := gatedApp(t)
		if err := os.Remove(app.cfg.TestsPath()); err != nil {
			t.Fatal(err)
		}
		if _, err := deployDoc(t, app, gatedFlow("LINE 4")); err != nil {
			t.Fatalf("with nothing to fail, the deploy was refused: %v", err)
		}
	})
	t.Run("tests that can't run", func(t *testing.T) {
		app, _ := gatedApp(t)
		app.tests.command = func() (string, []string, error) { return "/nonexistent/hotloop-flow", nil, nil }
		_, err := deployDoc(t, app, gatedFlow("LINE 3"))
		if err == nil || !strings.Contains(err.Error(), "the flow tests couldn't be run") {
			t.Fatalf("a gate that couldn't run the tests let the deploy through: %v", err)
		}
		if app.currentRuntime() != nil {
			t.Error("something started")
		}
	})
	t.Run("tests that hang", func(t *testing.T) {
		app, _ := gatedApp(t)
		app.tests.timeout = 200 * time.Millisecond
		hang := writeFile(t, t.TempDir(), "hang.sh", "#!/bin/sh\nsleep 30\n")
		if err := os.Chmod(hang, 0o700); err != nil {
			t.Fatal(err)
		}
		app.tests.command = func() (string, []string, error) { return hang, nil, nil }
		began := time.Now()
		_, err := deployDoc(t, app, gatedFlow("LINE 3"))
		if err == nil || !strings.Contains(err.Error(), "took longer than tests.timeout") {
			t.Fatalf("a hung test run: %v", err)
		}
		if waited := time.Since(began); waited > 10*time.Second {
			t.Errorf("the deploy waited %s on a hung test run", waited)
		}
	})
	t.Run("rollback is not gated", func(t *testing.T) {
		// A rollback is how somebody gets out of a bad state, to a state that
		// already ran. Holding it up on today's tests is how an outage gets
		// longer.
		app, _ := gatedApp(t)
		app.tests.gate = false
		if _, err := deployDoc(t, app, gatedFlow("LINE 4")); err != nil {
			t.Fatal(err)
		}
		first, _ := app.history.Latest()
		if _, err := deployDoc(t, app, gatedFlow("LINE 5")); err != nil {
			t.Fatal(err)
		}
		app.tests.gate = true
		_, err := app.rollback(context.Background(), api.RollbackRequest{Deployment: first.Seq, ExpectedRev: app.flowStore.Rev()})
		if err != nil {
			t.Fatalf("a rollback was refused: %v", err)
		}
	})
}

// TestTheTestsOverTheAPI: the suite is kept, read and run over the API, and a
// gated deploy over HTTP is a 422 carrying the report.
func TestTheTestsOverTheAPI(t *testing.T) {
	app, _ := gatedApp(t)
	e := serveAppWith(t, app, map[string][]string{
		"dana":   {"flows.read", "flows.write"},
		"viewer": {"flows.read"},
	}, func(d *api.Deps) { d.Tests = app.tests })
	dana, viewer := e.login(t, "dana"), e.login(t, "viewer")

	code, out := e.call(t, "PUT", "/tests", dana, []byte("tests:\n  - name: x\n    expcet: []\n"))
	if code != http.StatusBadRequest || !strings.Contains(string(out), "expcet") {
		t.Fatalf("a suite that doesn't parse: %d %s", code, out)
	}
	if code, _ := e.call(t, "PUT", "/tests", viewer, []byte(gateSuite)); code != http.StatusForbidden {
		t.Fatalf("flows.read saved the tests: %d", code)
	}
	if code, out := e.call(t, "PUT", "/tests", dana, []byte(gateSuite)); code != http.StatusOK {
		t.Fatalf("saving the tests: %d %s", code, out)
	}
	code, out = e.call(t, "GET", "/tests", viewer, nil)
	var got struct {
		Tests string `json:"tests"`
		File  string `json:"file"`
		Gate  bool   `json:"gate"`
	}
	if code != http.StatusOK || json.Unmarshal(out, &got) != nil || got.Tests != gateSuite || got.File != "flows.test.yaml" || !got.Gate {
		t.Fatalf("GET /tests: %d %s", code, out)
	}

	// Run against a working copy nobody deployed.
	body, _ := json.Marshal(map[string]any{"flows": json.RawMessage(gatedFlow("LINE 4"))})
	code, out = e.call(t, "POST", "/tests/run", viewer, body)
	var rep flowtest.Report
	if code != http.StatusOK || json.Unmarshal(out, &rep) != nil || rep.Failed != 1 || rep.Tests[0].Problems[0].Node != "out" {
		t.Fatalf("POST /tests/run: %d %s", code, out)
	}

	// A gated deploy over HTTP.
	code, out = e.call(t, "POST", "/flows", dana, []byte(gatedFlow("LINE 4")))
	var refused struct {
		Error string          `json:"error"`
		Tests flowtest.Report `json:"tests"`
	}
	if code != http.StatusUnprocessableEntity || json.Unmarshal(out, &refused) != nil ||
		refused.Tests.Failed != 1 || !strings.Contains(refused.Error, "the flows already running are untouched") {
		t.Fatalf("a gated deploy over HTTP: %d %s", code, out)
	}
	trail, _ := os.ReadFile(app.cfg.AuditPath())
	for _, want := range []string{`"tests.saved"`, `"flow tests failed"`} {
		if !strings.Contains(string(trail), want) {
			t.Errorf("the audit trail has no %s:\n%s", want, trail)
		}
	}
}

// TestTheDeployCommandPrintsTheRefusal: a pipeline deploying a change that
// fails its tests gets a red job with the report in its log.
func TestTheDeployCommandPrintsTheRefusal(t *testing.T) {
	err := (&apiError{status: http.StatusUnprocessableEntity, msg: "the deploy was refused", tests: &flowtest.Report{
		File: "flows.test.yaml", Failed: 1,
		Tests: []flowtest.Result{{Name: "the label says LINE 3", Status: flowtest.Fail,
			Problems: []flowtest.Problem{{Expect: 1, Message: `msg.payload: wanted "LINE 3", got "LINE 4"`}}}},
	}})
	for _, want := range []string{"the deploy was refused", "--- FAIL: the label says LINE 3", `expect 1: msg.payload: wanted "LINE 3", got "LINE 4"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("wanted %q in:\n%s", want, err.Error())
		}
	}
}
