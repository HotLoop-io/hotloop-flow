// Package flowdiff compares two flow documents the way a person reviewing a
// deploy reads them: node by node, property by property.
//
// A line diff of the flow file is honest but useless. Drag one node twenty
// pixels and it reports a change; change a threshold and it reports a change;
// and the reviewer can't tell which one is about to stop a press. So this says
// which nodes were added, removed or changed, which properties changed from
// what to what, which wires moved, and it keeps layout (where a node sits on
// the canvas) apart from logic, so a tidy-up never reads like a behaviour
// change.
//
// It is an engine package, not an editor feature. The API, the CLI, the git
// difftool and the editor's canvas all draw from the same answer.
package flowdiff

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// Kind summarises what happened to one entry.
type Kind string

const (
	Added   Kind = "added"
	Removed Kind = "removed"
	// Changed means something about how it runs is different: a property, its
	// wires, its tab, its group.
	Changed Kind = "changed"
	// Moved means only its place on the canvas changed. Nothing about how the
	// flow runs is different.
	Moved Kind = "moved"
)

// credentialSentinel is what the editor sends for a credential field it showed
// but nobody touched. It means "unchanged", never a new value.
const credentialSentinel = "__PWRD__"

// Entry is one node, tab, subflow or group that differs.
type Entry struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	// Z is the tab or subflow it lives on, on the newer side when it exists
	// there.
	Z    string `json:"z,omitempty"`
	Kind Kind   `json:"kind"`

	// Props are logic changes, in a stable order.
	Props []Prop `json:"props,omitempty"`
	// Wires are connections added or removed, by output port.
	Wires []Wire `json:"wires,omitempty"`
	// Layout are position and size changes. Reported, never mixed in with
	// Props.
	Layout []Prop `json:"layout,omitempty"`
}

// Prop is one property that differs. A path reads like the expression that
// would reach it: rules[1].v, env[0].value. Old is absent for an added
// property and New for a removed one.
//
// Secret marks a credential. Its values are never carried, only the fact that
// it changed.
type Prop struct {
	Path   string `json:"path"`
	Old    any    `json:"old,omitempty"`
	New    any    `json:"new,omitempty"`
	HasOld bool   `json:"hasOld"`
	HasNew bool   `json:"hasNew"`
	Secret bool   `json:"secret,omitempty"`
}

// Wire is a connection that appeared or disappeared.
type Wire struct {
	Port  int    `json:"port"`
	To    string `json:"to"`
	Added bool   `json:"added"`
}

// Result is a whole diff.
type Result struct {
	Entries []Entry `json:"entries"`
	Summary Summary `json:"summary"`
}

// Summary counts entries by kind.
type Summary struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
	Changed int `json:"changed"`
	Moved   int `json:"moved"`
}

// Empty reports whether nothing at all differs, layout included.
func (r Result) Empty() bool { return len(r.Entries) == 0 }

// LogicChanged reports whether anything about how the flows run differs.
func (r Result) LogicChanged() bool {
	return r.Summary.Added+r.Summary.Removed+r.Summary.Changed > 0
}

// Diff compares two flow documents. Either may be nil, which reads as empty.
//
// Entries come out in the newer document's order, then removed entries in the
// older document's order, so a diff reads top to bottom the way the file does.
func Diff(oldFlows, newFlows *engine.Flows) Result {
	oldEntries, oldOrder := entries(oldFlows)
	newEntries, newOrder := entries(newFlows)

	// Entries is never nil, so the JSON form is always a list a client can
	// iterate without a null check.
	res := Result{Entries: []Entry{}}
	for _, id := range newOrder {
		n := newEntries[id]
		o, existed := oldEntries[id]
		if !existed {
			res.add(describe(id, n, Added))
			continue
		}
		if e, differs := compare(id, o, n); differs {
			res.add(e)
		}
	}
	for _, id := range oldOrder {
		if _, still := newEntries[id]; !still {
			res.add(describe(id, oldEntries[id], Removed))
		}
	}
	return res
}

func (r *Result) add(e Entry) {
	r.Entries = append(r.Entries, e)
	switch e.Kind {
	case Added:
		r.Summary.Added++
	case Removed:
		r.Summary.Removed++
	case Changed:
		r.Summary.Changed++
	case Moved:
		r.Summary.Moved++
	}
}

func entries(f *engine.Flows) (map[string]map[string]any, []string) {
	out := map[string]map[string]any{}
	if f == nil {
		return out, nil
	}
	order := make([]string, 0, len(f.Order))
	for _, id := range f.Order {
		raw, ok := f.Entry(id)
		if !ok {
			continue
		}
		out[id] = raw
		order = append(order, id)
	}
	return out, order
}

func describe(id string, raw map[string]any, kind Kind) Entry {
	e := Entry{ID: id, Kind: kind}
	e.Type, _ = raw["type"].(string)
	e.Z, _ = raw["z"].(string)
	e.Name = displayName(raw)
	return e
}

