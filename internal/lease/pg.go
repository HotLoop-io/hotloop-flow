package lease

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres keeps the lease in one row of a table of its own. The expiry is the
// database's clock, so two Flow boxes whose clocks disagree still agree on who
// holds it.
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects with a connection URL and makes the table if it is not
// there. The pool is small and gives up connecting quickly: a lease that cannot be
// renewed has to say so before it runs out.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	url = strings.TrimSpace(url)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("the lease database address is not one: %w", err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	p := &Postgres{pool: pool}
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS hotloop_flow_lease (
		name       text PRIMARY KEY,
		holder     text NOT NULL,
		epoch      bigint NOT NULL,
		expires_at timestamptz NOT NULL)`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("making the lease table: %w", err)
	}
	return p, nil
}

// Close closes the pool.
func (p *Postgres) Close() { p.pool.Close() }

// Acquire is the whole protocol in one statement. The row is the lease: it is
// taken when it is not there, when this holder already has it, or when its
// expiry has passed. The number goes up only when the holder changes.
func (p *Postgres) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (Grant, error) {
	var g Grant
	err := p.pool.QueryRow(ctx, `
		INSERT INTO hotloop_flow_lease AS l (name, holder, epoch, expires_at)
		VALUES ($1, $2, 1, now() + make_interval(secs => $3))
		ON CONFLICT (name) DO UPDATE
		   SET holder = EXCLUDED.holder,
		       epoch = CASE WHEN l.holder = EXCLUDED.holder THEN l.epoch ELSE l.epoch + 1 END,
		       expires_at = EXCLUDED.expires_at
		 WHERE l.holder = EXCLUDED.holder OR l.expires_at < now()
		RETURNING epoch, holder`, name, holder, ttl.Seconds()).Scan(&g.Epoch, &g.Holder)
	if err == nil {
		g.Held = true
		return g, nil
	}
	if err != pgx.ErrNoRows {
		return Grant{}, err
	}
	// Somebody else has it and it has not run out: say who.
	if err := p.pool.QueryRow(ctx, `SELECT epoch, holder FROM hotloop_flow_lease WHERE name = $1`, name).Scan(&g.Epoch, &g.Holder); err != nil {
		return Grant{}, err
	}
	return g, nil
}

// Release gives the lease up early, but only if holder has it.
func (p *Postgres) Release(ctx context.Context, name, holder string) error {
	_, err := p.pool.Exec(ctx, `UPDATE hotloop_flow_lease SET expires_at = now() - interval '1 second' WHERE name = $1 AND holder = $2`, name, holder)
	return err
}
