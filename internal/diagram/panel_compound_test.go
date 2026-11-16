package diagram

import (
	"reflect"
	"strings"
	"testing"
)

// P2: array and record controls, and the knob. The server holds each to its
// kind the way it holds a number to its range, and the diagram reads one value.

func arrayControl(extra map[string]any) map[string]any {
	raw := map[string]any{"kind": "array", "elem": "number"}
	for k, v := range extra {
		raw[k] = v
	}
	return raw
}

func recordControl(extra map[string]any) map[string]any {
	raw := map[string]any{"kind": "record", "fields": "rate:number, on:bool, note:string"}
	for k, v := range extra {
		raw[k] = v
	}
	return raw
}

func TestArrayControlHoldsEachItemToItsKindAndRange(t *testing.T) {
	raw := arrayControl(map[string]any{"min": "0", "max": "10", "maxlen": "3"})
	got, err := coerceControl(raw, []any{1.0, "2.5", 3.0})
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{1.0, 2.5, 3.0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for _, bad := range []struct {
		name string
		v    any
		want string
	}{
		{"an item above the range", []any{1.0, 11.0}, "item 2: 11 is above the maximum 10"},
		{"an item that is not a number", []any{"x"}, `item 1: "x" is not a number`},
		{"too many items", []any{1.0, 2.0, 3.0, 4.0}, "4 items is more than the most this control holds, 3"},
		{"not a list", 5.0, "an array control takes a list"},
	} {
		if _, err := coerceControl(raw, bad.v); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: error %v, want it to say %q", bad.name, err, bad.want)
		}
	}
	// An empty list is a list.
	if got, err := coerceControl(raw, []any{}); err != nil || len(got.([]any)) != 0 {
		t.Fatalf("empty list: %v, %v", got, err)
	}
}

func TestArrayControlOfSwitchesAndOfText(t *testing.T) {
	got, err := coerceControl(arrayControl(map[string]any{"elem": "bool"}), []any{true, false, "true", 0.0})
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{true, false, true, false}; !reflect.DeepEqual(got, want) {
		t.Fatalf("switches: got %#v, want %#v", got, want)
	}
	if _, err := coerceControl(arrayControl(map[string]any{"elem": "bool"}), []any{"maybe"}); err == nil {
		t.Fatal("a list of switches took maybe")
	}
	got, err = coerceControl(arrayControl(map[string]any{"elem": "string"}), []any{"a", "b"})
	if err != nil || !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Fatalf("text: %#v, %v", got, err)
	}
	if _, err := coerceControl(arrayControl(map[string]any{"elem": "string"}), []any{1.0}); err == nil {
		t.Fatal("a list of text took a number")
	}
	if _, err := coerceControl(arrayControl(map[string]any{"elem": "matrix"}), []any{}); err == nil || !strings.Contains(err.Error(), "number, bool or string") {
		t.Fatalf("an unknown item kind: %v", err)
	}
}

func TestRecordControlHasExactlyItsFields(t *testing.T) {
	raw := recordControl(nil)
	got, err := coerceControl(raw, map[string]any{"rate": "4.5", "on": true, "note": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"rate": 4.5, "on": true, "note": "ok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	// A field left out takes its kind's empty value, so a record always has every field.
	got, err = coerceControl(raw, map[string]any{"rate": 2.0})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"rate": 2.0, "on": false, "note": ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for _, bad := range []struct {
		name string
		v    any
		want string
	}{
		{"a field that is not there", map[string]any{"speed": 1.0}, `"speed" is not a field of this record`},
		{"a field of the wrong kind", map[string]any{"rate": "fast"}, `field "rate": "fast" is not a number`},
		{"not an object", []any{1.0}, "a record control takes an object"},
	} {
		if _, err := coerceControl(raw, bad.v); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: error %v, want it to say %q", bad.name, err, bad.want)
		}
	}
}

func TestRecordFieldsAreReadStrictly(t *testing.T) {
	for _, bad := range []struct{ fields, want string }{
		{"", "needs its fields"},
		{"a:number, a:bool", `the field "a" is listed twice`},
		{"a:matrix", `the field "a" is "matrix"`},
		{":number", "has no name"},
	} {
		if _, err := recordFields(map[string]any{"fields": bad.fields}); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("fields %q: error %v, want %q", bad.fields, err, bad.want)
		}
	}
	got, err := recordFields(map[string]any{"fields": "level, mode:string"})
	if err != nil || !reflect.DeepEqual(got, []recField{{"level", "number"}, {"mode", "string"}}) {
		t.Fatalf("a field with no kind is a number: %#v, %v", got, err)
	}
}

