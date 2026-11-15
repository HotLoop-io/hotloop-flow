package diagram

import (
	"fmt"
	"time"
)

// A waveform is a block of evenly spaced samples that knows when it started and
// how far apart they are. On a wire it is a record with the type "waveform":
//
//	{"type": "waveform", "t0": "2026-11-14T12:00:00Z", "dt": 0.001, "y": [...], "attrs": {...}}
//
// t0 is the time of the first sample (RFC 3339, nanoseconds), dt the seconds
// between samples, y the samples and attrs named properties such as a unit or a
// channel name. It is plain JSON, so it crosses to the browser, to a log and to a
// file unchanged, and a node that wants only the samples reads y.
const waveType = "waveform"

// DefaultMaxSamples caps one waveform. A block larger than this is cut at the
// cap and the rest is counted as dropped, not held: a stream that outruns its
// consumer must not be able to fill the memory of a Pi.
const DefaultMaxSamples = 1 << 20

type wave struct {
	T0    time.Time
	Dt    float64
	Y     []float64
	Attrs map[string]any
}

// Duration is how long the waveform spans: the samples times the step.
func (w wave) Duration() float64 { return float64(len(w.Y)) * w.Dt }

// token is the waveform as the record that goes on a wire.
func (w wave) token() map[string]any {
	attrs := map[string]any{}
	for k, v := range w.Attrs {
		attrs[k] = v
	}
	return map[string]any{
		"type":  waveType,
		"t0":    w.T0.UTC().Format(time.RFC3339Nano),
		"dt":    w.Dt,
		"y":     toTokens(w.Y),
		"attrs": attrs,
	}
}

// asWave reads a token as a waveform, or says it is not one.
func asWave(v any) (wave, bool) {
	m, ok := v.(map[string]any)
	if !ok || m["type"] != waveType {
		return wave{}, false
	}
	w := wave{Attrs: map[string]any{}}
	var err error
	if s, _ := m["t0"].(string); s != "" {
		if w.T0, err = time.Parse(time.RFC3339Nano, s); err != nil {
			return wave{}, false
		}
	}
	dt, ok := num(m["dt"])
	if !ok || dt <= 0 {
		return wave{}, false
	}
	w.Dt = dt
	if w.Y, ok = floats(m["y"]); !ok {
		if m["y"] != nil {
			return wave{}, false
		}
		w.Y = []float64{}
	}
	if a, ok := m["attrs"].(map[string]any); ok {
		for k, v := range a {
			w.Attrs[k] = v
		}
	}
	return w, true
}

func waveFault(format string, a ...any) error {
	return &Fault{Code: CodeBadInput, Source: fmt.Sprintf(format, a...)}
}
