// Package wiretype is typed wires: optional JSON Schemas on a node's ports.
//
// A node may carry two keys in its flow entry, both ignored by every other tool
// that reads a flow file, so a flow with schemas still round-trips byte for
// byte:
//
//	"ew_inSchema":   {...}           what the node accepts
//	"ew_outSchemas": [{...}, null]   what each output port promises, by port
//
// A schema describes the whole message. Keys that start with an underscore,
// _msgid and the rest of the runtime's own bookkeeping, are left out of what it
// sees, so a closed schema does not have to name them.
//
// Two checks use them. At deploy, Check refuses a flow where one end of a wire
// promises something the other end can never accept. At run time a message that
// breaks a schema it can't be caught against ahead of time is stopped where it
// broke it and raised as an error with the path of every bad field.
//
// A node with no schemas costs nothing and behaves exactly as it always did.
package wiretype

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/schema"
)

// The flow-entry keys.
const (
	InKey  = "ew_inSchema"
	OutKey = "ew_outSchemas"
)

// Ports is the compiled schemas of one node.
type Ports struct {
	// In checks what the node receives. nil when the node declares none.
	In *schema.Schema
	// Out[i] checks what leaves output i. An entry is nil for a port that
	// declares none.
	Out []*schema.Schema

	inDoc  any
	outDoc []any
}

// Parse reads and compiles a node's port schemas. It returns nil, nil for the
// common node that has none, so the caller's hot path is a nil check.
func Parse(n *engine.Node) (*Ports, error) {
	inRaw, hasIn := n.Raw[InKey]
	outRaw, hasOut := n.Raw[OutKey]
	if (!hasIn || inRaw == nil) && (!hasOut || outRaw == nil) {
		return nil, nil
	}
	p := &Ports{}
	declared := false
	if hasIn && inRaw != nil {
		sc, err := schema.CompileValue(inRaw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", InKey, err)
		}
		p.In, p.inDoc = sc, inRaw
	}
	if hasOut && outRaw != nil {
		list, ok := outRaw.([]any)
		if !ok {
			return nil, fmt.Errorf("%s is a list with one schema or null for each output port", OutKey)
		}
		if len(list) > len(n.Wires) {
			return nil, fmt.Errorf("%s has %d entries and the node has %d output ports", OutKey, len(list), len(n.Wires))
		}
		p.Out = make([]*schema.Schema, len(list))
		p.outDoc = list
		for i, raw := range list {
			if raw == nil {
				continue
			}
			sc, err := schema.CompileValue(raw)
			if err != nil {
				return nil, fmt.Errorf("%s[%d] (output %d): %w", OutKey, i, i+1, err)
			}
			p.Out[i] = sc
			declared = true
		}
	}
	if p.In == nil && !declared {
		return nil, nil
	}
	return p, nil
}

// CheckIn validates a message arriving at the node. A mismatch is an error
// carrying *schema.Violations.
func (p *Ports) CheckIn(m *engine.Msg) error {
	if p == nil || p.In == nil {
		return nil
	}
	if err := p.In.Validate("", view(m)); err != nil {
		return fmt.Errorf("the message does not match this node's input schema: %w", err)
	}
	return nil
}

// CheckOut validates a message the node is sending on a port (0-based).
func (p *Ports) CheckOut(port int, m *engine.Msg) error {
	if p == nil || port < 0 || port >= len(p.Out) || p.Out[port] == nil {
		return nil
	}
	if err := p.Out[port].Validate("", view(m)); err != nil {
		return fmt.Errorf("the message does not match the schema of output %d: %w", port+1, err)
	}
	return nil
}

// view is the message as a schema sees it: its own keys, without the runtime's
// underscore-prefixed bookkeeping. It shares values with the message and
// changes nothing in it.
func view(m *engine.Msg) map[string]any {
	out := make(map[string]any, len(m.Data))
	for k, v := range m.Data {
		if strings.HasPrefix(k, "_") {
			continue
		}
		out[k] = v
	}
	return out
}

// Violations returns the field-level detail an error carries, shaped for a
// message: a list of {path, keyword, message}. It is nil when the error is not
// a schema mismatch.
func Violations(err error) []any {
	v := schema.ViolationsOf(err)
	if v == nil {
		return nil
	}
	out := make([]any, len(v.List))
	for i, x := range v.List {
		out[i] = map[string]any{"path": x.Path, "keyword": x.Keyword, "message": x.Message}
	}
	return out
}

// marshalDoc is for tests and the conflict checker: a schema as it was written.
func marshalDoc(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
