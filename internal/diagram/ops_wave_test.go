package diagram

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/tdms"
)

func rampJSON(n int) string {
	s := "["
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += "1"
	}
	return s + "]"
}

// D2: a 1 kHz, 1,000 sample waveform has dt 0.001 and spans one second, and
// taking it apart gives back what went in.
func TestWaveformAtOneKilohertz(t *testing.T) {
	b := newFlow(t).tab("d", nil).
		node("d", "y", "dia-const", map[string]any{"value": rampJSON(1000)}).
		node("d", "w", "dia-wave", map[string]any{"rate": 1000, "attrs": `{"unit":"V"}`}).
		node("d", "p", "dia-wave-parts", nil).
		node("d", "dt", "dia-indicator", nil).node("d", "dur", "dia-indicator", nil).node("d", "n", "dia-indicator", nil).
		wire("y.value", "w.y").wire("w.wave", "p.wave").
		wire("p.dt", "dt.value").wire("p.duration", "dur.value").wire("p.samples", "n.value")
	r, err := b.run("d")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	if got := lastOn(t, r, "d/p.dt>dt.value"); got != 0.001 {
		t.Errorf("dt %v", got)
	}
	if got, _ := lastOn(t, r, "d/p.duration>dur.value").(float64); math.Abs(got-1) > 1e-9 {
		t.Errorf("duration %v, want 1", got)
	}
	if got := lastOn(t, r, "d/p.samples>n.value"); got != 1000.0 {
		t.Errorf("samples %v", got)
	}
}

// The size cap cuts a waveform and counts what it dropped; an array that fits is
// untouched.
func TestWaveformSizeCapCountsWhatItDrops(t *testing.T) {
	b := newFlow(t).tab("d", nil).
		node("d", "y", "dia-const", map[string]any{"value": rampJSON(50)}).
		node("d", "w", "dia-wave", map[string]any{"rate": 10, "maxSamples": 32}).
		node("d", "n", "dia-indicator", nil).node("d", "drop", "dia-indicator", nil).
		wire("y.value", "w.y").wire("w.samples", "n.value").wire("w.dropped", "drop.value")
	r, err := b.run("d")
	if err != nil {
		t.Fatal(err)
	}
	if got := lastOn(t, r, "d/w.samples>n.value"); got != 32.0 {
		t.Errorf("held %v samples, want 32", got)
	}
	if got := lastOn(t, r, "d/w.dropped>drop.value"); got != 18.0 {
		t.Errorf("dropped %v, want 18", got)
	}
}

// Analysis takes a waveform for its samples and uses its step: the integral of a
// constant 2 over a 1 kHz, 1,001 sample waveform (one second) is 2.
func TestAnalysisReadsAWaveformAndItsStep(t *testing.T) {
	b := newFlow(t).tab("d", nil).
		node("d", "y", "dia-const", map[string]any{"value": "[" + repeat("2,", 1000) + "2]"}).
		node("d", "w", "dia-wave", map[string]any{"rate": 1000}).
		node("d", "i", "dia-integral", nil).node("d", "area", "dia-indicator", nil).
		wire("y.value", "w.y").wire("w.wave", "i.y").wire("i.result", "area.value")
	r, err := b.run("d")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := lastOn(t, r, "d/i.result>area.value").(float64); math.Abs(got-2) > 1e-9 {
		t.Errorf("area %v, want 2", got)
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// D2: a waveform's start time, step, sample count and attributes survive logging
// to TDMS, in the properties a TDMS reader looks for.
func TestWaveformSurvivesLoggingToTDMS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wave.tdms")
	t0 := "2026-11-14T12:00:00Z"
	b := newFlow(t).tab("d", nil).
		node("d", "y", "dia-const", map[string]any{"value": "[" + repeat("1.5,", 99) + "1.5]"}).
		node("d", "t", "dia-const", map[string]any{"value": `"` + t0 + `"`}).
		node("d", "w", "dia-wave", map[string]any{"rate": 1000, "attrs": `{"unit_string":"V","channel":"ai0"}`}).
		node("d", "rec", "dia-bundle", map[string]any{"names": "v"}).
		node("d", "log", "dia-log-tdms", map[string]any{"path": path, "group": "Bench"}).
		node("d", "n", "dia-indicator", nil).
		wire("y.value", "w.y").wire("t.value", "w.t0").wire("w.wave", "rec.v").wire("rec.record", "log.row").wire("log.rows", "n.value")
	m := b.manager()
	mustProblemFree(t, m, "d")
	r, err := m.Start("d", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	f, err := tdms.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ch := f.Group("Bench").Channel("v")
	if ch == nil {
		t.Fatalf("no channel v: %+v", f.Group("Bench"))
	}
	if vals, _ := ch.Values.([]float64); len(vals) != 100 {
		t.Errorf("samples %d", len(vals))
	}
	if v, _ := tdms.Prop(ch.Properties, "wf_increment"); v != 0.001 {
		t.Errorf("wf_increment %v", v)
	}
	if v, _ := tdms.Prop(ch.Properties, "wf_start_time"); v == nil {
		t.Errorf("no wf_start_time")
	} else if ts, ok := v.(time.Time); !ok || !ts.Equal(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("wf_start_time %v", v)
	}
	if v, _ := tdms.Prop(ch.Properties, "unit_string"); v != "V" {
		t.Errorf("unit_string %v", v)
	}
	if v, _ := tdms.Prop(ch.Properties, "channel"); v != "ai0" {
		t.Errorf("channel %v", v)
	}
}
