package nodes

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// ---------------------------------------------------------------------------
// The number crunching, with no device at all
// ---------------------------------------------------------------------------

func TestModbusEncodeThenDecodeIsTheSameNumber(t *testing.T) {
	cases := []struct {
		typ string
		in  any
	}{
		{"uint16", 0.0}, {"uint16", 65535.0}, {"uint16", 1234.0},
		{"int16", -1.0}, {"int16", -32768.0}, {"int16", 32767.0},
		{"uint32", 305419896.0}, {"uint32", 4294967295.0},
		{"int32", -2147483648.0}, {"int32", -7.0},
		{"float32", 3.5}, {"float32", -12.25}, {"float32", 0.0},
		{"float64", 1.0000000001}, {"float64", -9876543.21},
		{"uint64", 9007199254740992.0}, {"int64", -9007199254740992.0},
	}
	for _, little := range []bool{false, true} {
		for _, low := range []bool{false, true} {
			for _, c := range cases {
				t.Run(fmt.Sprintf("%s/%v/little=%v/low=%v", c.typ, c.in, little, low), func(t *testing.T) {
					ty := modbusTypes[c.typ]
					words, err := encodeModbusValue(c.in, ty, little, low)
					if err != nil {
						t.Fatalf("encode: %v", err)
					}
					if len(words) != ty.words {
						t.Fatalf("%d registers, want %d", len(words), ty.words)
					}
					got, err := decodeModbusWords(words, ty, little, low)
					if err != nil {
						t.Fatalf("decode: %v", err)
					}
					if len(got) != 1 || got[0] != c.in {
						t.Fatalf("round trip gave %#v, want %#v", got, c.in)
					}
				})
			}
		}
	}
}

func TestModbusWordAndByteOrderMeanWhatTheyHaveAlwaysMeant(t *testing.T) {
	ty := modbusTypes["uint32"]
	// 0x12345678 as ABCD is registers 0x1234, 0x5678.
	w, _ := encodeModbusValue(float64(0x12345678), ty, false, false)
	if !reflect.DeepEqual(w, []uint16{0x1234, 0x5678}) {
		t.Errorf("big/high gave %#x", w)
	}
	// CDAB swaps the registers.
	w, _ = encodeModbusValue(float64(0x12345678), ty, false, true)
	if !reflect.DeepEqual(w, []uint16{0x5678, 0x1234}) {
		t.Errorf("big/low gave %#x", w)
	}
	// BADC swaps the bytes inside each.
	w, _ = encodeModbusValue(float64(0x12345678), ty, true, false)
	if !reflect.DeepEqual(w, []uint16{0x3412, 0x7856}) {
		t.Errorf("little/high gave %#x", w)
	}
	// A 32 bit float 3.5 is 0x40600000.
	w, _ = encodeModbusValue(3.5, modbusTypes["float32"], false, false)
	if !reflect.DeepEqual(w, []uint16{0x4060, 0x0000}) {
		t.Errorf("float32 3.5 gave %#x", w)
	}
}

func TestModbusSixtyFourBitIntegersPastTheDoubleLimitStayExact(t *testing.T) {
	ty := modbusTypes["uint64"]
	words, err := encodeModbusValue("18446744073709551615", ty, false, false)
	if err != nil {
		t.Fatalf("a string of the exact digits should be accepted: %v", err)
	}
	got, err := decodeModbusWords(words, ty, false, false)
	if err != nil || got[0] != "18446744073709551615" {
		t.Fatalf("got %#v, %v. Past 2^53 the digits come back as a string, exactly", got, err)
	}
	ty = modbusTypes["int64"]
	words, _ = encodeModbusValue("-9223372036854775808", ty, false, false)
	got, _ = decodeModbusWords(words, ty, false, false)
	if got[0] != "-9223372036854775808" {
		t.Fatalf("got %#v", got)
	}
}

