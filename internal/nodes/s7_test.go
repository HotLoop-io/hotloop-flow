package nodes

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// ---------------------------------------------------------------------------
// Addresses and numbers, with no CPU at all
// ---------------------------------------------------------------------------

func TestS7AddressesAreReadTheWaySiemensWritesThem(t *testing.T) {
	for _, c := range []struct {
		in     string
		area   byte
		db     int
		at     int
		bit    int
		letter byte
	}{
		{"DB1.DBX0.0", 'D', 1, 0, 0, 'X'},
		{"db12.dbx34.7", 'D', 12, 34, 7, 'X'},
		{"DB1.DBB4", 'D', 1, 4, -1, 'B'},
		{"DB 2 .DBW 6", 'D', 2, 6, -1, 'W'},
		{"DB65535.DBD100", 'D', 65535, 100, -1, 'D'},
		{"M10.2", 'M', 0, 10, 2, 'X'},
		{"MW10", 'M', 0, 10, -1, 'W'},
		{"MD20", 'M', 0, 20, -1, 'D'},
		{"I0.0", 'I', 0, 0, 0, 'X'},
		{"E1.5", 'I', 0, 1, 5, 'X'},
		{"IB3", 'I', 0, 3, -1, 'B'},
		{"Q0.1", 'Q', 0, 0, 1, 'X'},
		{"A2.0", 'Q', 0, 2, 0, 'X'},
		{"QW4", 'Q', 0, 4, -1, 'W'},
	} {
		a, err := parseS7Address(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if a.area != c.area || a.db != c.db || a.byteAt != c.at || a.bit != c.bit || a.letter != c.letter {
			t.Errorf("%s read as %+v", c.in, a)
		}
	}
	for _, c := range []struct{ in, want string }{
		{"", "isn't one this node reads"},
		{"DB1", "isn't one this node reads"},
		{"DB0.DBW0", "data block 0"},
		{"DB1.DBW", "isn't one this node reads"},
		{"DB1.DBX0", "has to say which bit"},
		{"DB1.DBW0.1", "names a bit and a word size"},
		{"DB1.DBX0.8", "isn't one this node reads"},
		{"M10", "has no size"},
		{"DB1.DBW9999999999", "out of range"},
		{"T5", "isn't one this node reads"},
	} {
		if _, err := parseS7Address(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want it to say %q", c.in, err, c.want)
		}
	}
}

func TestS7TheSizeLetterAndTheDataTypeHaveToAgree(t *testing.T) {
	for _, c := range []struct{ addr, typ, want string }{
		{"DB1.DBW0", "float32", "Use DB1.DBD0"},
		{"DB1.DBD0", "int16", "Use DB1.DBW0"},
		{"DB1.DBX0.0", "uint8", "Use DB1.DBB0"},
		{"MW4", "bool", "Use M4.0"},
		{"DB1.DBB4", "bool", "Use DB1.DBX4.0"},
		{"IW0", "string", "Use IB0"},
	} {
		a, err := parseS7Address(c.addr)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.checkLetter(s7Types[c.typ]); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s as %s: %v, want it to say %q", c.addr, c.typ, err, c.want)
		}
	}
	for _, c := range []struct{ addr, typ string }{
		{"DB1.DBD0", "float32"}, {"DB1.DBD0", "uint32"}, {"DB1.DBB0", "int8"}, {"DB1.DBW0", "int16"},
		{"DB1.DBD0", "float64"}, {"DB1.DBB0", "int64"}, {"DB1.DBB0", "string"}, {"M0.0", "bool"}, {"DB1.DBX0.0", "bool"},
	} {
		a, _ := parseS7Address(c.addr)
		if err := a.checkLetter(s7Types[c.typ]); err != nil {
			t.Errorf("%s as %s: %v", c.addr, c.typ, err)
		}
	}
}

