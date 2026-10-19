package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openSpool(t *testing.T, path string, max int64) (*SQLite, *Spool) {
	t.Helper()
	db := openDB(t, path)
	sp, err := OpenSpool(db, SpoolOptions{MaxBytes: max, AckFlush: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return db, sp
}

func body(i int) []byte { return []byte(fmt.Sprintf(`{"n":%d}`, i)) }

func TestSpoolKeepsOrderAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, sp := openSpool(t, path, 0)
	q := sp.Wire("a:0>b")
	for i := 0; i < 100; i++ {
		if _, err := q.Append(body(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Deliver the first 40.
	var cursor uint64
	for i := 0; i < 40; i++ {
		e, ok, err := q.Next(cursor)
		if err != nil || !ok {
			t.Fatalf("next %d: %v %v", i, ok, err)
		}
		if !bytes.Equal(e.Body, body(i)) {
			t.Fatalf("entry %d is %s", i, e.Body)
		}
		q.Ack(e)
		cursor = e.Seq
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, sp = openSpool(t, path, 0)
	defer db.Close()
	defer sp.Close()
	q = sp.Wire("a:0>b")
	if q.Depth() != 60 {
		t.Fatalf("depth after restart = %d, want 60", q.Depth())
	}
	cursor = 0
	for i := 40; i < 100; i++ {
		e, ok, _ := q.Next(cursor)
		if !ok || !bytes.Equal(e.Body, body(i)) {
			t.Fatalf("after restart, entry %d is %s (ok=%v)", i, e.Body, ok)
		}
		cursor = e.Seq
	}
	if _, ok, _ := q.Next(cursor); ok {
		t.Fatal("more entries than were appended")
	}
}

func TestSpoolWiresAreIndependent(t *testing.T) {
	db, sp := openSpool(t, filepath.Join(t.TempDir(), "state.db"), 0)
	defer db.Close()
	defer sp.Close()
	a, b := sp.Wire("a"), sp.Wire("b")
	_, _ = a.Append(body(1))
	_, _ = b.Append(body(2))
	_, _ = a.Append(body(3))
	e, _, _ := b.Next(0)
	if !bytes.Equal(e.Body, body(2)) {
		t.Fatalf("wire b read %s", e.Body)
	}
	first, _, _ := a.Next(0)
	a.Ack(first)
	if a.Depth() != 1 || b.Depth() != 1 {
		t.Fatalf("depths a=%d b=%d", a.Depth(), b.Depth())
	}
}

// A kill leaves the files as they are. A fresh open of a copy taken while the
// writer is live and nothing is closed finds every message that was committed.
func TestSpoolCommittedMessagesAreInTheFilesBeforeAppendReturns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, sp := openSpool(t, path, 0)
	defer db.Close()
	defer sp.Close()
	q := sp.Wire("w")
	for i := 0; i < 50; i++ {
		if _, err := q.Append(body(i)); err != nil {
			t.Fatal(err)
		}
	}
	copyDir := filepath.Join(dir, "after-kill")
	_ = os.MkdirAll(copyDir, 0o755)
	for _, suf := range []string{"", "-wal", "-shm"} {
		if data, err := os.ReadFile(path + suf); err == nil {
			_ = os.WriteFile(filepath.Join(copyDir, "state.db"+suf), data, 0o644)
		}
	}
	db2, sp2 := openSpool(t, filepath.Join(copyDir, "state.db"), 0)
	defer db2.Close()
	defer sp2.Close()
	if got := sp2.Wire("w").Depth(); got != 50 {
		t.Fatalf("a process started from the files after a kill found %d messages, want 50", got)
	}
}

func TestSpoolQuotaRefusesAndFreesOnAck(t *testing.T) {
	db, sp := openSpool(t, filepath.Join(t.TempDir(), "state.db"), 1000)
	defer db.Close()
	defer sp.Close()
	q := sp.Wire("w")
	each := int64(len(body(0))) + RowOverhead
	fit := int(1000 / each)
	for i := 0; i < fit; i++ {
		if _, err := q.Append(body(i)); err != nil {
			t.Fatalf("append %d of %d: %v", i, fit, err)
		}
	}
	if _, err := q.Append(body(99)); err != ErrQuota {
		t.Fatalf("append past the quota = %v, want ErrQuota", err)
	}
	if sp.Bytes() > 1000 {
		t.Fatalf("quota used %d of 1000", sp.Bytes())
	}
	space := sp.Space()
	e, _, _ := q.Next(0)
	q.Ack(e)
	select {
	case <-space:
	default:
		t.Fatal("freeing space did not wake a waiting sender")
	}
	if _, err := q.Append(body(100)); err != nil {
		t.Fatalf("append after an ack: %v", err)
	}
}

// The quota is about the file on disk, so the file has to stay near it. Fill to
// the quota, drain it, fill it again, many times: freed pages are reused and the
// file does not grow past what one fill needs.
func TestSpoolFileStaysBoundedByTheQuota(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	const quota = 256 << 10
	db, sp := openSpool(t, path, quota)
	defer db.Close()
	q := sp.Wire("w")
	payload := bytes.Repeat([]byte("x"), 200)
	for round := 0; round < 6; round++ {
		var cursor uint64
		n := 0
		for {
			if _, err := q.Append(payload); err != nil {
				break
			}
			n++
		}
		if n == 0 {
			t.Fatal("nothing fit")
		}
		for i := 0; i < n; i++ {
			e, ok, _ := q.Next(cursor)
			if !ok {
				t.Fatalf("round %d: ran out at %d of %d", round, i, n)
			}
			q.Ack(e)
			cursor = e.Seq
		}
		if err := sp.FlushAcks(); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > quota*3/2 {
		t.Fatalf("database file is %d bytes after six fill-and-drain rounds against a quota of %d", st.Size(), quota)
	}
}

func TestSpoolEvictOldestSparesTheMessageInFlight(t *testing.T) {
	db, sp := openSpool(t, filepath.Join(t.TempDir(), "state.db"), 0)
	defer db.Close()
	defer sp.Close()
	q := sp.Wire("w")
	for i := 0; i < 5; i++ {
		_, _ = q.Append(body(i))
	}
	first, _, _ := q.Next(0)
	q.SetInflight(first.Seq)
	for i := 0; i < 2; i++ {
		if ok, err := q.EvictOldest(); !ok || err != nil {
			t.Fatalf("evict %d: %v %v", i, ok, err)
		}
	}
	var cursor uint64
	var got [][]byte
	for {
		e, ok, _ := q.Next(cursor)
		if !ok {
			break
		}
		got = append(got, e.Body)
		cursor = e.Seq
	}
	want := [][]byte{body(0), body(3), body(4)}
	if len(got) != 3 || !bytes.Equal(got[0], want[0]) || !bytes.Equal(got[1], want[1]) || !bytes.Equal(got[2], want[2]) {
		t.Fatalf("left %q, want the in-flight message and the two newest", got)
	}
	if q.Depth() != 3 || q.Dropped() != 2 {
		t.Fatalf("depth %d dropped %d", q.Depth(), q.Dropped())
	}
}

func TestSpoolPurge(t *testing.T) {
	db, sp := openSpool(t, filepath.Join(t.TempDir(), "state.db"), 0)
	defer db.Close()
	defer sp.Close()
	a, b := sp.Wire("a"), sp.Wire("b")
	for i := 0; i < 10; i++ {
		_, _ = a.Append(body(i))
		_, _ = b.Append(body(i))
	}
	n, err := a.Purge()
	if err != nil || n != 10 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if a.Depth() != 0 || b.Depth() != 10 {
		t.Fatalf("depths a=%d b=%d", a.Depth(), b.Depth())
	}
	if want := 10 * (int64(len(body(0))) + RowOverhead); sp.Bytes() != want {
		t.Fatalf("quota charged %d, want %d", sp.Bytes(), want)
	}
}

// Many senders at once share commits, every message is there, and sequence
// numbers only go up.
func TestSpoolConcurrentSendersShareCommits(t *testing.T) {
	db, sp := openSpool(t, filepath.Join(t.TempDir(), "state.db"), 0)
	defer db.Close()
	defer sp.Close()
	q := sp.Wire("w")
	var wg sync.WaitGroup
	const senders, each = 16, 100
	for s := 0; s < senders; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := q.Append(body(i)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if q.Depth() != senders*each {
		t.Fatalf("depth = %d", q.Depth())
	}
	var cursor, last uint64
	count := 0
	for {
		e, ok, _ := q.Next(cursor)
		if !ok {
			break
		}
		if e.Seq <= last {
			t.Fatalf("sequence went backwards: %d after %d", e.Seq, last)
		}
		last, cursor = e.Seq, e.Seq
		count++
	}
	if count != senders*each {
		t.Fatalf("read %d, want %d", count, senders*each)
	}
}

func TestSpoolClosedRefusesNewMessages(t *testing.T) {
	db, sp := openSpool(t, filepath.Join(t.TempDir(), "state.db"), 0)
	defer db.Close()
	q := sp.Wire("w")
	if err := sp.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Append(body(1)); err != ErrSpoolClosed {
		t.Fatalf("append after close = %v", err)
	}
}

func TestCodecRoundTrip(t *testing.T) {
	in := map[string]any{
		"payload": map[string]any{"n": 1.5, "s": "a&b<c>", "l": []any{true, nil, "x"}},
		"raw":     []byte("bytes"),
		"strs":    []string{"p", "q"},
	}
	enc, err := EncodeValue(in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc, []byte{'\\', 'u', '0', '0', '2', '6'}) {
		t.Errorf("ampersand was HTML-escaped, which inflates every payload: %s", enc)
	}
	out, err := DecodeValue(enc)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if string(m["raw"].([]byte)) != "bytes" {
		t.Errorf("raw = %#v", m["raw"])
	}
	if l := m["strs"].([]any); len(l) != 2 || l[1] != "q" {
		t.Errorf("strs = %#v", m["strs"])
	}
	if p := m["payload"].(map[string]any); p["s"] != "a&b<c>" || p["n"] != 1.5 {
		t.Errorf("payload = %#v", p)
	}
}

func TestCodecNamesWhatItCannotStore(t *testing.T) {
	_, err := EncodeValue(map[string]any{"a": []any{1.0, struct{}{}}})
	if err == nil {
		t.Fatal("a struct was accepted")
	}
	if want := "value.a[1]"; !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Errorf("error %q does not name the path %s", err, want)
	}
}

// Depth is how many messages are on disk. It used to go up when an append was
// handed to the writer, so a reader that saw 100 could still be a commit short
// of 100 rows, and a file copied on its word lost the last one. This watches
// Depth while appends race and checks the file always holds at least what Depth
// claims.
func TestSpoolDepthNeverCountsAMessageThatIsNotOnDisk(t *testing.T) {
	db, sp := openSpool(t, filepath.Join(t.TempDir(), "state.db"), 0)
	defer db.Close()
	defer sp.Close()
	q := sp.Wire("a:0>b")

	const writers, each = 8, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := q.Append(body(w*each + i)); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	defer wg.Wait()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	for {
		claimed := q.Depth()
		var rows int64
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM wire_queue WHERE wire = ?`, "a:0>b").Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows < claimed {
			t.Fatalf("Depth said %d messages were queued and the file held %d", claimed, rows)
		}
		select {
		case <-done:
			if got := q.Depth(); got != writers*each {
				t.Fatalf("Depth is %d after %d appends returned", got, writers*each)
			}
			return
		default:
		}
	}
}
