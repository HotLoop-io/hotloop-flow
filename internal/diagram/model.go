// Package diagram is the second way Flow runs a graph.
//
// The message runtime sends one message at a time through a node and any input
// wakes it. A diagram is the other model: every node runs once per run, when each
// of its wired inputs holds a value, and independent branches run at the same
// time. That is what a loop with carried state, an error wire, a panel of
// controls and indicators, probes and breakpoints need, and none of it fits the
// message model without changing what existing flows do. So it lives beside it.
//
// A diagram is a tab that carries the key ew_diagram. Its nodes are ordinary
// entries of the flow file: id, type, x, y and wires. Wires name the target node.
// Which input of that node a wire lands on is the one new thing, and it is kept in
// ew_in on the source node, in the same shape as wires, so a tool that does not
// know about it carries it through untouched and the file round trips byte for
// byte.
package diagram

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// InKey is the node key that says which input port each wire lands on.
// InKey[output][i] is the input port name of the target Wires[output][i].
const InKey = "ew_in"

// PanelKey is the node key that places a control or an indicator on the panel.
const PanelKey = "ew_panel"

// Ports every node has, on top of the ones its type declares. After is an
// optional input that holds a node back until another has finished, and Done is
// an output that says the node has. They are how order is set where no data
// flows, and they are drawn on the wire like any other.
const (
	PortAfter = "after"
	PortDone  = "done"
	PortErr   = "err"
)

// Port is one named input or output of a node.
type Port struct {
	Name     string
	Optional bool
}

// ExpandPorts turns a descriptor's port list into the names a particular node has.
// An entry starting with "@" names a property whose comma-separated value adds
// ports, so a loop with the carried values "sum,window" has an input and an output
// for each. A trailing "?" marks an input optional.
func ExpandPorts(spec []string, raw map[string]any) []Port {
	var out []Port
	seen := map[string]bool{}
	add := func(name string, optional bool) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, Port{Name: name, Optional: optional})
	}
	for _, s := range spec {
		opt := strings.HasSuffix(s, "?")
		s = strings.TrimSuffix(s, "?")
		if strings.HasPrefix(s, "@") {
			v, _ := raw[strings.TrimPrefix(s, "@")].(string)
			for _, name := range strings.Split(v, ",") {
				add(name, opt)
			}
			continue
		}
		add(s, opt)
	}
	return out
}

// InPorts returns a node's input ports: the type's, then after.
func InPorts(d node.Descriptor, raw map[string]any) []Port {
	return append(ExpandPorts(d.InputPorts, raw), Port{Name: PortAfter, Optional: true})
}

// OutPorts returns a node's output ports: the type's, then done.
func OutPorts(d node.Descriptor, raw map[string]any) []Port {
	return append(ExpandPorts(d.OutputPorts, raw), Port{Name: PortDone})
}

// Wire is one connection in a diagram.
type Wire struct {
	// Key names the wire for probes, breakpoints and the editor. It is the
	// source node, its output port and the target and its input port.
	Key      string
	From     string
	FromPort string
	To       string
	ToPort   string
}

// DNode is a node of a compiled diagram.
type DNode struct {
	ID    string
	Tab   string // the diagram this node is on
	Type  string
	Name  string
	Raw   map[string]any
	Kind  *Kind
	In    []Port
	Out   []Port
	Op    Op
	Needs []string // the input ports that have a wire, in port order
}

// Diagram is a tab compiled for running. It is immutable: a run reads it, and a
// deploy compiles a new one.
type Diagram struct {
	ID       string
	Label    string
	Settings map[string]any
	Nodes    map[string]*DNode
	Order    []string // file order
	Wires    []Wire
	// Into[node][port] is the wire landing there; there is at most one.
	Into map[string]map[string]*Wire
	// From[node][port] lists the wires leaving there.
	From map[string]map[string][]*Wire
	// Problems are why the diagram cannot run, each naming a node.
	Problems []Problem
	// Bodies lists the diagrams this one runs inside its structures, by tab id.
	Bodies []string
}

// Problem is one reason a diagram cannot run, for the error list.
type Problem struct {
	Node string `json:"node,omitempty"`
	Port string `json:"port,omitempty"`
	Text string `json:"text"`
}

func (p Problem) String() string {
	switch {
	case p.Node != "" && p.Port != "":
		return fmt.Sprintf("%s input %s: %s", p.Node, p.Port, p.Text)
	case p.Node != "":
		return fmt.Sprintf("%s: %s", p.Node, p.Text)
	}
	return p.Text
}

