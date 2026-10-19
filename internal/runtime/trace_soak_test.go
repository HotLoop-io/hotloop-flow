package runtime_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes" // the real palette
	rt "github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

const soakFlow = `[
  {"id":"t1","type":"tab","label":"Soak"},
  {"id":"src","type":"inject","z":"t1","name":"src","x":1,"y":1,"wires":[["c1"]]},
  {"id":"c1","type":"change","z":"t1","name":"c1","rules":[{"t":"set","p":"stage","pt":"msg","to":"one","tot":"str"}],"x":2,"y":1,"wires":[["sw"]]},
  {"id":"sw","type":"switch","z":"t1","name":"sw","property":"payload.n","propertyType":"msg","rules":[{"t":"gte","v":"0","vt":"num"},{"t":"else"}],"checkall":"false","outputs":2,"x":3,"y":1,"wires":[["c2"],[]]},
  {"id":"c2","type":"change","z":"t1","name":"c2","rules":[{"t":"set","p":"stage","pt":"msg","to":"two","tot":"str"}],"x":4,"y":1,"wires":[["dbg"]]},
  {"id":"dbg","type":"debug","z":"t1","name":"dbg","tosidebar":false,"x":5,"y":1,"wires":[]}
]`

type soakPoint struct {
	sent      int
	heapAlloc uint64
	heapInuse uint64
	kept      int
	tracerB   int64
}

// soak pushes n messages through five nodes and samples the heap, after a GC,
// ten times along the way. It returns the samples and how long it took.
func soak(t *testing.T, n int, tracer *engine.Tracer) ([]soakPoint, time.Duration) {
	t.Helper()
	flows, err := engine.ParseFlows([]byte(soakFlow))
	if err != nil {
		t.Fatal(err)
	}
	r := rt.New(node.Default, flows, rt.Options{})
	r.SetTracer(tracer)
	go func() {
		for range r.Events() {
		}
	}()
	if fails := r.Start(context.Background()); len(fails) > 0 {
		t.Fatal(fails)
	}
	defer r.Stop(context.Background())

	text := strings.Repeat("a reading from line three ", 4)
	var points []soakPoint
	began := time.Now()
	step := n / 10
	for i := 1; i <= n; i++ {
		m := engine.NewMsgWithPayload(map[string]any{"n": float64(i), "text": text, "unit": "degC"})
		m.SetTopic("line3/temp")
		if err := r.SendFrom("src", 0, m); err != nil {
			t.Fatal(err)
		}
		if i%step == 0 {
			for !r.Settled() {
				time.Sleep(time.Millisecond)
			}
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			p := soakPoint{sent: i, heapAlloc: ms.HeapAlloc, heapInuse: ms.HeapInuse}
			if tracer != nil {
				st := tracer.Stats()
				p.kept, p.tracerB = st.Kept, st.Bytes
			}
			points = append(points, p)
		}
	}
	for !r.Settled() {
		time.Sleep(time.Millisecond)
	}
	took := time.Since(began)

	for _, s := range r.Snapshots() {
		if s.NodeID == "dbg" && s.Received != int64(n) {
			t.Fatalf("the Debug node got %d of %d messages", s.Received, n)
		}
	}
	return points, took
}

// TestTraceMemoryStaysFlatUnderASoak is the claim the README makes: tracing
// every message through five nodes keeps the same memory at the millionth
// message as at the hundred-thousandth. It runs a small soak by default so the
// suite stays quick, and the full million when HOTLOOP_FLOW_SOAK_MESSAGES says
// so, which CI does in a step of its own and the release checklist does by hand.
func TestTraceMemoryStaysFlatUnderASoak(t *testing.T) {
	n := 50000
	if v := os.Getenv("HOTLOOP_FLOW_SOAK_MESSAGES"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 10 {
			t.Fatalf("HOTLOOP_FLOW_SOAK_MESSAGES=%q", v)
		}
	}

	untraced, untracedTook := soak(t, n, nil)
	tracer := engine.NewTracer(engine.TraceLimits{})
	traced, tracedTook := soak(t, n, tracer)
	// What an instance really runs with: the same limits, and a ceiling on new
	// traces a second, which is what a source this fast runs into.
	defTracer := engine.NewTracer(engine.DefaultTraceLimits())
	defPoints, defTook := soak(t, n, defTracer)

	lim := tracer.Limits()
	st := tracer.Stats()
	ds := defTracer.Stats()
	var report strings.Builder
	fmt.Fprintf(&report, "messages: %d through 5 nodes (5 hops each, the source counting as one)\n", n)
	fmt.Fprintf(&report, "limits: %d traces, %d hops each, %d bytes per message, %d bytes in all\n",
		lim.Traces, lim.MaxHops, lim.MaxMessageBytes, lim.MaxBytes)
	fmt.Fprintf(&report, "%-10s %-16s %-16s %-14s %-8s %-12s\n", "sent", "heap (every msg)", "heap (default)", "heap (off)", "kept", "tracer bytes")
	for i, p := range traced {
		fmt.Fprintf(&report, "%-10d %-16s %-16s %-14s %-8d %-12d\n", p.sent, mib(p.heapAlloc), mib(defPoints[i].heapAlloc), mib(untraced[i].heapAlloc), p.kept, p.tracerB)
	}
	rate := func(d time.Duration) float64 { return float64(n) / d.Seconds() }
	fmt.Fprintf(&report, "speed: %.0f messages a second with tracing off, %.0f tracing every message (%s), %.0f with the default %d traces a second limit (%s)\n",
		rate(untracedTook), rate(tracedTook), tracedTook.Round(time.Millisecond), rate(defTook), engine.DefaultTraceLimits().PerSecond, defTook.Round(time.Millisecond))
	fmt.Fprintf(&report, "tracing every message: %d started, %d forgotten, %d hops dropped\n", st.Started, st.Evicted, st.HopsDropped)
	fmt.Fprintf(&report, "default limits: %d started, %d skipped over the per-second limit\n", ds.Started, ds.Skipped)
	t.Log("\n" + report.String())
	if out := os.Getenv("HOTLOOP_FLOW_SOAK_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if st.Started != int64(n) {
		t.Errorf("the tracer started %d traces for %d messages", st.Started, n)
	}
	if st.Kept > lim.Traces || st.Bytes > lim.MaxBytes {
		t.Errorf("the tracer holds %d traces and %d bytes, over its limits of %d and %d", st.Kept, st.Bytes, lim.Traces, lim.MaxBytes)
	}
	if st.Evicted != st.Started-int64(st.Kept) {
		t.Errorf("%d started, %d kept, %d forgotten: they don't add up", st.Started, st.Kept, st.Evicted)
	}
	if ds.Started+ds.Skipped != int64(n) || ds.Skipped == 0 {
		t.Errorf("with the default limit %d started and %d skipped for %d messages", ds.Started, ds.Skipped, n)
	}
	// Flat means the last sample is no bigger than the first, give or take
	// what the allocator does. The first is taken a tenth of the way in, long
	// after the tracer filled, so it already holds all it ever will.
	for name, pts := range map[string][]soakPoint{"every message": traced, "default limits": defPoints} {
		first, last := pts[0].heapAlloc, pts[len(pts)-1].heapAlloc
		if last > first+first/2+8<<20 {
			t.Errorf("tracing with %s: the heap grew from %s to %s over the soak", name, mib(first), mib(last))
		}
	}
}

func mib(b uint64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
