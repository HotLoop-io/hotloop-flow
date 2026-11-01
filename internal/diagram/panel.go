package diagram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Panel is the operator side of a diagram: the values of its controls, the last
// value each indicator was given, and what its charts have collected. It outlives
// a run, so a person sets a control before pressing run and reads the result
// after, and a headless run can have a panel opened on it later.
type Panel struct {
	id string

	mu       sync.Mutex
	controls map[string]any
	inds     map[string]any
	charts   map[string]*Chart
	terms    map[string]*DNode

	// What changed since the last flush, which is what goes out as one event.
	dirtyInds   map[string]any
	dirtyCharts map[string]chartDelta
	publish     func(map[string]any)
}

// Chart is what a chart or graph on the panel shows.
type Chart struct {
	Mode string    `json:"mode"` // chart, graph or xy
	Cap  int       `json:"history"`
	T    []int64   `json:"t,omitempty"` // unix milliseconds, for a chart
	X    []float64 `json:"x,omitempty"`
	Y    []float64 `json:"y"`
	// Dropped counts samples a full chart let go of, so nothing is thrown away
	// without being counted.
	Dropped int64 `json:"dropped"`
}

type chartDelta struct {
	Replace bool      `json:"replace,omitempty"`
	T       []int64   `json:"t,omitempty"`
	X       []float64 `json:"x,omitempty"`
	Y       []float64 `json:"y"`
}

func newPanel(id string, publish func(map[string]any)) *Panel {
	return &Panel{
		id: id, controls: map[string]any{}, inds: map[string]any{}, charts: map[string]*Chart{},
		terms: map[string]*DNode{}, dirtyInds: map[string]any{}, dirtyCharts: map[string]chartDelta{},
		publish: publish,
	}
}

// begin gets the panel ready for a run: it learns the terminals, clears the
// indicators and charts, and keeps the controls where the operator left them.
func (p *Panel) begin(terms []*DNode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terms = map[string]*DNode{}
	for _, t := range terms {
		p.terms[t.ID] = t
	}
	p.inds = map[string]any{}
	p.charts = map[string]*Chart{}
	p.dirtyInds = map[string]any{}
	p.dirtyCharts = map[string]chartDelta{}
	for id, t := range p.terms {
		switch t.Type {
		case "dia-chart":
			p.charts[id] = &Chart{Mode: strProp(t.Raw, "mode", "chart"), Cap: intProp(t.Raw, "history", 1024)}
			p.dirtyCharts[id] = chartDelta{Replace: true, Y: []float64{}}
		case "dia-indicator":
			p.dirtyInds[id] = nil
		}
	}
}

// Control reads a control's value, falling back to its default.
func (p *Panel) Control(id string, def any) any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.controls[id]; ok {
		return v
	}
	return def
}

// TakeControl reads a control and, for a latched button, puts it back to false in
// the same step, so one press is one run of whatever waits on it.
func (p *Panel) TakeControl(id string, def any, latch bool) any {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.controls[id]
	if !ok {
		v = def
	}
	if latch {
		if b, _ := v.(bool); b {
			p.controls[id] = false
			p.dirtyInds["control:"+id] = false
		}
	}
	return v
}

// SetControl sets a control from the browser or the API, checking it against the
// control's kind, range and options, and tells everyone watching.
func (p *Panel) SetControl(id string, v any) (any, error) {
	p.mu.Lock()
	t, ok := p.terms[id]
	p.mu.Unlock()
	if !ok || t.Type != "dia-control" {
		return nil, fmt.Errorf("%q is not a control on this panel", id)
	}
	v, err := coerceControl(t.Raw, v)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.controls[id] = v
	p.dirtyInds["control:"+id] = v
	p.mu.Unlock()
	return v, nil
}

// SetIndicator gives an indicator a value.
func (p *Panel) SetIndicator(id string, v any) {
	j := jsonValue(v)
	p.mu.Lock()
	p.inds[id] = j
	p.dirtyInds[id] = j
	p.mu.Unlock()
}

