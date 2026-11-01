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
				choice("kind", "Kind", "number", "number", "bool", "string", "enum"),
				choice("style", "Look", "entry", "entry", "slider", "knob", "switch", "button", "led"),
				prop("default", "Starting value", node.PropString, "0"),
				prop("min", "Smallest", node.PropString, ""),
				prop("max", "Largest", node.PropString, ""),
				prop("step", "Step", node.PropString, ""),
				prop("options", "Choices, comma separated (for a dropdown)", node.PropString, ""),
				prop("latch", "A button stays pressed until the diagram reads it", node.PropBool, false),
			},
			Help: "An input on the panel. The operator sets it in the browser and the diagram reads it when this node runs.",
		},
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
				choice("mode", "Mode", "chart", "chart", "graph", "xy"),
				prop("history", "Points kept", node.PropNumber, 1024),
				prop("unit", "Unit", node.PropString, ""),
			},
			Help: "A chart keeps a rolling history of what it is given, 1,024 points unless you change it. A graph replaces its data with each array. An XY chart plots x against y and keeps its history too. Change the mode and nothing needs rewiring.",
		},
		New: func(raw map[string]any) (Op, error) {
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
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

func controlDefault(raw map[string]any) (any, error) {
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
