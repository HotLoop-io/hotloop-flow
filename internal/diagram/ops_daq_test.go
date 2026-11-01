package diagram

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/ni/nitest"
)

// These tests run the dia-daq-read node against nitest's stand-in server, which
// we wrote and which answers with a sine wave. They have not run against NI's
// server.

func daqProps(s *nitest.StandInServer, extra map[string]any) map[string]any {
	p := map[string]any{"address": s.Addr, "device": "cDAQ1Mod1", "channels": "ai0:1", "rate": 1000.0, "samples": 1000.0, "callTimeoutSec": 5.0}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func daqFlow(t *testing.T, props map[string]any) *flowBuilder {
	return newFlow(t).tab("main", nil).
		node("main", "daq", "dia-daq-read", props).
		node("main", "y", "dia-indicator", nil).
		node("main", "dt", "dia-indicator", nil).
		node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
		wire("daq.y", "y.value").wire("daq.dt", "dt.value").wire("daq.err", "err.value")
}

// I4 and I6 through the node: 1,000 samples at 1 kS/s on two channels, from the
// stand-in, spanning 1.000 s.
func TestDAQNodeReadsThousandSamplesFromTheStandIn(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	b := daqFlow(t, daqProps(s, nil))
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	y, _ := lastOn(t, r, "main/daq.y>y.value").([]any)
	if len(y) != 2 {
		t.Fatalf("y has %d channels, want 2: %v", len(y), y)
	}
	dt, _ := lastOn(t, r, "main/daq.dt>dt.value").(float64)
	for c := range y {
		ch, _ := y[c].([]any)
		if len(ch) != 1000 {
			t.Fatalf("channel %d has %d samples", c, len(ch))
		}
		for i, v := range ch {
			if v.(float64) != nitest.Value(c, int64(i), 1000) {
				t.Fatalf("channel %d sample %d is %v", c, i, v)
			}
		}
		if span := float64(len(ch)) * dt; math.Abs(span-1.000) > 1e-9 {
			t.Fatalf("channel %d spans %.9f s, want 1.000 s", c, span)
		}
	}
	if s.Tasks() != 0 {
		t.Fatalf("the run ended with %d tasks still on the server", s.Tasks())
	}
	calls := s.Calls()
	if calls[len(calls)-1] != "ClearTask" {
		t.Fatalf("the last call was %s, want ClearTask: %v", calls[len(calls)-1], calls)
	}
}

// One channel comes out as a plain array, not an array of one.
func TestDAQNodeOneChannelIsAPlainArray(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	r, err := daqFlow(t, daqProps(s, map[string]any{"channels": "ai1", "samples": 50.0})).run("main")
	if err != nil {
		t.Fatal(err)
	}
	y, _ := lastOn(t, r, "main/daq.y>y.value").([]any)
	if len(y) != 50 {
		t.Fatalf("y is %d long", len(y))
	}
	if _, nested := y[0].(float64); !nested {
		t.Fatalf("y[0] is %T, want a number", y[0])
	}
	if y[7].(float64) != nitest.Value(0, 7, 1000) {
		t.Fatalf("y[7] is %v", y[7])
	}
}

// A refusal from the driver is on the error wire with NI's code and text.
func TestDAQNodeDeviceRefusalIsOnTheErrorWire(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	r, err := daqFlow(t, daqProps(s, map[string]any{"device": "cDAQ9Mod9"})).run("main")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := lastOn(t, r, "main/daq.err>err.value").(map[string]any)
	if !ok || e["status"] != true || e["code"] != float64(CodeDevice) ||
		!strings.Contains(fmt.Sprint(e["source"]), "-200220") || !strings.Contains(fmt.Sprint(e["source"]), "not in this chassis") {
		t.Fatalf("error out is %v", lastOn(t, r, "main/daq.err>err.value"))
	}
	if s.Tasks() != 0 {
		t.Fatalf("a task that never got its channels was left on the server")
	}
}

// A server that never answers is a timeout on the error wire, not a hung run.
func TestDAQNodeTimeoutIsOnTheErrorWire(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	s.Hang(true)
	start := time.Now()
	r, err := daqFlow(t, daqProps(s, map[string]any{"callTimeoutSec": 0.3})).run("main")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := lastOn(t, r, "main/daq.err>err.value").(map[string]any)
	if !ok || e["code"] != float64(CodeTimeout) {
		t.Fatalf("error out is %v", lastOn(t, r, "main/daq.err>err.value"))
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("a 0.3 s timeout took %v", took)
	}
}

// A for loop reads a block of a continuous task on each pass: one task for the
// run, the blocks run on from each other, and the task is cleared at the end.
func TestDAQNodeContinuousReadsRunOnAcrossLoopPasses(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	b := newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
		node("main", "three", "dia-const", map[string]any{"value": "3"}).
		node("main", "loop", "dia-for", map[string]any{"body": "body"}).
		node("main", "blocks", "dia-indicator", nil).
		wire("three.value", "loop.n").wire("loop.collected", "blocks.value").
		node("body", "daq", "dia-daq-read", daqProps(s, map[string]any{"channels": "ai0", "mode": "continuous", "samples": 10.0, "rate": 100.0})).
		node("body", "out", "dia-body-out", map[string]any{"name": "collect"}).
		wire("daq.y", "out.value")
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	blocks, _ := lastOn(t, r, "main/loop.collected>blocks.value").([]any)
	if len(blocks) != 3 {
		t.Fatalf("%d blocks, want 3: %v", len(blocks), blocks)
	}
	for k, blk := range blocks {
		for i, v := range blk.([]any) {
			if want := nitest.Value(0, int64(k*10+i), 100); v.(float64) != want {
				t.Fatalf("block %d sample %d is %v, want %v", k, i, v, want)
			}
		}
	}
	creates := 0
	for _, c := range s.Calls() {
		if c == "CreateTask" {
			creates++
		}
	}
	if creates != 1 || s.Tasks() != 0 {
		t.Fatalf("%d tasks made, %d left: %v", creates, s.Tasks(), s.Calls())
	}
}

// A finite acquisition starts again on each pass, so each block is a fresh one.
func TestDAQNodeFiniteAcquiresAgainOnEachPass(t *testing.T) {
	s := nitest.NewStandIn(t, nitest.Options{})
	b := newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
		node("main", "two", "dia-const", map[string]any{"value": "2"}).
		node("main", "loop", "dia-for", map[string]any{"body": "body"}).
		node("main", "blocks", "dia-indicator", nil).
		wire("two.value", "loop.n").wire("loop.collected", "blocks.value").
		node("body", "daq", "dia-daq-read", daqProps(s, map[string]any{"channels": "ai0", "samples": 5.0, "rate": 100.0})).
		node("body", "out", "dia-body-out", map[string]any{"name": "collect"}).
		wire("daq.y", "out.value")
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := lastOn(t, r, "main/loop.collected>blocks.value").([]any)
	if len(blocks) != 2 || fmt.Sprint(blocks[0]) != fmt.Sprint(blocks[1]) {
		t.Fatalf("two finite acquisitions should match: %v", blocks)
	}
}

func TestDAQNodeThroughTLSAndMutualTLS(t *testing.T) {
	pki := nitest.NewPKI(t)
	sc, sk := pki.Issue(t, "server", true)
	cc, ck := pki.Issue(t, "client", false)
	run := func(t *testing.T, s *nitest.StandInServer, extra map[string]any) map[string]any {
		r, err := daqFlow(t, daqProps(s, extra)).run("main")
		if err != nil {
			t.Fatal(err)
		}
		e, _ := lastOn(t, r, "main/daq.err>err.value").(map[string]any)
		return e
	}
	t.Run("server side", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk})
		if e := run(t, s, map[string]any{"tls": "on", "caFile": pki.CAFile, "samples": 10.0}); e["status"] == true {
			t.Fatalf("error %v", e)
		}
	})
	t.Run("mutual", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk, ClientCA: pki.CAFile})
		if e := run(t, s, map[string]any{"tls": "mutual", "caFile": pki.CAFile, "certFile": cc, "keyFile": ck, "samples": 10.0}); e["status"] == true {
			t.Fatalf("error %v", e)
		}
		if s.PeerCerts() == 0 {
			t.Fatal("the stand-in never saw the node's certificate")
		}
	})
	t.Run("plain node against a secured server is an error on the wire", func(t *testing.T) {
		s := nitest.NewStandIn(t, nitest.Options{ServerCert: sc, ServerKey: sk})
		e := run(t, s, map[string]any{"callTimeoutSec": 2.0})
		if e["status"] != true || e["code"] != float64(CodeDevice) {
			t.Fatalf("error out is %v", e)
		}
		if len(s.Calls()) != 0 {
			t.Fatalf("the stand-in ran %v for a plain client", s.Calls())
		}
	})
}

