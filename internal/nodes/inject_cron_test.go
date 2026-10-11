package nodes

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// A crontab is about the wall clock, and so is the test of one, but not the
// wall clock this box happens to be running on. These tests used to read the
// real clock and allow a 300ms window after each second. That window assumed
// an idle machine: under the race detector in a full suite, with every core
// busy, the goroutine that wakes the timer can sit waiting for a core for most
// of a second, and the injection lands late through no fault of the schedule.
// The scheduler had nothing to fix. It reads the clock when it wakes and works
// the next date out from that, so a late wake costs that one injection its
// punctuality and nothing after it.
//
// So the schedule gets a clock the test moves, and the assertions are exact: a
// date either lands on the second the crontab names or the schedule is wrong.
// A late wake is simulated on purpose, to prove it doesn't carry forward.

// manualClock is a node.Clock that only moves when the test moves it. A timer's
// function runs on the goroutine that moved the clock.
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualTimer
}

type manualTimer struct {
	c    *manualClock
	when time.Time
	f    func()
	done bool
}

func newManualClock(start time.Time) *manualClock { return &manualClock{now: start} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) AfterFunc(d time.Duration, f func()) node.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{c: c, when: c.now.Add(max(d, 0)), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *manualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.done
	t.done = true
	return was
}

// step runs the earliest timer due by limit with the clock reading lateBy past
// the moment it was due, which is a process that woke late. It reports whether
// there was one.
func (c *manualClock) step(limit time.Time, lateBy time.Duration) bool {
	c.mu.Lock()
	var next *manualTimer
	for _, t := range c.timers {
		if !t.done && !t.when.After(limit) && (next == nil || t.when.Before(next.when)) {
			next = t
		}
	}
	if next == nil {
		c.mu.Unlock()
		return false
	}
	next.done = true
	if at := next.when.Add(lateBy); at.After(c.now) {
		c.now = at
	}
	c.mu.Unlock()
	next.f()
	return true
}

// advance moves the clock d forward, running every timer that falls due on the
// way at the moment it falls due.
func (c *manualClock) advance(d time.Duration) {
	limit := c.Now().Add(d)
	for c.step(limit, 0) {
	}
	c.mu.Lock()
	if limit.After(c.now) {
		c.now = limit
	}
	c.mu.Unlock()
}

// clockedServices is the test services with a clock the test owns.
type clockedServices struct {
	*testServices
	clock node.Clock
}

func (s clockedServices) Clock() node.Clock { return s.clock }

// payloadMillis returns the date payload each message carried: the
// millisecond it was injected.
func payloadMillis(e *testEmitter) []int64 {
	var out []int64
	for _, m := range e.on(0) {
		if f, ok := m.Payload().(float64); ok {
			out = append(out, int64(f))
		}
	}
	return out
}

// startMillis is 12:00:00.400, a start that is not on a second, so a schedule
// that counted a second from whenever it started would show.
func startMillis() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 400_000_000, time.Local) }

func wantMillis(t *testing.T, got []int64, want ...time.Time) {
	t.Helper()
	var w []int64
	for _, x := range want {
		w = append(w, x.UnixMilli())
	}
	if len(got) != len(w) {
		t.Fatalf("injected at %v, want %v", got, w)
	}
	for i := range w {
		if got[i] != w[i] {
			t.Errorf("injection %d landed at %d, want %d (%dms off)", i, got[i], w[i], got[i]-w[i])
		}
	}
}

func TestInjectFiresOnItsCrontab(t *testing.T) {
	clk := newManualClock(startMillis())
	n := build(t, "inject", `{"crontab":"* * * * * *","props":[{"p":"payload","vt":"date"}]}`,
		clockedServices{newTestServices(), clk})
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	// Three seconds and a bit. Every second on the second, not every second
	// from whenever the node happened to start.
	clk.advance(3500 * time.Millisecond)
	s := startMillis().Truncate(time.Second)
	wantMillis(t, payloadMillis(e), s.Add(1*time.Second), s.Add(2*time.Second), s.Add(3*time.Second))
}

