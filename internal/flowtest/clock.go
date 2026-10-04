package flowtest

import (
	"container/heap"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// virtualClock is the time a flow test keeps. Nothing waits on it: the test
// moves it, straight to the next thing any node is waiting for, so five
// minutes of Delay is one step and a millisecond of real time.
//
// A timer's function runs on the goroutine that moves the clock, and the test
// lets the flow settle after each one before it moves on. That is what makes it
// exact rather than fast: a timer that fires sends a message, the message is
// handled and maybe arms the next timer, and only then does the clock look for
// what's due next, the same order the real minutes would have taken.
type virtualClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    int
	timers timerHeap
}

func newVirtualClock(start time.Time) *virtualClock { return &virtualClock{now: start} }

var _ node.Clock = (*virtualClock)(nil)

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *virtualClock) AfterFunc(d time.Duration, f func()) node.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &virtualTimer{c: c, when: c.now.Add(max(d, 0)), seq: c.seq, f: f}
	heap.Push(&c.timers, t)
	return t
}

// pending reports how many timers are waiting.
func (c *virtualClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// fireNext moves the clock to the earliest timer due no later than limit and
// runs it, reporting whether there was one.
func (c *virtualClock) fireNext(limit time.Time) bool {
	c.mu.Lock()
	if len(c.timers) == 0 || c.timers[0].when.After(limit) {
		c.mu.Unlock()
		return false
	}
	t := heap.Pop(&c.timers).(*virtualTimer)
	if t.when.After(c.now) {
		c.now = t.when
	}
	c.mu.Unlock()
	t.f()
	return true
}

// moveTo sets the clock forward to t, never back.
func (c *virtualClock) moveTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
}

type virtualTimer struct {
	c     *virtualClock
	when  time.Time
	seq   int
	f     func()
	index int
}

func (t *virtualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.index < 0 {
		return false
	}
	heap.Remove(&t.c.timers, t.index)
	return true
}

// timerHeap orders timers by when they're due, and two due at the same moment
// by which was set first, which is the order a real clock fires them in.
type timerHeap []*virtualTimer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if h[i].when.Equal(h[j].when) {
		return h[i].seq < h[j].seq
	}
	return h[i].when.Before(h[j].when)
}
func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *timerHeap) Push(x any) {
	t := x.(*virtualTimer)
	t.index = len(*h)
	*h = append(*h, t)
}
func (h *timerHeap) Pop() any {
	old := *h
	t := old[len(old)-1]
	old[len(old)-1] = nil
	t.index = -1
	*h = old[:len(old)-1]
	return t
}
