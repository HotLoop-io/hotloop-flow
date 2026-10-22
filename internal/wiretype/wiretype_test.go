package wiretype

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

func parse(t *testing.T, doc string) *engine.Flows {
	t.Helper()
	f, err := engine.ParseFlows([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const tempOut = `{"type":"object","required":["payload"],"properties":{"payload":{"type":"object","required":["temp"],"properties":{"temp":{"type":"string"}}}}}`
const tempIn = `{"type":"object","required":["payload"],"properties":{"payload":{"type":"object","required":["temp"],"properties":{"temp":{"type":"number"}}}}}`

func flowWith(out, in string) string {
	return `[{"id":"t1","type":"tab","label":"T"},
	 {"id":"a","type":"junction","z":"t1","name":"sensor","x":1,"y":1,"wires":[["b"]],"ew_outSchemas":[` + out + `]},
	 {"id":"b","type":"junction","z":"t1","name":"limit","x":2,"y":1,"wires":[[]],"ew_inSchema":` + in + `}]`
}

func TestTheEditorCanSeeAMismatch(t *testing.T) {
	ps := Check(parse(t, flowWith(tempOut, tempIn)))
	if len(ps) != 1 {
		t.Fatalf("problems: %v", ps)
	}
	want := `junction "sensor" (a) output 1 -> "limit" (b): payload.temp: the sender sends string and the receiver takes number`
	if ps[0].String() != want {
		t.Fatalf("got  %s\nwant %s", ps[0], want)
	}
}

func TestAgreeingEndsAreFine(t *testing.T) {
	same := strings.ReplaceAll(tempOut, `"string"`, `"number"`)
	if ps := Check(parse(t, flowWith(same, tempIn))); len(ps) != 0 {
		t.Fatalf("%v", ps)
	}
	// integer fits a number
	narrow := strings.ReplaceAll(tempOut, `"string"`, `"integer"`)
	if ps := Check(parse(t, flowWith(narrow, tempIn))); len(ps) != 0 {
		t.Fatalf("%v", ps)
	}
}

func TestOneEndDeclaringNothingIsNotJudged(t *testing.T) {
	doc := `[{"id":"t1","type":"tab","label":"T"},
	 {"id":"a","type":"junction","z":"t1","x":1,"y":1,"wires":[["b"]],"ew_outSchemas":[` + tempOut + `]},
	 {"id":"b","type":"junction","z":"t1","x":2,"y":1,"wires":[[]]}]`
	if ps := Check(parse(t, doc)); len(ps) != 0 {
		t.Fatalf("%v", ps)
	}
}

func TestConflictKinds(t *testing.T) {
	cases := []struct {
		name, sender, receiver, want string
	}{
		{"enum", `{"properties":{"mode":{"enum":["auto","manual"]}}}`, `{"properties":{"mode":{"enum":["run","stop"]}}}`, "mode: the sender only sends"},
		{"const", `{"properties":{"n":{"const":5}}}`, `{"properties":{"n":{"type":"number","minimum":10}}}`, "n: the sender only sends 5"},
		{"range", `{"properties":{"n":{"type":"number","maximum":10}}}`, `{"properties":{"n":{"type":"number","minimum":20}}}`, "n: the sender sends at most 10 and the receiver wants at least 20"},
		{"closed sender lacks required", `{"properties":{"a":{}},"additionalProperties":false}`, `{"required":["b"]}`, "b: the receiver requires it and the sender's schema allows no such field"},
		{"closed receiver refuses always-sent", `{"required":["x"]}`, `{"properties":{"a":{}},"additionalProperties":false}`, "x: the sender always sends it"},
		{"array", `{"properties":{"xs":{"type":"array","items":{"type":"string"}}}}`, `{"properties":{"xs":{"type":"array","items":{"type":"number"}}}}`, "xs[0]: the sender sends string"},
		{"type list", `{"properties":{"v":{"type":["string","null"]}}}`, `{"properties":{"v":{"type":["number","boolean"]}}}`, "v: the sender sends string or null and the receiver takes number or boolean"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Conflicts(decode(t, c.sender), decode(t, c.receiver))
			if len(got) != 1 || !strings.Contains(got[0], c.want) {
				t.Fatalf("got %q, want one containing %q", got, c.want)
			}
		})
	}
}

// What it can't prove it leaves for run time.
func TestNothingIsRefusedOnAGuess(t *testing.T) {
	for name, pair := range map[string][2]string{
		"sender doesn't mention a required field": {`{"properties":{"a":{"type":"string"}}}`, `{"required":["b"]}`},
		"ref":           {`{"$ref":"#/$defs/x","$defs":{"x":{"type":"string"}}}`, `{"type":"number"}`},
		"anyOf":         {`{"anyOf":[{"type":"string"},{"type":"number"}]}`, `{"type":"number"}`},
		"range touches": {`{"type":"number","maximum":10}`, `{"type":"number","minimum":10}`},
		"enum overlaps": {`{"enum":["a","b"]}`, `{"enum":["b","c"]}`},
	} {
		if got := Conflicts(decode(t, pair[0]), decode(t, pair[1])); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestBadSchemasAreProblems(t *testing.T) {
	doc := `[{"id":"t1","type":"tab","label":"T"},
	 {"id":"a","type":"junction","z":"t1","x":1,"y":1,"wires":[[]],"ew_inSchema":{"type":"strnig"}},
	 {"id":"b","type":"junction","z":"t1","x":1,"y":1,"wires":[[]],"ew_outSchemas":[{},{}]},
	 {"id":"c","type":"junction","z":"t1","x":1,"y":1,"wires":[[]],"ew_outSchemas":"nope"}]`
	ps := Check(parse(t, doc))
	if len(ps) != 3 {
		t.Fatalf("%v", ps)
	}
	for _, p := range ps {
		t.Log(p)
	}
	if !strings.Contains(ps[1].Message, "2 entries and the node has 1 output ports") {
		t.Fatalf("%v", ps[1])
	}
}

func TestDisabledNodesAreNotChecked(t *testing.T) {
	doc := strings.Replace(flowWith(tempOut, tempIn), `"name":"limit"`, `"name":"limit","d":true`, 1)
	if ps := Check(parse(t, doc)); len(ps) != 0 {
		t.Fatalf("%v", ps)
	}
}

func TestRuntimeChecksIgnoreBookkeepingKeys(t *testing.T) {
	n := parse(t, `[{"id":"t1","type":"tab","label":"T"},
	 {"id":"a","type":"junction","z":"t1","x":1,"y":1,"wires":[[]],"ew_inSchema":{"type":"object","additionalProperties":false,"properties":{"payload":{"type":"number"}}}}]`).Nodes["a"]
	p, err := Parse(n)
	if err != nil {
		t.Fatal(err)
	}
	m := engine.NewMsg()
	m.EnsureID()
	m.Data["payload"] = 1.0
	if err := p.CheckIn(m); err != nil {
		t.Fatalf("_msgid should be invisible: %v", err)
	}
	m.Data["payload"] = "x"
	err = p.CheckIn(m)
	if err == nil || !strings.Contains(err.Error(), "payload: got string, want number") {
		t.Fatalf("%v", err)
	}
	if len(Violations(err)) != 1 {
		t.Fatal("no violations on the error")
	}
}

func TestNoSchemasIsNil(t *testing.T) {
	n := parse(t, flowWith(`null`, `null`)).Nodes["a"]
	if p, err := Parse(n); p != nil || err != nil {
		t.Fatalf("%v %v", p, err)
	}
	var p *Ports
	if p.CheckIn(engine.NewMsg()) != nil || p.CheckOut(0, engine.NewMsg()) != nil {
		t.Fatal("nil ports must pass everything")
	}
}

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
