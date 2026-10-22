package nodes

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The gated write.
//
// A write node can be told to send its value through a HotLoop instead of to the
// equipment. HotLoop's write gate then decides, in its own order: that writes are
// switched on, that the tag is armed, that the value is in range. It records the
// attempt, refused or not. The equipment connection on the node is not used, so
// a node with a HotLoop selected never writes to the PLC behind the gate's back,
// and a node with none selected is exactly the node it was before.

// hotloopGateHelp is the paragraph every equipment write node adds to its help.
const hotloopGateHelp = "\n\nWith a HotLoop selected under Write through HotLoop, the value goes to HotLoop's write gate " +
	"instead of to the equipment, and a write the gate refuses arrives at a Catch node with the gate's reason."

// hotloopGateProps are the two properties the equipment write nodes carry.
func hotloopGateProps() []node.Prop {
	return []node.Prop{
		{Name: "hotloop", Kind: node.PropConfigRef, Label: "Write through HotLoop", ConfigType: "hotloop-flow-hotloop",
			Help: "Leave empty to write straight to the equipment. With a HotLoop here, the write goes to HotLoop's write " +
				"gate instead, as the tag named below, and the equipment connection above is not used. If HotLoop " +
				"refuses, or can't be reached, the write does not happen: it never falls back to writing directly."},
		{Name: "hotloopTag", Kind: node.PropTypedInput, Label: "HotLoop tag", Default: "",
			TypeProp: "hotloopTagType", TypedInputTypes: hotloopStringTypes(), Placeholder: "plc-1/Setpoint",
			Help: "The tag in HotLoop that this write is for, written device/tag. HotLoop's own address, data type, scale " +
				"and range for that tag apply, so the data type and address on this node are not used."},
	}
}

// hotloopGate is a write node's way through HotLoop.
type hotloopGate struct {
	nodeID string
	cfgID  string
	tag    TypedValue
	svc    node.Services
	target hotloopTarget
}

// newHotloopGate reads the gate properties off a write node. It answers nil when
// no HotLoop is selected, which is every flow that has never heard of one.
func newHotloopGate(def *node.Definition) (*hotloopGate, error) {
	p := def.Node
	cfg := p.PropString("hotloop", "")
	if cfg == "" {
		return nil, nil
	}
	g := &hotloopGate{
		nodeID: p.ID,
		cfgID:  cfg,
		tag:    ReadTypedValue(p.Raw, "hotloopTag", "hotloopTagType", node.TypeStr),
		svc:    def.Services,
	}
	if g.tag.Type == node.TypeStr && strings.TrimSpace(g.tag.Value) == "" {
		return nil, fmt.Errorf("a HotLoop is selected to write through, so the HotLoop tag is required: the tag in HotLoop " +
			"this write is for, like plc-1/Setpoint. Clear the HotLoop to write straight to the equipment")
	}
	if err := g.tag.Check(); err != nil {
		return nil, err
	}
	return g, nil
}

// hotloopScalar is the one value HotLoop's gate takes: a bool, a number or text.
func hotloopScalar(raw any) (any, error) {
	if l, ok := raw.([]any); ok {
		if len(l) != 1 {
			return nil, fmt.Errorf("HotLoop's gate writes one value to one tag, and the value is a list of %d. "+
				"Send one message per tag", len(l))
		}
		raw = l[0]
	}
	switch v := raw.(type) {
	case bool, string:
		return v, nil
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, nil
	}
	return nil, fmt.Errorf("%v (%s) can't go through HotLoop's gate, which takes a bool, a number or text",
		raw, describeType(raw))
}

// write sends one value through the gate and, when HotLoop lets it through,
// passes the message on. It is the whole of a gated node's Receive after the
// value has been worked out.
func (g *hotloopGate) write(ctx context.Context, m *engine.Msg, raw any, out node.Emitter,
	fail func(node.Emitter, error) error) error {
	ec := EvalContext{Msg: m, Services: g.svc, Ctx: ctx}
	tag, err := hotloopID(g.tag, ec, "HotLoop tag")
	if err != nil {
		return fail(out, err)
	}
	if !strings.Contains(tag, "/") || strings.HasPrefix(tag, "/") || strings.HasSuffix(tag, "/") {
		return fail(out, fmt.Errorf("HotLoop tag %q has to be written device/tag, like plc-1/Setpoint", tag))
	}
	value, err := hotloopScalar(raw)
	if err != nil {
		return fail(out, err)
	}
	reason := hotloopReason(g.nodeID, "")

	server := "stand-in"
	if si := node.StandInOf(g.svc); si != nil {
		if _, err := si.Call("hotloop", map[string]any{"op": "write", "tag": tag, "value": value, "reason": reason}); err != nil {
			return fail(out, fmt.Errorf("HotLoop's gate refused the write to %s: %w", tag, err))
		}
	} else {
		tgt, err := hotloopTarget0(g.svc, g.cfgID, &g.target)
		if err != nil {
			return fail(out, err)
		}
		server = tgt.describe()
		status, body, err := tgt.call(ctx, http.MethodPost, "/api/write",
			map[string]any{"tag": tag, "value": value, "reason": reason})
		if err != nil {
			return fail(out, fmt.Errorf("writing %s through HotLoop: %w", tag, err))
		}
		if status != http.StatusOK {
			verb := "failed"
			if status == http.StatusForbidden || status == http.StatusUnprocessableEntity {
				verb = "refused"
			}
			return fail(out, fmt.Errorf("HotLoop's gate %s the write to %s: %s (HTTP %d)",
				verb, tag, hotloopErrorText(body), status))
		}
	}
	m.Data["hotloop"] = hotloopInfo(server, map[string]any{"tag": tag, "written": value, "reason": reason})
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate("gated "+fmt.Sprint(value), 32)})
	out.Send(0, m)
	return nil
}
