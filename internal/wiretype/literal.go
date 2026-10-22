package wiretype

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/schema"
)

// literal is one message property an Inject node sets to a value that is the
// same every time it fires.
type literal struct {
	path  []string
	value any
}

// injectLiterals reads the static values an Inject node puts on its message.
//
// Only a string, number, boolean or JSON literal counts. A timestamp, an
// environment variable, flow or global context, another message property,
// JSONata, a buffer or a regular expression is not known until the node fires,
// so it is skipped and never guessed: a check that guessed would refuse a flow
// that works. The value types read here are the ones the Inject node itself
// reads with the same meaning (TypedValue.Eval in internal/nodes); a test there
// holds the two together.
func injectLiterals(n *engine.Node) []literal {
	var out []literal
	add := func(prop, typ string, v any) {
		path, ok := plainPath(prop)
		if !ok {
			return
		}
		val, ok := staticValue(typ, v)
		if !ok {
			return
		}
		out = append(out, literal{path: path, value: val})
	}

	if arr, ok := n.Raw["props"].([]any); ok && len(arr) > 0 {
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			p, _ := m["p"].(string)
			if p == "" {
				continue
			}
			typ, _ := m["vt"].(string)
			if typ == "" {
				typ = "str"
			}
			if _, hasV := m["v"]; !hasV && p == engine.PropTopic {
				// The topic row takes the node's own topic.
				add(p, "str", n.PropString("topic", ""))
				continue
			}
			add(p, typ, m["v"])
		}
		return out
	}
	// A flow written before the props array existed.
	typ := n.PropString("payloadType", "date")
	add(engine.PropPayload, typ, n.Raw["payload"])
	add(engine.PropTopic, "str", n.PropString("topic", ""))
	return out
}

// plainPath splits "payload" or "payload.temp". A path with brackets or quotes
// is left alone.
func plainPath(p string) ([]string, bool) {
	if p == "" || strings.ContainsAny(p, "[]\"'") {
		return nil, false
	}
	parts := strings.Split(p, ".")
	for _, s := range parts {
		if s == "" {
			return nil, false
		}
	}
	return parts, true
}

// staticValue is what a literal of this type evaluates to, and false for
// anything not known until run time.
func staticValue(typ string, v any) (any, bool) {
	text := ""
	switch t := v.(type) {
	case string:
		text = t
	case nil:
		text = ""
	case float64:
		text = strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		text = strconv.FormatBool(t)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, false
		}
		text = string(b)
	}
	switch typ {
	case "str", "":
		return text, true
	case "num":
		f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, false
		}
		return f, true
	case "bool":
		b, err := strconv.ParseBool(strings.TrimSpace(text))
		if err != nil {
			return nil, false
		}
		return b, true
	case "json":
		if strings.TrimSpace(text) == "" {
			return nil, false
		}
		var out any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return nil, false
		}
		return out, true
	}
	return nil, false
}

// LiteralConflicts checks the literals an Inject node sets against the schema
// of the node it is wired to. Every conflict is provable from the literal alone:
// the literal fails the sub-schema its property has, or the schema is closed
// and has no such property. A required property the Inject doesn't set is never
// a conflict here, because it may be set by a value that isn't static.
func LiteralConflicts(lits []literal, receiver any) []string {
	var out []string
	for _, l := range lits {
		out = append(out, literalConflict(l, receiver)...)
	}
	return out
}

func literalConflict(l literal, receiver any) []string {
	cur := receiver
	for i, name := range l.path {
		s, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		props, _ := s["properties"].(map[string]any)
		sub, declared := props[name]
		if !declared {
			if s["additionalProperties"] == false && s["patternProperties"] == nil {
				return []string{fmt.Sprintf("%s: the inject sets it and the receiver's schema allows no such field",
					schema.Path("", l.path[:i+1]...))}
			}
			return nil
		}
		cur = sub
	}
	compiled, err := schema.CompileValue(cur)
	if err != nil {
		// A sub-schema that points elsewhere in the receiver's schema can't be
		// judged on its own. Left for run time.
		return nil
	}
	err = compiled.Validate(schema.Path("", l.path...), l.value)
	v := schema.ViolationsOf(err)
	if v == nil {
		return nil
	}
	shown, _ := json.Marshal(l.value)
	text := string(shown)
	if len(text) > 40 {
		text = text[:37] + "..."
	}
	out := make([]string, len(v.List))
	for i, x := range v.List {
		out[i] = fmt.Sprintf("%s: the inject sets %s, which the receiver does not take: %s", x.Path, text, x.Message)
	}
	return out
}

// InjectValues is what the deploy check believes an Inject node sets for
// certain, by property path. The node's own tests compare it with what the node
// really sends, so the two can't drift apart.
func InjectValues(n *engine.Node) map[string]any {
	out := map[string]any{}
	for _, l := range injectLiterals(n) {
		out[strings.Join(l.path, ".")] = l.value
	}
	return out
}
