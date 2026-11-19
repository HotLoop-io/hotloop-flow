package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/fence"
)

// clock is a clock a test moves by hand, shared by the store and the managers so
// they agree on what time it is.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// memStore is the store's protocol in memory, with the store's clock.
type memStore struct {
	mu      sync.Mutex
	clk     *clock
	holder  string
	epoch   int64
	expires time.Time
	down    error
	// slow moves the clock during Acquire, the time a slow answer takes.
	slow time.Duration
}

func (s *memStore) Acquire(_ context.Context, name, holder string, ttl time.Duration) (Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down != nil {
		return Grant{}, s.down
	}
	if s.slow > 0 {
		s.clk.add(s.slow)
	}
	now := s.clk.now()
	switch {
	case s.holder == "":
		s.holder, s.epoch, s.expires = holder, 1, now.Add(ttl)
		return Grant{Held: true, Epoch: s.epoch, Holder: holder}, nil
	case s.holder == holder:
		s.expires = now.Add(ttl)
		return Grant{Held: true, Epoch: s.epoch, Holder: holder}, nil
	case s.expires.Before(now):
		s.holder, s.epoch, s.expires = holder, s.epoch+1, now.Add(ttl)
		return Grant{Held: true, Epoch: s.epoch, Holder: holder}, nil
	}
	return Grant{Held: false, Epoch: s.epoch, Holder: s.holder}, nil
}

func (s *memStore) Release(_ context.Context, _, holder string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holder == holder {
		s.expires = s.clk.now().Add(-time.Second)
	}
	return nil
}

func rig(t *testing.T) (*clock, *memStore, func(holder string) *Manager) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	st := &memStore{clk: clk}
	mk := func(holder string) *Manager {
		m, err := New(Config{Store: st, Name: "flow", Holder: holder, TTL: 15 * time.Second, Renew: 4 * time.Second, Margin: 5 * time.Second, Attempt: 2 * time.Second, Now: clk.now})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	return clk, st, mk
}

