package nodes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// pgConfigFor builds a real hotloop-flow-postgres config node at addr and closes
// it when the test ends.
func pgConfigFor(t *testing.T, addr string) node.Node {
	t.Helper()
	host, port := splitHostPort(t, addr)
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.creds["password"] = "flowtest"
	cfg := build(t, "hotloop-flow-postgres", `{"host":"`+host+`","port":`+port+`,"database":"flowtest","user":"flow","sslmode":"disable","maxConns":2}`, svc)
	t.Cleanup(func() { _ = cfg.(node.Closer).Close(context.Background(), false) })
	return cfg
}

func TestCheckPostgresTargetRefusesWhatIsNotAnIdentifier(t *testing.T) {
	for _, c := range []struct {
		table string
		cols  []string
		want  string
	}{
		{"public.readings", []string{"n", "sensor"}, ""},
		{"readings; drop table x", []string{"n"}, "only letters"},
		{"readings", []string{"n", "n"}, "listed twice"},
		{"readings", []string{"a.b"}, "dot"},
		{"readings", []string{"x y"}, "only letters"},
		{"readings", nil, "no columns"},
		{"", []string{"n"}, "is empty"},
	} {
		err := CheckPostgresTarget(c.table, c.cols)
		if c.want == "" && err != nil {
			t.Errorf("%q %v: %v", c.table, c.cols, err)
		}
		if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%q %v: error %v, want one with %q", c.table, c.cols, err, c.want)
		}
	}
}

func TestPgValueSendsAWholeNumberAsAnInteger(t *testing.T) {
	if v, ok := pgValue(5.0).(int64); !ok || v != 5 {
		t.Errorf("5.0 became %#v", pgValue(5.0))
	}
	if v, ok := pgValue(2.5).(float64); !ok || v != 2.5 {
		t.Errorf("2.5 became %#v", pgValue(2.5))
	}
	if v, ok := pgValue(1e300).(float64); !ok || v != 1e300 {
		t.Errorf("1e300 became %#v", pgValue(1e300))
	}
	if pgValue("x") != "x" || pgValue(nil) != nil {
		t.Error("text and null must pass through")
	}
}

// A real database: every kind of value a diagram holds lands in the column that
// fits it, a duplicate is an error or is skipped as asked, and a server nobody
// listens on is an error that cannot have written the row.
func TestInsertPostgresAgainstARealDatabase(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_PG")
	cfg := pgConfigFor(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	host, port := splitHostPort(t, addr)
	conn, err := pgx.Connect(ctx, "host="+host+" port="+port+" dbname=flowtest user=flow password=flowtest sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	table := "flowlab_bridge_" + strings.ToLower(strings.ReplaceAll(time.Now().Format("150405.000"), ".", ""))
	if _, err := conn.Exec(ctx, `CREATE TABLE `+table+` (id int PRIMARY KEY, label text, f double precision, big bigint, ok boolean, doc jsonb, made timestamptz)`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `DROP TABLE `+table)

	when := time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC)
	cols := []string{"id", "label", "f", "big", "ok", "doc", "made"}
	vals := []any{1.0, "dryer 1", 2.5, 9007199254740.0, true, map[string]any{"a": 1.0, "b": []any{1.0, "x"}}, when}
	if err := InsertPostgres(ctx, cfg, table, cols, vals, "error"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var id, big int64
	var label string
	var f float64
	var ok bool
	var doc string
	var made time.Time
	if err := conn.QueryRow(ctx, `SELECT id, label, f, big, ok, doc::text, made FROM `+table).Scan(&id, &label, &f, &big, &ok, &doc, &made); err != nil {
		t.Fatal(err)
	}
	if id != 1 || label != "dryer 1" || f != 2.5 || big != 9007199254740 || !ok || !made.Equal(when) || !strings.Contains(doc, `"a": 1`) {
		t.Errorf("row = %d %q %v %d %v %s %v", id, label, f, big, ok, doc, made)
	}

	// A null is a null, and a whole number goes into a float column.
	if err := InsertPostgres(ctx, cfg, table, []string{"id", "label", "f"}, []any{2.0, nil, 3.0}, "error"); err != nil {
		t.Fatalf("null and whole float: %v", err)
	}

	// The same key twice.
	err = InsertPostgres(ctx, cfg, table, []string{"id"}, []any{1.0}, "error")
	if err == nil || errors.Is(err, ErrInsertUnconfirmed) || !errors.Is(err, ErrRowRejected) {
		t.Errorf("a duplicate key gave %v, want an error the database refused the row with", err)
	}
	// A table that is not there is a thing to put right: the row waits, it is not refused.
	err = InsertPostgres(ctx, cfg, table+"_missing", []string{"id"}, []any{1.0}, "error")
	if err == nil || errors.Is(err, ErrRowRejected) || errors.Is(err, ErrInsertUnconfirmed) {
		t.Errorf("a missing table gave %v, want an error that is neither a refusal nor unconfirmed", err)
	}
	// Rows go in one statement, all or none: one bad row takes nothing in.
	err = InsertPostgresRows(ctx, cfg, table, []string{"id", "label"}, [][]any{{10.0, "a"}, {11.0, "b"}, {1.0, "dup"}, {12.0, "c"}}, "error")
	if !errors.Is(err, ErrRowRejected) {
		t.Errorf("a batch with a duplicate gave %v", err)
	}
	var left int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE id >= 10`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows of a refused batch were written (%v), want none", left, err)
	}
	if err := InsertPostgresRows(ctx, cfg, table, []string{"id", "label"}, [][]any{{10.0, "a"}, {11.0, "b"}, {12.0, "c"}}, "error"); err != nil {
		t.Fatalf("a batch: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE id >= 10`).Scan(&left); err != nil || left != 3 {
		t.Errorf("%d rows of the batch were written (%v), want 3", left, err)
	}
	if err := InsertPostgres(ctx, cfg, table, []string{"id"}, []any{1.0}, "nothing"); err != nil {
		t.Errorf("a duplicate with skip: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE id < 10`).Scan(&n); err != nil || n != 2 {
		t.Errorf("rows = %d (%v), want 2", n, err)
	}

	// An identifier that is not one never reaches the server.
	if err := InsertPostgres(ctx, cfg, table+"; drop table "+table, []string{"id"}, []any{3.0}, "error"); err == nil {
		t.Error("an injected table name was accepted")
	}
	if err := InsertPostgres(ctx, cfg, table, []string{"id", "label"}, []any{3.0}, "error"); err == nil {
		t.Error("two columns and one value were accepted")
	}
}

// A server nobody listens on is refused before the row is sent, so it can never
// have been written and must not be counted as a possible duplicate.
func TestInsertPostgresToADeadServerIsNotUnconfirmed(t *testing.T) {
	cfg := pgConfigFor(t, "127.0.0.1:1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := InsertPostgres(ctx, cfg, "t", []string{"id"}, []any{1.0}, "error")
	if err == nil {
		t.Fatal("an insert into nothing succeeded")
	}
	if errors.Is(err, ErrInsertUnconfirmed) {
		t.Errorf("a connection refused counted as unconfirmed: %v", err)
	}
}
