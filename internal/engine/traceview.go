package engine

import "time"

// HopView is a hop the way the API shows it and the replay reads it back: the
// message is plain JSON, and Canvas names the node on the canvas to light for
// a hop that happened inside a subflow.
type HopView struct {
	Hop
	Msg    any    `json:"msg,omitempty"`
	Canvas string `json:"canvas,omitempty"`
}

// TraceView is a trace as JSON.
type TraceView struct {
	ID        string    `json:"id"`
	Started   time.Time `json:"started"`
	Truncated bool      `json:"truncated,omitempty"`
	Hops      []HopView `json:"hops"`
}

// View turns a snapshot into something that encodes. Nothing in it can fail to
// marshal, whatever the messages held.
func (s TraceSnapshot) View() TraceView {
	v := TraceView{ID: s.ID, Started: s.Started, Truncated: s.Truncated, Hops: make([]HopView, len(s.Hops))}
	for i, h := range s.Hops {
		hv := HopView{Hop: h}
		if h.Msg != nil {
			hv.Msg = JSONSafe(h.Msg)
		}
		v.Hops[i] = hv
	}
	return v
}

// Snapshot is View's way back, for a trace that crossed a process boundary as
// JSON.
func (v TraceView) Snapshot() TraceSnapshot {
	s := TraceSnapshot{ID: v.ID, Started: v.Started, Truncated: v.Truncated, Hops: make([]Hop, len(v.Hops))}
	for i, hv := range v.Hops {
		h := hv.Hop
		if m, ok := hv.Msg.(map[string]any); ok {
			h.Msg = m
		}
		s.Hops[i] = h
	}
	return s
}
