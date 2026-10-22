package wasmhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/HotLoop-io/hotloop-flow/internal/wasmhost/wasmasm"
)

// These tests use guests assembled in the test (see wasmasm), so they run
// without a toolchain and can't skip: the instructions that misbehave are
// written out in the test that depends on them.

const okResponse = `{"send":[[{"payload":"fine"}]]}`

// sharedCache keeps a guest built by a real toolchain, which is megabytes of
// WebAssembly, from being compiled to machine code again for every test.
var sharedCache = wazero.NewCompilationCache()

func compileBytes(t *testing.T, wasm []byte, l Limits, o Options) *Module {
	t.Helper()
	if o.Cache == nil {
		o.Cache = sharedCache
	}
	m, err := CompileWith(context.Background(), t.Name(), wasm, l, o)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func callOK(t *testing.T, m *Module) {
	t.Helper()
	resp, err := m.Call(context.Background(), Request{Msg: map[string]any{"payload": 1.0}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(resp.Send) != 1 || resp.Send[0][0]["payload"] != "fine" {
		t.Fatalf("response = %#v", resp)
	}
}

// heapInUse is what the Go heap holds after a collection, which is where a
// guest's linear memory lives.
func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func TestGuestPastTheMemoryCeilingTrapsAndTheMemoryComesBack(t *testing.T) {
	const ceiling = 256 << 20
	hog := compileBytes(t, wasmasm.Hog(), Limits{MaxMemoryBytes: ceiling, Timeout: 30 * time.Second}, Options{})
	good := compileBytes(t, wasmasm.Echo(okResponse), Limits{}, Options{})
	callOK(t, good)

	before := heapInUse()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	allocatedBefore := ms.TotalAlloc

	_, err := hog.Call(context.Background(), Request{Msg: map[string]any{}})

	if !errors.Is(err, ErrOutOfMemory) {
		t.Fatalf("a guest past its ceiling got %v, want ErrOutOfMemory", err)
	}
	if !strings.Contains(err.Error(), "out of bounds memory access") {
		t.Errorf("the error does not say what the guest did: %v", err)
	}
	if strings.Contains(err.Error(), "wasm stack trace") {
		t.Errorf("the error carries the guest's stack trace: %v", err)
	}

	// What the call allocated in all, which is the most the host ever held for
	// this guest. The guest grew a page at a time until the ceiling said no, so
	// it cost the ceiling; the host holding two or three times that while it
	// copied a buffer to grow it would show up here as well.
	runtime.ReadMemStats(&ms)
	cost := ms.TotalAlloc - allocatedBefore
	if cost < ceiling {
		t.Fatalf("the guest only cost %d MiB; it never reached its %d MiB ceiling, so the test proves nothing", cost>>20, ceiling>>20)
	}
	if cost > ceiling+ceiling/4 {
		t.Errorf("the guest cost %d MiB against a %d MiB ceiling", cost>>20, ceiling>>20)
	}

	// The trapped instance was closed rather than pooled, so its memory is
	// garbage. Whatever is left is the runtime's own small change.
	after := heapInUse()
	t.Logf("ceiling %d MiB, the call allocated %d MiB, heap %d KiB before and %d KiB after the trap",
		ceiling>>20, cost>>20, before>>10, after>>10)
	if after > before+8<<20 {
		t.Errorf("the heap is %d MiB after the trap, was %d MiB before: the guest's memory did not come back",
			after>>20, before>>20)
	}

	// Another node's guest never noticed, and neither did this node's next call.
	callOK(t, good)
}

func TestGuestThatLoopsPastTheTimeoutTrapsWithoutHoldingAnythingUp(t *testing.T) {
	spin := compileBytes(t, wasmasm.Spin(), Limits{Timeout: 150 * time.Millisecond}, Options{})
	good := compileBytes(t, wasmasm.Echo(okResponse), Limits{}, Options{})
	callOK(t, good) // its instance exists before the heap is measured

	before := heapInUse()
	start := time.Now()
	_, err := spin.Call(context.Background(), Request{Msg: map[string]any{}})
	took := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("a guest that never returns got %v, want ErrTimeout", err)
	}
	if took < 100*time.Millisecond || took > 3*time.Second {
		t.Errorf("the timeout was 150ms and the call took %v", took)
	}
	t.Logf("a guest that loops forever was stopped after %v: %v", took.Round(time.Millisecond), err)

	callOK(t, good)
	if after := heapInUse(); after > before+8<<20 {
		t.Errorf("the heap grew %d MiB over a timed-out guest", (after-before)>>20)
	}
}

func TestOneGuestTrappingLeavesAnotherRunning(t *testing.T) {
	spin := compileBytes(t, wasmasm.Spin(), Limits{Timeout: 200 * time.Millisecond}, Options{})
	good := compileBytes(t, wasmasm.Echo(okResponse), Limits{}, Options{})

	done := make(chan error, 1)
	go func() {
		_, err := spin.Call(context.Background(), Request{Msg: map[string]any{}})
		done <- err
	}()

	// While the first is spinning, the second answers, every time, without
	// waiting for it.
	var calls int
	for {
		select {
		case err := <-done:
			if !errors.Is(err, ErrTimeout) {
				t.Fatalf("the spinning guest got %v", err)
			}
			if calls < 20 {
				t.Errorf("only %d calls got through while the other guest was spinning", calls)
			}
			t.Logf("%d calls to the healthy guest completed while the other spun", calls)
			return
		default:
			callOK(t, good)
			calls++
		}
	}
}

func TestGuestThatTrapsIsNotReused(t *testing.T) {
	m := compileBytes(t, wasmasm.Trap(), Limits{}, Options{})
	_, err := m.Call(context.Background(), Request{Msg: map[string]any{}})
	if !errors.Is(err, ErrGuestTrapped) {
		t.Fatalf("got %v, want ErrGuestTrapped", err)
	}
	m.mu.Lock()
	pooled := len(m.pool)
	m.mu.Unlock()
	if pooled != 0 {
		t.Errorf("%d instances pooled after a trap", pooled)
	}
}

func TestGuestImportingWhatTheHostDoesNotProvideIsRefusedAtCompile(t *testing.T) {
	_, err := Compile(context.Background(), "x", wasmasm.ImportsNothingItShould(), Limits{})
	if err == nil || !strings.Contains(err.Error(), "env.reach_out") {
		t.Fatalf("err = %v, want it to name env.reach_out", err)
	}
}

func TestContextImportsNeedAContextCapability(t *testing.T) {
	wasm := wasmasm.ContextSet(0, "k", `1`, okResponse)
	_, err := Compile(context.Background(), "x", wasm, Limits{})
	if err == nil || !strings.Contains(err.Error(), "context_set") {
		t.Fatalf("err = %v, want a refusal naming context_set", err)
	}
}

// mapAccess is a context a test can look inside.
type mapAccess struct {
	mu   sync.Mutex
	vals map[ContextScope]map[string]any
}

func newMapAccess() *mapAccess {
	return &mapAccess{vals: map[ContextScope]map[string]any{ScopeFlow: {}, ScopeGlobal: {}}}
}

func (a *mapAccess) Get(s ContextScope, k string) (any, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := a.vals[s][k]
	return v, ok, nil
}

func (a *mapAccess) Set(s ContextScope, k string, v any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.vals[s][k] = v
	return nil
}

func TestContextCapabilityIsPerScope(t *testing.T) {
	acc := newMapAccess()
	ctx := WithContextAccess(context.Background(), acc)

	// Granted for the flow, and the write lands there and nowhere else.
	flow := compileBytes(t, wasmasm.ContextSet(0, "seen", `{"n":2}`, okResponse), Limits{},
		Options{Capabilities: Capabilities{ContextFlow: true}})
	if _, err := flow.Call(ctx, Request{Msg: map[string]any{}}); err != nil {
		t.Fatalf("a granted flow write failed: %v", err)
	}
	if v, _ := acc.vals[ScopeFlow]["seen"].(map[string]any); v["n"] != 2.0 {
		t.Errorf("flow context = %#v", acc.vals[ScopeFlow])
	}
	if len(acc.vals[ScopeGlobal]) != 0 {
		t.Errorf("global context = %#v", acc.vals[ScopeGlobal])
	}

	// Asking for the global scope with only the flow granted is refused, and
	// the refusal is a trap in the guest because it treats it as one.
	global := compileBytes(t, wasmasm.ContextSet(1, "stolen", `1`, okResponse), Limits{},
		Options{Capabilities: Capabilities{ContextFlow: true}})
	if _, err := global.Call(ctx, Request{Msg: map[string]any{}}); !errors.Is(err, ErrGuestTrapped) {
		t.Errorf("an ungranted global write got %v, want a trap", err)
	}
	if len(acc.vals[ScopeGlobal]) != 0 {
		t.Errorf("an ungranted write reached the global context: %#v", acc.vals[ScopeGlobal])
	}

	// Without an access on the call there is nothing to reach, granted or not.
	if _, err := flow.Call(context.Background(), Request{Msg: map[string]any{}}); !errors.Is(err, ErrGuestTrapped) {
		t.Errorf("a context call with no access got %v, want a trap", err)
	}
}

func TestContextGetReturnsTheValueAsJSON(t *testing.T) {
	acc := newMapAccess()
	acc.vals[ScopeGlobal]["limit"] = map[string]any{"hi": 90.0, "unit": "kPa"}
	ctx := WithContextAccess(context.Background(), acc)

	m := compileBytes(t, wasmasm.ContextGet(1, "limit"), Limits{},
		Options{Capabilities: Capabilities{ContextGlobal: true}})
	resp, err := m.Call(ctx, Request{Msg: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := resp.Send[0][0]["payload"].(map[string]any)
	if got["hi"] != 90.0 || got["unit"] != "kPa" {
		t.Errorf("payload = %#v", resp.Send[0][0]["payload"])
	}

	// A key that isn't there is the guest's own business: this one traps on it.
	missing := compileBytes(t, wasmasm.ContextGet(1, "nothing"), Limits{},
		Options{Capabilities: Capabilities{ContextGlobal: true}})
	if _, err := missing.Call(ctx, Request{Msg: map[string]any{}}); !errors.Is(err, ErrGuestTrapped) {
		t.Errorf("a missing key got %v", err)
	}
}

// The clock and random tests need a guest that asks the operating system's
// clock and random source through WASI, which means one built by a real
// toolchain. Built in CI, and CI fails the build if these skip.
const capGuestPath = "../../testdata/capguest/guest.wasm"

func loadCapGuest(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(capGuestPath))
	if err != nil {
		t.Skipf("no compiled guest at %s; build it with "+
			"GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o %s ./testdata/capguest",
			capGuestPath, capGuestPath)
	}
	return data
}

func readings(t *testing.T, m *Module) (nanos int64, random string) {
	t.Helper()
	resp, err := m.Call(context.Background(), Request{Msg: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	out := resp.Send[0][0]
	return int64(out["unixNano"].(float64)), out["random"].(string)
}

func TestWithoutTheClockCapabilityTimeIsNotTheRealTime(t *testing.T) {
	m := compileBytes(t, loadCapGuest(t), Limits{}, Options{})
	a, _ := readings(t, m)
	time.Sleep(50 * time.Millisecond)
	b, _ := readings(t, m)

	// wazero's stand-in clock starts at 2022-01-01 and steps by itself on every
	// read. What matters is that it isn't the plant's clock: waiting changes
	// nothing about it.
	if d := time.Since(time.Unix(0, a)); d < 24*time.Hour {
		t.Errorf("the guest saw the real time, %v ago, without the capability", d)
	}
	if b-a > int64(10*time.Millisecond) {
		t.Errorf("50ms of waiting moved the guest's clock by %v", time.Duration(b-a))
	}
}

func TestWithTheClockCapabilityTimeIsReal(t *testing.T) {
	m := compileBytes(t, loadCapGuest(t), Limits{}, Options{Capabilities: Capabilities{Clock: true}})
	a, _ := readings(t, m)
	if d := time.Since(time.Unix(0, a)); d < -time.Second || d > 5*time.Second {
		t.Errorf("the guest's clock is %v from now", d)
	}
	time.Sleep(20 * time.Millisecond)
	if b, _ := readings(t, m); b <= a {
		t.Errorf("the clock did not advance: %d then %d", a, b)
	}
}

func TestWithoutTheRandomCapabilityRandomIsTheSameEveryTime(t *testing.T) {
	// Two separate modules, so nothing is shared but the bytes.
	_, a := readings(t, compileBytes(t, loadCapGuest(t), Limits{}, Options{}))
	_, b := readings(t, compileBytes(t, loadCapGuest(t), Limits{}, Options{}))
	if a != b {
		t.Errorf("random bytes differed without the capability: %s and %s", a, b)
	}
}

func TestWithTheRandomCapabilityRandomIsReal(t *testing.T) {
	m := compileBytes(t, loadCapGuest(t), Limits{}, Options{Capabilities: Capabilities{Random: true}})
	_, a := readings(t, m)
	_, b := readings(t, m)
	_, frozen := readings(t, compileBytes(t, loadCapGuest(t), Limits{}, Options{}))
	if a == b || a == frozen || b == frozen {
		t.Errorf("random bytes repeated: %s %s %s", a, b, frozen)
	}
}
