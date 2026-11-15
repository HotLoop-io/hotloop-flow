// Package instruments holds the per-instrument write switch.
//
// A FlowLab diagram can talk to a bench instrument over a raw socket or a serial
// port. Reading one (an *IDN? or a MEAS:VOLT? query) changes nothing. Writing one
// can: a supply's output, a hipot's high voltage, a load's current. So every
// instrument starts with its writes off, a diagram that tries one is refused with
// the reason, and only someone holding the instrument.write permission turns an
// instrument's writes on, by its address, one instrument at a time. Reads never
// go through the switch.
package instruments

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Allowance is the state of one instrument's switch.
type Allowance struct {
	Allowed bool      `json:"allowed"`
	By      string    `json:"by,omitempty"`
	At      time.Time `json:"at,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// Switch is the set of switches, kept on disk so a restart does not turn an
// instrument's writes back on or off by itself.
type Switch struct {
	path string

	mu sync.Mutex
	m  map[string]Allowance
}

// Open reads the switches from path. A file that does not exist means every
// instrument is off.
func Open(path string) (*Switch, error) {
	s := &Switch{path: path, m: map[string]Allowance{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.m); err != nil {
		return nil, fmt.Errorf("%s is not a set of instrument switches: %w", path, err)
	}
	return s, nil
}

// Normalize makes an instrument's name the one the switch is kept under: its
// address without a tcp:// prefix's noise, a serial port without its settings, so
// the same instrument opened two ways is one switch.
func Normalize(id string) string {
	id = strings.TrimSpace(id)
	if rest, ok := strings.CutPrefix(id, "serial://"); ok {
		if i := strings.IndexByte(rest, '?'); i >= 0 {
			rest = rest[:i]
		}
		return "serial://" + rest
	}
	if strings.HasPrefix(id, "/dev/") || strings.HasPrefix(strings.ToUpper(id), "COM") {
		return "serial://" + id
	}
	return "tcp://" + strings.TrimPrefix(id, "tcp://")
}

// Allowed says whether writes to the instrument are on. Everything is off until
// somebody turns it on.
func (s *Switch) Allowed(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[Normalize(id)].Allowed
}

// Set turns an instrument's writes on or off and keeps who did it and why.
func (s *Switch) Set(id string, allowed bool, by, note string) error {
	id = Normalize(id)
	if id == "tcp://" || id == "serial://" {
		return fmt.Errorf("no instrument named")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.m[id]
	s.m[id] = Allowance{Allowed: allowed, By: by, At: time.Now().UTC(), Note: strings.TrimSpace(note)}
	if err := s.saveLocked(); err != nil {
		if had {
			s.m[id] = prev
		} else {
			delete(s.m, id)
		}
		return err
	}
	return nil
}

// List is every instrument that has a switch, by name.
func (s *Switch) List() map[string]Allowance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Allowance, len(s.m))
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// Names is the switches' instruments in order.
func (s *Switch) Names() []string {
	l := s.List()
	out := make([]string, 0, len(l))
	for k := range l {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Switch) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
