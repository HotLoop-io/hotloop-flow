package diagram

import (
	"sort"
	"time"
)

// Summary is a diagram in the list.
type Summary struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Role  string `json:"role"`
	// Owner is the diagram to run to run this one. It is the diagram itself
	// unless this is the body of a loop in another.
	Owner     string    `json:"owner"`
	Autostart bool      `json:"autostart"`
	State     RunState  `json:"state"`
	Problems  []Problem `json:"problems"`
}

// Terminal is a control, indicator or chart as the panel draws it.
type Terminal struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Diagram string         `json:"diagram"`
	Label   string         `json:"label"`
	Kind    string         `json:"kind,omitempty"`
	Style   string         `json:"style,omitempty"`
	Unit    string         `json:"unit,omitempty"`
	Digits  int            `json:"digits,omitempty"`
	Min     *float64       `json:"min,omitempty"`
	Max     *float64       `json:"max,omitempty"`
	Step    *float64       `json:"step,omitempty"`
	Options []string       `json:"options,omitempty"`
	Default any            `json:"default,omitempty"`
	Mode    string         `json:"mode,omitempty"`
	History int            `json:"history,omitempty"`
	Layout  map[string]any `json:"layout,omitempty"`
	Order   int            `json:"order"`
}

// PortSet is the input and output port names of a node, the same ones the editor
// draws.
type PortSet struct {
	In  []string `json:"in"`
	Out []string `json:"out"`
}

// RunInfo is the state of the current run.
type RunInfo struct {
	ID       string    `json:"id"`
	State    RunState  `json:"state"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Waiting  []string  `json:"waiting,omitempty"`
	NodeRuns int64     `json:"nodeRuns"`
	Errors   int64     `json:"errors"`
}

// Description is everything a panel or an editor needs about one diagram.
type Description struct {
	Summary
	Terminals   []Terminal              `json:"terminals"`
	Panel       PanelSnapshot           `json:"panel"`
	Run         *RunInfo                `json:"run,omitempty"`
	Breakpoints []string                `json:"breakpoints"`
	Probes      []string                `json:"probes"`
	Wires       map[string]WireSnapshot `json:"wires"`
	Ports       map[string]PortSet      `json:"ports"`
}

// List summarizes every diagram in file order.
func (m *Manager) List() []Summary {
	ids := m.IDs()
	out := make([]Summary, 0, len(ids))
	for _, id := range ids {
		if s, ok := m.summary(id); ok {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) summary(id string) (Summary, bool) {
	d, ok := m.Diagram(id)
	if !ok {
		return Summary{}, false
	}
	s := Summary{ID: id, Owner: m.ownerOf(id), Label: d.Label, Role: strProp(d.Settings, "role", "diagram"), State: "idle", Problems: d.Problems}
	s.Autostart, _ = d.Settings["autostart"].(bool)
	if s.Problems == nil {
		s.Problems = []Problem{}
	}
	if r := m.Current(id); r != nil {
		s.State, _ = r.State()
	}
	return s, true
}

// Describe returns the whole picture of a diagram.
func (m *Manager) Describe(id string) (Description, error) {
	sum, ok := m.summary(id)
	if !ok {
		return Description{}, ErrNoDiagram
	}
	p, err := m.Panel(id)
	if err != nil {
		return Description{}, err
	}
	desc := Description{
		Summary: sum, Terminals: []Terminal{}, Panel: p.Snapshot(), Breakpoints: []string{}, Probes: m.Probes(id),
		Wires: map[string]WireSnapshot{}, Ports: map[string]PortSet{},
	}
	if desc.Probes == nil {
		desc.Probes = []string{}
	}
	for i, t := range m.Terminals(id) {
		desc.Terminals = append(desc.Terminals, terminalOf(t, i))
	}
	sort.SliceStable(desc.Terminals, func(i, j int) bool {
		a, b := desc.Terminals[i].Layout, desc.Terminals[j].Layout
		return layoutKey(a) < layoutKey(b)
	})
	if bp := m.Breakpoints(id); bp != nil {
		desc.Breakpoints = bp
	}
	if r := m.Current(id); r != nil {
		st, err := r.State()
		ri := &RunInfo{ID: r.ID, State: st, Started: r.Started, Waiting: r.ctl.Waiting()}
		if err != nil {
			ri.Error = err.Error()
		}
		ri.NodeRuns, ri.Errors = r.Stats()
		desc.Run = ri
		desc.Wires = r.WireValues()
	}
	// The diagram's own tab and every body it runs.
	m.mu.Lock()
	set := m.set
	m.mu.Unlock()
	seen := map[string]bool{}
	var walk func(string)
	walk = func(tab string) {
		if seen[tab] {
			return
		}
		seen[tab] = true
		d, ok := set.Diagrams[tab]
		if !ok {
			return
		}
		for _, nid := range d.Order {
			n := d.Nodes[nid]
			ps := PortSet{}
			for _, x := range n.In {
				ps.In = append(ps.In, x.Name)
			}
			for _, x := range n.Out {
				ps.Out = append(ps.Out, x.Name)
			}
			desc.Ports[nid] = ps
		}
		for _, b := range d.Bodies {
			walk(b)
		}
	}
	walk(id)
	return desc, nil
}

func layoutKey(l map[string]any) float64 {
	if l == nil {
		return 1e9
	}
	r, _ := num(l["row"])
	c, _ := num(l["col"])
	return r*1000 + c
}

func terminalOf(t *DNode, order int) Terminal {
	r := t.Raw
	out := Terminal{
		ID: t.ID, Type: t.Type, Diagram: t.Tab, Label: strProp(r, "label", t.Name), Kind: strProp(r, "kind", ""), Style: strProp(r, "style", ""),
		Unit: strProp(r, "unit", ""), Digits: intOr(r["digits"], 2), Options: splitList(strProp(r, "options", "")),
		Mode: strProp(r, "mode", ""), Order: order,
	}
	if out.Label == "" {
		out.Label = t.ID
	}
	if v, ok := limit(r["min"]); ok {
		out.Min = &v
	}
	if v, ok := limit(r["max"]); ok {
		out.Max = &v
	}
	if v, ok := limit(r["step"]); ok {
		out.Step = &v
	}
	if t.Type == "dia-chart" {
		out.History = intProp(r, "history", 1024)
	}
	if t.Type == "dia-control" {
		out.Default, _ = controlDefault(r)
	}
	out.Layout, _ = r[PanelKey].(map[string]any)
	return out
}

func (m *Manager) ownerOf(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set.Owner(id)
}
