package diagram

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// five nodes plus a constant: (a + b) * c, shown twice.
func arithmetic(t *testing.T) *flowBuilder {
	return newFlow(t).tab("main", nil).
		node("main", "a", "dia-const", map[string]any{"value": "2"}).
		node("main", "b", "dia-const", map[string]any{"value": "3"}).
		node("main", "c", "dia-const", map[string]any{"value": "4"}).
		node("main", "add", "dia-math", map[string]any{"op": "add"}).
		node("main", "mul", "dia-math", map[string]any{"op": "mul"}).
		node("main", "out", "dia-indicator", nil).node("main", "again", "dia-indicator", nil).
		wire("a.value", "add.a").wire("b.value", "add.b").
		wire("add.result", "mul.a").wire("c.value", "mul.b").
		wire("mul.result", "out.value").wire("mul.result", "again.value")
}

func runDoc(t *testing.T, doc []byte) *Run {
	flows, err := engine.ParseFlows(doc)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil)
	t.Cleanup(m.Close)
	m.Apply(flows)
	d, _ := m.Diagram("main")
	if len(d.Problems) > 0 {
		t.Fatalf("problems %v", d.Problems)
	}
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	return r
}

// H1: select two nodes, make a sub-diagram. The connector is inferred (an input
// for each wire that came in, an output for the one that went out to two nodes)
// and the run gives the same answers.
func TestExtractSubInfersTheConnectorAndKeepsTheResult(t *testing.T) {
	b := arithmetic(t)
	before := runDoc(t, mustJSON(t, b.entries))
	if got := lastOn(t, before, "main/mul.result>out.value"); got != 20.0 {
		t.Fatalf("before: %v", got)
	}

	doc, err := ExtractSub(mustJSON(t, b.entries), "main", []string{"add", "mul"}, "Scale")
	if err != nil {
		t.Fatal(err)
	}
	flows, _ := engine.ParseFlows(doc)
	set := CompileAll(flows)
	var sub *DNode
	for _, id := range set.Order {
		for _, nid := range set.Diagrams[id].Order {
			if n := set.Diagrams[id].Nodes[nid]; n.Type == "dia-sub" {
				sub = n
			}
		}
	}
	if sub == nil {
		t.Fatal("no sub-diagram node was made")
	}
	if got := strProp(sub.Raw, "ins", ""); got != "a,b,b2" {
		t.Errorf("inputs %q, want a,b,b2", got)
	}
	if got := strProp(sub.Raw, "outs", ""); got != "mul_result" {
		t.Errorf("outputs %q, want mul_result", got)
	}
	after := runDoc(t, doc)
	for _, w := range []string{"out", "again"} {
		found := false
		for k, v := range after.WireValues() {
			if strings.HasSuffix(k, ">"+w+".value") && v.Last == 20.0 {
				found = true
			}
		}
		if !found {
			t.Errorf("indicator %s did not show 20 after the extraction", w)
		}
	}
	// The label is the name and the body tab exists with the moved nodes.
	if d := set.Diagrams[strProp(sub.Raw, "body", "")]; d == nil || d.Label != "Scale" || len(d.Nodes) != 2+3+1 {
		t.Errorf("body: %+v", d)
	}
}

func TestExtractSubRefusesWhatItCannotDo(t *testing.T) {
	doc := mustJSON(t, arithmetic(t).entries)
	for _, c := range []struct {
		tab  string
		ids  []string
		want string
	}{{"main", nil, "select at least one"}, {"nope", []string{"add"}, "not a diagram"}, {"main", []string{"zzz"}, "not a node"}} {
		if _, err := ExtractSub(doc, c.tab, c.ids, ""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v", c.ids, err)
		}
	}
}

// A recommended input runs without a wire and warns at deploy; an optional one
// does not warn; a required one is a problem.
func TestSubInputMarks(t *testing.T) {
	b := newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
		node("main", "s", "dia-sub", map[string]any{"body": "body", "ins": "need,opt?,rec~", "outs": "r"}).
		node("body", "n", "dia-body-in", map[string]any{"name": "need"}).
		node("body", "o", "dia-body-in", map[string]any{"name": "opt"}).
		node("body", "c", "dia-body-in", map[string]any{"name": "rec"}).
		node("body", "r", "dia-body-out", map[string]any{"name": "r"}).
		wire("n.value", "r.value")
	d, _ := b.manager().Diagram("main")
	var probs, warns []string
	for _, p := range d.Problems {
		probs = append(probs, p.String())
	}
	for _, p := range d.Warnings {
		warns = append(warns, p.String())
	}
	if len(probs) != 1 || !strings.Contains(probs[0], "need: a required input has no wire") {
		t.Errorf("problems %v", probs)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "rec: a recommended input has no wire") {
		t.Errorf("warnings %v", warns)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
