package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// The deployment type header decides how much of the plant a save restarts, so
// the API has to hand the deploy exactly what was asked for: nothing said is a
// partial deploy, our header and Node-RED's both work, ours wins when both are
// sent, and a type nobody knows is refused before anything is saved.
func TestDeployTypeHeader(t *testing.T) {
	ts := newTestServer(t, map[string][]string{"admin": {"*"}})
	tok := ts.mustLogin(t, "admin")
	doc := []byte(`[{"id":"t1","type":"tab","label":"Line 3"}]`)

	cases := []struct {
		hdr  []string
		want runtime.DeployMode
	}{
		{nil, runtime.DeployNodes},
		{[]string{"HotLoop-Flow-Deployment-Type", "full"}, runtime.DeployFull},
		{[]string{"HotLoop-Flow-Deployment-Type", "flows"}, runtime.DeployFlows},
		{[]string{"Node-RED-Deployment-Type", "full"}, runtime.DeployFull},
		{[]string{"Node-RED-Deployment-Type", "full", "HotLoop-Flow-Deployment-Type", "nodes"}, runtime.DeployNodes},
	}
	for _, c := range cases {
		ts.modes = nil
		res, out := ts.do(t, "POST", "/flows", tok, doc, c.hdr...)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%v: %d %s", c.hdr, res.StatusCode, out)
		}
		if !slices.Equal(ts.modes, []runtime.DeployMode{c.want}) {
			t.Errorf("%v: deploy was handed %v, want %s", c.hdr, ts.modes, c.want)
		}
		var body struct {
			Type      string   `json:"type"`
			Restarted []string `json:"restarted"`
		}
		if err := json.Unmarshal(out, &body); err != nil {
			t.Fatal(err)
		}
		if body.Type != string(c.want) {
			t.Errorf("%v: response type = %q, want %q", c.hdr, body.Type, c.want)
		}
		if body.Restarted == nil {
			t.Errorf("%v: restarted came back as null, want a list", c.hdr)
		}
	}

	ts.modes = nil
	res, _ := ts.do(t, "POST", "/flows", tok, doc, "Node-RED-Deployment-Type", "reload")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown deployment type: %d, want 400", res.StatusCode)
	}
	if len(ts.modes) != 0 {
		t.Error("an unknown deployment type still reached the deploy")
	}
}
