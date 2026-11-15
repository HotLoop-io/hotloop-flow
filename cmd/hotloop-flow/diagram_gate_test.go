package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// B8: a diagram commands equipment through HotLoop's write gate and no other way.
// The gate here is a server that answers like HotLoop's POST /api/write: it takes
// a value in range and refuses one out of range, in the gate's own words.
type gateServer struct {
	*httptest.Server
	mu     sync.Mutex
	writes []map[string]any
}

func newGateServer(t *testing.T) *gateServer {
	g := &gateServer{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/write" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		g.writes = append(g.writes, in)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if v, _ := in["value"].(float64); v > 100 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fmt.Sprintf("%g is above the range of plc-1/Setpoint (0 to 100)", v)})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *gateServer) seen() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.writes...)
}

func gateFlow(url, value string) string {
	return fmt.Sprintf(`[
  {"id":"hl","type":"hotloop-flow-hotloop","name":"plant","url":%q,"username":"","timeout":5},
  {"id":"main","type":"tab","label":"Heat","ew_diagram":{}},
  {"id":"v","type":"dia-const","z":"main","x":100,"y":100,"value":%q,"wires":[["w"]],"ew_in":[["value"]]},
  {"id":"w","type":"dia-hotloop-write","z":"main","x":300,"y":100,"hotloop":"hl","tag":"plc-1/Setpoint","wires":[["ok"],["why"],["bad"]],"ew_in":[["value"],["value"],["value"]]},
  {"id":"ok","type":"dia-indicator","z":"main","x":500,"y":60,"wires":[]},
  {"id":"why","type":"dia-indicator","z":"main","x":500,"y":100,"wires":[]},
  {"id":"bad","type":"dia-indicator","z":"main","x":500,"y":140,"wires":[]}
]`, url, value)
}

func runGateDiagram(t *testing.T, flow string) *diagram.Run {
	app := newTestApp(t)
	app.diagrams = diagram.NewManager(nil)
	defer app.diagrams.Close()
	app.diagrams.SetServices(diagram.Services{
		ConfigNode: func(id string) (node.Node, bool) { return app.currentRuntime().ConfigNode(id) },
	})
	deployJSON(t, app, flow, runtime.DeployFull)
	d, ok := app.diagrams.Diagram("main")
	if !ok || len(d.Problems) > 0 {
		t.Fatalf("the diagram: %v", d)
	}
	r, err := app.diagrams.Start("main", diagram.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	return r
}

func wireLast(t *testing.T, r *diagram.Run, key string) any {
	t.Helper()
	w, ok := r.WireValues()[key]
	if !ok {
		t.Fatalf("no value on wire %s", key)
	}
	return w.Last
}

// A value in range goes through the gate, with the diagram and the node in the
// reason HotLoop records.
func TestADiagramWritesThroughTheGateAndNamesItself(t *testing.T) {
	g := newGateServer(t)
	r := runGateDiagram(t, gateFlow(g.URL, "60"))
	if st, err := r.State(); st != diagram.StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	w := g.seen()
	if len(w) != 1 || w[0]["tag"] != "plc-1/Setpoint" || w[0]["value"] != 60.0 || w[0]["reason"] != "flow:main/w" {
		t.Fatalf("the gate saw %v", w)
	}
	if got := wireLast(t, r, "main/w.written>ok.value"); got != 60.0 {
		t.Fatalf("written %v", got)
	}
	if got := wireLast(t, r, "main/w.reason>why.value"); got != "flow:main/w" {
		t.Fatalf("reason %v", got)
	}
}

// A refusal reaches the diagram as an error with the gate's reason, and the
// equipment was not written to by anything else.
func TestADiagramGateRefusalIsAnErrorWithTheGatesReason(t *testing.T) {
	g := newGateServer(t)
	r := runGateDiagram(t, gateFlow(g.URL, "250"))
	e, _ := wireLast(t, r, "main/w.err>bad.value").(map[string]any)
	src, _ := e["source"].(string)
	if e["status"] != true || e["code"] != float64(diagram.CodeGate) || !strings.Contains(src, "250 is above the range of plc-1/Setpoint (0 to 100)") {
		t.Fatalf("error out %v", e)
	}
	if got := len(g.seen()); got != 1 {
		t.Fatalf("the gate was asked %d times", got)
	}
}

// With no HotLoop the write never happens: a node without one does not deploy as
// a way around the gate.
func TestAGateWriteNeedsAHotLoop(t *testing.T) {
	flow := `[{"id":"main","type":"tab","label":"Heat","ew_diagram":{}},
  {"id":"w","type":"dia-hotloop-write","z":"main","x":300,"y":100,"tag":"plc-1/Setpoint","wires":[[],[],[]]}]`
	app := newTestApp(t)
	app.diagrams = diagram.NewManager(nil)
	defer app.diagrams.Close()
	deployJSON(t, app, flow, runtime.DeployFull)
	d, _ := app.diagrams.Diagram("main")
	found := false
	for _, p := range d.Problems {
		found = found || strings.Contains(p.Text, "no HotLoop selected")
	}
	if !found {
		t.Fatalf("problems %v", d.Problems)
	}
}