// Set holds every compiled diagram of a flow file.
type Set struct {
	Diagrams map[string]*Diagram
	Order    []string
}

// CompileAll compiles every diagram tab in the flow file.
func CompileAll(flows *engine.Flows) *Set {
	s := &Set{Diagrams: map[string]*Diagram{}}
	if flows == nil {
		return s
	}
	var ids []string
	for id, t := range flows.Tabs {
		if t.IsDiagram() {
			ids = append(ids, id)
		}
	}
	// File order, so the list in the editor and the API is the order of the tabs.
	pos := map[string]int{}
	for i, id := range flows.Order {
		pos[id] = i
	}
	sort.Slice(ids, func(i, j int) bool { return pos[ids[i]] < pos[ids[j]] })
	for _, id := range ids {
		s.Diagrams[id] = compile(flows, id)
		s.Order = append(s.Order, id)
	}
	for _, id := range s.Order {
		d := s.Diagrams[id]
		for _, nid := range d.Order {
			if dn := d.Nodes[nid]; dn.Kind.Check != nil {
				d.Problems = append(d.Problems, dn.Kind.Check(s, d, dn)...)
			}
		}
	}
	// A structure inherits the problems of its body: a loop that runs a broken
	// diagram is broken too, and says so where the person is looking. Repeated, so
	// a body inside a body reaches the top.
	defer func() {
		for pass := 0; pass < len(s.Order); pass++ {
			changed := false
			for _, id := range s.Order {
				d := s.Diagrams[id]
				for _, nid := range d.Order {
					dn := d.Nodes[nid]
					if !dn.Kind.Structure {
						continue
					}
					b, ok := s.Diagrams[strProp(dn.Raw, "body", "")]
					if !ok || len(b.Problems) == 0 || hasProblem(d, nid, "the body") {
						continue
					}
					d.Problems = append(d.Problems, Problem{Node: nid, Text: "the body " + strconv.Quote(b.Label) + " cannot run: " + b.Problems[0].String()})
					changed = true
				}
			}
			if !changed {
				return
			}
		}
	}()
	// A structure names its body by tab id, so check each body is a diagram.
	for _, id := range s.Order {
		d := s.Diagrams[id]
		for _, b := range d.Bodies {
			if _, ok := s.Diagrams[b]; !ok {
				d.Problems = append(d.Problems, Problem{Text: fmt.Sprintf("a structure names the body %q, which is not a diagram", b)})
			}
		}
	}
	return s
}

