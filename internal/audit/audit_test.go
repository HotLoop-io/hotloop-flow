package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, maxBytes int64, keep int) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path, maxBytes, keep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func TestRecordAndReadNewestFirst(t *testing.T) {
	l, path := open(t, 0, 0)
	base := time.Date(2026, 10, 3, 6, 0, 0, 0, time.UTC)
	for i, e := range []Entry{
		{Event: LoginFailed, User: "dana", Remote: "10.0.0.7:5100"},
		{Event: Login, User: "dana", Remote: "10.0.0.7:5100"},
		{Event: Deploy, User: "dana", Detail: map[string]any{"deployment": 4}},
		{Event: Inject, User: "sam", Detail: map[string]any{"node": "a1"}},
	} {
		e.Time = base.Add(time.Duration(i) * time.Minute)
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}

	all, err := l.Read(Query{})
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, e := range all {
		events = append(events, e.Event)
	}
	if strings.Join(events, ",") != "inject,deploy,login,login.failed" {
		t.Fatalf("order: %v", events)
	}
	if all[1].Detail["deployment"] != float64(4) {
		t.Fatalf("detail came back as %v", all[1].Detail)
	}

	logins, _ := l.Read(Query{Event: "login."})
	if len(logins) != 1 || logins[0].Event != LoginFailed {
		t.Fatalf("prefix query: %+v", logins)
	}
	exact, _ := l.Read(Query{Event: Login})
	if len(exact) != 1 || exact[0].Event != Login {
		t.Fatalf("exact query: %+v", exact)
	}
	sam, _ := l.Read(Query{User: "sam"})
	if len(sam) != 1 || sam[0].Event != Inject {
		t.Fatalf("user query: %+v", sam)
	}
	recent, _ := l.Read(Query{Since: base.Add(90 * time.Second)})
	if len(recent) != 2 {
		t.Fatalf("since query: %+v", recent)
	}
	one, _ := l.Read(Query{Limit: 1})
	if len(one) != 1 || one[0].Event != Inject {
		t.Fatalf("limit: %+v", one)
	}

	if os.PathSeparator != '\\' {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("audit log mode %o, want 600", info.Mode().Perm())
		}
	}
}

// Rotation keeps the trail bounded and a read still sees across the files it
// kept, in order.
func TestRotationKeepsItBoundedAndReadable(t *testing.T) {
	l, path := open(t, 600, 2)
	for i := 0; i < 40; i++ {
		if err := l.Record(Entry{Event: Deploy, User: "dana", Detail: map[string]any{"n": i}}); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := filepath.Glob(path + "*")
	if len(files) != 3 {
		t.Fatalf("files: %v, want the live one and two rotated", files)
	}
	for _, f := range files {
		if info, _ := os.Stat(f); info.Size() > 600 {
			t.Fatalf("%s is %d bytes, past the 600 limit", f, info.Size())
		}
	}
	got, err := l.Read(Query{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Detail["n"] != float64(39) {
		t.Fatalf("newest = %v", got[0].Detail)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Detail["n"].(float64) != got[i-1].Detail["n"].(float64)-1 {
			t.Fatalf("out of order or gapped at %d: %v then %v", i, got[i-1].Detail, got[i].Detail)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("kept more rotated files than asked")
	}
}

// A crash mid-write leaves a torn last line. Everything before it still reads.
func TestATornLineIsSkipped(t *testing.T) {
	l, path := open(t, 0, 0)
	if err := l.Record(Entry{Event: Login, User: "dana"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprint(f, `{"time":"2026-10-03T06:00:00Z","event":"dep`)
	f.Close()

	l2, err := Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	got, err := l2.Read(Query{})
	if err != nil || len(got) != 1 || got[0].Event != Login {
		t.Fatalf("after a torn line: %v %+v", err, got)
	}
}

// A trail that can't be written says so and counts it. It never panics, and
// the caller decides what to do.
func TestAFailedWriteIsCounted(t *testing.T) {
	l, _ := open(t, 0, 0)
	l.Close()
	if err := l.Record(Entry{Event: Login}); err == nil {
		t.Fatal("a write to a closed trail succeeded")
	}
	if l.Failures() != 1 {
		t.Fatalf("failures = %d", l.Failures())
	}
}
