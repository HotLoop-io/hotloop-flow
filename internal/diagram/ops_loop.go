package diagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The two ends of a body. A body is a diagram of its own that a structure runs
// once for each pass: dia-body-in nodes are where values come in (the pass number
// as "iter", each carried value by its name, and what the structure was handed),
// and dia-body-out nodes are where they go back out. A value goes out under a name:
// "stop" ends a while loop, a carried name is that value's next one, "err" is the
// error the structure reports, and any other name is a result.

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-body-in", Color: "#c9d3e0", Icon: "in", PaletteLabel: "body in", LabelProp: "name",
			OutputPorts: []string{"value"},
			Props:       []node.Prop{prop("name", "Name", node.PropString, "iter")},
			Help:        "Where a value enters a loop body. \"iter\" is the pass number, starting at 0. \"item\" is the current element of an array a for loop was handed. Any other name is a carried value or an input of the loop.",
		},
		New: func(raw map[string]any) (Op, error) {
			name := strProp(raw, "name", "")
			return opFunc(func(_ context.Context, rc *Ctx, _ map[string]any) (map[string]any, error) {
				return map[string]any{"value": rc.Boundary[name]}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-body-out", Color: "#c9d3e0", Icon: "out", PaletteLabel: "body out", LabelProp: "name",
			InputPorts: []string{"value"},
			Props:      []node.Prop{prop("name", "Name", node.PropString, "stop")},
			Help:       "Where a value leaves a loop body. \"stop\" ends a while loop when it is true. A carried value's name sends its next value around. \"err\" is the loop's error out. \"collect\" is gathered into the for loop's array out.",
		},
		New: func(raw map[string]any) (Op, error) {
			name := strProp(raw, "name", "")
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				rc.Emit(name, in["value"])
				return nil, nil
			}), nil
		},
		TakesErrors: true,
	})

	register(structure("dia-while", "while loop", false))
	register(structure("dia-for", "for loop", true))
}

func structure(typ, label string, isFor bool) Kind {
	in := []string{"@carried?", "@ins", "err?"}
	out := []string{"@carried", "@outs", "iterations", "err"}
	help := "Runs its body again and again until the body says stop. The body is a diagram of its own, named in Body. Carried values start from the wire on their input, or from Start values, and come out as they were on the last pass. Always runs at least once."
	props := []node.Prop{
		{Name: "body", Label: "Body (diagram)", Kind: node.PropString, Required: true, Help: "The id of the diagram tab this loop runs."},
		prop("carried", "Carried values, comma separated", node.PropString, ""),
		{Name: "start", Label: "Start values (JSON object)", Kind: node.PropString, Default: "{}", Help: `For example {"sum": 0, "window": []}. A wire on a carried value's input wins.`},
		prop("ins", "Inputs, comma separated", node.PropString, ""),
		prop("outs", "Results, comma separated", node.PropString, ""),
		prop("maxIterations", "Stop after this many passes (0 for no limit)", node.PropNumber, 0),
	}
	if isFor {
		in = append([]string{"n?", "items?"}, in...)
		out = []string{"@carried", "@outs", "collected", "iterations", "err"}
		help = "Runs its body a counted number of times: N passes, or one pass for each element of the array on Items. The body's \"collect\" comes out as an array. With a count of zero it returns the start values. The body may also say stop, to end early."
	}
	return Kind{
		Descriptor: node.Descriptor{
			Type: typ, Color: "#c9d3e0", Icon: "loop", PaletteLabel: label, LabelProp: "body",
			InputPorts: in, OutputPorts: out, Props: props, Help: help,
		},
		Structure: true,
		Check:     checkLoop(isFor),
		New: func(raw map[string]any) (Op, error) {
			l := &loop{
				body:    strProp(raw, "body", ""),
				carried: splitList(strProp(raw, "carried", "")),
				outs:    splitList(strProp(raw, "outs", "")),
				ins:     splitList(strProp(raw, "ins", "")),
				max:     intOr(raw["maxIterations"], 0),
				isFor:   isFor,
			}
			if l.body == "" {
				return nil, fmt.Errorf("a loop needs a body")
			}
			if s := strings.TrimSpace(strProp(raw, "start", "")); s != "" {
				if err := json.Unmarshal([]byte(s), &l.start); err != nil {
					return nil, fmt.Errorf("the start values are not a JSON object: %v", err)
				}
			}
			return l, nil
		},
	}
}

func intOr(v any, def int) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(x), "%d", &n); err == nil {
			return n
		}
	}
	return def
}

type loop struct {
	body    string
	carried []string
	ins     []string
	outs    []string
	max     int
	isFor   bool
	start   map[string]any
}

