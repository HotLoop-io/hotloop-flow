package node

import "time"

// Clock is the time a node keeps: what time it is, and calling back once a
// while has passed.
//
// A node that waits, a Delay, a Trigger, an Inject on an interval or a crontab,
// a Join or a Batch with a timeout, asks its services for one instead of
// reaching for the time package. In a running flow that's the wall clock. In a
// flow test it's a clock the test moves, so a five-minute Delay is checked in
// milliseconds and a crontab that fires at half past six fires when the test
// says it's half past six.
type Clock interface {
	Now() time.Time

	// AfterFunc calls f once d has passed. The wall clock calls it on a
	// goroutine of its own, the way time.AfterFunc does; a test's clock may
	// call it on the goroutine that moved the clock forward. Either way f
	// must not assume it runs on the node's own goroutine.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending AfterFunc.
type Timer interface {
	// Stop cancels the call, reporting whether it did. False means f has
	// already been called, or is being called, or the timer was stopped
	// before.
	Stop() bool
}

// WallClock is the real one.
var WallClock Clock = wallClock{}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// ClockOf returns the clock a node's services carry, or the wall clock when
// they carry none, which is every time except under a flow test.
func ClockOf(svc Services) Clock {
	if p, ok := svc.(interface{ Clock() Clock }); ok {
		if c := p.Clock(); c != nil {
			return c
		}
	}
	return WallClock
}
