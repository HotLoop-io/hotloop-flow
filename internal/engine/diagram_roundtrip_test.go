package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A tab that is a diagram and nodes that name the input a wire lands on are new
// keys in the flow file. The file still loads and saves byte for byte, and
// editing one node leaves them alone.
func TestRoundTripCarriesDiagrams(t *testing.T) {
	src := `[
    {"id":"d","type":"tab","label":"Scale","disabled":false,"info":"","ew_diagram":{"autostart":true}},
    {"id":"a","type":"dia-const","z":"d","value":"1","x":100,"y":80,"ew_in":[[""],["after"]],"wires":[["c"],["c"]]},
    {"id":"b","type":"dia-const","z":"d","value":"2","x":100,"y":160,"ew_in":[["b"],[]],"wires":[["c"],[]]},
    {"id":"c","type":"dia-math","z":"d","op":"add","x":300,"y":120,"wires":[[],[]]}
]`
	f, err := ParseFlows([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	g, err := ParseFlows(saved)
	if err != nil {
		t.Fatal(err)
	}
	again, err := g.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(saved) {
		w, got := firstDiffContext(string(saved), string(again))
		t.Fatalf("a file with diagrams changed on a second save\n--- want ---\n%s\n--- got ---\n%s", w, got)
	}
	if tab := g.Tabs["d"]; tab == nil || !tab.IsDiagram() {
		t.Fatal("the tab lost its diagram key")
	}
	if settings, _ := g.Tabs["d"].Diagram(); settings["autostart"] != true {
		t.Fatalf("settings: %v", settings)
	}
	if strings.Index(string(saved), `"ew_in"`) > strings.Index(string(saved), `"wires"`) {
		t.Fatal("ew_in moved after wires: key order was not kept")
	}

	// Touching one node rewrites it, and the new keys come through in place.
	g.Touch("a")
	out, err := g.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var want, got []map[string]any
	if err := json.Unmarshal(saved, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("a touched diagram node changed more than its bytes\nwant %v\ngot  %v", want, got)
	}
}

// The example logger, every entry touched so each is written again from its
// parsed form, is the same file.
func TestExampleDiagramSurvivesBeingRewritten(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "diagram-scale-logger", "flow.json"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseFlows(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range f.Order {
		f.Touch(id)
	}
	out, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var want, got []map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("rewriting every entry of the example changed it")
	}
	if len(want) < 10 {
		t.Fatalf("the example has only %d entries", len(want))
	}
}
