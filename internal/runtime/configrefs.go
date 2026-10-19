package runtime

import (
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// RefProblem says how a config reference is wrong.
type RefProblem int

const (
	// RefMissing is an id that names nothing in the file.
	RefMissing RefProblem = iota
	// RefNotConfig is an id that names something in the file that is not a
	// configuration node at all: a tab, a subflow, a group or an ordinary node.
	RefNotConfig
	// RefWrongType is an id that names a configuration node of a different kind
	// than the setting takes, such as a TLS setting pointing at an MQTT broker.
	RefWrongType
)

// ConfigRef is a node pointing at a configuration node that is not usable. Most
// often it is not in the flow file: it happens when a flow is deleted and takes
// its flow-scoped config nodes with it while a subflow, or a hand-edited or
// imported file, still points at them. It can also name something that is in the
// file and is not what the setting takes, which a hand edit or a pasted id gets
// you. Nothing in the file says it is broken until something tries to use it,
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
	// Why says how the reference is wrong, and Found what the id names when it
	// names something: the node type, or "tab", "subflow" or "group".
	Why   RefProblem
	Found string
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
// that is not in the file, or names something in the file that is not a config
// node of the type the setting takes, in file order. Subflow template internals are read
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
		why, found := checkRef(reg, f, target, p.ConfigType)
		if why == refOK {
			continue
		}
		out = append(out, ConfigRef{
			NodeID: n.ID, NodeType: n.Type, NodeName: n.Name,
			Prop: p.Name, Target: target, ConfigType: p.ConfigType,
			Why: why, Found: found,
		})
	}
	return out
}

// refOK is not a RefProblem a report ever carries; it is checkRef's answer for a
// reference that is fine.
const refOK RefProblem = -1

// checkRef looks one reference up. It returns what is wrong with it, and what
// the id names when it names something.
func checkRef(reg *node.Registry, f *engine.Flows, target, want string) (RefProblem, string) {
	t, ok := f.Nodes[target]
	if !ok {
		switch {
		case f.Tabs[target] != nil:
			return RefNotConfig, "tab"
		case f.Subflows[target] != nil:
			return RefNotConfig, "subflow"
		case f.Groups[target] != nil:
			return RefNotConfig, "group"
		}
		return RefMissing, ""
	}
	if t.Type == want {
		if r, known := reg.Lookup(t.Type); known && r.Descriptor.IsConfig {
			return refOK, t.Type
		}
	}
	if r, known := reg.Lookup(t.Type); known && r.Descriptor.IsConfig {
		return RefWrongType, t.Type
	}
	return RefNotConfig, t.Type
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
	switch c.Why {
	case RefNotConfig:
		return fmt.Sprintf("its %s setting points at %q, which is a %s and not a config node, and that setting takes a %s config node",
			c.Prop, c.Target, c.Found, c.ConfigType)
	case RefWrongType:
		return fmt.Sprintf("its %s setting points at %q, which is a %s config node, and that setting takes a %s config node",
			c.Prop, c.Target, c.Found, c.ConfigType)
	}
	return fmt.Sprintf("its %s setting points at the %s config node %q, and no node with that id is in the file",
		c.Prop, c.ConfigType, c.Target)
}

// fix is what to do about it.
func (c ConfigRef) fix() string {
	if c.Why == RefMissing {
		return "pick a " + c.ConfigType + ", or add that config node back"
	}
	return "pick a " + c.ConfigType
}

// Error is the message a node that cannot start is given.
func (c ConfigRef) Error() string {
	return c.Problem() + ", so it did not start. Open it and " + c.fix()
}

// Warning is the file-level report line for it.
func (c ConfigRef) Warning() string {
	if c.Why == RefMissing {
		return c.Label() + ": " + c.Problem() + ". It will not start until you pick a " + c.ConfigType + " or add that config node back"
	}
	return c.Label() + ": " + c.Problem() + ". It will not start until you pick a " + c.ConfigType
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