func compile(flows *engine.Flows, tabID string) *Diagram {
	tab := flows.Tabs[tabID]
	d := &Diagram{
		ID:    tabID,
		Label: tab.Label,
		Nodes: map[string]*DNode{},
		Into:  map[string]map[string]*Wire{},
		From:  map[string]map[string][]*Wire{},
	}
	d.Settings, _ = tab.Diagram()

	for _, id := range flows.Order {
		n, ok := flows.Nodes[id]
		if !ok || n.Z != tabID || n.IsConfig || n.Disabled {
			continue
		}
		k, known := kinds[n.Type]
		if !known {
			d.Problems = append(d.Problems, Problem{Node: id, Text: fmt.Sprintf("the type %q does not run in a diagram", n.Type)})
			continue
		}
		dn := &DNode{ID: id, Tab: tabID, Type: n.Type, Name: n.Name, Raw: n.Raw, Kind: k}
		dn.In = InPorts(k.Descriptor, n.Raw)
		dn.Out = OutPorts(k.Descriptor, n.Raw)
		d.Nodes[id] = dn
		d.Order = append(d.Order, id)
	}

	for _, id := range d.Order {
		n := flows.Nodes[id]
		dn := d.Nodes[id]
		ins := wireInputs(n.Raw[InKey])
		for oi, targets := range n.Wires {
			if oi >= len(dn.Out) {
				d.Problems = append(d.Problems, Problem{Node: id, Text: fmt.Sprintf("wires from output %d, and the node has %d outputs", oi, len(dn.Out))})
				continue
			}
			for ti, to := range targets {
				tn, ok := d.Nodes[to]
				if !ok {
					d.Problems = append(d.Problems, Problem{Node: id, Text: fmt.Sprintf("a wire goes to %q, which is not on this diagram", to)})
					continue
				}
				port := ""
				if oi < len(ins) && ti < len(ins[oi]) {
					port = ins[oi][ti]
				}
				if port == "" {
					port = tn.In[0].Name
				}
				if !hasPort(tn.In, port) {
					d.Problems = append(d.Problems, Problem{Node: to, Port: port, Text: fmt.Sprintf("a wire from %s lands on an input this node does not have", id)})
					continue
				}
				w := &Wire{From: id, FromPort: dn.Out[oi].Name, To: to, ToPort: port}
				w.Key = fmt.Sprintf("%s.%s>%s.%s", w.From, w.FromPort, w.To, w.ToPort)
				if d.Into[to] == nil {
					d.Into[to] = map[string]*Wire{}
				}
				if prev, taken := d.Into[to][port]; taken {
					d.Problems = append(d.Problems, Problem{Node: to, Port: port, Text: fmt.Sprintf("two wires land here, from %s and from %s", prev.From, id)})
					continue
				}
				d.Into[to][port] = w
				if d.From[id] == nil {
					d.From[id] = map[string][]*Wire{}
				}
				d.From[id][w.FromPort] = append(d.From[id][w.FromPort], w)
				d.Wires = append(d.Wires, *w)
			}
		}
	}

	for _, id := range d.Order {
		dn := d.Nodes[id]
		for _, p := range dn.In {
			if _, wired := d.Into[id][p.Name]; wired {
				dn.Needs = append(dn.Needs, p.Name)
			} else if !p.Optional {
				d.Problems = append(d.Problems, Problem{Node: id, Port: p.Name, Text: "a required input has no wire"})
			}
		}
		if dn.Kind.New != nil {
			op, err := dn.Kind.New(dn.Raw)
			if err != nil {
				d.Problems = append(d.Problems, Problem{Node: id, Text: err.Error()})
			}
			dn.Op = op
		}
		if b, ok := dn.Raw["body"].(string); ok && b != "" && dn.Kind.Structure {
			d.Bodies = append(d.Bodies, b)
		}
	}
	d.Problems = append(d.Problems, cycles(d)...)
	return d
}

// cycles finds nodes that wait on themselves. In a diagram a node runs once, when
// its inputs are ready, so a wire that comes back round to an earlier node could
// never be ready. Carrying a value from one pass to the next is what a loop
// structure is for.
func cycles(d *Diagram) []Problem {
	indeg := map[string]int{}
	for _, id := range d.Order {
		indeg[id] = 0
	}
	for _, w := range d.Wires {
		indeg[w.To]++
	}
	var queue []string
	for _, id := range d.Order {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for _, ws := range d.From[id] {
			for _, w := range ws {
				if indeg[w.To]--; indeg[w.To] == 0 {
					queue = append(queue, w.To)
				}
			}
		}
	}
	if seen == len(d.Order) {
		return nil
	}
	var out []Problem
	for _, id := range d.Order {
		if indeg[id] > 0 {
			out = append(out, Problem{Node: id, Text: "this node is in a wire loop, so it could never be ready; carry the value through a loop structure instead"})
		}
	}
	return out
}

func hasProblem(d *Diagram, node, prefix string) bool {
	for _, p := range d.Problems {
		if p.Node == node && strings.HasPrefix(p.Text, prefix) {
			return true
		}
	}
	return false
}

func hasPort(ps []Port, name string) bool {
	for _, p := range ps {
		if p.Name == name {
			return true
		}
	}
	return false
}

// wireInputs reads ew_in: for each output, the input port name of each wire.
func wireInputs(v any) [][]string {
	arr, _ := v.([]any)
	out := make([][]string, len(arr))
	for i, a := range arr {
		inner, _ := a.([]any)
		for _, s := range inner {
			str, _ := s.(string)
			out[i] = append(out[i], str)
		}
	}
	return out
}

// Terminals returns the panel controls and indicators of a diagram and, in order,
// of the bodies it runs inside its structures.
func (s *Set) Terminals(id string) []*DNode {
	var out []*DNode
	seen := map[string]bool{}
	var walk func(string)
	walk = func(tab string) {
		if seen[tab] {
			return
		}
		seen[tab] = true
		d, ok := s.Diagrams[tab]
		if !ok {
			return
		}
		for _, nid := range d.Order {
			if dn := d.Nodes[nid]; dn.Kind.Terminal {
				out = append(out, dn)
			}
		}
		for _, b := range d.Bodies {
			walk(b)
		}
	}
	walk(id)
	return out
}
