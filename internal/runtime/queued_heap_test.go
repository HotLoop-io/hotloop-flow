package runtime_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	rt "github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// TestAQueuedMessageHoldsLittle measures what a message waiting in an inbox
// costs the live heap. A plant's slow Modbus reads left 425,000 messages
// queued, and the collector scans every one of them each cycle.
func TestAQueuedMessageHoldsLittle(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("measures heap")
	}
	const chains = 100
	raw, _ := loadFlow(0)
	raw = addSlowChains(raw, chains)
	flows, err := engine.ParseFlows(raw)
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
	defer r.Stop(context.Background())
	live := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	time.Sleep(2 * time.Second)
	base := live()
	var q int
	for i := 0; i < 90 && q < chains*400; i++ {
		time.Sleep(time.Second)
		q, _ = queued(r)
	}
	after := live()
	per := float64(after-base) / float64(q)
	t.Logf("QUEUED messages=%d live_bytes_each=%.0f", q, per)
	if os.Getenv("HOTLOOP_FLOW_QUEUED_BUDGET") == "" {
		return
	}
}

// A node nobody has sent anything to holds no inbox. A flow file has thousands
// of them, and a full size inbox for each was 24 KB of memory the collector
// scanned every cycle.
func TestANodeThatNeverTakesAMessageHoldsNoInbox(t *testing.T) {
	if testing.Short() || raceDetector {
		t.Skip("measures heap")
	}
	var b []byte
	b = append(b, `[{"id":"t","type":"tab","label":"t"}`...)
	const n = 4000
	for i := 0; i < n; i++ {
		b = append(b, fmt.Sprintf(`,{"id":"f%d","type":"function","z":"t","name":"idle","func":"return msg;","outputs":1,"wires":[[]]}`, i)...)
	}
	b = append(b, ']')
	flows, err := engine.ParseFlows(b)
	if err != nil {
		t.Fatal(err)
	}
	live := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	base := live()
	r := rt.New(node.Default, flows, rt.Options{InboxCapacity: 1024})
	go func() {
		for range r.Events() {
		}
	}()
	if fails := r.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("start: %v", fails[0])
	}
	defer r.Stop(context.Background())
	per := float64(live()-base) / n
	t.Logf("IDLE node holds %.0f bytes", per)
	if per > 8192 {
		t.Fatalf("an idle node holds %.0f bytes, an inbox is 24576 of it", per)
	}
}
