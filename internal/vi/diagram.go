// The block diagram of a VI as a graph: its nodes, their terminals and the wires
// between them. The object classes and tag numbers are from pylabview (MIT, see
// NOTICE). Which primitive a primitive node is, and what a compound arithmetic
// node computes, is NI's own table and is not in the file, so those nodes are
// named by number and reported as not mapped.

package vi

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// Term is one terminal of a node.
type Term struct {
	UID int32 `json:"uid"`
	// Kind is the class of the terminal's own object: parm, iUseDCO, lpTun and so on.
	Kind string `json:"kind"`
	// Input is true for a terminal a wire arrives at.
	Input bool `json:"input"`
	// Param is the terminal's place on the node it belongs to, counted from 1, or 0.
	Param int `json:"param,omitempty"`
}

// Node is one thing on a block diagram.
type Node struct {
	UID int32 `json:"uid"`
	// Class is the object class: prim, iUse (a call of a VI), forLoop, whileLoop,
	// caseSel, cpdArith, or whatever the file says.
	Class string `json:"class"`
	// Label is the node's own text where it has one: the name of the VI a call runs.
	Label string `json:"label,omitempty"`
	// PrimIndex and PrimResID identify a primitive. NI's table of what each does is not in the file.
	PrimIndex int64  `json:"primIndex,omitempty"`
	PrimResID int64  `json:"primResID,omitempty"`
	Terms     []Term `json:"terms"`
	// Bodies are the diagrams a structure holds: one for a loop, one per case for a case structure.
	Bodies []*Diagram `json:"bodies,omitempty"`
	// Panel is "control" or "indicator" for a front panel terminal, with PanelKind
	// the class of its front panel object (stdNum, stdBool, stdString and so on).
	Panel     string `json:"panel,omitempty"`
	PanelKind string `json:"panelKind,omitempty"`
	// Const is the value of a constant and HasConst says it was read.
	Const    any    `json:"const,omitempty"`
	HasConst bool   `json:"hasConst,omitempty"`
	ConstHex string `json:"constHex,omitempty"`
}

// Wire is a wire: from one terminal to the terminals it feeds, by node and
// terminal number in the diagram that holds it.
type Wire struct {
	From  Ref   `json:"from"`
	To    []Ref `json:"to"`
	Known bool  `json:"known"`
}

// Ref names a terminal: the node's uid and the terminal's uid.
type Ref struct {
	Node int32 `json:"node"`
	Term int32 `json:"term"`
}

// Diagram is a block diagram, or the diagram inside one structure.
type Diagram struct {
	Nodes []*Node `json:"nodes"`
	Wires []Wire  `json:"wires"`
}

type builder struct {
	byUID  map[int32]*hnode
	fp     map[int32]*hnode
	types  *typeTable
	owner  map[int32]*Node // terminal uid to the node that holds it
	notes  []string
	budget int
}

// index collects every object of a heap by uid: the objects that are written out
// in full, not the references to them.
func index(n *hnode, into map[int32]*hnode) {
	if n.class() >= 0 && n.uid() >= 0 && len(n.kids) > 0 {
		if _, ok := into[n.uid()]; !ok {
			into[n.uid()] = n
		}
	}
	for _, k := range n.kids {
		index(k, into)
	}
}

func (b *builder) resolve(n *hnode) *hnode {
	if n == nil {
		return nil
	}
	if n.class() >= 0 && len(n.kids) > 0 {
		return n
	}
	if u := n.uid(); u >= 0 {
		if o := b.byUID[u]; o != nil {
			return o
		}
	}
	return n
}

func className(n *hnode) string {
	if n == nil || n.class() < 0 {
		return ""
	}
	if s, ok := classNames[n.class()]; ok {
		return s
	}
	return fmt.Sprintf("class%d", n.class())
}

// elements are the entries of a list tag, with references resolved.
func (b *builder) elements(n *hnode) []*hnode {
	var out []*hnode
	if n == nil {
		return out
	}
	for _, k := range n.kids {
		if k.tag == sysArrayElem {
			out = append(out, b.resolve(k))
		}
	}
	return out
}

// textOf is the first text found under a node, inside a label.
func textOf(n *hnode) string {
	var walk func(*hnode, bool) string
	walk = func(n *hnode, inLabel bool) string {
		if n == nil {
			return ""
		}
		if n.class() == classLabel {
			inLabel = true
		}
		if n.tag == tagText && inLabel && n.up != nil && n.up.class() == classTextHair {
			return strings.Trim(string(n.content), "\"\x00")
		}
		for _, k := range n.kids {
			if s := walk(k, inLabel); s != "" {
				return s
			}
		}
		return ""
	}
	return walk(n, false)
}

