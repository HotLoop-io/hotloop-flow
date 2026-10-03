package flowtest

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// contains reports whether got holds everything in want, and when it doesn't,
// says where they first part ways.
//
// Objects match when every key in want is in got with a matching value; extra
// keys in got are fine, because a test about the payload shouldn't break when a
// node starts setting msg.topic. Arrays match element for element and have to
// be the same length, because "the batch had three readings" is usually the
// point. Numbers compare by value whatever Go type carries them, and a buffer
// matches a string with the same bytes.
func contains(want, got any, path string) (bool, string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := asObject(got)
		if !ok {
			return false, fmt.Sprintf("%s: wanted an object, got %s", pathName(path), show(got))
		}
		for _, k := range sortedKeys(w) {
			gv, present := g[k]
			if !present {
				return false, fmt.Sprintf("%s: missing", pathName(join(path, k)))
			}
			if ok, why := contains(w[k], gv, join(path, k)); !ok {
				return false, why
			}
		}
		return true, ""

	case []any:
		g, ok := got.([]any)
		if !ok {
			return false, fmt.Sprintf("%s: wanted an array, got %s", pathName(path), show(got))
		}
		if len(g) != len(w) {
			return false, fmt.Sprintf("%s: wanted %d elements, got %d", pathName(path), len(w), len(g))
		}
		for i := range w {
			if ok, why := contains(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); !ok {
				return false, why
			}
		}
		return true, ""

	case float64:
		if g, ok := number(got); ok && (g == w || (math.IsNaN(g) && math.IsNaN(w))) {
			return true, ""
		}

	case string:
		switch g := got.(type) {
		case string:
			if g == w {
				return true, ""
			}
		case []byte:
			if string(g) == w {
				return true, ""
			}
		case engine.ImmutableBytes:
			if string(g) == w {
				return true, ""
			}
		}

	case bool:
		if g, ok := got.(bool); ok && g == w {
			return true, ""
		}

	case nil:
		if got == nil {
			return true, ""
		}
	}
	return false, fmt.Sprintf("%s: wanted %s, got %s", pathName(path), show(want), show(got))
}

func asObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func pathName(path string) string {
	if path == "" {
		return "the message"
	}
	return "msg." + path
}

// show renders a value the way a failure message prints it: as JSON, cut
// short when it's long, because a 4 KB payload in a CI log helps nobody find
// the one field that was wrong.
func show(v any) string {
	switch b := v.(type) {
	case []byte:
		return fmt.Sprintf("a %d byte buffer %s", len(b), strconv.Quote(clip(string(b), 80)))
	case engine.ImmutableBytes:
		return fmt.Sprintf("a %d byte buffer %s", len(b), strconv.Quote(clip(string(b), 80)))
	}
	out, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return clip(string(out), 200)
}

// showMsg renders a whole message for a failure, leaving out the id every
// message carries and nobody wrote an expectation about.
func showMsg(data map[string]any) string {
	trimmed := make(map[string]any, len(data))
	for k, v := range data {
		if k == engine.PropMsgID {
			continue
		}
		trimmed[k] = v
	}
	return show(trimmed)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("... (%d more bytes)", len(s)-cut)
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// errorText returns msg.error.message, which is where a Catch node puts the
// error it caught.
func errorText(data map[string]any) (string, bool) {
	e, ok := data["error"].(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := e["message"].(string)
	return s, ok
}
