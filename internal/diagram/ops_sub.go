package diagram

import (
	"context"
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// A sub-diagram is a diagram used as a node: it has a symbol on the canvas and a
// connector, named inputs and outputs, and runs its body once each time it is
// reached. The body is a diagram of its own whose body-in nodes are the inputs
// and whose body-out nodes are the outputs, the same two ends a loop body has.
//
// An input is required unless its name is marked: "pressure?" is optional, and
// "limit~" is recommended, which runs without it and warns at deploy when it has
// no wire.

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-sub", Color: "#c9d3e0", Icon: "loop", PaletteLabel: "sub-diagram", LabelProp: "body",
			InputPorts:  []string{"@ins", "err?"},
			OutputPorts: []string{"@outs", "err"},
			Props: []node.Prop{
				{Name: "body", Label: "Body (diagram)", Kind: node.PropDiagram, Required: true, Help: "The diagram this runs."},
				prop("ins", "Inputs, comma separated (name? is optional, name~ is recommended)", node.PropString, ""),
				prop("outs", "Outputs, comma separated", node.PropString, ""),
			},
			Help: "A diagram used as a node. Its inputs and outputs are the connector: each input is a body in of that name in the body, each output a body out. An input is required unless its name ends in ? (optional) or ~ (recommended: it runs without it and deploy warns when it is not wired). Select nodes and make a sub-diagram from them and the inputs and outputs are worked out for you.",
		},
		Structure: true,
		Check:     checkSub,
		Warn:      warnSub,
		New: func(raw map[string]any) (Op, error) {
			body := strProp(raw, "body", "")
			if body == "" {
				return nil, fmt.Errorf("a sub-diagram needs a body")
			}
			return &subOp{ins: portNames(strProp(raw, "ins", "")), outs: portNames(strProp(raw, "outs", ""))}, nil
		},
	})
}

// portNames is a connector list without the marks on the names.
func portNames(list string) []string {
	var out []string
	for _, n := range splitList(list) {
		out = append(out, strings.TrimRight(n, "?~"))
	}
	return out
}

type subOp struct {
	ins, outs []string
}

func (o *subOp) Run(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
	r := rc.Run
	bid, _ := r.set.Body(rc.Node.ID)
	body, ok := r.set.Diagrams[bid]
	if !ok {
		return nil, &Fault{Code: CodeBadInput, Source: "the body is not a diagram"}
	}
	boundary := map[string]any{}
	for _, n := range o.ins {
		boundary[n] = in[n]
	}
	outs, err := r.exec(ctx, body, rc.Depth+1, boundary)
	out := map[string]any{}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		e := Error{Status: true, Code: CodeInternal, Source: rc.Node.Type + ": " + err.Error(), Node: rc.Node.ID}
		var f *Fault
		if asFault(err, &f) {
			e.Code = f.Code
		}
		out[PortErr] = e
		return out, nil
	}
	for _, n := range o.outs {
		out[n] = outs[n]
	}
	if e, bad := asError(outs[PortErr]); bad {
		out[PortErr] = e
	}
	return out, nil
}

// checkSub holds the connector to the body: every input has a body in of its name
// to receive it, and every output a body out of its name to send it.
func checkSub(s *Set, _ *Diagram, n *DNode) []Problem {
	bid, _ := s.Body(n.ID)
	body, ok := s.Diagrams[bid]
	if !ok {
		return nil // reported once for the diagram
	}
	in, out := map[string]bool{}, map[string]bool{}
	for _, id := range body.Order {
		b := body.Nodes[id]
		switch b.Type {
		case "dia-body-in":
			in[strProp(b.Raw, "name", "")] = true
		case "dia-body-out":
			out[strProp(b.Raw, "name", "")] = true
		}
	}
	var p []Problem
	for _, name := range portNames(strProp(n.Raw, "ins", "")) {
		if !in[name] {
			p = append(p, Problem{Node: n.ID, Text: fmt.Sprintf("the input %q has no body in of that name in %q", name, body.Label)})
		}
	}
	for _, name := range portNames(strProp(n.Raw, "outs", "")) {
		if !out[name] {
			p = append(p, Problem{Node: n.ID, Text: fmt.Sprintf("the output %q has no body out of that name in %q", name, body.Label)})
		}
	}
	return p
}

// warnSub says when a recommended input has no wire.
func warnSub(_ *Set, d *Diagram, n *DNode) []Problem {
	var w []Problem
	for _, name := range splitList(strProp(n.Raw, "ins", "")) {
		if !strings.HasSuffix(name, "~") {
			continue
		}
		port := strings.TrimSuffix(name, "~")
		if _, wired := d.Into[n.ID][port]; !wired {
			w = append(w, Problem{Node: n.ID, Port: port, Text: "a recommended input has no wire"})
		}
	}
	return w
}
