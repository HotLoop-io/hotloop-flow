package wiretype

import (
	"fmt"
	"sort"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/schema"
)

// Problem is one thing wrong with the typed wires in a flow.
type Problem struct {
	NodeID string `json:"nodeId"`
	Name   string `json:"name,omitempty"`
	Type   string `json:"type"`
	// Port is the 1-based output port a wire leaves from, 0 when the problem is
	// the node's own schemas rather than a wire.
	Port int `json:"port,omitempty"`
	// To is the node the wire arrives at.
	To      string `json:"to,omitempty"`
	ToName  string `json:"toName,omitempty"`
	Message string `json:"message"`
}

func (p Problem) String() string {
	from := describe(p.Type, p.Name, p.NodeID)
	if p.To == "" {
		return from + ": " + p.Message
	}
	return fmt.Sprintf("%s output %d -> %s: %s", from, p.Port, describe("", p.ToName, p.To), p.Message)
}

func describe(typ, name, id string) string {
	switch {
	case typ != "" && name != "":
		return fmt.Sprintf("%s %q (%s)", typ, name, id)
	case typ != "":
		return fmt.Sprintf("%s (%s)", typ, id)
	case name != "":
		return fmt.Sprintf("%q (%s)", name, id)
	}
	return id
}

// RefusedError is a deploy refused because of the typed wires. It lists every
// problem, not the first.
type RefusedError struct {
	Problems []Problem
}

func (e *RefusedError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.String()
	}
	noun := "problems"
	if len(parts) == 1 {
		noun = "problem"
	}
	return fmt.Sprintf("the deploy was refused because of %d typed wire %s, and the flows already running are untouched; %s",
		len(parts), noun, strings.Join(parts, "; "))
}

// Check finds what is wrong with the schemas and wires of a flow: a schema that
// does not compile, an outSchemas list longer than the node has outputs, and a
// wire where the sending end promises something the receiving end can never
// accept. nil means nothing is wrong.
//
// An Inject node has no schema of its own, so its static values stand in for
// one: a literal payload or property that the node it feeds can never accept is
// refused the same way. See LiteralConflicts.
//
// A wire is only judged when both ends declare a schema, and only refused when
// the two cannot agree on a field however the rest of the message turns out:
// different types, values outside the other end's enum or range, a field the
// receiver requires and the sender's closed schema rules out. A sender that
// merely doesn't promise a field the receiver requires is not refused, because
// the message might well carry it. That one goes to the run-time check.
func Check(flows *engine.Flows) []Problem {
	var problems []Problem
	ports := map[string]*Ports{}

	for _, id := range flows.Order {
		n, ok := flows.Nodes[id]
		if !ok || n.IsConfig || n.Disabled {
			continue
		}
		p, err := Parse(n)
		if err != nil {
			problems = append(problems, Problem{NodeID: n.ID, Name: n.Name, Type: n.Type, Message: err.Error()})
			continue
		}
		if p != nil {
			ports[n.ID] = p
		}
	}

	for _, id := range flows.Order {
		from, ok := ports[id]
		if !ok || from.Out == nil {
			continue
		}
		n := flows.Nodes[id]
		for port, targets := range n.Wires {
			if port >= len(from.outDoc) || from.outDoc[port] == nil {
				continue
			}
			for _, tid := range targets {
				to, ok := ports[tid]
				if !ok || to.In == nil {
					continue
				}
				tn := flows.Nodes[tid]
				for _, c := range Conflicts(from.outDoc[port], to.inDoc) {
					problems = append(problems, Problem{
						NodeID: n.ID, Name: n.Name, Type: n.Type, Port: port + 1,
						To: tn.ID, ToName: tn.Name, Message: c,
					})
				}
			}
		}
	}
	problems = append(problems, injectProblems(flows, ports)...)
	return problems
}

// injectProblems checks what each Inject node sets for certain against the
// input schema of every node it is wired to.
func injectProblems(flows *engine.Flows, ports map[string]*Ports) []Problem {
	var problems []Problem
	for _, id := range flows.Order {
		n, ok := flows.Nodes[id]
		if !ok || n.Type != "inject" || n.Disabled || len(n.Wires) == 0 {
			continue
		}
		var lits []literal
		for _, tid := range n.Wires[0] {
			to, ok := ports[tid]
			if !ok || to.In == nil {
				continue
			}
			if lits == nil {
				lits = injectLiterals(n)
			}
			tn := flows.Nodes[tid]
			for _, c := range LiteralConflicts(lits, to.inDoc) {
				problems = append(problems, Problem{
					NodeID: n.ID, Name: n.Name, Type: n.Type, Port: 1,
					To: tn.ID, ToName: tn.Name, Message: c,
				})
			}
		}
	}
	return problems
}

