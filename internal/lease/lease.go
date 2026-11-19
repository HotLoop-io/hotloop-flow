// Package lease holds the lease that decides which of two Flow installs may write
// to equipment, and the fencing number that goes up each time the writer changes.
//
// The active instance renews the lease before it runs out. A standby tries for it
// and gets it only once it has expired, and the number goes up. Every equipment
// write checks, just before it sends, that this instance is the holder and has
// more of its lease left than a write needs (the margin). Because the holder
// counts its lease from the moment it asked for it, never from the answer, it
// stops writing before the store lets anyone else start.
package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/fence"
	"github.com/HotLoop-io/hotloop-flow/internal/metrics"
)

// Grant is the store's answer to one try for the lease.
type Grant struct {
	// Held says this holder has the lease now.
	Held bool
	// Epoch is the fencing number: it goes up each time the holder changes.
	Epoch int64
	// Holder is who has it, which is this holder when Held.
	Holder string
}

// Store is where the lease lives. Acquire is one atomic step: it gives the lease
// to holder when holder already has it or it has expired, for ttl from now by the
// store's own clock, and reports who has it either way. Release gives it up early.
type Store interface {
	Acquire(ctx context.Context, name, holder string, ttl time.Duration) (Grant, error)
	Release(ctx context.Context, name, holder string) error
}

// Config is one lease manager.
type Config struct {
	Store  Store
	Name   string
	Holder string
	// TTL is how long a grant lasts. Renew is how often it is asked for again, and
	// Margin is how much lease a write needs left, which is the longest a write
	// may take.
	TTL, Renew, Margin time.Duration
	// Attempt bounds one try, so a store that does not answer cannot hold the
	// manager past the time it would have renewed.
	Attempt time.Duration
	Log     *slog.Logger
	// Now is the clock, for tests. It must be monotonic.
	Now func() time.Time
}

// Validate refuses settings under which an active instance could dip below its
// margin between renewals, which would make it stop writing for no reason.
func (c Config) Validate() error {
	switch {
	case c.Store == nil:
		return errors.New("no lease store")
	case c.Name == "":
		return errors.New("the lease has no name")
	case c.Holder == "":
		return errors.New("this instance has no holder name")
	case c.TTL <= 0 || c.Renew <= 0 || c.Margin <= 0 || c.Attempt <= 0:
		return errors.New("ttl, renew, margin and attempt must all be above zero")
	case c.Margin >= c.TTL:
		return fmt.Errorf("the write margin %s must be less than the ttl %s, or the holder could never write", c.Margin, c.TTL)
	case c.Renew+c.Attempt >= c.TTL-c.Margin:
		return fmt.Errorf("renewing every %s with up to %s per try leaves the holder under its %s margin between renewals; make the ttl longer, or the renewal faster", c.Renew, c.Attempt, c.Margin)
	}
	return nil
}

// Role is what this instance is.
type Role string

const (
	// Standby has never held the lease, or does not hold it now.
	Standby Role = "standby"
	// Active holds the lease with more than the margin left.
	Active Role = "active"
	// Lost held the lease and cannot say it still does.
	Lost Role = "lost"
)

// Status is what an instance reports about itself.
type Status struct {
	Role   Role   `json:"role"`
	Holder string `json:"holder"`
	Name   string `json:"name"`
	// Epoch is the fencing number this instance last held, and Current the number
	// the store last said was current.
	Epoch   int64 `json:"epoch"`
	Current int64 `json:"current"`
	// Owner is who the store says has the lease.
	Owner string `json:"owner,omitempty"`
	// LeftMS is how much of its lease an active instance has left.
	LeftMS int64 `json:"leftMs,omitempty"`
	// Takeovers counts the times this instance took the lease from another holder
	// or from nobody; Refused counts the writes it held back.
	Takeovers int64            `json:"takeovers"`
	Refused   map[string]int64 `json:"refused"`
	Error     string           `json:"error,omitempty"`
}

// Manager keeps the lease and answers the fence.
type Manager struct {
	cfg Config

	mu        sync.Mutex
	held      bool
	everHeld  bool
	until     time.Time
	epoch     int64
	current   int64
	owner     string
	lastErr   string
	takeovers int64

	refused sync.Map // op -> *atomic.Int64
	stop    chan struct{}
	done    chan struct{}
}

// New checks the settings and returns a manager. Run starts it.
func New(cfg Config) (*Manager, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Manager{cfg: cfg, stop: make(chan struct{}), done: make(chan struct{})}, nil
}

// Hostname is the default holder name.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "flow"
	}
	return h
}

