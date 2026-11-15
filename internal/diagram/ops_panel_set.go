package diagram

import (
	"context"
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Programmatic control of a panel object: a diagram can show or hide a control or
// an indicator, turn a control off, colour it, rename it, and move the range of a
// number control, by the object's name and not by wiring to it. A case that sees
// an error turns an indicator red with one node.

// panelProps are the properties a diagram can set, and what each takes.
var panelProps = []string{"visible", "enabled", "color", "label", "min", "max"}

// panelColors are the colours an object can be given. They are names, not
// codes, so the panel draws each in the theme the person has chosen.
var panelColors = []string{"default", "red", "amber", "green", "blue", "grey"}

// ResolveTerminal finds the panel object a name means: the label of exactly one
// of them, or an id.
func ResolveTerminal(terms []*DNode, name string) (*DNode, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("no panel object named")
	}
	var byLabel []*DNode
	for _, t := range terms {
		if t.ID == name {
			return t, nil
		}
		if strProp(t.Raw, "label", "") == name {
			byLabel = append(byLabel, t)
		}
	}
	switch len(byLabel) {
	case 1:
		return byLabel[0], nil
	case 0:
		return nil, fmt.Errorf("no panel object is named %q", name)
	}
	return nil, fmt.Errorf("%d panel objects are named %q, so it does not say which; rename one or use its id", len(byLabel), name)
}

func coerceAttr(prop string, v any) (any, error) {
	switch prop {
	case "visible", "enabled":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s takes true or false, not %v", prop, cell(v))
		}
		return b, nil
	case "color":
		s, _ := v.(string)
		for _, c := range panelColors {
			if s == c {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%v is not a colour (%s)", cell(v), strings.Join(panelColors, ", "))
	case "label":
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("label takes text")
		}
		return s, nil
	case "min", "max":
		f, ok := num(v)
		if !ok {
			return nil, fmt.Errorf("%s takes a number, not %v", prop, cell(v))
		}
		return f, nil
	}
	return nil, fmt.Errorf("%q is not a panel property (%s)", prop, strings.Join(panelProps, ", "))
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-panel-set", Color: "#e3cfa1", Icon: "control", PaletteLabel: "set panel object", LabelProp: "target",
			InputPorts: []string{"value", "target?", "err?"}, OutputPorts: []string{"set", "err"},
			Props: []node.Prop{
				prop("target", "Panel object (its label)", node.PropString, ""),
				choice("property", "Property", "color", panelProps...),
			},
			Help: "Changes one property of a control or an indicator on the panel while the diagram runs, by the object's name: visible (true or false), enabled (true or false, off stops the operator setting a control), color (red, amber, green, blue, grey or default), label (text), and min or max (a number, for a number control). The object is named by its label, or its id, and set is the value that was applied. A name that matches no object, or more than one, is an error. A run starts from the panel as it was designed.",
		},
		Check: checkPanelSet,
		New: func(raw map[string]any) (Op, error) {
			target := strProp(raw, "target", "")
			prop := strProp(raw, "property", "color")
			known := false
			for _, k := range panelProps {
				known = known || k == prop
			}
			if !known {
				return nil, fmt.Errorf("%q is not a panel property (%s)", prop, strings.Join(panelProps, ", "))
			}
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				t := target
				if s, ok := in["target"].(string); ok && strings.TrimSpace(s) != "" {
					t = s
				}
				if err := rc.Run.Panel().SetAttr(t, prop, in["value"]); err != nil {
					return nil, &Fault{Code: CodeBadInput, Source: err.Error()}
				}
				return map[string]any{"set": in["value"]}, nil
			}), nil
		},
	})
}

// checkPanelSet says at deploy when a name that is typed on the node matches no
// panel object of the diagram it belongs to, or more than one.
func checkPanelSet(s *Set, d *Diagram, n *DNode) []Problem {
	t := strProp(n.Raw, "target", "")
	if t == "" {
		return nil // wired in
	}
	if _, err := ResolveTerminal(s.Terminals(s.Owner(d.ID)), t); err != nil {
		return []Problem{{Node: n.ID, Text: err.Error()}}
	}
	return nil
}
