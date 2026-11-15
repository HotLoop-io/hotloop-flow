package diagram

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// textLog is an open log file. A run opens it the first time a row arrives and
// closes it when the run ends. Every row is written and flushed before the node
// finishes, so a power cut loses at most the row being written.
type textLog struct {
	f       *os.File
	w       *csv.Writer
	columns []string
	opened  time.Time
}

func (l *textLog) Close() error {
	if l.w != nil {
		l.w.Flush()
	}
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-log-text", Color: "#bfd9c9", Icon: "log", PaletteLabel: "log to text file", LabelProp: "path",
			InputPorts: []string{"row", "err?"}, OutputPorts: []string{"rows", "err"},
			Props: append([]node.Prop{
				{Name: "path", Label: "File", Kind: node.PropString, Required: true, Default: "log.csv"},
				prop("columns", "Columns, comma separated (empty for the record's fields in order of name)", node.PropString, ""),
				choice("delimiter", "Separator", "comma", "comma", "tab", "semicolon"),
				prop("timeName", "Name of the time column", node.PropString, "time"),
			}, rotationProps()...),
			Help: "Appends each record it is given as one row of a text file, with a time column first. A new file starts with a header. A file that already has rows is appended to, as long as its header is the one this node would write, and a different header is an error, not a mixed file. It can start a new file when this one reaches a size or an age and keep only the newest finished files: bench.csv becomes bench-20261114T120000Z.csv and the log goes on in bench.csv. The age counts from when a run first writes to the file.",
		},
		New: func(raw map[string]any) (Op, error) {
			path := strProp(raw, "path", "")
			if path == "" {
				return nil, fmt.Errorf("the log has no file")
			}
			cols := splitList(strProp(raw, "columns", ""))
			timeName := strProp(raw, "timeName", "time")
			rot, err := rotationOf(raw)
			if err != nil {
				return nil, err
			}
			comma := ','
			switch strProp(raw, "delimiter", "comma") {
			case "tab":
				comma = '\t'
			case "semicolon":
				comma = ';'
			}
			return opFunc(func(_ context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				rec, ok := in["row"].(map[string]any)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the row input is not a record"}
				}
				lg := rc.Run.Local("log:"+rc.Node.ID, func() any { return &textLog{} }).(*textLog)
				var err error
				if lg.f != nil {
					if st, serr := lg.f.Stat(); serr == nil && rot.due(st.Size(), lg.opened, time.Now()) {
						_ = lg.Close()
						lg.f, lg.w = nil, nil
						if err = rot.finish(path, time.Now()); err != nil {
							return nil, &Fault{Code: CodeFile, Source: err.Error()}
						}
					}
				}
				if lg.f == nil {
					columns := cols
					if len(columns) == 0 {
						for k := range rec {
							columns = append(columns, k)
						}
						sort.Strings(columns)
					}
					if err = lg.open(path, comma, append([]string{timeName}, columns...)); err != nil {
						return nil, &Fault{Code: CodeFile, Source: err.Error()}
					}
					lg.columns = columns
					lg.opened = time.Now()
				}
				row := make([]string, 0, len(lg.columns)+1)
				row = append(row, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
				for _, c := range lg.columns {
					row = append(row, cell(rec[c]))
				}
				if err = lg.w.Write(row); err != nil {
					return nil, &Fault{Code: CodeFile, Source: err.Error()}
				}
				lg.w.Flush()
				if err = lg.w.Error(); err != nil {
					return nil, &Fault{Code: CodeFile, Source: err.Error()}
				}
				return map[string]any{"rows": 1.0}, nil
			}), nil
		},
	})
}

func (l *textLog) open(path string, comma rune, header []string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	want := strings.Join(header, string(comma))
	if st.Size() > 0 {
		first, err := firstLine(path)
		if err != nil {
			_ = f.Close()
			return err
		}
		if first != want {
			_ = f.Close()
			return fmt.Errorf("%s already has the header %q, and this log would write %q", path, first, want)
		}
	}
	l.f = f
	l.w = csv.NewWriter(f)
	l.w.Comma = comma
	if st.Size() == 0 {
		if err := l.w.Write(header); err != nil {
			return err
		}
		l.w.Flush()
		return l.w.Error()
	}
	return nil
}

func firstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return fmt.Sprintf("%v", jsonValue(v))
}
