package flowdiff

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// maxValueLen bounds how much of one value a text diff prints. A Function
// node's whole body on one line helps nobody; the path and the start of it say
// where to look.
const maxValueLen = 120

// Text renders a diff for a person: a summary line, then what changed, grouped
// by the tab or subflow it lives on, then the entries that only moved.
//
// The documents are for names. A wire to "a1b2c3" means nothing at 3 AM; a
// wire to change "set alarm" (a1b2c3) does.
func Text(r Result, oldFlows, newFlows *engine.Flows) string {
	if r.Empty() {
		return "No changes.\n"
	}
	names := labeller{oldFlows, newFlows}

	var b strings.Builder
	b.WriteString(summaryLine(r.Summary))
	b.WriteString("\n")
	if !r.LogicChanged() {
		b.WriteString("Layout only. Nothing about how the flows run changed.\n")
	}

	// Group logic changes by container, in order of first appearance.
	var containers []string
	byContainer := map[string][]Entry{}
	var moved []Entry
	for _, e := range r.Entries {
		if e.Kind == Moved {
			moved = append(moved, e)
			continue
		}
		if _, seen := byContainer[e.Z]; !seen {
			containers = append(containers, e.Z)
		}
		byContainer[e.Z] = append(byContainer[e.Z], e)
	}

	for _, z := range containers {
		b.WriteString("\n")
		if z == "" {
			b.WriteString("Tabs, subflows and config nodes\n")
		} else {
			b.WriteString(names.container(z) + "\n")
		}
		for _, e := range byContainer[z] {
			writeEntry(&b, e, names)
		}
	}

	if len(moved) > 0 {
		b.WriteString("\nMoved on the canvas only\n")
		for _, e := range moved {
			b.WriteString("  " + names.entry(e) + "\n")
		}
	}
	return b.String()
}

func summaryLine(s Summary) string {
	var parts []string
	add := func(n int, word string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, word))
		}
	}
	add(s.Added, "added")
	add(s.Removed, "removed")
	add(s.Changed, "changed")
	add(s.Moved, "moved")
	return strings.Join(parts, ", ")
}

func writeEntry(b *strings.Builder, e Entry, names labeller) {
	mark := map[Kind]string{Added: "+", Removed: "-", Changed: "~"}[e.Kind]
	b.WriteString("  " + mark + " " + names.entry(e) + "\n")
	for _, p := range e.Props {
		b.WriteString("      " + propLine(p) + "\n")
	}
	for _, w := range e.Wires {
		verb := "wire removed"
		if w.Added {
			verb = "wire added"
		}
		fmt.Fprintf(b, "      %s: port %d -> %s\n", verb, w.Port+1, names.id(w.To))
	}
	if len(e.Layout) > 0 {
		b.WriteString("      and moved on the canvas\n")
	}
}

func propLine(p Prop) string {
	switch {
	case p.Secret && p.HasOld && p.HasNew:
		return p.Path + ": changed"
	case p.Secret && p.HasNew:
		return p.Path + ": set"
	case p.Secret:
		return p.Path + ": removed"
	case !p.HasOld:
		return p.Path + ": added " + value(p.New)
	case !p.HasNew:
		return p.Path + ": removed (was " + value(p.Old) + ")"
	}
	return p.Path + ": " + value(p.Old) + " -> " + value(p.New)
}

func value(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	s := string(b)
	if len(s) > maxValueLen {
		s = s[:maxValueLen] + "..."
	}
	return s
}

// labeller names things from whichever document still has them.
type labeller struct{ oldFlows, newFlows *engine.Flows }

func (l labeller) raw(id string) (map[string]any, bool) {
	for _, f := range []*engine.Flows{l.newFlows, l.oldFlows} {
		if f == nil {
			continue
		}
		if raw, ok := f.Entry(id); ok {
			return raw, true
		}
	}
	return nil, false
}

func (l labeller) id(id string) string {
	raw, ok := l.raw(id)
	if !ok {
		return id
	}
	typ, _ := raw["type"].(string)
	return label(typ, displayName(raw), id)
}

func (l labeller) entry(e Entry) string { return label(e.Type, e.Name, e.ID) }

func (l labeller) container(z string) string {
	raw, ok := l.raw(z)
	if !ok {
		return "On " + z
	}
	typ, _ := raw["type"].(string)
	kind := "Tab"
	if typ == engine.TypeSubflow {
		kind = "Subflow"
	}
	if name := displayName(raw); name != "" {
		return fmt.Sprintf("%s %q (%s)", kind, name, z)
	}
	return fmt.Sprintf("%s %s", kind, z)
}

func label(typ, name, id string) string {
	if name != "" {
		return fmt.Sprintf("%s %q (%s)", typ, name, id)
	}
	return fmt.Sprintf("%s (%s)", typ, id)
}
