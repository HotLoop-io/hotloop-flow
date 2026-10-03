package flowdiff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

func parse(t testing.TB, doc string) *engine.Flows {
	t.Helper()
	f, err := engine.ParseFlows([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	return f
}

// The line every switch node on a pressure line has.
const base = `[
{"id":"t1","type":"tab","label":"Line 3"},
{"id":"in1","type":"mqtt in","z":"t1","name":"press 01","topic":"press/01","x":100,"y":80,"wires":[["sw1"]]},
{"id":"sw1","type":"switch","z":"t1","name":"Pressure check","property":"payload","rules":[{"t":"lt","v":"7.5","vt":"num"},{"t":"gte","v":"7.5","vt":"num"}],"x":300,"y":80,"wires":[["ok1"],["alarm1"]]},
{"id":"ok1","type":"debug","z":"t1","name":"ok","x":500,"y":60,"wires":[]},
{"id":"alarm1","type":"change","z":"t1","name":"set alarm","rules":[{"t":"set","p":"alarm","pt":"msg","to":"true","tot":"bool"}],"x":500,"y":120,"wires":[[]]},
{"id":"b1","type":"mqtt-broker","name":"plant broker","broker":"10.0.0.5","port":"1883"}
]`

// Each case is the old document, an edit to it, and the text a reviewer
// should read. The text is the contract: if it changes, it changes here,
// where somebody has to look at it.
var cases = []struct {
	name     string
	edit     func(string) string
	want     string
	logic    bool
	isLayout bool
}{
	{
		name: "a threshold change",
		edit: func(s string) string {
			return strings.Replace(s, `{"t":"gte","v":"7.5"`, `{"t":"gte","v":"8.0"`, 1)
		},
		want: `1 changed

Tab "Line 3" (t1)
  ~ switch "Pressure check" (sw1)
      rules[1].v: "7.5" -> "8.0"
`,
		logic: true,
	},
	{
		name: "dragging two nodes is layout, never logic",
		edit: func(s string) string {
			s = strings.Replace(s, `"x":300,"y":80`, `"x":320,"y":100`, 1)
			return strings.Replace(s, `"name":"ok","x":500,"y":60`, `"name":"ok","x":520,"y":60`, 1)
		},
		want: `2 moved
Layout only. Nothing about how the flows run changed.

Moved on the canvas only
  switch "Pressure check" (sw1)
  debug "ok" (ok1)
`,
		isLayout: true,
	},
	{
		name: "a rewire",
		edit: func(s string) string {
			return strings.Replace(s, `"wires":[["ok1"],["alarm1"]]`, `"wires":[["ok1","alarm1"],[]]`, 1)
		},
		want: `1 changed

Tab "Line 3" (t1)
  ~ switch "Pressure check" (sw1)
      wire added: port 1 -> change "set alarm" (alarm1)
      wire removed: port 2 -> change "set alarm" (alarm1)
`,
		logic: true,
	},
	{
		name: "a change that also moved says both, and counts as a change",
		edit: func(s string) string {
			return strings.Replace(s, `"topic":"press/01","x":100`, `"topic":"press/02","x":140`, 1)
		},
		want: `1 changed

Tab "Line 3" (t1)
  ~ mqtt in "press 01" (in1)
      topic: "press/01" -> "press/02"
      and moved on the canvas
`,
		logic: true,
	},
	{
		name: "a node type change",
		edit: func(s string) string {
			return strings.Replace(s, `"id":"ok1","type":"debug"`, `"id":"ok1","type":"complete"`, 1)
		},
		want: `1 changed

Tab "Line 3" (t1)
  ~ complete "ok" (ok1)
      type: "debug" -> "complete"
`,
		logic: true,
	},
	{
		name: "added and removed",
		edit: func(s string) string {
			s = strings.Replace(s, `{"id":"ok1","type":"debug","z":"t1","name":"ok","x":500,"y":60,"wires":[]},`, "", 1)
			s = strings.Replace(s, `"wires":[["ok1"],["alarm1"]]`, `"wires":[[],["alarm1"]]`, 1)
			return strings.Replace(s, `{"id":"b1"`, `{"id":"log1","type":"file","z":"t1","name":"audit file","filename":"/data/audit.log","x":700,"y":120,"wires":[]},
{"id":"b1"`, 1)
		},
		want: `1 added, 1 removed, 1 changed

Tab "Line 3" (t1)
  ~ switch "Pressure check" (sw1)
      wire removed: port 1 -> debug "ok" (ok1)
  + file "audit file" (log1)
  - debug "ok" (ok1)
`,
		logic: true,
	},
	{
		name: "a credential change says which field, never the value",
		edit: func(s string) string {
			return strings.Replace(s, `"port":"1883"}`, `"port":"1883","credentials":{"user":"line3","password":"hunter2-but-longer"}}`, 1)
		},
		want: `1 changed

Tabs, subflows and config nodes
  ~ mqtt-broker "plant broker" (b1)
      credentials.password: set
      credentials.user: set
`,
		logic: true,
	},
	{
		name: "a credential field the editor didn't touch is not a change",
		edit: func(s string) string {
			return strings.Replace(s, `"port":"1883"}`, `"port":"1883","credentials":{"password":"__PWRD__"}}`, 1)
		},
		want: "No changes.\n",
	},
	{
		name: "a disabled node is a logic change",
		edit: func(s string) string {
			return strings.Replace(s, `"id":"alarm1","type":"change","z":"t1",`, `"id":"alarm1","type":"change","z":"t1","d":true,`, 1)
		},
		want: `1 changed

Tab "Line 3" (t1)
  ~ change "set alarm" (alarm1)
      d: added true
`,
		logic: true,
	},
	{
		name: "a key that isn't an identifier is quoted",
		edit: func(s string) string {
			return strings.Replace(s, `"port":"1883"}`, `"port":"1883","x-vendor key":{"a":1}}`, 1)
		},
		want: `1 changed

Tabs, subflows and config nodes
  ~ mqtt-broker "plant broker" (b1)
      x-vendor key: added {"a":1}
`,
		logic: true,
	},
}

func TestDiffReadsLikeAReview(t *testing.T) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			oldF, newF := parse(t, base), parse(t, c.edit(base))
			res := Diff(oldF, newF)
			if got := Text(res, oldF, newF); got != c.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, c.want)
			}
			if res.LogicChanged() != c.logic {
				t.Errorf("LogicChanged = %v, want %v", res.LogicChanged(), c.logic)
			}
			if c.isLayout && (res.Empty() || res.LogicChanged()) {
				t.Errorf("a layout-only edit came out as %+v", res.Summary)
			}
		})
	}
}

