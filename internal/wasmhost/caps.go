package wasmhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Capabilities are the host access a guest is given. The zero value gives it
// nothing, and that is what Compile uses.
//
// Each one is wired in only when it is true. There is no switch that is merely
// checked: a guest without Clock is handed wazero's frozen clock, a guest
// without Random is handed a fixed byte sequence, and a guest without a context
// capability is refused at compile time if it imports the context functions at
// all.
type Capabilities struct {
	// Clock gives the guest the real wall clock and the real monotonic clock,
	// through WASI clock_time_get.
	Clock bool
	// Random gives the guest the operating system's random bytes, through WASI
	// random_get.
	Random bool
	// ContextFlow lets the guest read and write the flow context of the flow its
	// node is in.
	ContextFlow bool
	// ContextGlobal lets the guest read and write the global context.
	ContextGlobal bool
}

func (c Capabilities) context() bool { return c.ContextFlow || c.ContextGlobal }

// Options configures CompileWith.
type Options struct {
	Capabilities Capabilities

	// Cache, when set, is shared between modules so the same bytes are compiled
	// to machine code once however many nodes run them.
	Cache wazero.CompilationCache
}

// ContextScope names a context store a guest can reach.
type ContextScope int

const (
	ScopeFlow ContextScope = iota
	ScopeGlobal
)

// ContextAccess is what the context host functions call. The caller puts it on
// the context it passes to Module.Call with WithContextAccess, so each call
// reaches the context of the node making it and no instance can hold on to
// another node's.
type ContextAccess interface {
	Get(scope ContextScope, key string) (any, bool, error)
	Set(scope ContextScope, key string, value any) error
}

type accessKey struct{}

// WithContextAccess returns a context carrying the access a guest's context
// calls go through.
func WithContextAccess(ctx context.Context, a ContextAccess) context.Context {
	return context.WithValue(ctx, accessKey{}, a)
}

// The host module's name and the functions it exports.
//
//	context_get(scope i32, keyPtr i32, keyLen i32, outPtr i32, outCap i32) -> i32
//	context_set(scope i32, keyPtr i32, keyLen i32, valPtr i32, valLen i32) -> i32
//
// Scope is 0 for the flow's context and 1 for the global one. A value crosses
// as JSON. context_get returns the length of the JSON, writing it only when it
// fits in outCap, so a guest that gets back more than it offered retries with a
// bigger buffer. Both return a negative number for a failure.
const (
	HostModule = "hotloop_flow"
	FuncGet    = "context_get"
	FuncSet    = "context_set"

	// ResultMissing is what context_get returns for a key that isn't there.
	ResultMissing = -1
	// ResultDenied is what either returns when the scope wasn't granted, the
	// key or the JSON was bad, or the store refused.
	ResultDenied = -2

	maxContextKey   = 1 << 10
	maxContextValue = 1 << 20
)

func checkImports(c wazero.CompiledModule, caps Capabilities) error {
	if mems := c.ImportedMemories(); len(mems) > 0 {
		mod, name, _ := mems[0].Import()
		return fmt.Errorf("guest imports its memory (%s.%s); the host gives each guest its own", mod, name)
	}
	for _, f := range c.ImportedFunctions() {
		mod, name, _ := f.Import()
		switch mod {
		case "wasi_snapshot_preview1":
		case HostModule:
			if name != FuncGet && name != FuncSet {
				return fmt.Errorf("guest imports %s.%s, which the host does not provide", mod, name)
			}
			if !caps.context() {
				return fmt.Errorf("guest imports %s.%s but was not given a context capability", mod, name)
			}
		default:
			return fmt.Errorf("guest imports %s.%s, which the host does not provide", mod, name)
		}
	}
	return nil
}

func instantiateHostModule(ctx context.Context, rt wazero.Runtime, caps Capabilities) error {
	i32 := api.ValueTypeI32
	_, err := rt.NewHostModuleBuilder(HostModule).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, s []uint64) {
			s[0] = uint64(uint32(int32(contextGet(ctx, m, caps, s))))
		}), []api.ValueType{i32, i32, i32, i32, i32}, []api.ValueType{i32}).
		Export(FuncGet).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, s []uint64) {
			s[0] = uint64(uint32(int32(contextSet(ctx, m, caps, s))))
		}), []api.ValueType{i32, i32, i32, i32, i32}, []api.ValueType{i32}).
		Export(FuncSet).
		Instantiate(ctx)
	return err
}

// scopeFor maps the guest's scope number to a granted scope.
func scopeFor(n uint32, caps Capabilities) (ContextScope, bool) {
	switch ContextScope(n) {
	case ScopeFlow:
		return ScopeFlow, caps.ContextFlow
	case ScopeGlobal:
		return ScopeGlobal, caps.ContextGlobal
	}
	return 0, false
}

func readKey(m api.Module, ptr, n uint32) (string, bool) {
	if n == 0 || n > maxContextKey {
		return "", false
	}
	b, ok := m.Memory().Read(ptr, n)
	if !ok {
		return "", false
	}
	return strings.Clone(string(b)), true
}

func contextGet(ctx context.Context, m api.Module, caps Capabilities, s []uint64) int32 {
	scope, granted := scopeFor(uint32(s[0]), caps)
	access, _ := ctx.Value(accessKey{}).(ContextAccess)
	if !granted || access == nil {
		return ResultDenied
	}
	key, ok := readKey(m, uint32(s[1]), uint32(s[2]))
	if !ok {
		return ResultDenied
	}
	v, found, err := access.Get(scope, key)
	if err != nil {
		return ResultDenied
	}
	if !found {
		return ResultMissing
	}
	data, err := json.Marshal(v)
	if err != nil || len(data) > maxContextValue {
		return ResultDenied
	}
	outPtr, outCap := uint32(s[3]), uint32(s[4])
	if uint32(len(data)) <= outCap && len(data) > 0 {
		if !m.Memory().Write(outPtr, data) {
			return ResultDenied
		}
	}
	return int32(len(data))
}

func contextSet(ctx context.Context, m api.Module, caps Capabilities, s []uint64) int32 {
	scope, granted := scopeFor(uint32(s[0]), caps)
	access, _ := ctx.Value(accessKey{}).(ContextAccess)
	if !granted || access == nil {
		return ResultDenied
	}
	key, ok := readKey(m, uint32(s[1]), uint32(s[2]))
	if !ok {
		return ResultDenied
	}
	n := uint32(s[4])
	if n == 0 || n > maxContextValue {
		return ResultDenied
	}
	raw, ok := m.Memory().Read(uint32(s[3]), n)
	if !ok {
		return ResultDenied
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ResultDenied
	}
	if err := access.Set(scope, key, v); err != nil {
		return ResultDenied
	}
	return 0
}
