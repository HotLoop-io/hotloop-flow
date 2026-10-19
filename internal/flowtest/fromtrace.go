package flowtest

import (
	"errors"
	"fmt"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// ErrNoEnd means the trace never reached a place a test can watch.
var ErrNoEnd = errors.New("that message never reached the end of anything a test can watch: " +
	"a test checks what a node with no outputs, like a Debug or an MQTT Out, was handed")

// injectionFor works out what has to go into a flow to start the same journey
// the trace made, and checks the node it started at is one a test can put a
// message into.
func injectionFor(g *graphView, snap engine.TraceSnapshot) (Injection, *engine.Node, error) {
	if len(snap.Hops) == 0 || snap.Hops[0].Entry == "" {
		return Injection{}, nil, errors.New("that trace is empty")
	}
	root := snap.Hops[0]
	src, ok := g.flows.Nodes[root.Node]
	if !ok {
		if _, _, derived := engine.SplitDerivedID(root.Node); derived {
			return Injection{}, nil, fmt.Errorf("it started inside a subflow, at %s; a test puts messages into the instance instead", root.Node)
		}
		return Injection{}, nil, fmt.Errorf("it started at %s, which isn't in these flows", root.Node)
	}
	if root.Cut {
		return Injection{}, nil, fmt.Errorf("the message that started it was too big to keep, so it can't be sent again")
	}
	inj := Injection{Node: src.ID}
	data, _ := plain(withoutID(root.Msg)).(map[string]any)
	switch root.Entry {
	case "emit":
		if g.inputs(src) > 0 {
			return Injection{}, nil, fmt.Errorf("%s has an input, and it was a message it made on its own that started this; "+
				"a test can only send one in from outside", nodeName(src))
		}
		for _, h := range snap.Hops {
			if len(h.Parents) > 0 && h.Parents[0] == root.ID && h.Port > 0 {
				return Injection{}, nil, fmt.Errorf("it came in from output %d of %s, and a test puts a message in at a source's first output",
					h.Port+1, nodeName(src))
			}
		}
		inj.Msg = data
	default:
		if g.inputs(src) > 0 {
			inj.Msg = data
		}
	}
	return inj, src, nil
}

// FromTrace turns a captured trace into a test: the message that started it,
// put in again, and what each place it ended at was handed, expected there. It
// is what the flow did, pinned, so a change that makes it do something else
// fails. A trace is better than one Debug message for this because it
// remembers every place the message ended: through a split, every piece.
func FromTrace(flowsDoc []byte, snap engine.TraceSnapshot) (Case, error) {
	flows, err := engine.ParseFlows(flowsDoc)
	if err != nil {
		return Case{}, err
	}
	g := newGraphView(flows, node.Default)
	inj, src, err := injectionFor(g, snap)
	if err != nil {
		return Case{}, err
	}

	var expect []Expectation
	var first *engine.Node
	var firstShown map[string]any
	for _, h := range snap.Ends() {
		n, ok := flows.Nodes[h.Node]
		if !ok || g.outputs(n) != 0 || g.runs(n) != nil {
			continue
		}
		if h.Cut {
			return Case{}, fmt.Errorf("the message that reached %s was too big to keep", nodeName(n))
		}
		var shown map[string]any
		if n.Type == "debug" {
			if shown, err = shownPart(n, withoutID(h.Msg)); err != nil {
				return Case{}, err
			}
		} else {
			shown, _ = plain(withoutID(h.Msg)).(map[string]any)
		}
		if first == nil {
			first, firstShown = n, shown
		}
		expect = append(expect, Expectation{Node: n.ID, Msg: shown})
	}
	if len(expect) == 0 {
		return Case{}, ErrNoEnd
	}
	name := fmt.Sprintf("%s gets %s from %s", nodeName(first), describeShown(firstShown), nodeName(src))
	if len(expect) > 1 {
		name = fmt.Sprintf("%s sends %d messages on, first %s at %s", nodeName(src), len(expect),
			describeShown(firstShown), nodeName(first))
	}
	return Case{Name: name, Inject: []Injection{inj}, Expect: expect}, nil
}
