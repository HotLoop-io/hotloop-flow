package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// useRegistry builds a node with one config reference, so the check has something to
// read besides a real node's descriptor.
func useRegistry(t *testing.T) *testRegistry {
	t.Helper()
	tr := newTestRegistry()
	d := node.Descriptor{
		Type: "use", Category: node.CategoryCommon, Color: "#E31837", Icon: "cog",
		Inputs: 1, Outputs: 0, Compatibility: node.Compatibility{Level: node.CompatOnly},
		Props: []node.Prop{{Name: "server", Kind: node.PropConfigRef, ConfigType: "thing-server"}},
	}
	if err := tr.Register(d, func(*node.Definition) (node.Node, error) { return passNode{}, nil }); err != nil {
		t.Fatal(err)
	}
	cd := node.Descriptor{
		Type: "thing-server", Category: node.CategoryCommon, Color: "#E31837", Icon: "cog",
		IsConfig: true, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}
	if err := tr.Register(cd, func(*node.Definition) (node.Node, error) { return passNode{}, nil }); err != nil {
		t.Fatal(err)
	}
	od := node.Descriptor{
		Type: "other-server", Category: node.CategoryCommon, Color: "#E31837", Icon: "cog",
		IsConfig: true, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}
	if err := tr.Register(od, func(*node.Definition) (node.Node, error) { return passNode{}, nil }); err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestOnlyAnIdThatNamesNothingIsADanglingConfigReference(t *testing.T) {
	tr := useRegistry(t)
	f := mustFlows(t, `[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"srv","type":"thing-server","name":"S"},
	  {"id":"ok","type":"use","z":"t1","server":"srv","wires":[]},
	  {"id":"unset","type":"use","z":"t1","server":"","wires":[]},
	  {"id":"adding","type":"use","z":"t1","server":"_ADD_","wires":[]},
	  {"id":"legacy","type":"use","z":"t1","server":"true","wires":[]},
	  {"id":"off","type":"use","z":"t1","d":true,"server":"gone-1","wires":[]},
	  {"id":"bad","type":"use","z":"t1","name":"the bad one","server":"gone-2","wires":[]}
	]`)
	refs := MissingConfigRefs(tr.Registry, f)
	if len(refs) != 1 || refs[0].NodeID != "bad" || refs[0].Target != "gone-2" {
		t.Fatalf("refs = %+v, want only the enabled node whose id names nothing", refs)
	}
	if w := ConfigRefWarnings(tr.Registry, f); len(w) != 1 || !strings.Contains(w[0], "use bad (the bad one)") {
		t.Fatalf("warnings = %q", w)
	}
}

func TestANodeWithADanglingConfigReferenceIsAStartFailureAndTheRestRuns(t *testing.T) {
	tr := useRegistry(t)
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	rt := New(tr.Registry, mustFlows(t, `[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"bad","type":"use","z":"t1","server":"gone","wires":[]},
	  {"id":"p","type":"pass","z":"t1","wires":[]}
	]`), Options{})
	defer rt.Stop(context.Background())
	fails := rt.Start(context.Background())
	if len(fails) != 1 || fails[0].NodeID != "bad" ||
		!strings.Contains(fails[0].Err.Error(), `"gone"`) || !strings.Contains(fails[0].Err.Error(), "no node with that id") {
		t.Fatalf("failures = %v", fails)
	}
	if err := rt.Inject("p", engine.NewMsgWithPayload(1.0)); err != nil {
		t.Fatalf("the node next to it stopped taking messages: %v", err)
	}
}

// A reference that names something in the file is not fine just because the id
// exists. The editor only offers config nodes of the right type, so these come
// from a hand edit, a pasted id or an import, and they are as broken as an id
// that names nothing.
const wrongRefFlow = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"sf","type":"subflow","name":"S","in":[],"out":[]},
  {"id":"g1","type":"group","z":"t1","name":"G","nodes":[]},
  {"id":"srv","type":"thing-server","name":"S"},
  {"id":"other","type":"other-server","name":"O"},
  {"id":"p","type":"pass","z":"t1","wires":[]},
  {"id":"ok","type":"use","z":"t1","server":"srv","wires":[]},
  {"id":"plain","type":"use","z":"t1","server":"p","wires":[]},
  {"id":"kind","type":"use","z":"t1","server":"other","wires":[]},
  {"id":"tab","type":"use","z":"t1","server":"t1","wires":[]},
  {"id":"sub","type":"use","z":"t1","server":"sf","wires":[]},
  {"id":"grp","type":"use","z":"t1","server":"g1","wires":[]},
  {"id":"gone","type":"use","z":"t1","server":"nope","wires":[]}
]`

func TestAReferenceToTheWrongKindOfNodeIsReportedLikeAMissingOne(t *testing.T) {
	tr := useRegistry(t)
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	f := mustFlows(t, wrongRefFlow)

	got := map[string]ConfigRef{}
	for _, r := range MissingConfigRefs(tr.Registry, f) {
		got[r.NodeID] = r
	}
	want := map[string]struct {
		why   RefProblem
		found string
		text  string
	}{
		"plain": {RefNotConfig, "pass", `"p", which is a pass and not a config node`},
		"kind":  {RefWrongType, "other-server", `"other", which is a other-server config node, and that setting takes a thing-server config node`},
		"tab":   {RefNotConfig, "tab", `which is a tab and not a config node`},
		"sub":   {RefNotConfig, "subflow", `which is a subflow and not a config node`},
		"grp":   {RefNotConfig, "group", `which is a group and not a config node`},
		"gone":  {RefMissing, "", "no node with that id is in the file"},
	}
	if len(got) != len(want) {
		t.Fatalf("reported %d nodes, want %d: %+v", len(got), len(want), got)
	}
	for id, w := range want {
		r, ok := got[id]
		if !ok {
			t.Errorf("%s was not reported", id)
			continue
		}
		if r.Why != w.why || r.Found != w.found || !strings.Contains(r.Problem(), w.text) {
			t.Errorf("%s: why=%v found=%q problem=%q", id, r.Why, r.Found, r.Problem())
		}
	}
	if _, bad := got["ok"]; bad {
		t.Error("a node pointing at the right config node was reported")
	}
	if w := ConfigRefWarnings(tr.Registry, f); len(w) != len(want) {
		t.Errorf("%d warnings, want %d: %q", len(w), len(want), w)
	}
}

func TestANodeWhoseConfigReferenceIsTheWrongKindDoesNotStart(t *testing.T) {
	tr := useRegistry(t)
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	rt := New(tr.Registry, mustFlows(t, wrongRefFlow), Options{})
	defer rt.Stop(context.Background())
	fails := map[string]string{}
	for _, f := range rt.Start(context.Background()) {
		fails[f.NodeID] = f.Err.Error()
	}
	for _, id := range []string{"plain", "kind", "tab", "sub", "grp", "gone"} {
		msg, ok := fails[id]
		if !ok {
			t.Errorf("%s started", id)
			continue
		}
		if !strings.Contains(msg, "so it did not start") || !strings.Contains(msg, "pick a thing-server") {
			t.Errorf("%s: %q does not say what to do", id, msg)
		}
	}
	if _, bad := fails["ok"]; bad {
		t.Errorf("the node with a good reference failed: %s", fails["ok"])
	}
	if err := rt.Inject("p", engine.NewMsgWithPayload(1.0)); err != nil {
		t.Fatalf("the node next to them stopped taking messages: %v", err)
	}
}
