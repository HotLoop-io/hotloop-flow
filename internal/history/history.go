// Package history is the deployment log: every deploy, kept as an immutable
// record of who, when, why, and the exact bytes that went live.
//
// "What changed, who changed it, put it back" is the first thing anybody asks
// after a line stops. Three .bak files answer none of it: they say nothing about
// who, they roll over after three saves, and the one you need is always the
// fourth. This package is the answer, and it is an engine package rather than
// an editor feature on purpose, so the API, the CLI and anything else built on
// the engine get the same history.
//
// Records are append-only. A rollback is a new record that happens to carry old
// bytes, never an edit to an old one, so the log can't be made to lie about
// what ran when.
package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// Kind says how a record came about.
type Kind string

const (
	// KindDeploy is a deploy through the API.
	KindDeploy Kind = "deploy"
	// KindRollback is a deploy of an earlier record's flows and credentials.
	KindRollback Kind = "rollback"
	// KindBaseline is what was already on disk when the process started and
	// the log didn't know about it: a first start with history, or a flow file
	// somebody changed by hand. Recorded so the next deploy has something to
	// diff against and to roll back to, and so a hand edit shows up instead of
	// silently becoming the past.
	KindBaseline Kind = "baseline"
)

// DefaultRetain is how many records are kept when the configuration doesn't
// say. A hundred deploys is months of normal work on one line, and a flow file
// is small next to the volume it lives on.
const DefaultRetain = 100

// MaxNoteLength bounds a deploy note. It's a sentence for the next person, not
// a document, and an unbounded string field is an easy way to fill a volume.
const MaxNoteLength = 1000

// Record is one deployment.
type Record struct {
	Seq        int64     `json:"seq"`
	Kind       Kind      `json:"kind"`
	Rev        string    `json:"rev"`
	ParentRev  string    `json:"parentRev,omitempty"`
	User       string    `json:"user,omitempty"`
	Remote     string    `json:"remote,omitempty"`
	Note       string    `json:"note,omitempty"`
	Time       time.Time `json:"time"`
	RollbackOf int64     `json:"rollbackOf,omitempty"`

	// Flows is the flow file exactly as it was written. Credentials is the
	// credential file as it stood after the deploy, in the same format the
	// credential store writes: encrypted when a secret is set. Neither is
	// part of a listing, and the API never hands out Credentials at all.
	Flows       []byte `json:"flows,omitempty"`
	Credentials []byte `json:"credentials,omitempty"`
}

// Meta returns the record without its payloads, which is what a listing shows.
func (r Record) Meta() Record {
	r.Flows, r.Credentials = nil, nil
	return r
}

// ErrNotFound is returned for a sequence number the log doesn't hold, either
// because it never existed or because retention has removed it.
var ErrNotFound = errors.New("no such deployment")

// Log is the deployment log, one file per record under a directory.
type Log struct {
	dir    string
	retain int

	mu    sync.Mutex
	index []Record // metadata, oldest first
	last  int64    // highest sequence number ever seen, kept or not
}

// Open reads the log in dir, creating the directory if needed. retain is how
// many records to keep, and zero keeps every one.
//
// A record that can't be read doesn't stop the process. It is reported in the
// warnings, its sequence number is still counted so it is never reused, and
// everything else in the log stays available.
func Open(dir string, retain int) (*Log, []string, error) {
	if retain < 0 {
		return nil, nil, fmt.Errorf("history retention must not be negative")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	l := &Log{dir: dir, retain: retain}
	var warnings []string
	for _, e := range entries {
		seq, ok := seqFromName(e.Name())
		if !ok || e.IsDir() {
			// Temp files from an interrupted write start with a dot and are
			// never a record. Anything else that isn't ours is left alone.
			continue
		}
		if seq > l.last {
			l.last = seq
		}
		rec, err := readRecord(filepath.Join(dir, e.Name()))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("deployment %d is unreadable and was skipped: %v", seq, err))
			continue
		}
		if rec.Seq != seq {
			warnings = append(warnings, fmt.Sprintf("deployment file %s says it is record %d; skipped", e.Name(), rec.Seq))
			continue
		}
		l.index = append(l.index, rec.Meta())
	}
	sort.Slice(l.index, func(i, j int) bool { return l.index[i].Seq < l.index[j].Seq })
	return l, warnings, nil
}

