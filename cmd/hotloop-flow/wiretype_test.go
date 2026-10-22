package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/wiretype"
)

// typedFlow has a sensor whose output promises payload.temp is text, wired to a
// limit check whose input wants a number. Count says what the sensor sends.
func typedFlow(sends string) string {
	return `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"sensor","type":"junction","z":"t1","name":"sensor","x":1,"y":1,"wires":[["limit"]],
   "ew_outSchemas":[{"type":"object","properties":{"payload":{"type":"object","properties":{"temp":{"type":"` + sends + `"}}}}}]},
  {"id":"limit","type":"junction","z":"t1","name":"limit","x":2,"y":1,"wires":[[]],
   "ew_inSchema":{"type":"object","properties":{"payload":{"type":"object","properties":{"temp":{"type":"number"}}}}}}
]`
}

func TestADeployWithAVisibleMismatchIsRefusedAndNothingNotices(t *testing.T) {
	app, _ := newApp(t)

	if _, err := deployDoc(t, app, typedFlow("number")); err != nil {
		t.Fatalf("a flow whose wires agree was refused: %v", err)
	}
	running := app.currentRuntime()
	file := append([]byte(nil), app.flowStore.Bytes()...)
	rev := app.flowStore.Rev()
	latest, _ := app.history.Latest()

	_, err := deployDoc(t, app, typedFlow("string"))
	var refused *wiretype.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("came back %v, want a refusal for the typed wire", err)
	}
	t.Log(err)
	want := `junction "sensor" (sensor) output 1 -> "limit" (limit): payload.temp: the sender sends string and the receiver takes number`
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "the flows already running are untouched") {
		t.Errorf("the refusal reads:\n%s", err)
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
}

func TestTheTypedWireRefusalOverHTTPAndInTheAuditTrail(t *testing.T) {
	app, _ := newApp(t)
	e := serveAppWith(t, app, map[string][]string{"dana": {"flows.read", "flows.write"}}, nil)
	dana := e.login(t, "dana")

	code, out := e.call(t, "POST", "/flows", dana, []byte(typedFlow("string")))
	var body struct {
		Error string `json:"error"`
		Wires []struct {
			NodeID  string `json:"nodeId"`
			To      string `json:"to"`
			Port    int    `json:"port"`
			Message string `json:"message"`
		} `json:"wires"`
	}
	if code != http.StatusUnprocessableEntity || json.Unmarshal(out, &body) != nil || len(body.Wires) != 1 {
		t.Fatalf("POST /flows: %d %s", code, out)
	}
	w := body.Wires[0]
	if w.NodeID != "sensor" || w.To != "limit" || w.Port != 1 || !strings.Contains(w.Message, "payload.temp") {
		t.Fatalf("wires: %+v", w)
	}
	trail, _ := os.ReadFile(app.cfg.AuditPath())
	if !strings.Contains(string(trail), `"typed wires"`) {
		t.Errorf("the audit trail has no typed wires refusal:\n%s", trail)
	}

	// The same flow with agreeing ends goes in.
	if code, out := e.call(t, "POST", "/flows", dana, []byte(typedFlow("number"))); code != http.StatusOK {
		t.Fatalf("an agreeing flow: %d %s", code, out)
	}
}

// The editor asks the runtime which wires can never agree, on a working copy,
// with read permission and without deploying anything.
func TestTheEditorCanAskWhichWiresClash(t *testing.T) {
	app, _ := newApp(t)
	e := serveAppWith(t, app, map[string][]string{"viewer": {"flows.read"}}, nil)
	viewer := e.login(t, "viewer")

	code, out := e.call(t, "POST", "/flows/types", viewer, []byte(typedFlow("string")))
	var body struct {
		Wires []struct {
			NodeID  string `json:"nodeId"`
			To      string `json:"to"`
			Port    int    `json:"port"`
			Message string `json:"message"`
		} `json:"wires"`
	}
	if code != http.StatusOK || json.Unmarshal(out, &body) != nil || len(body.Wires) != 1 ||
		body.Wires[0].NodeID != "sensor" || body.Wires[0].To != "limit" || body.Wires[0].Port != 1 {
		t.Fatalf("POST /flows/types: %d %s", code, out)
	}

	code, out = e.call(t, "POST", "/flows/types", viewer, []byte(typedFlow("number")))
	if code != http.StatusOK || !strings.Contains(string(out), `"wires":[]`) {
		t.Fatalf("agreeing flow: %d %s", code, out)
	}
	if code, _ := e.call(t, "POST", "/flows/types", viewer, []byte(`{`)); code != http.StatusBadRequest {
		t.Fatalf("garbage: %d", code)
	}
	if got := app.flowStore.Bytes(); strings.Contains(string(got), "sensor") {
		t.Fatal("asking deployed the flow")
	}
}

