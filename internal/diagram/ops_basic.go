package diagram

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

func prop(name, label string, kind node.PropKind, def any) node.Prop {
	return node.Prop{Name: name, Label: label, Kind: kind, Default: def}
}

func choice(name, label, def string, options ...string) node.Prop {
	p := node.Prop{Name: name, Label: label, Kind: node.PropSelect, Default: def}
	for _, o := range options {
		p.Options = append(p.Options, node.Option{Value: o, Label: o})
	}
	return p
}

// opFunc makes an Op from a function.
type opFunc func(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error)

func (f opFunc) Run(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
	return f(ctx, rc, in)
}

func fixed(op Op) func(map[string]any) (Op, error) {
	return func(map[string]any) (Op, error) { return op, nil }
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-const", Color: "#d9d2c3", Icon: "const", PaletteLabel: "constant", LabelProp: "value",
			OutputPorts: []string{"value"},
			Props:       []node.Prop{prop("value", "Value (JSON)", node.PropString, "0")},
			Help:        "A value that never changes. Write it as JSON: 45, \"text\", true or [1, 2, 3]. Text that is not JSON is taken as text.",
		},
		New: func(raw map[string]any) (Op, error) {
			v := parseLiteral(raw["value"])
			return opFunc(func(context.Context, *Ctx, map[string]any) (map[string]any, error) {
				return map[string]any{"value": v}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-math", Color: "#d9d2c3", Icon: "math", PaletteLabel: "math", LabelProp: "op",
			InputPorts: []string{"a", "b?"}, OutputPorts: []string{"result"},
			Props: []node.Prop{choice("op", "Operation", "add", "add", "sub", "mul", "div", "mod", "pow", "min", "max", "abs", "neg", "sqrt", "round")},
			Help:  "Arithmetic on numbers or, one by one, on arrays. A needs a wire; B is for the two-input operations.",
		},
		New: func(raw map[string]any) (Op, error) {
			op := strProp(raw, "op", "add")
			f, ok := mathOps[op]
			if !ok {
				return nil, fmt.Errorf("%q is not a math operation", op)
			}
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				r, err := broadcast(in["a"], in["b"], f)
				if err != nil {
					return nil, &Fault{Code: CodeBadInput, Source: err.Error()}
				}
				return map[string]any{"result": r}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-compare", Color: "#d9d2c3", Icon: "compare", PaletteLabel: "compare", LabelProp: "op",
			InputPorts: []string{"a", "b"}, OutputPorts: []string{"result"},
			Props: []node.Prop{choice("op", "Test", "eq", "eq", "ne", "lt", "le", "gt", "ge")},
			Help:  "Compares two numbers or two pieces of text and gives true or false.",
		},
		New: func(raw map[string]any) (Op, error) {
			op := strProp(raw, "op", "eq")
			switch op {
			case "eq", "ne", "lt", "le", "gt", "ge":
			default:
				return nil, fmt.Errorf("%q is not a comparison", op)
			}
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				r, err := compare(op, in["a"], in["b"])
				if err != nil {
					return nil, &Fault{Code: CodeBadInput, Source: err.Error()}
				}
				return map[string]any{"result": r}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-logic", Color: "#d9d2c3", Icon: "logic", PaletteLabel: "logic", LabelProp: "op",
			InputPorts: []string{"a", "b?"}, OutputPorts: []string{"result"},
			Props: []node.Prop{choice("op", "Operation", "and", "and", "or", "xor", "not")},
			Help:  "And, or, xor on two values, or not on one. An error record counts as true when it holds an error, so an error can stop a loop.",
		},
		New: func(raw map[string]any) (Op, error) {
			op := strProp(raw, "op", "and")
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				a, b := truthy(in["a"]), truthy(in["b"])
				var r bool
				switch op {
				case "and":
					r = a && b
				case "or":
					r = a || b
				case "xor":
					r = a != b
				case "not":
					r = !a
				default:
					return nil, &Fault{Code: CodeBadInput, Source: op + " is not a logic operation"}
				}
				return map[string]any{"result": r}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-wait", Color: "#d9d2c3", Icon: "wait", PaletteLabel: "wait", LabelProp: "ms",
			InputPorts: []string{"value?"}, OutputPorts: []string{"value"},
			Props: []node.Prop{prop("ms", "Milliseconds", node.PropNumber, 100)},
			Help:  "Waits, then passes its input on. Two of these side by side finish together, because branches with no wire between them run at the same time.",
		},
		New: func(raw map[string]any) (Op, error) {
			ms := floatProp(raw, "ms", 100)
			return opFunc(func(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				if err := rc.Wait(ctx, time.Duration(ms*float64(time.Millisecond))); err != nil {
					return nil, err
				}
				return map[string]any{"value": in["value"]}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-bundle", Color: "#d9d2c3", Icon: "bundle", PaletteLabel: "bundle", LabelProp: "names",
			InputPorts: []string{"@names", "err?"}, OutputPorts: []string{"record", "err"},
			Props: []node.Prop{prop("names", "Field names, comma separated", node.PropString, "a,b")},
			Help:  "Gathers values into one record with a named field for each input.",
		},
		New: func(raw map[string]any) (Op, error) {
			names := splitList(strProp(raw, "names", ""))
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				rec := make(map[string]any, len(names))
				for _, n := range names {
					rec[n] = in[n]
				}
				return map[string]any{"record": rec}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-field", Color: "#d9d2c3", Icon: "field", PaletteLabel: "field", LabelProp: "field",
			InputPorts: []string{"record"}, OutputPorts: []string{"value"},
			Props: []node.Prop{prop("field", "Field", node.PropString, "")},
			Help:  "Takes one field out of a record.",
		},
		New: func(raw map[string]any) (Op, error) {
			f := strProp(raw, "field", "")
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				m, ok := in["record"].(map[string]any)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the input is not a record"}
				}
				v, has := m[f]
				if !has {
					return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("the record has no field %q", f)}
				}
				return map[string]any{"value": v}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-array", Color: "#d9d2c3", Icon: "array", PaletteLabel: "build array", LabelProp: "names",
			InputPorts: []string{"@names"}, OutputPorts: []string{"array"},
			Props: []node.Prop{prop("names", "Input names, comma separated", node.PropString, "a,b")},
			Help:  "Builds an array from its inputs, in the order they are named.",
		},
		New: func(raw map[string]any) (Op, error) {
			names := splitList(strProp(raw, "names", ""))
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				arr := make([]any, 0, len(names))
				for _, n := range names {
					arr = append(arr, in[n])
				}
				return map[string]any{"array": arr}, nil
			}), nil
		},
	})
}