func TestS7EncodeThenDecodeIsTheSameNumber(t *testing.T) {
	for _, c := range []struct {
		typ string
		in  any
	}{
		{"uint8", 255.0}, {"int8", -128.0}, {"uint16", 65535.0}, {"int16", -32768.0},
		{"uint32", 4294967295.0}, {"int32", -2147483648.0}, {"float32", -12.25}, {"float64", 1.0000000001},
		{"uint64", 9007199254740992.0}, {"int64", -9007199254740992.0},
		{"uint64", "18446744073709551615"}, {"int64", "-9223372036854775808"},
	} {
		ty := s7Types[c.typ]
		b, err := s7Encode(ty, c.in)
		if err != nil || len(b) != ty.size {
			t.Fatalf("%s %v: %x, %v", c.typ, c.in, b, err)
		}
		if got := s7Decode(ty, b); got != c.in {
			t.Errorf("%s: %v came back as %#v", c.typ, c.in, got)
		}
	}
	// S7 is big endian.
	if b, _ := s7Encode(s7Types["uint16"], float64(0x1234)); !bytes.Equal(b, []byte{0x12, 0x34}) {
		t.Errorf("0x1234 as %x", b)
	}
	for _, c := range []struct {
		typ  string
		in   any
		want string
	}{
		{"uint8", 256.0, "outside what uint8 holds"},
		{"int16", 3.5, "isn't a whole number"},
		{"uint16", -1.0, "outside"},
		{"float32", 1e300, "too big for a float32"},
		{"int32", "abc", "isn't a number"},
		{"uint64", 1e20, "Past 2^53"},
	} {
		if _, err := s7Encode(s7Types[c.typ], c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: %v, want %q", c.typ, c.in, err, c.want)
		}
	}
}