func TestModbusRefusesToWriteAValueThatDoesNotFit(t *testing.T) {
	cases := []struct {
		typ  string
		in   any
		want string
	}{
		{"uint16", 70000.0, "outside what a uint16 register holds"},
		{"uint16", -1.0, "outside what a uint16 register holds"},
		{"int16", 40000.0, "outside what a int16 register holds"},
		{"uint16", 3.5, "isn't a whole number"},
		{"uint32", 4294967296.0, "outside"},
		{"float32", 1e300, "too big for a float32"},
		{"uint16", "abc", "isn't a number"},
		{"uint16", math.NaN(), "can't be written"},
		{"uint64", 1e20, "Past 2^53"},
	}
	for _, c := range cases {
		_, err := encodeModbusValue(c.in, modbusTypes[c.typ], false, false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: error = %v, want it to mention %q", c.typ, c.in, err, c.want)
		}
	}
}

func TestModbusCoilValues(t *testing.T) {
	for in, want := range map[any]bool{true: true, false: false, 1.0: true, 0.0: false, "on": true, "false": false} {
		got, err := modbusBool(in)
		if err != nil || got != want {
			t.Errorf("%#v = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := modbusBool(2.0); err == nil || !strings.Contains(err.Error(), "on or off") {
		t.Errorf("2 is not a coil value, and the error should say what is: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Configuration that cannot work refuses to deploy
// ---------------------------------------------------------------------------

func TestModbusConfigRefusesWhatCannotWork(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ cfg, want string }{
		{`{}`, "host is required"},
		{`{"host":"modbus://plc"}`, "no scheme, path or port"},
		{`{"host":"plc","port":0}`, "port 0 is out of range"},
		{`{"host":"plc","port":70000}`, "out of range"},
		{`{"host":"plc","unitId":300}`, "unit ID 300"},
		{`{"host":"plc","timeout":0}`, "timeout"},
	} {
		err := buildErr(t, "hotloop-flow-modbus", c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want %q", c.cfg, err, c.want)
		}
	}
	for _, c := range []struct{ typ, cfg, want string }{
		{"modbus read", `{}`, "no server selected"},
		{"modbus read", `{"server":"s","area":"bogus"}`, "area"},
		{"modbus read", `{"server":"s","dataType":"int128"}`, "data type"},
		{"modbus read", `{"server":"s","unitId":"abc"}`, "unit ID"},
		{"modbus read", `{"server":"s","address":"1 +","addressType":"jsonata"}`, ""},
		{"modbus write", `{"server":"s","area":"input"}`, "read-only"},
		{"modbus write", `{}`, "no server selected"},
	} {
		err := buildErr(t, c.typ, c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: error = %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Under a flow test the node does its work and hands the wire to the stand-in
// ---------------------------------------------------------------------------

type scriptedStandIn struct {
	mu    sync.Mutex
	calls []map[string]any
	reply map[string]any
	err   error
}

func (s *scriptedStandIn) Call(kind string, sent map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, map[string]any{"kind": kind, "sent": sent})
	return s.reply, s.err
}

func TestModbusReadUnderAFlowTestNeverTouchesTheNetwork(t *testing.T) {
	svc := newTestServices()
	si := &scriptedStandIn{reply: map[string]any{"values": []any{1.5, 2.5}}}
	svc.standIn = si
	n := build(t, "modbus read", `{"server":"srv","area":"holding","address":"12","addressType":"num",
		"quantity":"2","quantityType":"num","dataType":"float32"}`, svc)
	e, err := send(t, n, msg(t, `{"payload":"go"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := e.on(0)[0]
	if !reflect.DeepEqual(got.Payload(), []any{1.5, 2.5}) {
		t.Errorf("payload = %#v", got.Payload())
	}
	sent := si.calls[0]["sent"].(map[string]any)
	if si.calls[0]["kind"] != "modbus" || sent["op"] != "read" || sent["address"] != 12.0 ||
		sent["quantity"] != 2.0 || sent["dataType"] != "float32" || sent["area"] != "holding" {
		t.Errorf("the stand-in was told %#v", si.calls[0])
	}

	// A device that is down is whatever the test scripts.
	si.err = fmt.Errorf("the PLC is off")
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "the PLC is off") {
		t.Errorf("a scripted failure should reach the flow, got %v", err)
	}
}

func TestModbusWriteUnderAFlowTestSaysExactlyWhatItWouldHaveSent(t *testing.T) {
	svc := newTestServices()
	si := &scriptedStandIn{}
	svc.standIn = si
	n := build(t, "modbus write", `{"server":"srv","area":"holding","address":"30","dataType":"float32",
		"wordOrder":"low","value":"payload","valueType":"msg"}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":3.5}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	sent := si.calls[0]["sent"].(map[string]any)
	// 3.5 is 0x4060 0x0000, and low word first puts the zero word first.
	if !reflect.DeepEqual(sent["registers"], []any{0.0, float64(0x4060)}) || sent["op"] != "write" {
		t.Errorf("sent = %#v", sent)
	}
}

func TestModbusReadChecksTheRequestBeforeItBothersTheDevice(t *testing.T) {
	svc := newTestServices()
	svc.standIn = &scriptedStandIn{}
	for _, c := range []struct{ cfg, want string }{
		{`{"server":"s","address":"70000","addressType":"num"}`, "address is 70000"},
		{`{"server":"s","address":"65535","addressType":"num","quantity":"2","quantityType":"num"}`, "runs past the end"},
		{`{"server":"s","quantity":"126","quantityType":"num"}`, "allows 125"},
		{`{"server":"s","quantity":"63","quantityType":"num","dataType":"float32"}`, "126 registers in one request"},
		{`{"server":"s","area":"coil","quantity":"2001","quantityType":"num"}`, "allows 2000"},
		{`{"server":"s","address":"nope","addressType":"msg"}`, "resolved to nothing"},
	} {
		n := build(t, "modbus read", c.cfg, svc)
		_, err := send(t, n, msg(t, `{}`))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want %q", c.cfg, err, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Against a real Modbus TCP server
// ---------------------------------------------------------------------------

// The server is oitc/modbus-server, started with testdata/plant/modbus_server.json
// (see the CI workflow for the exact command). It's a pymodbus server, not a
// mock of one: the node frames real requests and the server answers with real
// responses and real exception codes.
//
//	HOTLOOP_FLOW_TEST_MODBUS=localhost:5020 go test ./internal/nodes/ -run Modbus -v

func modbusLive(t *testing.T, addr, extra string) (*credServices, func()) {
	t.Helper()
	host, port := splitHostPort(t, addr)
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cfg := build(t, "hotloop-flow-modbus", `{"host":"`+host+`","port":`+port+`,"unitId":1,"timeout":0.7`+extra+`}`, svc)
	svc.configs["srv"] = cfg
	return svc, func() { _ = cfg.(node.Closer).Close(context.Background(), false) }
}

func readOnce(t *testing.T, svc node.Services, cfg string) (any, map[string]any, error) {
	t.Helper()
	n := build(t, "modbus read", `{"server":"srv",`+cfg+`}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		return nil, nil, err
	}
	m := e.on(0)[0]
	info, _ := m.Data["modbus"].(map[string]any)
	return m.Payload(), info, nil
}

func TestModbusReadsWhatTheRealServerHolds(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_MODBUS")
	svc, done := modbusLive(t, addr, "")
	defer done()

	for _, c := range []struct {
		name string
		cfg  string
		want any
	}{
		{"holding uint16", `"area":"holding","address":"0","dataType":"uint16"`, 1234.0},
		{"holding int16 reads 0xFFFF as -1", `"area":"holding","address":"1","dataType":"int16"`, -1.0},
		{"holding float32 0x40600000", `"area":"holding","address":"10","dataType":"float32"`, 3.5},
		{"holding uint32 ABCD", `"area":"holding","address":"20","dataType":"uint32"`, 305419896.0},
		{"holding uint32 CDAB", `"area":"holding","address":"20","dataType":"uint32","wordOrder":"low"`, float64(0x56781234)},
		{"two registers as a list", `"area":"holding","address":"0","quantity":"2","dataType":"uint16"`, []any{1234.0, 65535.0}},
		{"input registers", `"area":"input","address":"0","quantity":"3","dataType":"uint16"`, []any{777.0, 778.0, 779.0}},
		{"coils", `"area":"coil","address":"0","quantity":"2"`, []any{true, false}},
		{"discrete inputs", `"area":"discrete","address":"0","quantity":"3"`, []any{true, false, true}},
	} {
		got, info, err := readOnce(t, svc, c.cfg)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
		}
		t.Logf("%s: read %#v from %v", c.name, got, info["server"])
	}
}

func TestModbusWriteIsReadBackFromTheRealServer(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_MODBUS")
	svc, done := modbusLive(t, addr, "")
	defer done()

	write := func(cfg, body string) {
		t.Helper()
		n := build(t, "modbus write", `{"server":"srv",`+cfg+`}`, svc)
		e, err := send(t, n, msg(t, body))
		if err != nil {
			t.Fatalf("write %s: %v", cfg, err)
		}
		if len(e.on(0)) != 1 {
			t.Fatalf("a write that worked should pass the message on")
		}
	}
	check := func(cfg string, want any) {
		t.Helper()
		got, _, err := readOnce(t, svc, cfg)
		if err != nil {
			t.Fatalf("read back %s: %v", cfg, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back %s = %#v, want %#v", cfg, got, want)
		}
		t.Logf("wrote then read back %s = %#v", cfg, got)
	}

	write(`"area":"holding","address":"30","dataType":"uint16","value":"payload","valueType":"msg"`, `{"payload":4321}`)
	check(`"area":"holding","address":"30","dataType":"uint16"`, 4321.0)

	write(`"area":"holding","address":"31","dataType":"int16","value":"payload","valueType":"msg"`, `{"payload":-42}`)
	check(`"area":"holding","address":"31","dataType":"int16"`, -42.0)

	write(`"area":"holding","address":"32","dataType":"float32","value":"payload","valueType":"msg"`, `{"payload":-12.25}`)
	check(`"area":"holding","address":"32","dataType":"float32"`, -12.25)

	write(`"area":"holding","address":"34","dataType":"uint32","wordOrder":"low","value":"payload","valueType":"msg"`, `{"payload":305419896}`)
	check(`"area":"holding","address":"34","dataType":"uint32","wordOrder":"low"`, 305419896.0)
	// And the same two registers read the other way round are the other number,
	// which is what proves the order reached the wire.
	check(`"area":"holding","address":"34","dataType":"uint32"`, float64(0x56781234))

	write(`"area":"holding","address":"40","dataType":"uint16","value":"payload","valueType":"msg"`, `{"payload":[10,20,30,40]}`)
	check(`"area":"holding","address":"40","quantity":"4","dataType":"uint16"`, []any{10.0, 20.0, 30.0, 40.0})

	write(`"area":"coil","address":"3","value":"payload","valueType":"msg"`, `{"payload":true}`)
	check(`"area":"coil","address":"3"`, true)
	write(`"area":"coil","address":"4","value":"payload","valueType":"msg"`, `{"payload":[true,false,true]}`)
	check(`"area":"coil","address":"4","quantity":"3"`, []any{true, false, true})
	write(`"area":"coil","address":"3","value":"payload","valueType":"msg"`, `{"payload":false}`)
	check(`"area":"coil","address":"3"`, false)
}

func TestModbusServerSaysNoAndTheErrorSaysWhatToDo(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_MODBUS")
	svc, done := modbusLive(t, addr, "")
	defer done()

	_, _, err := readOnce(t, svc, `"area":"holding","address":"5000","dataType":"uint16"`)
	if err == nil {
		t.Fatal("address 5000 is not in the server's map and should have been refused")
	}
	t.Logf("operator sees: %v", err)
	if !strings.Contains(err.Error(), "illegal data address") || !strings.Contains(err.Error(), "40001") {
		t.Errorf("the error should name the Modbus exception and explain zero based addressing: %v", err)
	}
	// An exception is an answer. The connection is fine and the next read works.
	if got, _, err := readOnce(t, svc, `"area":"holding","address":"0"`); err != nil || got != 1234.0 {
		t.Errorf("after an exception the same connection should still work: %v, %v", got, err)
	}
}

func TestModbusRefusedConnectionSaysWhatIsWrong(t *testing.T) {
	// A port nothing listens on: bind one, note it, close it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	addr := l.Addr().String()
	l.Close()

	svc, done := modbusLive(t, addr, "")
	defer done()
	_, _, err = readOnce(t, svc, `"area":"holding","address":"0"`)
	if err == nil || !strings.Contains(err.Error(), "refused the connection") ||
		!strings.Contains(err.Error(), "switched on in the device") {
		t.Errorf("error = %v", err)
	}
	t.Logf("operator sees: %v", err)
}

// flakyLine sits between the node and the real server so a test can make the
// device go silent without touching the server. The server stays real. In
// silent mode the request is swallowed on the way in, which is exactly what a
// PLC that has hung looks like from the network: the socket stays open and
// nothing ever answers.
type flakyLine struct {
	l        net.Listener
	upstream string
	silent   atomic.Bool
	mu       sync.Mutex
	conns    []net.Conn
	accepted atomic.Int32
	// sent is every byte the client sent, so a test can look at the request
	// itself and not only at what the server did with it.
	sent []byte
}

func newFlakyLine(t *testing.T, upstream string) *flakyLine {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	f := &flakyLine{l: l, upstream: upstream}
	go f.serve()
	t.Cleanup(func() { l.Close(); f.dropAll() })
	return f
}

func (f *flakyLine) addr() string { return f.l.Addr().String() }

func (f *flakyLine) serve() {
	for {
		c, err := f.l.Accept()
		if err != nil {
			return
		}
		f.accepted.Add(1)
		up, err := net.Dial("tcp", f.upstream)
		if err != nil {
			c.Close()
			continue
		}
		f.mu.Lock()
		f.conns = append(f.conns, c, up)
		f.mu.Unlock()
		go func() { _, _ = io.Copy(c, up); c.Close() }()
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if err != nil {
					up.Close()
					return
				}
				f.mu.Lock()
				f.sent = append(f.sent, buf[:n]...)
				f.mu.Unlock()
				if f.silent.Load() {
					continue
				}
				if _, err := up.Write(buf[:n]); err != nil {
					return
				}
			}
		}()
	}
}

// requests is a copy of what the client has sent so far.
func (f *flakyLine) requests() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.sent...)
}

func (f *flakyLine) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
	f.conns = nil
}

func TestModbusDeviceGoesSilentThenComesBack(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_MODBUS")
	line := newFlakyLine(t, addr)
	svc, done := modbusLive(t, line.addr(), "")
	defer done()

	read := func() (any, error) {
		got, _, err := readOnce(t, svc, `"area":"holding","address":"0"`)
		return got, err
	}
	if got, err := read(); err != nil || got != 1234.0 {
		t.Fatalf("before anything goes wrong: %v, %v", got, err)
	}
	if line.accepted.Load() != 1 {
		t.Fatalf("one connection expected so far, saw %d", line.accepted.Load())
	}

	// The device hangs with the socket still open.
	line.silent.Store(true)
	start := time.Now()
	_, err := read()
	if err == nil || !strings.Contains(err.Error(), "The device is silent") ||
		!strings.Contains(err.Error(), "unit ID 1") {
		t.Fatalf("error = %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("gave up after %s, and the timeout is 0.7s", took)
	}
	t.Logf("device silent, operator sees: %v", err)

	// Silence goes on. It keeps saying so and keeps trying, and none of it
	// wedges the node.
	if _, err := read(); err == nil || !strings.Contains(err.Error(), "silent") {
		t.Errorf("still silent, still the same message: %v", err)
	}

	// It comes back. No redeploy, no restart: the next message works.
	line.silent.Store(false)
	got, err := read()
	if err != nil || got != 1234.0 {
		t.Fatalf("after the device came back: %v, %v", got, err)
	}
	t.Logf("device back, read %v after %d connections", got, line.accepted.Load())
	if line.accepted.Load() < 2 {
		t.Errorf("a silent connection can't be trusted again, so a new one was expected, saw %d", line.accepted.Load())
	}

	// And a device that reboots, dropping every connection, comes back too.
	line.dropAll()
	if _, err := read(); err == nil {
		t.Log("the dropped connection was noticed on the first try")
	} else {
		t.Logf("operator sees on the dropped connection: %v", err)
		if !strings.Contains(err.Error(), "next message") {
			t.Errorf("the error should say the next message reconnects: %v", err)
		}
	}
	if got, err := read(); err != nil || got != 1234.0 {
		t.Fatalf("after the reboot: %v, %v", got, err)
	}
}

func TestModbusUnknownHostSaysSo(t *testing.T) {
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cfg := build(t, "hotloop-flow-modbus", `{"host":"no-such-plc.invalid","port":502,"timeout":1}`, svc)
	svc.configs["srv"] = cfg
	defer cfg.(node.Closer).Close(context.Background(), false)
	_, _, err := readOnce(t, svc, `"area":"holding","address":"0"`)
	if err == nil || !strings.Contains(err.Error(), "couldn't find a host called") {
		t.Errorf("error = %v", err)
	}
}
