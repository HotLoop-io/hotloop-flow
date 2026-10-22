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
