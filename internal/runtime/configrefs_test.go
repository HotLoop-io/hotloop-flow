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
