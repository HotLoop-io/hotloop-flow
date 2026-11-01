package diagram

import (
	"context"
	"fmt"
	"math"

	"github.com/HotLoop-io/hotloop-flow/internal/diagram/dsp"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Analysis nodes: statistics of an array, its integral and derivative, and
// Butterworth filters. The arithmetic is in the dsp package. These nodes read
// tokens, turn a refusal from dsp into a fault on the error wire, and, for the
// filter, keep its memory between calls.

const analysisColor = "#c9d6d9"

func toTokens(v []float64) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// array reads the y input as an array of numbers, or says why it can't.
func array(in map[string]any) ([]float64, error) {
	y, ok := floats(in["y"])
	if !ok {
		return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("%v is not an array of numbers", in["y"])}
	}
	return y, nil
}

func badInput(err error) error { return &Fault{Code: CodeBadInput, Source: err.Error()} }

// stepSize is the time between samples, in seconds. The dt input wins, then the
// rate input in hertz, then the rate property.
func stepSize(in map[string]any, rate float64) (float64, error) {
	if v, wired := in["dt"]; wired {
		dt, ok := num(v)
		if !ok {
			return 0, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("%v is not a number of seconds", v)}
		}
		return dt, nil
	}
	if v, wired := in["rate"]; wired {
		r, ok := num(v)
		if !ok || r <= 0 {
			return 0, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("%v is not a sample rate above zero", v)}
		}
		return 1 / r, nil
	}
	return 1 / rate, nil
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-stats", Color: analysisColor, Icon: "stats", PaletteLabel: "statistics",
			InputPorts: []string{"y", "err?"}, OutputPorts: []string{"mean", "rms", "min", "max", "stddev", "err"},
			Help: "Takes an array of numbers and gives its mean, RMS (root mean square), smallest, largest, and standard deviation. " +
				"The standard deviation divides by n minus one, the sample form, so it needs at least two readings. " +
				"An empty array, or one with anything that is not a finite number in it, is an error on the error wire.",
		},
		New: func(map[string]any) (Op, error) {
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				y, err := array(in)
				if err != nil {
					return nil, err
				}
				out := map[string]any{}
				for name, fn := range map[string]func([]float64) (float64, error){
					"mean": dsp.Mean, "rms": dsp.RMS, "min": dsp.Min, "max": dsp.Max, "stddev": dsp.StdDev,
				} {
					v, err := fn(y)
					if err != nil {
						return nil, badInput(fmt.Errorf("%s: %w", name, err))
					}
					out[name] = v
				}
				return out, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-integral", Color: analysisColor, Icon: "integral", PaletteLabel: "integral", LabelProp: "mode",
			InputPorts: []string{"y", "rate?", "dt?", "err?"}, OutputPorts: []string{"result", "err"},
			Props: []node.Prop{
				choice("mode", "Result", "total", "total", "running"),
				prop("rate", "Samples per second", node.PropNumber, 1),
			},
			Help: "The area under an array of samples, by the trapezoid rule. Total gives one number. Running gives the area so far at each sample, starting at zero. " +
				"The time between samples comes from the dt input in seconds, else the rate input in hertz, else the Samples per second property. " +
				"At one sample per second the answer is in units times samples.",
		},
		New: func(raw map[string]any) (Op, error) {
			mode := strProp(raw, "mode", "total")
			if mode != "total" && mode != "running" {
				return nil, fmt.Errorf("%q is not a result (total or running)", mode)
			}
			rate := floatProp(raw, "rate", 1)
			if rate <= 0 {
				return nil, fmt.Errorf("the samples per second must be above zero")
			}
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				y, err := array(in)
				if err != nil {
					return nil, err
				}
				dt, err := stepSize(in, rate)
				if err != nil {
					return nil, err
				}
				if mode == "running" {
					r, err := dsp.RunningIntegral(y, dt)
					if err != nil {
						return nil, badInput(err)
					}
					return map[string]any{"result": toTokens(r)}, nil
				}
				a, err := dsp.Integrate(y, dt)
				if err != nil {
					return nil, badInput(err)
				}
				return map[string]any{"result": a}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-derivative", Color: analysisColor, Icon: "derivative", PaletteLabel: "derivative",
			InputPorts: []string{"y", "rate?", "dt?", "err?"}, OutputPorts: []string{"result", "err"},
			Props: []node.Prop{prop("rate", "Samples per second", node.PropNumber, 1)},
			Help: "The slope of an array of samples at each sample, as an array the same length. Inside the array it uses the samples on both sides; at the first and last sample it uses the one neighbor it has. " +
				"It needs at least two samples. The time between samples comes from the dt input in seconds, else the rate input in hertz, else the Samples per second property. " +
				"Differentiating amplifies noise, so filter a noisy signal first.",
		},
		New: func(raw map[string]any) (Op, error) {
			rate := floatProp(raw, "rate", 1)
			if rate <= 0 {
				return nil, fmt.Errorf("the samples per second must be above zero")
			}
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				y, err := array(in)
				if err != nil {
					return nil, err
				}
				dt, err := stepSize(in, rate)
				if err != nil {
					return nil, err
				}
				d, err := dsp.Derivative(y, dt)
				if err != nil {
					return nil, badInput(err)
				}
				return map[string]any{"result": toTokens(d)}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-filter", Color: analysisColor, Icon: "filter", PaletteLabel: "filter", LabelProp: "kind",
			InputPorts: []string{"y", "rate?", "err?"}, OutputPorts: []string{"y", "err"},
			Props: []node.Prop{
				choice("kind", "Kind", "lowpass", "lowpass", "highpass", "bandpass", "bandstop"),
				prop("cutoff", "Cutoff in hertz (the lower edge of a band)", node.PropNumber, 100),
				prop("cutoff2", "Upper edge in hertz (bandpass and bandstop)", node.PropNumber, 200),
				prop("order", "Order, 1 to 10", node.PropNumber, 4),
				prop("rate", "Samples per second", node.PropNumber, 1000),
			},
			Help: "A Butterworth filter: flat in the band it passes, with no ripple, and 3 dB down at the cutoff. " +
				"Lowpass keeps what is slower than the cutoff, highpass keeps what is faster, bandpass keeps what is between the two edges, and bandstop removes it. " +
				"A higher order cuts off more sharply and rings a little longer after a step. The cutoff must be above zero and below half the sample rate. " +
				"Give it an array, or one number at a time, and it remembers where it was between calls in a run, so a signal cut into pieces is filtered as one. " +
				"The rate input wins over the Samples per second property. " +
				"It starts from rest, so the first samples of an array carry a start-up settle; it delays the signal by an amount that depends on frequency, so it is wrong to use where the timing between two signals has to line up exactly; and it is not a linear-phase or zero-phase filter.",
		},
		New: func(raw map[string]any) (Op, error) {
			kind := dsp.Kind(strProp(raw, "kind", "lowpass"))
			order := intProp(raw, "order", 4)
			rate := floatProp(raw, "rate", 1000)
			f1, f2 := floatProp(raw, "cutoff", 100), floatProp(raw, "cutoff2", 200)
			if _, err := dsp.Design(kind, order, rate, f1, f2); err != nil {
				return nil, fmt.Errorf("the filter cannot be built: %v", err)
			}
			type state struct {
				rate   float64
				stream *dsp.Stream
			}
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				r := rate
				if v, wired := in["rate"]; wired {
					x, ok := num(v)
					if !ok {
						return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("%v is not a sample rate", v)}
					}
					r = x
				}
				var y []float64
				v, scalar := num(in["y"])
				if scalar {
					y = []float64{v}
				} else {
					var err error
					if y, err = array(in); err != nil {
						return nil, err
					}
					if len(y) == 0 {
						return nil, badInput(dsp.ErrEmpty)
					}
				}
				st := rc.Run.Local("filter:"+rc.Node.ID, func() any { return &state{} }).(*state)
				if st.stream == nil || st.rate != r {
					secs, err := dsp.Design(kind, order, r, f1, f2)
					if err != nil {
						return nil, badInput(err)
					}
					st.stream, st.rate = dsp.NewStream(secs), r
				}
				for i, s := range y {
					if math.IsNaN(s) || math.IsInf(s, 0) {
						return nil, badInput(fmt.Errorf("sample %d is not a finite number", i))
					}
				}
				out := st.stream.Process(y)
				if scalar {
					return map[string]any{"y": out[0]}, nil
				}
				return map[string]any{"y": toTokens(out)}, nil
			}), nil
		},
	})
}
