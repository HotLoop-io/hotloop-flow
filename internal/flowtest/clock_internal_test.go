package flowtest

import (
	"testing"
	"time"
)

// TestVirtualTimersFireInOrderAndStopMeansStop pins down the two promises the
// nodes lean on and can't show from the outside, because every one of them
// guards against a late timer anyway: timers due at the same moment fire in
// the order they were set, and a stopped timer never fires.
func TestVirtualTimersFireInOrderAndStopMeansStop(t *testing.T) {
	start := time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)
	c := newVirtualClock(start)

	var fired []string
	c.AfterFunc(time.Minute, func() { fired = append(fired, "a") })
	stopped := c.AfterFunc(30*time.Second, func() { fired = append(fired, "stopped") })
	c.AfterFunc(time.Minute, func() { fired = append(fired, "b") })
	c.AfterFunc(-time.Second, func() { fired = append(fired, "now") })

	if !stopped.Stop() {
		t.Fatal("stopping a pending timer reported it had already fired")
	}
	if stopped.Stop() {
		t.Fatal("stopping it twice reported a second cancellation")
	}
	if c.pending() != 3 {
		t.Fatalf("%d timers pending, want 3", c.pending())
	}

	for c.fireNext(start.Add(time.Hour)) {
	}
	if got := len(fired); got != 3 || fired[0] != "now" || fired[1] != "a" || fired[2] != "b" {
		t.Fatalf("fired %v, want [now a b]", fired)
	}
	if !c.Now().Equal(start.Add(time.Minute)) {
		t.Fatalf("the clock is at %s, want the last timer's time", c.Now())
	}

	late := c.AfterFunc(time.Hour, func() { fired = append(fired, "late") })
	if c.fireNext(start.Add(time.Hour)) {
		t.Fatal("a timer past the limit fired")
	}
	c.moveTo(start)
	if !c.Now().Equal(start.Add(time.Minute)) {
		t.Fatal("the clock went backwards")
	}
	if !late.Stop() {
		t.Fatal("the late timer should still have been pending")
	}
}