// Start tries for the lease once, before it returns, so the first thing an
// instance knows about itself is whether it is the writer or the standby, and then
// keeps trying every Renew in the background until ctx ends or Close is called.
func (m *Manager) Start(ctx context.Context) {
	m.try(ctx)
	go m.run(ctx)
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	t := time.NewTicker(m.cfg.Renew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.release()
			return
		case <-m.stop:
			m.release()
			return
		case <-t.C:
			m.try(ctx)
		}
	}
}

// Close stops the manager, gives the lease up so a standby can take it at once,
// and waits for that.
func (m *Manager) Close() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
}

func (m *Manager) try(ctx context.Context) {
	started := m.cfg.Now()
	actx, cancel := context.WithTimeout(ctx, m.cfg.Attempt)
	g, err := m.cfg.Store.Acquire(actx, m.cfg.Name, m.cfg.Holder, m.cfg.TTL)
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		// Keep what we had: a holder whose lease runs out stops writing on its own,
		// and a standby stays a standby.
		m.lastErr = err.Error()
		return
	}
	m.lastErr = ""
	m.current, m.owner = g.Epoch, g.Holder
	if !g.Held {
		if m.held {
			m.cfg.Log.Warn("the lease was taken by another holder", "lease", m.cfg.Name, "owner", g.Holder, "epoch", g.Epoch)
		}
		m.held = false
		return
	}
	if !m.held || m.epoch != g.Epoch {
		m.takeovers++
		m.cfg.Log.Info("this instance holds the lease", "lease", m.cfg.Name, "holder", m.cfg.Holder, "epoch", g.Epoch)
	}
	m.held, m.everHeld, m.epoch = true, true, g.Epoch
	// Counted from before the try, never from the answer: the store started its
	// clock no earlier than this, so this instance's idea of when the lease ends
	// is never later than the store's.
	m.until = started.Add(m.cfg.TTL)
}

func (m *Manager) release() {
	m.mu.Lock()
	was := m.held
	m.held = false
	m.mu.Unlock()
	if !was {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Attempt)
	defer cancel()
	if err := m.cfg.Store.Release(ctx, m.cfg.Name, m.cfg.Holder); err != nil {
		m.cfg.Log.Warn("could not give the lease up", "error", err)
	}
}

// Check is the fence: nil when this instance holds the lease with more than the
// margin left, and the reason it may not write when it does not.
func (m *Manager) Check(op string) error {
	m.mu.Lock()
	held, ever, until := m.held, m.everHeld, m.until
	m.mu.Unlock()
	left := until.Sub(m.cfg.Now())
	if held && left >= m.cfg.Margin {
		return nil
	}
	m.count(op)
	if ever {
		return fence.ErrLost
	}
	return fence.ErrStandby
}

func (m *Manager) count(op string) {
	v, _ := m.refused.LoadOrStore(op, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// Status says what this instance is now.
func (m *Manager) Status() Status {
	m.mu.Lock()
	held, ever, until := m.held, m.everHeld, m.until
	st := Status{Holder: m.cfg.Holder, Name: m.cfg.Name, Epoch: m.epoch, Current: m.current, Owner: m.owner, Takeovers: m.takeovers, Error: m.lastErr}
	m.mu.Unlock()
	left := until.Sub(m.cfg.Now())
	switch {
	case held && left >= m.cfg.Margin:
		st.Role, st.LeftMS = Active, left.Milliseconds()
	case ever:
		st.Role = Lost
	default:
		st.Role = Standby
	}
	st.Refused = map[string]int64{}
	m.refused.Range(func(k, v any) bool {
		st.Refused[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return st
}

// Families are what the manager reports to the metrics endpoint: the role, the
// fencing number it holds, the takeovers, and the writes it has held back.
func (m *Manager) Families() []metrics.Family {
	one := func(name, help, typ string, v func(Status) float64) metrics.Family {
		return metrics.Family{Name: name, Help: help, Type: typ, Collect: func() []metrics.Sample {
			return []metrics.Sample{{Value: v(m.Status())}}
		}}
	}
	return []metrics.Family{
		one("hotloop_flow_failover_active", "1 when this install holds the lease and may write to equipment, 0 when it is the standby or has lost it.", "gauge",
			func(s Status) float64 {
				if s.Role == Active {
					return 1
				}
				return 0
			}),
		one("hotloop_flow_failover_epoch", "The fencing number this install last held.", "gauge", func(s Status) float64 { return float64(s.Epoch) }),
		one("hotloop_flow_failover_takeovers_total", "How many times this install took the lease.", "counter", func(s Status) float64 { return float64(s.Takeovers) }),
		{Name: "hotloop_flow_failover_refused_total", Help: "Equipment writes held back because this install does not hold the lease, by what was writing.", Type: "counter",
			Collect: func() []metrics.Sample {
				var out []metrics.Sample
				for op, n := range m.Status().Refused {
					out = append(out, metrics.Sample{Labels: [][2]string{{"op", op}}, Value: float64(n)})
				}
				return out
			}},
	}
}