func TestS7ConfigRefusesWhatCannotWork(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ typ, cfg, want string }{
		{"hotloop-flow-s7", `{}`, "host is required"},
		{"hotloop-flow-s7", `{"host":"s7://plc"}`, "no scheme, path or port"},
		{"hotloop-flow-s7", `{"host":"plc","port":0}`, "port 0 is out of range"},
		{"hotloop-flow-s7", `{"host":"plc","rack":9}`, "rack 9"},
		{"hotloop-flow-s7", `{"host":"plc","slot":40}`, "slot 40"},
		{"hotloop-flow-s7", `{"host":"plc","timeout":0}`, "timeout"},
		{"s7 read", `{}`, "no server selected"},
		{"s7 read", `{"server":"s","dataType":"int128"}`, "data type"},
		{"s7 read", `{"server":"s","address":"DB1.DBW0","dataType":"float32"}`, "Use DB1.DBD0"},
		{"s7 read", `{"server":"s","address":"nowhere"}`, "isn't one this node reads"},
		{"s7 write", `{"server":"s","address":"DB1.DBX0.0","dataType":"int16"}`, "Use DB1.DBW0"},
		{"s7 write", `{}`, "no server selected"},
	} {
		err := buildErr(t, c.typ, c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: error = %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

func TestS7ReadAndWriteUnderAFlowTestNeverTouchTheNetwork(t *testing.T) {
	svc := newTestServices()
	si := &scriptedStandIn{reply: map[string]any{"values": []any{1.5, 2.5}}}
	svc.standIn = si
	n := build(t, "s7 read", `{"server":"srv","address":"DB5.DBD8","dataType":"float32","quantity":"2","quantityType":"num"}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.on(0)[0].Payload(), []any{1.5, 2.5}) {
		t.Errorf("payload = %#v", e.on(0)[0].Payload())
	}
	sent := si.calls[0]["sent"].(map[string]any)
	if si.calls[0]["kind"] != "s7" || sent["op"] != "read" || sent["address"] != "DB5.DBD8" || sent["quantity"] != 2.0 || sent["dataType"] != "float32" {
		t.Errorf("the stand-in was told %#v", si.calls[0])
	}

	w := build(t, "s7 write", `{"server":"srv","address":"M4.1","dataType":"bool","value":"payload","valueType":"msg"}`, svc)
	if _, err := send(t, w, msg(t, `{"payload":true}`)); err != nil {
		t.Fatal(err)
	}
	sent = si.calls[1]["sent"].(map[string]any)
	if sent["op"] != "write" || sent["address"] != "M4.1" || !reflect.DeepEqual(sent["values"], []any{true}) {
		t.Errorf("sent = %#v", sent)
	}

	si.err = fmt.Errorf("the PLC is off")
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "the PLC is off") {
		t.Errorf("a scripted failure should reach the flow, got %v", err)
	}
}

func TestS7ChecksTheRequestBeforeItBothersTheCPU(t *testing.T) {
	svc := newTestServices()
	svc.standIn = &scriptedStandIn{}
	for _, c := range []struct{ typ, cfg, body, want string }{
		{"s7 read", `{"server":"s","address":"x","addressType":"msg"}`, `{}`, "resolved to nothing"},
		{"s7 read", `{"server":"s","address":"x","addressType":"msg"}`, `{"x":"DB1.DBW"}`, "isn't one this node reads"},
		{"s7 read", `{"server":"s","address":"DB1.DBW0","quantity":"3000","quantityType":"num"}`, `{}`, "bytes in one read"},
		{"s7 read", `{"server":"s","address":"DB1.DBB0","dataType":"string","quantity":"2","quantityType":"num"}`, `{}`, "one at a time"},
		{"s7 write", `{"server":"s","address":"DB1.DBW0","value":"payload","valueType":"msg"}`, `{"payload":70000}`, "outside what int16 holds"},
		{"s7 write", `{"server":"s","address":"DB1.DBW0","value":"payload","valueType":"msg"}`, `{"payload":[1,"x"]}`, "value 1:"},
		{"s7 write", `{"server":"s","address":"DB1.DBX0.0","dataType":"bool","value":"payload","valueType":"msg"}`, `{"payload":2}`, "can't be a coil value"},
		{"s7 write", `{"server":"s","address":"DB1.DBB0","dataType":"string","value":"payload","valueType":"msg"}`, `{"payload":5}`, "isn't text"},
		{"s7 write", `{"server":"s","address":"DB1.DBW0","value":"payload","valueType":"msg"}`, `{"payload":[]}`, "empty list"},
	} {
		n := build(t, c.typ, c.cfg, svc)
		_, err := send(t, n, msg(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: error = %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Against a real S7 server
// ---------------------------------------------------------------------------

// The server is python-snap7's, started from testdata/plant/s7: python-snap7
// 3.2.1 (MIT) has a server written in pure Python that speaks the S7 protocol.
// It is a different implementation from the gos7 client under test, which is
// the point. The CI workflow has the exact commands, and
// testdata/plant/s7/s7_server.py has the contents of the data blocks.
//
//	HOTLOOP_FLOW_TEST_S7=localhost:102 go test ./internal/nodes/ -run S7 -v

func s7Live(t *testing.T, addr, extra string) (*credServices, func()) {
	t.Helper()
	host, port := splitHostPort(t, addr)
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cfg := build(t, "hotloop-flow-s7", `{"host":"`+host+`","port":`+port+`,"rack":0,"slot":1,"timeout":0.7`+extra+`}`, svc)
	svc.configs["srv"] = cfg
	return svc, func() { _ = cfg.(node.Closer).Close(context.Background(), false) }
}

func s7ReadOnce(t *testing.T, svc node.Services, cfg string) (any, map[string]any, error) {
	t.Helper()
	n := build(t, "s7 read", `{"server":"srv",`+cfg+`}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		return nil, nil, err
	}
	m := e.on(0)[0]
	info, _ := m.Data["s7"].(map[string]any)
	return m.Payload(), info, nil
}

func s7WriteOnce(t *testing.T, svc node.Services, cfg, body string) error {
	t.Helper()
	n := build(t, "s7 write", `{"server":"srv",`+cfg+`,"value":"payload","valueType":"msg"}`, svc)
	e, err := send(t, n, msg(t, body))
	if err == nil && len(e.on(0)) != 1 {
		t.Fatal("a write that worked should pass the message on")
	}
	return err
}

func TestS7ReadsWhatTheRealServerHolds(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_S7")
	svc, done := s7Live(t, addr, "")
	defer done()

	for _, c := range []struct {
		name string
		cfg  string
		want any
	}{
		{"int16", `"address":"DB1.DBW0","dataType":"int16"`, 1234.0},
		{"int32", `"address":"DB1.DBD2","dataType":"int32"`, -70000.0},
		{"float32", `"address":"DB1.DBD6","dataType":"float32"`, 12.5},
		{"bit set", `"address":"DB1.DBX10.0","dataType":"bool"`, true},
		{"bit clear", `"address":"DB1.DBX10.1","dataType":"bool"`, false},
		{"three bits on from the bit named", `"address":"DB1.DBX10.0","dataType":"bool","quantity":"3","quantityType":"num"`, []any{true, false, true}},
		{"bits across a byte", `"address":"DB1.DBX10.6","dataType":"bool","quantity":"4","quantityType":"num"`, []any{false, false, false, false}},
		{"uint8", `"address":"DB1.DBB11","dataType":"uint8"`, 200.0},
		{"int8 reads 200 as -56", `"address":"DB1.DBB11","dataType":"int8"`, -56.0},
		{"uint16", `"address":"DB1.DBW12","dataType":"uint16"`, 65000.0},
		{"uint32", `"address":"DB1.DBD14","dataType":"uint32"`, 4000000000.0},
		{"float64", `"address":"DB1.DBD18","dataType":"float64"`, 3.141592653589793},
		{"int64", `"address":"DB1.DBD26","dataType":"int64"`, -5000000000.0},
		{"string", `"address":"DB1.DBB34","dataType":"string"`, "hello"},
		{"a list of INTs", `"address":"DB1.DBW56","dataType":"int16","quantity":"5","quantityType":"num"`, []any{10.0, 20.0, 30.0, 40.0, 50.0}},
		{"the same list from the middle", `"address":"DB1.DBW60","dataType":"int16","quantity":"3","quantityType":"num"`, []any{30.0, 40.0, 50.0}},
		{"merker word", `"address":"MW2","dataType":"int16"`, 4321.0},
		{"merker bit", `"address":"M5.1","dataType":"bool"`, true},
		{"input byte", `"address":"IB0","dataType":"uint8"`, 15.0},
		{"the German spelling", `"address":"EB0","dataType":"uint8"`, 15.0},
	} {
		got, info, err := s7ReadOnce(t, svc, c.cfg)
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

func TestS7WriteIsReadBackFromTheRealServer(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_S7")
	svc, done := s7Live(t, addr, "")
	defer done()

	check := func(write, wbody, read string, want any) {
		t.Helper()
		if err := s7WriteOnce(t, svc, write, wbody); err != nil {
			t.Fatalf("write %s: %v", write, err)
		}
		got, _, err := s7ReadOnce(t, svc, read)
		if err != nil {
			t.Fatalf("read back %s: %v", read, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back %s = %#v, want %#v", read, got, want)
		}
		t.Logf("wrote %s %s then read back %#v", write, wbody, got)
	}
	check(`"address":"DB2.DBW0","dataType":"int16"`, `{"payload":-4321}`, `"address":"DB2.DBW0","dataType":"int16"`, -4321.0)
	check(`"address":"DB2.DBD2","dataType":"int32"`, `{"payload":-2000000000}`, `"address":"DB2.DBD2","dataType":"int32"`, -2000000000.0)
	check(`"address":"DB2.DBD6","dataType":"float32"`, `{"payload":-12.25}`, `"address":"DB2.DBD6","dataType":"float32"`, -12.25)
	check(`"address":"DB2.DBD10","dataType":"uint32"`, `{"payload":4294967295}`, `"address":"DB2.DBD10","dataType":"uint32"`, 4294967295.0)
	check(`"address":"DB2.DBD14","dataType":"float64"`, `{"payload":2.718281828459045}`, `"address":"DB2.DBD14","dataType":"float64"`, 2.718281828459045)
	check(`"address":"DB2.DBB22","dataType":"uint8"`, `{"payload":250}`, `"address":"DB2.DBB22","dataType":"uint8"`, 250.0)
	check(`"address":"DB2.DBB24","dataType":"int64"`, `{"payload":"9007199254740993"}`, `"address":"DB2.DBB24","dataType":"int64"`, "9007199254740993")
	check(`"address":"DB2.DBW32","dataType":"uint16"`, `{"payload":[1,2,3,65535]}`, `"address":"DB2.DBW32","dataType":"uint16","quantity":"4","quantityType":"num"`, []any{1.0, 2.0, 3.0, 65535.0})
	check(`"address":"MW10","dataType":"int16"`, `{"payload":1717}`, `"address":"MW10","dataType":"int16"`, 1717.0)
	check(`"address":"QW2","dataType":"uint16"`, `{"payload":4660}`, `"address":"QW2","dataType":"uint16"`, 4660.0)

	// The same bytes read the other way round are the other number, which is
	// what proves the order reached the wire as S7 has it.
	if got, _, err := s7ReadOnce(t, svc, `"address":"DB2.DBB32","dataType":"uint8","quantity":"2","quantityType":"num"`); err != nil || !reflect.DeepEqual(got, []any{0.0, 1.0}) {
		t.Errorf("1 as a big endian word is the bytes 0 then 1: %v, %v", got, err)
	}

	// A string goes into an S7 STRING that has room for it, and is read back.
	// DB2 starts as zeros, so the first byte, the maximum length, is set first.
	if err := s7WriteOnce(t, svc, `"address":"DB2.DBB40","dataType":"uint8"`, `{"payload":10}`); err != nil {
		t.Fatal(err)
	}
	check(`"address":"DB2.DBB40","dataType":"string"`, `{"payload":"x"}`, `"address":"DB2.DBB40","dataType":"string"`, "x")
}

func TestS7StringsKeepTheirMaximumLength(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_S7")
	svc, done := s7Live(t, addr, "")
	defer done()

	// DB1.DBB34 is a STRING[20] holding "hello".
	if err := s7WriteOnce(t, svc, `"address":"DB1.DBB34","dataType":"string"`, `{"payload":"Kessel 3 laeuft nicht"}`); err == nil ||
		!strings.Contains(err.Error(), "has room for 20") {
		t.Fatalf("21 bytes into a STRING[20]: %v", err)
	} else {
		t.Logf("operator sees: %v", err)
	}
	if got, _, err := s7ReadOnce(t, svc, `"address":"DB1.DBB34","dataType":"string"`); err != nil || got != "hello" {
		t.Errorf("a refused write leaves the string alone: %v, %v", got, err)
	}
	if err := s7WriteOnce(t, svc, `"address":"DB1.DBB34","dataType":"string"`, `{"payload":"twenty chars exactly"}`); err != nil {
		t.Fatalf("20 bytes fit: %v", err)
	}
	if got, _, err := s7ReadOnce(t, svc, `"address":"DB1.DBB34","dataType":"string"`); err != nil || got != "twenty chars exactly" {
		t.Errorf("read back %v, %v", got, err)
	}
	// Put it back for the next run of the tests.
	if err := s7WriteOnce(t, svc, `"address":"DB1.DBB34","dataType":"string"`, `{"payload":"hello"}`); err != nil {
		t.Fatal(err)
	}
}

// The simulator takes a bit write and changes the whole byte, which a real CPU
// does not, so this test reads the request instead of the byte: it has to be the
// protocol's bit write, to the bit asked for, and not a byte write that would
// take the other seven bits with it.
func TestS7ABitIsWrittenAsABitAndNotAsAByte(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_S7")
	line := newFlakyLine(t, addr)
	svc, done := s7Live(t, line.addr(), "")
	defer done()

	before := len(line.requests())
	if err := s7WriteOnce(t, svc, `"address":"DB2.DBX20.3","dataType":"bool"`, `{"payload":true}`); err != nil {
		t.Fatalf("the bit write: %v", err)
	}
	req := line.requests()[before:]
	// Write Var: item syntax 0x10, transport size BIT 0x01, one item, DB 2, area
	// 0x84, the address 20*8+3 = 163 = 0x0000A3 as a bit number.
	param := []byte{0x12, 0x0a, 0x10, 0x01, 0x00, 0x01, 0x00, 0x02, 0x84, 0x00, 0x00, 0xA3}
	// Its data item: return code 0, transport size BIT 0x03, one bit, the value.
	data := []byte{0x00, 0x03, 0x00, 0x01, 0x01}
	if !bytes.Contains(req, param) || !bytes.Contains(req, data) {
		t.Fatalf("the write on the wire isn't a bit write of DB2.DBX20.3:\n% x", req)
	}
	t.Logf("on the wire: item % x, data % x", param, data)

	// Several bits go as one bit write each, counting on from the bit named and
	// over a byte boundary.
	before = len(line.requests())
	if err := s7WriteOnce(t, svc, `"address":"DB2.DBX21.6","dataType":"bool"`, `{"payload":[true,false,true]}`); err != nil {
		t.Fatal(err)
	}
	req = line.requests()[before:]
	for _, bit := range []byte{21*8 + 6, 21*8 + 7, 22*8 + 0} {
		item := []byte{0x12, 0x0a, 0x10, 0x01, 0x00, 0x01, 0x00, 0x02, 0x84, 0x00, 0x00, bit}
		if !bytes.Contains(req, item) {
			t.Errorf("no bit write item for bit number %d in\n% x", bit, req)
		}
	}
}

func TestS7CPUSaysNoAndTheErrorSaysWhatToDo(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_S7")
	svc, done := s7Live(t, addr, "")
	defer done()

	_, _, err := s7ReadOnce(t, svc, `"address":"DB99.DBW0","dataType":"int16"`)
	if err == nil {
		t.Fatal("DB99 does not exist and should have been refused")
	}
	t.Logf("a data block that isn't there, operator sees: %v", err)
	if !strings.Contains(err.Error(), "isn't available") || !strings.Contains(err.Error(), "PUT/GET") {
		t.Errorf("the error should say what the CPU said and where to look: %v", err)
	}
	// An answer is not a dead connection: the same one works for the next request.
	if got, _, err := s7ReadOnce(t, svc, `"address":"DB1.DBW0","dataType":"int16"`); err != nil || got != 1234.0 {
		t.Errorf("after a refusal the same connection should still work: %v, %v", got, err)
	}

	_, _, err = s7ReadOnce(t, svc, `"address":"DB1.DBW250","dataType":"int16","quantity":"20","quantityType":"num"`)
	if err == nil {
		t.Fatal("reading past the end of DB1 should have been refused")
	}
	t.Logf("past the end of a block, operator sees: %v", err)
	if got, _, err := s7ReadOnce(t, svc, `"address":"DB1.DBW0","dataType":"int16"`); err != nil || got != 1234.0 {
		t.Errorf("after a refusal the same connection should still work: %v, %v", got, err)
	}
}

func TestS7RefusedConnectionSaysWhatIsWrong(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	addr := l.Addr().String()
	l.Close()

	svc, done := s7Live(t, addr, "")
	defer done()
	_, _, err = s7ReadOnce(t, svc, `"address":"DB1.DBW0"`)
	if err == nil || !strings.Contains(err.Error(), "refused the connection") ||
		!strings.Contains(err.Error(), "switched on in the CPU") {
		t.Errorf("error = %v", err)
	}
	t.Logf("operator sees: %v", err)
}

// A CPU that accepts the connection and never answers. gos7 used to write a raw
// "*net.OpError ... i/o timeout" line to Go's global logger from Send every time
// that happened. The operator already gets the node's own error, so nothing
// else may reach the global logger, and the error still has to reach the flow.
func TestS7TimeoutDoesNotWriteToTheGlobalLogger(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	var logged bytes.Buffer
	var mu sync.Mutex
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}))
	defer log.SetOutput(oldOut)
	defer log.SetFlags(oldFlags)

	svc, done := s7Live(t, l.Addr().String(), "")
	_, _, err = s7ReadOnce(t, svc, `"address":"DB1.DBW0","dataType":"int16"`)
	done()
	if err == nil || !strings.Contains(err.Error(), "silent") {
		t.Fatalf("the timeout has to reach the flow as the node's own error, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if logged.Len() != 0 {
		t.Errorf("the S7 library wrote to the global logger: %q", logged.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestS7UnknownHostSaysSo(t *testing.T) {
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cfg := build(t, "hotloop-flow-s7", `{"host":"no-such-plc.invalid","timeout":1}`, svc)
	svc.configs["srv"] = cfg
	defer cfg.(node.Closer).Close(context.Background(), false)
	_, _, err := s7ReadOnce(t, svc, `"address":"DB1.DBW0"`)
	if err == nil || !strings.Contains(err.Error(), "couldn't find a host called") {
		t.Errorf("error = %v", err)
	}
}

func TestS7DeviceGoesSilentThenComesBack(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_S7")
	line := newFlakyLine(t, addr)
	svc, done := s7Live(t, line.addr(), "")
	defer done()

	read := func() (any, error) {
		got, _, err := s7ReadOnce(t, svc, `"address":"DB1.DBW0","dataType":"int16"`)
		return got, err
	}
	if got, err := read(); err != nil || got != 1234.0 {
		t.Fatalf("before anything goes wrong: %v, %v", got, err)
	}
	if line.accepted.Load() != 1 {
		t.Fatalf("one connection expected so far, saw %d", line.accepted.Load())
	}

	// The CPU hangs with the socket still open.
	line.silent.Store(true)
	start := time.Now()
	_, err := read()
	if err == nil || !strings.Contains(err.Error(), "The CPU is silent") {
		t.Fatalf("error = %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("gave up after %s, and the timeout is 0.7s", took)
	}
	t.Logf("CPU silent, operator sees: %v", err)

	// Silence goes on. It keeps saying so and keeps trying, and none of it wedges the node.
	if _, err := read(); err == nil || !strings.Contains(err.Error(), "silent") {
		t.Errorf("still silent, still the same message: %v", err)
	}

	// It comes back. No redeploy, no restart: the next message works.
	line.silent.Store(false)
	got, err := read()
	if err != nil || got != 1234.0 {
		t.Fatalf("after the CPU came back: %v, %v", got, err)
	}
	t.Logf("CPU back, read %v after %d connections", got, line.accepted.Load())
	if line.accepted.Load() < 2 {
		t.Errorf("a silent connection can't be trusted again, so a new one was expected, saw %d", line.accepted.Load())
	}

	// And a CPU that restarts, dropping every connection, comes back too.
	line.dropAll()
	if _, err := read(); err == nil {
		t.Log("the dropped connection was noticed on the first try")
	} else {
		t.Logf("operator sees on the dropped connection: %v", err)
	}
	if got, err := read(); err != nil || got != 1234.0 {
		t.Fatalf("after the restart: %v, %v", got, err)
	}
}

func TestS7NodesShareOneConnectionAndTakeTurns(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_S7")
	line := newFlakyLine(t, addr)
	svc, done := s7Live(t, line.addr(), "")
	defer done()

	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			got, _, err := s7ReadOnce(t, svc, `"address":"DB1.DBW0","dataType":"int16"`)
			if err == nil && got != 1234.0 {
				err = fmt.Errorf("read %v", got)
			}
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Errorf("%v", err)
		}
	}
	if n := line.accepted.Load(); n != 1 {
		t.Errorf("eight nodes opened %d connections, want the one they share", n)
	}
}
