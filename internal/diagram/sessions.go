package diagram

import (
	"io"
	"sync"
)

// sessions holds what a run opened, so the run closes it when it ends however it
// ends: a serial port left open by a stopped run would be a port nobody can use.
type sessions struct {
	mu   sync.Mutex
	list []io.Closer
}

func (s *sessions) add(c io.Closer) {
	s.mu.Lock()
	s.list = append(s.list, c)
	s.mu.Unlock()
}

func (s *sessions) closeAll() {
	s.mu.Lock()
	list := s.list
	s.list = nil
	s.mu.Unlock()
	for _, c := range list {
		_ = c.Close()
	}
}
