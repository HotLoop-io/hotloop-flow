package runtime

import (
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// Capture keeps the last few messages that came into the flow at a source, and
// the last few that reached a Debug node, so that something an operator saw in
// the debug sidebar can become a test: the message that went in, and what came
// out the other end. A message keeps its id from one node to the next, so the
// id the debug sidebar shows is enough to find both.
//
// It's a ring, small on purpose, and a message too big to be worth keeping is
// skipped rather than copied: this is a convenience for the editor, and it
// must never be the thing that grows a pod.
type Capture struct {
	mu      sync.Mutex
	entries []Captured
	next    int
	// maxBytes is the most a single kept message may hold, roughly counted.
	maxBytes int
}

// Captured is one kept message.
type Captured struct {
	NodeID string `json:"nodeId"`
	// Port is the output a source sent it from; -1 for a Debug node's arrival.
	Port  int            `json:"port"`
	MsgID string         `json:"msgId"`
	Data  map[string]any `json:"-"`
	At    time.Time      `json:"at"`
}

// Defaults for the capture the server keeps.
const (
	DefaultCaptureSize     = 64
	DefaultCaptureMaxBytes = 64 << 10
)

// NewCapture keeps the last size messages of at most maxBytes each.
func NewCapture(size, maxBytes int) *Capture {
	return &Capture{entries: make([]Captured, size), maxBytes: maxBytes}
}

func (c *Capture) add(nodeID string, port int, m *engine.Msg, at time.Time) {
	if c == nil || len(c.entries) == 0 {
		return
	}
	if !fits(m.Data, c.maxBytes) {
		return
	}
	e := Captured{NodeID: nodeID, Port: port, MsgID: m.ID(), Data: m.Clone().Data, At: at}
	c.mu.Lock()
	c.entries[c.next] = e
	c.next = (c.next + 1) % len(c.entries)
	c.mu.Unlock()
}

// Find returns every kept message with an id, oldest first.
func (c *Capture) Find(msgID string) []Captured {
	if c == nil || msgID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Captured
	for i := range len(c.entries) {
		e := c.entries[(c.next+i)%len(c.entries)]
		if e.MsgID == msgID {
			out = append(out, e)
		}
	}
	return out
}

// fits reports whether a message is under budget bytes, counting strings and
// buffers by length and everything else as a few bytes, and giving up the
// moment it's over rather than walking a huge message to the end.
func fits(v any, budget int) bool {
	left := budget
	var walk func(v any, depth int) bool
	walk = func(v any, depth int) bool {
		if depth > 32 {
			return false
		}
		switch t := v.(type) {
		case string:
			left -= len(t)
		case []byte:
			left -= len(t)
		case engine.ImmutableBytes:
			left -= len(t)
		case map[string]any:
			for k, e := range t {
				left -= len(k)
				if left < 0 || !walk(e, depth+1) {
					return false
				}
			}
		case []any:
			for _, e := range t {
				if left < 0 || !walk(e, depth+1) {
					return false
				}
			}
		default:
			left -= 8
		}
		return left >= 0
	}
	return walk(v, 0)
}

// SetCapture installs the capture. Call it before Start.
func (rt *Runtime) SetCapture(c *Capture) { rt.capture = c }

// isSource reports whether a node type brings messages into the flow from
// outside it rather than passing them along: no input, and not one of the
// handlers the runtime itself routes errors, statuses and completions to.
func (rt *Runtime) isSource(typ string) bool {
	switch typ {
	case TypeCatch, TypeStatus, TypeComplete:
		return false
	}
	reg, ok := rt.reg.Lookup(typ)
	return ok && reg.Descriptor.Inputs == 0
}
