package diagram

import (
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/fence"
	"github.com/HotLoop-io/hotloop-flow/internal/instruments"
)

// CodeInstrument is the error code of a write to an instrument whose writes are
// switched off.
const CodeInstrument = 5007

// CodeFenced is the error code of a write this instance may not make because it is
// the standby, or its lease is running out. See internal/fence.
const CodeFenced = 5008

// InstrumentGate is what a diagram asks before it writes to an instrument.
type InstrumentGate interface {
	Allowed(id string) bool
}

// AllowAll is the gate that lets every write through, for a test that is about
// something else. A real runtime is given the switch, which starts with every
// instrument off.
type AllowAll struct{}

// Allowed is always true.
func (AllowAll) Allowed(string) bool { return true }

// Audit events a diagram's instrument traffic leaves.
const (
	AuditInstrumentWrite   = "instrument.write"
	AuditInstrumentRefused = "instrument.refused"
)

// identified is a session that knows which instrument it talks to.
type identified interface{ id() string }

func (s *serialSession) id() string { return instruments.Normalize("serial://" + s.cfg.Device) }
func (s *tcpSession) id() string    { return instruments.Normalize("tcp://" + s.addr) }

// isWriteCommand says whether an instrument command can change anything. A query
// (SCPI statements that end in ?) only asks; anything else, a setting, an output
// on, a reset, writes. Several statements in one line are a write unless every
// one of them is a query.
func isWriteCommand(cmd string) bool {
	any := false
	for _, st := range strings.Split(cmd, ";") {
		st = strings.TrimSpace(st)
		if st == "" {
			continue
		}
		any = true
		if !strings.HasSuffix(st, "?") {
			return true
		}
	}
	return !any
}

// guardWrite is the one place a diagram's write to an instrument is decided. It
// lets the write through only when that instrument's writes are on, and it leaves
// an audit row either way, with the diagram and the node.
func (m *Manager) guardWrite(rc *Ctx, instrument, what string) error {
	if err := fence.Check("instrument write"); err != nil {
		return &Fault{Code: CodeFenced, Source: "not written to " + instrument + ": " + err.Error()}
	}
	who := "flow:" + rc.Diagram.ID + "/" + rc.Node.ID
	m.mu.Lock()
	gate, audit := m.services.Instruments, m.services.Audit
	m.mu.Unlock()
	detail := map[string]any{"instrument": instrument, "diagram": rc.Diagram.ID, "node": rc.Node.ID, "command": what}
	if gate != nil && gate.Allowed(instrument) {
		if audit != nil {
			audit(AuditInstrumentWrite, who, detail)
		}
		return nil
	}
	if audit != nil {
		audit(AuditInstrumentRefused, who, detail)
	}
	return &Fault{Code: CodeInstrument, Source: "writes to " + instrument + " are switched off. A diagram can read an instrument freely, but writing to one needs that instrument's writes turned on by someone with the instrument.write permission (PUT /instruments/writes), and they are off until then"}
}

// writeTarget is one instrument a diagram would write to, and the nodes that do.
type writeTarget struct {
	Instrument string   `json:"instrument"`
	Nodes      []string `json:"nodes"`
}

// instrumentOf follows a node's session wire back to the node that opened the
// instrument, through any node that hands the session on, and says which
// instrument that is.
func instrumentOf(d *Diagram, nodeID string) (string, bool) {
	for depth := 0; depth < 32; depth++ {
		w, ok := d.Into[nodeID]["session"]
		if !ok {
			return "", false
		}
		src := d.Nodes[w.From]
		if src == nil {
			return "", false
		}
		switch src.Type {
		case "dia-serial-open":
			return instruments.Normalize("serial://" + strProp(src.Raw, "device", "")), true
		case "dia-instr-open":
			l, err := parseAddress(strProp(src.Raw, "address", ""))
			if err != nil {
				return "", false
			}
			return l.(identified).id(), true
		}
		nodeID = src.ID // a write or a query hands the session on
	}
	return "", false
}

// warnWrites is what deploy says about a node that would write to an instrument:
// which one, and that its writes have to be on.
func warnWrites(isWrite func(n *DNode) bool) func(*Set, *Diagram, *DNode) []Problem {
	return func(_ *Set, d *Diagram, n *DNode) []Problem {
		if !isWrite(n) {
			return nil
		}
		id, ok := instrumentOf(d, n.ID)
		if !ok {
			return []Problem{{Node: n.ID, Text: "writes to an instrument, and which one is not known until it runs; its writes have to be switched on"}}
		}
		return []Problem{{Node: n.ID, Text: "writes to " + id + ", and that instrument's writes are off until someone with the instrument.write permission turns them on"}}
	}
}

// Writes lists the instruments a diagram would write to, with the nodes that do.
func (m *Manager) Writes(id string) []writeTarget {
	m.mu.Lock()
	set := m.set
	m.mu.Unlock()
	if set == nil {
		return nil
	}
	by := map[string]*writeTarget{}
	var order []string
	for _, did := range set.Order {
		if set.Owner(did) != set.Owner(id) && did != id {
			continue
		}
		d := set.Diagrams[did]
		for _, nid := range d.Order {
			n := d.Nodes[nid]
			if n.Kind.WritesInstrument == nil || !n.Kind.WritesInstrument(n) {
				continue
			}
			inst, ok := instrumentOf(d, nid)
			if !ok {
				inst = "(unknown until it runs)"
			}
			t := by[inst]
			if t == nil {
				t = &writeTarget{Instrument: inst}
				by[inst] = t
				order = append(order, inst)
			}
			t.Nodes = append(t.Nodes, nid)
		}
	}
	out := make([]writeTarget, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	return out
}
