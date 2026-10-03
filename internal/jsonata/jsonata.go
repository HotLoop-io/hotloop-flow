// Package jsonata evaluates JSONata expressions the way a flow expects them to
// behave: against a message, with Node-RED's own functions bound in.
//
// The language itself comes from github.com/recolabs/gnata, a pure Go JSONata 2.x
// (MIT). It was picked over writing one because it already passes the published
// jsonata-js test suite, it is maintained, and it has one dependency. Writing a
// second implementation of a language this size is how you end up with two
// slightly different JSONatas and a bug report that only reproduces in one of
// them. The suite is run against this package, not just against gnata, so what
// is proven is what the nodes actually call.
//
// What this package adds is everything that is Node-RED rather than JSONata:
//
//   - $flowContext(key), $globalContext(key) and $env(name), resolved against
//     the node evaluating the expression
//   - $clone(value), a deep copy
//   - legacy mode: an expression that names msg is evaluated against {msg: msg}
//     instead of msg, which is how expressions written before Node-RED 0.17
//     still work and how plenty of imported flows are still written
//   - conversion between message values and JSONata values in both directions
package jsonata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/recolabs/gnata"
)

// Timeout bounds one evaluation. JSONata has no loops, but a recursive lambda
// or a range like [1..1e7] can still keep a node busy for a long time, and a
// node's goroutine is the only thing draining its inbox. Node-RED has no limit
// at all, so a bad expression there stalls the whole event loop. A variable so
// a test can prove it without waiting ten seconds.
var Timeout = 10 * time.Second

// legacyRef is Node-RED's own test for an expression that names msg, taken
// from prepareJSONataExpression in @node-red/util.
var legacyRef = regexp.MustCompile(`(^|[^a-zA-Z0-9_'".])msg([^a-zA-Z0-9_'"]|$)`)

// Expr is a compiled expression bound to the node that evaluates it. It is
// safe for concurrent use.
type Expr struct {
	src    string
	expr   *gnata.Expression
	env    *gnata.CustomEnvironment
	legacy bool
}

// Compile parses an expression and binds Node-RED's functions to svc. svc may
// be nil, in which case the context functions find nothing and $env finds only
// the process environment, same as an expression evaluated outside a node in
// Node-RED.
func Compile(src string, svc node.Services, opts ...gnata.Option) (*Expr, error) {
	if len(opts) == 0 {
		opts = []gnata.Option{gnata.WithTimeout(Timeout)}
	}
	e, err := gnata.Compile(src, opts...)
	if err != nil {
		return nil, fmt.Errorf("JSONata expression %q: %w", src, err)
	}
	return &Expr{
		src:    src,
		expr:   e,
		env:    gnata.NewCustomEnvironment(nodeFunctions(svc)),
		legacy: legacyRef.MatchString(src),
	}, nil
}

// Source returns the expression text.
func (x *Expr) Source() string { return x.src }

// EvalMsg evaluates against a message, the way a node property typed jsonata
// is evaluated. vars are extra $-bindings, such as $A and $I in a Join node's
// reduce expression.
//
// defined is false when the expression produced nothing, which JSONata calls
// undefined and is not the same thing as null.
func (x *Expr) EvalMsg(ctx context.Context, msg *engine.Msg, vars map[string]any) (value any, defined bool, err error) {
	var data any = map[string]any{}
	if msg != nil {
		data = msg.Data
	}
	return x.EvalValue(ctx, data, vars)
}

// EvalValue evaluates against any message value, such as one element of an
// array the Sort node is ordering. Legacy mode applies here too: Node-RED wraps
// whatever it evaluates against, not only whole messages.
func (x *Expr) EvalValue(ctx context.Context, v any, vars map[string]any) (value any, defined bool, err error) {
	data := ToValue(v)
	if x.legacy {
		m := gnata.NewOrderedMapWithCapacity(1)
		m.Set("msg", data)
		data = m
	}
	return x.Eval(ctx, data, vars)
}

// Eval evaluates against input that is already a JSONata value: a converted
// message, decoded JSON, or nil for no input at all.
func (x *Expr) Eval(ctx context.Context, input any, vars map[string]any) (value any, defined bool, err error) {
	bound := make(map[string]any, len(vars))
	for k, v := range vars {
		bound[k] = ToValue(v)
	}
	res, err := x.expr.EvalWithCustomEnvironmentAndVars(ctx, input, x.env, bound)
	if err != nil {
		return nil, false, fmt.Errorf("JSONata expression %q: %w", x.src, err)
	}
	if res == nil {
		return nil, false, nil
	}
	return FromValue(res), true, nil
}

// errorCodeText finds the code in an error's text: evaluation errors read
// "T2001: ...", parse errors "JSONata error S0101 ...".
var errorCodeText = regexp.MustCompile(`^(?:JSONata error )?([A-Z]\d{4})\b`)

// ErrorCode returns the JSONata error code a compile or an evaluation failed
// with, such as S0101 or T2001, or "" when the error did not come from the
// language.
func ErrorCode(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		v := reflect.ValueOf(e)
		if v.Kind() == reflect.Pointer && !v.IsNil() && v.Elem().Kind() == reflect.Struct {
			if f := v.Elem().FieldByName("Code"); f.IsValid() && f.Kind() == reflect.String && f.String() != "" {
				return f.String()
			}
		}
		if m := errorCodeText.FindStringSubmatch(e.Error()); m != nil {
			return m[1]
		}
	}
	return ""
}

