package flowtest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Difference is one place a replay came out different from the original.
type Difference struct {
	Node  string `json:"node"`
	Label string `json:"label"`
	// Kind is "changed" when both ended a message at the node and the
	// messages differ, "missing" when only the original did, and "extra" when
	// only the replay did.
	Kind    string         `json:"kind"`
	Before  map[string]any `json:"before,omitempty"`
	After   map[string]any `json:"after,omitempty"`
	Changes []string       `json:"changes,omitempty"`
}

// ReplayResult is what putting a captured message into a flow again did, next
// to what it did the first time.
type ReplayResult struct {
	// Same is true when every place the original ended is a place the replay
	// ended, with the same message.
	Same        bool         `json:"same"`
	Differences []Difference `json:"differences,omitempty"`
	// Trace is the replay's own journey, in the flow it ran against, for the
	// canvas to light.
	Trace *engine.TraceView `json:"trace,omitempty"`
	// Problems says why it couldn't be run, when it couldn't.
	Problems []Problem `json:"problems,omitempty"`
	Log      []string  `json:"log,omitempty"`
}

// Replay puts the message that started a trace into a flow, usually a draft of
// the one the trace was captured from, and compares where it ends up with where
// it ended up the first time.
//
// It runs the way a flow test does, and for the same reason: against the real
// runtime and nodes with nothing reaching outside the process. A node that
// would have called a web service answers with nothing here, so a flow whose
// output depends on an answer shows that as a difference.
func Replay(ctx context.Context, flowsDoc []byte, orig engine.TraceSnapshot, opts Options) ReplayResult {
	if opts.Registry == nil {
		opts.Registry = node.Default
	}
	flows, err := engine.ParseFlows(flowsDoc)
	if err != nil {
		return ReplayResult{Problems: []Problem{{Message: fmt.Sprintf("the flow file doesn't parse: %v", err)}}}
	}
	g := newGraphView(flows, opts.Registry)
	inj, _, err := injectionFor(g, orig)
	if err != nil {
		return ReplayResult{Problems: []Problem{{Message: err.Error()}}}
	}

	tracer := engine.NewTracer(engine.TraceLimits{Traces: 64})
	opts.Tracer = tracer
	res := runCase(ctx, flowsDoc, &Case{Name: "replay", Inject: []Injection{inj}}, opts)
	out := ReplayResult{Log: res.Log}
	if res.Status == Error {
		out.Problems = res.Problems
		return out
	}

	var replay *engine.Trace
	traces := tracer.Traces()
	for i := len(traces) - 1; i >= 0; i-- {
		snap := traces[i].Snapshot()
		if len(snap.Hops) > 0 && snap.Hops[0].Node == inj.Node {
			replay = traces[i]
			break
		}
	}
	if replay == nil {
		out.Problems = []Problem{{Message: "the message went in but nothing handled it"}}
		return out
	}
	snap := replay.Snapshot()
	v := snap.View()
	out.Trace = &v
	out.Differences = compareEnds(g, orig, snap)
	out.Same = len(out.Differences) == 0
	return out
}

// compareEnds sets the places the original message ended against the places the
// replay's did, node by node, in the order they happened.
func compareEnds(g *graphView, orig, replay engine.TraceSnapshot) []Difference {
	byNode := func(s engine.TraceSnapshot) (map[string][]map[string]any, []string) {
		m := map[string][]map[string]any{}
		var order []string
		for _, h := range s.Ends() {
			if _, seen := m[h.Node]; !seen {
				order = append(order, h.Node)
			}
			m[h.Node] = append(m[h.Node], normal(withoutID(h.Msg)))
		}
		return m, order
	}
	before, order := byNode(orig)
	after, orderAfter := byNode(replay)
	for _, id := range orderAfter {
		if _, ok := before[id]; !ok {
			order = append(order, id)
		}
	}

	var diffs []Difference
	for _, id := range order {
		a, b := before[id], after[id]
		for i := 0; i < max(len(a), len(b)); i++ {
			d := Difference{Node: id, Label: g.label(id)}
			switch {
			case i >= len(b):
				d.Kind, d.Before = "missing", a[i]
			case i >= len(a):
				d.Kind, d.After = "extra", b[i]
			default:
				d.Changes = diffValues("", a[i], b[i])
				if len(d.Changes) == 0 {
					continue
				}
				d.Kind, d.Before, d.After = "changed", a[i], b[i]
			}
			diffs = append(diffs, d)
		}
	}
	return diffs
}

// normal puts a message through JSON, so a number that was an int on one side
// and a float on the other, or bytes that are text, compare as what they are.
func normal(m map[string]any) map[string]any {
	b, err := json.Marshal(engine.JSONSafe(m))
	if err != nil {
		return m
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return m
	}
	return out
}

// diffValues lists the properties two values differ at, one line each.
func diffValues(path string, a, b any) []string {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		var out []string
		for _, k := range names {
			sub := k
			if path != "" {
				sub = path + "." + k
			}
			av, inA := am[k]
			bv, inB := bm[k]
			switch {
			case !inB:
				out = append(out, fmt.Sprintf("%s: %s -> (gone)", sub, clip(show(av), 60)))
			case !inA:
				out = append(out, fmt.Sprintf("%s: (not there) -> %s", sub, clip(show(bv), 60)))
			default:
				out = append(out, diffValues(sub, av, bv)...)
			}
		}
		return out
	}
	al, aok := a.([]any)
	bl, bok := b.([]any)
	if aok && bok && len(al) == len(bl) {
		var out []string
		for i := range al {
			out = append(out, diffValues(fmt.Sprintf("%s[%d]", path, i), al[i], bl[i])...)
		}
		return out
	}
	if show(a) == show(b) {
		return nil
	}
	if path == "" {
		path = "(message)"
	}
	return []string{fmt.Sprintf("%s: %s -> %s", path, clip(strings.TrimSpace(show(a)), 60), clip(strings.TrimSpace(show(b)), 60))}
}