func TestCompoundControlsStartFromTheirDefault(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  map[string]any
		want any
	}{
		{"an empty array", arrayControl(nil), []any{}},
		{"an array in JSON", arrayControl(map[string]any{"default": "[1, 2, 3]"}), []any{1.0, 2.0, 3.0}},
		{"an array as a comma list", arrayControl(map[string]any{"default": "4, 5"}), []any{4.0, 5.0}},
		{"a record with every field empty", recordControl(nil), map[string]any{"rate": 0.0, "on": false, "note": ""}},
		{"a record in JSON", recordControl(map[string]any{"default": `{"rate": 3, "note": "hi"}`}), map[string]any{"rate": 3.0, "on": false, "note": "hi"}},
	} {
		got, err := controlDefault(c.raw)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, %v; want %#v", c.name, got, err, c.want)
		}
	}
	for _, bad := range []map[string]any{
		arrayControl(map[string]any{"default": "[1, "}),
		arrayControl(map[string]any{"default": "one, two"}),
		recordControl(map[string]any{"default": "[1]"}),
		recordControl(map[string]any{"default": `{"speed": 1}`}),
		{"kind": "record"},
	} {
		if _, err := controlDefault(bad); err == nil {
			t.Errorf("a control with the starting value %v was accepted", bad)
		}
	}
}

// Scalar controls answer as they did before the compound ones were added.
func TestScalarControlsAreUnchanged(t *testing.T) {
	cases := []struct {
		raw  map[string]any
		v    any
		want string
	}{
		{map[string]any{"kind": "number", "min": "0", "max": "5"}, 6.0, "6 is above the maximum 5"},
		{map[string]any{"kind": "number"}, "abc", `"abc" is not a number`},
		{map[string]any{"kind": "bool"}, "perhaps", `"perhaps" is not true or false`},
		{map[string]any{"kind": "string"}, 4.0, "a text control takes text"},
		{map[string]any{"kind": "enum", "options": "a,b"}, "c", `"c" is not one of the choices`},
	}
	for _, c := range cases {
		if _, err := coerceControl(c.raw, c.v); err == nil || err.Error() != c.want {
			t.Errorf("%v with %v: error %v, want %q", c.raw, c.v, err, c.want)
		}
	}
}

// The panel serves what a browser needs to draw them.
func TestTerminalsCarryTheShapeOfCompoundControls(t *testing.T) {
	arr := terminalOf(&DNode{ID: "a", Type: "dia-control", Raw: arrayControl(map[string]any{"elem": "string", "maxlen": "4"})}, 0)
	if arr.Elem != "string" || arr.MaxLen != 4 {
		t.Fatalf("array terminal: %+v", arr)
	}
	rec := terminalOf(&DNode{ID: "r", Type: "dia-control", Raw: recordControl(nil)}, 1)
	if !reflect.DeepEqual(rec.Fields, []recField{{"rate", "number"}, {"on", "bool"}, {"note", "string"}}) {
		t.Fatalf("record terminal fields: %+v", rec.Fields)
	}
}

// A knob needs a range to turn through, and says so when it has none. Nothing
// else about a control warns.
func TestKnobWithoutARangeWarns(t *testing.T) {
	warns := func(raw map[string]any) bool {
		d, ok := newFlow(t).tab("main", nil).node("main", "k", "dia-control", raw).
			node("main", "i", "dia-indicator", map[string]any{"kind": "number"}).wire("k.value", "i.value").manager().Diagram("main")
		if !ok {
			t.Fatal("no diagram")
		}
		for _, w := range d.Warnings {
			if strings.Contains(w.Text, "a knob needs a smallest and a largest value") {
				return true
			}
		}
		return false
	}
	if !warns(map[string]any{"kind": "number", "style": "knob"}) {
		t.Fatal("a knob with no range did not warn")
	}
	if !warns(map[string]any{"kind": "number", "style": "knob", "min": "0"}) {
		t.Fatal("a knob with half a range did not warn")
	}
	if warns(map[string]any{"kind": "number", "style": "knob", "min": "0", "max": "100"}) {
		t.Fatal("a knob with a range warned")
	}
	if warns(map[string]any{"kind": "number", "style": "slider"}) {
		t.Fatal("a slider warned, and it never did")
	}
}

