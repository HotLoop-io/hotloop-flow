package runtime_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	_ "github.com/HotLoop-io/hotloop-flow/internal/nodes" // the real palette
	rt "github.com/HotLoop-io/hotloop-flow/internal/runtime"
)

// The standing load benchmark.
//
// On 2026-11-20 a Flow instance at a plant ran 12,753 nodes, eleven cloned
// dryer tabs and a recorder tab, took about 3,000 messages a second with nobody
// looking at a dashboard or the debug sidebar, and sat at its two CPU limit.
// This builds a flow of that shape, with no I/O in it, runs it for a while the
// same way (no browser connected, the dashboard hub mounted, debug nodes that
// send to the sidebar), and reports the CPU the process spent for every
// thousand messages it handled. The test fails over a budget so the cost of a
// message, and the cost of just holding that many nodes, cannot creep back.
//
// What stands in for the plant: the Modbus getters are inject nodes that send
// a two-register array, and the PostgreSQL recorders are debug nodes. Everything
// else is the type the plant runs, and the counts follow the plant's.

const (
	loadTabs        = 11   // cloned dryer tabs
	loadNodesPerTab = 1000 // nodes in each, active and idle together
	loadPipes       = 25   // active pipelines in each tab
	loadRecorder    = 1750 // nodes in the recorder tab
	loadRecPipes    = 40
)

