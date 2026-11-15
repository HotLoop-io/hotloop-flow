package diagram

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/tdms"
)

// logLoop writes n rows through a log node inside a for loop body.
func logLoop(t *testing.T, n int, logType string, cfg map[string]any) *flowBuilder {
	b := newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
		node("main", "n", "dia-const", map[string]any{"value": "60"}).
		node("main", "loop", "dia-for", map[string]any{"body": "body"}).
		wire("n.value", "loop.n").
		node("body", "iter", "dia-body-in", map[string]any{"name": "iter"}).
		node("body", "rec", "dia-bundle", map[string]any{"names": "n"}).
		node("body", "log", logType, cfg).
		wire("iter.value", "rec.n").wire("rec.record", "log.row")
	return b
}

func finishedFiles(t *testing.T, dir, base, ext string) []string {
	m, err := filepath.Glob(filepath.Join(dir, base+"-*"+ext))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func dataLines(t *testing.T, path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return len(lines) - 1 // the header
}

// F2: a text log starts a new file at its size, every finished file has its own
// header, and no row is lost between them.
func TestTextLogRotatesBySizeAndLosesNoRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bench.csv")
	r, err := logLoop(t, 60, "dia-log-text", map[string]any{"path": path, "rotateMB": 0.0005}).run("main")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	done := finishedFiles(t, dir, "bench", ".csv")
	if len(done) < 2 {
		t.Fatalf("finished files: %v", done)
	}
	total := dataLines(t, path)
	for _, f := range done {
		b, _ := os.ReadFile(f)
		if !strings.HasPrefix(string(b), "time,n\n") {
			t.Errorf("%s has no header: %q", f, string(b)[:20])
		}
		total += dataLines(t, f)
	}
	if total != 60 {
		t.Fatalf("%d rows across the files, want 60", total)
	}
}

// F2: retention keeps only the newest finished files, and only files of the
// log's own pattern; the live log and a stranger beside it are left alone.
func TestTextLogRetentionKeepsTheNewestAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bench.csv")
	stranger := filepath.Join(dir, "bench-notes.csv")
	if err := os.WriteFile(stranger, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := logLoop(t, 60, "dia-log-text", map[string]any{"path": path, "rotateMB": 0.0005, "keep": 2}).run("main")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	var kept []string
	for _, f := range finishedFiles(t, dir, "bench", ".csv") {
		if f != stranger {
			kept = append(kept, f)
		}
	}
	if len(kept) != 2 {
		t.Fatalf("kept %v, want 2", kept)
	}
	if b, err := os.ReadFile(stranger); err != nil || string(b) != "mine" {
		t.Fatalf("a file beside the log was touched: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the live log is gone: %v", err)
	}
}

// F2: a TDMS log rotates the same way, each file readable on its own with the
// file properties written again, and the rows add up.
func TestTDMSLogRotatesAndEachFileReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bench.tdms")
	r, err := logLoop(t, 60, "dia-log-tdms", map[string]any{"path": path, "group": "G", "rotateMB": 0.0003, "properties": `{"title":"run"}`}).run("main")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	done := finishedFiles(t, dir, "bench", ".tdms")
	if len(done) < 1 {
		t.Fatalf("no finished files")
	}
	total := 0
	for _, f := range append(done, path) {
		tf, err := tdms.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if v, _ := tdms.Prop(tf.Properties, "title"); v != "run" {
			t.Errorf("%s: title %v", f, v)
		}
		if g := tf.Group("G"); g != nil && g.Channel("n") != nil {
			total += len(g.Channel("n").Values.([]float64))
		}
	}
	if total != 60 {
		t.Fatalf("%d rows across the files, want 60", total)
	}
}

func TestRotationDueAndSettings(t *testing.T) {
	now := time.Now()
	r := rotation{maxBytes: 100, maxAge: time.Hour}
	for _, c := range []struct {
		size int64
		age  time.Duration
		want bool
	}{{0, 5 * time.Hour, false}, {99, time.Minute, false}, {100, time.Minute, true}, {5, 2 * time.Hour, true}} {
		if got := r.due(c.size, now.Add(-c.age), now); got != c.want {
			t.Errorf("size %d age %v: %v", c.size, c.age, got)
		}
	}
	if _, err := rotationOf(map[string]any{"keep": 3.0}); err == nil {
		t.Error("keep with nothing to rotate by was accepted")
	}
	if _, err := rotationOf(map[string]any{"rotateMB": -1.0}); err == nil {
		t.Error("a negative size was accepted")
	}
}
