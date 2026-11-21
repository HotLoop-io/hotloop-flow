package runtime_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	rt "github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// twWriter stands in for a write to a device that answers slowly. It merges its
// queue the way a Modbus write does, by register, only when every way into it
// starts at an inject, and records the values it was given in order.
type twWriter struct {
	mu   sync.Mutex
	got  []float64
	wait time.Duration
}

func (w *twWriter) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	time.Sleep(w.wait)
	v, _ := m.Data["value"].(float64)
	w.mu.Lock()
	w.got = append(w.got, v)
	w.mu.Unlock()
	return nil
}
func (w *twWriter) CoalesceKey(m *engine.Msg) (string, bool) {
	if a, ok := m.Data["address"]; ok {
		return fmt.Sprint(a), true
	}
	return "", false
}
func (w *twWriter) RequiresTimerRoot() bool { return true }
func (w *twWriter) values() []float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]float64(nil), w.got...)
}

type twSource struct{}

func (twSource) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

var twWriters sync.Map

func init() {
	if err := node.Default.Register(node.Descriptor{
		Type: "tw-writer", Category: node.CategoryCommon, Color: "#E31837", Icon: "cog",
		Inputs: 1, Outputs: 0, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(def *node.Definition) (node.Node, error) {
		w := &twWriter{wait: 40 * time.Millisecond}
		twWriters.Store(def.Node.ID, w)
		return w, nil
	}); err != nil {
		panic(err)
	}
	if err := node.Default.Register(node.Descriptor{
		Type: "tw-operator", Category: node.CategoryCommon, Color: "#E31837", Icon: "cog",
		Inputs: 0, Outputs: 1, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(def *node.Definition) (node.Node, error) { return twSource{}, nil }); err != nil {
		panic(err)
	}
}

func startTW(t *testing.T, doc string) (*rt.Runtime, *twWriter) {
	t.Helper()
	flows, err := engine.ParseFlows([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	r := rt.New(node.Default, flows, rt.Options{InboxCapacity: 1024})
	go func() {
		for range r.Events() {
		}
	}()
	if fails := r.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("start: %v", fails[0])
	}
	t.Cleanup(func() { r.Stop(context.Background()) })
	v, _ := twWriters.Load("w")
	return r, v.(*twWriter)
}

func snapOf(r *rt.Runtime, id string) rt.Snapshot {
	for _, s := range r.Snapshots() {
		if s.NodeID == id {
			return s
		}
	}
	return rt.Snapshot{}
}

func burst(t *testing.T, r *rt.Runtime, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		m := engine.NewMsg()
		m.Data["address"], m.Data["value"] = float64(7), float64(i)
		if err := r.Inject("w", m); err != nil {
			t.Fatal(err)
		}
	}
}

// A write that mirrors a value read every second, with an inject at the root,
// is the same register written again. Against a device slower than the inject,
// older values that have not gone out are merged into the newest, and the queue
// does not grow.
func TestAnInjectRootedWriterMergesItsQueueAgainstASlowDevice(t *testing.T) {
	r, w := startTW(t, `[{"id":"t","type":"tab","label":"t"},
{"id":"i","type":"inject","z":"t","repeat":"0.01","once":true,"onceDelay":"0.1","props":[{"p":"address","v":"7","vt":"num"},{"p":"value","v":"1","vt":"num"}],"wires":[["w"]]},
{"id":"w","type":"tw-writer","z":"t","wires":[]}]`)
	time.Sleep(3 * time.Second)
	s := snapOf(r, "w")
	t.Logf("TIMER queued=%d high=%d written=%d merged=%d", s.QueueLen, s.QueueHigh, len(w.values()), s.Coalesced)
	if s.Coalesced == 0 || s.QueueLen > 10 {
		t.Fatalf("queued %d, merged %d: an inject-rooted writer should merge", s.QueueLen, s.Coalesced)
	}
}

// Every press of a button is a command. A writer an operator can reach delivers
// every message, in order, however slow the device is.
func TestAWriterAnOperatorCanReachDeliversEveryMessageInOrder(t *testing.T) {
	r, w := startTW(t, `[{"id":"t","type":"tab","label":"t"},
{"id":"op","type":"tw-operator","z":"t","wires":[["w"]]},
{"id":"w","type":"tw-writer","z":"t","wires":[]}]`)
	burst(t, r, 30)
	deadline := time.Now().Add(10 * time.Second)
	for len(w.values()) < 30 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	got := w.values()
	if len(got) != 30 {
		t.Fatalf("%d of 30 writes arrived, merged %d", len(got), snapOf(r, "w").Coalesced)
	}
	for i, v := range got {
		if v != float64(i+1) {
			t.Fatalf("write %d was %v, order broken: %v", i, v, got)
		}
	}
}

// A writer reachable from an inject and from an operator's control counts as
// the operator's, and never merges.
func TestAWriterReachableFromBothKindsNeverMerges(t *testing.T) {
	r, w := startTW(t, `[{"id":"t","type":"tab","label":"t"},
{"id":"i","type":"inject","z":"t","repeat":"5","props":[{"p":"address","v":"7","vt":"num"}],"wires":[["w"]]},
{"id":"op","type":"tw-operator","z":"t","wires":[["w"]]},
{"id":"w","type":"tw-writer","z":"t","wires":[]}]`)
	burst(t, r, 30)
	deadline := time.Now().Add(10 * time.Second)
	for len(w.values()) < 30 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(w.values()); n < 30 || snapOf(r, "w").Coalesced != 0 {
		t.Fatalf("%d of 30 writes arrived, merged %d", n, snapOf(r, "w").Coalesced)
	}
}

// The real Modbus write node, behind a real slow device, driven by an inject.
// With a small inbox and a short block limit, the writer used to drop what it
// could not take; now the newest value replaces the old and nothing drops.
func TestAnInjectRootedModbusWriteStopsDroppingAgainstASlowDevice(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("holds a device for 6 s")
	}
	port := freeLoopPort(t)
	doc := fmt.Sprintf(`[
{"id":"tab","type":"tab","label":"t"},
{"id":"dev","type":"modbus-server","z":"tab","hostname":"127.0.0.1","serverPort":"%d","responseDelay":"40","delayUnit":"ms","wires":[[],[],[],[],[]]},
{"id":"cl","type":"modbus-client","name":"plc","clienttype":"tcp","tcpHost":"127.0.0.1","tcpPort":"%d","tcpType":"DEFAULT","unit_id":1,"clientTimeout":3000},
{"id":"poll","type":"inject","z":"tab","repeat":"0.01","once":true,"onceDelay":"1","props":[{"p":"payload","v":"{\"value\":5,\"fc\":6,\"unitid\":1,\"address\":10,\"quantity\":1}","vt":"json"}],"wires":[["w"]]},
{"id":"w","type":"modbus-flex-write","z":"tab","server":"cl","wires":[[],[]]}
]`, port, port)
	flows, err := engine.ParseFlows([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	r := rt.New(node.Default, flows, rt.Options{InboxCapacity: 16, BlockTimeout: 500 * time.Millisecond})
	go func() {
		for range r.Events() {
		}
	}()
	if fails := r.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("start: %v", fails[0])
	}
	defer r.Stop(context.Background())
	time.Sleep(6 * time.Second)
	var s rt.Snapshot
	for _, x := range r.Snapshots() {
		if x.NodeID == "w" {
			s = x
		}
	}
	t.Logf("WRITE queued=%d dropped=%d written=%d merged=%d blocked=%d", s.QueueLen, s.Dropped, s.Received, s.Coalesced, s.Blocked)
	if s.Dropped != 0 || s.Received < 20 {
		t.Fatalf("dropped %d, wrote %d", s.Dropped, s.Received)
	}
}