// loadFlow returns the flow JSON and the number of nodes in it.
func loadFlow(scale float64) ([]byte, int) {
	var b bytes.Buffer
	n := 0
	add := func(s string, a ...any) {
		if n > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, s, a...)
		n++
	}
	b.WriteString("[")
	add(`{"id":"ub","type":"ui_base","name":"load","path":"/ui"}`)
	add(`{"id":"ut","type":"ui_tab","name":"Load","order":1}`)
	add(`{"id":"ug","type":"ui_group","name":"G","tab":"ut","order":1,"width":"6","disp":true}`)
	pipes := int(float64(loadPipes) * scale)
	recPipes := int(float64(loadRecPipes) * scale)
	tab := func(id string, total, p int, ui bool) {
		add(`{"id":"%s","type":"tab","label":"%s"}`, id, id)
		used := 0
		for i := 0; i < p; i++ {
			k := fmt.Sprintf("%s-%d", id, i)
			// source: a Modbus read, a two-register array every 0.75 s
			add(`{"id":"%s-i","type":"inject","z":"%s","name":"read","repeat":"0.75","once":true,"onceDelay":"%d","props":[{"p":"payload","v":"[%d,17116]","vt":"json"},{"p":"topic","v":"r%d","vt":"str"}],"wires":[["%s-f1"]]}`, k, id, 1+i%5, i, i, k)
			add(`{"id":"%s-f1","type":"function","z":"%s","name":"scale","func":"var v = msg.payload[0] * 0.1 + msg.payload[1];\nmsg.payload = v;\nmsg.reg = %d;\nreturn msg;","outputs":1,"wires":[["%s-c1"]]}`, k, id, i, k)
			add(`{"id":"%s-c1","type":"change","z":"%s","name":"tag","rules":[{"t":"set","p":"tag","pt":"msg","to":"dryer.%s.r%d","tot":"str"}],"wires":[["%s-d","%s-f2","%s-n","%s-ch"]]}`, k, id, id, i, k, k, k, k)
			add(`{"id":"%s-d","type":"debug","z":"%s","name":"","active":true,"tosidebar":true,"complete":"payload","wires":[]}`, k, id)
			add(`{"id":"%s-f2","type":"function","z":"%s","name":"on","func":"msg.payload = msg.payload > 50;\nreturn msg;","outputs":1,"wires":[["%s-l"]]}`, k, id, k)
			used += 6
			if ui {
				add(`{"id":"%s-n","type":"ui_numeric","z":"%s","group":"ug","order":%d,"label":"v","format":"{{value}}","min":0,"max":"3000","step":1,"passthru":false,"topic":"topic","topicType":"msg","wires":[[]]}`, k, id, i)
				add(`{"id":"%s-l","type":"ui_led","z":"%s","group":"ug","order":%d,"label":"s","labelPlacement":"left","labelAlignment":"left","colorForValue":[{"color":"#4f4a4a","value":"false","valueType":"bool"},{"color":"#eb0000","value":"true","valueType":"bool"}],"shape":"circle","wires":[]}`, k, id, i)
				add(`{"id":"%s-ch","type":"ui_chart","z":"%s","group":"ug","order":%d,"label":"c","chartType":"line","removeOlder":1,"removeOlderPoints":"","removeOlderUnit":"3600","wires":[[]]}`, k, id, i)
			} else {
				add(`{"id":"%s-n","type":"debug","z":"%s","name":"","active":true,"tosidebar":true,"complete":"payload","wires":[]}`, k, id)
				add(`{"id":"%s-l","type":"debug","z":"%s","name":"","active":true,"tosidebar":true,"complete":"payload","wires":[]}`, k, id)
				add(`{"id":"%s-ch","type":"debug","z":"%s","name":"","active":true,"tosidebar":true,"complete":"payload","wires":[]}`, k, id)
			}
			used += 3
		}
		// the rest of the tab is idle: nodes that wait for an operator, a
		// comment, and logic that only runs on a press
		for j := 0; used < int(float64(total)*scale); j++ {
			k := fmt.Sprintf("%s-idle-%d", id, j)
			switch j % 5 {
			case 0:
				add(`{"id":"%s","type":"function","z":"%s","name":"idle","func":"return msg;","outputs":1,"wires":[[]]}`, k, id)
			case 1:
				add(`{"id":"%s","type":"change","z":"%s","name":"idle","rules":[{"t":"set","p":"payload","pt":"msg","to":"1","tot":"num"}],"wires":[[]]}`, k, id)
			case 2:
				add(`{"id":"%s","type":"comment","z":"%s","name":"note"}`, k, id)
			case 3:
				if ui {
					add(`{"id":"%s","type":"ui_button","z":"%s","group":"ug","order":%d,"label":"b","passthru":false,"payload":"1","payloadType":"num","topic":"topic","wires":[[]]}`, k, id, j)
				} else {
					add(`{"id":"%s","type":"comment","z":"%s","name":"note"}`, k, id)
				}
			default:
				if ui {
					add(`{"id":"%s","type":"ui_switch","z":"%s","group":"ug","order":%d,"label":"s","passthru":false,"decouple":"false","topic":"topic","topicType":"msg","onvalue":"true","onvalueType":"bool","offvalue":"false","offvalueType":"bool","wires":[[]]}`, k, id, j)
				} else {
					add(`{"id":"%s","type":"debug","z":"%s","name":"","active":false,"complete":"payload","wires":[]}`, k, id)
				}
			}
			used++
		}
	}
	for t := 0; t < loadTabs; t++ {
		tab(fmt.Sprintf("tab%02d", t), loadNodesPerTab, pipes, true)
	}
	tab("recorder", loadRecorder, recPipes, false)
	b.WriteString("]")
	return b.Bytes(), n
}

func cpuNow() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func total(r *rt.Runtime) (recv int64, nodes int) {
	for _, s := range r.Snapshots() {
		recv += s.Received
		nodes++
	}
	return
}

// spin is a fixed amount of plain CPU work, timed to tell a slow machine from a
// slow runtime: the budget below is for the Beast, and a CI runner gets it scaled
// by how much longer the same loop takes there.
func spin() time.Duration {
	began := time.Now()
	x := uint64(88172645463325252)
	for i := 0; i < 300_000_000; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	if x == 0 {
		panic("unreachable")
	}
	return time.Since(began)
}

const (
	// spinOnTheBeast is spin() on the box the budgets were set on.
	spinOnTheBeast = 0.54 // seconds
	// Fixed: 96 CPU ms per 1,000 messages and 331 MB idle heap on the Beast.
	// Before the fixes: 294 and 795. The budgets sit between, so the old
	// behavior fails them and the fixed one has room for a busy runner.
	budgetCPUMsPer1k = 180.0
	budgetIdleHeapMB = 520.0
)

