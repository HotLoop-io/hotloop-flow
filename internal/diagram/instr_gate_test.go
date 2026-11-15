package diagram

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/instruments"
)

// lineRecorder is an instrument that answers *IDN? and remembers every other line it
// was sent, so a test can see whether a write reached it.
type lineRecorder struct {
	addr string
	mu   sync.Mutex
	got  []string
}

func newRecorder(t *testing.T) *lineRecorder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	r := &lineRecorder{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					if line == "*IDN?" {
						c.Write([]byte("ACME,Supply,1,1.0\n"))
						continue
					}
					r.mu.Lock()
					r.got = append(r.got, line)
					r.mu.Unlock()
				}
			}()
		}
	}()
	return r
}

func (r *lineRecorder) received() []string {
	time.Sleep(100 * time.Millisecond) // the line is on the wire, not yet read
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

type auditRow struct {
	event, who string
	detail     map[string]any
}

// gated is a manager whose instruments start with their writes off, and the rows
// it audited.
func gated(t *testing.T, b *flowBuilder) (*Manager, *instruments.Switch, func() []auditRow) {
	t.Helper()
	sw, err := instruments.Open(filepath.Join(t.TempDir(), "instruments.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var rows []auditRow
	m := NewManager(nil)
	t.Cleanup(m.Close)
	m.SetServices(Services{Instruments: sw, Audit: func(event, who string, d map[string]any) {
		mu.Lock()
		rows = append(rows, auditRow{event, who, d})
		mu.Unlock()
	}})
	m.Apply(b.flows())
	return m, sw, func() []auditRow { mu.Lock(); defer mu.Unlock(); return append([]auditRow(nil), rows...) }
}

func supplyFlow(t *testing.T, addr, command string) *flowBuilder {
	return newFlow(t).tab("main", nil).
		node("main", "open", "dia-instr-open", map[string]any{"address": addr}).
		node("main", "q", "dia-instr-query", map[string]any{"command": command, "reply": "none"}).
		node("main", "e", "dia-indicator", map[string]any{"kind": "error"}).
		wire("open.session", "q.session").wire("q.err", "e.value")
}

func runIt(t *testing.T, m *Manager) *Run {
	t.Helper()
	r, err := m.Start("main", StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	<-r.Done()
	return r
}

// A write is refused while the instrument's switch is off, with the reason, and
// the instrument never hears it; turn the switch on and the same diagram writes.
func TestAnInstrumentWriteIsRefusedUntilItsSwitchIsOn(t *testing.T) {
	inst := newRecorder(t)
	m, sw, audited := gated(t, supplyFlow(t, inst.addr, "OUTP ON"))

	r := runIt(t, m)
	e, _ := lastOn(t, r, "main/q.err>e.value").(map[string]any)
	src, _ := e["source"].(string)
	if e["status"] != true || e["code"] != float64(CodeInstrument) || !strings.Contains(src, "writes to tcp://"+inst.addr+" are switched off") {
		t.Fatalf("error out %v", e)
	}
	if got := inst.received(); len(got) != 0 {
		t.Fatalf("a refused write reached the instrument: %v", got)
	}

	if err := sw.Set(inst.addr, true, "lee", "bench 2"); err != nil {
		t.Fatal(err)
	}
	r = runIt(t, m)
	if v := lastOn(t, r, "main/q.err>e.value"); v != nil {
		if e, ok := v.(map[string]any); ok && e["status"] == true {
			t.Fatalf("an allowed write was an error: %v", e)
		}
	}
	if got := inst.received(); len(got) != 1 || got[0] != "OUTP ON" {
		t.Fatalf("the instrument got %v, want OUTP ON", got)
	}

	// Both were audited, with the diagram and the node.
	rows := audited()
	if len(rows) != 2 || rows[0].event != AuditInstrumentRefused || rows[1].event != AuditInstrumentWrite {
		t.Fatalf("audit rows %v", rows)
	}
	for _, row := range rows {
		if row.who != "flow:main/q" || row.detail["diagram"] != "main" || row.detail["node"] != "q" ||
			row.detail["instrument"] != "tcp://"+inst.addr || row.detail["command"] != "OUTP ON" {
			t.Fatalf("audit row %+v", row)
		}
	}
}

// Reads stay open: a query goes through with every switch off, and nothing is
// audited as a write.
func TestAnInstrumentQueryNeedsNoSwitch(t *testing.T) {
	inst := newRecorder(t)
	b := newFlow(t).tab("main", nil).
		node("main", "open", "dia-instr-open", map[string]any{"address": inst.addr}).
		node("main", "q", "dia-instr-query", map[string]any{"command": "*IDN?"}).
		node("main", "id", "dia-indicator", map[string]any{"kind": "string"}).
		wire("open.session", "q.session").wire("q.reply", "id.value")
	m, _, audited := gated(t, b)
	r := runIt(t, m)
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s, %v", st, err)
	}
	if got := lastOn(t, r, "main/q.reply>id.value"); got != "ACME,Supply,1,1.0" {
		t.Fatalf("reply %v", got)
	}
	if rows := audited(); len(rows) != 0 {
		t.Fatalf("a read was audited as a write: %v", rows)
	}
}

// Deploy says which instruments a diagram would write to, and a read-only
// diagram says nothing.
func TestDeployNamesTheInstrumentsADiagramWouldWriteTo(t *testing.T) {
	inst := newRecorder(t)
	m, _, _ := gated(t, supplyFlow(t, inst.addr, "VOLT 5;OUTP ON"))
	d, _ := m.Diagram("main")
	if len(d.Problems) > 0 {
		t.Fatalf("problems %v", d.Problems)
	}
	if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0].Text, "writes to tcp://"+inst.addr) || d.Warnings[0].Node != "q" {
		t.Fatalf("warnings %v", d.Warnings)
	}
	if w := m.Writes("main"); len(w) != 1 || w[0].Instrument != "tcp://"+inst.addr || len(w[0].Nodes) != 1 {
		t.Fatalf("writes %v", w)
	}

	read := newFlow(t).tab("main", nil).
		node("main", "open", "dia-instr-open", map[string]any{"address": inst.addr}).
		node("main", "q", "dia-instr-query", map[string]any{"command": "MEAS:VOLT?;MEAS:CURR?"}).
		wire("open.session", "q.session")
	m2, _, _ := gated(t, read)
	if d, _ := m2.Diagram("main"); len(d.Warnings) != 0 {
		t.Fatalf("a read-only diagram warned: %v", d.Warnings)
	}
}

func TestWhatIsAWriteCommand(t *testing.T) {
	for cmd, want := range map[string]bool{
		"*IDN?": false, "MEAS:VOLT?": false, "MEAS:VOLT?;MEAS:CURR?": false, "OUTP ON": true, "VOLT 5": true,
		"*RST": true, "*CLS": true, "VOLT 5;MEAS:VOLT?": true, "": true,
	} {
		if got := isWriteCommand(cmd); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

// The switch is kept on disk, one instrument at a time, and the same instrument
// opened two ways is one switch.
func TestTheInstrumentSwitchSurvivesARestartAndNamesOneInstrumentOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.json")
	sw, _ := instruments.Open(path)
	if sw.Allowed("10.0.0.5:5025") {
		t.Fatal("an instrument nobody turned on is on")
	}
	if err := sw.Set("tcp://10.0.0.5:5025", true, "lee", "bench"); err != nil {
		t.Fatal(err)
	}
	if err := sw.Set("serial:///dev/ttyUSB0?baud=19200", true, "lee", ""); err != nil {
		t.Fatal(err)
	}
	again, _ := instruments.Open(path)
	if !again.Allowed("10.0.0.5:5025") || !again.Allowed("/dev/ttyUSB0") || again.Allowed("10.0.0.6:5025") {
		t.Fatalf("after a restart: %v", again.List())
	}
	if a := again.List()["tcp://10.0.0.5:5025"]; a.By != "lee" || a.Note != "bench" || a.At.IsZero() {
		t.Fatalf("allowance %+v", a)
	}
	if err := sw.Set("10.0.0.5:5025", false, "lee", "done"); err != nil || sw.Allowed("tcp://10.0.0.5:5025") {
		t.Fatalf("turning it off: %v", err)
	}
}
