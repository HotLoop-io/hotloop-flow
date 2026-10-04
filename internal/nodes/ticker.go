package nodes

import (
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// clockTicker calls a function every interval on a node.Clock, which is how the
// nodes that repeat (Inject on an interval, the Delay node's rate limiter,
// Batch by time) run on the wall clock in a flow and on the test's clock in a
// flow test.
//
// It behaves like a time.Ticker read by one goroutine. Ticks keep to the
// schedule rather than drifting by however long each call took, a tick that
// would land while the last call is still running is skipped rather than run
// alongside it, and calls never overlap, so the function needs no locking of
// its own against itself.
type clockTicker struct {
	clock node.Clock
	f     func()

	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	timer    node.Timer
	stopped  bool
	// gen tells a timer armed before a Reset or a Stop that it's stale.
	gen int
}

func newClockTicker(c node.Clock, interval time.Duration, f func()) *clockTicker {
	t := &clockTicker{clock: c, f: f, interval: interval}
	t.mu.Lock()
	t.next = c.Now().Add(interval)
	t.arm()
	t.mu.Unlock()
	return t
}

// arm schedules the next tick. The caller holds mu.
func (t *clockTicker) arm() {
	gen := t.gen
	wait := max(t.next.Sub(t.clock.Now()), 0)
	t.timer = t.clock.AfterFunc(wait, func() { t.fire(gen) })
}

func (t *clockTicker) fire(gen int) {
	t.mu.Lock()
	if t.stopped || gen != t.gen {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()

	t.f()

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || gen != t.gen {
		// Stopped, or Reset by f itself, which armed its own timer.
		return
	}
	now := t.clock.Now()
	t.next = t.next.Add(t.interval)
	if !t.next.After(now) {
		t.next = now.Add(t.interval)
	}
	t.arm()
}

// Reset starts the period again from now, at a new interval.
func (t *clockTicker) Reset(interval time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.gen++
	if t.timer != nil {
		t.timer.Stop()
	}
	t.interval = interval
	t.next = t.clock.Now().Add(interval)
	t.arm()
}

// Stop ends it. A call already running finishes; no other starts.
func (t *clockTicker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	t.gen++
	if t.timer != nil {
		t.timer.Stop()
	}
}