// A bad setting is in the diagram's error list, not a failure mid run.
func TestDAQNodeBadSettingsAreProblems(t *testing.T) {
	for name, props := range map[string]map[string]any{
		"no channels":          {"channels": ""},
		"zero rate":            {"rate": 0.0},
		"mutual needs cert":    {"tls": "mutual"},
		"backwards range":      {"min": 5.0, "max": -5.0},
		"unknown wiring":       {"terminal": "sideways"},
		"unknown tc":           {"kind": "thermocouple", "tcType": "Q"},
		"address without port": {"address": "plant-daq"},
	} {
		t.Run(name, func(t *testing.T) {
			base := map[string]any{"address": "127.0.0.1:31763", "device": "cDAQ1Mod1", "channels": "ai0"}
			for k, v := range props {
				base[k] = v
			}
			m := newFlow(t).tab("main", nil).node("main", "daq", "dia-daq-read", base).manager()
			d, _ := m.Diagram("main")
			if len(d.Problems) == 0 {
				t.Fatal("the diagram has no problems")
			}
		})
	}
}

func TestDAQNodeDescriptorSaysWhatItNeeds(t *testing.T) {
	k := kinds["dia-daq-read"]
	if k == nil {
		t.Fatal("the node is not registered")
	}
	help := k.Descriptor.Help
	for _, want := range []string{"DAQmx driver", "gRPC Device Server", "TLS", "stand-in server only"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help does not say %q", want)
		}
	}
}
