package flowtest_test

import (
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
)

// H5: a test of a FlowLab diagram sets its controls, runs it and checks its
// indicators, with the real diagram engine.

const scaleDiagram = `[
  {"id":"main","type":"tab","label":"Scale","ew_diagram":{}},
  {"id":"w","type":"dia-control","z":"main","x":10,"y":10,"label":"Weight","kind":"number","wires":[["twice","cmp"]],"ew_in":[["a","a"]]},
  {"id":"k","type":"dia-const","z":"main","x":10,"y":60,"value":"2","wires":[["twice"]],"ew_in":[["b"]]},
  {"id":"twice","type":"dia-math","z":"main","x":100,"y":10,"op":"mul","wires":[["out"]],"ew_in":[["value"]]},
  {"id":"lim","type":"dia-const","z":"main","x":10,"y":110,"value":"10","wires":[["cmp"]],"ew_in":[["b"]]},
  {"id":"cmp","type":"dia-compare","z":"main","x":100,"y":110,"op":"gt","wires":[["heavy"]],"ew_in":[["value"]]},
  {"id":"out","type":"dia-indicator","z":"main","x":200,"y":10,"label":"Double","kind":"number"},
  {"id":"heavy","type":"dia-indicator","z":"main","x":200,"y":110,"label":"Heavy","kind":"bool"}
]`

func TestADiagramTestSetsControlsRunsAndChecksIndicators(t *testing.T) {
	r := run(t, scaleDiagram, `
tests:
  - name: a light load
    diagram:
      name: Scale
      controls: {Weight: 4}
      expect: {Double: 8, Heavy: false}
  - name: a heavy load, by range
    diagram:
      name: Scale
      controls: {Weight: 12.5}
      expect: {Double: {min: 24, max: 26}, Heavy: true}
`)
	if !r.OK() || r.Passed != 2 {
		t.Fatalf("%+v %s", r, problems(r.Tests[0])+problems(r.Tests[1]))
	}
}

func TestADiagramTestSaysWhatDidNotMatch(t *testing.T) {
	rep := run(t, scaleDiagram, `
tests:
  - name: wrong on purpose
    diagram:
      name: Scale
      controls: {Weight: 4}
      expect: {Double: 9, Heavy: true}
`)
	if rep.OK() || rep.Failed != 1 {
		t.Fatalf("%+v", rep)
	}
	got := rep.Tests[0].Problems
	if len(got) != 2 || !strings.Contains(got[0].Message, "Double") || !strings.Contains(got[0].Message, "expected 9, got 8") || !strings.Contains(got[1].Message, "Heavy") {
		t.Fatalf("%+v", got)
	}
}

func TestADiagramTestRefusesWhatItCannotRun(t *testing.T) {
	for _, c := range []struct{ suite, want string }{
		{`{name: x, diagram: {name: Nothing, expect: {Double: 1}}}`, `no diagram is named "Nothing"`},
		{`{name: x, diagram: {name: Scale, controls: {Nope: 1}, expect: {Double: 1}}}`, `"Nope" is not a control`},
		{`{name: x, diagram: {name: Scale, controls: {Weight: "heavy"}, expect: {Double: 1}}}`, "control \"Weight\""},
		{`{name: x, diagram: {name: Scale, expect: {Weight: 1}}}`, `"Weight" is not an indicator`},
	} {
		r := one(t, scaleDiagram, "tests:\n  - "+c.suite+"\n")
		if r.Status != flowtest.Error && r.Status != flowtest.Fail {
			t.Errorf("%s: %s", c.suite, r.Status)
		}
		if len(r.Problems) == 0 || !strings.Contains(r.Problems[0].Message, c.want) {
			t.Errorf("%s: %+v want %q", c.suite, r.Problems, c.want)
		}
	}
}

func TestADiagramTestWillNotRunNodesThatReachOutside(t *testing.T) {
	flows := `[
  {"id":"main","type":"tab","label":"Bench","ew_diagram":{}},
  {"id":"p","type":"dia-serial-open","z":"main","x":1,"y":1,"device":"/dev/ttyUSB0","wires":[[]],"ew_in":[[]]}
]`
	r := one(t, flows, "tests:\n  - {name: x, diagram: {name: Bench, expect: {X: 1}}}\n")
	if r.Status != flowtest.Error || len(r.Problems) == 0 || !strings.Contains(r.Problems[0].Message, "dia-serial-open") {
		t.Fatalf("%+v", r)
	}
}

// A diagram that writes rows into a database is refused by a test, never run: a
// test has no database stand-in, and it must not write to a real one.
func TestADiagramTestRefusesToWriteRowsToADatabase(t *testing.T) {
	doc := `[
  {"id":"pg","type":"hotloop-flow-postgres","host":"127.0.0.1","port":1,"database":"x","user":"x"},
  {"id":"main","type":"tab","label":"Rows","ew_diagram":{}},
  {"id":"c","type":"dia-const","z":"main","x":10,"y":10,"value":"{\"a\":1}","wires":[["out"]],"ew_in":[["row"]]},
  {"id":"out","type":"dia-postgres-out","z":"main","x":100,"y":10,"server":"pg","table":"t","columns":"a","wires":[]},
  {"id":"one","type":"dia-const","z":"main","x":10,"y":60,"value":"1","wires":[["seen"]],"ew_in":[["value"]]},
  {"id":"seen","type":"dia-indicator","z":"main","x":100,"y":60,"label":"Seen","kind":"number"}
]`
	r := one(t, doc, "tests:\n  - {name: x, diagram: {name: Rows, expect: {Seen: 1}}}\n")
	if r.Status == flowtest.Pass {
		t.Fatalf("a test ran a diagram that writes to a database: %+v", r)
	}
	var said string
	for _, p := range r.Problems {
		said += p.Message
	}
	if !strings.Contains(said, "dia-postgres-out") {
		t.Fatalf("problems %q do not name the node", said)
	}
}
