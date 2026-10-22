package wiretype

import (
	"strings"
	"testing"
)

const limitIn = `{"type":"object","properties":{"payload":{"type":"number","minimum":0,"maximum":100},"topic":{"type":"string"}},"additionalProperties":false}`

func injectFlow(props, in string) string {
	return `[{"id":"t1","type":"tab","label":"T"},
	 {"id":"i1","type":"inject","z":"t1","name":"reading","props":` + props + `,"x":1,"y":1,"wires":[["l1"]]},
	 {"id":"l1","type":"junction","z":"t1","name":"limit","x":2,"y":1,"wires":[[]],"ew_inSchema":` + in + `}]`
}

func problemsFor(t *testing.T, props, in string) []string {
	t.Helper()
	var out []string
	for _, p := range Check(parse(t, injectFlow(props, in))) {
		out = append(out, p.String())
	}
	return out
}

func TestAClashingLiteralIsRefused(t *testing.T) {
	got := problemsFor(t, `[{"p":"payload","v":"hot","vt":"str"},{"p":"topic","v":"line3","vt":"str"}]`, limitIn)
	want := `inject "reading" (i1) output 1 -> "limit" (l1): payload: the inject sets "hot", which the receiver does not take: got string, want number`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestLiteralKinds(t *testing.T) {
	cases := []struct {
		name, props, wantIn string
	}{
		{"number out of range", `[{"p":"payload","v":"250","vt":"num"}]`, "payload: the inject sets 250"},
		{"boolean", `[{"p":"payload","v":"true","vt":"bool"}]`, "got boolean, want number"},
		{"json object", `[{"p":"payload","v":"{\"a\":1}","vt":"json"}]`, "got object, want number"},
		{"a field a closed schema doesn't have", `[{"p":"extra","v":"1","vt":"num"}]`, "extra: the inject sets it and the receiver's schema allows no such field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := problemsFor(t, c.props, limitIn)
			if len(got) != 1 || !strings.Contains(got[0], c.wantIn) {
				t.Fatalf("got %q, want one containing %q", got, c.wantIn)
			}
		})
	}
}

func TestAMatchingLiteralDeploys(t *testing.T) {
	if got := problemsFor(t, `[{"p":"payload","v":"42.5","vt":"num"},{"p":"topic","v":"line3","vt":"str"}]`, limitIn); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}

func TestDynamicValuesAreNotChecked(t *testing.T) {
	for name, props := range map[string]string{
		"timestamp": `[{"p":"payload","v":"","vt":"date"}]`,
		"env":       `[{"p":"payload","v":"LIMIT","vt":"env"}]`,
		"flow":      `[{"p":"payload","v":"limit","vt":"flow"}]`,
		"global":    `[{"p":"payload","v":"limit","vt":"global"}]`,
		"jsonata":   `[{"p":"payload","v":"\"hot\"","vt":"jsonata"}]`,
		"msg":       `[{"p":"payload","v":"other","vt":"msg"}]`,
		"buffer":    `[{"p":"payload","v":"[1,2]","vt":"bin"}]`,
		"bad json":  `[{"p":"payload","v":"{nope","vt":"json"}]`,
		"bracket":   `[{"p":"payload[0]","v":"x","vt":"str"}]`,
	} {
		if got := problemsFor(t, props, limitIn); len(got) != 0 {
			t.Errorf("%s was checked: %q", name, got)
		}
	}
}

func TestOnlyWhatIsProvableIsRefused(t *testing.T) {
	// A required property the inject doesn't set may come from a dynamic one.
	if got := problemsFor(t, `[{"p":"payload","v":"5","vt":"num"}]`, `{"required":["payload","topic"]}`); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	// A $ref the literal can't be judged against alone is left to run time.
	if got := problemsFor(t, `[{"p":"payload","v":"x","vt":"str"}]`, `{"$defs":{"n":{"type":"number"}},"properties":{"payload":{"$ref":"#/$defs/n"}}}`); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	// An inject that feeds a node with no schema is not judged.
	doc := injectFlow(`[{"p":"payload","v":"x","vt":"str"}]`, `null`)
	if got := Check(parse(t, doc)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// A disabled inject is not judged.
	doc = strings.Replace(injectFlow(`[{"p":"payload","v":"x","vt":"str"}]`, limitIn), `"name":"reading"`, `"name":"reading","d":true`, 1)
	if got := Check(parse(t, doc)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestOldStyleInjectNodes(t *testing.T) {
	doc := `[{"id":"t1","type":"tab","label":"T"},
	 {"id":"i1","type":"inject","z":"t1","name":"old","payload":"hot","payloadType":"str","topic":"t","x":1,"y":1,"wires":[["l1"]]},
	 {"id":"l1","type":"junction","z":"t1","x":2,"y":1,"wires":[[]],"ew_inSchema":` + limitIn + `}]`
	got := Check(parse(t, doc))
	if len(got) != 1 || !strings.Contains(got[0].Message, "payload: the inject sets \"hot\"") {
		t.Fatalf("%v", got)
	}
	// An old inject whose payload is a timestamp, the default, is dynamic.
	doc = strings.Replace(doc, `"payload":"hot","payloadType":"str"`, `"payload":""`, 1)
	if got := Check(parse(t, doc)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestNestedLiteralPaths(t *testing.T) {
	in := `{"properties":{"payload":{"type":"object","properties":{"temp":{"type":"number"}},"additionalProperties":false}}}`
	got := problemsFor(t, `[{"p":"payload.temp","v":"hot","vt":"str"},{"p":"payload.humidity","v":"3","vt":"num"}]`, in)
	if len(got) != 2 ||
		!strings.Contains(got[0], `payload.temp: the inject sets "hot", which the receiver does not take: got string, want number`) ||
		!strings.Contains(got[1], "payload.humidity: the inject sets it and the receiver's schema allows no such field") {
		t.Fatalf("%q", got)
	}
	if got := problemsFor(t, `[{"p":"payload.temp","v":"3","vt":"num"}]`, in); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}