// A wake that comes late costs that one injection its punctuality and nothing
// after it. The next date is worked out from the wall, not from when the last
// one happened to fire, so the schedule gets back on the second by itself.
func TestInjectCatchesUpAfterALateWake(t *testing.T) {
	clk := newManualClock(startMillis())
	n := build(t, "inject", `{"crontab":"* * * * * *","props":[{"p":"payload","vt":"date"}]}`,
		clockedServices{newTestServices(), clk})
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	s := startMillis().Truncate(time.Second)
	if !clk.step(s.Add(time.Hour), 912*time.Millisecond) {
		t.Fatal("the schedule armed no timer")
	}
	clk.advance(1600 * time.Millisecond)

	// The late one is 912ms past its second, that is what late means. The
	// next two are exactly on theirs.
	wantMillis(t, payloadMillis(e), s.Add(1*time.Second+912*time.Millisecond), s.Add(2*time.Second), s.Add(3*time.Second))
}

// Node-RED gives a repeat interval priority over a crontab. A flow carrying both
// has to keep repeating, not switch to a schedule it never ran on.
func TestInjectRepeatWinsOverCrontab(t *testing.T) {
	n := build(t, "inject", `{"repeat":"0.1","crontab":"0 0 1 1 *","props":[{"p":"payload","vt":"date"}]}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()
	waitFor(t, 3*time.Second, "the interval to fire", func() bool { return e.total() >= 3 })
}

// With "inject once at start", the startup injection comes first and the
// schedule starts after it, which is the order Node-RED uses.
func TestInjectOnceThenSchedule(t *testing.T) {
	clk := newManualClock(startMillis())
	n := build(t, "inject", `{"once":true,"onceDelay":"0.05","crontab":"* * * * * *",
        "props":[{"p":"payload","vt":"date"}]}`, clockedServices{newTestServices(), clk})
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	clk.advance(2 * time.Second)
	s := startMillis().Truncate(time.Second)
	wantMillis(t, payloadMillis(e),
		startMillis().Add(50*time.Millisecond), s.Add(1*time.Second), s.Add(2*time.Second))
}

func TestInjectRefusesABadCrontab(t *testing.T) {
	err := buildErr(t, "inject", `{"crontab":"* 25 * * *","props":[]}`, newTestServices())
	if err == nil || !strings.Contains(err.Error(), "hours field") {
		t.Fatalf("err = %v, want the crontab refused at deploy with the field named", err)
	}
}

// A crontab whose last date has passed stops, and says so, rather than
// sitting there looking scheduled.
func TestInjectSaysWhenItsScheduleHasEnded(t *testing.T) {
	n := build(t, "inject", `{"crontab":"0 0 0 1 1 * 2019","props":[]}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()
	waitFor(t, 3*time.Second, "the ended status", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, s := range e.statuses {
			if s.Text == "schedule has ended" {
				return true
			}
		}
		return false
	})
	if e.total() != 0 {
		t.Errorf("a schedule with no dates left injected %d messages", e.total())
	}
}

// The clock gets set after the schedule was worked out, the way an edge box
// with no battery-backed clock boots at the wrong time and is put right by NTP
// minutes later. The schedule follows the clock within one recheck instead of
// sleeping out the whole wait it computed against the wrong time.
func TestInjectScheduleFollowsAClockThatIsSet(t *testing.T) {
	var mu sync.Mutex
	// A minute before noon, as far as the box knows.
	fake := time.Date(2026, 10, 3, 11, 59, 0, 0, time.Local)

	n := build(t, "inject", `{"crontab":"0 0 12 * * *","props":[]}`, newTestServices())
	inj := n.(*injectNode)
	inj.recheck = 20 * time.Millisecond
	inj.clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fake
	}
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	time.Sleep(100 * time.Millisecond)
	if e.total() != 0 {
		t.Fatal("injected before noon")
	}
	// NTP answers: it is actually just past noon.
	mu.Lock()
	fake = time.Date(2026, 10, 3, 12, 0, 1, 0, time.Local)
	mu.Unlock()
	waitFor(t, 2*time.Second, "the noon injection after the clock was set", func() bool { return e.total() == 1 })
}

// The repeat interval also starts after the startup injection rather than
// alongside it.
func TestInjectOnceThenRepeat(t *testing.T) {
	n := build(t, "inject", `{"once":true,"onceDelay":"0.3","repeat":"0.2","props":[{"p":"payload","vt":"date"}]}`, newTestServices())
	e := newTestEmitter()
	started := time.Now()
	_, cancel := startNode(t, n, e)
	defer cancel()
	waitFor(t, 3*time.Second, "three injections", func() bool { return e.total() >= 3 })
	cancel()
	at := payloadMillis(e)
	if first := time.UnixMilli(at[0]).Sub(started); first < 250*time.Millisecond {
		t.Errorf("the first injection came %s after start: the interval fired before the startup injection", first)
	}
}
