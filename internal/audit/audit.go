// Package audit is the record of who did what to this instance, and from
// where: logins and failed logins, deploys and refused deploys, rollbacks,
// injects, and anything else that changes what a plant floor is running.
//
// Before it, the whole trail was a "failed login" line in a log nobody keeps.
// An engine that can command equipment needs better than that. When something
// goes wrong at 3 AM the first questions are who touched it and from which
// machine, and "nobody knows" is not an answer anybody accepts twice.
//
// The deployment log is the audit trail's first table: every deploy and
// rollback here points at its deployment record, which holds the bytes.
//
// One JSON object per line, appended and synced to disk before the action it
// records is reported as done, rotated by size so a busy instance can't fill
// its volume.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Events.
const (
	Login          = "login"
	LoginFailed    = "login.failed"
	Logout         = "logout"
	Deploy         = "deploy"
	DeployRefused  = "deploy.refused"
	Rollback       = "rollback"
	RollbackFailed = "rollback.refused"
	Inject         = "inject"
	MFAEnabled     = "mfa.enabled"
	MFADisabled    = "mfa.disabled"
	MFAReset       = "mfa.reset"
	// MFAFailed sits under login. so ?event=login. shows every way a sign-in
	// went wrong in one list.
	MFAFailed = "login.mfa_failed"
)

// Entry is one thing that happened.
type Entry struct {
	Time  time.Time `json:"time"`
	Event string    `json:"event"`
	// User is who did it: a username, token:<name> for an API token, or empty
	// when authentication is off. For a failed login it is the name that was
	// tried, which is the interesting part.
	User string `json:"user,omitempty"`
	// Remote is the address the request came from, as the socket saw it.
	Remote string `json:"remote,omitempty"`
	// ForwardedFor is the X-Forwarded-For header exactly as it arrived. A
	// proxy sets it and so can anybody else, so it is kept apart from Remote
	// and labelled for what it is: a claim.
	ForwardedFor string `json:"forwardedFor,omitempty"`
	// Detail is the specifics: which deployment, which node, why it failed.
	Detail map[string]any `json:"detail,omitempty"`
}

// Defaults.
const (
	DefaultMaxBytes = 16 << 20
	DefaultKeep     = 4
)

// Log is the audit trail on disk.
type Log struct {
	path     string
	maxBytes int64
	keep     int

	mu       sync.Mutex
	f        *os.File
	size     int64
	failures int64
}

// Open opens (or creates) the trail at path. maxBytes is the size at which
// the file rotates, keep is how many rotated files are kept.
func Open(path string, maxBytes int64, keep int) (*Log, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if keep < 1 {
		keep = DefaultKeep
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Log{path: path, maxBytes: maxBytes, keep: keep}
	if err := l.openFile(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) openFile() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening the audit log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, info.Size()
	return nil
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Record appends an entry and syncs it to disk. It never stops the thing being
// recorded: an audit trail that can't be written is counted, returned, and
// shouts in the process log, but a line doesn't stop over it.
func (l *Log) Record(e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		l.fail()
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		l.failures++
		return fmt.Errorf("the audit log is closed")
	}
	if l.size+int64(len(line)) > l.maxBytes && l.size > 0 {
		if err := l.rotateLocked(); err != nil {
			l.failures++
			return err
		}
	}
	n, err := l.f.Write(line)
	l.size += int64(n)
	if err == nil {
		err = l.f.Sync()
	}
	if err != nil {
		l.failures++
		return fmt.Errorf("writing the audit log: %w", err)
	}
	return nil
}

func (l *Log) fail() {
	l.mu.Lock()
	l.failures++
	l.mu.Unlock()
}

// Failures counts entries that could not be written.
func (l *Log) Failures() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failures
}

// rotateLocked shifts audit.log.N up by one and starts a fresh file.
func (l *Log) rotateLocked() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	l.f = nil
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.keep))
	for i := l.keep - 1; i >= 1; i-- {
		from, to := fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return l.openFile()
}

// Query filters a read of the trail.
type Query struct {
	// Event matches exactly, or a prefix ending in "." ("login." matches
	// login.failed). Empty matches everything.
	Event string
	// User matches exactly. Empty matches everybody.
	User string
	// Since drops anything older.
	Since time.Time
	// Limit bounds how many come back. Zero means 200.
	Limit int
}

// Read returns matching entries, newest first, across the current file and
// the rotated ones.
func (l *Log) Read(q Query) ([]Entry, error) {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	l.mu.Lock()
	keep := l.keep
	l.mu.Unlock()

	files := []string{l.path}
	for i := 1; i <= keep; i++ {
		files = append(files, fmt.Sprintf("%s.%d", l.path, i))
	}

	var out []Entry
	for _, name := range files {
		entries, err := readFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if !q.Since.IsZero() && e.Time.Before(q.Since) {
				return out, nil
			}
			if !matches(e, q) {
				continue
			}
			out = append(out, e)
			if len(out) >= q.Limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func matches(e Entry, q Query) bool {
	if q.User != "" && e.User != q.User {
		return false
	}
	switch {
	case q.Event == "":
	case strings.HasSuffix(q.Event, "."):
		if !strings.HasPrefix(e.Event, q.Event) {
			return false
		}
	default:
		if e.Event != q.Event {
			return false
		}
	}
	return true
}

func readFile(name string) ([]Entry, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var e Entry
		// A torn last line from a crash mid-write is skipped rather than
		// failing the whole read; every complete line before it still counts.
		if err := json.Unmarshal(sc.Bytes(), &e); err == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
