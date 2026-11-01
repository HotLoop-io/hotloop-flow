package diagram

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// A builder for flow files in tests, so a diagram reads as the wires it has.
type flowBuilder struct {
	t       *testing.T
	entries []map[string]any
	byID    map[string]map[string]any
}

func newFlow(t *testing.T) *flowBuilder {
	return &flowBuilder{t: t, byID: map[string]map[string]any{}}
}

func (b *flowBuilder) tab(id string, settings map[string]any) *flowBuilder {
	if settings == nil {
		settings = map[string]any{}
	}
	e := map[string]any{"id": id, "type": "tab", "label": id, engine.DiagramKey: settings}
	b.entries = append(b.entries, e)
	b.byID[id] = e
	return b
}

func (b *flowBuilder) node(tab, id, typ string, props map[string]any) *flowBuilder {
	e := map[string]any{"id": id, "type": typ, "z": tab, "x": 100.0, "y": 100.0, "wires": []any{}}
	for k, v := range props {
		e[k] = v
	}
	b.entries = append(b.entries, e)
	b.byID[id] = e
	return b
}

// wire connects "node.outport" to "node.inport".
func (b *flowBuilder) wire(from, to string) *flowBuilder {
	fn, fp := split(b.t, from)
	tn, tp := split(b.t, to)
	src, ok := b.byID[fn]
	if !ok {
		b.t.Fatalf("no node %s", fn)
	}
	d, _ := node.Default.Lookup(src["type"].(string))
	outs := OutPorts(d.Descriptor, src)
	oi := -1
	for i, p := range outs {
		if p.Name == fp {
			oi = i
		}
	}
	if oi < 0 {
		b.t.Fatalf("%s has no output %s (has %v)", fn, fp, outs)
	}
	wires, _ := src["wires"].([]any)
	for len(wires) < len(outs) {
		wires = append(wires, []any{})
	}
	ins, _ := src[InKey].([]any)
	for len(ins) < len(outs) {
		ins = append(ins, []any{})
	}
	wires[oi] = append(wires[oi].([]any), tn)
	ins[oi] = append(ins[oi].([]any), tp)
	src["wires"], src[InKey] = wires, ins
	return b
}

func split(t *testing.T, s string) (string, string) {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		t.Fatalf("%q is not node.port", s)
	}
	return s[:i], s[i+1:]
}

func (b *flowBuilder) flows() *engine.Flows {
	raw, err := json.Marshal(b.entries)
	if err != nil {
		b.t.Fatal(err)
	}
	f, err := engine.ParseFlows(raw)
	if err != nil {
		b.t.Fatal(err)
	}
	return f
}

func (b *flowBuilder) manager() *Manager {
	m := NewManager(nil)
	b.t.Cleanup(m.Close)
	m.Apply(b.flows())
	return m
}

func (b *flowBuilder) run(tab string) (*Run, error) {
	m := b.manager()
	r, err := m.Start(tab, StartOptions{})
	if err != nil {
		return nil, err
	}
	<-r.Done()
	return r, nil
}

func mustProblemFree(t *testing.T, m *Manager, tab string) {
	t.Helper()
	d, ok := m.Diagram(tab)
	if !ok {
		t.Fatalf("no diagram %s", tab)
	}
	if len(d.Problems) > 0 {
		t.Fatalf("diagram %s has problems: %v", tab, d.Problems)
	}
}

func lastOn(t *testing.T, r *Run, key string) any {
	t.Helper()
	w, ok := r.WireValues()[key]
	if !ok {
		keys := []string{}
		for k := range r.WireValues() {
			keys = append(keys, k)
		}
		t.Fatalf("no value on wire %s; wires: %v", key, keys)
	}
	return w.Last
}

func f(v float64) float64 { return v }

var _ = fmt.Sprint
