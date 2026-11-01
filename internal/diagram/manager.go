package diagram

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// ErrNoDiagram is returned for a diagram id that is not deployed.
var ErrNoDiagram = errors.New("no such diagram")

// ErrBusy is returned when a diagram already has a run going.
var ErrBusy = errors.New("this diagram is already running")

// Publisher carries an event to everyone watching, and must not block.
type Publisher func(topic string, data map[string]any)

// Manager owns the diagrams of the deployed flow file and the runs of them.
type Manager struct {
	mu      sync.Mutex
	set     *Set
	runs    map[string]*Run // the current run of each diagram, running or last finished
	panels  map[string]*Panel
	pub     Publisher
	probes  map[string]map[string]int // diagram -> wire key -> history depth
	breaks  map[string][]string       // diagram -> breakpoint keys, for the next run and this one
	bgCtx   context.Context
	bgStop  context.CancelFunc
	started bool
	nextRun int64
	flushWG sync.WaitGroup
}

// NewManager makes a manager. The publisher may be nil, and set later.
func NewManager(pub Publisher) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		set: &Set{Diagrams: map[string]*Diagram{}}, runs: map[string]*Run{}, panels: map[string]*Panel{},
		pub: pub, probes: map[string]map[string]int{}, breaks: map[string][]string{}, bgCtx: ctx, bgStop: cancel,
	}
	return m
}

// SetPublisher sets where events go.
func (m *Manager) SetPublisher(p Publisher) {
	m.mu.Lock()
	m.pub = p
	m.mu.Unlock()
}

func (m *Manager) publish(topic string, data map[string]any) {
	m.mu.Lock()
	p := m.pub
	m.mu.Unlock()
	if p != nil {
		p(topic, data)
	}
}

// Apply swaps in the diagrams of a deployed flow file. Runs of the old ones stop,
// and a diagram that says autostart starts again, so a deploy of a headless
// logger is a restart of the logger and nothing else.
func (m *Manager) Apply(flows *engine.Flows) {
	if m == nil {
		return
	}
	set := CompileAll(flows)

	m.mu.Lock()
	old := m.runs
	m.runs = map[string]*Run{}
	m.set = set
	for id := range m.panels {
		if _, ok := set.Diagrams[id]; !ok {
			delete(m.panels, id)
		}
	}
	m.mu.Unlock()

	for _, r := range old {
		r.Stop()
	}
	for _, r := range old {
		<-r.Done()
	}

	for _, id := range set.Order {
		d := set.Diagrams[id]
		if auto, _ := d.Settings["autostart"].(bool); auto && len(d.Problems) == 0 && !isBody(d) {
			if _, err := m.Start(id, StartOptions{}); err != nil {
				m.publish("diagram/"+id+"/run", map[string]any{"diagram": id, "state": string(StateFailed), "error": err.Error()})
			}
		}
	}
}

func isBody(d *Diagram) bool {
	role, _ := d.Settings["role"].(string)
	return role == "body"
}

// Close stops every run and the background work.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	runs := m.runs
	m.runs = map[string]*Run{}
	m.mu.Unlock()
	for _, r := range runs {
		r.Stop()
	}
	for _, r := range runs {
		<-r.Done()
	}
	m.bgStop()
	m.flushWG.Wait()
}

// StartOptions are the settings of one run.
type StartOptions struct {
	// Highlight pauses before each node for this long.
	Highlight time.Duration
	// Pause starts the run stopped before its first node.
	Pause bool
	// Breakpoints, if given, replace the diagram's breakpoints for this run.
	Breakpoints []string
}

