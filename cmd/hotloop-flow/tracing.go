package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/flowtest"
	"github.com/HotLoop-io/hotloop-flow/internal/metrics"
	"github.com/HotLoop-io/hotloop-flow/internal/otlp"
)

// traceFamilies is what the tracer is holding and what it has let go of, so an
// operator can see the buffer is full and bounded rather than guess.
func traceFamilies(t *engine.Tracer) []metrics.Family {
	one := func(name, typ, help string, v func(engine.TraceStats) float64) metrics.Family {
		return metrics.Family{Name: name, Type: typ, Help: help, Collect: func() []metrics.Sample {
			return []metrics.Sample{{Value: v(t.Stats())}}
		}}
	}
	return []metrics.Family{
		one("hotloop_flow_traces_kept", "gauge", "Message traces held in the buffer right now.",
			func(s engine.TraceStats) float64 { return float64(s.Kept) }),
		one("hotloop_flow_trace_bytes", "gauge", "Roughly how many bytes the kept traces hold.",
			func(s engine.TraceStats) float64 { return float64(s.Bytes) }),
		one("hotloop_flow_traces_started_total", "counter", "Message traces started.",
			func(s engine.TraceStats) float64 { return float64(s.Started) }),
		one("hotloop_flow_traces_forgotten_total", "counter", "Traces pushed out of the buffer to make room.",
			func(s engine.TraceStats) float64 { return float64(s.Evicted) }),
		one("hotloop_flow_traces_skipped_total", "counter", "Messages not traced because trace.perSecond had been reached.",
			func(s engine.TraceStats) float64 { return float64(s.Skipped) }),
		one("hotloop_flow_trace_hops_dropped_total", "counter", "Hops not written down because their trace was full or already forgotten.",
			func(s engine.TraceStats) float64 { return float64(s.HopsDropped) }),
	}
}

// otlpFamilies is the OpenTelemetry export's own counters. Failed above zero
// means the collector isn't getting spans.
func otlpFamilies(e *otlp.Exporter) []metrics.Family {
	one := func(name, help string, v func(otlp.Stats) int64) metrics.Family {
		return metrics.Family{Name: name, Type: "counter", Help: help, Collect: func() []metrics.Sample {
			return []metrics.Sample{{Value: float64(v(e.Stats()))}}
		}}
	}
	return []metrics.Family{
		one("hotloop_flow_otlp_spans_exported_total", "Spans the collector accepted.",
			func(s otlp.Stats) int64 { return s.Exported }),
		one("hotloop_flow_otlp_spans_dropped_total", "Spans dropped because the export queue was full.",
			func(s otlp.Stats) int64 { return s.Dropped }),
		one("hotloop_flow_otlp_spans_failed_total", "Spans the collector didn't take after a retry.",
			func(s otlp.Stats) int64 { return s.Failed }),
	}
}

// Replay runs a replay in a child process: this binary, `test -stdin`, with
// the trace in the bundle. A child for the reason tests get one: the draft
// starts every node in it, and some of them register in places the whole
// process shares.
func (s *testSuite) Replay(ctx context.Context, flows []byte, trace engine.TraceView) (flowtest.ReplayResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	bundle, err := json.Marshal(TestBundle{Flows: flows, Runtime: s.runtime, Replay: &trace})
	if err != nil {
		return flowtest.ReplayResult{}, err
	}
	exe, env, err := s.command()
	if err != nil {
		return flowtest.ReplayResult{}, fmt.Errorf("finding this binary to replay with: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, "test", "-stdin")
	cmd.Env = env
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(bundle)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &capped{buf: &stderr, max: 8 << 10}
	err = cmd.Run()
	if ctx.Err() != nil {
		return flowtest.ReplayResult{}, fmt.Errorf("the replay took longer than tests.timeout (%s) and was stopped", s.timeout)
	}
	if err != nil {
		return flowtest.ReplayResult{}, fmt.Errorf("running the replay: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var res flowtest.ReplayResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return flowtest.ReplayResult{}, fmt.Errorf("the replay answered with something that isn't a result: %v", err)
	}
	return res, nil
}
