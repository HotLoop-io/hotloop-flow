package runtime

import (
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// ConfigRef is a node pointing at a configuration node that is not in the flow
// file. It happens when a flow is deleted and takes its flow-scoped config nodes
// with it while a subflow, or a hand-edited or imported file, still points at
// them. Nothing in the file says it is broken until something tries to use it,
// and a node that only looks its config up when a message arrives would sit
// there looking fine until the first one.
type ConfigRef struct {
	NodeID   string
	NodeType string
	NodeName string
	// Prop is the property that holds the reference, and Target the id it holds.
	Prop   string
	Target string
	// ConfigType is the kind of config node the property takes.
	ConfigType string
}

// skippedRef reports whether a config reference property value names no config
// node on purpose: empty, the editor's "add a new one" marker, or the true and
// false that Node-RED's older TLS settings were saved as.
func skippedRef(v string) bool {
	switch strings.TrimSpace(v) {
	case "", "_ADD_", "true", "false":
		return true
	}
	return false
}

// MissingConfigRefs lists every enabled node whose config reference names an id
// that is not in the file, in file order. Subflow template internals are read
// where they are written, once, however many instances copy them.
func MissingConfigRefs(reg *node.Registry, f *engine.Flows) []ConfigRef {
	var out []ConfigRef
	for _, id := range f.Order {
		if n, ok := f.Nodes[id]; ok {
			out = append(out, missingRefsOf(reg, f, n)...)
		}
	}
	return out
}

func missingRefsOf(reg *node.Registry, f *engine.Flows, n *engine.Node) []ConfigRef {
	if n.Disabled {
		return nil
	}
	r, ok := reg.Lookup(n.Type)
	if !ok {
		return nil
	}
	var out []ConfigRef
	for _, p := range r.Descriptor.Props {
		if p.Kind != node.PropConfigRef {
			continue
		}
		target := n.PropString(p.Name, "")
		if skippedRef(target) {
			continue
		}
		if _, exists := f.Nodes[target]; exists {
			continue
		}
		out = append(out, ConfigRef{
			NodeID: n.ID, NodeType: n.Type, NodeName: n.Name,
			Prop: p.Name, Target: target, ConfigType: p.ConfigType,
		})
	}
	return out
}

// Label names the node the way a report line does.
func (c ConfigRef) Label() string {
	if c.NodeName != "" {
		return fmt.Sprintf("%s %s (%s)", c.NodeType, c.NodeID, c.NodeName)
	}
	return fmt.Sprintf("%s %s", c.NodeType, c.NodeID)
}

// Problem says what is wrong.
func (c ConfigRef) Problem() string {
	return fmt.Sprintf("its %s setting points at the %s config node %q, and no node with that id is in the file",
		c.Prop, c.ConfigType, c.Target)
}

// Error is the message a node that cannot start is given.
func (c ConfigRef) Error() string {
	return c.Problem() + ", so it did not start. Open it and pick a " + c.ConfigType + ", or add that config node back"
}

// Warning is the file-level report line for it.
func (c ConfigRef) Warning() string {
	return c.Label() + ": " + c.Problem() + ". It will not start until you pick a " + c.ConfigType + " or add that config node back"
}

// ConfigRefWarnings is MissingConfigRefs as report lines, for the places that
// carry a flow file's warnings: the editor on load, a deploy's answer, the log
// at start-up and the import report.
func ConfigRefWarnings(reg *node.Registry, f *engine.Flows) []string {
	refs := MissingConfigRefs(reg, f)
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, len(refs))
	for i, c := range refs {
		out[i] = c.Warning()
	}
	return out
}