func (b *builder) node(n *hnode) *Node {
	out := &Node{UID: n.uid(), Class: className(n)}
	if v, ok := n.field(tagPrimIndex).int(); ok {
		out.PrimIndex = v
	}
	if v, ok := n.field(tagPrimResID).int(); ok {
		out.PrimResID = v
	}
	if l := n.field(tagLabel); l != nil {
		out.Label = textOf(b.resolve(l))
	}
	for _, t := range b.elements(n.field(tagTermList)) {
		tm := Term{UID: t.uid()}
		if f, ok := t.field(tagObjFlags).int(); ok && f&0x8000 != 0 {
			tm.Input = true
		}
		var dco *hnode
		if d := t.field(tagDCO); d != nil {
			dco = b.resolve(d)
			tm.Kind = className(dco)
			if p, ok := dco.field(tagParamIdx).int(); ok {
				tm.Param = int(p)
			}
		}
		if t.class() == classFPTerm {
			out.Panel = "control"
			if d := t.field(tagDCO); d != nil && d.uid() >= 0 {
				if f := b.fp[d.uid()]; f != nil {
					if dd := f.field(tagDDO); dd != nil {
						out.PanelKind = className(dd)
					}
					if out.Label == "" {
						out.Label = textOf(f)
					}
				}
			}
		}
		if dco != nil && dco.class() == classBDConst {
			b.constant(out, dco)
		}
		out.Terms = append(out.Terms, tm)
		b.owner[t.uid()] = out
	}
	return out
}

// constant reads a constant's value, by the type the file gives the constant.
func (b *builder) constant(n *Node, dco *hnode) {
	v := dco.field(tagConstValue)
	if v == nil {
		return
	}
	n.ConstHex = fmt.Sprintf("%X", v.content)
	id, ok := dco.field(tagTypeDesc).int()
	if !ok {
		return
	}
	td := b.types.heapType(id)
	if td == nil {
		return
	}
	c := v.content
	size := map[int]int{tdInt8: 1, tdUInt8: 1, tdInt16: 2, tdUInt16: 2, tdInt32: 4, tdUInt32: 4, tdFloat32: 4, tdInt64: 8, tdUInt64: 8, tdFloat64: 8, tdBoolean: 1}
	switch td.code {
	case tdString:
		if len(c) >= 4 && int(binary.BigEndian.Uint32(c)) == len(c)-4 {
			n.Const, n.HasConst = string(c[4:]), true
		}
		return
	}
	sz, known := size[td.code]
	if !known || len(c) != sz {
		return
	}
	var u uint64
	for _, x := range c {
		u = u<<8 | uint64(x)
	}
	switch td.code {
	case tdInt8:
		n.Const = int64(int8(u))
	case tdInt16:
		n.Const = int64(int16(u))
	case tdInt32:
		n.Const = int64(int32(u))
	case tdInt64:
		n.Const = int64(u)
	case tdUInt8, tdUInt16, tdUInt32, tdUInt64:
		n.Const = u
	case tdFloat32:
		n.Const = float64(math.Float32frombits(uint32(u)))
	case tdFloat64:
		n.Const = math.Float64frombits(u)
	case tdBoolean:
		n.Const = u != 0
	}
	n.HasConst = true
}

// diagram reads one diag object: its nodes, structures and signals.
func (b *builder) diagram(d *hnode) *Diagram {
	out := &Diagram{}
	b.budget--
	if b.budget < 0 {
		b.notes = append(b.notes, "the diagram nests deeper than is read")
		return out
	}
	// Every node of the diagram is in its z plane list (the nodeList holds only
	// the top ones, as references); the structures hold their own diagrams.
	seen := map[int32]bool{}
	add := func(e *hnode) {
		if e == nil || e.class() < 0 || seen[e.uid()] {
			return
		}
		switch e.class() {
		case classTerm, classFPTerm, classSignal, classLabel:
			return
		}
		if len(e.kids) == 0 || e.field(tagTermList) == nil {
			return
		}
		seen[e.uid()] = true
		if e.class() == classSRN {
			// The diagram's own region holds the front panel terminals and the
			// constants, each of which is a node of its own on the canvas.
			whole := b.node(e)
			for _, t := range b.elements(e.field(tagTermList)) {
				out.Nodes = append(out.Nodes, b.termNode(t, whole))
			}
			return
		}
		nd := b.node(e)
		for _, dl := range b.elements(e.field(tagDiagramList)) {
			nd.Bodies = append(nd.Bodies, b.diagram(dl))
		}
		out.Nodes = append(out.Nodes, nd)
	}
	for _, e := range b.elements(d.field(tagNodeList)) {
		add(e)
	}
	for _, e := range b.elements(d.field(tagZPlaneList)) {
		add(e)
	}
	// A structure node's own terminals (its tunnels) are wired in the diagram
	// that holds the structure, and its body's wires end on them too.
	for _, s := range b.elements(d.field(tagSignalList)) {
		if s.class() != classSignal {
			continue
		}
		var refs []int32
		for _, t := range b.elements(s.field(tagTermList)) {
			refs = append(refs, t.uid())
		}
		if len(refs) < 2 {
			continue
		}
		out.Wires = append(out.Wires, Wire{Known: true, From: Ref{Term: refs[0]}, To: refsOf(refs[1:])})
	}
	return out
}

