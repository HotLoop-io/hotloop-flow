package nodes

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// This is the way a FlowLab diagram reaches a running hotloop-flow-postgres
// config node: the same pool, credentials, TLS and network interface the postgres
// node uses, so a diagram never holds a second set of them.

// ErrInsertUnconfirmed marks an insert that was sent and then did not report back
// before the time ran out. The database may have committed it, so a caller that
// keeps the row and sends it again can write it twice. An insert refused before it
// was sent, because the pool could not connect, never carries it.
var ErrInsertUnconfirmed = errors.New("the database did not confirm the insert, and it may have been written")

// CheckPostgresTarget checks a table and its columns the way the postgres node
// does, so a diagram refuses a name that is not an identifier at deploy.
func CheckPostgresTarget(table string, columns []string) error {
	if err := validateSQLIdentifier(table); err != nil {
		return fmt.Errorf("table %q %w", table, err)
	}
	if len(columns) == 0 {
		return fmt.Errorf("no columns")
	}
	seen := map[string]bool{}
	for _, c := range columns {
		if strings.Contains(c, ".") {
			return fmt.Errorf("column %q has a dot in it", c)
		}
		if err := validateSQLIdentifier(c); err != nil {
			return fmt.Errorf("column %q %w", c, err)
		}
		k := strings.ToLower(c)
		if seen[k] {
			return fmt.Errorf("column %q is listed twice", c)
		}
		seen[k] = true
	}
	return nil
}

// ErrRowRejected marks a row the database itself refused for what is in it: a
// value that does not fit its column, or a key that is already there. The same
// row will be refused every time, so a caller that keeps rows to send later must
// not keep this one. A missing table, a wrong password and a link that is down are
// not this: they can be put right, and the row waits.
var ErrRowRejected = errors.New("the database refused the row")

// MaxInsertRows is the most rows InsertPostgresRows takes at once, so a statement
// stays under the protocol's limit of 65,535 parameters whatever the columns.
func MaxInsertRows(columns int) int {
	if columns < 1 {
		return 1
	}
	n := 65535 / columns
	if n > 1000 {
		n = 1000
	}
	if n < 1 {
		n = 1
	}
	return n
}

// InsertPostgres writes one row through a running hotloop-flow-postgres config
// node. onConflict is "error" or "nothing", the same choices the postgres node
// has. A connection that is down is an error now, never a row parked in memory:
// the caller decides whether to keep it, and a durable output writes it to disk.
func InsertPostgres(ctx context.Context, cfg node.Node, table string, columns []string, values []any, onConflict string) error {
	return InsertPostgresRows(ctx, cfg, table, columns, [][]any{values}, onConflict)
}

// InsertPostgresRows writes rows in one statement, so they all land or none do. A
// backlog that waited for the database is written this way: one round trip and one
// commit for hundreds of rows, where a row at a time falls behind a plant that
// keeps sending.
func InsertPostgresRows(ctx context.Context, cfg node.Node, table string, columns []string, rows [][]any, onConflict string) error {
	p, ok := cfg.(PoolProvider)
	if !ok {
		return fmt.Errorf("the config node is not a PostgreSQL server")
	}
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > MaxInsertRows(len(columns)) {
		return fmt.Errorf("%d rows at once is more than a statement can carry", len(rows))
	}
	for _, r := range rows {
		if len(columns) != len(r) {
			return fmt.Errorf("%d columns and %d values", len(columns), len(r))
		}
	}
	if err := CheckPostgresTarget(table, columns); err != nil {
		return err
	}
	if onConflict != "" && onConflict != "error" && onConflict != "nothing" {
		return fmt.Errorf("on conflict must be error or nothing, got %q", onConflict)
	}
	pool, err := p.Pool(ctx)
	if err != nil {
		return err
	}
	args := make([]any, 0, len(rows)*len(columns))
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" (")
	b.WriteString(strings.Join(columns, ", "))
	b.WriteString(") VALUES ")
	for ri, r := range rows {
		if ri > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for i, v := range r {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(len(args) + 1))
			args = append(args, pgValue(v))
		}
		b.WriteByte(')')
	}
	if onConflict == "nothing" {
		b.WriteString(" ON CONFLICT DO NOTHING")
	}
	if _, err := pool.Exec(ctx, b.String(), args...); err != nil {
		var ce *pgconn.ConnectError
		if errors.As(err, &ce) {
			// It never reached the server, so it cannot have been written.
			return fmt.Errorf("insert into %s: %w", table, err)
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) {
			if strings.HasPrefix(pe.Code, "22") || strings.HasPrefix(pe.Code, "23") {
				// The server answered, and it answered no, to the row itself.
				return fmt.Errorf("insert into %s: %w (%w)", table, err, ErrRowRejected)
			}
			// The server answered, and nothing was written.
			return fmt.Errorf("insert into %s: %w", table, err)
		}
		// The statement was sent and the answer never came back, by a timeout or a
		// link that died under it. The server may have committed it.
		return fmt.Errorf("insert into %s: %w (%w)", table, err, ErrInsertUnconfirmed)
	}
	return nil
}

// pgValue turns a value a diagram holds into one the driver can encode into any
// column type. A diagram's numbers are float64, so a whole one is handed over as
// an integer: the driver then takes it for an integer, float or numeric column
// alike, where a float64 would be refused by an integer column. A record or an
// array goes in as it is, which is what a json or jsonb column wants.
func pgValue(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
		return x
	case time.Time:
		return x
	default:
		return v
	}
}
