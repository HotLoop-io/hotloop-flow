package diagram

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// DefaultReadingPattern reads a frame like "ST,GS,  12.34 kg": a status word, a
// mode word, then a number and its unit.
const DefaultReadingPattern = `^\s*(?P<status>[A-Za-z]{2}),\s*[A-Za-z]{2},\s*(?P<value>[-+]?\d+(?:\.\d+)?)\s*(?P<unit>[A-Za-z%]+)\s*$`

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-parse-reading", Color: "#d3c4de", Icon: "parse", PaletteLabel: "parse reading", LabelProp: "pattern",
			InputPorts: []string{"line", "err?"}, OutputPorts: []string{"value", "unit", "status", "err"},
			Props: []node.Prop{
				{Name: "pattern", Label: "Pattern", Kind: node.PropString, Default: DefaultReadingPattern,
					Help: "A regular expression. The group named value is the number, unit is the unit, and status, if there is one, comes out as text."},
			},
			Help: "Takes a number and its unit out of a line of text. A frame that does not fit the pattern is an error on the error wire, with the frame in its source.",
		},
		New: func(raw map[string]any) (Op, error) {
			re, err := regexp.Compile(strProp(raw, "pattern", DefaultReadingPattern))
			if err != nil {
				return nil, fmt.Errorf("the pattern is not a regular expression: %v", err)
			}
			vi := re.SubexpIndex("value")
			if vi < 0 {
				return nil, fmt.Errorf("the pattern has no group named value")
			}
			ui, si := re.SubexpIndex("unit"), re.SubexpIndex("status")
			return opFunc(func(_ context.Context, _ *Ctx, in map[string]any) (map[string]any, error) {
				line, ok := in["line"].(string)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the line input is not text"}
				}
				m := re.FindStringSubmatch(line)
				if m == nil {
					return nil, &Fault{Code: CodeParse, Source: fmt.Sprintf("unrecognized frame %q", clip(line, 60))}
				}
				f, err := strconv.ParseFloat(strings.TrimSpace(m[vi]), 64)
				if err != nil {
					return nil, &Fault{Code: CodeParse, Source: fmt.Sprintf("%q is not a number", m[vi])}
				}
				out := map[string]any{"value": f}
				if ui >= 0 {
					out["unit"] = m[ui]
				}
				if si >= 0 {
					out["status"] = m[si]
				}
				return out, nil
			}), nil
		},
	})

	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-stable", Color: "#d3c4de", Icon: "stable", PaletteLabel: "stable value", LabelProp: "size",
			InputPorts: []string{"value", "window?", "err?"}, OutputPorts: []string{"stable", "window", "mean", "spread", "err"},
			Props: []node.Prop{
				prop("size", "Readings in the window", node.PropNumber, 5),
				prop("tolerance", "Widest spread that counts as stable", node.PropNumber, 0.02),
			},
			Help: "Says whether the last readings agree. It keeps a window of the latest readings and is stable when the window is full and its highest and lowest differ by no more than the tolerance. Wire the window out of a loop's carried value and back in to carry it from pass to pass. Leave Window unwired and the node keeps its own window for the run.",
		},
		New: func(raw map[string]any) (Op, error) {
			size := intProp(raw, "size", 5)
			tol := floatProp(raw, "tolerance", 0.02)
			if tol < 0 {
				return nil, fmt.Errorf("the tolerance cannot be negative")
			}
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				v, ok := num(in["value"])
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("%v is not a number", in["value"])}
				}
				var win []float64
				_, wired := in["window"]
				var local *[]float64
				if wired {
					win, _ = floats(in["window"])
				} else {
					local = rc.Run.Local("stable:"+rc.Node.ID, func() any { return new([]float64) }).(*[]float64)
					win = *local
				}
				win = append(append([]float64(nil), win...), v)
				if len(win) > size {
					win = win[len(win)-size:]
				}
				if local != nil {
					*local = win
				}
				lo, hi, sum := math.Inf(1), math.Inf(-1), 0.0
				for _, x := range win {
					lo, hi, sum = math.Min(lo, x), math.Max(hi, x), sum+x
				}
				spread := hi - lo
				arr := make([]any, len(win))
				for i, x := range win {
					arr[i] = x
				}
				return map[string]any{
					"stable": len(win) >= size && spread <= tol, "window": arr,
					"mean": sum / float64(len(win)), "spread": spread,
				}, nil
			}), nil
		},
	})
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