func envDur(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if x, err := time.ParseDuration(v); err == nil {
			return x
		}
	}
	return d
}

// TestLoadPlantShapedFlowStaysUnderCPUBudget is the standing check. Set
// HOTLOOP_FLOW_LOAD_PROFILE=<file> to write a CPU profile of the measured
// window, and HOTLOOP_FLOW_LOAD_SCALE to run a smaller flow.
func TestLoadPlantShapedFlowStaysUnderCPUBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("the load check holds 12,000 nodes for about 40 s")
	}
	if raceDetector {
		t.Skip("a CPU budget does not hold under the race detector; the plain run is the check")
	}
	scale := 1.0
	if v := os.Getenv("HOTLOOP_FLOW_LOAD_SCALE"); v != "" {
		scale, _ = strconv.ParseFloat(v, 64)
	}
	raw, count := loadFlow(scale)
	flows, err := engine.ParseFlows(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := rt.New(node.Default, flows, rt.Options{InboxCapacity: 1024})
	// What the process does with events when no browser is connected: take them
	// off the channel and drop them.
	go func() {
		for range r.Events() {
		}
	}()
	slow := spin().Seconds() / spinOnTheBeast
	if slow < 1 {
		slow = 1
	}
	began := time.Now()
	if fails := r.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("%d nodes failed to start, first: %v", len(fails), fails[0])
	}
	defer r.Stop(context.Background())
	started := time.Since(began)

	time.Sleep(envDur("HOTLOOP_FLOW_LOAD_WARMUP", 8*time.Second))
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	idleHeap := m0.HeapAlloc

	var prof bytes.Buffer
	if f := os.Getenv("HOTLOOP_FLOW_LOAD_PROFILE"); f != "" {
		if err := pprof.StartCPUProfile(&prof); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.WriteFile(f, prof.Bytes(), 0o644) }()
	}
	r0, _ := total(r)
	c0, w0 := cpuNow(), time.Now()
	runtime.ReadMemStats(&m0)
	time.Sleep(envDur("HOTLOOP_FLOW_LOAD_WINDOW", 20*time.Second))
	r1, nodes := total(r)
	c1, w1 := cpuNow(), time.Now()
	runtime.ReadMemStats(&m1)
	if prof.Len() > 0 || os.Getenv("HOTLOOP_FLOW_LOAD_PROFILE") != "" {
		pprof.StopCPUProfile()
	}

	wall := w1.Sub(w0).Seconds()
	msgs := float64(r1 - r0)
	cpu := (c1 - c0).Seconds()
	rate := msgs / wall
	cpuPerK := cpu / msgs * 1000 * 1000 // CPU milliseconds per 1,000 messages
	cores := cpu / wall
	gcs := float64(m1.NumGC-m0.NumGC) / wall
	t.Logf("LOAD slowness=%.2f nodes=%d start=%.1fs msgs/s=%.0f cpu_cores=%.3f cpu_ms_per_1k_msgs=%.2f gc_per_s=%.2f heap_idle_MB=%.0f goroutines=%d",
		slow, nodes, started.Seconds(), rate, cores, cpuPerK, gcs, float64(idleHeap)/1e6, runtime.NumGoroutine())
	_ = count

	if rate < 1500*scale {
		t.Fatalf("the flow ran %.0f msg/s, under the 1500 a plant-shaped flow should (the benchmark is not loading it)", rate)
	}
	if limit := budgetCPUMsPer1k * slow; cpuPerK > limit {
		t.Errorf("%.1f CPU ms per 1,000 messages is over the budget of %.1f (the machine is %.2fx the Beast)", cpuPerK, limit, slow)
	}
	if float64(idleHeap)/1e6 > budgetIdleHeapMB {
		t.Errorf("holding the flow costs %.0f MB of heap, over the budget of %.0f", float64(idleHeap)/1e6, budgetIdleHeapMB)
	}
}
