package diagram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
)

// CodeGate is the error code of a write HotLoop's gate refused.
const CodeGate = 5006

// A diagram writes to equipment one way: through HotLoop's write gate. The node
// takes a value and a HotLoop tag and sends them to the gate, which decides in
// its own order that writes are switched on, the tag is armed and the value is in
// range. HotLoop records every attempt, refused or not, with the reason
// flow:<diagram>/<node id>, so the audit trail names the diagram and the node. A
// refusal reaches the diagram as an error with the gate's own words, and the
// equipment is never written to by Flow itself, with a gate that cannot be
// reached or says no.

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-hotloop-write", Color: "#bfd9c9", Icon: "hotloop", PaletteLabel: "write through HotLoop", LabelProp: "tag",
			InputPorts: []string{"value", "tag?", "err?"}, OutputPorts: []string{"written", "reason", "err"},
			Props: []node.Prop{
				{Name: "hotloop", Label: "HotLoop", Kind: node.PropConfigRef, ConfigType: "hotloop-flow-hotloop", Required: true,
					Help: "The HotLoop whose write gate decides."},
				{Name: "tag", Label: "HotLoop tag", Kind: node.PropString, Placeholder: "plc-1/Setpoint",
					Help: "The tag this writes, written device/tag. Used when no tag is wired in."},
			},
			Help: "Writes its value to a tag in HotLoop through HotLoop's write gate, which decides in its own order that writes are switched on, that the tag is armed and that the value is in range. This is how a diagram commands equipment: Flow never writes to a machine itself. HotLoop records every attempt, refused or not, with the reason flow:<diagram>/<node>. A refusal is an error on the error wire with the gate's own reason, and so is a HotLoop that cannot be reached; the write does not happen. Written is the value that went in, and Reason is what HotLoop was told.",
		},
		New: func(raw map[string]any) (Op, error) {
			cfgID := strProp(raw, "hotloop", "")
			if cfgID == "" {
				return nil, fmt.Errorf("no HotLoop selected: a write to equipment has to go through a HotLoop's write gate")
			}
			tag := strProp(raw, "tag", "")
			return opFunc(func(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				t := tag
				if s, ok := in["tag"].(string); ok && strings.TrimSpace(s) != "" {
					t = s
				}
				if strings.TrimSpace(t) == "" {
					return nil, &Fault{Code: CodeBadInput, Source: "no HotLoop tag: set one on the node or wire one in"}
				}
				cfg, ok := rc.Run.mgr.configNode(cfgID)
				if !ok {
					return nil, &Fault{Code: CodeDevice, Source: fmt.Sprintf("the HotLoop connection %s is not running", cfgID)}
				}
				reason := fmt.Sprintf("flow:%s/%s", rc.Diagram.ID, rc.Node.ID)
				if err := nodes.GatedWrite(ctx, cfg, strings.TrimSpace(t), jsonValue(in["value"]), reason); err != nil {
					code := CodeDevice
					if errors.Is(err, nodes.ErrGateRefused) {
						code = CodeGate
					}
					return nil, &Fault{Code: code, Source: err.Error()}
				}
				return map[string]any{"written": in["value"], "reason": reason}, nil
			}), nil
		},
	})
}
