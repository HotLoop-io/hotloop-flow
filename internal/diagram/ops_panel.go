package diagram

import (
	"context"
	"fmt"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Controls and indicators are the diagram's terminals on its panel. A control
// reads the value the operator has set at the moment it runs, so a control inside
// a loop gives the loop the latest value on every pass. An indicator shows what
// reaches it.

func init() {
	register(Kind{
		Terminal: true,
		Descriptor: node.Descriptor{
			Type: "dia-control", Color: "#e3cfa1", Icon: "control", PaletteLabel: "control", LabelProp: "label",
			OutputPorts: []string{"value"},
			Props: []node.Prop{
				prop("label", "Label", node.PropString, "control"),
				choice("kind", "Kind", "number", "number", "bool", "string", "enum", "array", "record"),
				choice("style", "Look", "entry", "entry", "spin", "slider", "knob", "switch", "button", "led"),
				prop("default", "Starting value", node.PropString, "0"),
				prop("min", "Smallest", node.PropString, ""),
				prop("max", "Largest", node.PropString, ""),
				prop("step", "Step", node.PropString, ""),
				prop("options", "Choices, comma separated (for a dropdown)", node.PropString, ""),
				choice("elem", "Items are (for an array)", "number", "number", "bool", "string"),
				prop("maxlen", "Most items (for an array, empty for no limit)", node.PropString, ""),
				prop("fields", "Fields, name:number or name:bool or name:string, comma separated (for a record)", node.PropString, ""),
				prop("latch", "A button stays pressed until the diagram reads it", node.PropBool, false),
			},
			Help: "An input on the panel. The operator sets it in the browser and the diagram reads it when this node runs. A number is typed, stepped with the spin look, slid or turned (a slider and a knob need a smallest and a largest value). An array is a list of numbers, switches or text the operator adds to and removes from, and the diagram reads it as a list. A record is a fixed set of named fields and the diagram reads it as an object. The starting value of an array or a record is JSON.",
		},
		Warn: warnControl,
		New: func(raw map[string]any) (Op, error) {
			def, err := controlDefault(raw)
			if err != nil {
				return nil, err
			}
			latch, _ := raw["latch"].(bool)
			return opFunc(func(_ context.Context, rc *Ctx, _ map[string]any) (map[string]any, error) {
				v := rc.Run.Panel().TakeControl(rc.Node.ID, def, latch)
				return map[string]any{"value": v}, nil
			}), nil
		},
	})

	register(Kind{
		Terminal: true,
		Descriptor: node.Descriptor{
			Type: "dia-indicator", Color: "#e3cfa1", Icon: "indicator", PaletteLabel: "indicator", LabelProp: "label",
			InputPorts: []string{"value"},
			Props: []node.Prop{
				prop("label", "Label", node.PropString, "indicator"),
				choice("kind", "Kind", "number", "number", "bool", "string", "error", "text"),
				choice("style", "Look", "big", "big", "plain", "led"),
				prop("unit", "Unit", node.PropString, ""),
				prop("digits", "Decimal places", node.PropNumber, 2),
			},
			Help: "An output on the panel. It shows the last value that reached it. A value that is nothing, such as what a node that skipped for an error hands on, leaves it as it was. An error indicator takes nothing to mean no error.",
		},
		New: func(raw map[string]any) (Op, error) {
			isErr := strProp(raw, "kind", "number") == "error"
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				v := in["value"]
				if v == nil && isErr {
					// Nothing on an error wire is no error, which clears a
					// red indicator once the fault has gone.
					v = Error{}
				}
				if v != nil {
					rc.Run.Panel().SetIndicator(rc.Node.ID, v)
				}
				return nil, nil
			}), nil
		},
	})

	register(Kind{
		Terminal: true,
		Descriptor: node.Descriptor{
			Type: "dia-chart", Color: "#e3cfa1", Icon: "chart", PaletteLabel: "chart", LabelProp: "label",
			InputPorts: []string{"y", "x?"},
			Props: []node.Prop{
				prop("label", "Label", node.PropString, "chart"),
				choice("mode", "Mode", "chart", "chart", "graph", "xy", "intensity"),
				prop("history", "Points kept", node.PropNumber, 1024),
				prop("unit", "Unit", node.PropString, ""),
			},
			Help: "A chart keeps a rolling history of what it is given, 1,024 points unless you change it. A graph replaces its data with each array. An XY chart plots x against y and keeps its history too. An intensity chart takes a grid (an array of rows of numbers, up to 512 by 512) and draws it as colors from its smallest value to its largest, replacing the picture with each grid. Change the mode and nothing needs rewiring.",
		},
		New: func(raw map[string]any) (Op, error) {
			isGrid := strProp(raw, "mode", "chart") == "intensity"
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				if isGrid {
					if in["y"] == nil {
						return nil, nil
					}
					rows, err := gridOf(in["y"])
					if err != nil {
						return nil, &Fault{Code: CodeBadInput, Source: err.Error()}
					}
					rc.Run.Panel().PutGrid(rc.Node.ID, rows)
					return nil, nil
				}
				y, ok := floats(in["y"])
				if !ok {
					if in["y"] == nil {
						return nil, nil
					}
					return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("%v is not a number or an array of numbers", in["y"])}
				}
				x, _ := floats(in["x"])
				rc.Run.Panel().PutChart(rc.Node.ID, x, y)
				return nil, nil
			}), nil
		},
	})
}

