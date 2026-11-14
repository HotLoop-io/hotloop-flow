package diagram

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// The case structure picks one body to run from the value on its selector: a
// boolean, a number, a string or an enum (which is a string). Only that body runs.
// A selector no case names runs the default case; with no default it is an error
// on the error wire, never a silent skip.

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-case", Color: "#c9d3e0", Icon: "loop", PaletteLabel: "case structure", LabelProp: "cases",
			InputPorts:  []string{"selector", "@ins", "err?"},
			OutputPorts: []string{"@outs", "case", "err"},
			Props: []node.Prop{
				{Name: "cases", Label: "Cases (JSON object)", Kind: node.PropString, Default: "{}", Required: true,
					Help: `Each case name and the diagram it runs, for example {"true": "heat", "false": "idle"} or {"1": "a", "2": "b"}.`},
				prop("default", "Default case (diagram)", node.PropString, ""),
				prop("ins", "Inputs, comma separated", node.PropString, ""),
				prop("outs", "Results, comma separated", node.PropString, ""),
			},
			Help: "Runs one body, chosen by the value on Selector. A boolean picks \"true\" or \"false\", a number picks the case of that value, and a string or an enum picks the case of that name. A value no case names runs the default case, and with no default the node reports an error. Every body must send each result back out under the same name. Case out says which case ran.",
		},
		Structure: true,
		Cases:     caseList,
		Check:     checkCase,
		Warn:      warnCase,
		New: func(raw map[string]any) (Op, error) {
			if _, err := parseCases(raw); err != nil {
				return nil, err
			}
			return &caseOp{ins: splitList(strProp(raw, "ins", "")), outs: splitList(strProp(raw, "outs", ""))}, nil
		},
	})
}

func parseCases(raw map[string]any) (map[string]string, error) {
	out := map[string]string{}
	s := strings.TrimSpace(strProp(raw, "cases", ""))
	if s != "" {
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, fmt.Errorf("the cases are not a JSON object of case name to diagram: %v", err)
		}
	}
	if d := strings.TrimSpace(strProp(raw, "default", "")); d != "" {
		out[DefaultCase] = d
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("a case structure needs at least one case")
	}
	return out, nil
}

func caseList(raw map[string]any) []CaseBody {
	m, err := parseCases(raw)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]CaseBody, 0, len(names))
	for _, n := range names {
		out = append(out, CaseBody{Name: n, Body: m[n]})
	}
	return out
}

// caseKey is the name a selector value goes by.
func caseKey(v any) (string, error) {
	switch x := v.(type) {
	case bool:
		return strconv.FormatBool(x), nil
	case string:
		return x, nil
	}
	if f, ok := num(v); ok {
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	}
	return "", fmt.Errorf("the selector %v is not a boolean, a number or a string", v)
}

// matchCase finds the case a selector value names. A number matches a case named
// for it however it is written, so 2 matches "2" and "2.0".
func matchCase(v any, have func(string) bool, names []string) (string, bool) {
	key, err := caseKey(v)
	if err != nil {
		return "", false
	}
	if have(key) && key != DefaultCase {
		return key, true
	}
	if f, ok := num(v); ok {
		for _, n := range names {
			if g, err := strconv.ParseFloat(n, 64); err == nil && g == f {
				return n, true
			}
		}
	}
	if key == DefaultCase && have(key) {
		// A string selector named "default" is the case of that name only if the
		// person wrote it as a case, which is the default case; both mean it.
		return key, true
	}
	return "", false
}

type caseOp struct {
	ins  []string
	outs []string
}

func (c *caseOp) Run(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
	r := rc.Run
	sel := in["selector"]
	key, err := caseKey(sel)
	if err != nil {
		return nil, &Fault{Code: CodeBadInput, Source: err.Error()}
	}
	names := caseNames(rc.Node.Raw)
	have := func(n string) bool { _, ok := r.set.CaseBody(rc.Node.ID, n); return ok }
	chosen, ok := matchCase(sel, have, names)
	if !ok {
		if !have(DefaultCase) {
			return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("no case is named %q and there is no default case", key)}
		}
		chosen = DefaultCase
	}
	bid, _ := r.set.CaseBody(rc.Node.ID, chosen)
	body := r.set.Diagrams[bid]
	if body == nil {
		return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("the body of case %q is not a diagram", chosen)}
	}
	boundary := map[string]any{"selector": sel}
	for _, n := range c.ins {
		boundary[n] = in[n]
	}
	out := map[string]any{"case": chosen}
	outs, err := r.exec(ctx, body, rc.Depth+1, boundary)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		e := Error{Status: true, Code: CodeInternal, Source: rc.Node.Type + ": " + err.Error(), Node: rc.Node.ID}
		var f *Fault
		if asFault(err, &f) {
			e.Code = f.Code
		}
		out[PortErr] = e
		return out, nil
	}
	for _, n := range c.outs {
		out[n] = outs[n]
	}
	if e, bad := asError(outs[PortErr]); bad {
		out[PortErr] = e
	}
	return out, nil
}

func caseNames(raw map[string]any) []string {
	var out []string
	for _, c := range caseList(raw) {
		out = append(out, c.Name)
	}
	return out
}

// checkCase reports what would fail in the middle of a run: a case whose body
// does not send out a result the structure promises.
func checkCase(s *Set, _ *Diagram, n *DNode) []Problem {
	var out []Problem
	for _, c := range caseList(n.Raw) {
		bid, ok := s.CaseBody(n.ID, c.Name)
		if !ok {
			continue // reported once for the diagram
		}
		body := s.Diagrams[bid]
		have := map[string]bool{}
		for _, id := range body.Order {
			if b := body.Nodes[id]; b.Type == "dia-body-out" {
				have[strProp(b.Raw, "name", "")] = true
			}
		}
		for _, o := range splitList(strProp(n.Raw, "outs", "")) {
			if !have[o] {
				out = append(out, Problem{Node: n.ID, Text: fmt.Sprintf("the result %q has no body out of that name in %q, the body of case %q", o, body.Label, c.Name)})
			}
		}
	}
	return out
}

// warnCase says when a selector value could reach no case. A boolean selector
// with both cases written needs no default; anything else does.
func warnCase(s *Set, _ *Diagram, n *DNode) []Problem {
	has := map[string]bool{}
	for _, c := range caseList(n.Raw) {
		has[c.Name] = true
	}
	if has[DefaultCase] || (has["true"] && has["false"]) {
		return nil
	}
	return []Problem{{Node: n.ID, Text: "the case structure has no default case, so a selector value no case names is an error"}}
}
