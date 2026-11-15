package diagram

import (
	"strings"
	"testing"
)

func typedFlow(t *testing.T, fields map[string]any, names string) *flowBuilder {
	return newFlow(t).tab("d", map[string]any{"types": map[string]any{"Reading": map[string]any{"fields": fields}}}).
		node("d", "p", "dia-const", map[string]any{"value": "101.5"}).
		node("d", "ok", "dia-const", map[string]any{"value": "true"}).
		node("d", "rec", "dia-bundle", map[string]any{"names": names, "shape": "Reading"}).
		node("d", "out", "dia-indicator", nil).node("d", "e", "dia-indicator", nil).
		wire("p.value", "rec.pressure").wire("ok.value", "rec.ok").wire("rec.record", "out.value").wire("rec.err", "e.value")
}

// D1: a record built to a shared type runs, and the same type is JSON Schema.
func TestSharedRecordTypeBuildsAndIsASchema(t *testing.T) {
	b := typedFlow(t, map[string]any{"pressure": "number", "ok": "boolean"}, "pressure,ok")
	m := b.manager()
	mustProblemFree(t, m, "d")
	r, err := m.Start("d", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	rec, _ := lastOn(t, r, "d/rec.record>out.value").(map[string]any)
	if rec["pressure"] != 101.5 || rec["ok"] != true {
		t.Fatalf("record %v", rec)
	}
	set := CompileAll(b.flows())
	sch, ok := set.Schema("Reading")
	if !ok || sch["type"] != "object" || sch["additionalProperties"] != false {
		t.Fatalf("schema %v", sch)
	}
	props := sch["properties"].(map[string]any)
	if props["pressure"].(map[string]any)["type"] != "number" || props["ok"].(map[string]any)["type"] != "boolean" {
		t.Fatalf("properties %v", props)
	}
}

// D1: rename a field in the shared type and every use reports the break at
// deploy, naming the node and the field.
func TestRenamingASharedFieldBreaksEveryUseAtDeploy(t *testing.T) {
	b := typedFlow(t, map[string]any{"pressure_kpa": "number", "ok": "boolean"}, "pressure,ok")
	m := b.manager()
	d, _ := m.Diagram("d")
	text := ""
	for _, p := range d.Problems {
		text += p.String() + "\n"
	}
	for _, want := range []string{`rec: the type "Reading" has no field "pressure"`, `has a field "pressure_kpa" this bundle does not set`} {
		if !strings.Contains(text, want) {
			t.Errorf("problems lack %q:\n%s", want, text)
		}
	}
	if _, err := m.Start("d", StartOptions{}); err == nil {
		t.Error("a diagram with a broken type started")
	}
}

// A value of the wrong kind is an error on the wire, and a type nobody defined
// is a problem at deploy.
func TestSharedRecordTypeRefusesTheWrongKind(t *testing.T) {
	b := typedFlow(t, map[string]any{"pressure": "integer", "ok": "boolean"}, "pressure,ok")
	r, err := b.run("d")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := lastOn(t, r, "d/rec.err>e.value").(map[string]any)
	if src, _ := e["source"].(string); !strings.Contains(src, "Reading.pressure: 101.5 is not a whole number") {
		t.Fatalf("error %v", e)
	}

	b = newFlow(t).tab("d", nil).node("d", "rec", "dia-bundle", map[string]any{"names": "a", "shape": "Nope"})
	d, _ := b.manager().Diagram("d")
	found := false
	for _, p := range d.Problems {
		found = found || strings.Contains(p.Text, `"Nope" is not defined`)
	}
	if !found {
		t.Fatalf("problems %v", d.Problems)
	}
}

// Enums and nested records are checked, and a field node holds to the type too.
func TestSharedTypesNestAndEnumerate(t *testing.T) {
	tab := map[string]any{"types": map[string]any{
		"Pos":   map[string]any{"fields": map[string]any{"x": "number", "mode": map[string]any{"enum": []any{"idle", "run"}}}},
		"Robot": map[string]any{"fields": map[string]any{"pos": "record:Pos", "name": "string"}},
	}}
	set := CompileAll(newFlow(t).tab("d", tab).flows())
	if err := set.ValidateRecord("Robot", map[string]any{"name": "r1", "pos": map[string]any{"x": 1.0, "mode": "run"}}); err != nil {
		t.Fatal(err)
	}
	if err := set.ValidateRecord("Robot", map[string]any{"name": "r1", "pos": map[string]any{"x": 1.0, "mode": "fly"}}); err == nil || !strings.Contains(err.Error(), "Robot.pos: Pos.mode") {
		t.Fatalf("enum: %v", err)
	}
	b := newFlow(t).tab("d", tab).node("d", "f", "dia-field", map[string]any{"field": "colour", "shape": "Pos"})
	d, _ := b.manager().Diagram("d")
	found := false
	for _, p := range d.Problems {
		found = found || strings.Contains(p.Text, `no field "colour"`)
	}
	if !found {
		t.Fatalf("problems %v", d.Problems)
	}
}