// Secrets never leave in the structured form either.
func TestCredentialValuesNeverAppear(t *testing.T) {
	newDoc := strings.Replace(base, `"port":"1883"}`, `"port":"1883","credentials":{"password":"hunter2-but-longer"}}`, 1)
	res := Diff(parse(t, base), parse(t, newDoc))
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hunter2") {
		t.Fatalf("the diff carries the password: %s", out)
	}
	if !strings.Contains(string(out), `"secret":true`) {
		t.Fatalf("the credential change isn't marked secret: %s", out)
	}
}

// Subflows: an edit inside the template is a change on the subflow, and
// moving one of its ports is layout.
func TestSubflowTemplateEdits(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "flows_kitchen_sink.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldDoc := string(data)
	newDoc := strings.Replace(oldDoc, `"to": "payload", "tot": "msg"`, `"to": "payload * 2", "tot": "msg"`, 1)
	newDoc = strings.Replace(newDoc, `{ "x": 400, "y": 60, "wires": [{ "id": "1111222233334444", "port": 0 }] }`,
		`{ "x": 420, "y": 60, "wires": [{ "id": "1111222233334444", "port": 0 }] }`, 1)
	if newDoc == oldDoc {
		t.Fatal("the edits didn't apply to the test file")
	}
	oldF, newF := parse(t, oldDoc), parse(t, newDoc)
	res := Diff(oldF, newF)
	got := Text(res, oldF, newF)
	want := `1 changed, 1 moved

Subflow "Scale Reading" (5f6e7d8c9b0a1234)
  ~ change "scale" (1111222233334444)
      rules[0].to: "payload" -> "payload * 2"

Moved on the canvas only
  subflow "Scale Reading" (5f6e7d8c9b0a1234)
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	// Rewiring a port is not layout.
	rewired := strings.Replace(oldDoc, `"in": [
            { "x": 40, "y": 60, "wires": [{ "id": "1111222233334444" }] }`, `"in": [
            { "x": 40, "y": 60, "wires": [] }`, 1)
	if rewired == oldDoc {
		t.Fatal("the rewire didn't apply")
	}
	if res := Diff(oldF, parse(t, rewired)); !res.LogicChanged() {
		t.Fatalf("rewiring a subflow input came out as %+v", res.Summary)
	}
}

func TestDiffAgainstNothing(t *testing.T) {
	f := parse(t, base)
	if res := Diff(nil, f); res.Summary.Added != 6 || res.LogicChanged() == false {
		t.Fatalf("everything from nothing = %+v", res.Summary)
	}
	if res := Diff(f, nil); res.Summary.Removed != 6 {
		t.Fatalf("everything to nothing = %+v", res.Summary)
	}
	if res := Diff(nil, nil); !res.Empty() {
		t.Fatal("nothing against nothing differs")
	}
}

// The property that has to hold for every document anybody will ever have: a
// flow compared with itself is no change at all.
func FuzzDiffOfAFlowWithItselfIsEmpty(f *testing.F) {
	for _, name := range []string{"flows_kitchen_sink.json", "flows_nodered_format.json"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(base))
	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := engine.ParseFlows(data)
		if err != nil {
			return
		}
		b, err := engine.ParseFlows(data)
		if err != nil {
			t.Fatalf("parsed once and not twice: %v", err)
		}
		if res := Diff(a, b); !res.Empty() {
			t.Fatalf("a document differs from itself: %+v", res.Entries)
		}
		// And the text never panics on whatever the fuzzer built.
		_ = Text(Diff(nil, a), nil, a)
	})
}

func TestDiffWithItselfOnTheRealFiles(t *testing.T) {
	for _, name := range []string{"flows_kitchen_sink.json", "flows_nodered_format.json"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if res := Diff(parse(t, string(data)), parse(t, string(data))); !res.Empty() {
			t.Errorf("%s differs from itself: %+v", name, res.Entries)
		}
	}
}
