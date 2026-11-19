package diagram

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/HotLoop-io/hotloop-flow/internal/nodes"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// A row for the database, as it waits on disk: where it goes and what is in it.
// The time is the one the row arrived at the node, so a row that sat on disk for
// five minutes is written with the time it was made, not the time it was sent.
type pgEnvelope struct {
	Table      string          `json:"table"`
	Columns    []string        `json:"columns"`
	Values     json.RawMessage `json:"values"`
	TimeColumn string          `json:"timeColumn,omitempty"`
	Time       time.Time       `json:"time,omitempty"`
	OnConflict string          `json:"onConflict,omitempty"`
}

// pgWait is how long a direct insert may take. A durable output gives up sooner,
// at the outbox's own send time, and keeps the row on disk.
const pgWait = 10 * time.Second

// row is the columns this row is written to and its values, the time column last.
func (e pgEnvelope) row() ([]string, []any, error) {
	var vals []any
	if err := json.Unmarshal(e.Values, &vals); err != nil {
		return nil, nil, fmt.Errorf("a queued row is not readable: %w", err)
	}
	cols := e.Columns
	if e.TimeColumn != "" {
		cols = append(append([]string(nil), cols...), e.TimeColumn)
		vals = append(vals, e.Time)
	}
	return cols, vals, nil
}

func (e pgEnvelope) insert(ctx context.Context, cfg node.Node) error {
	cols, vals, err := e.row()
	if err != nil {
		return err
	}
	return nodes.InsertPostgres(ctx, cfg, e.Table, cols, vals, e.OnConflict)
}

// sameTarget is whether two rows can go in one statement.
func (e pgEnvelope) sameTarget(o pgEnvelope) bool {
	if e.Table != o.Table || e.TimeColumn != o.TimeColumn || e.OnConflict != o.OnConflict || len(e.Columns) != len(o.Columns) {
		return false
	}
	for i := range e.Columns {
		if e.Columns[i] != o.Columns[i] {
			return false
		}
	}
	return true
}

// pgDeliverBatch writes the leading rows that go to the same table and columns in
// one statement, in order, and says how many it wrote. Rows behind a row that
// goes elsewhere wait for the next call, so the order of the queue holds.
func pgDeliverBatch(m *Manager, raw map[string]any) func(ctx context.Context, bodies [][]byte) (int, error) {
	server := strProp(raw, "server", "")
	return func(ctx context.Context, bodies [][]byte) (int, error) {
		cfg, ok := m.configNode(server)
		if !ok {
			return 0, fmt.Errorf("the server %s is not running", server)
		}
		var first pgEnvelope
		var cols []string
		var rows [][]any
		for i, body := range bodies {
			var e pgEnvelope
			if err := json.Unmarshal(body, &e); err != nil {
				if i == 0 {
					return 0, fmt.Errorf("a queued row is not readable: %w (%w)", err, nodes.ErrRowRejected)
				}
				break
			}
			if i > 0 && !first.sameTarget(e) {
				break
			}
			c, v, err := e.row()
			if err != nil {
				if i == 0 {
					return 0, fmt.Errorf("%w (%w)", err, nodes.ErrRowRejected)
				}
				break
			}
			if i == 0 {
				first, cols = e, c
			}
			if len(rows) >= nodes.MaxInsertRows(len(cols)) {
				break
			}
			rows = append(rows, v)
		}
		if err := nodes.InsertPostgresRows(ctx, cfg, first.Table, cols, rows, first.OnConflict); err != nil {
			return 0, err
		}
		return len(rows), nil
	}
}

func pgDeliver(m *Manager, raw map[string]any) func(ctx context.Context, body []byte) error {
	server := strProp(raw, "server", "")
	return func(ctx context.Context, body []byte) error {
		var e pgEnvelope
		if err := json.Unmarshal(body, &e); err != nil {
			return fmt.Errorf("a queued row is not readable: %w", err)
		}
		cfg, ok := m.configNode(server)
		if !ok {
			return fmt.Errorf("the server %s is not running", server)
		}
		return e.insert(ctx, cfg)
	}
}

