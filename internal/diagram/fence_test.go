//go:build linux

package diagram

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/fence"
)

type holdAll struct{ err error }

func (h holdAll) Check(string) error { return h.err }

// fenceRecorder is an instrument that keeps every line it is sent.
func fenceRecorder(t *testing.T) (string, func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var lines []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					mu.Lock()
					lines = append(lines, strings.TrimSpace(l))
					mu.Unlock()
				}
			}()
		}
	}()
	return ln.Addr().String(), func() []string {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// An instrument write from a standby is refused before a byte goes to the
// instrument, with its own error code, and the same diagram writes when the gate
// says yes, so the test can see a write.
func TestAStaleHoldersWriteToAnInstrumentIsRefused(t *testing.T) {
	addr, got := fenceRecorder(t)
	build := func() *Run {
		b := newFlow(t).tab("main", nil).
			node("main", "open", "dia-instr-open", map[string]any{"address": addr}).
			node("main", "q", "dia-instr-query", map[string]any{"command": "OUTP ON", "reply": "none"}).
			node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
			wire("open.session", "q.session").wire("q.err", "err.value")
		r, err := b.run("main")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	restore := fence.Set(holdAll{fence.ErrStandby})
	r := build()
	restore()
	e, _ := lastOn(t, r, "main/q.err>err.value").(map[string]any)
	if e["code"] != float64(CodeFenced) || !strings.Contains(fmt.Sprint(e["source"]), "standby") {
		t.Fatalf("error out %v", e)
	}
	if l := got(); len(l) != 0 {
		t.Fatalf("a standby sent %v to the instrument", l)
	}
	build()
	if l := got(); len(l) != 1 || l[0] != "OUTP ON" {
		t.Fatalf("with the gate open the instrument saw %v", l)
	}
}

// A serial write from a holder that has lost its lease sends nothing down the port.
func TestAStaleHoldersSerialWriteIsRefused(t *testing.T) {
	master, slave := ptyPair(t)
	build := func() *Run {
		b := newFlow(t).tab("main", nil).
			node("main", "port", "dia-serial-open", map[string]any{"device": slave, "baud": 9600.0}).
			node("main", "cmd", "dia-const", map[string]any{"value": `"START"`}).
			node("main", "w", "dia-serial-write", map[string]any{"terminator": `\n`}).
			node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
			wire("port.session", "w.session").wire("cmd.value", "w.data").wire("w.err", "err.value")
		r, err := b.run("main")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	readAll := func() string {
		_ = master.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		buf := make([]byte, 64)
		n, _ := master.Read(buf)
		return string(buf[:n])
	}
	restore := fence.Set(holdAll{fence.ErrLost})
	r := build()
	restore()
	e, _ := lastOn(t, r, "main/w.err>err.value").(map[string]any)
	if e["code"] != float64(CodeFenced) {
		t.Fatalf("error out %v", e)
	}
	if got := readAll(); got != "" {
		t.Fatalf("a holder that lost its lease wrote %q to the port", got)
	}
	build()
	if got := readAll(); !strings.Contains(got, "START") {
		t.Fatalf("with the gate open the port saw %q, so the test cannot see a write", got)
	}
}