// nodeFunctions are the functions Node-RED binds into every expression.
func nodeFunctions(svc node.Services) map[string]gnata.CustomFunc {
	contextGet := func(scope node.ContextScope) gnata.CustomFunc {
		return func(args []any, _ any) (any, error) {
			if svc == nil || len(args) == 0 {
				return nil, nil
			}
			key, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("the context key must be a string, got %T", args[0])
			}
			// A second argument names a context store. There is one store
			// here, so every name resolves to it, which is also what
			// Node-RED does with a store name it does not know.
			v, found, err := contextPath(svc.Context(scope), key)
			if err != nil || !found {
				return nil, err
			}
			return ToValue(v), nil
		}
	}
	return map[string]gnata.CustomFunc{
		"flowContext":   contextGet(node.ScopeFlow),
		"globalContext": contextGet(node.ScopeGlobal),
		"env": func(args []any, _ any) (any, error) {
			if len(args) == 0 {
				return "", nil
			}
			name, ok := args[0].(string)
			if !ok {
				return "", nil
			}
			// Node-RED answers "" rather than undefined for a variable that
			// is not set.
			if svc != nil {
				if v, ok := svc.Env(name); ok {
					return v, nil
				}
				return "", nil
			}
			if v, ok := os.LookupEnv(name); ok {
				return v, nil
			}
			return "", nil
		},
		"clone": func(args []any, _ any) (any, error) {
			if len(args) == 0 {
				return nil, nil
			}
			return ToValue(FromValue(args[0])), nil
		},
		"moment": func([]any, any) (any, error) {
			// Node-RED binds moment.js here. Reimplementing moment's parser
			// and formatter in Go is a project of its own, and returning
			// something almost right would be worse than refusing.
			return nil, errors.New("$moment is not available in this build: use $now(), " +
				"$fromMillis() and $toMillis(), which take the same picture strings")
		},
	}
}

// ToValue converts a message value into what JSONata expects: JSON-shaped
// maps, arrays, strings, float64 numbers, booleans and null.
//
// A Go nil inside a message is JSON null, because that is what decoding a JSON
// null gives. A buffer becomes an array of byte values, which is what a Buffer
// turns into whenever Node-RED serialises one. A value with no JSON shape at all
// — an HTTP response handle riding on msg.res, a connection a config node
// handed along — is left out, as if the property were not there.
func ToValue(v any) any {
	out, ok := toValue(v, 0)
	if !ok {
		return nil
	}
	return out
}

// maxDepth stops a self-referential message from recursing forever.
const maxDepth = 256

func toValue(v any, depth int) (any, bool) {
	if depth > maxDepth {
		return nil, false
	}
	switch t := v.(type) {
	case nil:
		return gnata.Null, true
	case bool, string, float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int8:
		return float64(t), true
	case int16:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint:
		return float64(t), true
	case uint8:
		return float64(t), true
	case uint16:
		return float64(t), true
	case uint32:
		return float64(t), true
	case uint64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return t.String(), true
		}
		return f, true
	case time.Time:
		// A JavaScript Date serialises as its ISO string.
		return t.UTC().Format("2006-01-02T15:04:05.000Z"), true
	case engine.ImmutableBytes:
		return byteArray(t), true
	case []byte:
		return byteArray(t), true
	case map[string]any:
		// Built in sorted key order. A message map has no order of its own,
		// and handing JSONata one that iterates differently every time would
		// make $keys(), $each() and every constructed object come out in a
		// different order from one message to the next.
		m := gnata.NewOrderedMapWithCapacity(len(t))
		for _, k := range sortedKeys(t) {
			if cv, ok := toValue(t[k], depth+1); ok {
				m.Set(k, cv)
			}
		}
		return m, true
	case map[string]string:
		m := gnata.NewOrderedMapWithCapacity(len(t))
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			m.Set(k, t[k])
		}
		return m, true
	case []any:
		return listValue(t, depth), true
	case *[]any:
		if t == nil {
			return gnata.Null, true
		}
		return listValue(*t, depth), true
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, true
	case []float64:
		out := make([]any, len(t))
		for i, f := range t {
			out[i] = f
		}
		return out, true
	case error:
		return map[string]any{"message": t.Error()}, true
	}
	// Values gnata produced itself, an OrderedMap or its null, pass through.
	if gnata.IsNull(v) {
		return v, true
	}
	if _, ok := v.(*gnata.OrderedMap); ok {
		return v, true
	}
	return nil, false
}

func listValue(in []any, depth int) []any {
	out := make([]any, 0, len(in))
	for _, e := range in {
		if cv, ok := toValue(e, depth+1); ok {
			out = append(out, cv)
		} else {
			// Keep the position: an array index is data.
			out = append(out, gnata.Null)
		}
	}
	return out
}

func byteArray(b []byte) []any {
	out := make([]any, len(b))
	for i, c := range b {
		out[i] = float64(c)
	}
	return out
}

// FromValue converts a JSONata result back into a plain message value:
// map[string]any, []any, string, float64, bool, or nil for null.
func FromValue(v any) any {
	v = gnata.NormalizeValue(v)
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = FromValue(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = FromValue(e)
		}
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			// JSON has no such numbers, and neither does a message that has
			// to survive a trip through the debug sidebar or a broker.
			return nil
		}
		return t
	}
	return v
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// contextPath reads a context key that may carry a property path, the way
// flow.get("line.speed") reads property speed of what is stored under line.
func contextPath(store node.Context, expr string) (any, bool, error) {
	key, rest := expr, ""
	if i := strings.IndexAny(expr, ".["); i > 0 {
		key = expr[:i]
		if expr[i] == '.' {
			rest = expr[i+1:]
		} else {
			rest = expr[i:]
		}
	}
	root, ok, err := store.Get(key)
	if err != nil || !ok || rest == "" {
		return root, ok, err
	}
	return engine.GetProperty(root, rest)
}
