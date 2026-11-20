package runtime_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	rt "github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// soakRun holds the plant-shaped flow for a long time and returns one sample
// per window: what the process cost for every thousand messages in that window,
// and the goroutines and live heap at its end. keep is how long a chart holds
// its points, "3600" being the plant's hour.
type soakSample struct {
	At         time.Duration
	Msgs       float64
	CPUPer1k   float64
	Cores      float64
	Goroutines int
	HeapMB     float64
	GCPerS     float64
}

func soakRun(t *testing.T, scale float64, keepSeconds string, windows int, window time.Duration, profileAt int) []soakSample {
	t.Helper()
	raw, _ := loadFlow(scale)
	if n, _ := strconv.Atoi(os.Getenv("HOTLOOP_FLOW_SOAK_SLOW")); n > 0 {
		raw = addSlowChains(raw, n)
	}
	raw = bytes.ReplaceAll(raw, []byte(`"removeOlder":1,"removeOlderPoints":"","removeOlderUnit":"3600"`),
		[]byte(`"removeOlder":1,"removeOlderPoints":"","removeOlderUnit":"`+keepSeconds+`"`))
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
		t.Fatalf("%d nodes failed to start, first: %v", len(fails), fails[0])
	}
	defer r.Stop(context.Background())
	began := time.Now()
	time.Sleep(8 * time.Second)

	var out []soakSample
	for i := 0; i < windows; i++ {
		var prof bytes.Buffer
		profile := (i == profileAt || i == 2) && os.Getenv("HOTLOOP_FLOW_SOAK_PROFILE") != ""
		if profile {
			if err := pprof.StartCPUProfile(&prof); err != nil {
				t.Fatal(err)
			}
		}
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		r0, _ := total(r)
		c0, w0 := cpuNow(), time.Now()
		time.Sleep(window)
		r1, _ := total(r)
		c1, w1 := cpuNow(), time.Now()
		runtime.ReadMemStats(&m1)
		if profile {
			pprof.StopCPUProfile()
			_ = os.WriteFile(os.Getenv("HOTLOOP_FLOW_SOAK_PROFILE")+"."+time.Since(began).Round(time.Second).String(), prof.Bytes(), 0o644)
		}
		wall := w1.Sub(w0).Seconds()
		msgs := float64(r1 - r0)
		s := soakSample{
			At: time.Since(began).Round(time.Second), Msgs: msgs / wall,
			CPUPer1k: (c1 - c0).Seconds() / msgs * 1e6, Cores: (c1 - c0).Seconds() / wall,
			Goroutines: runtime.NumGoroutine(), HeapMB: float64(m1.HeapAlloc) / 1e6,
			GCPerS: float64(m1.NumGC-m0.NumGC) / wall,
		}
		qd, full := queued(r)
		t.Logf("SOAK queued=%d full=%d at=%s msgs/s=%.0f cpu_cores=%.3f cpu_ms_per_1k=%.1f goroutines=%d heap_MB=%.0f gc_per_s=%.2f",
			qd, full, s.At, s.Msgs, s.Cores, s.CPUPer1k, s.Goroutines, s.HeapMB, s.GCPerS)
		out = append(out, s)
	}
	return out
}

// TestSoakPlantShapedFlow is the long run: HOTLOOP_FLOW_SOAK=<duration> holds
// the load flow that long and logs a window a minute. It is skipped unless that
// is set, because the plant's cost grew with uptime and a 20 s window cannot see it.
func TestSoakPlantShapedFlow(t *testing.T) {
	d := os.Getenv("HOTLOOP_FLOW_SOAK")
	if d == "" || raceDetector {
		t.Skip("set HOTLOOP_FLOW_SOAK=<duration> to run")
	}
	total, err := time.ParseDuration(d)
	if err != nil {
		t.Fatal(err)
	}
	keep := os.Getenv("HOTLOOP_FLOW_SOAK_KEEP")
	if keep == "" {
		keep = "3600"
	}
	window := envDur("HOTLOOP_FLOW_SOAK_WINDOW", time.Minute)
	n := int(total / window)
	soakRun(t, 1, keep, n, window, n-2)
}

// slowSink stands in for a device that answers slower than it is asked: a
// Modbus read against a dryer that is off takes seconds, and the inject in
// front of it keeps asking.
type slowSink struct{ d time.Duration }

func (s slowSink) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	select {
	case <-time.After(s.d):
	case <-ctx.Done():
	}
	return nil
}

func init() {
	err := node.Default.Register(node.Descriptor{
		Type: "soak-slow-sink", Category: node.CategoryCommon, Color: "#E31837", Icon: "cog",
		Inputs: 1, Outputs: 0, Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(def *node.Definition) (node.Node, error) { return slowSink{d: 2 * time.Second}, nil })
	if err != nil {
		panic(err)
	}
}

// addSlowChains appends n inject -> function -> slow sink chains to a flow.
func addSlowChains(raw []byte, n int) []byte {
	var b bytes.Buffer
	b.Write(bytes.TrimSuffix(raw, []byte("]")))
	b.WriteString(`,{"id":"slowtab","type":"tab","label":"slow"}`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `,{"id":"si%d","type":"inject","z":"slowtab","name":"ask","repeat":"0.75","once":true,"onceDelay":"1","props":[{"p":"payload","v":"[1,2]","vt":"json"}],"wires":[["sf%d"]]}`, i, i)
		fmt.Fprintf(&b, `,{"id":"sf%d","type":"function","z":"slowtab","name":"p","func":"msg.reg = 1;\nreturn msg;","outputs":1,"wires":[["ss%d"]]}`, i, i)
		fmt.Fprintf(&b, `,{"id":"ss%d","type":"soak-slow-sink","z":"slowtab","wires":[]}`, i)
	}
	b.WriteString("]")
	return b.Bytes()
}

// queued is how many messages sit in inboxes, and how many inboxes are full.
func queued(r *rt.Runtime) (msgs, full int) {
	for _, s := range r.Snapshots() {
		msgs += int(s.QueueLen)
		if s.QueueLen >= s.QueueCap && s.QueueCap > 0 {
			full++
		}
	}
	return
}
