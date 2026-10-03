package nodes

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// These run against the real clock, because the schedule's whole job is the
// real clock. The crontabs carry a seconds field so the tests take seconds, not
// minutes.

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

func TestInjectFiresOnItsCrontab(t *testing.T) {
	n := build(t, "inject", `{"crontab":"* * * * * *","props":[{"p":"payload","vt":"date"}]}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	waitFor(t, 5*time.Second, "three scheduled injections", func() bool { return e.total() >= 3 })
	cancel()

	at := payloadMillis(e)
	for i, ms := range at {
		// Every second on the second, not every second from whenever the
		// node happened to start.
		if off := ms % 1000; off > 300 {
			t.Errorf("injection %d landed %dms past the second; the schedule is drifting", i, off)
		}
		if i > 0 {
			if gap := ms - at[i-1]; gap < 700 || gap > 1300 {
				t.Errorf("injections %d and %d are %dms apart, want about a second", i-1, i, gap)
			}
		}
	}
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
	n := build(t, "inject", `{"once":true,"onceDelay":"0.05","crontab":"* * * * * *",
        "props":[{"p":"payload","vt":"date"}]}`, newTestServices())
	e := newTestEmitter()
	started := time.Now()
	_, cancel := startNode(t, n, e)
	defer cancel()

	waitFor(t, 5*time.Second, "the startup injection and two scheduled ones", func() bool { return e.total() >= 3 })
	cancel()
	at := payloadMillis(e)
	if first := time.UnixMilli(at[0]).Sub(started); first < 40*time.Millisecond || first > 500*time.Millisecond {
		t.Errorf("the startup injection came %s after start, want about 50ms", first)
	}
	for i, ms := range at[1:] {
		if off := ms % 1000; off > 300 {
			t.Errorf("scheduled injection %d landed %dms past the second", i+1, off)
		}
	}
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
