package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

func openDB(t *testing.T, path string) *SQLite {
	t.Helper()
	db, err := OpenSQLite(path, SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// restart closes everything the way a clean stop does and opens it again.
func restart(t *testing.T, path string, pc *PersistentContexts, db *SQLite, interval time.Duration) (*PersistentContexts, *SQLite) {
	t.Helper()
	if err := pc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openDB(t, path)
	return NewPersistentContexts(db, interval), db
}

func mustGet(t *testing.T, c node.Context, key string) any {
	t.Helper()
	v, ok, err := c.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("key %q is missing", key)
	}
	return v
}

// The control: the memory store, which is what every restart used to leave a
// flow with.
func TestMemoryContextForgetsAtRestart(t *testing.T) {
	a := NewScopedContexts()
	_ = a.Global().Set("count", 7.0)
	b := NewScopedContexts()
	if _, ok, _ := b.Global().Get("count"); ok {
		t.Fatal("a fresh memory store had a value in it")
	}
}

func TestPersistentContextSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	pc := NewPersistentContexts(db, time.Hour)
	sc := pc.Scoped()

	_ = sc.Global().Set("plant", "cleveland")
	_ = sc.Flow("tab1").Set("setpoint", 72.5)
	_ = sc.Node("n1").Set("seen", true)
	_ = sc.Global().Set("shape", map[string]any{"a": []any{1.0, "x"}, "b": nil})
	_ = sc.Global().Set("blob", []byte{9, 8, 7})
	when := time.Date(2026, 10, 3, 4, 5, 6, 7, time.UTC)
	_ = sc.Global().Set("when", when)

	pc, db = restart(t, path, pc, db, time.Hour)
	defer db.Close()
	defer pc.Close()
	sc = pc.Scoped()

	if got := mustGet(t, sc.Global(), "plant"); got != "cleveland" {
		t.Errorf("global plant = %#v", got)
	}
	if got := mustGet(t, sc.Flow("tab1"), "setpoint"); got != 72.5 {
		t.Errorf("flow setpoint = %#v", got)
	}
	if got := mustGet(t, sc.Node("n1"), "seen"); got != true {
		t.Errorf("node seen = %#v", got)
	}
	if got := mustGet(t, sc.Global(), "blob").([]byte); string(got) != string([]byte{9, 8, 7}) {
		t.Errorf("blob = %v", got)
	}
	if got := mustGet(t, sc.Global(), "when").(time.Time); !got.Equal(when) {
		t.Errorf("when = %v", got)
	}
	shape := mustGet(t, sc.Global(), "shape").(map[string]any)
	if arr := shape["a"].([]any); len(arr) != 2 || arr[0] != 1.0 || arr[1] != "x" {
		t.Errorf("shape = %#v", shape)
	}
	// A scope that was never written to is empty, not shared with another.
	if _, ok, _ := sc.Flow("tab2").Get("setpoint"); ok {
		t.Error("flow tab2 sees tab1's key")
	}
}

func TestPersistentAtomicOperationsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	pc := NewPersistentContexts(db, time.Hour)
	g := pc.Scoped().Global()

	// Claim a key exactly once.
	if ok, _ := g.CompareAndSwap("owner", nil, "node-a"); !ok {
		t.Fatal("the first claim failed")
	}
	if ok, _ := g.CompareAndSwap("owner", nil, "node-b"); ok {
		t.Fatal("a second claim on a taken key succeeded")
	}
	if n, _ := g.Increment("count", 5); n != 5 {
		t.Fatalf("increment = %v", n)
	}
	if _, err := g.Update("list", func(cur any) (any, error) {
		l, _ := cur.([]any)
		return append(l, "first"), nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = g.Set("int", 3) // a function node can store an int

	pc, db = restart(t, path, pc, db, time.Hour)
	defer db.Close()
	defer pc.Close()
	g = pc.Scoped().Global()

	if got := mustGet(t, g, "owner"); got != "node-a" {
		t.Errorf("owner = %#v", got)
	}
	if n, _ := g.Increment("count", 1); n != 6 {
		t.Errorf("counter did not resume: %v", n)
	}
	if ok, _ := g.CompareAndSwap("owner", "node-a", "node-c"); !ok {
		t.Error("CompareAndSwap against a value that came back from disk failed")
	}
	// The int stored before the restart comes back as a float, and must still
	// compare equal to the int that was written.
	if ok, _ := g.CompareAndSwap("int", 3, 4); !ok {
		t.Error("CompareAndSwap(3 -> 4) failed after the value round-tripped")
	}
	list := mustGet(t, g, "list").([]any)
	if len(list) != 1 || list[0] != "first" {
		t.Errorf("list = %#v", list)
	}
}

func TestPersistentIncrementIsAtomicAndKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	pc := NewPersistentContexts(db, 20*time.Millisecond)
	g := pc.Scoped().Global()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				if _, err := g.Increment("n", 1); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	pc, db = restart(t, path, pc, db, time.Hour)
	defer db.Close()
	defer pc.Close()
	if got := mustGet(t, pc.Scoped().Global(), "n"); got != 4000.0 {
		t.Fatalf("8 x 500 increments left %v after a restart, want 4000", got)
	}
}

// Writes are behind: what has not been flushed is what a kill -9 loses, and
// what has been flushed is not.
func TestPersistentWritesAreBehindUntilFlushed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	defer db.Close()
	pc := NewPersistentContexts(db, time.Hour)
	g := pc.Scoped().Global()
	_ = g.Set("kept", "yes")
	if err := pc.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = g.Set("lost", "yes")

	// A reader that opens the same file sees what is on disk right now.
	other := NewPersistentContexts(db, time.Hour)
	og := other.Scoped().Global()
	if _, ok, _ := og.Get("kept"); !ok {
		t.Error("a flushed value is not on disk")
	}
	if _, ok, _ := og.Get("lost"); ok {
		t.Error("an unflushed value was already on disk")
	}
}

func TestPersistentZeroIntervalWritesThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	defer db.Close()
	pc := NewPersistentContexts(db, 0)
	g := pc.Scoped().Global()
	_ = g.Set("a", 1.0)
	_, _ = g.Increment("b", 2)
	_ = g.Set("c", "x")
	_ = g.Delete("c")

	other := NewPersistentContexts(db, 0).Scoped().Global()
	if mustGet(t, other, "a") != 1.0 || mustGet(t, other, "b") != 2.0 {
		t.Fatal("write-through values are not on disk")
	}
	if _, ok, _ := other.Get("c"); ok {
		t.Fatal("a deleted key came back")
	}
}

// Setting a value to what it already is must not touch the disk. This is the
// SD card test: a flow that writes the same reading every second wears nothing.
func TestPersistentUnchangedValuesWriteNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	defer db.Close()
	pc := NewPersistentContexts(db, time.Hour)
	g := pc.Scoped().Global()

	_ = g.Set("temp", 21.5)
	_ = pc.Flush()
	if got := pc.rowsWritten.Load(); got != 1 {
		t.Fatalf("first write changed %d rows, want 1", got)
	}
	for range 100 {
		_ = g.Set("temp", 21.5)
		_ = pc.Flush()
	}
	if got := pc.rowsWritten.Load(); got != 1 {
		t.Fatalf("100 writes of the same value changed %d rows in total, want 1", got)
	}
	_ = g.Set("temp", 21.6)
	_ = pc.Flush()
	if got := pc.rowsWritten.Load(); got != 2 {
		t.Fatalf("a changed value wrote %d rows in total, want 2", got)
	}
}

// Ten thousand increments inside one interval are one row written, not ten
// thousand.
func TestPersistentBatchesAnIntervalIntoOneWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	defer db.Close()
	pc := NewPersistentContexts(db, time.Hour)
	g := pc.Scoped().Global()
	for range 10000 {
		_, _ = g.Increment("n", 1)
	}
	_ = pc.Flush()
	if pc.flushes.Load() != 1 || pc.rowsWritten.Load() != 1 {
		t.Fatalf("%d flushes wrote %d rows, want 1 and 1", pc.flushes.Load(), pc.rowsWritten.Load())
	}
}

func TestPersistentCleanForgetsDeletedNodesAndFlows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	pc := NewPersistentContexts(db, time.Hour)
	sc := pc.Scoped()
	_ = sc.Global().Set("g", 1.0)
	_ = sc.Flow("live").Set("k", 1.0)
	_ = sc.Flow("gone").Set("k", 1.0)
	_ = sc.Node("live-n").Set("k", 1.0)
	_ = sc.Node("gone-n").Set("k", 1.0)
	_ = pc.Flush()

	sc.Clean(map[string]bool{"live-n": true}, map[string]bool{"live": true})

	pc, db = restart(t, path, pc, db, time.Hour)
	defer db.Close()
	defer pc.Close()
	sc = pc.Scoped()
	if _, ok, _ := sc.Global().Get("g"); !ok {
		t.Error("the global scope lost its key")
	}
	if _, ok, _ := sc.Flow("live").Get("k"); !ok {
		t.Error("a live flow lost its context")
	}
	if _, ok, _ := sc.Node("live-n").Get("k"); !ok {
		t.Error("a live node lost its context")
	}
	if _, ok, _ := sc.Flow("gone").Get("k"); ok {
		t.Error("a deleted flow's context is still on disk")
	}
	if _, ok, _ := sc.Node("gone-n").Get("k"); ok {
		t.Error("a deleted node's context is still on disk")
	}
}

// What cannot be stored is refused where the flow can see it, not skipped.
func TestPersistentRefusesWhatItCannotStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	defer db.Close()
	pc := NewPersistentContexts(db, time.Hour)
	g := pc.Scoped().Global()

	if err := g.Set("fn", func() {}); err == nil {
		t.Fatal("a function was accepted")
	}
	if _, ok, _ := g.Get("fn"); ok {
		t.Fatal("the refused value is in the store anyway")
	}
	if err := g.Set("nan", nanValue()); err == nil {
		t.Fatal("NaN was accepted")
	}
	if _, err := g.Update("k", func(any) (any, error) { return make(chan int), nil }); err == nil {
		t.Fatal("Update stored a channel")
	}
}

func nanValue() float64 {
	var z float64
	return z / z
}

// A user's own keys that look like the storage tags are theirs.
func TestPersistentKeysThatLookLikeTagsAreNotMisread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDB(t, path)
	pc := NewPersistentContexts(db, time.Hour)
	g := pc.Scoped().Global()
	tricky := map[string]any{"$bytes": "AAEC"}
	_ = g.Set("tricky", tricky)
	_ = g.Set("tricky2", map[string]any{"$time": "2026-01-01T00:00:00Z"})
	_ = g.Set("dollars", map[string]any{"$$x": 1.0, "$y": 2.0})

	pc, db = restart(t, path, pc, db, time.Hour)
	defer db.Close()
	defer pc.Close()
	g = pc.Scoped().Global()
	if got := mustGet(t, g, "tricky").(map[string]any); got["$bytes"] != "AAEC" {
		t.Errorf("tricky = %#v", got)
	}
	if got := mustGet(t, g, "tricky2").(map[string]any); got["$time"] != "2026-01-01T00:00:00Z" {
		t.Errorf("tricky2 = %#v", got)
	}
	if got := mustGet(t, g, "dollars").(map[string]any); got["$$x"] != 1.0 || got["$y"] != 2.0 {
		t.Errorf("dollars = %#v", got)
	}
}
