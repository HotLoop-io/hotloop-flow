package diagram

import (
	"sync"
	"time"
)

// traceMax is the most events a run keeps for replay. A run that has more says so
// and keeps the first of them, because a replay that starts in the middle has no
// state to start from.
const traceMax = 20000

// TraceEvent is one thing that happened in a run: a node starting, finishing or
// failing, or a value arriving on a wire. T is the milliseconds since the run
// began, and Data is what was published live, so a replay feeds the same handler
// the live run feeds.
type TraceEvent struct {
	T    float64        `json:"t"`
	Kind string         `json:"kind"`
	Data map[string]any `json:"data"`
}

// Trace is the recording of a run.
type Trace struct {
	Run       string       `json:"run"`
	Diagram   string       `json:"diagram"`
	State     RunState     `json:"state"`
	Started   time.Time    `json:"started"`
	Events    []TraceEvent `json:"events"`
	Truncated bool         `json:"truncated"`
	Dropped   int64        `json:"dropped"`
}

// traceRecorder keeps the events of one run.
type traceRecorder struct {
	mu      sync.Mutex
	started time.Time
	events  []TraceEvent
	dropped int64
}

func (rc *traceRecorder) add(kind string, data map[string]any) {
	cp := make(map[string]any, len(data))
	for k, v := range data {
		cp[k] = v
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.events) >= traceMax {
		rc.dropped++
		return
	}
	rc.events = append(rc.events, TraceEvent{T: float64(time.Since(rc.started).Microseconds()) / 1000, Kind: kind, Data: cp})
}

// addToken keeps a value that crossed a wire nobody was watching live. The preview
// is made only when there is room for it.
func (rc *traceRecorder) addToken(wire string, v any, diagram string) {
	rc.mu.Lock()
	full := len(rc.events) >= traceMax
	if full {
		rc.dropped++
	}
	rc.mu.Unlock()
	if full {
		return
	}
	rc.add("token", map[string]any{"wire": wire, "value": preview(v), "diagram": diagram})
}

// Trace returns what the run did, in order. It can be asked for while the run
// goes on and after it ends, and it stays until the diagram is run again.
func (r *Run) Trace() Trace {
	r.rec.mu.Lock()
	ev := append([]TraceEvent(nil), r.rec.events...)
	dropped := r.rec.dropped
	r.rec.mu.Unlock()
	st, _ := r.State()
	return Trace{Run: r.ID, Diagram: r.Diagram, State: st, Started: r.Started, Events: ev, Truncated: dropped > 0, Dropped: dropped}
}

// Trace returns the recording of the last run of a diagram, or ErrNoRun when it
// has not been run since the flow was deployed.
func (m *Manager) Trace(id string) (Trace, error) {
	if _, ok := m.Diagram(id); !ok {
		return Trace{}, ErrNoDiagram
	}
	r := m.Current(id)
	if r == nil {
		return Trace{}, ErrNoRun
	}
	return r.Trace(), nil
}