// Conflicts compares what a sender promises with what a receiver accepts, both
// as written, and returns the fields where no message can satisfy both. It is
// deliberately blunt: a keyword it does not model, a $ref, allOf, anyOf, oneOf,
// not or if on either side makes it stop looking at that part, never refuse it.
// A false refusal stops a deploy for nothing; a miss is still caught at run
// time.
func Conflicts(sender, receiver any) []string {
	var out []string
	conflicts(sender, receiver, nil, &out)
	sort.Strings(out)
	return out
}

var opaqueKeys = []string{"$ref", "$dynamicRef", "allOf", "anyOf", "oneOf", "not", "if", "then", "else", "dependentSchemas", "dependencies"}

func opaque(m map[string]any) bool {
	for _, k := range opaqueKeys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func conflicts(sender, receiver any, path []string, out *[]string) {
	s, ok1 := sender.(map[string]any)
	r, ok2 := receiver.(map[string]any)
	if !ok1 || !ok2 || opaque(s) || opaque(r) {
		return
	}
	at := schema.Path("", path...)

	// Types.
	st, rt := typeSet(s["type"]), typeSet(r["type"])
	if len(st) > 0 && len(rt) > 0 && !typesOverlap(st, rt) {
		*out = append(*out, fmt.Sprintf("%s: the sender sends %s and the receiver takes %s", at, join(st), join(rt)))
		return
	}

	// Values the sender can only send, checked against what the receiver takes.
	if vals, ok := onlyValues(s); ok {
		if rc, err := schema.CompileValue(receiver); err == nil {
			allBad := true
			for _, v := range vals {
				if rc.Validate("", v) == nil {
					allBad = false
					break
				}
			}
			if allBad {
				*out = append(*out, fmt.Sprintf("%s: the sender only sends %s and the receiver does not take it", at, show(vals)))
				return
			}
		}
	}

	// Numeric ranges that cannot meet.
	if smax, ok := num(s["maximum"]); ok {
		if rmin, ok := num(r["minimum"]); ok && smax < rmin {
			*out = append(*out, fmt.Sprintf("%s: the sender sends at most %v and the receiver wants at least %v", at, smax, rmin))
			return
		}
	}
	if smin, ok := num(s["minimum"]); ok {
		if rmax, ok := num(r["maximum"]); ok && smin > rmax {
			*out = append(*out, fmt.Sprintf("%s: the sender sends at least %v and the receiver wants at most %v", at, smin, rmax))
			return
		}
	}

	// Objects.
	sprops, _ := s["properties"].(map[string]any)
	rprops, _ := r["properties"].(map[string]any)
	if required, ok := r["required"].([]any); ok {
		sClosed := s["additionalProperties"] == false && s["patternProperties"] == nil
		for _, name := range strs(required) {
			if _, declared := sprops[name]; sClosed && !declared {
				*out = append(*out, fmt.Sprintf("%s: the receiver requires it and the sender's schema allows no such field", schema.Path("", append(append([]string{}, path...), name)...)))
			}
		}
	}
	if r["additionalProperties"] == false && r["patternProperties"] == nil {
		if required, ok := s["required"].([]any); ok {
			for _, name := range strs(required) {
				if _, declared := rprops[name]; !declared {
					*out = append(*out, fmt.Sprintf("%s: the sender always sends it and the receiver's schema allows no such field", schema.Path("", append(append([]string{}, path...), name)...)))
				}
			}
		}
	}
	for name, sub := range sprops {
		if rsub, ok := rprops[name]; ok {
			conflicts(sub, rsub, append(append([]string{}, path...), name), out)
		}
	}

	// Arrays of one kind of thing, reported against the first element, which is
	// enough to name the field.
	if si, ok := s["items"].(map[string]any); ok {
		if ri, ok := r["items"].(map[string]any); ok {
			conflicts(si, ri, append(append([]string{}, path...), "0"), out)
		}
	}
}

func typeSet(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		return strs(t)
	}
	return nil
}

func typesOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y || (x == "integer" && y == "number") || (x == "number" && y == "integer") {
				return true
			}
		}
	}
	return false
}

func onlyValues(s map[string]any) ([]any, bool) {
	if c, ok := s["const"]; ok {
		return []any{c}, true
	}
	if e, ok := s["enum"].([]any); ok && len(e) > 0 {
		return e, true
	}
	return nil, false
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	}
	return 0, false
}

func strs(l []any) []string {
	var out []string
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func join(l []string) string { return strings.Join(l, " or ") }

func show(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = string(marshalDoc(v))
	}
	return strings.Join(parts, ", ")
}
