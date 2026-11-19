package nodes

import (
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/fence"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// stale is a gate that holds every write, the way a standby or an active
// instance whose lease has run out does.
type stale struct{ err error }

func (s stale) Check(string) error { return s.err }

// counter is something that listens on a port and counts who connects, so a test
// can say a refused write sent nothing at all, not even a connection.
type counter struct {
	addr string
	n    atomic.Int64
}

func listen(t *testing.T) *counter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &counter{addr: ln.Addr().String()}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.n.Add(1)
			conn.Close()
		}
	}()
	return c
}

// connects says how many connections the listener has seen, after giving a send
// that is going to be made time to be made.
func (c *counter) connects() int64 {
	time.Sleep(300 * time.Millisecond)
	return c.n.Load()
}

// F: a write to equipment that comes from a standby, or from a holder whose lease
// has run out, is refused before anything is sent. One test for each node that can
// command a PLC, direct and through HotLoop's gate; each also proves it can see a
// send, by connecting when the gate says yes.
func TestAStaleHoldersWriteToAPLCIsRefusedBeforeAnythingIsSent(t *testing.T) {
	type path struct {
		name, typ, cfg, payload string
		live                    func(t *testing.T, addr string) (*credServices, func())
	}
	paths := []path{
		{"modbus", "modbus write", `"area":"holding","address":"30","dataType":"uint16","value":"payload","valueType":"msg"`, `{"payload":1}`,
			func(t *testing.T, a string) (*credServices, func()) { return modbusLive(t, a, "") }},
		{"s7", "s7 write", `"address":"M4.1","dataType":"bool","value":"payload","valueType":"msg"`, `{"payload":true}`,
			func(t *testing.T, a string) (*credServices, func()) { return s7Live(t, a, "") }},
		{"opcua", "opcua write", `"nodeId":"ns=2;s=Setpoint","dataType":"Int32","value":"payload","valueType":"msg"`, `{"payload":5}`,
			func(t *testing.T, a string) (*credServices, func()) { return opcLive(t, a, opcOpts{}) }},
		{"ethernetip", "ethernetip write", `"tag":"Speed","dataType":"int16","value":"payload","valueType":"msg"`, `{"payload":5}`,
			func(t *testing.T, a string) (*credServices, func()) { return enipLive(t, a, "") }},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			ln := listen(t)
			svc, done := p.live(t, ln.addr)
			defer done()
			n := build(t, p.typ, `{"server":"srv",`+p.cfg+`}`, svc)

			restore := fence.Set(stale{fence.ErrStandby})
			_, err := send(t, n, msg(t, p.payload))
			restore()
			if !errors.Is(err, fence.ErrStandby) {
				t.Fatalf("a standby's write: %v, want the standby refusal", err)
			}
			if got := ln.connects(); got != 0 {
				t.Fatalf("a refused write connected %d times", got)
			}

			restore = fence.Set(stale{fence.ErrLost})
			_, err = send(t, n, msg(t, p.payload))
			restore()
			if !errors.Is(err, fence.ErrLost) {
				t.Fatalf("a holder that lost its lease: %v, want the lost refusal", err)
			}
			if got := ln.connects(); got != 0 {
				t.Fatalf("a refused write connected %d times", got)
			}

			// With the gate saying yes, the same write is attempted: the listener
			// sees it. Without this the two checks above could pass for the wrong reason.
			restore = fence.Set(stale{nil})
			_, _ = send(t, n, msg(t, p.payload))
			restore()
			if got := ln.connects(); got == 0 {
				t.Fatal("with the gate open the write was not even attempted, so the test cannot see a send")
			}
		})
	}
}