func TestTheSettingsRefuseWhatWouldMakeTheHolderStopForNoReason(t *testing.T) {
	ok := Config{Store: &memStore{}, Name: "flow", Holder: "a", TTL: 15 * time.Second, Renew: 4 * time.Second, Margin: 5 * time.Second, Attempt: 2 * time.Second}
	if err := ok.Validate(); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	for name, mut := range map[string]func(*Config){
		"margin as long as the ttl": func(c *Config) { c.Margin = c.TTL },
		"renewing too slowly":       func(c *Config) { c.Renew = 9 * time.Second },
		"no name":                   func(c *Config) { c.Name = "" },
		"no holder":                 func(c *Config) { c.Holder = "" },
		"no store":                  func(c *Config) { c.Store = nil },
		"zero ttl":                  func(c *Config) { c.TTL = 0 },
	} {
		c := ok
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The first to ask is the writer. The other is the standby, and its writes are
// refused until it has the lease.
func TestOnlyTheHolderMayWrite(t *testing.T) {
	_, _, mk := rig(t)
	a, b := mk("a"), mk("b")
	a.try(context.Background())
	b.try(context.Background())
	if err := a.Check("modbus write"); err != nil {
		t.Fatalf("the holder was refused: %v", err)
	}
	if err := b.Check("modbus write"); !errors.Is(err, fence.ErrStandby) {
		t.Fatalf("the standby got %v, want ErrStandby", err)
	}
	if s := b.Status(); s.Role != Standby || s.Owner != "a" || s.Refused["modbus write"] != 1 {
		t.Fatalf("%+v", s)
	}
	if s := a.Status(); s.Role != Active || s.Epoch != 1 || s.Takeovers != 1 {
		t.Fatalf("%+v", s)
	}
}

// The old holder stops writing before the new one can start: it refuses once it has
// less than the margin left, which is before the lease runs out, and the standby
// can take it only after it has. After that, the old one is refused for good.
func TestTheStaleHolderIsRefusedBeforeAnyoneElseCanWrite(t *testing.T) {
	clk, _, mk := rig(t)
	a, b := mk("a"), mk("b")
	a.try(context.Background())
	// A stops renewing (a hung process, a cut link). Ten seconds on it still has
	// five left, which is the margin, and may write.
	clk.add(10 * time.Second)
	if err := a.Check("x"); err != nil {
		t.Fatalf("with exactly the margin left: %v", err)
	}
	b.try(context.Background())
	if b.Check("x") == nil {
		t.Fatal("the standby wrote while the old holder's lease had not run out")
	}
	// A moment more and the holder has less than the margin: it refuses, and the
	// lease has not run out, so nobody else may write yet either.
	clk.add(time.Second)
	if err := a.Check("x"); !errors.Is(err, fence.ErrLost) {
		t.Fatalf("a holder under its margin got %v, want ErrLost", err)
	}
	b.try(context.Background())
	if b.Check("x") == nil {
		t.Fatal("the standby wrote before the lease ran out")
	}
	// The lease runs out and the standby takes it, with a higher number.
	clk.add(5 * time.Second)
	b.try(context.Background())
	if err := b.Check("x"); err != nil {
		t.Fatalf("the new holder was refused: %v", err)
	}
	if s := b.Status(); s.Epoch != 2 {
		t.Fatalf("the number did not go up: %+v", s)
	}
	// The old one comes back and asks. It is not the holder, and stays refused.
	a.try(context.Background())
	if err := a.Check("x"); err == nil {
		t.Fatal("the old holder wrote after another took the lease")
	}
	if s := a.Status(); s.Role != Lost || s.Owner != "b" {
		t.Fatalf("%+v", s)
	}
}

// A lease is counted from before it was asked for, so a slow answer cannot make the
// holder think it has longer than the store does.
func TestALeaseIsCountedFromBeforeTheAsk(t *testing.T) {
	clk, st, mk := rig(t)
	st.slow = 3 * time.Second
	a := mk("a")
	start := clk.now()
	a.try(context.Background())
	if got := a.until.Sub(start); got != 15*time.Second {
		t.Fatalf("the lease ends %s after the ask, want 15s from before it", got)
	}
}

// A store that does not answer does not stop a holder at once: it goes on until its
// own lease is too short, and then holds its writes.
func TestAHolderThatCannotRenewRunsOutAndStops(t *testing.T) {
	clk, st, mk := rig(t)
	a := mk("a")
	a.try(context.Background())
	st.down = errors.New("connection refused")
	clk.add(4 * time.Second)
	a.try(context.Background())
	if err := a.Check("x"); err != nil {
		t.Fatalf("a failed renewal with time left: %v", err)
	}
	if a.Status().Error == "" {
		t.Error("the failure is not in the status")
	}
	clk.add(7 * time.Second) // 11 s in, 4 s left, under the 5 s margin
	if err := a.Check("x"); !errors.Is(err, fence.ErrLost) {
		t.Fatalf("got %v, want ErrLost", err)
	}
}

// A clean stop gives the lease up, so the standby does not wait out a ttl.
func TestAHolderThatStopsGivesTheLeaseUp(t *testing.T) {
	clk, _, mk := rig(t)
	a, b := mk("a"), mk("b")
	a.try(context.Background())
	a.release()
	clk.add(time.Second)
	b.try(context.Background())
	if err := b.Check("x"); err != nil {
		t.Fatalf("the standby after a release: %v", err)
	}
	if a.Check("x") == nil {
		t.Fatal("the released holder may still write")
	}
}

func TestTheGateIsTheOneEveryWriteAsks(t *testing.T) {
	_, _, mk := rig(t)
	a := mk("a")
	restore := fence.Set(a)
	defer restore()
	if !fence.On() {
		t.Fatal("the gate is not set")
	}
	if err := fence.Check("modbus write"); !errors.Is(err, fence.ErrStandby) {
		t.Fatalf("an instance that has not asked for the lease: %v", err)
	}
	a.try(context.Background())
	if err := fence.Check("modbus write"); err != nil {
		t.Fatalf("the holder: %v", err)
	}
	restore()
	if fence.On() {
		t.Fatal("restore left a gate set")
	}
}
