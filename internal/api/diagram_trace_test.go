package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// G1: the editor asks for what the last run did, to replay it after it is over. A
// diagram that has not run says so with a 404, one that does not exist is a 404
// too, and one that ran returns its events in order.
func TestDiagramTraceServesTheLastRun(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Enabled = false
	cfg.Auth.Insecure = true
	m := diagram.NewManager(nil)
	t.Cleanup(m.Close)
	srv := httptest.NewServer(New(Deps{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", Diagrams: m}).Handler())
	t.Cleanup(srv.Close)

	flows, err := engine.ParseFlows([]byte(`[{"id":"main","type":"tab","label":"Main","ew_diagram":{}},
	 {"id":"a","type":"dia-const","z":"main","x":10,"y":10,"value":"2","wires":[["add"]],"ew_in":[["a"]]},
	 {"id":"b","type":"dia-const","z":"main","x":10,"y":50,"value":"3","wires":[["add"]],"ew_in":[["b"]]},
	 {"id":"add","type":"dia-math","z":"main","x":50,"y":10,"op":"add","wires":[["out"]],"ew_in":[["value"]]},
	 {"id":"out","type":"dia-indicator","z":"main","x":90,"y":10,"wires":[]}]`))
	if err != nil {
		t.Fatal(err)
	}
	m.Apply(flows)

	get := func(path string) (int, []byte) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	if code, _ := get("/diagrams/main/trace"); code != http.StatusNotFound {
		t.Fatalf("a diagram that has not run: %d, want 404", code)
	}
	if code, _ := get("/diagrams/nope/trace"); code != http.StatusNotFound {
		t.Fatalf("a diagram that is not there: %d, want 404", code)
	}
	r, err := m.Start("main", diagram.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	code, body := get("/diagrams/main/trace")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	var tr struct {
		Run       string `json:"run"`
		State     string `json:"state"`
		Truncated bool   `json:"truncated"`
		Events    []struct {
			T    float64        `json:"t"`
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Run != r.ID || tr.State != "done" || tr.Truncated || len(tr.Events) == 0 {
		t.Fatalf("trace %+v", tr)
	}
	var sawSum bool
	for _, e := range tr.Events {
		if e.Kind == "token" && e.Data["wire"] == "main/add.result>out.value" && e.Data["value"] == 5.0 {
			sawSum = true
		}
	}
	if !sawSum {
		t.Fatalf("the recording has no 5 on the add wire: %s", body)
	}
}
