// Package fence is the one question every write to equipment asks first: may
// this instance write right now?
//
// With no failover configured there is no gate and the answer is always yes, so
// a single instance behaves exactly as it always has. With failover on, the gate
// is the lease manager, and the answer is no on a standby and no on an active
// instance whose lease has run out or is too close to running out for a write
// to finish inside it. That check, made at the moment of each write, is what
// makes "never two writers" provable: after the old active's lease has run out
// it cannot send, whatever state its process is in.
package fence

import (
	"errors"
	"sync/atomic"
)

// ErrStandby is the refusal on an instance that does not hold the lease.
var ErrStandby = errors.New("this instance is the standby and holds equipment writes until it takes over the lease")

// ErrLost is the refusal on an instance that held the lease and can no longer be
// sure it does: it did not renew in time, so another instance may be the writer.
var ErrLost = errors.New("this instance's lease has run out or is too close to running out, so it holds equipment writes rather than risk a second writer")

// Gate decides one write. op names what is about to write, for the count of what
// was refused.
type Gate interface {
	Check(op string) error
}

type holder struct{ g Gate }

var current atomic.Pointer[holder]

// Set installs the gate for the process and returns a function that puts back
// the one that was there. Passing nil turns fencing off.
func Set(g Gate) (restore func()) {
	prev := current.Swap(&holder{g})
	return func() { current.Store(prev) }
}

// Check asks the gate whether a write by op may go ahead. Nil means it may, and
// with no gate set it always does.
func Check(op string) error {
	h := current.Load()
	if h == nil || h.g == nil {
		return nil
	}
	return h.g.Check(op)
}

// On says whether a gate is set.
func On() bool {
	h := current.Load()
	return h != nil && h.g != nil
}