func refsOf(u []int32) []Ref {
	out := make([]Ref, len(u))
	for i, x := range u {
		out[i] = Ref{Term: x}
	}
	return out
}

// settle fills each wire's node numbers from the terminal numbers and picks the
// source: the one terminal that is not an input of its node.
func settle(d *Diagram, owner map[int32]*Node, terms map[int32]Term) {
	for i := range d.Wires {
		w := &d.Wires[i]
		all := append([]Ref{w.From}, w.To...)
		src := -1
		for k, r := range all {
			if t, ok := terms[r.Term]; ok && !t.Input {
				src = k
				break
			}
		}
		if src < 0 {
			w.Known = false
			src = 0
		}
		all[0], all[src] = all[src], all[0]
		for k := range all {
			if n := owner[all[k].Term]; n != nil {
				all[k].Node = n.UID
			}
		}
		w.From, w.To = all[0], all[1:]
	}
	for _, n := range d.Nodes {
		for _, b := range n.Bodies {
			settle(b, owner, terms)
		}
	}
}

// panelDirections decides which front panel terminals are controls and which
// are indicators: a control starts a wire and an indicator ends one.
func panelDirections(d *Diagram, byTerm map[int32]*Node) {
	for _, w := range d.Wires {
		if n := byTerm[w.From.Term]; n != nil && n.Panel != "" {
			n.Panel = "control"
		}
		for _, r := range w.To {
			if n := byTerm[r.Term]; n != nil && n.Panel != "" {
				n.Panel = "indicator"
			}
		}
	}
	for _, n := range d.Nodes {
		for _, b := range n.Bodies {
			panelDirections(b, byTerm)
		}
	}
}

// readDiagram builds the graph of a VI's block diagram.
func readDiagram(bd, fp *hnode, types *typeTable) (*Diagram, []string, error) {
	b := &builder{byUID: map[int32]*hnode{}, fp: map[int32]*hnode{}, types: types, owner: map[int32]*Node{}, budget: 256}
	index(bd, b.byUID)
	index(fp, b.fp)
	var root *hnode
	for _, o := range bd.kids {
		if o.tag == sysRootObject {
			root = o.field(tagRoot)
		}
	}
	if root == nil {
		return nil, nil, fmt.Errorf("the block diagram has no root")
	}
	d := b.diagram(root)
	terms := map[int32]Term{}
	byTerm := map[int32]*Node{}
	var collect func(*Diagram)
	collect = func(d *Diagram) {
		for _, n := range d.Nodes {
			for _, t := range n.Terms {
				terms[t.UID] = t
				byTerm[t.UID] = n
			}
			for _, bd := range n.Bodies {
				collect(bd)
			}
		}
	}
	collect(d)
	settle(d, byTerm, terms)
	panelDirections(d, byTerm)
	return d, b.notes, nil
}

// termNode makes a node of one terminal of the diagram's region: a front panel
// terminal, or a constant. whole is the region read as a single node, which
// holds each terminal in the same order.
func (b *builder) termNode(t *hnode, whole *Node) *Node {
	for _, tn := range whole.Terms {
		if tn.UID != t.uid() {
			continue
		}
		n := &Node{UID: t.uid(), Class: className(t), Terms: []Term{tn}}
		if t.class() == classFPTerm {
			n.Panel = "control"
			if d := t.field(tagDCO); d != nil && d.uid() >= 0 {
				if f := b.fp[d.uid()]; f != nil {
					if dd := f.field(tagDDO); dd != nil {
						n.PanelKind = className(dd)
					}
					n.Label = textOf(f)
				}
			}
		} else if d := t.field(tagDCO); d != nil {
			if dco := b.resolve(d); dco.class() == classBDConst {
				n.Class = "constant"
				b.constant(n, dco)
			}
		}
		b.owner[t.uid()] = n
		return n
	}
	return &Node{UID: t.uid(), Class: className(t)}
}
