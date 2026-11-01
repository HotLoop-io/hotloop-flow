package diagram

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// StepMode says how far a step goes.
type StepMode string

const (
	// StepInto pauses before the very next node, inside a structure or not.
	StepInto StepMode = "into"
	// StepOver pauses before the next node at this depth or shallower, so a
	// structure's whole body runs without stopping.
	StepOver StepMode = "over"
	// StepOut pauses before the next node shallower than this one.
	StepOut StepMode = "out"
)

// Controller is a run's debugger. A node asks it before it runs, and a wire asks
// before it hands a value on. Breakpoints, stepping and pause all go through the
// one place, so a run that is not being debugged pays for a lock and a map lookup.
type Controller struct {
	mu          sync.Mutex
	breakpoints map[string]bool
	stepping    bool
	step        StepMode
	depth       int // the depth a step is measured from
	waiters     []*waiter
	released    bool
	notify      func(kind string, data map[string]any)
	cancelled   bool
}

type waiter struct {
	key   string
	depth int
	diag  string
	go_   chan struct{}
}

func newController(notify func(string, map[string]any)) *Controller {
	return &Controller{breakpoints: map[string]bool{}, notify: notify}
}

// SetBreakpoints replaces the set. A key is a node id, "wire:" and a wire key, or
// "diagram:" and a tab id.
func (c *Controller) SetBreakpoints(keys []string) {
	c.mu.Lock()
	c.breakpoints = map[string]bool{}
	for _, k := range keys {
		c.breakpoints[k] = true
	}
	c.mu.Unlock()
}

// Breakpoints lists the keys in order.
func (c *Controller) Breakpoints() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.breakpoints))
	for k := range c.breakpoints {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Pause makes the run stop before the next node starts.
func (c *Controller) Pause() {
	c.mu.Lock()
	c.stepping, c.step, c.depth = true, StepInto, 0
	c.mu.Unlock()
}

// Paused reports whether the run has a node waiting to be let through.
func (c *Controller) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters) > 0
}

// Waiting names what the run is stopped before.
func (c *Controller) Waiting() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.waiters))
	for _, w := range c.waiters {
		out = append(out, w.key)
	}
	return out
}

// Resume lets everything go and runs at full speed until the next breakpoint.
func (c *Controller) Resume() {
	c.mu.Lock()
	c.stepping = false
	c.releaseAllLocked()
	c.mu.Unlock()
	c.notify("paused", map[string]any{"waiting": []string{}})
}

// Release lets every waiter go, for a stop.
func (c *Controller) Release() {
	c.mu.Lock()
	c.stepping = false
	c.cancelled = true
	c.releaseAllLocked()
	c.mu.Unlock()
}

func (c *Controller) releaseAllLocked() {
	for _, w := range c.waiters {
		close(w.go_)
	}
	c.waiters = nil
}

// Step lets the node that is waiting run, then stops before the next one the mode
// reaches. With nothing waiting it just arms the stop.
func (c *Controller) Step(mode StepMode) {
	c.mu.Lock()
	c.stepping, c.step = true, mode
	if len(c.waiters) > 0 {
		w := c.waiters[0]
		c.waiters = c.waiters[1:]
		c.depth = w.depth
		close(w.go_)
	}
	left := c.keysLocked()
	c.mu.Unlock()
	c.notify("paused", map[string]any{"waiting": left})
}

func (c *Controller) keysLocked() []string {
	out := make([]string, 0, len(c.waiters))
	for _, w := range c.waiters {
		out = append(out, w.key)
	}
	return out
}

func (c *Controller) shouldStopLocked(key string, depth int) bool {
	if c.breakpoints[key] {
		return true
	}
	// Stepping goes node by node. A wire or the start of a pass is only a place
	// to stop when somebody set a breakpoint there.
	if !c.stepping || strings.HasPrefix(key, "wire:") || strings.HasPrefix(key, "diagram:") {
		return false
	}
	switch c.step {
	case StepOver:
		return depth <= c.depth
	case StepOut:
		return depth < c.depth
	}
	return true
}

// before blocks while the run should be stopped before key. It returns an error
// only when the run is cancelled while waiting.
func (c *Controller) before(ctx context.Context, key string, depth int, diag string) error {
	c.mu.Lock()
	if c.cancelled || !c.shouldStopLocked(key, depth) {
		c.mu.Unlock()
		return nil
	}
	w := &waiter{key: key, depth: depth, diag: diag, go_: make(chan struct{})}
	c.waiters = append(c.waiters, w)
	waiting := c.keysLocked()
	c.mu.Unlock()
	c.notify("paused", map[string]any{"waiting": waiting, "at": key, "depth": depth})
	select {
	case <-w.go_:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		for i, x := range c.waiters {
			if x == w {
				c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		return ctx.Err()
	}
}

// WireSnapshot is a wire's last value and, if it is watched, its recent ones.
type WireSnapshot struct {
	Last    any         `json:"last"`
	At      time.Time   `json:"at"`
	Count   int64       `json:"count"`
	History []WireValue `json:"history,omitempty"`
}

// WireValue is one value a wire carried and when.
type WireValue struct {
	Value any       `json:"value"`
	At    time.Time `json:"at"`
}

type wireRec struct {
	last  any
	at    time.Time
	count int64
	ring  []WireValue
	cap   int
}

// wireStore keeps what every wire last carried. Reading it never touches a run:
// a value is kept as a reference to the immutable token, and turned into JSON only
// when somebody asks.
type wireStore struct {
	mu    sync.Mutex
	wires map[string]*wireRec
}

const defaultHistory = 8

func (s *wireStore) record(key string, v any, history int) {
	if history < defaultHistory {
		history = defaultHistory
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wires == nil {
		s.wires = map[string]*wireRec{}
	}
	w := s.wires[key]
	if w == nil {
		w = &wireRec{}
		s.wires[key] = w
	}
	w.last, w.at = v, now
	w.count++
	w.cap = history
	w.ring = append(w.ring, WireValue{Value: v, At: now})
	if len(w.ring) > w.cap {
		w.ring = append([]WireValue(nil), w.ring[len(w.ring)-w.cap:]...)
	}
}

func (s *wireStore) snapshot() map[string]WireSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]WireSnapshot, len(s.wires))
	for k, w := range s.wires {
		snap := WireSnapshot{Last: jsonValue(w.last), At: w.at, Count: w.count}
		for _, h := range w.ring {
			snap.History = append(snap.History, WireValue{Value: jsonValue(h.Value), At: h.At})
		}
		out[k] = snap
	}
	return out
}