func init() {
	register(Kind{
		Descriptor: node.Descriptor{
			Type: "dia-postgres-out", Color: "#bfd9c9", Icon: "db", PaletteLabel: "postgres out", LabelProp: "table",
			InputPorts: []string{"row", "err?"}, OutputPorts: []string{"sent", "err"},
			Props: []node.Prop{
				{Name: "server", Label: "Server", Kind: node.PropConfigRef, ConfigType: "hotloop-flow-postgres", Required: true},
				{Name: "table", Label: "Table", Kind: node.PropString, Required: true, Help: "Schema-qualified names are allowed, e.g. public.readings."},
				prop("columns", "Columns, comma separated (empty for the record's fields in order of name)", node.PropString, ""),
				prop("timeColumn", "Column for the time the row was made (empty for none)", node.PropString, ""),
				{Name: "onConflict", Label: "On conflict", Kind: node.PropSelect, Default: "error", Options: []node.Option{
					{Value: "error", Label: "Raise an error"}, {Value: "nothing", Label: "Skip the row"}}},
				{Name: "durable", Label: "Keep what the database misses", Kind: node.PropBool, Default: false,
					Help: "When the database is down, rows are written to disk and inserted, in order, when it is back. They survive a restart. Without this, a row the database misses is an error on the error wire and is gone."},
			},
			Help: "Writes each record it is given as one row of a table in PostgreSQL or TimescaleDB, through the server the flow file configures, the same connection the postgres node uses. A record's fields go in the columns of the same name. A row is written every time the node runs, so inside a loop that is one row per pass. The time column, when set, gets the time the row arrived here. Mark it durable and a row the database misses waits on disk and is inserted in order when the database is back, even across a restart. Delivery from disk is at least once: the one row that was in flight when the link died can arrive twice, so give the table a unique key and choose Skip on conflict if that matters. The sent output is true when the row went straight in and false when it is waiting on disk.",
		},
		Durable:      func(raw map[string]any) bool { d, _ := raw["durable"].(bool); return d },
		Deliver:      pgDeliver,
		DeliverBatch: pgDeliverBatch,
		New: func(raw map[string]any) (Op, error) {
			server := strProp(raw, "server", "")
			if server == "" {
				return nil, fmt.Errorf("no server selected")
			}
			table := strProp(raw, "table", "")
			cols := splitList(strProp(raw, "columns", ""))
			timeCol := strings.TrimSpace(strProp(raw, "timeColumn", ""))
			if len(cols) > 0 {
				if err := nodes.CheckPostgresTarget(table, cols); err != nil {
					return nil, err
				}
			} else if err := nodes.CheckPostgresTarget(table, []string{"x"}); err != nil {
				return nil, err
			}
			if timeCol != "" {
				if err := nodes.CheckPostgresTarget(table, []string{timeCol}); err != nil {
					return nil, fmt.Errorf("time column: %w", err)
				}
				for _, c := range cols {
					if strings.EqualFold(c, timeCol) {
						return nil, fmt.Errorf("%q is both a column and the time column", c)
					}
				}
			}
			onConflict := strProp(raw, "onConflict", "error")
			durable, _ := raw["durable"].(bool)
			return opFunc(func(ctx context.Context, rc *Ctx, in map[string]any) (map[string]any, error) {
				rec, ok := in["row"].(map[string]any)
				if !ok {
					return nil, &Fault{Code: CodeBadInput, Source: "the row input is not a record"}
				}
				columns := cols
				if len(columns) == 0 {
					for k := range rec {
						columns = append(columns, k)
					}
					sort.Strings(columns)
					if err := nodes.CheckPostgresTarget(table, columns); err != nil {
						return nil, &Fault{Code: CodeBadInput, Source: err.Error()}
					}
					for _, c := range columns {
						if timeCol != "" && strings.EqualFold(c, timeCol) {
							return nil, &Fault{Code: CodeBadInput, Source: fmt.Sprintf("the record has a field %q, and that is the time column", c)}
						}
					}
				}
				vals := make([]any, len(columns))
				for i, c := range columns {
					vals[i] = jsonValue(rec[c])
				}
				raws, err := json.Marshal(vals)
				if err != nil {
					return nil, &Fault{Code: CodeBadInput, Source: err.Error()}
				}
				env := pgEnvelope{Table: table, Columns: columns, Values: raws, TimeColumn: timeCol, OnConflict: onConflict}
				if timeCol != "" {
					env.Time = time.Now().UTC()
				}
				m := rc.Run.mgr
				if !durable {
					cfg, ok := m.configNode(server)
					if !ok {
						return nil, &Fault{Code: CodeDevice, Source: fmt.Sprintf("the server %s is not running", server)}
					}
					cctx, cancel := context.WithTimeout(ctx, pgWait)
					defer cancel()
					if err := env.insert(cctx, cfg); err != nil {
						return nil, &Fault{Code: CodeDevice, Source: err.Error()}
					}
					return map[string]any{"sent": true}, nil
				}
				box := m.outboxFor(rc.Diagram.ID, rc.Node.ID)
				if box == nil {
					return nil, &Fault{Code: CodeDevice, Source: "a durable output needs the state store, and this runtime has none"}
				}
				body, _ := json.Marshal(env)
				sent, err := box.Put(ctx, body)
				if err != nil {
					code := CodeDevice
					if err == store.ErrQuota {
						code = CodeFile
					}
					return nil, &Fault{Code: code, Source: err.Error()}
				}
				return map[string]any{"sent": sent}, nil
			}), nil
		},
	})
}
