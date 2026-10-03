package api

import (
	"net"
	"sync"
	"time"
)

// limiter slows down password guessing.
//
// Two limits, because one alone fails in a way somebody will find. Five wrong
// passwords for one account from one address locks that account out from that
// address for a while: enough to stop a script, short enough that the operator
// who fat-fingered it at 3 AM is back in after a coffee. Locking the account
// everywhere would hand anybody who can reach the port a way to lock the real
// operator out from across the plant, so the account lock is per address. A
// second, looser limit counts every failure from an address whatever account
// it tried, which is what stops one machine walking a list of usernames.
//
// Behind a reverse proxy every request has the proxy's address, so the second
// limit then applies to everybody at once. That is the safe way round to be
// wrong, and the numbers are configurable.
type limiter struct {
	attempts   int
	perAddress int
	window     time.Duration
	lockFor    time.Duration
	now        func() time.Time

	mu        sync.Mutex
	byAccount map[string]*strikes // account and address
	byAddress map[string]*strikes
}

type strikes struct {
	at          []time.Time
	lockedUntil time.Time
}

// maxTracked bounds how many accounts and addresses are remembered, so a flood
// of junk usernames can't grow memory without end. Past it, the oldest are
// forgotten first, which only ever errs towards letting somebody try again.
const maxTracked = 10000

func newLimiter(attempts, perAddress int, window, lockFor time.Duration) *limiter {
	return &limiter{
		attempts: attempts, perAddress: perAddress, window: window, lockFor: lockFor,
		now:       time.Now,
		byAccount: map[string]*strikes{},
		byAddress: map[string]*strikes{},
	}
}

func accountKey(user, addr string) string { return user + "\x00" + addr }

// addressOf is the host part of a remote address.
func addressOf(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

// check reports whether a sign-in may be tried, and if not, for how long not.
func (l *limiter) check(user, addr string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	wait := time.Duration(0)
	for _, s := range []*strikes{l.byAccount[accountKey(user, addr)], l.byAddress[addr]} {
		if s != nil && s.lockedUntil.After(now) {
			if d := s.lockedUntil.Sub(now); d > wait {
				wait = d
			}
		}
	}
	return wait, wait == 0
}

// fail counts a failed sign-in and reports whether it just locked something.
func (l *limiter) fail(user, addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	locked := false
	if l.strike(l.byAccount, accountKey(user, addr), l.attempts, now) {
		locked = true
	}
	if l.strike(l.byAddress, addr, l.perAddress, now) {
		locked = true
	}
	return locked
}

func (l *limiter) strike(m map[string]*strikes, key string, limit int, now time.Time) bool {
	s := m[key]
	if s == nil {
		if len(m) >= maxTracked {
			l.forgetOldest(m)
		}
		s = &strikes{}
		m[key] = s
	}
	kept := s.at[:0]
	for _, t := range s.at {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	s.at = append(kept, now)
	if len(s.at) >= limit && !s.lockedUntil.After(now) {
		s.lockedUntil = now.Add(l.lockFor)
		s.at = nil
		return true
	}
	return false
}

func (l *limiter) forgetOldest(m map[string]*strikes) {
	var oldestKey string
	var oldest time.Time
	for k, s := range m {
		last := s.lockedUntil
		if n := len(s.at); n > 0 && s.at[n-1].After(last) {
			last = s.at[n-1]
		}
		if oldestKey == "" || last.Before(oldest) {
			oldestKey, oldest = k, last
		}
	}
	delete(m, oldestKey)
}

// succeed clears the account's strikes from that address. The address's
// count stays: one good password doesn't excuse fifty bad ones for other
// accounts.
func (l *limiter) succeed(user, addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byAccount, accountKey(user, addr))
}
