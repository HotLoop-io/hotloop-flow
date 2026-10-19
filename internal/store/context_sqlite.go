package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/metrics"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// DefaultContextFlushInterval is how long a changed context value may wait
// before it is written. Thirty seconds, the same as Node-RED's file store. It
// was five until I measured it: a counter bumped twenty times a second cost a
// flash card 5.1 MiB an hour at five seconds and 1.7 at thirty.
const DefaultContextFlushInterval = 30 * time.Second

// PersistentContexts is context that survives a restart.
//
// Every scope is a MemoryContext loaded from SQLite when the scope is first
// used, so reads never touch the disk and CompareAndSwap, Increment and Update
// are the same code as the memory store, atomic under the same lock.
//
// Writes are behind: a change is held, and the changes from one interval go to
// the file in one transaction. A counter bumped ten times a second costs one
// write per interval, not ten, and a value set back to what it already was
// costs none. That is the SD card argument. The price is that a kill -9 can
// lose up to one interval of changes. A clean stop writes everything first.
// With a zero interval every change is written before the call returns.
type PersistentContexts struct {
	sql      *SQLite
	interval time.Duration

	mu    sync.Mutex
	dirty map[ctxKey][]byte // nil value means delete

	flushMu sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	stopped sync.Once

	flushes       atomic.Int64
	flushFailures atomic.Int64
	rowsWritten   atomic.Int64
}

type ctxKey struct{ scope, key string }

// NewPersistentContexts starts the flusher. Close it to write the last changes.
func NewPersistentContexts(db *SQLite, flushInterval time.Duration) *PersistentContexts {
	p := &PersistentContexts{
		sql:      db,
		interval: flushInterval,
		dirty:    map[ctxKey][]byte{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if flushInterval > 0 {
		go p.loop()
	} else {
		close(p.done)
	}
	return p
}

// Scoped returns the scope set to hand the runtime.
func (p *PersistentContexts) Scoped() *ScopedContexts {
	s := NewScopedContextsWith(func(scope string) node.Context { return p.open(scope) })
	s.onDrop = p.forgetScope
	s.onClean = func(keepNode, keepFlow func(string) bool) {
		p.forgetScopesWhere(func(scope string) bool {
			switch {
			case strings.HasPrefix(scope, "node:"):
				return !keepNode(strings.TrimPrefix(scope, "node:"))
			case strings.HasPrefix(scope, "flow:"):
				return !keepFlow(strings.TrimPrefix(scope, "flow:"))
			}
			return false
		})
	}
	return s
}

// open loads one scope.
func (p *PersistentContexts) open(scope string) *MemoryContext {
	m := NewMemoryContext()
	rows, err := p.sql.db.Query(`SELECT key, value FROM context WHERE scope = ?`, scope)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var key string
			var raw []byte
			if rows.Scan(&key, &raw) != nil {
				continue
			}
			v, err := DecodeValue(raw)
			if err != nil {
				// A value that won't decode is skipped, and it stays in the
				// file, so a bad row is a missing key and not a failed start.
				continue
			}
			m.data[key] = v
		}
	}
	m.onChange = func(key string, value any, deleted bool) error {
		return p.record(scope, key, value, deleted)
	}
	return m
}

func (p *PersistentContexts) record(scope, key string, value any, deleted bool) error {
	var enc []byte
	if !deleted {
		var err error
		enc, err = EncodeValue(value)
		if err != nil {
			return fmt.Errorf("context %s key %q: %w", scope, key, err)
		}
	}
	if p.interval <= 0 {
		return p.write(map[ctxKey][]byte{{scope, key}: enc})
	}
	p.mu.Lock()
	p.dirty[ctxKey{scope, key}] = enc
	p.mu.Unlock()
	return nil
}

func (p *PersistentContexts) loop() {
	defer close(p.done)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = p.Flush()
		case <-p.stop:
			return
		}
	}
}

// Flush writes every held change.
func (p *PersistentContexts) Flush() error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	p.mu.Lock()
	batch := p.dirty
	p.dirty = map[ctxKey][]byte{}
	p.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if err := p.write(batch); err != nil {
		// Put it back, without trampling anything changed since.
		p.mu.Lock()
		for k, v := range batch {
			if _, newer := p.dirty[k]; !newer {
				p.dirty[k] = v
			}
		}
		p.mu.Unlock()
		p.flushFailures.Add(1)
		return err
	}
	return nil
}

func (p *PersistentContexts) write(batch map[ctxKey][]byte) error {
	tx, err := p.sql.db.Begin()
	if err != nil {
		return err
	}
	var changed int64
	for k, v := range batch {
		var res sql.Result
		if v == nil {
			res, err = tx.Exec(`DELETE FROM context WHERE scope = ? AND key = ?`, k.scope, k.key)
		} else {
			// WHERE value IS NOT excluded.value: a value written back as what
			// it already was changes nothing, and a transaction that changed
			// nothing writes nothing.
			res, err = tx.Exec(`INSERT INTO context (scope, key, value) VALUES (?, ?, ?)
				ON CONFLICT (scope, key) DO UPDATE SET value = excluded.value
				WHERE value IS NOT excluded.value`, k.scope, k.key, v)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed += n
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.flushes.Add(1)
	p.rowsWritten.Add(changed)
	return nil
}

// forgetScope drops a scope's rows, for a node that was deleted.
func (p *PersistentContexts) forgetScope(scope string) {
	p.forgetScopesWhere(func(s string) bool { return s == scope })
}

// forgetScopesWhere removes the stored scopes the predicate selects, along with
// any change still waiting to be written to them. Node-RED sweeps the context
// of deleted nodes the same way, and without it every node ever deployed would
// leave its keys in the file for good.
func (p *PersistentContexts) forgetScopesWhere(match func(scope string) bool) {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	p.mu.Lock()
	for k := range p.dirty {
		if match(k.scope) {
			delete(p.dirty, k)
		}
	}
	p.mu.Unlock()

	rows, err := p.sql.db.Query(`SELECT DISTINCT scope FROM context`)
	if err != nil {
		return
	}
	var doomed []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil && match(s) {
			doomed = append(doomed, s)
		}
	}
	rows.Close()
	for _, s := range doomed {
		_, _ = p.sql.db.Exec(`DELETE FROM context WHERE scope = ?`, s)
	}
}

// Close stops the flusher and writes what is left.
func (p *PersistentContexts) Close() error {
	p.stopped.Do(func() { close(p.stop) })
	<-p.done
	return p.Flush()
}

// Families are the metrics this store contributes.
func (p *PersistentContexts) Families() []metrics.Family {
	return []metrics.Family{
		{
			Name: "hotloop_flow_context_flushes_total", Type: "counter",
			Help:    "Transactions that wrote context changes to the state database.",
			Collect: func() []metrics.Sample { return []metrics.Sample{{Value: float64(p.flushes.Load())}} },
		},
		{
			Name: "hotloop_flow_context_rows_written_total", Type: "counter",
			Help:    "Context rows actually changed on disk. A value set to what it already was does not count.",
			Collect: func() []metrics.Sample { return []metrics.Sample{{Value: float64(p.rowsWritten.Load())}} },
		},
		{
			Name: "hotloop_flow_context_flush_failures_total", Type: "counter",
			Help:    "Context flushes that failed. The changes are kept and tried again.",
			Collect: func() []metrics.Sample { return []metrics.Sample{{Value: float64(p.flushFailures.Load())}} },
		},
	}
}
