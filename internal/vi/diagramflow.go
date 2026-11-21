package vi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A FlowLab diagram is a tab whose nodes are dia-* entries. What the VI's block
// diagram holds that FlowLab has a node for comes across as that node: constants,
// controls and indicators. Everything else comes across as a dia-lv node in the
// place it had, with its wires, so the structure is the VI's own and the diagram
// reports every such node as a problem until a person replaces it.

// lvNode is a node as it will be written.
type lvNode struct {
	id      string
	typ     string
	name    string
	extra   map[string]any
	ins     []int32 // terminal uids of the input ports, in port order
	outs    []int32
	level   int
	outWire [][]string
	outIn   [][]string
}

// diagramTabs writes one diagram and the diagrams of its structures as tabs.
func (in *Info) diagramTabs(slugID string) ([]map[string]any, []string) {
	var out []map[string]any
	var unmapped []string
	counts := map[string]int{}
	var calls []string
	var prims = map[int64]int{}
	panelOther := map[string]bool{}
	tabN := 0
	var emit func(d *Diagram, tabID, label string, body bool)
	emit = func(d *Diagram, tabID, label string, body bool) {
		meta := map[string]any{}
		if body {
			meta["role"] = "body"
		}
		out = append(out, map[string]any{"id": tabID, "type": "tab", "label": label, "disabled": false, "info": "", "ew_diagram": meta})
		var nodes []*lvNode
		byTerm := map[int32]*lvNode{}
		isOut := map[int32]bool{}
		// A terminal is an input when a wire of this diagram ends on it and an
		// output when one starts there, which settles the tunnels of a structure
		// (an input on one side and an output on the other). A terminal no wire
		// touches goes by the flag the file keeps on it.
		src, dst := map[int32]bool{}, map[int32]bool{}
		for _, w := range d.Wires {
			src[w.From.Term] = true
			for _, r := range w.To {
				dst[r.Term] = true
			}
		}
		isInput := func(t Term) bool {
			if dst[t.UID] && !src[t.UID] {
				return true
			}
			if src[t.UID] && !dst[t.UID] {
				return false
			}
			return t.Input
		}
		for _, n := range d.Nodes {
			ln := &lvNode{id: fmt.Sprintf("%s-n%d", tabID, n.UID), extra: map[string]any{}}
			input := isInput
			switch {
			case n.Panel != "" && panelType(n) != "":
				ln.typ = panelType(n)
				ln.name = n.Label
				ln.extra["label"] = n.Label
				ln.extra["kind"] = panelKind(n)
				ln.ins, ln.outs = portsOf(n, "value", input)
			case n.HasConst:
				ln.typ = "dia-const"
				b, _ := json.Marshal(n.Const)
				ln.extra["value"] = string(b)
				ln.name = ""
				_, ln.outs = portsOf(n, "value", input)
			default:
				ln.typ = "dia-lv"
				ln.name = lvName(n)
				ln.extra["lvclass"] = n.Class
				if n.Panel != "" {
					ln.extra["lvclass"] = "front panel " + n.PanelKind
					panelOther[n.PanelKind] = true
				}
				var ins, outs []string
				for _, t := range n.Terms {
					if dst[t.UID] && src[t.UID] {
						ln.ins = append(ln.ins, t.UID)
						ins = append(ins, fmt.Sprintf("in%d", len(ins)+1))
						ln.outs = append(ln.outs, t.UID)
						outs = append(outs, fmt.Sprintf("out%d", len(outs)+1))
					} else if isInput(t) {
						ln.ins = append(ln.ins, t.UID)
						ins = append(ins, fmt.Sprintf("in%d", len(ins)+1))
					} else {
						ln.outs = append(ln.outs, t.UID)
						outs = append(outs, fmt.Sprintf("out%d", len(outs)+1))
					}
				}
				ln.extra["inputs"] = strings.Join(ins, ",")
				ln.extra["outputs"] = strings.Join(outs, ",")
				switch {
				case n.Class == "iUse":
					calls = append(calls, strings.TrimSpace(n.Label))
				case n.Class == "prim":
					prims[n.PrimIndex]++
					ln.extra["lvprim"] = fmt.Sprintf("primitive %d (resource %d)", n.PrimIndex, n.PrimResID)
				default:
					counts[n.Class]++
				}
				if len(n.Bodies) > 0 {
					var labels []string
					for i, b := range n.Bodies {
						tabN++
						id := fmt.Sprintf("%s-body%d", slugID, tabN)
						bl := fmt.Sprintf("%s %s %d", label, strings.ToLower(n.Class), i+1)
						emit(b, id, clip(bl), true)
						labels = append(labels, clip(bl))
					}
					ln.extra["bodies"] = strings.Join(labels, ", ")
				}
			}
			nodes = append(nodes, ln)
			for _, u := range ln.outs {
				isOut[u] = true
				byTerm[u] = ln
			}
			for _, u := range ln.ins {
				byTerm[u] = ln
			}
		}
		// Terminals the wires of this diagram end on that belong to something
		// outside it, such as the tunnels of the structure that holds this body.
		edge := func(u int32, out bool) *lvNode {
			if ln := byTerm[u]; ln != nil {
				return ln
			}
			ln := &lvNode{id: fmt.Sprintf("%s-t%d", tabID, u), typ: "dia-lv", name: "tunnel", extra: map[string]any{"lvclass": "boundary terminal"}}
			if out {
				ln.outs = []int32{u}
				ln.extra["inputs"], ln.extra["outputs"] = "", "out1"
			} else {
				ln.ins = []int32{u}
				ln.extra["inputs"], ln.extra["outputs"] = "in1", ""
			}
			nodes = append(nodes, ln)
			byTerm[u] = ln
			return ln
		}
		for _, w := range d.Wires {
			src := edge(w.From.Term, true)
			oi := indexOf(src.outs, w.From.Term)
			if oi < 0 {
				// a terminal of a node that is not an output of it: wire from the node's first output
				oi = 0
			}
			for len(src.outWire) <= oi {
				src.outWire = append(src.outWire, nil)
				src.outIn = append(src.outIn, nil)
			}
			for _, to := range w.To {
				dst := edge(to.Term, false)
				ii := indexOf(dst.ins, to.Term)
				if ii < 0 {
					ii = 0
				}
				src.outWire[oi] = append(src.outWire[oi], dst.id)
				src.outIn[oi] = append(src.outIn[oi], portName(dst, ii))
			}
		}
		levels(nodes)
		rows := map[int]int{}
		for _, ln := range nodes {
			n := map[string]any{"id": ln.id, "type": ln.typ, "z": tabID, "x": 120 + ln.level*220, "y": 80 + rows[ln.level]*90}
			rows[ln.level]++
			if ln.name != "" {
				n["name"] = ln.name
			}
			for k, v := range ln.extra {
				n[k] = v
			}
			nOut := len(ln.outs)
			if len(ln.outWire) > nOut {
				nOut = len(ln.outWire)
			}
			wires := make([][]string, nOut)
			ewin := make([][]string, nOut)
			for i := 0; i < nOut; i++ {
				wires[i], ewin[i] = []string{}, []string{}
				if i < len(ln.outWire) {
					wires[i], ewin[i] = append(wires[i], ln.outWire[i]...), append(ewin[i], ln.outIn[i]...)
				}
			}
			n["wires"], n["ew_in"] = wires, ewin
			out = append(out, n)
		}
	}
	emit(in.Diagram, slugID, clipLabel(strings.TrimSuffix(in.Name, ".vi"))+" diagram", false)
	// What was not mapped, by kind, so the dialog says it in a few lines.
	if len(prims) > 0 {
		var ks []string
		total := 0
		var idx []int
		for k, c := range prims {
			total += c
			idx = append(idx, int(k))
		}
		sort.Ints(idx)
		for _, k := range idx {
			ks = append(ks, fmt.Sprint(k))
		}
		unmapped = append(unmapped, fmt.Sprintf("%d primitive node(s), of primitive numbers %s: they are on the diagram with their wires, and what each one does is NI's own table, not in the file, so each stays a LabVIEW node that stops the diagram until it is replaced", total, strings.Join(ks, ", ")))
	}
	if len(calls) > 0 {
		unmapped = append(unmapped, fmt.Sprintf("%d call(s) of other VIs (%s): each is on the diagram with its wires and stops it until it is replaced; the VI called is another file", len(calls), strings.Join(dedupe(calls), ", ")))
	}
	var cs []string
	for k, c := range counts {
		cs = append(cs, fmt.Sprintf("%d %s", c, k))
	}
	sort.Strings(cs)
	if len(cs) > 0 {
		unmapped = append(unmapped, "structures and other nodes kept as LabVIEW nodes with their wires, which stop the diagram until replaced: "+strings.Join(cs, ", "))
	}
	if len(panelOther) > 0 {
		var ks []string
		for k := range panelOther {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		unmapped = append(unmapped, "front panel objects of a kind FlowLab has no control for ("+strings.Join(ks, ", ")+"), kept as LabVIEW nodes")
	}
	return out, unmapped
}

func panelType(n *Node) string {
	switch n.PanelKind {
	case "stdNum", "stdColorNum", "stdBool", "stdString":
		if n.Panel == "indicator" {
			return "dia-indicator"
		}
		return "dia-control"
	}
	return ""
}

func panelKind(n *Node) string {
	switch n.PanelKind {
	case "stdBool":
		return "bool"
	case "stdString":
		return "string"
	}
	return "number"
}

// portsOf gives the input and the output terminal uids of a node that has one
// port named name.
func portsOf(n *Node, name string, input func(Term) bool) (ins, outs []int32) {
	for _, t := range n.Terms {
		if input(t) {
			ins = append(ins, t.UID)
		} else {
			outs = append(outs, t.UID)
		}
	}
	return
}

func lvName(n *Node) string {
	switch n.Class {
	case "iUse":
		if n.Label != "" {
			return "calls " + strings.TrimSpace(n.Label)
		}
		return "call of a VI"
	case "prim":
		return fmt.Sprintf("primitive %d", n.PrimIndex)
	}
	if n.Label != "" {
		return n.Class + " " + n.Label
	}
	return n.Class
}

func portName(n *lvNode, i int) string {
	if n.typ == "dia-lv" {
		return fmt.Sprintf("in%d", i+1)
	}
	return "value"
}

func indexOf(s []int32, v int32) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if x == "" {
			x = "(unnamed)"
		}
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func clip(s string) string { return clipLabel(s) }

// levels sets each node's column: how many wires it is from a node that has no input.
func levels(nodes []*lvNode) {
	byID := map[string]*lvNode{}
	for _, ln := range nodes {
		byID[ln.id] = ln
	}
	for pass := 0; pass <= len(nodes); pass++ {
		changed := false
		for _, ln := range nodes {
			for _, outs := range ln.outWire {
				for _, id := range outs {
					if t := byID[id]; t != nil && t.level < ln.level+1 && ln.level < len(nodes) {
						t.level = ln.level + 1
						changed = true
					}
				}
			}
		}
		if !changed {
			break
		}
	}
}