// PutChart adds to a chart, or replaces its data, as its mode says.
func (p *Panel) PutChart(id string, x, y []float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.charts[id]
	if c == nil {
		c = &Chart{Mode: "chart", Cap: 1024}
		p.charts[id] = c
	}
	now := time.Now().UnixMilli()
	switch c.Mode {
	case "graph":
		c.X, c.Y = append([]float64(nil), x...), append([]float64(nil), y...)
		p.dirtyCharts[id] = chartDelta{Replace: true, X: c.X, Y: c.Y}
		return
	case "xy":
		c.X = append(c.X, x...)
		c.Y = append(c.Y, y...)
		d := p.dirtyCharts[id]
		d.X, d.Y = append(d.X, x...), append(d.Y, y...)
		p.dirtyCharts[id] = d
	default:
		for range y {
			c.T = append(c.T, now)
		}
		c.Y = append(c.Y, y...)
		d := p.dirtyCharts[id]
		for range y {
			d.T = append(d.T, now)
		}
		d.Y = append(d.Y, y...)
		p.dirtyCharts[id] = d
	}
	if over := len(c.Y) - c.Cap; over > 0 {
		c.Y = append([]float64(nil), c.Y[over:]...)
		if len(c.T) >= over {
			c.T = append([]int64(nil), c.T[over:]...)
		}
		if len(c.X) >= over {
			c.X = append([]float64(nil), c.X[over:]...)
		}
		c.Dropped += int64(over)
	}
}

// PanelSnapshot is the whole panel, for a browser that has just opened it.
type PanelSnapshot struct {
	Controls   map[string]any    `json:"controls"`
	Indicators map[string]any    `json:"indicators"`
	Charts     map[string]*Chart `json:"charts"`
}

// Snapshot copies the panel's current state.
func (p *Panel) Snapshot() PanelSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := PanelSnapshot{Controls: map[string]any{}, Indicators: map[string]any{}, Charts: map[string]*Chart{}}
	for k, v := range p.controls {
		s.Controls[k] = v
	}
	for k, v := range p.inds {
		s.Indicators[k] = v
	}
	for k, c := range p.charts {
		cc := *c
		cc.T = append([]int64(nil), c.T...)
		cc.X = append([]float64(nil), c.X...)
		cc.Y = append([]float64(nil), c.Y...)
		s.Charts[k] = &cc
	}
	return s
}

// flushLoop sends what changed, ten times a second at most, so a loop running
// flat out is a stream a phone can keep up with. A value is the last one set.
func (p *Panel) flushLoop(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.flush()
		}
	}
}

func (p *Panel) flush() {
	p.mu.Lock()
	if len(p.dirtyInds) == 0 && len(p.dirtyCharts) == 0 {
		p.mu.Unlock()
		return
	}
	data := map[string]any{"indicators": p.dirtyInds, "charts": p.dirtyCharts}
	p.dirtyInds = map[string]any{}
	p.dirtyCharts = map[string]chartDelta{}
	p.mu.Unlock()
	if p.publish != nil {
		p.publish(data)
	}
}

func coerceControl(raw map[string]any, v any) (any, error) {
	kind := strProp(raw, "kind", "number")
	switch kind {
	case "bool":
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			b, err := strconv.ParseBool(x)
			if err != nil {
				return nil, fmt.Errorf("%q is not true or false", x)
			}
			return b, nil
		case float64:
			return x != 0, nil
		}
		return nil, fmt.Errorf("a switch takes true or false")
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("a text control takes text")
		}
		return s, nil
	case "enum":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("a dropdown takes one of its choices")
		}
		for _, o := range splitList(strProp(raw, "options", "")) {
			if o == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%q is not one of the choices", s)
	}
	f, ok := num(v)
	if s, isStr := v.(string); isStr && !ok {
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(s), 64); err != nil {
			return nil, fmt.Errorf("%q is not a number", s)
		}
		ok = true
	}
	if !ok {
		return nil, fmt.Errorf("a numeric control takes a number")
	}
	if lo, has := limit(raw["min"]); has && f < lo {
		return nil, fmt.Errorf("%v is below the minimum %v", f, lo)
	}
	if hi, has := limit(raw["max"]); has && f > hi {
		return nil, fmt.Errorf("%v is above the maximum %v", f, hi)
	}
	return f, nil
}

func strProp(raw map[string]any, key, def string) string {
	if s, ok := raw[key].(string); ok && s != "" {
		return s
	}
	return def
}

func intProp(raw map[string]any, key string, def int) int {
	switch x := raw[key].(type) {
	case float64:
		if x > 0 {
			return int(x)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func floatProp(raw map[string]any, key string, def float64) float64 {
	switch x := raw[key].(type) {
	case float64:
		return x
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f
		}
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// limit reads a range end, which the dialog stores as text and may leave empty.
func limit(v any) (float64, bool) {
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f, err == nil
	}
	return num(v)
}
