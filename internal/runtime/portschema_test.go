package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/metrics"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// portSchemaFlow wires p (output 1 promises a numeric payload.temp) to k (its
// input wants the same, plus a topic). A Catch node on the tab collects errors.
const portSchemaFlow = `[
  {"id":"t1","type":"tab","label":"T"},
  {"id":"p","type":"pass","z":"t1","x":1,"y":1,"wires":[["k"]],
   "ew_outSchemas":[{"type":"object","properties":{"payload":{"type":"object","required":["temp"],"properties":{"temp":{"type":"number"}}}}}]},
  {"id":"k","type":"sink","z":"t1","x":2,"y":1,"wires":[],
   "ew_inSchema":{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}}}},
  {"id":"c","type":"catch","z":"t1","x":1,"y":3,"wires":[["out"]]},
  {"id":"out","type":"caught","z":"t1","x":2,"y":3,"wires":[]}
]`

type portHarness struct {
	rt     *Runtime
	sink   *sinkNode
	caught *sinkNode
}

func startPortFlow(t *testing.T, flowsJSON string) *portHarness {
	t.Helper()
	sink, caught := &sinkNode{}, &sinkNode{}
	tr := newTestRegistry()
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("caught", 1, 0, func(string) node.Node { return caught })
	rt := New(tr.Registry, mustFlows(t, flowsJSON), Options{})
	drain(rt)
	// Start reports the nodes that would not build; a test that cares reads them
	// from the graph.
	_ = rt.Start(context.Background())
	t.Cleanup(func() { rt.Stop(context.Background()) })
	return &portHarness{rt: rt, sink: sink, caught: caught}
}

func (h *portHarness) inject(t *testing.T, id string, data map[string]any) {
	t.Helper()
	if err := h.rt.Inject(id, engine.WrapMsg(data)); err != nil {
		t.Fatal(err)
	}
}

func (h *portHarness) mismatches(id string) int64 {
	for _, s := range h.rt.Snapshots() {
		if s.NodeID == id {
			return s.SchemaMismatches
		}
	}
	return -1
}

func TestAMessageThatMatchesBothEndsFlowsThrough(t *testing.T) {
	h := startPortFlow(t, portSchemaFlow)
	h.inject(t, "p", map[string]any{"topic": "line3", "payload": map[string]any{"temp": 21.5}})
	waitFor(t, "the message to arrive", func() bool { return h.sink.count() == 1 })
	if h.caught.count() != 0 || h.mismatches("p") != 0 || h.mismatches("k") != 0 {
		t.Fatalf("caught %d, mismatches p=%d k=%d", h.caught.count(), h.mismatches("p"), h.mismatches("k"))
	}
}

func TestABadOutputIsStoppedAtTheSenderAndCaughtWithThePath(t *testing.T) {
	h := startPortFlow(t, portSchemaFlow)
	h.inject(t, "p", map[string]any{"topic": "line3", "payload": map[string]any{"temp": "hot"}})
	waitFor(t, "the error to reach Catch", func() bool { return h.caught.count() == 1 })

	m := h.caught.all()[0]
	text, _, _ := m.Get("error.message")
	want := "the message does not match the schema of output 1: payload.temp: got string, want number"
	if fmt.Sprint(text) != want {
		t.Fatalf("error.message\n got %v\nwant %s", text, want)
	}
	src, _, _ := m.Get("error.source.id")
	if src != "p" {
		t.Fatalf("source %v", src)
	}
	v, _, _ := m.Get("error.violations")
	list, _ := v.([]any)
	if len(list) != 1 || list[0].(map[string]any)["path"] != "payload.temp" {
		t.Fatalf("violations %v", v)
	}
	if h.sink.count() != 0 {
		t.Fatal("a message that breaks its output schema must not be delivered")
	}
	if got := h.mismatches("p"); got != 1 {
		t.Fatalf("p mismatches %d", got)
	}

	var b strings.Builder
	mh := metrics.NewHandler("test", func() []metrics.NodeStat {
		var out []metrics.NodeStat
		for _, s := range h.rt.Snapshots() {
			out = append(out, metrics.NodeStat{NodeID: s.NodeID, Type: s.Type, SchemaMismatches: s.SchemaMismatches})
		}
		return out
	})
	mh.Write(&b)
	if !strings.Contains(b.String(), `hotloop_flow_node_schema_mismatches_total{node="p",type="pass"} 1`) {
		t.Fatalf("metric missing:\n%s", b.String())
	}
}

func TestABadInputIsRefusedBeforeTheNodeSeesIt(t *testing.T) {
	h := startPortFlow(t, portSchemaFlow)
	// Fine on the way out of p, but the sink wants a topic.
	h.inject(t, "p", map[string]any{"payload": map[string]any{"temp": 1.0}})
	waitFor(t, "the error to reach Catch", func() bool { return h.caught.count() == 1 })

	text, _, _ := h.caught.all()[0].Get("error.message")
	if !strings.Contains(fmt.Sprint(text), `input schema: topic: missing property "topic"`) {
		t.Fatalf("error.message %v", text)
	}
	src, _, _ := h.caught.all()[0].Get("error.source.id")
	if src != "k" || h.sink.count() != 0 || h.mismatches("k") != 1 {
		t.Fatalf("source %v, sink %d, mismatches %d", src, h.sink.count(), h.mismatches("k"))
	}
}

func TestNodesWithoutSchemasAreUnchanged(t *testing.T) {
	plain := strings.NewReplacer(
		`"ew_outSchemas":[{"type":"object","properties":{"payload":{"type":"object","required":["temp"],"properties":{"temp":{"type":"number"}}}}}]`, `"x2":1`,
		`"ew_inSchema":{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}}}`, `"x3":1`,
	).Replace(portSchemaFlow)
	h := startPortFlow(t, plain)
	h.inject(t, "p", map[string]any{"payload": "anything at all"})
	waitFor(t, "the message to arrive", func() bool { return h.sink.count() == 1 })
	if h.caught.count() != 0 || h.mismatches("p") != 0 {
		t.Fatal("no schemas, no checks")
	}
}

func TestASchemaThatDoesNotCompileFailsOnlyThatNode(t *testing.T) {
	broken := strings.Replace(portSchemaFlow, `"ew_inSchema":{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}}}`, `"ew_inSchema":{"type":"strnig"}`, 1)
	h := startPortFlow(t, broken)
	failed := h.rt.graph().failureList()
	if len(failed) != 1 || failed[0].NodeID != "k" || !strings.Contains(failed[0].Err.Error(), "ew_inSchema") {
		t.Fatalf("failures %v", failed)
	}
}
