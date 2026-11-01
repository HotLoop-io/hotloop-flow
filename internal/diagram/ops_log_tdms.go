package diagram

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/tdms"
)

// tdmsLog is an open TDMS file. A run opens it the first time a row arrives and
// closes it when the run ends.
type tdmsLog struct {
	w       *tdms.Writer
	columns []string
	kinds   []tdmsKind
}

type tdmsKind int

const (
	kindNumber tdmsKind = iota
	kindBool
	kindText
)

func (l *tdmsLog) Close() error {
	if l.w != nil {
		return l.w.Close()
	}
	return nil
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-log-tdms", Color: "#bfd9c9", Icon: "log", PaletteLabel: "log to TDMS file", LabelProp: "path",
			InputPorts: []string{"row", "err?"}, OutputPorts: []string{"rows", "err"},
			Props: []node.Prop{
				{Name: "path", Label: "File", Kind: node.PropString, Required: true, Default: "log.tdms"},
				prop("group", "Group", node.PropString, "Measurements"),
				prop("columns", "Columns, comma separated (empty for the record's fields in order of name)", node.PropString, ""),
				prop("properties", "File properties (JSON object, written when the run starts)", node.PropJSON, "{}"),
				prop("sync", "Write each row to disk before going on", node.PropBool, true),
			},
			Help: "Appends each record it is given to a TDMS file, the format test benches and analysis tools read. Every column of the record is a channel in one group. A number is stored as a 64 bit float, a true or false as a bool, and anything else as text. A column that holds a list of numbers adds all of them, which is how a block of samples goes in. The properties are written on the file when the run starts. A file that already exists is appended to, so runs follow each other in one file, and a file cut short by a power loss is repaired to its last complete row before the new rows go on. With write to disk on, a power cut loses at most the row being written.",
		},
		New: func(raw map[string]any) (Op, error) {
			path := strProp(raw, "path", "")
			if path == "" {
				return nil, fmt.Errorf("the log has no file")
			}
			group := strProp(raw, "group", "Measurements")
			cols := splitList(strProp(raw, "columns", ""))
			props, err := tdmsFileProps(raw["properties"])
			if err != nil {
				return nil, err
			}
			syncRows := true
			if b, ok := raw["sync"].(bool); ok {
				syncRows = b
			}
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				rec, ok := in["row"].(map[string]any)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the row input is not a record"}
				}
				lg := rc.Run.Local("tdms:"+rc.Node.ID, func() any { return &tdmsLog{} }).(*tdmsLog)
				if lg.w == nil {
					columns := cols
					if len(columns) == 0 {
						for k := range rec {
							columns = append(columns, k)
						}
						sort.Strings(columns)
					}
					if len(columns) == 0 {
						return nil, &Fault{Code: CodeBadInput, Source: "the record has no fields to log"}
					}
					var opts []tdms.Option
					if syncRows {
						opts = append(opts, tdms.WithSync())
					}
					w, err := tdms.Open(path, opts...)
					if err != nil {
						return nil, &Fault{Code: CodeFile, Source: err.Error()}
					}
					for _, p := range props {
						if err := w.SetFileProperty(p.name, p.value); err != nil {
							_ = w.Close()
							return nil, &Fault{Code: CodeFile, Source: err.Error()}
						}
					}
					lg.w, lg.columns = w, columns
					for _, c := range columns {
						lg.kinds = append(lg.kinds, kindOf(rec[c]))
					}
				}
				data := make([]tdms.ChannelData, len(lg.columns))
				for i, c := range lg.columns {
					vals, err := tdmsColumn(lg.kinds[i], rec[c])
					if err != nil {
						return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("column %s: %v", c, err)}
					}
					data[i] = tdms.ChannelData{Group: group, Channel: c, Values: vals}
				}
				if err := lg.w.WriteChannels(data...); err != nil {
					return nil, &Fault{Code: CodeFile, Source: err.Error()}
				}
				return map[string]any{"rows": 1.0}, nil
			}), nil
		},
	})
}

type tdmsProp struct {
	name  string
	value any
}

// tdmsFileProps reads the properties setting: a JSON object, or the text of one.
// Numbers, text and true or false are kept; nothing else has a place in a file.
func tdmsFileProps(v any) ([]tdmsProp, error) {
	var m map[string]any
	switch x := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		m = x
	case string:
		if strings.TrimSpace(x) == "" {
			return nil, nil
		}
		if err := json.Unmarshal([]byte(x), &m); err != nil {
			return nil, fmt.Errorf("the properties are not a JSON object: %v", err)
		}
	default:
		return nil, fmt.Errorf("the properties are not a JSON object")
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []tdmsProp
	for _, k := range names {
		switch x := m[k].(type) {
		case float64, string, bool:
			out = append(out, tdmsProp{k, x})
		case nil:
		default:
			return nil, fmt.Errorf("property %s is not a number, text or true or false", k)
		}
	}
	return out, nil
}

func kindOf(v any) tdmsKind {
	switch x := v.(type) {
	case float64, int, int64:
		return kindNumber
	case bool:
		return kindBool
	case []any:
		if len(x) > 0 {
			for _, e := range x {
				if _, ok := num(e); !ok {
					return kindText
				}
			}
			return kindNumber
		}
	}
	return kindText
}

// tdmsColumn turns a record field into the values of its channel. The channel's
// kind was fixed by the first row of the run, so a number that turns up as text
// later is an error on the wire and not a column of mixed types.
func tdmsColumn(k tdmsKind, v any) (any, error) {
	switch k {
	case kindNumber:
		switch x := v.(type) {
		case nil:
			return []float64{math.NaN()}, nil
		case []any:
			out := make([]float64, len(x))
			for i, e := range x {
				f, ok := num(e)
				if !ok {
					return nil, fmt.Errorf("item %d is not a number", i)
				}
				out[i] = f
			}
			return out, nil
		}
		f, ok := num(v)
		if !ok {
			return nil, fmt.Errorf("%v is not a number, and this column started as numbers", cell(v))
		}
		return []float64{f}, nil
	case kindBool:
		if v == nil {
			return []bool{false}, nil
		}
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%v is not true or false, and this column started as true or false", cell(v))
		}
		return []bool{b}, nil
	}
	return []string{cell(v)}, nil
}
