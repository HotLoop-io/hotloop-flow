package nodes

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// Helpers for the nodes that talk to a node.StandIn under a flow test instead
// of the outside world. Each node does its own work right up to the wire and
// calls the stand-in where it would have dialled, published, queried or
// written; these turn what it would have sent into something a test can read,
// and what the test scripted back into what the node expects to get.

// wireValue is bytes as a test reads them: text when they're text, which they
// nearly always are on a plant floor, and the bytes themselves when they
// aren't.
func wireValue(b []byte) any {
	if utf8.Valid(b) {
		return string(b)
	}
	return append([]byte(nil), b...)
}

// replyBytes reads a scripted reply's body. A string is the bytes of the
// string; anything else a test can write in YAML, an object or a list or a
// number, goes as JSON, which is what a web service or a device that speaks
// JSON would have sent.
func replyBytes(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(t), nil
	case []byte:
		return t, nil
	case engine.ImmutableBytes:
		return t, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("the scripted reply can't be written as bytes: %w", err)
	}
	return b, nil
}

// replyNumber reads a number out of a scripted reply, with a fallback when the
// test left it out.
func replyNumber(reply map[string]any, key string, def float64) (float64, error) {
	v, ok := reply[key]
	if !ok || v == nil {
		return def, nil
	}
	f, ok := asFloat(v)
	if !ok {
		return 0, fmt.Errorf("the scripted reply's %s is %v, which isn't a number", key, v)
	}
	return f, nil
}

// replyList reads a list out of a scripted reply, empty when it's left out.
func replyList(reply map[string]any, key string) ([]any, error) {
	v, ok := reply[key]
	if !ok || v == nil {
		return nil, nil
	}
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("the scripted reply's %s should be a list, got %T", key, v)
	}
	return l, nil
}
