package diagram

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// simulator answers *IDN?, MEAS:LIST? and WAV? over a raw socket, and ignores
// anything else, which is how a real instrument treats a command it does not own.
func simulator(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
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
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch strings.TrimSpace(line) {
					case "*IDN?":
						fmt.Fprint(c, "ACME,Sim 1,SN42,1.0\n")
					case "MEAS:LIST?":
						fmt.Fprint(c, "1.5,2.5,-3E-1\n")
					case "*STB?":
						fmt.Fprint(c, "20\n")
					case "WAV?":
						b := []byte("#41024")
						for i := 0; i < 512; i++ {
							b = binary.BigEndian.AppendUint16(b, uint16(int16(i-256)))
						}
						c.Write(append(b, '\n'))
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// I1 and I2 over a raw socket: *IDN? is parsed, a comma list is numbers, a status
// byte names its bits, and a #41024 block is 512 samples.
func TestInstrumentQueriesOverARawSocket(t *testing.T) {
	addr := simulator(t)
	b := newFlow(t).tab("main", nil).
		node("main", "open", "dia-instr-open", map[string]any{"address": addr}).
		node("main", "idn", "dia-instr-query", map[string]any{"command": "*IDN?"}).
		node("main", "idnp", "dia-scpi-parse", map[string]any{"as": "identity"}).
		node("main", "list", "dia-instr-query", map[string]any{"command": "MEAS:LIST?"}).
		node("main", "listp", "dia-scpi-parse", map[string]any{"as": "numbers"}).
		node("main", "stb", "dia-instr-query", map[string]any{"command": "*STB?"}).
		node("main", "stbp", "dia-scpi-parse", map[string]any{"as": "status byte"}).
		node("main", "wav", "dia-instr-query", map[string]any{"command": "WAV?", "reply": "block", "format": "int16"}).
		node("main", "iid", "dia-indicator", nil).node("main", "ilist", "dia-indicator", nil).
		node("main", "istb", "dia-indicator", nil).node("main", "iwav", "dia-indicator", nil).
		wire("open.session", "idn.session").wire("idn.session", "list.session").wire("list.session", "stb.session").wire("stb.session", "wav.session").
		wire("idn.reply", "idnp.reply").wire("idnp.value", "iid.value").
		wire("list.reply", "listp.reply").wire("listp.value", "ilist.value").
		wire("stb.reply", "stbp.reply").wire("stbp.value", "istb.value").
		wire("wav.samples", "iwav.value")
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.State(); st != StateDone {
		t.Fatalf("state %s %v", st, err)
	}
	if got := fmt.Sprint(lastOn(t, r, "main/idnp.value>iid.value")); got != "map[firmware:1.0 manufacturer:ACME model:Sim 1 serial:SN42]" {
		t.Fatal(got)
	}
	if got := fmt.Sprint(lastOn(t, r, "main/listp.value>ilist.value")); got != "[1.5 2.5 -0.3]" {
		t.Fatal(got)
	}
	if got := fmt.Sprint(lastOn(t, r, "main/stbp.value>istb.value")); got != "map[byte:20 set:[error queue message available]]" {
		t.Fatal(got)
	}
	w, _ := lastOn(t, r, "main/wav.samples>iwav.value").([]any)
	if len(w) != 512 || w[0] != float64(-256) || w[511] != float64(255) {
		t.Fatalf("%d samples", len(w))
	}
}

// A query the instrument never answers is a timeout on the error wire naming the
// address, and a refused connection is a device fault, not a hang.
func TestInstrumentTimeoutAndRefusedConnection(t *testing.T) {
	addr := simulator(t)
	b := newFlow(t).tab("main", nil).
		node("main", "open", "dia-instr-open", map[string]any{"address": addr}).
		node("main", "q", "dia-instr-query", map[string]any{"command": "NOPE?", "timeoutMs": 250.0}).
		node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
		wire("open.session", "q.session").wire("q.err", "err.value")
	start := time.Now()
	r, err := b.run("main")
	if err != nil {
		t.Fatal(err)
	}
	e, _ := lastOn(t, r, "main/q.err>err.value").(map[string]any)
	if e["code"] != float64(CodeTimeout) || !strings.Contains(fmt.Sprint(e["source"]), addr) {
		t.Fatalf("error out %v", e)
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > time.Second {
		t.Fatalf("took %v", took)
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	b2 := newFlow(t).tab("main", nil).
		node("main", "open", "dia-instr-open", map[string]any{"address": dead}).
		node("main", "err", "dia-indicator", map[string]any{"kind": "error"}).
		wire("open.err", "err.value")
	r2, err := b2.run("main")
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := lastOn(t, r2, "main/open.err>err.value").(map[string]any)
	if e2["code"] != float64(CodeDevice) {
		t.Fatalf("error out %v", e2)
	}
}

func TestParseAddress(t *testing.T) {
	for _, ok := range []string{"10.0.0.5:5025", "tcp://h:1", "serial:///dev/ttyUSB0?baud=19200&parity=E", "/dev/ttyS0", "COM3"} {
		if _, err := parseAddress(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "nohost", ":80", "h:99999", "serial://"} {
		if _, err := parseAddress(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
