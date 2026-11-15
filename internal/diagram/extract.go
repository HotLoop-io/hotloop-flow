package diagram

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractSub makes a sub-diagram out of some nodes of a diagram and returns the
// new flow file. The nodes move into a new body tab, a dia-sub takes their place,
// and the connector is worked out from the wires that cross the edge of the
// selection: a wire coming in becomes an input (one for each source output, even
// when it feeds several selected nodes), and a wire going out becomes an output
// (one for each selected output, however many nodes outside it feeds). The wires
// outside are reconnected to the new node, so a run gives the answers it gave
// before.
func ExtractSub(doc []byte, tab string, ids []string, name string) ([]byte, error) {
	var entries []map[string]any
	if err := json.Unmarshal(doc, &entries); err != nil {
		return nil, fmt.Errorf("the flow file does not parse: %w", err)
	}
	byID := map[string]map[string]any{}
	for _, e := range entries {
		if id, _ := e["id"].(string); id != "" {
			byID[id] = e
		}
	}
	t, ok := byID[tab]
	if !ok || t["type"] != "tab" || t["ew_diagram"] == nil {
		return nil, fmt.Errorf("%q is not a diagram", tab)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("select at least one node")
	}
	sel := map[string]bool{}
	var cx, cy float64
	for _, id := range ids {
		e, ok := byID[id]
		if !ok || e["z"] != tab {
			return nil, fmt.Errorf("%q is not a node of %q", id, tab)
		}
		if _, known := kinds[e["type"].(string)]; !known {
			return nil, fmt.Errorf("%q is a %s, which does not run in a diagram", id, e["type"])
		}
		sel[id] = true
		x, _ := num(e["x"])
		y, _ := num(e["y"])
		cx += x
		cy += y
	}
	cx, cy = cx/float64(len(sel)), cy/float64(len(sel))

	name = strings.TrimSpace(name)
	if name == "" {
		name = "Sub-diagram"
	}
	labels := map[string]bool{}
	for _, e := range entries {
		if l, _ := e["label"].(string); l != "" && e["type"] == "tab" {
			labels[l] = true
		}
	}
	for i, base := 2, name; labels[name]; i++ {
		name = fmt.Sprintf("%s %d", base, i)
	}
	fresh := func(prefix string) string {
		for i := 1; ; i++ {
			id := fmt.Sprintf("%s-%d", prefix, i)
			if _, taken := byID[id]; !taken {
				byID[id] = nil
				return id
			}
		}
	}
	bodyID, subID := fresh("sub-body"), fresh("sub")

	// The nodes of the diagram, in file order, so the same selection always makes
	// the same sub-diagram.
	var order []string
	for _, e := range entries {
		if id, _ := e["id"].(string); id != "" && e["z"] == tab {
			order = append(order, id)
		}
	}
	outPorts := func(e map[string]any) []Port {
		k := kinds[e["type"].(string)]
		return OutPorts(k.Descriptor, e)
	}
	inPorts := func(e map[string]any) []Port {
		k := kinds[e["type"].(string)]
		return InPorts(k.Descriptor, e)
	}

	type edge struct {
		src         string
		oi          int
		oname       string
		target      string
		port, label string
	}
	var edges []edge
	for _, id := range order {
		e := byID[id]
		ports := outPorts(e)
		wires, _ := e["wires"].([]any)
		ins := wireInputs(e[InKey])
		for oi, w := range wires {
			targets, _ := w.([]any)
			for ti, tv := range targets {
				to, _ := tv.(string)
				te, ok := byID[to]
				if !ok || te == nil {
					continue
				}
				port := ""
				if oi < len(ins) && ti < len(ins[oi]) {
					port = ins[oi][ti]
				}
				if port == "" {
					port = inPorts(te)[0].Name
				}
				oname := fmt.Sprintf("%d", oi)
				if oi < len(ports) {
					oname = ports[oi].Name
				}
				edges = append(edges, edge{src: id, oi: oi, oname: oname, target: to, port: port})
			}
		}
	}

	used := map[string]bool{}
	pick := func(base string) string {
		n := base
		for i := 2; used[n]; i++ {
			n = fmt.Sprintf("%s%d", base, i)
		}
		used[n] = true
		return n
	}
	type conn struct{ name, id string }
	inputs := map[string]conn{}  // by source output
	outputs := map[string]conn{} // by selected output
	var inNames, outNames []string
	var bodyIns, bodyOuts []map[string]any
	for _, ed := range edges {
		key := fmt.Sprintf("%s\x00%d", ed.src, ed.oi)
		switch {
		case !sel[ed.src] && sel[ed.target]:
			if _, ok := inputs[key]; !ok {
				n := pick(ed.port)
				inputs[key] = conn{name: n, id: fresh("sub-in")}
				inNames = append(inNames, n)
				bodyIns = append(bodyIns, map[string]any{
					"id": inputs[key].id, "type": "dia-body-in", "z": bodyID, "x": 60.0, "y": 60.0 + 80.0*float64(len(inNames)),
					"name": n, "wires": []any{[]any{}, []any{}}, InKey: []any{[]any{}, []any{}},
				})
			}
		case sel[ed.src] && !sel[ed.target]:
			if _, ok := outputs[key]; !ok {
				n := pick(ed.src + "_" + ed.oname)
				outputs[key] = conn{name: n, id: fresh("sub-out")}
				outNames = append(outNames, n)
				bodyOuts = append(bodyOuts, map[string]any{
					"id": outputs[key].id, "type": "dia-body-out", "z": bodyID, "x": 900.0, "y": 60.0 + 80.0*float64(len(outNames)),
					"name": n, "wires": []any{[]any{}}, InKey: []any{[]any{}},
				})
			}
		}
	}

	// Rebuild every wire list of the diagram's nodes.
	type slot struct {
		targets []any
		ports   []any
	}
	rebuilt := map[string][]slot{}
	add := func(owner string, oi int, to, port string) {
		s := rebuilt[owner]
		for len(s) <= oi {
			s = append(s, slot{})
		}
		for i, t := range s[oi].targets {
			if t == to && s[oi].ports[i] == port {
				rebuilt[owner] = s
				return // one wire to one input from one output
			}
		}
		s[oi].targets = append(s[oi].targets, to)
		s[oi].ports = append(s[oi].ports, port)
		rebuilt[owner] = s
	}
	byBodyIn := map[string]map[string]any{}
	for _, b := range bodyIns {
		byBodyIn[b["id"].(string)] = b
	}
	subOut := map[string]int{} // output name to its place on the sub-diagram node
	for i, n := range outNames {
		subOut[n] = i
	}
	for _, id := range order {
		rebuilt[id] = make([]slot, len(outPorts(byID[id])))
	}
	for _, ed := range edges {
		key := fmt.Sprintf("%s\x00%d", ed.src, ed.oi)
		switch {
		case sel[ed.src] && sel[ed.target], !sel[ed.src] && !sel[ed.target]:
			add(ed.src, ed.oi, ed.target, ed.port)
		case !sel[ed.src] && sel[ed.target]:
			c := inputs[key]
			add(ed.src, ed.oi, subID, c.name)
			add(c.id, 0, ed.target, ed.port)
		default:
			c := outputs[key]
			add(ed.src, ed.oi, c.id, "value")
			add(subID, subOut[c.name], ed.target, ed.port)
		}
	}
	apply := func(e map[string]any, id string, n int) {
		s := rebuilt[id]
		wires, ins := make([]any, n), make([]any, n)
		for i := 0; i < n; i++ {
			w, p := []any{}, []any{}
			if i < len(s) {
				if s[i].targets != nil {
					w, p = s[i].targets, s[i].ports
				}
			}
			wires[i], ins[i] = w, p
		}
		e["wires"], e[InKey] = wires, ins
	}
	for _, id := range order {
		e := byID[id]
		apply(e, id, len(outPorts(e)))
		if sel[id] {
			e["z"] = bodyID
		}
	}
	for _, b := range bodyIns {
		apply(b, b["id"].(string), 2)
	}

	sub := map[string]any{
		"id": subID, "type": "dia-sub", "z": tab, "x": cx, "y": cy, "name": name,
		"body": bodyID, "ins": strings.Join(inNames, ","), "outs": strings.Join(outNames, ","),
	}
	apply(sub, subID, len(outNames)+2)
	bodyTab := map[string]any{"id": bodyID, "type": "tab", "label": name, "ew_diagram": map[string]any{"role": "body"}}

	entries = append(entries, bodyTab, sub)
	for _, b := range bodyIns {
		entries = append(entries, b)
	}
	for _, b := range bodyOuts {
		entries = append(entries, b)
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	return out, nil
}
