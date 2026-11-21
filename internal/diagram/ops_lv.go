package diagram

import (
	"fmt"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// A LabVIEW node is what the VI importer leaves where the VI had a primitive, a
// call of another VI, or a structure that FlowLab does not read the meaning of.
// It keeps the wires, so the diagram has the VI's shape, and it never runs: a
// diagram that holds one is a problem in the error list until the node is
// replaced, because a diagram that ran with a guess in that place would give
// numbers that are wrong while they look right.
func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-lv", Color: "#c9c3d9", Icon: "lv", PaletteLabel: "LabVIEW node", LabelProp: "name",
			InputPorts: []string{"@inputs"}, OutputPorts: []string{"@outputs"},
			Props: []node.Prop{
				prop("name", "Name", node.PropString, ""),
				prop("lvclass", "What it was in the VI", node.PropString, ""),
				prop("lvprim", "Primitive", node.PropString, ""),
				prop("bodies", "Diagrams it holds", node.PropString, ""),
				prop("inputs", "Inputs, comma separated", node.PropString, ""),
				prop("outputs", "Outputs, comma separated", node.PropString, ""),
			},
			Help: "A node that came from a LabVIEW VI and has no FlowLab equivalent read from the file: a primitive, a call of another VI, or a structure. It keeps its place and its wires so the diagram has the VI's shape. Replace it with the node that does the same, wire the new one the same way, and delete this one. A diagram that holds one does not run.",
		},
		New: func(raw map[string]any) (Op, error) {
			what := strProp(raw, "lvclass", "a LabVIEW node")
			if p := strProp(raw, "lvprim", ""); p != "" {
				what = p
			}
			return nil, fmt.Errorf("this is %s from a LabVIEW VI, which FlowLab has no node for yet: replace it with the node that does the same", what)
		},
	})
}
