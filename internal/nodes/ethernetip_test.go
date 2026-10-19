package nodes

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// ---------------------------------------------------------------------------
// Values and configuration, with no controller at all
// ---------------------------------------------------------------------------

func TestEtherNetIPConvertsToTheTypeTheTagHolds(t *testing.T) {
	for _, c := range []struct {
		typ  string
		in   any
		want any
	}{
		{"bool", true, true}, {"bool", 0.0, false}, {"bool", "on", true},
		{"int8", -128.0, int8(-128)}, {"uint8", 255.0, byte(255)},
		{"int16", -32768.0, int16(-32768)}, {"uint16", 65535.0, uint16(65535)},
		{"int32", -2147483648.0, int32(-2147483648)}, {"uint32", 4294967295.0, uint32(4294967295)},
		{"int64", "-9223372036854775808", int64(-9223372036854775808)},
		{"uint64", "18446744073709551615", uint64(18446744073709551615)},
		{"int64", -5000000000.0, int64(-5000000000)},
		{"float32", 1.5, float32(1.5)}, {"float64", 2.718281828459045, 2.718281828459045},
		{"string", "Kessel 3", "Kessel 3"},
	} {
		got, err := enipConvert(enipTypes[c.typ], c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v: %#v, %v; want %#v", c.typ, c.in, got, err, c.want)
		}
	}
	for _, c := range []struct {
		typ  string
		in   any
		want string
	}{
		{"int8", 128.0, "outside what int8 holds"},
		{"uint16", -1.0, "outside what uint16 holds"},
		{"int32", 1.5, "isn't a whole number"},
		{"float32", 1e300, "too big for a float32"},
		{"uint32", "abc", "isn't a number"},
		{"uint64", 1e20, "Past 2^53"},
		{"string", 5.0, "isn't text"},
		{"bool", 2.0, "can't be a coil value"},
	} {
		if _, err := enipConvert(enipTypes[c.typ], c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: %v, want %q", c.typ, c.in, err, c.want)
		}
	}
	// What was read goes back to the flow as the numbers a flow has.
	if enipFlowValue(int8(-5)) != -5.0 || enipFlowValue(float32(0.5)) != 0.5 || enipFlowValue(uint64(1<<60)) != "1152921504606846976" {
		t.Error("values read from a tag don't come back as a flow's numbers")
	}
}

