package diagram

import (
	"strings"
	"testing"
)

func panelSetFlow(t *testing.T, target, prop, value string) *flowBuilder {
	return newFlow(t).tab("main", nil).
		node("main", "ind", "dia-indicator", map[string]any{"label": "Pressure", "kind": "number"}).
		node("main", "ctl", "dia-control", map[string]any{"label": "Setpoint", "kind": "number"}).
		node("main", "v", "dia-const", map[string]any{"value": value}).
		node("main", "set", "dia-panel-set", map[string]any{"target": target, "property": prop}).
		node("main", "done", "dia-indicator", map[string]any{"label": "Applied", "kind": "string"}).
		node("main", "p", "dia-const", map[string]any{"value": "42"}).
		wire("v.value", "set.value").wire("set.set", "done.value").wire("p.value", "ind.value")
}

// P5: a node turns an indicator red by its name, and the panel carries it, in the
// snapshot a browser opening it gets and in the event the browser watching gets.
func TestPanelSetTurnsAnObjectRedByName(t *testing.T) {
	m := panelSetFlow(t, "Pressure", "color", `"red"`).manager()
	mustProblemFree(t, m, "main")
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	p, _ := m.Panel("main")
	if got := p.Snapshot().Attributes["ind"]["color"]; got != "red" {
		t.Fatalf("attributes %v", p.Snapshot().Attributes)
	}
}

// Disabling a control stops the operator setting it, and a run starts from the
// panel as it was designed.
func TestPanelSetDisablesAControlAndTheNextRunStartsClean(t *testing.T) {
	m := panelSetFlow(t, "Setpoint", "enabled", "false").manager()
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	p, _ := m.Panel("main")
	if _, err := p.SetControl("ctl", 5.0); err == nil || !strings.Contains(err.Error(), "switched off by the diagram") {
		t.Fatalf("a disabled control took a value: %v", err)
	}
	p.begin(m.Terminals("main"))
	if _, err := p.SetControl("ctl", 5.0); err != nil {
		t.Fatalf("a new run did not start from the design: %v", err)
	}
}

// A name that matches nothing is a problem at deploy; a wrong kind of value is an
// error on the wire.
func TestPanelSetRefusesWhatItCannotApply(t *testing.T) {
	d, _ := panelSetFlow(t, "Nothing like it", "color", `"red"`).manager().Diagram("main")
	found := false
	for _, p := range d.Problems {
		found = found || strings.Contains(p.Text, `no panel object is named "Nothing like it"`)
	}
	if !found {
		t.Fatalf("problems %v", d.Problems)
	}

	b := panelSetFlow(t, "Pressure", "color", `"purple"`).
		node("main", "e", "dia-indicator", map[string]any{"kind": "error"}).wire("set.err", "e.value")
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := lastOn(t, r, "main/set.err>e.value").(map[string]any)
	if src, _ := e["source"].(string); !strings.Contains(src, `"purple" is not a colour`) && !strings.Contains(src, "is not a colour") {
		t.Fatalf("error %v", e)
	}
}