// displayName is what a person calls the entry: a node's name, a tab's label.
func displayName(raw map[string]any) string {
	for _, k := range []string{"name", "label"} {
		if s, ok := raw[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// layoutKeys are where something sits and how big it's drawn. Moving a node
// changes x and y; resizing a group changes w and h; "l" shows or hides a
// node's label on the canvas. None of them change what runs.
var layoutKeys = map[string]bool{"x": true, "y": true, "w": true, "h": true, "l": true}

// compare diffs one entry present on both sides.
func compare(id string, o, n map[string]any) (Entry, bool) {
	e := describe(id, n, Changed)

	keys := unionKeys(o, n)
	for _, k := range keys {
		ov, oHas := o[k]
		nv, nHas := n[k]
		switch {
		case k == "wires":
			e.Wires = append(e.Wires, wireDiff(ov, nv)...)
		case k == "credentials":
			e.Props = append(e.Props, credentialDiff(ov, nv)...)
		case layoutKeys[k]:
			if !equal(ov, nv) || oHas != nHas {
				e.Layout = append(e.Layout, prop(k, ov, oHas, nv, nHas))
			}
		case e.Type == engine.TypeSubflow && (k == "in" || k == "out" || k == "status"):
			// A subflow's ports carry their own canvas position. Moving a port
			// is layout; rewiring it is not.
			logic, layout := splitPortLayout(k, ov, oHas, nv, nHas)
			e.Props = append(e.Props, logic...)
			e.Layout = append(e.Layout, layout...)
		default:
			e.Props = append(e.Props, valueDiff(k, ov, oHas, nv, nHas)...)
		}
	}

	switch {
	case len(e.Props) > 0 || len(e.Wires) > 0:
		e.Kind = Changed
	case len(e.Layout) > 0:
		e.Kind = Moved
	default:
		return Entry{}, false
	}
	return e, true
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	var keys []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	// A type change is the headline, so it comes first, then where the entry
	// lives, then everything else alphabetically so the same diff always reads
	// the same way.
	rank := func(k string) int {
		switch k {
		case "type":
			return 0
		case "z":
			return 1
		case "g":
			return 2
		case "name", "label":
			return 3
		case "d":
			return 4
		}
		return 5
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := rank(keys[i]), rank(keys[j])
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	return keys
}

func prop(path string, ov any, oHas bool, nv any, nHas bool) Prop {
	return Prop{Path: path, Old: ov, New: nv, HasOld: oHas, HasNew: nHas}
}

// valueDiff walks into objects and arrays so the reported path is the leaf
// that changed, not the whole rule list it lives in.
func valueDiff(path string, ov any, oHas bool, nv any, nHas bool) []Prop {
	if oHas && nHas && equal(ov, nv) {
		return nil
	}
	if !oHas || !nHas {
		return []Prop{prop(path, ov, oHas, nv, nHas)}
	}

	switch o := ov.(type) {
	case map[string]any:
		n, ok := nv.(map[string]any)
		if !ok {
			break
		}
		var out []Prop
		keys := unionKeys(o, n)
		for _, k := range keys {
			ov2, oHas2 := o[k]
			nv2, nHas2 := n[k]
			out = append(out, valueDiff(joinKey(path, k), ov2, oHas2, nv2, nHas2)...)
		}
		return out
	case []any:
		n, ok := nv.([]any)
		if !ok {
			break
		}
		var out []Prop
		for i := 0; i < len(o) || i < len(n); i++ {
			var ov2, nv2 any
			oHas2, nHas2 := i < len(o), i < len(n)
			if oHas2 {
				ov2 = o[i]
			}
			if nHas2 {
				nv2 = n[i]
			}
			out = append(out, valueDiff(path+"["+strconv.Itoa(i)+"]", ov2, oHas2, nv2, nHas2)...)
		}
		return out
	}
	return []Prop{prop(path, ov, true, nv, true)}
}

// joinKey writes a key the way a property expression would: a.b when the key
// is a plain identifier, a["odd key"] when it isn't.
func joinKey(path, key string) string {
	plain := key != ""
	for i, r := range key {
		if !(r == '_' || r == '$' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			plain = false
			break
		}
	}
	if plain {
		return path + "." + key
	}
	q, _ := json.Marshal(key)
	return path + "[" + string(q) + "]"
}

// splitPortLayout diffs a subflow's in, out or status ports, keeping their
// canvas position apart from their wiring.
func splitPortLayout(path string, ov any, oHas bool, nv any, nHas bool) (logic, layout []Prop) {
	for _, p := range valueDiff(path, ov, oHas, nv, nHas) {
		last := p.Path[strings.LastIndexAny(p.Path, ".[")+1:]
		if (last == "x" || last == "y") && strings.Contains(p.Path, ".") {
			layout = append(layout, p)
			continue
		}
		logic = append(logic, p)
	}
	return logic, layout
}

// credentialDiff reports which credential fields changed and never what to.
func credentialDiff(ov, nv any) []Prop {
	o, _ := ov.(map[string]any)
	n, _ := nv.(map[string]any)
	var out []Prop
	for _, k := range unionKeys(o, n) {
		oval, oHas := o[k]
		nval, nHas := n[k]
		if nHas && fmt.Sprint(nval) == credentialSentinel {
			continue
		}
		if oHas && nHas && equal(oval, nval) {
			continue
		}
		out = append(out, Prop{Path: joinKey("credentials", k), HasOld: oHas, HasNew: nHas, Secret: true})
	}
	return out
}

// wireDiff compares output wiring port by port.
func wireDiff(ov, nv any) []Wire {
	o, n := wireSets(ov), wireSets(nv)
	ports := len(o)
	if len(n) > ports {
		ports = len(n)
	}
	var out []Wire
	for p := 0; p < ports; p++ {
		var op, np []string
		if p < len(o) {
			op = o[p]
		}
		if p < len(n) {
			np = n[p]
		}
		for _, to := range np {
			if !contains(op, to) {
				out = append(out, Wire{Port: p, To: to, Added: true})
			}
		}
		for _, to := range op {
			if !contains(np, to) {
				out = append(out, Wire{Port: p, To: to, Added: false})
			}
		}
	}
	return out
}

func wireSets(v any) [][]string {
	ports, _ := v.([]any)
	out := make([][]string, len(ports))
	for i, p := range ports {
		targets, _ := p.([]any)
		for _, t := range targets {
			if s, ok := t.(string); ok {
				out[i] = append(out[i], s)
			}
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func equal(a, b any) bool { return reflect.DeepEqual(a, b) }