func (l *loop) Run(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
	r := rc.Run
	body, ok := r.set.Diagrams[l.body]
	if !ok {
		return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("the body %q is not a diagram", l.body)}
	}

	carried := map[string]any{}
	for _, n := range l.carried {
		if v, wired := in[n]; wired {
			carried[n] = v
		} else {
			carried[n] = l.start[n]
		}
	}
	finish := func(iters int, last map[string]any, collected []any, e *Error) map[string]any {
		out := map[string]any{"iterations": float64(iters)}
		for n, v := range carried {
			out[n] = v
		}
		for _, n := range l.outs {
			out[n] = last[n]
		}
		if l.isFor {
			if collected == nil {
				collected = []any{}
			}
			out["collected"] = collected
		}
		if e != nil {
			out[PortErr] = *e
		}
		return out
	}

	count := -1 // -1: no count, a while loop
	var items []any
	if l.isFor {
		count = int(^uint(0) >> 1)
		if v, wired := in["n"]; wired {
			f, ok := num(v)
			if !ok || f < 0 {
				return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("the count %v is not a number of passes", v)}
			}
			count = int(f)
		}
		if v, wired := in["items"]; wired {
			arr, ok := v.([]any)
			if !ok {
				return nil, &Fault{Code: CodeBadInput, Source: "the items input is not an array"}
			}
			items = arr
			if len(arr) < count {
				count = len(arr)
			}
		}
		if _, nWired := in["n"]; !nWired && items == nil {
			return nil, &Fault{Code: CodeBadInput, Source: "a for loop needs a count or an array"}
		}
	}

	var collected []any
	last := map[string]any{}
	for i := 0; ; i++ {
		if l.isFor && i >= count {
			return finish(i, last, collected, nil), nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		boundary := map[string]any{"iter": float64(i)}
		for _, n := range l.ins {
			boundary[n] = in[n]
		}
		for n, v := range carried {
			boundary[n] = v
		}
		if items != nil {
			boundary["item"] = items[i]
		}
		outs, err := r.exec(ctx, body, rc.Depth+1, boundary)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			e := Error{Status: true, Code: CodeInternal, Source: rc.Node.Type + ": " + err.Error(), Node: rc.Node.ID}
			var f *Fault
			if asFault(err, &f) {
				e.Code = f.Code
			}
			return finish(i, last, collected, &e), nil
		}
		for _, n := range l.carried {
			if v, has := outs[n]; has {
				carried[n] = v
			}
		}
		for _, n := range l.outs {
			last[n] = outs[n]
		}
		if l.isFor {
			if v, has := outs["collect"]; has {
				collected = append(collected, v)
			}
		}
		if e, bad := asError(outs[PortErr]); bad {
			return finish(i+1, last, collected, &e), nil
		}
		stop := false
		if v, has := outs["stop"]; has {
			stop = truthy(v)
		} else if !l.isFor {
			return nil, &Fault{Code: CodeBadInput, Source: "the body has no stop output, so the loop could never end"}
		}
		if stop {
			return finish(i+1, last, collected, nil), nil
		}
		if l.max > 0 && i+1 >= l.max {
			e := Error{Status: true, Code: CodeIterations, Source: fmt.Sprintf("%s: stopped at the limit of %d passes", rc.Node.Type, l.max), Node: rc.Node.ID}
			return finish(i+1, last, collected, &e), nil
		}
	}
}

func asFault(err error, f **Fault) bool {
	return errors.As(err, f)
}

// checkLoop looks into a loop's body and reports what would make it fail in the
// middle of a run: a body that is missing, a while loop whose body never says
// stop, and a carried value the body does not send back.
func checkLoop(isFor bool) func(*Set, *Diagram, *DNode) []Problem {
	return func(s *Set, _ *Diagram, n *DNode) []Problem {
		name, _ := n.Raw["body"].(string)
		body, ok := s.Diagrams[name]
		if !ok {
			return nil // reported once for the diagram
		}
		outs, ins := map[string]bool{}, map[string]bool{}
		for _, id := range body.Order {
			b := body.Nodes[id]
			switch b.Type {
			case "dia-body-out":
				outs[strProp(b.Raw, "name", "")] = true
			case "dia-body-in":
				ins[strProp(b.Raw, "name", "")] = true
			}
		}
		var out []Problem
		if !isFor && !outs["stop"] {
			out = append(out, Problem{Node: n.ID, Text: fmt.Sprintf("the body %q has no body out named stop, so the loop could never end", body.Label)})
		}
		for _, c := range splitList(strProp(n.Raw, "carried", "")) {
			if !outs[c] {
				out = append(out, Problem{Node: n.ID, Text: fmt.Sprintf("the carried value %q has no body out of that name in %q to carry it to the next pass", c, body.Label)})
			}
		}
		for _, o := range splitList(strProp(n.Raw, "outs", "")) {
			if !outs[o] {
				out = append(out, Problem{Node: n.ID, Text: fmt.Sprintf("the result %q has no body out of that name in %q", o, body.Label)})
			}
		}
		return out
	}
}