// Retain reports how many records the log keeps; zero means all of them.
func (l *Log) Retain() int { return l.retain }

// Append writes a new record and returns it with its sequence number and time
// filled in. The write is atomic: a crash leaves either the whole record or no
// record, never half of one.
func (l *Log) Append(r Record) (Record, error) {
	if len(r.Note) > MaxNoteLength {
		return Record{}, fmt.Errorf("a deploy note is limited to %d bytes", MaxNoteLength)
	}
	if r.Kind == "" {
		return Record{}, fmt.Errorf("a deployment record needs a kind")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	r.Seq = l.last + 1
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	path := filepath.Join(l.dir, nameFor(r.Seq))

	// Immutable means an existing record is never replaced. The sequence
	// counter already guarantees a fresh name; this is the belt to that brace,
	// because WriteFileAtomic renames over whatever is there.
	if _, err := os.Lstat(path); err == nil {
		return Record{}, fmt.Errorf("deployment %d already exists; refusing to overwrite it", r.Seq)
	}

	data, err := json.Marshal(r)
	if err != nil {
		return Record{}, fmt.Errorf("serialising deployment %d: %w", r.Seq, err)
	}
	if err := store.WriteFileAtomic(path, data, 0o600); err != nil {
		return Record{}, err
	}

	l.last = r.Seq
	l.index = append(l.index, r.Meta())
	l.pruneLocked()
	return r, nil
}

// pruneLocked removes the oldest records beyond the retention count. The newest
// is never a candidate, whatever the setting says, because the newest is what
// is running.
func (l *Log) pruneLocked() {
	if l.retain <= 0 || len(l.index) <= l.retain {
		return
	}
	drop := len(l.index) - l.retain
	kept := l.index[:0:0]
	for i, rec := range l.index {
		if i < drop {
			// A record that won't delete stays in the index. Pretending it was
			// pruned would leave a file on disk the log no longer admits to.
			if err := os.Remove(filepath.Join(l.dir, nameFor(rec.Seq))); err != nil && !os.IsNotExist(err) {
				kept = append(kept, rec)
			}
			continue
		}
		kept = append(kept, rec)
	}
	l.index = kept
}

// List returns record metadata, newest first. A limit of zero or less returns
// everything the log holds.
func (l *Log) List(limit int) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.index)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Record, 0, limit)
	for i := n - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, l.index[i])
	}
	return out
}

// Latest returns the newest record's metadata.
func (l *Log) Latest() (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.index) == 0 {
		return Record{}, false
	}
	return l.index[len(l.index)-1], true
}

// Get returns a whole record, payloads included.
func (l *Log) Get(seq int64) (Record, error) {
	l.mu.Lock()
	found := false
	for _, r := range l.index {
		if r.Seq == seq {
			found = true
			break
		}
	}
	l.mu.Unlock()
	if !found {
		return Record{}, fmt.Errorf("%w: %d", ErrNotFound, seq)
	}
	return readRecord(filepath.Join(l.dir, nameFor(seq)))
}

// FindRev returns the newest record that put a given revision live.
func (l *Log) FindRev(rev string) (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.index) - 1; i >= 0; i-- {
		if l.index[i].Rev == rev {
			return l.index[i], true
		}
	}
	return Record{}, false
}

func readRecord(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	return r, nil
}

// Ten digits sorts correctly as text in a directory listing for longer than
// any line will run, which is what an operator looking at the volume sees.
func nameFor(seq int64) string { return fmt.Sprintf("%010d.json", seq) }

func seqFromName(name string) (int64, bool) {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok || len(base) == 0 || strings.HasPrefix(name, ".") {
		return 0, false
	}
	n, err := strconv.ParseInt(base, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