// warnControl says when a knob has no range to turn through. It is a warning
// and not a refusal: the panel falls back to a typed entry, as a slider does.
func warnControl(_ *Set, _ *Diagram, n *DNode) []Problem {
	if strProp(n.Raw, "style", "") != "knob" || strProp(n.Raw, "kind", "number") != "number" {
		return nil
	}
	_, lo := limit(n.Raw["min"])
	_, hi := limit(n.Raw["max"])
	if lo && hi {
		return nil
	}
	return []Problem{{Node: n.ID, Text: "a knob needs a smallest and a largest value to turn through, and shows a typed entry until it has both"}}
}

func controlDefault(raw map[string]any) (any, error) {
	if k := strProp(raw, "kind", "number"); k == "array" || k == "record" {
		return compoundDefault(raw, k)
	}
	d := raw["default"]
	if d == nil || d == "" {
		switch strProp(raw, "kind", "number") {
		case "bool":
			return false, nil
		case "string":
			return "", nil
		case "enum":
			if o := splitList(strProp(raw, "options", "")); len(o) > 0 {
				return o[0], nil
			}
			return "", nil
		}
		return 0.0, nil
	}
	return coerceControl(raw, d)
}

func floats(v any) ([]float64, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case []any:
		out := make([]float64, 0, len(x))
		for _, e := range x {
			f, ok := num(e)
			if !ok {
				return nil, false
			}
			out = append(out, f)
		}
		return out, true
	case []float64:
		return x, true
	}
	if f, ok := num(v); ok {
		return []float64{f}, true
	}
	return nil, false
}

// intensityMax is the most rows and the most columns an intensity chart takes.
const intensityMax = 512

// gridOf reads an array of rows of numbers. Every row has to be as long as the first.
func gridOf(v any) ([][]float64, error) {
	rows, ok := v.([]any)
	if !ok || len(rows) == 0 {
		return nil, fmt.Errorf("an intensity chart takes an array of rows of numbers")
	}
	if len(rows) > intensityMax {
		return nil, fmt.Errorf("%d rows is more than an intensity chart draws, %d", len(rows), intensityMax)
	}
	out := make([][]float64, len(rows))
	for i, r := range rows {
		_, isList := r.([]any)
		row, ok := floats(r)
		if !isList || !ok || len(row) == 0 {
			return nil, fmt.Errorf("row %d is not an array of numbers", i+1)
		}
		if len(row) > intensityMax {
			return nil, fmt.Errorf("row %d has %d columns, more than an intensity chart draws, %d", i+1, len(row), intensityMax)
		}
		if i > 0 && len(row) != len(out[0]) {
			return nil, fmt.Errorf("row %d has %d columns and row 1 has %d", i+1, len(row), len(out[0]))
		}
		out[i] = row
	}
	return out, nil
}