// injectFlow is an Inject node that sets payload from a literal, wired to a limit
// check whose input schema takes a payload between 0 and 100.
func injectFlow(v, vt string) string {
	return `[
  {"id":"t1","type":"tab","label":"Line 3"},
  {"id":"reading","type":"inject","z":"t1","name":"reading","props":[{"p":"payload","v":"` + v + `","vt":"` + vt + `"}],"x":1,"y":1,"wires":[["limit"]]},
  {"id":"limit","type":"junction","z":"t1","name":"limit","x":2,"y":1,"wires":[[]],
   "ew_inSchema":{"type":"object","properties":{"payload":{"type":"number","minimum":0,"maximum":100}}}}
]`
}

func TestAnInjectLiteralTheReceiverCanNeverTakeIsRefusedAndNothingNotices(t *testing.T) {
	app, _ := newApp(t)
	e := serveAppWith(t, app, map[string][]string{"dana": {"flows.read", "flows.write"}}, nil)
	dana := e.login(t, "dana")

	// A literal that matches deploys, and so does one that isn't known until it fires.
	for _, ok := range []string{injectFlow("42.5", "num"), injectFlow("", "date"), injectFlow("LIMIT", "env"), injectFlow("flow.limit", "jsonata")} {
		if code, out := e.call(t, "POST", "/flows", dana, []byte(ok)); code != http.StatusOK {
			t.Fatalf("a flow that should deploy: %d %s", code, out)
		}
	}
	if _, err := deployDoc(t, app, injectFlow("42.5", "num")); err != nil {
		t.Fatal(err)
	}
	running := app.currentRuntime()
	file := append([]byte(nil), app.flowStore.Bytes()...)
	rev := app.flowStore.Rev()
	latest, _ := app.history.Latest()

	// A literal outside the receiver's range, and one of the wrong type, are refused.
	for _, bad := range []struct{ doc, want string }{
		{injectFlow("250", "num"), `payload: the inject sets 250, which the receiver does not take: maximum`},
		{injectFlow("hot", "str"), `payload: the inject sets "hot", which the receiver does not take: got string, want number`},
	} {
		code, out := e.call(t, "POST", "/flows", dana, []byte(bad.doc))
		var body struct {
			Error string `json:"error"`
			Wires []struct {
				NodeID  string `json:"nodeId"`
				Type    string `json:"type"`
				To      string `json:"to"`
				Message string `json:"message"`
			} `json:"wires"`
		}
		if code != http.StatusUnprocessableEntity || json.Unmarshal(out, &body) != nil || len(body.Wires) != 1 {
			t.Fatalf("POST /flows: %d %s", code, out)
		}
		w := body.Wires[0]
		if w.NodeID != "reading" || w.Type != "inject" || w.To != "limit" || !strings.Contains(w.Message, bad.want) ||
			!strings.Contains(body.Error, `inject "reading" (reading) output 1 -> "limit" (limit)`) ||
			!strings.Contains(body.Error, "the flows already running are untouched") {
			t.Fatalf("the refusal: %s", out)
		}
		t.Log(body.Error)
	}
	trail, _ := os.ReadFile(app.cfg.AuditPath())
	if !strings.Contains(string(trail), `"typed wires"`) {
		t.Errorf("the audit trail has no typed wires refusal:\n%s", trail)
	}

	// What was running before is still running, and nothing was written or recorded.
	if app.currentRuntime() != running || string(app.flowStore.Bytes()) != string(file) || app.flowStore.Rev() != rev {
		t.Error("a refused deploy changed what was running")
	}
	if got, _ := app.history.Latest(); got.Seq != latest.Seq {
		t.Errorf("a refused deploy was recorded as deployment %d", got.Seq)
	}
}
