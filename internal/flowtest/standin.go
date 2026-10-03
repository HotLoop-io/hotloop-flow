package flowtest

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// callPort is the tap a node's calls to the outside world are recorded on,
// alongside its output ports and what it received.
const callPort = -2

// outside is one test's stand-in for everything beyond the process. Every node
// gets its own view of it, and every call any of them makes is recorded and
// answered from the test's script.
type outside struct {
	rec *recorder

	mu      sync.Mutex
	replies map[string][]Reply
	calls   map[string]int
}

func newOutside(rec *recorder, replies map[string][]Reply) *outside {
	return &outside{rec: rec, replies: replies, calls: map[string]int{}}
}

// forNode is what the runtime hands each node as it's built.
func (o *outside) forNode(id string) node.StandIn { return nodeOutside{o: o, id: id} }

type nodeOutside struct {
	o  *outside
	id string
}

func (n nodeOutside) Call(kind string, sent map[string]any) (map[string]any, error) {
	data, _ := jsonValue(sent).(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	n.o.rec.add(tap{node: n.id, port: callPort}, &engine.Msg{Data: data})

	n.o.mu.Lock()
	k := n.o.calls[n.id]
	n.o.calls[n.id]++
	script := n.o.replies[n.id]
	n.o.mu.Unlock()

	if len(script) == 0 {
		return nil, nil
	}
	r := script[min(k, len(script)-1)]
	if r.Error != "" {
		return nil, errors.New(r.Error)
	}
	if r.Reply == nil {
		return nil, nil
	}
	// A copy, because the node may keep what it's given on a message that
	// the next call's reply must not change underneath it.
	return engine.WrapMsg(r.Reply).Clone().Data, nil
}

// jsonValue turns what a node would have sent into the shapes a test file can
// write: objects, lists, strings, float64 numbers, booleans and null. Bytes
// that are text become the text.
func jsonValue(v any) any {
	switch t := v.(type) {
	case nil, string, bool, float64:
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = jsonValue(e)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = e
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = jsonValue(e)
		}
		return out
	case []string:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = e
		}
		return out
	case []byte:
		return bytesValue(t)
	case engine.ImmutableBytes:
		return bytesValue(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	}
	if f, ok := number(v); ok {
		return f
	}
	return fmt.Sprint(v)
}

func bytesValue(b []byte) any {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]any, len(b))
	for i, c := range b {
		out[i] = float64(c)
	}
	return out
}