// Start begins a run of a diagram.
func (m *Manager) Start(id string, opt StartOptions) (*Run, error) {
	m.mu.Lock()
	d, ok := m.set.Diagrams[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNoDiagram
	}
	if cur := m.runs[id]; cur != nil {
		if st, _ := cur.State(); st == StateRunning || st == StatePaused {
			m.mu.Unlock()
			return nil, ErrBusy
		}
	}
	if len(d.Problems) > 0 {
		m.mu.Unlock()
		return nil, fmt.Errorf("the diagram cannot run: %s", d.Problems[0])
	}
	set := m.set
	m.nextRun++
	ctx, cancel := context.WithCancel(m.bgCtx)
	r := &Run{
		ID: fmt.Sprintf("%s-%d", id, m.nextRun), Diagram: id, Started: time.Now(),
		set: set, mgr: m, cancel: cancel, done: make(chan struct{}), state: StateRunning,
	}
	r.ctl = newController(func(kind string, data map[string]any) { r.emit(kind, data) })
	if opt.Breakpoints != nil {
		m.breaks[id] = append([]string(nil), opt.Breakpoints...)
	}
	r.ctl.SetBreakpoints(m.breaks[id])
	if opt.Pause {
		r.ctl.Pause()
	}
	r.SetHighlight(opt.Highlight)
	m.runs[id] = r
	p := m.panelLocked(id)
	m.mu.Unlock()

	p.begin(set.Terminals(id))
	r.emit("run", map[string]any{"run": r.ID, "state": string(StateRunning)})
	go func() {
		_, err := r.exec(ctx, d, 0, nil)
		cancel()
		r.finish(err)
	}()
	return r, nil
}

// Stop ends the current run of a diagram.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	r := m.runs[id]
	_, ok := m.set.Diagrams[id]
	m.mu.Unlock()
	if !ok {
		return ErrNoDiagram
	}
	if r != nil {
		r.Stop()
	}
	return nil
}

// Current returns the run a diagram has now, or the last one it had.
func (m *Manager) Current(id string) *Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id]
}

// Diagram returns a compiled diagram.
func (m *Manager) Diagram(id string) (*Diagram, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.set.Diagrams[id]
	return d, ok
}

// IDs lists the diagrams in file order.
func (m *Manager) IDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.set.Order...)
}

// Terminals returns the panel's controls and indicators of a diagram.
func (m *Manager) Terminals(id string) []*DNode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set.Terminals(id)
}

func (m *Manager) panelFor(id string) *Panel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.panelLocked(id)
}

func (m *Manager) panelLocked(id string) *Panel {
	p := m.panels[id]
	if p == nil {
		p = newPanel(id, func(data map[string]any) { m.publishLater("diagram/"+id+"/values", data) })
		m.panels[id] = p
		m.flushWG.Add(1)
		go func() {
			defer m.flushWG.Done()
			p.flushLoop(m.bgCtx)
		}()
	}
	return p
}

func (m *Manager) publishLater(topic string, data map[string]any) {
	data["diagram"] = trimTopic(topic)
	m.publish(topic, data)
}

func trimTopic(t string) string {
	// diagram/<id>/values
	for i := len("diagram/"); i < len(t); i++ {
		if t[i] == '/' {
			return t[len("diagram/"):i]
		}
	}
	return t
}

// Panel returns the panel state of a diagram.
func (m *Manager) Panel(id string) (*Panel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.set.Diagrams[id]; !ok {
		return nil, ErrNoDiagram
	}
	return m.panelLocked(id), nil
}

// SetProbes sets which wires are watched, and how many values each remembers. A
// probe never touches a run: it keeps more of what already went by.
func (m *Manager) SetProbes(id string, wires []string, depth int) {
	if depth <= 0 {
		depth = 100
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pr := map[string]int{}
	for _, w := range wires {
		pr[w] = depth
	}
	m.probes[id] = pr
}

// Probes lists the watched wires of a diagram.
func (m *Manager) Probes(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for w := range m.probes[id] {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) probeDepth(id, wire string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.probes[id][wire]
}

func (m *Manager) wantsToken(id, wire string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.probes[id][wire]
	return ok
}

// SetBreakpoints sets the breakpoints of a diagram, for the run it has now and
// every run after it, so they can be set before pressing run.
func (m *Manager) SetBreakpoints(id string, keys []string) {
	m.mu.Lock()
	m.breaks[id] = append([]string(nil), keys...)
	r := m.runs[id]
	m.mu.Unlock()
	if r != nil {
		r.ctl.SetBreakpoints(keys)
	}
}

// Breakpoints lists the breakpoints a diagram has.
func (m *Manager) Breakpoints(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.breaks[id]...)
}
