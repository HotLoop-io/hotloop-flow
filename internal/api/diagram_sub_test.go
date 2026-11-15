package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
)

// H1: the editor sends its flow file and a selection and gets the flow file back
// with a sub-diagram in it; nothing is stored.
func TestDiagramSubMakesASubDiagramFromASelection(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Enabled = false
	cfg.Auth.Insecure = true
	m := diagram.NewManager(nil)
	t.Cleanup(m.Close)
	srv := httptest.NewServer(New(Deps{Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", Diagrams: m}).Handler())
	t.Cleanup(srv.Close)

	flows := `[{"id":"main","type":"tab","label":"Main","ew_diagram":{}},
	 {"id":"a","type":"dia-const","z":"main","x":10,"y":10,"value":"2","wires":[["add"]],"ew_in":[["a"]]},
	 {"id":"add","type":"dia-math","z":"main","x":50,"y":10,"op":"add","wires":[["out"]],"ew_in":[["value"]]},
	 {"id":"out","type":"dia-indicator","z":"main","x":90,"y":10,"wires":[]}]`
	body, _ := json.Marshal(map[string]any{"flows": json.RawMessage(flows), "nodes": []string{"add"}, "name": "Adder"})
	res, err := http.Post(srv.URL+"/diagrams/main/sub", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Flows []map[string]any `json:"flows"`
		Error string           `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%d %v %s", res.StatusCode, err, out.Error)
	}
	var sub map[string]any
	for _, e := range out.Flows {
		if e["type"] == "dia-sub" {
			sub = e
		}
	}
	if sub == nil || sub["ins"] != "a" || sub["outs"] != "add_result" || sub["name"] != "Adder" {
		t.Fatalf("sub %v", sub)
	}

	bad, _ := json.Marshal(map[string]any{"flows": json.RawMessage(flows), "nodes": []string{}})
	res2, err := http.Post(srv.URL+"/diagrams/main/sub", "application/json", bytes.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("an empty selection: %d", res2.StatusCode)
	}
}
