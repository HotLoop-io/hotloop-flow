//go:build linux

package diagram

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// I3: a simulator streams scale frames over a pty and the read node returns one
// line per frame.
func TestSerialReadReturnsOneLinePerFrame(t *testing.T) {
	master, slave := ptyPair(t)
	b := newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
		node("main", "port", "dia-serial-open", map[string]any{"device": slave, "baud": 9600.0}).
		node("main", "five", "dia-const", map[string]any{"value": "5"}).
		node("main", "loop", "dia-for", map[string]any{"body": "body", "ins": "session"}).
		node("main", "lines", "dia-indicator", nil).
		wire("port.session", "loop.session").wire("five.value", "loop.n").wire("loop.collected", "lines.value").
		node("body", "session", "dia-body-in", map[string]any{"name": "session"}).
		node("body", "read", "dia-serial-read", map[string]any{"timeoutMs": 3000.0}).
		node("body", "collect", "dia-body-out", map[string]any{"name": "collect"}).
		wire("session.value", "read.session").wire("read.line", "collect.value")
	m := b.manager()
	mustProblemFree(t, m, "main")
	go func() {
		waitOpened(t, master)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(master, "ST,GS,  %5.2f kg\r\n", 12.34+float64(i))
			time.Sleep(20 * time.Millisecond)
		}
	}()
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	got, _ := lastOn(t, r, "main/loop.collected>lines.value").([]any)
	want := "[ST,GS,  12.34 kg ST,GS,  13.34 kg ST,GS,  14.34 kg ST,GS,  15.34 kg ST,GS,  16.34 kg]"
	if fmt.Sprint(got) != want {
		t.Fatalf("read %v\nwant %s", got, want)
	}
}

// A read that gets nothing returns a timeout on its error wire.
func TestSerialReadTimesOutOnTheErrorWire(t *testing.T) {
	_, slave := ptyPair(t)
	b := newFlow(t).tab("main", nil).
		node("main", "port", "dia-serial-open", map[string]any{"device": slave}).
		node("main", "read", "dia-serial-read", map[string]any{"timeoutMs": 250.0}).
		node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
		wire("port.session", "read.session").wire("read.err", "err.value")
	start := time.Now()
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	e, ok := lastOn(t, r, "main/read.err>err.value").(map[string]any)
	if !ok || e["status"] != true || e["code"] != float64(CodeTimeout) || !strings.Contains(fmt.Sprint(e["source"]), "nothing arrived") {
		t.Fatalf("error out is %v", lastOn(t, r, "main/read.err>err.value"))
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 900*time.Millisecond {
		t.Fatalf("a 250 ms timeout took %v", took)
	}
}

// X6: the read fails, the parse after it does not run, the loop stops, and the
// loop's error out holds the source and the code.
func TestErrorWireSkipsStopsTheLoopAndKeepsTheSource(t *testing.T) {
	master, slave := ptyPair(t)
	b := newFlow(t).tab("main", nil).tab("body", map[string]any{"role": "body"}).
		node("main", "port", "dia-serial-open", map[string]any{"device": slave}).
		node("main", "loop", "dia-while", map[string]any{"body": "body", "ins": "session"}).
		node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
		node("main", "passes", "dia-indicator", nil).
		wire("port.session", "loop.session").wire("loop.err", "err.value").wire("loop.iterations", "passes.value").
		node("body", "session", "dia-body-in", map[string]any{"name": "session"}).
		node("body", "read", "dia-serial-read", map[string]any{"timeoutMs": 2000.0}).
		node("body", "parse", "dia-parse-reading", nil).
		node("body", "never", "dia-const", map[string]any{"value": "false"}).
		node("body", "stop", "dia-body-out", map[string]any{"name": "stop"}).
		node("body", "errOut", "dia-body-out", map[string]any{"name": "err"}).
		wire("session.value", "read.session").wire("read.line", "parse.line").wire("read.err", "parse.err").
		wire("parse.err", "errOut.value").wire("never.value", "stop.value")
	m := NewManager(nil)
	rc := &recorder{}
	m.SetPublisher(rc.publish)
	t.Cleanup(m.Close)
	m.Apply(b.flows())
	mustProblemFree(t, m, "main")
	go func() {
		waitOpened(t, master)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(master, "ST,GS,  10.00 kg\r\n")
			time.Sleep(30 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)
		_ = master.Close() // the cable is pulled
	}()
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the loop never stopped")
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	e, _ := lastOn(t, r, "main/loop.err>err.value").(map[string]any)
	if e["status"] != true || e["code"] != float64(CodeDevice) || !strings.HasPrefix(fmt.Sprint(e["source"]), "dia-serial-read: lost "+slave) || e["node"] != "read" {
		t.Fatalf("the loop's error out is %v", e)
	}
	if got := lastOn(t, r, "main/loop.iterations>passes.value"); got != 4.0 {
		t.Fatalf("the loop made %v passes; three good frames and the failed read is four", got)
	}
	count := func(node, state string) int { _, n := rc.nodeEvent(node, state); return n }
	if count("read", "done") != 3 || count("read", "error") != 1 {
		t.Fatalf("read: %d done, %d error", count("read", "done"), count("read", "error"))
	}
	if count("parse", "done") != 3 || count("parse", "skipped") != 1 {
		t.Fatalf("parse: %d done, %d skipped; with an error in, parse must not run", count("parse", "done"), count("parse", "skipped"))
	}
}
