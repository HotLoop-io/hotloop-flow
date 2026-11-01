package api

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HotLoop-io/hotloop-flow/internal/config"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func findUser(name string) (config.User, bool) { return config.User{Username: name}, true }

// A sessions file that is there and cannot be read for a moment (a volume that
// is not up, an I/O error) must not be replaced by the sessions of one process.
// Starting empty signs people in again; overwriting as well loses the file for
// good. The file is left alone until it reads, and then its sessions are back.
func TestAnUnreadableSessionsFileIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	first := newTokenStore(0, path, findUser, quiet())
	tok, _ := first.issue(config.User{Username: "ada"})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	failing := true
	read := func(p string) ([]byte, error) {
		if failing {
			return nil, errors.New("input/output error")
		}
		return os.ReadFile(p)
	}
	second := newTokenStoreReading(0, path, findUser, quiet(), read)
	if _, ok := second.lookup(tok); ok {
		t.Fatal("a session was found while the file could not be read")
	}
	// A new sign-in during the outage must not write over the file.
	second.issue(config.User{Username: "grace"})
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("the sessions file was overwritten while it could not be read")
	}

	// Once it reads, the old sessions are back and the file keeps both.
	failing = false
	late, _ := second.issue(config.User{Username: "linus"})
	if _, ok := second.lookup(tok); !ok {
		t.Fatal("the session on disk did not come back once the file could be read")
	}
	if _, ok := second.lookup(late); !ok {
		t.Fatal("a session issued after recovery is missing")
	}
	third := newTokenStore(0, path, findUser, quiet())
	if _, ok := third.lookup(tok); !ok {
		t.Fatal("the original session was lost from the file")
	}
}

// A file whose content is not JSON is kept beside the original, not written
// over.
func TestACorruptSessionsFileIsKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTokenStore(0, path, findUser, quiet())
	s.issue(config.User{Username: "ada"})
	matches, _ := filepath.Glob(filepath.Join(dir, "sessions.json.unreadable-*"))
	if len(matches) != 1 {
		t.Fatalf("expected one kept copy, found %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); !strings.Contains(string(b), "{not json") {
		t.Fatal("the kept copy is not the original content")
	}
}