func TestEtherNetIPConfigRefusesWhatCannotWork(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ typ, cfg, want string }{
		{"hotloop-flow-ethernetip", `{}`, "host is required"},
		{"hotloop-flow-ethernetip", `{"host":"enip://plc"}`, "no scheme, path or port"},
		{"hotloop-flow-ethernetip", `{"host":"plc","port":0}`, "port 0 is out of range"},
		{"hotloop-flow-ethernetip", `{"host":"plc","path":"one,two"}`, "isn't a CIP route"},
		{"hotloop-flow-ethernetip", `{"host":"plc","timeout":0}`, "timeout"},
		{"ethernetip read", `{}`, "no controller selected"},
		{"ethernetip read", `{"server":"s"}`, "tag is required"},
		{"ethernetip read", `{"server":"s","tag":"A","dataType":"int128"}`, "data type"},
		{"ethernetip write", `{}`, "no controller selected"},
		{"ethernetip write", `{"server":"s"}`, "tag is required"},
	} {
		err := buildErr(t, c.typ, c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: error = %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

func TestEtherNetIPReadAndWriteUnderAFlowTestNeverTouchTheNetwork(t *testing.T) {
	svc := newTestServices()
	si := &scriptedStandIn{reply: map[string]any{"values": []any{1.5, 2.5}}}
	svc.standIn = si
	n := build(t, "ethernetip read", `{"server":"srv","tag":"Pump1.Speed","dataType":"float32","quantity":"2","quantityType":"num"}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e.on(0)[0].Payload(), []any{1.5, 2.5}) {
		t.Errorf("payload = %#v", e.on(0)[0].Payload())
	}
	sent := si.calls[0]["sent"].(map[string]any)
	if si.calls[0]["kind"] != "ethernetip" || sent["op"] != "read" || sent["tag"] != "Pump1.Speed" || sent["quantity"] != 2.0 || sent["dataType"] != "float32" {
		t.Errorf("the stand-in was told %#v", si.calls[0])
	}
	w := build(t, "ethernetip write", `{"server":"srv","tag":"Samples[2]","dataType":"int16","value":"payload","valueType":"msg"}`, svc)
	if _, err := send(t, w, msg(t, `{"payload":[7,8]}`)); err != nil {
		t.Fatal(err)
	}
	sent = si.calls[1]["sent"].(map[string]any)
	if sent["op"] != "write" || sent["tag"] != "Samples[2]" || !reflect.DeepEqual(sent["values"], []any{7.0, 8.0}) {
		t.Errorf("sent = %#v", sent)
	}
	si.err = fmt.Errorf("the PLC is off")
	if _, err := send(t, n, msg(t, `{}`)); err == nil || !strings.Contains(err.Error(), "the PLC is off") {
		t.Errorf("a scripted failure should reach the flow, got %v", err)
	}
}

func TestEtherNetIPChecksTheRequestBeforeItBothersTheController(t *testing.T) {
	svc := newTestServices()
	svc.standIn = &scriptedStandIn{}
	for _, c := range []struct{ typ, cfg, body, want string }{
		{"ethernetip read", `{"server":"s","tag":"x","tagType":"msg"}`, `{}`, "resolved to nothing"},
		{"ethernetip read", `{"server":"s","tag":"x","tagType":"msg"}`, `{"x":"a b"}`, "has a space"},
		{"ethernetip read", `{"server":"s","tag":"A","quantity":"5000","quantityType":"num"}`, `{}`, "has to be 1 to 2000"},
		{"ethernetip read", `{"server":"s","tag":"A","dataType":"string","quantity":"2","quantityType":"num"}`, `{}`, "one at a time"},
		{"ethernetip write", `{"server":"s","tag":"A","dataType":"int16","value":"payload","valueType":"msg"}`, `{"payload":70000}`, "outside what int16 holds"},
		{"ethernetip write", `{"server":"s","tag":"A","dataType":"int16","value":"payload","valueType":"msg"}`, `{"payload":[1,"x"]}`, "value 1:"},
		{"ethernetip write", `{"server":"s","tag":"A","dataType":"int16","value":"payload","valueType":"msg"}`, `{"payload":[]}`, "empty list"},
		{"ethernetip write", `{"server":"s","tag":"A","dataType":"string","value":"payload","valueType":"msg"}`, `{"payload":["a","b"]}`, "one at a time"},
	} {
		n := build(t, c.typ, c.cfg, svc)
		_, err := send(t, n, msg(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: error = %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Against a real EtherNet/IP server
// ---------------------------------------------------------------------------

// The server is ab_server from libplctag 2.7.2, the simulator libplctag's own
// tests run against, built by testdata/plant/ethernetip/Dockerfile and started
// with the tags in the CI workflow. It answers as a ControlLogix over CIP. It
// is a different implementation from the gologix client under test. Its tags
// start at zero, so what these tests read back is what they wrote. It matches
// tag names with their case, and gologix sends them in lower case, so the tags
// are defined in lower case.
//
//	HOTLOOP_FLOW_TEST_ENIP=localhost:44818 go test ./internal/nodes/ -run EtherNetIP -v

func enipLive(t *testing.T, addr, extra string) (*credServices, func()) {
	t.Helper()
	host, port := splitHostPort(t, addr)
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cfg := build(t, "hotloop-flow-ethernetip", `{"host":"`+host+`","port":`+port+`,"path":"1,0","timeout":0.7`+extra+`}`, svc)
	svc.configs["srv"] = cfg
	return svc, func() { _ = cfg.(node.Closer).Close(context.Background(), false) }
}

func enipReadOnce(t *testing.T, svc node.Services, cfg string) (any, map[string]any, error) {
	t.Helper()
	n := build(t, "ethernetip read", `{"server":"srv",`+cfg+`}`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		return nil, nil, err
	}
	m := e.on(0)[0]
	info, _ := m.Data["ethernetip"].(map[string]any)
	return m.Payload(), info, nil
}

func enipWriteOnce(t *testing.T, svc node.Services, cfg, body string) error {
	t.Helper()
	n := build(t, "ethernetip write", `{"server":"srv",`+cfg+`,"value":"payload","valueType":"msg"}`, svc)
	e, err := send(t, n, msg(t, body))
	if err == nil && len(e.on(0)) != 1 {
		t.Fatal("a write that worked should pass the message on")
	}
	return err
}

func TestEtherNetIPWriteIsReadBackFromTheRealServer(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_ENIP")
	svc, done := enipLive(t, addr, "")
	defer done()

	check := func(tag, typ, body string, want any, qty string) {
		t.Helper()
		cfg := `"tag":"` + tag + `","dataType":"` + typ + `"`
		if err := enipWriteOnce(t, svc, cfg, body); err != nil {
			t.Fatalf("write %s: %v", tag, err)
		}
		rcfg := cfg
		if qty != "" {
			rcfg += `,"quantity":"` + qty + `","quantityType":"num"`
		}
		got, info, err := enipReadOnce(t, svc, rcfg)
		if err != nil {
			t.Fatalf("read back %s: %v", tag, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back %s = %#v, want %#v", tag, got, want)
		}
		t.Logf("wrote %s %s then read back %#v from %v", tag, body, got, info["server"])
	}
	// The names are spelled the way a project spells them. The tags in the
	// server are lower case, and the library lower-cases what it sends.
	check("TestSint", "int8", `{"payload":-100}`, -100.0, "")
	check("TestInt", "int16", `{"payload":-30000}`, -30000.0, "")
	check("TestDint", "int32", `{"payload":-2000000000}`, -2000000000.0, "")
	check("TestLint", "int64", `{"payload":-5000000000}`, -5000000000.0, "")
	check("TestLint", "int64", `{"payload":"9007199254740993"}`, "9007199254740993", "")
	check("TestReal", "float32", `{"payload":-12.25}`, -12.25, "")
	check("TestLreal", "float64", `{"payload":2.718281828459045}`, 2.718281828459045, "")
	check("TestBool", "bool", `{"payload":true}`, true, "")
	check("TestBool", "bool", `{"payload":false}`, false, "")
	check("TestString", "string", `{"payload":"Kessel 3 laeuft"}`, "Kessel 3 laeuft", "")
	check("TestDintArr", "int32", `{"payload":[1,2,3,4,5,6,7,8,9,10]}`, []any{1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0, 10.0}, "10")
	check("TestRealArr", "float32", `{"payload":[0.5,1.5,2.5,3.5,4.5]}`, []any{0.5, 1.5, 2.5, 3.5, 4.5}, "5")

	// From the middle of an array, and one element of it.
	if got, _, err := enipReadOnce(t, svc, `"tag":"TestDintArr[3]","dataType":"int32","quantity":"4","quantityType":"num"`); err != nil || !reflect.DeepEqual(got, []any{4.0, 5.0, 6.0, 7.0}) {
		t.Errorf("four from element 3: %v, %v", got, err)
	}
	if err := enipWriteOnce(t, svc, `"tag":"TestDintArr[8]","dataType":"int32"`, `{"payload":[88,99]}`); err != nil {
		t.Fatalf("writing from the middle: %v", err)
	}
	if got, _, err := enipReadOnce(t, svc, `"tag":"TestDintArr[7]","dataType":"int32","quantity":"3","quantityType":"num"`); err != nil || !reflect.DeepEqual(got, []any{8.0, 88.0, 99.0}) {
		t.Errorf("the elements either side are untouched: %v, %v", got, err)
	}
}

func TestEtherNetIPControllerSaysNoAndTheErrorSaysWhatToDo(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_ENIP")
	svc, done := enipLive(t, addr, "")
	defer done()

	_, _, err := enipReadOnce(t, svc, `"tag":"NoSuchTag","dataType":"int32"`)
	if err == nil {
		t.Fatal("a tag that isn't there should have been refused")
	}
	t.Logf("a tag that isn't there, operator sees: %v", err)
	if !strings.Contains(err.Error(), "NoSuchTag") && !strings.Contains(err.Error(), "tag") {
		t.Errorf("the error should be about the tag: %v", err)
	}
	// An answer is not a dead connection.
	if err := enipWriteOnce(t, svc, `"tag":"TestDint","dataType":"int32"`, `{"payload":7}`); err != nil {
		t.Fatalf("after a refusal the next request should work: %v", err)
	}
	if got, _, err := enipReadOnce(t, svc, `"tag":"TestDint","dataType":"int32"`); err != nil || got != 7.0 {
		t.Errorf("after a refusal: %v, %v", got, err)
	}

	err = enipWriteOnce(t, svc, `"tag":"NoSuchTag","dataType":"int32"`, `{"payload":1}`)
	if err == nil {
		t.Fatal("a write to a tag that isn't there should have been refused")
	}
	t.Logf("writing a tag that isn't there, operator sees: %v", err)

	// A tag read as the wrong type.
	_, _, err = enipReadOnce(t, svc, `"tag":"TestDint","dataType":"float64"`)
	t.Logf("a DINT read as an LREAL, operator sees: %v", err)
	if err == nil || !strings.Contains(err.Error(), "different data type") {
		t.Errorf("a tag read as the wrong type should say so: %v", err)
	}
}

func TestEtherNetIPRefusedConnectionSaysWhatIsWrong(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	addr := l.Addr().String()
	l.Close()

	svc, done := enipLive(t, addr, "")
	defer done()
	_, _, err = enipReadOnce(t, svc, `"tag":"TestDint"`)
	if err == nil || !strings.Contains(err.Error(), "refused the connection") ||
		!strings.Contains(err.Error(), "switched on in the controller") {
		t.Errorf("error = %v", err)
	}
	t.Logf("operator sees: %v", err)
}

func TestEtherNetIPUnknownHostSaysSo(t *testing.T) {
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	cfg := build(t, "hotloop-flow-ethernetip", `{"host":"no-such-plc.invalid","timeout":1}`, svc)
	svc.configs["srv"] = cfg
	defer cfg.(node.Closer).Close(context.Background(), false)
	_, _, err := enipReadOnce(t, svc, `"tag":"TestDint"`)
	if err == nil || !strings.Contains(err.Error(), "couldn't find a host called") {
		t.Errorf("error = %v", err)
	}
}

func TestEtherNetIPDeviceGoesSilentThenComesBack(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_ENIP")
	line := newFlakyLine(t, addr)
	svc, done := enipLive(t, line.addr(), "")
	defer done()

	if err := enipWriteOnce(t, svc, `"tag":"TestInt","dataType":"int16"`, `{"payload":1234}`); err != nil {
		t.Fatal(err)
	}
	read := func() (any, error) {
		got, _, err := enipReadOnce(t, svc, `"tag":"TestInt","dataType":"int16"`)
		return got, err
	}
	if got, err := read(); err != nil || got != 1234.0 {
		t.Fatalf("before anything goes wrong: %v, %v", got, err)
	}
	if line.accepted.Load() != 1 {
		t.Fatalf("one connection expected so far, saw %d", line.accepted.Load())
	}

	// The controller hangs with the socket still open.
	line.silent.Store(true)
	start := time.Now()
	_, err := read()
	if err == nil || !strings.Contains(err.Error(), "The controller is silent") {
		t.Fatalf("error = %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("gave up after %s, and the timeout is 0.7s", took)
	}
	t.Logf("controller silent, operator sees: %v", err)

	// Silence goes on. It keeps saying so and keeps trying, and none of it wedges the node.
	if _, err := read(); err == nil || !strings.Contains(err.Error(), "silent") {
		t.Errorf("still silent, still the same message: %v", err)
	}

	// It comes back. No redeploy, no restart: the next message works.
	line.silent.Store(false)
	got, err := read()
	if err != nil || got != 1234.0 {
		t.Fatalf("after the controller came back: %v, %v", got, err)
	}
	t.Logf("controller back, read %v after %d connections", got, line.accepted.Load())
	if line.accepted.Load() < 2 {
		t.Errorf("a silent connection can't be trusted again, so a new one was expected, saw %d", line.accepted.Load())
	}

	// And a controller that restarts, dropping every connection, comes back too.
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
		t.Fatalf("after the restart: %v, %v", got, err)
	}
}

func TestEtherNetIPNodesShareOneConnectionAndTakeTurns(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_ENIP")
	line := newFlakyLine(t, addr)
	svc, done := enipLive(t, line.addr(), "")
	defer done()
	if err := enipWriteOnce(t, svc, `"tag":"TestInt","dataType":"int16"`, `{"payload":1234}`); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := enipReadOnce(t, svc, `"tag":"TestInt","dataType":"int16"`)
			if err == nil && got != 1234.0 {
				err = fmt.Errorf("read %v", got)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("%v", err)
		}
	}
	if n := line.accepted.Load(); n != 1 {
		t.Errorf("eight nodes opened %d connections, want the one they share", n)
	}
}
