package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/wasmhost"
)

// descriptor builds the node type's descriptor from the plugin's own. The
// editor draws the dialog from it with no code of the plugin's in the browser.
func (p *Plugin) descriptor() node.Descriptor {
	m := &p.Manifest
	label := m.Label
	if label == "" {
		label = m.Name
	}

	props := append([]node.Prop{{Name: propName, Kind: node.PropString, Label: "Name"}}, m.Props...)
	props = append(props,
		node.Prop{Name: propMemory, Kind: node.PropNumber, Label: "Memory limit (MiB)", Default: m.memoryMiB(),
			Help: "A hard ceiling on the plugin's memory. Past it the plugin traps, the message goes to a Catch " +
				"node, and nothing else is affected."},
		node.Prop{Name: propTimeout, Kind: node.PropNumber, Label: "Timeout (ms)", Default: m.timeoutMs(),
			Help: "A hard limit on one message. A plugin still running at the end of it is stopped."},
	)

	help := &strings.Builder{}
	if m.Description != "" {
		help.WriteString(m.Description + "\n\n")
	}
	if m.Help != "" {
		help.WriteString(m.Help + "\n\n")
	}
	fmt.Fprintf(help, "Plugin `%s` %s, WebAssembly, signed with key `%s`.", m.Name, m.Version, p.SignedBy)
	if len(p.Needs) > 0 {
		fmt.Fprintf(help, " It asks for: %s. The operator grants these in `plugins.grants`; a flow using it does not deploy until they are.", capList(p.Needs))
	} else {
		help.WriteString(" It asks for no host access.")
	}

	notes := "A plugin: a signed WebAssembly module, not a Node-RED node. It has no equivalent in a Node-RED flow."
	return node.Descriptor{
		Type:         m.Type(),
		Category:     node.CategoryFunction,
		Color:        "#FDD0A2",
		Icon:         "function",
		Inputs:       1,
		Outputs:      m.outputs(),
		PaletteLabel: label,
		LabelProp:    propName,
		Props:        props,
		Help:         help.String(),
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: notes,
		},
	}
}

// wasmNode runs one plugin against each message.
type wasmNode struct {
	plugin  *Plugin
	mod     *wasmhost.Module
	config  map[string]any
	outputs int
	svc     node.Services
}

// newNode builds a node from a flow entry. It runs when the flow starts, so
// every refusal here is also a refusal at deploy, and the node shows red.
func (h *Host) newNode(p *Plugin, def *node.Definition) (node.Node, error) {
	if err := h.admit(p, def.Node); err != nil {
		return nil, err
	}
	config, err := p.config(def.Node)
	if err != nil {
		return nil, err
	}
	mem, timeout := nodeLimits(p, def.Node)

	name := def.Node.Name
	if name == "" {
		name = p.Manifest.Name + ":" + def.Node.ID
	}
	mod, err := wasmhost.CompileWith(context.Background(), name, p.Module, wasmhost.Limits{
		MaxMemoryBytes: uint64(mem) << 20,
		Timeout:        time.Duration(timeout) * time.Millisecond,
	}, wasmhost.Options{Capabilities: hostCaps(p.Needs), Cache: h.cache})
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %w", p.Manifest.Name, err)
	}
	if err := mod.Warm(context.Background()); err != nil {
		_ = mod.Close(context.Background())
		return nil, fmt.Errorf("plugin %s: %w", p.Manifest.Name, err)
	}
	return &wasmNode{plugin: p, mod: mod, config: config, outputs: p.Manifest.outputs(), svc: def.Services}, nil
}

// config reads the node's properties as the dialog declared them and checks
// them, so a flow file that doesn't fit the plugin's own descriptor is refused
// the way a built-in node refuses one.
func (p *Plugin) config(n *engine.Node) (map[string]any, error) {
	out := map[string]any{}
	for _, prop := range p.Manifest.Props {
		raw, present := n.Prop(prop.Name)
		if !present {
			raw = prop.Default
		}
		v, err := coerce(prop, raw)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", prop.Name, err)
		}
		if prop.Required && (v == nil || v == "") {
			return nil, fmt.Errorf("property %q is required", prop.Name)
		}
		if v != nil {
			out[prop.Name] = v
		}
	}
	return out, nil
}

func coerce(p node.Prop, raw any) (any, error) {
	if raw == nil {
		return nil, nil
	}
	switch p.Kind {
	case node.PropNumber:
		switch v := raw.(type) {
		case float64, int, int64:
			return v, nil
		case json.Number:
			return v.Float64()
		case string:
			if strings.TrimSpace(v) == "" {
				return nil, nil
			}
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
				return nil, fmt.Errorf("%q is not a number", v)
			}
			return f, nil
		}
		return nil, fmt.Errorf("%v is not a number", raw)
	case node.PropBool:
		switch v := raw.(type) {
		case bool:
			return v, nil
		case string:
			switch v {
			case "true":
				return true, nil
			case "false":
				return false, nil
			}
		}
		return nil, fmt.Errorf("%v is not true or false", raw)
	case node.PropJSON:
		if s, ok := raw.(string); ok {
			if strings.TrimSpace(s) == "" {
				return nil, nil
			}
			var v any
			if err := json.Unmarshal([]byte(s), &v); err != nil {
				return nil, fmt.Errorf("is not valid JSON: %v", err)
			}
			return v, nil
		}
		return raw, nil
	case node.PropList:
		return raw, nil
	}
	return raw, nil
}

// access gives the guest the contexts of the node that is calling it.
type access struct{ svc node.Services }

func (a access) store(s wasmhost.ContextScope) node.Context {
	if s == wasmhost.ScopeGlobal {
		return a.svc.Context(node.ScopeGlobal)
	}
	return a.svc.Context(node.ScopeFlow)
}

func (a access) Get(s wasmhost.ContextScope, key string) (any, bool, error) {
	return a.store(s).Get(key)
}

func (a access) Set(s wasmhost.ContextScope, key string, v any) error {
	return a.store(s).Set(key, v)
}

func (n *wasmNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ctx = wasmhost.WithContextAccess(ctx, access{svc: n.svc})
	resp, err := n.mod.Call(ctx, wasmhost.Request{Msg: m.Data, Config: n.config})
	if err != nil {
		return fmt.Errorf("plugin %s: %w", n.plugin.Manifest.Name, err)
	}

	for _, l := range resp.Logs {
		out.Log(node.LogInfo, "%s", l)
	}
	if resp.Status != nil {
		out.Status(node.Status{Fill: resp.Status.Fill, Shape: resp.Status.Shape, Text: resp.Status.Text})
	}

	// What the guest sent before it complained is sent: it already happened.
	if len(resp.Send) > n.outputs {
		return fmt.Errorf("plugin %s returned messages for %d outputs and its descriptor declares %d",
			n.plugin.Manifest.Name, len(resp.Send), n.outputs)
	}
	byPort := make([][]*engine.Msg, len(resp.Send))
	for port, msgs := range resp.Send {
		for _, data := range msgs {
			if data != nil {
				byPort[port] = append(byPort[port], engine.WrapMsg(data))
			}
		}
	}
	out.SendAll(byPort)

	if resp.Error != "" {
		return fmt.Errorf("plugin %s: %s", n.plugin.Manifest.Name, resp.Error)
	}
	return nil
}

// Close releases the plugin's instances and its memory.
func (n *wasmNode) Close(ctx context.Context, _ bool) error { return n.mod.Close(ctx) }
