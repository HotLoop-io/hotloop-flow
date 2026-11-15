package diagram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-wave", Color: analysisColor, Icon: "wave", PaletteLabel: "build waveform", LabelProp: "rate",
			InputPorts:  []string{"y", "dt?", "rate?", "t0?", "attrs?", "err?"},
			OutputPorts: []string{"wave", "samples", "dropped", "err"},
			Props: []node.Prop{
				prop("rate", "Samples per second", node.PropNumber, 1000),
				prop("attrs", "Attributes (JSON object)", node.PropJSON, "{}"),
				prop("maxSamples", "Most samples one waveform holds", node.PropNumber, DefaultMaxSamples),
			},
			Help: "Makes a waveform from an array of samples: the samples, the time between them, when the first was taken, and named attributes such as a unit. " +
				"The time between samples is the dt input in seconds, else the rate input in hertz, else the Samples per second setting. " +
				"The start time is the t0 input (a time as text), else the moment the node runs. " +
				"A waveform holds at most the most samples setting; any more are cut off and counted on Dropped, so a stream that outruns what reads it cannot fill the memory. " +
				"Samples is how many it holds.",
		},
		New: func(raw map[string]any) (Op, error) {
			rate := floatProp(raw, "rate", 1000)
			if rate <= 0 {
				return nil, fmt.Errorf("the samples per second must be above zero")
			}
			max := int(floatProp(raw, "maxSamples", DefaultMaxSamples))
			if max <= 0 {
				return nil, fmt.Errorf("the most samples must be above zero")
			}
			attrs, err := waveAttrs(raw["attrs"])
			if err != nil {
				return nil, err
			}
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				y, ok := floats(in["y"])
				if !ok {
					return nil, waveFault("%v is not an array of numbers", in["y"])
				}
				dt, err := stepSize(in, rate)
				if err != nil {
					return nil, err
				}
				if dt <= 0 {
					return nil, waveFault("the time between samples must be above zero, got %v", dt)
				}
				w := wave{T0: time.Now().UTC(), Dt: dt, Y: y, Attrs: map[string]any{}}
				if v, wired := in["t0"]; wired {
					s, _ := v.(string)
					t, err := time.Parse(time.RFC3339Nano, s)
					if err != nil {
						return nil, waveFault("%v is not a time (RFC 3339, like 2026-11-14T12:00:00Z)", v)
					}
					w.T0 = t
				}
				for k, v := range attrs {
					w.Attrs[k] = v
				}
				if a, wired := in["attrs"]; wired {
					m, ok := a.(map[string]any)
					if !ok {
						return nil, waveFault("the attrs input is not a record")
					}
					for k, v := range m {
						w.Attrs[k] = v
					}
				}
				dropped := 0
				if len(w.Y) > max {
					dropped = len(w.Y) - max
					w.Y = w.Y[:max]
				}
				return map[string]any{"wave": w.token(), "samples": float64(len(w.Y)), "dropped": float64(dropped)}, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-wave-parts", Color: analysisColor, Icon: "wave", PaletteLabel: "waveform parts",
			InputPorts:  []string{"wave", "err?"},
			OutputPorts: []string{"y", "dt", "t0", "duration", "samples", "attrs", "err"},
			Help:        "Takes a waveform apart: its samples, the seconds between them, when the first was taken (as text), how long it spans (the samples times the step), how many samples it holds and its attributes.",
		},
		New: func(map[string]any) (Op, error) {
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				w, ok := asWave(in["wave"])
				if !ok {
					return nil, waveFault("%v is not a waveform", in["wave"])
				}
				return map[string]any{
					"y": toTokens(w.Y), "dt": w.Dt, "t0": w.T0.UTC().Format(time.RFC3339Nano),
					"duration": w.Duration(), "samples": float64(len(w.Y)), "attrs": w.token()["attrs"],
				}, nil
			}), nil
		},
	})
}

// waveAttrs reads the attributes setting: a JSON object, or the text of one.
func waveAttrs(v any) (map[string]any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return x, nil
	case string:
		if strings.TrimSpace(x) == "" {
			return nil, nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(x), &m); err != nil {
			return nil, fmt.Errorf("the attributes are not a JSON object: %v", err)
		}
		return m, nil
	}
	return nil, fmt.Errorf("the attributes are not a JSON object")
}
