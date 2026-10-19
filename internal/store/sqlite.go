package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	// Pure Go SQLite, no cgo, so the binary stays one static file that builds
	// for every board the image ships on without a C toolchain.
	_ "modernc.org/sqlite"
)

// SQLite is the one database file behind persistent context and durable wires.
//
// One store for both, not two: a second file would be a second thing to size,
// back up, and keep consistent with the first.
type SQLite struct {
	db   *sql.DB
	path string
}

// SQLiteOptions tunes how hard the file is pushed to disk.
type SQLiteOptions struct {
	// Synchronous is "normal" or "full". In WAL mode "normal" does not fsync on
	// every commit: a commit survives the process being killed, and only a
	// power cut can lose the last few. "full" fsyncs every commit, which is the
	// stronger promise and the one that wears an SD card.
	Synchronous string

	// WALAutoCheckpointPages is how many pages the write-ahead log grows to
	// before it is folded back into the database file. Zero uses
	// DefaultWALAutoCheckpointPages.
	WALAutoCheckpointPages int
}

// DefaultWALAutoCheckpointPages is 4000 pages, about 16 MiB. SQLite's own
// default is 1000. A page that gets rewritten many times before a checkpoint is
// written to the file once, so a longer log means fewer writes for the same
// traffic, at the price of a bigger -wal file beside the database.
const DefaultWALAutoCheckpointPages = 4000

// walSizeLimit keeps the log file from staying at its high-water mark forever
// after a burst.
const walSizeLimit = 64 << 20

// OpenSQLite opens, creating if needed, the database at path.
func OpenSQLite(path string, opts SQLiteOptions) (*SQLite, error) {
	if path == "" {
		return nil, fmt.Errorf("the state database needs a path")
	}
	sync := strings.ToLower(opts.Synchronous)
	switch sync {
	case "":
		sync = "normal"
	case "normal", "full":
	default:
		return nil, fmt.Errorf("synchronous %q is not normal or full", opts.Synchronous)
	}
	pages := opts.WALAutoCheckpointPages
	if pages <= 0 {
		pages = DefaultWALAutoCheckpointPages
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating the directory for %s: %w", path, err)
	}

	q := url.Values{}
	// page_size only takes effect on a database that has no tables yet, which is
	// exactly when this first runs. Every commit writes whole pages to the log,
	// so a small page is the difference between a queued message costing a
	// kilobyte and costing sixteen.
	for _, p := range []string{
		"busy_timeout(5000)",
		"page_size(1024)",
		"journal_mode(WAL)",
		"synchronous(" + sync + ")",
		fmt.Sprintf("wal_autocheckpoint(%d)", pages),
		fmt.Sprintf("journal_size_limit(%d)", walSizeLimit),
	} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Opaque: filepath.ToSlash(path)}).String() + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// One connection. SQLite lets one writer in at a time anyway, and a single
	// connection makes "a transaction" mean exactly one thing.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	s := &SQLite{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("preparing %s: %w", path, err)
	}
	return s, nil
}

// Path returns the database file.
func (s *SQLite) Path() string { return s.path }

// Close checkpoints the log and closes the file.
func (s *SQLite) Close() error {
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.db.Close()
}

func (s *SQLite) migrate() error {
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS context (
			scope TEXT NOT NULL,
			key   TEXT NOT NULL,
			value BLOB NOT NULL,
			PRIMARY KEY (scope, key)
		) WITHOUT ROWID`,
		// Keyed by wire then seq, so the oldest message on a wire is the first
		// row of its range and there is one b-tree to write per message, not a
		// table and an index.
		`CREATE TABLE IF NOT EXISTS wire_queue (
			wire TEXT NOT NULL,
			seq  INTEGER NOT NULL,
			body BLOB NOT NULL,
			PRIMARY KEY (wire, seq)
		) WITHOUT ROWID`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}