// What the operator sets is what the diagram reads when the control runs, as a
// list and as an object, and an indicator shows it.
func TestCompoundControlsReachTheDiagram(t *testing.T) {
	m := newFlow(t).tab("main", nil).
		node("main", "arr", "dia-control", arrayControl(map[string]any{"label": "Setpoints"})).
		node("main", "rec", "dia-control", recordControl(map[string]any{"label": "Recipe"})).
		node("main", "ai", "dia-indicator", map[string]any{"kind": "text", "label": "A"}).
		node("main", "ri", "dia-indicator", map[string]any{"kind": "text", "label": "R"}).
		wire("arr.value", "ai.value").wire("rec.value", "ri.value").manager()
	mustProblemFree(t, m, "main")
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	p, _ := m.Panel("main")
	// Before anyone sets anything the diagram reads the starting values.
	snap := p.Snapshot()
	if !reflect.DeepEqual(snap.Indicators["ai"], []any{}) {
		t.Fatalf("the array before it was set: %#v", snap.Indicators["ai"])
	}
	if !reflect.DeepEqual(snap.Indicators["ri"], map[string]any{"rate": 0.0, "on": false, "note": ""}) {
		t.Fatalf("the record before it was set: %#v", snap.Indicators["ri"])
	}

	if _, err := p.SetControl("arr", []any{1.0, 2.0, 3.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetControl("rec", map[string]any{"rate": 7.5, "on": true, "note": "batch 4"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetControl("arr", []any{"no"}); err == nil {
		t.Fatal("the array control took a word")
	}
	r, err = m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	snap = p.Snapshot()
	if !reflect.DeepEqual(snap.Indicators["ai"], []any{1.0, 2.0, 3.0}) {
		t.Fatalf("the array the diagram read: %#v", snap.Indicators["ai"])
	}
	if !reflect.DeepEqual(snap.Indicators["ri"], map[string]any{"rate": 7.5, "on": true, "note": "batch 4"}) {
		t.Fatalf("the record the diagram read: %#v", snap.Indicators["ri"])
	}
	if !reflect.DeepEqual(snap.Controls["arr"], []any{1.0, 2.0, 3.0}) {
		t.Fatalf("the panel's array: %#v", snap.Controls["arr"])
	}
}

// A diagram that cannot start because a control is wrong says so at deploy.
func TestARecordWithoutFieldsIsAProblemAtDeploy(t *testing.T) {
	d, _ := newFlow(t).tab("main", nil).
		node("main", "rec", "dia-control", map[string]any{"kind": "record"}).
		node("main", "i", "dia-indicator", map[string]any{"kind": "text"}).wire("rec.value", "i.value").manager().Diagram("main")
	found := false
	for _, p := range d.Problems {
		found = found || strings.Contains(p.Text, "needs its fields")
	}
	if !found {
		t.Fatalf("problems %v", d.Problems)
	}
}

// An operator sets a control before pressing run, and the first run reads it. The
// panel used to learn its controls only when a run began, so a control set before
// the first run was refused as not being on the panel.
func TestAControlCanBeSetBeforeTheFirstRun(t *testing.T) {
	m := newFlow(t).tab("main", nil).
		node("main", "arr", "dia-control", arrayControl(map[string]any{"label": "Setpoints"})).
		node("main", "ai", "dia-indicator", map[string]any{"kind": "text", "label": "A"}).
		wire("arr.value", "ai.value").manager()
	p, err := m.Panel("main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetControl("arr", []any{4.0, 5.0}); err != nil {
		t.Fatalf("a control set before the first run was refused: %v", err)
	}
	if _, err := p.SetControl("nothing", 1.0); err == nil {
		t.Fatal("a control that is not on the panel took a value")
	}
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	if got := p.Snapshot().Indicators["ai"]; !reflect.DeepEqual(got, []any{4.0, 5.0}) {
		t.Fatalf("the first run read %#v", got)
	}
}

// A control a deploy changed so that its old value no longer fits goes back to
// its default, instead of handing a diagram a number where it asked for a list.
func TestAControlThatChangedKindForgetsWhatDoesNotFitIt(t *testing.T) {
	p := newPanel("main", nil)
	num := &DNode{ID: "c", Type: "dia-control", Raw: map[string]any{"kind": "number"}}
	p.learn([]*DNode{num})
	if _, err := p.SetControl("c", 7.0); err != nil {
		t.Fatal(err)
	}
	p.learn([]*DNode{{ID: "c", Type: "dia-control", Raw: map[string]any{"kind": "number"}}})
	if got := p.Control("c", nil); got != 7.0 {
		t.Fatalf("a control that did not change lost its value: %v", got)
	}
	p.learn([]*DNode{{ID: "c", Type: "dia-control", Raw: arrayControl(nil)}})
	if got := p.Control("c", "default"); got != "default" {
		t.Fatalf("a number was kept for an array control: %v", got)
	}
}
