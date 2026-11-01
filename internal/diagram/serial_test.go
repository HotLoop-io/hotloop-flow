//go:build linux

package diagram

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// The cable is pulled and put back: the read says so on its error wire, the next
// read opens the port again, frames carry on, and both events are counted.
func TestSerialSessionReconnectsAfterAPull(t *testing.T) {
	dir := t.TempDir()
	link := dir + "/tty"
	masterA, slaveA := ptyPair(t)
	if err := os.Symlink(slaveA, link); err != nil {
		t.Fatal(err)
	}
	s := &serialSession{cfg: SerialConfig{Device: link, Baud: 9600, DataBits: 8, StopBits: 1, Parity: "N"}}
	t.Cleanup(func() { _ = s.Close() })
	s.mu.Lock()
	err := s.ensure()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitOpened(t, masterA)
	read := func() (string, error) {
		b, err := s.read(context.Background(), []byte("\n"), 0, 2*time.Second)
		return string(b), err
	}
	fmt.Fprint(masterA, "one\n")
	if got, err := read(); err != nil || got != "one" {
		t.Fatalf("first read: %q, %v", got, err)
	}

	_ = masterA.Close() // pulled
	_, err = read()
	var f *Fault
	if !errors.As(err, &f) || f.Code != CodeDevice {
		t.Fatalf("a pulled cable gave %v, want a device error", err)
	}
	if l, r := s.Counts(); l != 1 || r != 0 {
		t.Fatalf("after the pull: %d lost, %d reconnected", l, r)
	}

	masterB, slaveB := ptyPair(t) // put back, as a new terminal behind the same name
	_ = os.Remove(link)
	if err := os.Symlink(slaveB, link); err != nil {
		t.Fatal(err)
	}
	go func() {
		// The first read after the pull opens the port; write once it has.
		waitOpened(t, masterB)
		fmt.Fprint(masterB, "two\n")
	}()
	got, err := read()
	if err != nil || got != "two" {
		t.Fatalf("read after the cable came back: %q, %v", got, err)
	}
	if l, r := s.Counts(); l != 1 || r != 1 {
		t.Fatalf("after the return: %d lost, %d reconnected, want 1 and 1", l, r)
	}
}

// A port that is not there is retried once a second, not as fast as the loop can
// go, and every attempt is a fault on the error wire.
func TestAMissingPortIsNotRetriedInATightLoop(t *testing.T) {
	s := &serialSession{cfg: SerialConfig{Device: t.TempDir() + "/nothing", Baud: 9600, DataBits: 8, StopBits: 1, Parity: "N"}, lost: true}
	start := time.Now()
	attempts := 0
	for time.Since(start) < 2500*time.Millisecond {
		_, err := s.read(context.Background(), []byte("\n"), 0, time.Second)
		var f *Fault
		if !errors.As(err, &f) || f.Code != CodeDevice {
			t.Fatalf("got %v, want a device fault", err)
		}
		attempts++
	}
	if attempts > 4 {
		t.Fatalf("%d attempts in 2.5 s; a missing port should be tried about once a second", attempts)
	}
}