// The same four nodes writing through HotLoop's gate: a stale holder does not
// reach HotLoop, because HotLoop's gate would write to the equipment for it.
func TestAStaleHoldersWriteThroughHotloopIsRefusedBeforeItReachesHotloop(t *testing.T) {
	for _, c := range []struct{ typ, cfg string }{
		{"modbus write", `"server":"srv","hotloopTag":"plc-1/Setpoint"`},
		{"opcua write", `"server":"srv","nodeId":"ns=2;s=A","hotloopTag":"plc-1/Setpoint"`},
		{"s7 write", `"server":"srv","address":"DB1.DBW0","hotloopTag":"plc-1/Setpoint"`},
		{"ethernetip write", `"server":"srv","tag":"Speed","hotloopTag":"plc-1/Setpoint"`},
	} {
		t.Run(c.typ, func(t *testing.T) {
			f := newFakeHotloop(t)
			var writes atomic.Int64
			f.handle("POST /api/write", func(w http.ResponseWriter, r *http.Request) {
				writes.Add(1)
				writeJSON(w, 200, map[string]any{"tag": "plc-1/Setpoint", "written": "72.5"})
			})
			svc := f.services(t)
			n := build(t, c.typ, `{`+c.cfg+`,"hotloop":"hl","value":"payload","valueType":"msg"}`, svc)

			restore := fence.Set(stale{fence.ErrStandby})
			_, err := send(t, n, msg(t, `{"payload":72.5}`))
			restore()
			if !errors.Is(err, fence.ErrStandby) || writes.Load() != 0 {
				t.Fatalf("a standby's gated write: %v, %d writes reached HotLoop", err, writes.Load())
			}
			restore = fence.Set(stale{nil})
			if _, err := send(t, n, msg(t, `{"payload":72.5}`)); err != nil || writes.Load() != 1 {
				restore()
				t.Fatalf("with the gate open: %v, %d writes", err, writes.Load())
			}
			restore()
		})
	}
}

// HotLoop script and recipe nodes command equipment through HotLoop, so they are
// held the same way.
func TestAStaleHoldersHotloopScriptAndRecipeAreRefused(t *testing.T) {
	f := newFakeHotloop(t)
	var calls atomic.Int64
	f.handle("POST /api/scripts/cip/run", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, map[string]any{"id": "r1"})
	})
	f.handle("POST /api/recipes/grade_a/apply", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	svc := f.services(t)
	script := build(t, "hotloop script", `{"server":"hl","script":"cip","fields":"{\"minutes\":20}","fieldsType":"json"}`, svc)
	recipe := build(t, "hotloop recipe", `{"server":"hl","recipe":"grade_a","params":"{\"speed\":250}","paramsType":"json"}`, svc)
	restore := fence.Set(stale{fence.ErrLost})
	defer restore()
	for name, n := range map[string]node.Node{"script": script, "recipe": recipe} {
		if _, err := send(t, n, msg(t, `{}`)); !errors.Is(err, fence.ErrLost) {
			t.Errorf("%s: %v, want the lost refusal", name, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("%d calls reached HotLoop from a holder that had lost its lease", calls.Load())
	}
	f.mu.Lock()
	for _, r := range f.requests {
		if r != "POST /api/auth/login" {
			t.Errorf("a request reached HotLoop: %s", r)
		}
	}
	f.mu.Unlock()
}

// The diagram's own write to HotLoop goes through GatedWrite.
func TestAStaleHoldersDiagramWriteToHotloopIsRefused(t *testing.T) {
	f := newFakeHotloop(t)
	var writes atomic.Int64
	f.handle("POST /api/write", func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		writeJSON(w, 200, map[string]any{"written": "1"})
	})
	svc := f.services(t)
	cfg := svc.configs["hl"]

	restore := fence.Set(stale{fence.ErrStandby})
	err := GatedWrite(t.Context(), cfg, "plc-1/Setpoint", 1.0, "flow:main/w")
	restore()
	if !errors.Is(err, fence.ErrStandby) || writes.Load() != 0 {
		t.Fatalf("%v, %d writes reached HotLoop", err, writes.Load())
	}
	if err := GatedWrite(t.Context(), cfg, "plc-1/Setpoint", 1.0, "flow:main/w"); err != nil || writes.Load() != 1 {
		t.Fatalf("with no gate: %v, %d writes", err, writes.Load())
	}
}

// A flow test stands in for the outside world and writes nothing, so a standby can
// still run one, and the real send it would have been is never asked about.
func TestAFlowTestIsNeverHeldByTheFence(t *testing.T) {
	restore := fence.Set(stale{fence.ErrStandby})
	defer restore()
	svc := newTestServices()
	svc.standIn = &recordingStandIn{}
	if err := fenceWrite(svc, "modbus write"); err != nil {
		t.Fatalf("under a stand-in: %v", err)
	}
	if err := fenceWrite(newTestServices(), "modbus write"); !errors.Is(err, fence.ErrStandby) {
		t.Fatalf("without one: %v", err)
	}
}
