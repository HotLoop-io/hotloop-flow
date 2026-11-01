package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
	"github.com/HotLoop-io/hotloop-flow/internal/store"
)

// tokenStore holds sign-in sessions.
//
// Sessions survive a restart. They used to live only in memory, so every pod
// reschedule and every upgrade signed everybody out, which on a plant floor
// means somebody who needs to change a flow right now is hunting for a
// password instead. They are written to data.dir/sessions.json, and only as
// SHA-256 hashes of the tokens: the file proves a token is valid when one is
// presented, and is no use at all to whoever copies it off the volume.
//
// A session remembers the username, not the permissions. Permissions come
// from the configuration every time, so taking a permission away, or the user,
// takes effect on the next request rather than whenever the session expires.
type tokenStore struct {
	mu     sync.Mutex
	ttl    time.Duration
	path   string // empty keeps sessions in memory only
	find   func(string) (config.User, bool)
	log    *slog.Logger
	issued map[string]session // keyed by the token's hash

	// readFile reads the sessions file; nil is os.ReadFile. A test swaps it.
	readFile func(string) ([]byte, error)
	// unread is why the file could not be read at start, and holds writes off.
	unread error
}

type session struct {
	User    string    `json:"user"`
	Expires time.Time `json:"expires"`
}

func newTokenStore(ttl time.Duration, path string, find func(string) (config.User, bool), log *slog.Logger) *tokenStore {
	return newTokenStoreReading(ttl, path, find, log, nil)
}

// newTokenStoreReading is newTokenStore with the file reader named; nil is
// os.ReadFile. A test uses it to make the first read fail.
func newTokenStoreReading(ttl time.Duration, path string, find func(string) (config.User, bool), log *slog.Logger, read func(string) ([]byte, error)) *tokenStore {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	t := &tokenStore{ttl: ttl, path: path, find: find, log: log, issued: map[string]session{}, readFile: read}
	t.load()
	return t
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (t *tokenStore) load() {
	if t.path == "" {
		return
	}
	read := t.readFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(t.path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		// The file is there and could not be read (a volume that is not up
		// yet, EIO, permissions). Losing the in-memory view signs people out,
		// but overwriting the file as well would lose the sessions for good, and
		// the volume may be fine a moment later. So: serve with what can be
		// read (nothing), leave the file alone, say so loudly, and look again
		// before any write (saveLocked).
		t.unread = err
		t.log.Error("sign-in sessions could not be read; everybody signs in again until the file can be read, and it will not be overwritten before then", "path", t.path, "error", err)
		return
	}
	if err := json.Unmarshal(data, &t.issued); err != nil {
		// Unreadable content is not coming back by waiting. Keep it beside the
		// original rather than writing over it, and start fresh.
		aside := t.path + ".unreadable-" + time.Now().UTC().Format("20060102T150405Z")
		if werr := os.WriteFile(aside, data, 0o600); werr != nil {
			t.unread = werr
			t.log.Error("sign-in sessions are unreadable and could not be set aside; the file will not be overwritten", "path", t.path, "error", err, "aside_error", werr)
		} else {
			t.log.Warn("sign-in sessions are unreadable; kept a copy and everybody signs in again", "path", t.path, "copy", aside, "error", err)
		}
		t.issued = map[string]session{}
		return
	}
	t.mu.Lock()
	t.sweepLocked()
	t.mu.Unlock()
}

// unreadLocked says whether the sessions file may be written. After a read that
// failed, it looks again: if the file reads now, its sessions are merged in and
// writing is fine; if not, nothing is written, so a file that is only
// temporarily unreadable is never replaced by the sessions of one process.
func (t *tokenStore) unreadLocked() bool {
	if t.unread == nil {
		return false
	}
	read := t.readFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(t.path)
	if os.IsNotExist(err) {
		t.unread = nil
		return false
	}
	if err != nil {
		t.log.Error("sign-in sessions still cannot be read; not overwriting the file", "path", t.path, "error", err)
		return true
	}
	var onDisk map[string]session
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.log.Error("sign-in sessions on disk are unreadable; not overwriting the file", "path", t.path, "error", err)
		return true
	}
	now := time.Now()
	for k, s := range onDisk {
		if _, have := t.issued[k]; !have && now.Before(s.Expires) {
			t.issued[k] = s
		}
	}
	t.unread = nil
	return false
}

// saveLocked writes the sessions. A failed write is logged and the session
// still works until the next restart, because refusing a sign-in over a full
// disk helps nobody.
func (t *tokenStore) saveLocked() {
	if t.path == "" || t.unreadLocked() {
		return
	}
	data, err := json.Marshal(t.issued)
	if err == nil {
		err = store.WriteFileAtomic(t.path, data, 0o600)
	}
	if err != nil {
		t.log.Error("sign-in sessions could not be saved; they won't survive a restart", "error", err)
	}
}

func (t *tokenStore) issue(u config.User) (string, time.Time) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform we ship to. If it somehow
		// did, issuing a predictable token would be far worse than panicking.
		panic("hotloop-flow: crypto/rand failed while issuing a token: " + err.Error())
	}
	tok := hex.EncodeToString(b[:])
	expires := time.Now().Add(t.ttl)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	t.issued[hashToken(tok)] = session{User: u.Username, Expires: expires}
	t.saveLocked()
	return tok, expires
}

// lookup resolves a token to the user as the configuration has them now.
func (t *tokenStore) lookup(tok string) (config.User, bool) {
	if tok == "" {
		return config.User{}, false
	}
	t.mu.Lock()
	s, ok := t.issued[hashToken(tok)]
	t.mu.Unlock()
	if !ok || time.Now().After(s.Expires) {
		return config.User{}, false
	}
	return t.find(s.User)
}

func (t *tokenStore) revoke(tok string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := hashToken(tok)
	if _, ok := t.issued[key]; ok {
		delete(t.issued, key)
		t.saveLocked()
	}
}

// sweepLocked drops expired sessions. Called on issue rather than on a timer,
// so an idle instance holds no goroutine for it.
func (t *tokenStore) sweepLocked() {
	now := time.Now()
	for k, s := range t.issued {
		if now.After(s.Expires) {
			delete(t.issued, k)
		}
	}
}
