package history

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T, dir string, retain int) *Log {
	t.Helper()
	l, warnings, err := Open(dir, retain)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) > 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	return l
}

func appendDeploy(t *testing.T, l *Log, user, rev string, flows string) Record {
	t.Helper()
	r, err := l.Append(Record{Kind: KindDeploy, User: user, Rev: rev, Flows: []byte(flows), Credentials: []byte(`{"format":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAppendListAndGet(t *testing.T) {
	l := open(t, t.TempDir(), 0)
	appendDeploy(t, l, "dana", "r1", "[1]")
	appendDeploy(t, l, "sam", "r2", "[2]")
	third := appendDeploy(t, l, "dana", "r3", "[\n    3\n]\n")

	list := l.List(0)
	if len(list) != 3 {
		t.Fatalf("listed %d records, want 3", len(list))
	}
	wantUsers := []string{"dana", "sam", "dana"}
	for i, r := range list {
		if r.Seq != int64(3-i) || r.User != wantUsers[i] {
			t.Errorf("list[%d] = seq %d by %s, want seq %d by %s", i, r.Seq, r.User, 3-i, wantUsers[i])
		}
		if r.Flows != nil || r.Credentials != nil {
			t.Errorf("list[%d] carries payloads; a listing is metadata only", i)
		}
		if r.Time.IsZero() {
			t.Errorf("list[%d] has no time", i)
		}
	}
	if got := l.List(2); len(got) != 2 || got[0].Seq != 3 {
		t.Errorf("List(2) = %v", got)
	}

	full, err := l.Get(third.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full.Flows, []byte("[\n    3\n]\n")) {
		t.Errorf("flows came back as %q, not byte for byte", full.Flows)
	}
	if _, err := l.Get(99); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(99) err = %v, want ErrNotFound", err)
	}
}

// Reopening is how a restart sees the log. Same records, and the next one
// continues the sequence rather than reusing a number.
func TestReopenContinuesTheSequence(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, 0)
	appendDeploy(t, l, "dana", "r1", "[]")
	appendDeploy(t, l, "dana", "r2", "[]")

	l2 := open(t, dir, 0)
	if latest, ok := l2.Latest(); !ok || latest.Seq != 2 || latest.Rev != "r2" {
		t.Fatalf("latest after reopen = %+v", latest)
	}
	if r := appendDeploy(t, l2, "sam", "r3", "[]"); r.Seq != 3 {
		t.Fatalf("next record after reopen is %d, want 3", r.Seq)
	}
}

// Retention drops the oldest, and a pruned sequence number is never handed out
// again, even across a restart.
func TestRetentionDropsTheOldestOnly(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, 2)
	for i := 0; i < 5; i++ {
		appendDeploy(t, l, "dana", "r", "[]")
	}
	list := l.List(0)
	if len(list) != 2 || list[0].Seq != 5 || list[1].Seq != 4 {
		t.Fatalf("kept %v, want 5 and 4", list)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 2 {
		t.Fatalf("%d files on disk, want 2", len(files))
	}
	if _, err := l.Get(1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a pruned record is still readable: %v", err)
	}

	l2 := open(t, dir, 2)
	if r := appendDeploy(t, l2, "dana", "r", "[]"); r.Seq != 6 {
		t.Fatalf("after reopen the next record is %d, want 6", r.Seq)
	}
}

// Retention of one still keeps the newest, which is what is running.
func TestRetentionNeverDropsTheNewest(t *testing.T) {
	l := open(t, t.TempDir(), 1)
	appendDeploy(t, l, "dana", "r1", "[]")
	appendDeploy(t, l, "dana", "r2", "[]")
	if latest, ok := l.Latest(); !ok || latest.Rev != "r2" {
		t.Fatalf("latest = %+v", latest)
	}
}

// A crash between writing the temp file and renaming it leaves a dot-file
// behind. That is not a record and must not be read as one.
func TestInterruptedWriteIsNotARecord(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, 0)
	appendDeploy(t, l, "dana", "r1", "[]")
	if err := os.WriteFile(filepath.Join(dir, ".0000000002.json.tmp-123"), []byte(`{"seq":2,"kind":"dep`), 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := open(t, dir, 0)
	if got := len(l2.List(0)); got != 1 {
		t.Fatalf("%d records after an interrupted write, want 1", got)
	}
	if r := appendDeploy(t, l2, "dana", "r2", "[]"); r.Seq != 2 {
		t.Fatalf("next seq = %d, want 2", r.Seq)
	}
}

// A damaged record is reported and skipped. Its number is still taken.
func TestCorruptRecordIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, 0)
	appendDeploy(t, l, "dana", "r1", "[]")
	appendDeploy(t, l, "dana", "r2", "[]")
	if err := os.WriteFile(filepath.Join(dir, nameFor(2)), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l2, warnings, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "deployment 2") {
		t.Fatalf("warnings = %v", warnings)
	}
	if r := appendDeploy(t, l2, "dana", "r3", "[]"); r.Seq != 3 {
		t.Fatalf("next seq = %d, want 3; a damaged record's number was reused", r.Seq)
	}
}

func TestRecordsAreNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, 0)
	appendDeploy(t, l, "dana", "r1", "[]")
	// Something put a file where the next record would go.
	if err := os.WriteFile(filepath.Join(dir, nameFor(2)), []byte(`{"seq":2,"kind":"deploy","rev":"theirs"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(Record{Kind: KindDeploy, Rev: "mine"}); err == nil {
		t.Fatal("Append replaced an existing record")
	}
	data, _ := os.ReadFile(filepath.Join(dir, nameFor(2)))
	if !strings.Contains(string(data), "theirs") {
		t.Fatal("the existing record was changed")
	}
}

func TestNotesAreBounded(t *testing.T) {
	l := open(t, t.TempDir(), 0)
	if _, err := l.Append(Record{Kind: KindDeploy, Note: strings.Repeat("x", MaxNoteLength+1)}); err == nil {
		t.Fatal("accepted a note past the limit")
	}
	if _, err := l.Append(Record{Kind: KindDeploy, Note: strings.Repeat("x", MaxNoteLength)}); err != nil {
		t.Fatalf("refused a note at the limit: %v", err)
	}
}

func TestFindRevReturnsTheNewest(t *testing.T) {
	l := open(t, t.TempDir(), 0)
	appendDeploy(t, l, "dana", "same", "[]")
	appendDeploy(t, l, "dana", "other", "[]")
	appendDeploy(t, l, "sam", "same", "[]")
	r, ok := l.FindRev("same")
	if !ok || r.Seq != 3 {
		t.Fatalf("FindRev = %+v, want seq 3", r)
	}
	if _, ok := l.FindRev("never"); ok {
		t.Fatal("found a revision that was never deployed")
	}
}

func TestRecordFilesAreOwnerOnly(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("file modes are a Linux property; the image is Linux")
	}
	dir := t.TempDir()
	l := open(t, dir, 0)
	appendDeploy(t, l, "dana", "r1", "[]")
	info, err := os.Stat(filepath.Join(dir, nameFor(1)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %o, want 600", info.Mode().Perm())
	}
}
