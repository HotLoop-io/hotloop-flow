package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/runtime"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// The cut-and-restore test for a durable wire to InfluxDB: the historian goes
// away, readings keep coming at a steady rate and spill to disk, the historian
// comes back, and the backlog drains while the readings are still arriving. It
// measures how long the drain takes and checks that every reading arrived, once,
// in the order it was sent.
//
// The historian is a local server that takes a fixed time to answer a write, the
// way a real one over a network does, so what is measured is how many writes the
// drain makes. HOTLOOP_FLOW_DRAIN_BACKLOG, _RATE (readings a second) and
// _LATENCY (milliseconds a write takes) scale it; the defaults are small enough
// for the suite.

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

type historian struct {
	mu       sync.Mutex
	seqs     []float64
	writes   atomic.Int64
	down     atomic.Bool
	latency  time.Duration
	maxLines atomic.Int64
}

func (h *historian) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.down.Load() {
		http.Error(w, "the historian is down", http.StatusServiceUnavailable)
		return
	}
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			break
		}
	}
	time.Sleep(h.latency)
	lines := strings.Split(strings.TrimSpace(string(buf)), "\n")
	h.mu.Lock()
	for _, l := range lines {
		parts := strings.Fields(l)
		if len(parts) < 2 {
			continue
		}
		_, v, _ := strings.Cut(parts[1], "=")
		f, err := strconv.ParseFloat(strings.TrimSuffix(v, "i"), 64)
		if err == nil {
			h.seqs = append(h.seqs, f)
		}
	}
	h.mu.Unlock()
	h.writes.Add(1)
	if n := int64(len(lines)); n > h.maxLines.Load() {
		h.maxLines.Store(n)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *historian) got() []float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]float64(nil), h.seqs...)
}

func TestDurableWireDrainsABacklogFastAndInOrder(t *testing.T) {
	backlog := envInt("HOTLOOP_FLOW_DRAIN_BACKLOG", 600)
	rate := envInt("HOTLOOP_FLOW_DRAIN_RATE", 100)
	h := &historian{latency: time.Duration(envInt("HOTLOOP_FLOW_DRAIN_LATENCY", 8)) * time.Millisecond}
	srv := httptest.NewServer(h)
	defer srv.Close()

	flows, err := engine.ParseFlows([]byte(fmt.Sprintf(`[
	  {"id":"t1","type":"tab","label":"T"},
	  {"id":"cfg","type":"hotloop-flow-influxdb","url":%q,"apiVersion":"1","database":"plant"},
	  {"id":"a","type":"junction","z":"t1","x":1,"y":1,"ew_durable":["w"],"wires":[["w"]]},
	  {"id":"w","type":"influxdb out","z":"t1","x":2,"y":1,"server":"cfg","measurement":"readings","measurementType":"str",
	   "fields":[{"column":"seq","value":"payload","valueType":"msg"}],"wires":[[]]}
	]`, srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "state.db"), store.SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sp, err := store.OpenSpool(db, store.SpoolOptions{MaxBytes: 64 << 20, AckFlush: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	rt := runtime.New(node.Default, flows, runtime.Options{})
	rt.SetSpool(sp)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("start: %v", fails)
	}
	defer rt.Stop(context.Background())
	wire := sp.Wire("a:0>w")

	var sent atomic.Int64
	send := func() {
		n := sent.Add(1) - 1
		if err := rt.Inject("a", engine.NewMsgWithPayload(float64(n))); err != nil {
			t.Error(err)
		}
	}
	wait := func(what string, limit time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (sent %d, historian has %d, queued %d)", what, sent.Load(), len(h.got()), wire.Depth())
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Healthy for a moment.
	for i := 0; i < 20; i++ {
		send()
	}
	wait("the first readings to land", 10*time.Second, func() bool { return len(h.got()) == 20 })

	// Cut, and let a backlog build.
	h.down.Store(true)
	for i := 0; i < backlog; i++ {
		send()
	}
	wait("the backlog to be on disk", 30*time.Second, func() bool { return wire.Depth() >= int64(backlog) })

	// Restore, with the plant still sending.
	stop := make(chan struct{})
	var feeder sync.WaitGroup
	feeder.Add(1)
	go func() {
		defer feeder.Done()
		tick := time.NewTicker(time.Second / time.Duration(rate))
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				send()
			case <-stop:
				return
			}
		}
	}()
	writesBefore := h.writes.Load()
	h.down.Store(false)
	start := time.Now()
	wait("the queue to drain", 4*time.Minute, func() bool { return wire.Depth() == 0 })
	took := time.Since(start)
	close(stop)
	feeder.Wait()
	total := int(sent.Load())
	wait("every reading to land", 30*time.Second, func() bool { return len(h.got()) >= total })

	got := h.got()
	if len(got) != total {
		t.Fatalf("the historian has %d readings, %d were sent", len(got), total)
	}
	for i, v := range got {
		if v != float64(i) {
			t.Fatalf("reading %d is %v: out of order, lost or doubled", i, v)
		}
	}
	t.Logf("DRAIN backlog=%d rate=%d/s latency=%v: drained in %v, %d writes (largest %d readings), %d readings in all, in order, none lost",
		backlog, rate, h.latency, took.Round(time.Millisecond), h.writes.Load()-writesBefore, h.maxLines.Load(), total)
}