// parseLiteral reads a constant. A string that is JSON is that JSON; any other
// string is itself.
func parseLiteral(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	var out any
	d := json.NewDecoder(strings.NewReader(s))
	if err := d.Decode(&out); err == nil && !d.More() {
		return out
	}
	return s
}

var mathOps = map[string]func(a, b float64) float64{
	"add":   func(a, b float64) float64 { return a + b },
	"sub":   func(a, b float64) float64 { return a - b },
	"mul":   func(a, b float64) float64 { return a * b },
	"div":   func(a, b float64) float64 { return a / b },
	"mod":   math.Mod,
	"pow":   math.Pow,
	"min":   math.Min,
	"max":   math.Max,
	"abs":   func(a, _ float64) float64 { return math.Abs(a) },
	"neg":   func(a, _ float64) float64 { return -a },
	"sqrt":  func(a, _ float64) float64 { return math.Sqrt(a) },
	"round": func(a, _ float64) float64 { return math.Round(a) },
}

// broadcast applies f to two numbers, or element by element when either is an
// array. A scalar against an array is applied to each element.
func broadcast(a, b any, f func(x, y float64) float64) (any, error) {
	aa, aIsArr := a.([]any)
	bb, bIsArr := b.([]any)
	switch {
	case aIsArr && bIsArr:
		if len(aa) != len(bb) {
			return nil, fmt.Errorf("the arrays have %d and %d elements", len(aa), len(bb))
		}
		out := make([]any, len(aa))
		for i := range aa {
			r, err := broadcast(aa[i], bb[i], f)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case aIsArr:
		out := make([]any, len(aa))
		for i := range aa {
			r, err := broadcast(aa[i], b, f)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case bIsArr:
		out := make([]any, len(bb))
		for i := range bb {
			r, err := broadcast(a, bb[i], f)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	x, ok := num(a)
	if !ok {
		return nil, fmt.Errorf("%v is not a number", a)
	}
	y, ok := num(b)
	if !ok && b != nil {
		return nil, fmt.Errorf("%v is not a number", b)
	}
	return f(x, y), nil
}

func compare(op string, a, b any) (bool, error) {
	if x, ok := num(a); ok {
		y, ok := num(b)
		if !ok {
			return false, fmt.Errorf("cannot compare the number %v with %v", a, b)
		}
		switch op {
		case "eq":
			return x == y, nil
		case "ne":
			return x != y, nil
		case "lt":
			return x < y, nil
		case "le":
			return x <= y, nil
		case "gt":
			return x > y, nil
		default:
			return x >= y, nil
		}
	}
	if x, ok := a.(string); ok {
		y, ok := b.(string)
		if !ok {
			return false, fmt.Errorf("cannot compare the text %q with %v", x, b)
		}
		c := strings.Compare(x, y)
		switch op {
		case "eq":
			return c == 0, nil
		case "ne":
			return c != 0, nil
		case "lt":
			return c < 0, nil
		case "le":
			return c <= 0, nil
		case "gt":
			return c > 0, nil
		default:
			return c >= 0, nil
		}
	}
	if x, ok := a.(bool); ok {
		y, ok := b.(bool)
		if ok && (op == "eq" || op == "ne") {
			return (x == y) == (op == "eq"), nil
		}
	}
	return false, fmt.Errorf("cannot compare %v with %v", a, b)
}
