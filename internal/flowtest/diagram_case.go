package flowtest

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram"
	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// DiagramTest is a test of a FlowLab diagram: set its controls, run it once, and
// say what its indicators hold. It runs the real diagram engine in this process, the
// way a message test runs the real runtime, with nothing reaching outside it:
//
//   - name: weighs and flags a heavy load
//     diagram:
//     name: Scale logger        # the diagram's name or id
//     controls: {Limit: 10, Enabled: true}
//     expect:
//     Weight: 12.5            # a number matches to within a millionth
//     Heavy: true
//     Reading: {min: 12, max: 13}
//
// Controls and indicators are named by their label or their id, the way a panel
// names them. A diagram that has a node that would reach outside the test (a serial
// port, an instrument, a data acquisition device, MQTT, HotLoop, a file) is refused,
// and a diagram test has no scripted serial device yet.
type DiagramTest struct {
	Name     string         `yaml:"name" json:"name"`
	Controls map[string]any `yaml:"controls,omitempty" json:"controls,omitempty"`
	Expect   map[string]any `yaml:"expect,omitempty" json:"expect,omitempty"`
}

// diagramReaches are the diagram node types that talk to something outside the process.
var diagramReaches = map[string]bool{
	"dia-serial-open": true, "dia-serial-read": true, "dia-serial-write": true,
	"dia-instr-open": true, "dia-instr-query": true, "dia-daq-read": true,
	"dia-mqtt-out": true, "dia-hotloop-write": true, "dia-log-text": true, "dia-log-tdms": true,
}

func runDiagramCase(ctx context.Context, doc []byte, c *Case) (res Result) {
	res = Result{Name: c.Name, Status: Error}
	began := time.Now()
	defer func() { res.Seconds = time.Since(began).Seconds() }()
	problem := func(msg string, args ...any) Result {
		res.Problems = append(res.Problems, Problem{Message: fmt.Sprintf(msg, args...)})
		return res
	}
	dt := c.Diagram
	flows, err := engine.ParseFlows(doc)
	if err != nil {
		return problem("the flow file doesn't parse: %v", err)
	}
	set := diagram.CompileAll(flows)
	var id string
	for _, did := range set.Order {
		if d := set.Diagrams[did]; did == dt.Name || d.Label == dt.Name {
			id = did
		}
	}
	if id == "" {
		return problem("no diagram is named %q", dt.Name)
	}
	if d := set.Diagrams[id]; len(d.Problems) > 0 {
		return problem("the diagram cannot run: %s", d.Problems[0].Text)
	}
	// Everything the run can reach: the diagram and the bodies it runs.
	var reach []string
	seen := map[string]bool{}
	var walk func(string)
	walk = func(did string) {
		d := set.Diagrams[did]
		if d == nil || seen[did] {
			return
		}
		seen[did] = true
		for _, nid := range d.Order {
			if diagramReaches[d.Nodes[nid].Type] {
				reach = append(reach, d.Label+": "+nid+" ("+d.Nodes[nid].Type+")")
			}
		}
		for _, b := range d.Bodies {
			walk(b)
		}
	}
	walk(id)
	if len(reach) > 0 {
		sort.Strings(reach)
		return problem("this diagram has nodes that would talk to the world outside the test, and a diagram test has no stand-in for them: %s", strings.Join(reach, ", "))
	}

	m := diagram.NewManager(nil)
	defer m.Close()
	m.Apply(flows)
	p, err := m.Panel(id)
	if err != nil {
		return problem("%v", err)
	}
	terms := m.Terminals(id)
	for name, v := range dt.Controls {
		t, err := diagram.ResolveTerminal(terms, name)
		if err != nil || t.Type != "dia-control" {
			return problem("%q is not a control of this diagram", name)
		}
		if _, err := p.SetControl(t.ID, v); err != nil {
			return problem("control %q: %v", name, err)
		}
	}
	timeout, _ := c.timeout()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := m.Start(id, diagram.StartOptions{})
	if err != nil {
		return problem("%v", err)
	}
	select {
	case <-r.Done():
	case <-rctx.Done():
		r.Stop()
		return problem("the diagram was still running after %s", timeout)
	}
	if st, rerr := r.State(); st != diagram.StateDone {
		return problem("the run ended %s: %v", st, rerr)
	}
	snap := p.Snapshot()
	res.Status = Pass
	names := make([]string, 0, len(dt.Expect))
	for n := range dt.Expect {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t, err := diagram.ResolveTerminal(terms, name)
		if err != nil || t.Type != "dia-indicator" {
			res.Status = Error
			res.Problems = append(res.Problems, Problem{Message: fmt.Sprintf("%q is not an indicator of this diagram", name)})
			continue
		}
		got, ok := snap.Indicators[t.ID]
		if !ok {
			res.Status = Fail
			res.Problems = append(res.Problems, Problem{Message: fmt.Sprintf("indicator %q never received a value", name)})
			continue
		}
		if why := matchIndicator(dt.Expect[name], got); why != "" {
			res.Status = Fail
			res.Problems = append(res.Problems, Problem{Message: fmt.Sprintf("indicator %q: %s", name, why)})
		}
	}
	return res
}

// matchIndicator compares what a test expects with what an indicator holds. A number
// matches within a millionth, {min, max} is a range, and anything else is equal.
func matchIndicator(want, got any) string {
	if m, ok := want.(map[string]any); ok {
		lo, hasLo := asFloat(m["min"])
		hi, hasHi := asFloat(m["max"])
		if hasLo || hasHi {
			g, ok := asFloat(got)
			if !ok {
				return fmt.Sprintf("expected a number between %v and %v, got %v", m["min"], m["max"], got)
			}
			if (hasLo && g < lo) || (hasHi && g > hi) {
				return fmt.Sprintf("expected between %v and %v, got %v", m["min"], m["max"], g)
			}
			return ""
		}
	}
	if w, ok := asFloat(want); ok {
		if g, ok := asFloat(got); ok {
			if math.Abs(w-g) <= 1e-6*math.Max(1, math.Abs(w)) {
				return ""
			}
			return fmt.Sprintf("expected %v, got %v", w, g)
		}
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Sprintf("expected %v, got %v", want, got)
	}
	return ""
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}
