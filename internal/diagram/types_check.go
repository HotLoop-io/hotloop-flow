package diagram

import (
	"fmt"
	"strings"
)

// checkBundleType holds a bundle to the shared type it names: every field of the
// type is one of its names and every name is a field of the type. Rename a field
// in the type and every bundle that used the old name is a problem at the next
// deploy.
func checkBundleType(s *Set, _ *Diagram, n *DNode) []Problem {
	typ := strProp(n.Raw, "shape", "")
	if typ == "" {
		return nil
	}
	t, ok := s.Types[typ]
	if !ok {
		return []Problem{{Node: n.ID, Text: fmt.Sprintf("the type %q is not defined in any diagram", typ)}}
	}
	have := map[string]bool{}
	var out []Problem
	for _, name := range splitList(strProp(n.Raw, "names", "")) {
		have[name] = true
		if _, ok := t.Fields[name]; !ok {
			out = append(out, Problem{Node: n.ID, Text: fmt.Sprintf("the type %q has no field %q (it has %s)", typ, name, strings.Join(t.FieldNames(), ", "))})
		}
	}
	for _, f := range t.FieldNames() {
		if !have[f] {
			out = append(out, Problem{Node: n.ID, Text: fmt.Sprintf("the type %q has a field %q this bundle does not set", typ, f)})
		}
	}
	return out
}

// checkFieldType says when the shared type a field node names has no such field.
func checkFieldType(s *Set, _ *Diagram, n *DNode) []Problem {
	typ := strProp(n.Raw, "shape", "")
	if typ == "" {
		return nil
	}
	t, ok := s.Types[typ]
	if !ok {
		return []Problem{{Node: n.ID, Text: fmt.Sprintf("the type %q is not defined in any diagram", typ)}}
	}
	f := strProp(n.Raw, "field", "")
	if _, ok := t.Fields[f]; !ok {
		return []Problem{{Node: n.ID, Text: fmt.Sprintf("the type %q has no field %q (it has %s)", typ, f, strings.Join(t.FieldNames(), ", "))}}
	}
	return nil
}
