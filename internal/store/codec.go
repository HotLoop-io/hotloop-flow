package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// The persistent stores keep context values and queued messages as JSON, which
// is the one format every value a flow handles already survives: it is how a
// message arrives from MQTT or HTTP and how it leaves for the editor.
//
// JSON on its own loses two things a flow does carry, so they are tagged:
// binary payloads (a file read, a captured frame) and times. A tagged value is
// an object with the single key "$bytes" or "$time". A user's own map that
// happens to use a key starting with "$" gets one more "$" on the way in and
// loses it on the way out, so nothing a flow writes can be mistaken for a tag.
//
// What cannot be stored is refused with the path to it, never skipped. A
// context key that quietly lost half its value is a bug nobody finds until the
// plant restarts.

const (
	tagBytes = "$bytes"
	tagTime  = "$time"
)

// EncodeValue serialises a context value or a message property.
func EncodeValue(v any) ([]byte, error) {
	j, err := toStorable(v, "value", 0)
	if err != nil {
		return nil, err
	}
	return marshalNoEscape(j)
}

// DecodeValue is the inverse of EncodeValue. Numbers come back as float64, the
// same as a value that arrived from a JSON message.
func DecodeValue(data []byte) (any, error) {
	var j any
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("decoding stored value: %w", err)
	}
	return fromStorable(j)
}

// EncodeMsg serialises a message for a durable wire.
//
// A message often carries a handle that only means something inside this
// process: the request and response an HTTP In node attaches, a connection. A
// queued message can't carry one across a restart, and it can't be replied to
// after the fact either, so those properties are left out and named in the
// second result for the caller to report. The payload is never left out: if it
// can't be stored the message is refused.
func EncodeMsg(m *engine.Msg) (body []byte, skipped []string, err error) {
	if m == nil {
		return nil, nil, fmt.Errorf("no message to store")
	}
	storable := make(map[string]any, len(m.Data))
	for k, v := range m.Data {
		j, err := toStorable(v, "msg."+k, 0)
		if err != nil {
			if k == engine.PropPayload {
				return nil, nil, err
			}
			skipped = append(skipped, k)
			continue
		}
		if strings.HasPrefix(k, "$") {
			k = "$" + k
		}
		storable[k] = j
	}
	sort.Strings(skipped)
	body, err = marshalNoEscape(storable)
	return body, skipped, err
}

// DecodeMsg is the inverse of EncodeMsg.
func DecodeMsg(data []byte) (*engine.Msg, error) {
	v, err := DecodeValue(data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stored message is %T, not an object", v)
	}
	return &engine.Msg{Data: m}, nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// A payload with an ampersand or angle bracket would otherwise grow by
	// five bytes each time, and the quota counts these bytes.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// maxStoreDepth matches the clone limit: a message deeper than this is refused
// rather than recursed into.
const maxStoreDepth = 512

func toStorable(v any, path string, depth int) (any, error) {
	if depth > maxStoreDepth {
		return nil, fmt.Errorf("%s is nested more than %d levels deep", path, maxStoreDepth)
	}
	switch t := v.(type) {
	case nil:
		return nil, nil
	case bool, string, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return t, nil
	case float32:
		return finite(float64(t), path)
	case float64:
		return finite(t, path)
	case time.Time:
		return map[string]any{tagTime: t.Format(time.RFC3339Nano)}, nil
	case engine.ImmutableBytes:
		return map[string]any{tagBytes: base64.StdEncoding.EncodeToString(t)}, nil
	case []byte:
		return map[string]any{tagBytes: base64.StdEncoding.EncodeToString(t)}, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			s, err := toStorable(e, path+"."+k, depth+1)
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(k, "$") {
				k = "$" + k
			}
			out[k] = s
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			s, err := toStorable(e, fmt.Sprintf("%s[%d]", path, i), depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	case *[]any:
		if t == nil {
			return nil, nil
		}
		return toStorable(*t, path, depth)
	case []string:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = e
		}
		return out, nil
	case []float64:
		out := make([]any, len(t))
		for i, e := range t {
			f, err := finite(e, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return nil, err
			}
			out[i] = f
		}
		return out, nil
	case map[string]string:
		out := make(map[string]any, len(t))
		for k, e := range t {
			if strings.HasPrefix(k, "$") {
				k = "$" + k
			}
			out[k] = e
		}
		return out, nil
	case error:
		return t.Error(), nil
	default:
		return nil, fmt.Errorf("%s holds a %T, which can't be stored: only numbers, strings, booleans, times, byte buffers, lists and objects survive a restart", path, v)
	}
}

func finite(f float64, path string) (any, error) {
	if f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
		return nil, fmt.Errorf("%s is %v, and JSON has no way to write that", path, f)
	}
	return f, nil
}

func fromStorable(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if s, ok := t[tagBytes].(string); ok {
				b, err := base64.StdEncoding.DecodeString(s)
				if err != nil {
					return nil, fmt.Errorf("decoding stored bytes: %w", err)
				}
				return b, nil
			}
			if s, ok := t[tagTime].(string); ok {
				tm, err := time.Parse(time.RFC3339Nano, s)
				if err != nil {
					return nil, fmt.Errorf("decoding stored time: %w", err)
				}
				return tm, nil
			}
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			d, err := fromStorable(e)
			if err != nil {
				return nil, err
			}
			if strings.HasPrefix(k, "$$") {
				k = k[1:]
			}
			out[k] = d
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			d, err := fromStorable(e)
			if err != nil {
				return nil, err
			}
			out[i] = d
		}
		return out, nil
	default:
		return v, nil
	}
}
