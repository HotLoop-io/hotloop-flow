package nodes

import (
	"context"
	"io"
	"log"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/simonvetter/modbus"
)

// A real Modbus TCP server on a loopback port. What the nodes do against it
// is what they do against a PLC: the bytes go over a socket.

type nrDevice struct {
	mu       sync.Mutex
	coils    [200]bool
	discrete [200]bool
	holding  [200]uint16
	input    [200]uint16
	units    []uint8
}

func (d *nrDevice) HandleCoils(r *modbus.CoilsRequest) ([]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.units = append(d.units, r.UnitId)
	if int(r.Addr)+int(r.Quantity) > len(d.coils) {
		return nil, modbus.ErrIllegalDataAddress
	}
	if r.IsWrite {
		copy(d.coils[r.Addr:], r.Args)
		return nil, nil
	}
	return append([]bool(nil), d.coils[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}
func (d *nrDevice) HandleDiscreteInputs(r *modbus.DiscreteInputsRequest) ([]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if int(r.Addr)+int(r.Quantity) > len(d.discrete) {
		return nil, modbus.ErrIllegalDataAddress
	}
	return append([]bool(nil), d.discrete[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}
func (d *nrDevice) HandleHoldingRegisters(r *modbus.HoldingRegistersRequest) ([]uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.units = append(d.units, r.UnitId)
	if int(r.Addr)+int(r.Quantity) > len(d.holding) {
		return nil, modbus.ErrIllegalDataAddress
	}
	if r.IsWrite {
		copy(d.holding[r.Addr:], r.Args)
		return nil, nil
	}
	return append([]uint16(nil), d.holding[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}
func (d *nrDevice) HandleInputRegisters(r *modbus.InputRegistersRequest) ([]uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if int(r.Addr)+int(r.Quantity) > len(d.input) {
		return nil, modbus.ErrIllegalDataAddress
	}
	return append([]uint16(nil), d.input[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}

func startDevice(t *testing.T) (*nrDevice, int) {
	t.Helper()
	d := &nrDevice{}
	port := freePort(t)
	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL: "tcp://127.0.0.1:" + itoa(port), Timeout: time.Minute, Logger: log.New(io.Discard, "", 0),
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return d, port
}

func itoa(n int) string { return strconv.Itoa(n) }

// configServices is testServices that can hand back config nodes.
type configServices struct {
	*testServices
	mu      sync.Mutex
	configs map[string]node.Node
}

func (s *configServices) ConfigNode(id string) (node.Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.configs[id]
	return n, ok
}

func newConfigServices() *configServices {
	return &configServices{testServices: newTestServices(), configs: map[string]node.Node{}}
}

// buildConfig builds a config node, which has no wires or position.
func buildConfig(t *testing.T, typ, id, configJSON string, svc node.Services) node.Node {
	t.Helper()
	reg, ok := node.Default.Lookup(typ)
	if !ok {
		t.Fatalf("node type %q is not registered", typ)
	}
	raw := parseNodeConfig(t, typ, configJSON).Raw
	delete(raw, "x")
	delete(raw, "y")
	delete(raw, "wires")
	delete(raw, "z")
	raw["id"] = id
	n, err := reg.New(&node.Definition{Node: &engine.Node{ID: id, Type: typ, IsConfig: true, Raw: raw}, Services: svc})
	if err != nil {
		t.Fatalf("building %s: %v", typ, err)
	}
	return n
}

func nrClient(t *testing.T, svc *configServices, port int, extra string) {
	t.Helper()
	cfg := `{"clienttype":"tcp","tcpHost":"127.0.0.1","tcpPort":"` + itoa(port) + `","tcpType":"DEFAULT","unit_id":1,"clientTimeout":1000` + extra + `}`
	c := buildConfig(t, "modbus-client", "cl", cfg, svc)
	svc.configs["cl"] = c
	t.Cleanup(func() { _ = c.(node.Closer).Close(context.Background(), true) })
}

func firstOn(t *testing.T, e *testEmitter, port int) map[string]any {
	t.Helper()
	got := e.on(port)
	if len(got) == 0 {
		t.Fatalf("nothing on output %d (errors %v, on 1: %v)", port+1, e.errs, e.on(1))
	}
	return got[0].Data
}

func TestModbusGetterReadsRegistersAndCoilsOverTCP(t *testing.T) {
	dev, port := startDevice(t)
	copy(dev.holding[10:], []uint16{17087, 13107, 7})
	dev.coils[5], dev.coils[7] = true, true
	svc := newConfigServices()
	nrClient(t, svc, port, "")

	n := build(t, "modbus-getter", `{"server":"cl","dataType":"HoldingRegister","adr":"10","quantity":"3","unitid":""}`, svc)
	e, err := send(t, n, msg(t, `{"payload":"go","topic":"asked"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := firstOn(t, e, 0)
	if !reflect.DeepEqual(m["payload"], []any{17087.0, 13107.0, 7.0}) {
		t.Fatalf("payload = %#v", m["payload"])
	}
	rb := m["responseBuffer"].(map[string]any)
	if !reflect.DeepEqual(rb["buffer"], []byte{0x42, 0xBF, 0x33, 0x33, 0, 7}) {
		t.Fatalf("responseBuffer.buffer = %#v", rb["buffer"])
	}
	if in := m["input"].(map[string]any); in["payload"] != "go" || in["topic"] != "asked" {
		t.Fatalf("msg.input = %#v, want the message that asked", in)
	}
	if _, kept := m["topic"]; kept {
		t.Fatalf("without keepMsgProperties the topic should not carry over, got %v", m["topic"])
	}

	nc := build(t, "modbus-getter", `{"server":"cl","dataType":"Coil","adr":"4","quantity":"5","keepMsgProperties":true}`, svc)
	e, err = send(t, nc, msg(t, `{"payload":1,"topic":"kept"}`))
	if err != nil {
		t.Fatal(err)
	}
	m = firstOn(t, e, 0)
	if !reflect.DeepEqual(m["payload"], []any{false, true, false, true, false}) {
		t.Fatalf("coils = %#v", m["payload"])
	}
	if m["topic"] != "kept" {
		t.Fatalf("keepMsgProperties dropped the topic: %#v", m)
	}
}

func TestModbusGetterUsesTheNodesUnitId(t *testing.T) {
	dev, port := startDevice(t)
	svc := newConfigServices()
	nrClient(t, svc, port, "")
	n := build(t, "modbus-getter", `{"server":"cl","dataType":"HoldingRegister","adr":"0","quantity":"1","unitid":"7"}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err != nil {
		t.Fatal(err)
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	if len(dev.units) == 0 || dev.units[len(dev.units)-1] != 7 {
		t.Fatalf("unit ids the device saw: %v, want 7", dev.units)
	}
}

func TestModbusFlexGetterTakesTheRequestFromThePayload(t *testing.T) {
	dev, port := startDevice(t)
	copy(dev.holding[0:], []uint16{1, 2, 3, 4})
	copy(dev.input[3:], []uint16{99})
	dev.discrete[2] = true
	svc := newConfigServices()
	nrClient(t, svc, port, "")
	n := build(t, "modbus-flex-getter", `{"server":"cl"}`, svc)

	for _, c := range []struct {
		payload string
		want    any
	}{
		{`{"fc":3,"unitid":1,"address":0,"quantity":4}`, []any{1.0, 2.0, 3.0, 4.0}},
		{`{"fc":"4","unitid":"1","address":"3","quantity":"1"}`, []any{99.0}},
		{`{"fc":2,"address":1,"quantity":2}`, []any{false, true}},
	} {
		e, err := send(t, n, msg(t, `{"payload":`+c.payload+`}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := firstOn(t, e, 0)["payload"]; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s gave %#v, want %#v", c.payload, got, c.want)
		}
	}

	e, _ := send(t, n, msg(t, `{"payload":{"fc":6,"address":0,"quantity":1}}`))
	if len(e.on(1)) != 1 || len(e.on(0)) != 0 {
		t.Fatalf("a write function code in a getter should fail out the second output, got %d and %d", len(e.on(0)), len(e.on(1)))
	}
}

func TestModbusWriteSetsCoilsAndRegisters(t *testing.T) {
	dev, port := startDevice(t)
	svc := newConfigServices()
	nrClient(t, svc, port, "")

	coil := build(t, "modbus-write", `{"server":"cl","dataType":"Coil","adr":"9","quantity":"1"}`, svc)
	e, err := send(t, coil, msg(t, `{"payload":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstOn(t, e, 0)["payload"]; got != true {
		t.Fatalf("the write's output payload = %v, want what was sent", got)
	}
	dev.mu.Lock()
	ok := dev.coils[9]
	dev.mu.Unlock()
	if !ok {
		t.Fatal("coil 9 was not set on the device")
	}

	regs := build(t, "modbus-write", `{"server":"cl","dataType":"HoldingRegister","adr":"20","quantity":"3"}`, svc)
	if _, err := send(t, regs, msg(t, `{"payload":[1,65535,-2]}`)); err != nil {
		t.Fatal(err)
	}
	dev.mu.Lock()
	got := dev.holding[20:23]
	dev.mu.Unlock()
	if !reflect.DeepEqual(got, []uint16{1, 65535, 65534}) {
		t.Fatalf("registers = %v", got)
	}

	one := build(t, "modbus-write", `{"server":"cl","dataType":"HoldingRegister","adr":"30","quantity":"1"}`, svc)
	if _, err := send(t, one, msg(t, `{"payload":"123"}`)); err != nil {
		t.Fatal(err)
	}
	dev.mu.Lock()
	v := dev.holding[30]
	dev.mu.Unlock()
	if v != 123 {
		t.Fatalf("holding[30] = %d", v)
	}
}

func TestModbusFlexWriteHonorsTheFunctionCode(t *testing.T) {
	dev, port := startDevice(t)
	svc := newConfigServices()
	nrClient(t, svc, port, "")
	n := build(t, "modbus-flex-write", `{"server":"cl"}`, svc)

	for _, p := range []string{
		`{"value":321,"fc":6,"unitid":1,"address":28,"quantity":1}`,
		`{"value":[5,6,7],"fc":16,"unitid":1,"address":40,"quantity":3}`,
		`{"value":true,"fc":5,"unitid":1,"address":3,"quantity":1}`,
		`{"value":[true,false,true],"fc":15,"unitid":1,"address":50,"quantity":3}`,
	} {
		e, err := send(t, n, msg(t, `{"payload":`+p+`}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(e.on(1)) != 0 {
			t.Fatalf("%s failed: %v", p, e.on(1)[0].Data["error"])
		}
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	if dev.holding[28] != 321 || !reflect.DeepEqual(dev.holding[40:43], []uint16{5, 6, 7}) || !dev.coils[3] ||
		!reflect.DeepEqual(dev.coils[50:53], []bool{true, false, true}) {
		t.Fatalf("device state wrong: %v %v %v %v", dev.holding[28], dev.holding[40:43], dev.coils[3], dev.coils[50:53])
	}
}

func TestModbusFailureGoesOutTheSecondOutput(t *testing.T) {
	_, port := startDevice(t)
	svc := newConfigServices()
	nrClient(t, svc, port, "")

	n := build(t, "modbus-getter", `{"server":"cl","dataType":"HoldingRegister","adr":"500","quantity":"1","emptyMsgOnFail":true,"showErrors":true}`, svc)
	e, err := send(t, n, msg(t, `{"payload":1}`))
	if err != nil {
		t.Fatal(err)
	}
	em := firstOn(t, e, 1)
	if !strings.Contains(em["error"].(map[string]any)["message"].(string), "illegal data address") {
		t.Fatalf("error = %v", em["error"])
	}
	if first := e.on(0); len(first) != 1 || first[0].Data["payload"] != "" {
		t.Fatalf("emptyMsgOnFail should send an empty message out the first output, got %v", first)
	}
	if len(e.errs) != 1 {
		t.Fatalf("showErrors should raise the error for a Catch node, got %d", len(e.errs))
	}

	quiet := build(t, "modbus-getter", `{"server":"cl","dataType":"HoldingRegister","adr":"500","quantity":"1"}`, svc)
	e, _ = send(t, quiet, msg(t, `{}`))
	if len(e.errs) != 0 || len(e.on(1)) != 1 {
		t.Fatalf("without showErrors: %d errors and %d on the error output", len(e.errs), len(e.on(1)))
	}
}

func TestModbusReadPollsOnItsRateAndStopsOnClose(t *testing.T) {
	dev, port := startDevice(t)
	dev.holding[0] = 41
	svc := newConfigServices()
	nrClient(t, svc, port, "")
	n := build(t, "modbus-read", `{"server":"cl","dataType":"HoldingRegister","adr":"0","quantity":"1","rate":"20","rateUnit":"ms","topic":"sensor/a"}`, svc)
	e := newTestEmitter()
	if err := n.(node.Starter).Start(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.on(0)) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(e.on(0)) < 3 {
		t.Fatalf("only %d polls", len(e.on(0)))
	}
	m := e.on(0)[0].Data
	if m["topic"] != "sensor/a" || !reflect.DeepEqual(m["payload"], []any{41.0}) {
		t.Fatalf("poll message = %#v", m)
	}
	_ = n.(node.Closer).Close(context.Background(), false)
	time.Sleep(50 * time.Millisecond)
	count := len(e.on(0))
	time.Sleep(100 * time.Millisecond)
	if len(e.on(0)) != count {
		t.Fatal("the node kept polling after Close")
	}
}

func TestModbusReadWaitsOutItsStartDelay(t *testing.T) {
	dev, port := startDevice(t)
	dev.holding[0] = 1
	svc := newConfigServices()
	nrClient(t, svc, port, "")
	n := build(t, "modbus-read", `{"server":"cl","dataType":"HoldingRegister","adr":"0","quantity":"1","rate":"10","rateUnit":"ms","delayOnStart":true,"startDelayTime":"0.3"}`, svc)
	e := newTestEmitter()
	_ = n.(node.Starter).Start(context.Background(), e)
	defer n.(node.Closer).Close(context.Background(), false)
	time.Sleep(150 * time.Millisecond)
	if len(e.on(0)) != 0 {
		t.Fatal("polled before the start delay was up")
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(e.on(0)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(e.on(0)) == 0 {
		t.Fatal("never polled after the start delay")
	}
}

func TestModbusClientValidation(t *testing.T) {
	svc := newConfigServices()
	reg, _ := node.Default.Lookup("modbus-client")
	try := func(raw map[string]any) error {
		_, err := reg.New(&node.Definition{Node: &engine.Node{ID: "c", Type: "modbus-client", IsConfig: true, Raw: raw}, Services: svc})
		return err
	}
	if err := try(map[string]any{"clienttype": "tcp", "tcpHost": ""}); err == nil || !strings.Contains(err.Error(), "tcpHost") {
		t.Errorf("empty host: %v", err)
	}
	if err := try(map[string]any{"clienttype": "tcp", "tcpHost": "10.0.0.5", "tcpPort": "70000"}); err == nil {
		t.Error("port out of range accepted")
	}
	if err := try(map[string]any{"clienttype": "serial", "serialPort": "/dev/ttyUSB0", "serialType": "ASCII"}); err == nil {
		t.Error("serial ASCII accepted")
	}
	if err := try(map[string]any{"clienttype": "serial", "serialPort": "/dev/ttyUSB0", "serialType": "RTU-BUFFERD",
		"serialBaudrate": "19200", "serialParity": "even"}); err != nil {
		t.Errorf("serial RTU rejected: %v", err)
	}
	// The shape the SBF flows carry, with every extra property it has.
	if err := try(map[string]any{"clienttype": "tcp", "bufferCommands": true, "tcpHost": "192.168.50.11", "tcpPort": "502",
		"tcpType": "DEFAULT", "serialPort": "/dev/ttyUSB", "serialType": "RTU-BUFFERD", "serialBaudrate": "9600",
		"unit_id": 1.0, "commandDelay": 1.0, "clientTimeout": 1000.0, "reconnectOnTimeout": true, "reconnectTimeout": 2000.0}); err != nil {
		t.Errorf("the captured modbus-client shape was rejected: %v", err)
	}
}

func TestModbusNodeRefusesAnIOFile(t *testing.T) {
	svc := newConfigServices()
	if err := buildErr(t, "modbus-getter", `{"server":"cl","dataType":"Coil","adr":"0","quantity":"1","useIOFile":true,"ioFile":"x"}`, svc); err == nil {
		t.Fatal("an I/O file should stop the node, not be ignored")
	}
}

func TestModbusServerNodeServesAndReportsClientWrites(t *testing.T) {
	port := freePort(t)
	svc := newConfigServices()
	n := build(t, "modbus-server", `{"hostname":"127.0.0.1","serverPort":"`+itoa(port)+`","responseDelay":"0","delayUnit":"ms","coilsBufferSize":"100","holdingBufferSize":"100","inputBufferSize":"100","discreteBufferSize":"100"}`, svc)
	e := newTestEmitter()
	if err := n.(node.Starter).Start(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	defer n.(node.Closer).Close(context.Background(), false)

	if _, err := send(t, n, msg(t, `{"payload":{"register":"holding","address":4,"value":[11,22]}}`)); err != nil {
		t.Fatal(err)
	}
	cl, err := modbus.NewClient(&modbus.ClientConfiguration{URL: "tcp://127.0.0.1:" + itoa(port), Timeout: time.Second, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Open(); err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	got, err := cl.ReadRegisters(4, 2, modbus.HOLDING_REGISTER)
	if err != nil || !reflect.DeepEqual(got, []uint16{11, 22}) {
		t.Fatalf("client read %v, %v", got, err)
	}
	if err := cl.WriteRegister(10, 77); err != nil {
		t.Fatal(err)
	}
	if err := cl.WriteCoil(3, true); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.ReadRegisters(500, 1, modbus.HOLDING_REGISTER); err == nil {
		t.Fatal("a read past the buffer should be an illegal data address")
	}
	hold := firstOn(t, e, 0)["payload"].(map[string]any)
	if hold["register"] != "holding" || hold["address"] != 10.0 || hold["value"] != 77.0 {
		t.Fatalf("holding write event = %#v", hold)
	}
	coil := firstOn(t, e, 1)["payload"].(map[string]any)
	if coil["register"] != "coils" || coil["value"] != true {
		t.Fatalf("coil write event = %#v", coil)
	}
}

// The reason these nodes exist: a captured flow's own function nodes read what
// the getter sends. This is the SBF float: two registers, a Buffer, readFloatBE.
func TestModbusGetterFeedsTheFunctionNodeThatBuildsAFloat(t *testing.T) {
	dev, port := startDevice(t)
	dev.holding[2], dev.holding[3] = 17087, 13107 // 95.6 as the plant wrote it
	svc := newConfigServices()
	nrClient(t, svc, port, "")
	g := build(t, "modbus-getter", `{"server":"cl","dataType":"HoldingRegister","adr":"0","quantity":"4"}`, svc)
	e, err := send(t, g, msg(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	fn := build(t, "function", `{"func":"const buf = Buffer.from(msg.responseBuffer.buffer);\nconst b2 = Buffer.alloc(4);\nb2.writeUInt16BE(msg.payload[2], 0);\nb2.writeUInt16BE(msg.payload[3], 2);\nmsg.payload = [buf.readFloatBE(4), b2.readFloatBE(0)];\nreturn msg;","outputs":1}`, svc)
	e2, err := send(t, fn, e.on(0)[0])
	if err != nil {
		t.Fatal(err)
	}
	got := e2.on(0)[0].Payload().([]any)
	for _, v := range got {
		if f := float32(v.(float64)); f != float32(95.6) {
			t.Fatalf("float = %v, want 95.6 (both ways)", got)
		}
	}
}

// The same nodes against the real simulator CI runs (oitc/modbus-server, which
// is pymodbus), with the registers in testdata/plant/modbus_server.json.
//
//	HOTLOOP_FLOW_TEST_MODBUS=127.0.0.1:5020 go test ./internal/nodes/ -run NodeRedModbusSimulator -v
func TestNodeRedModbusSimulator(t *testing.T) {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_MODBUS")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	svc := newConfigServices()
	c := buildConfig(t, "modbus-client", "cl", `{"clienttype":"tcp","tcpHost":"`+host+`","tcpPort":"`+port+`","unit_id":1,"clientTimeout":3000}`, svc)
	svc.configs["cl"] = c
	defer c.(node.Closer).Close(context.Background(), true)

	get := func(cfg string) map[string]any {
		t.Helper()
		e, err := send(t, build(t, "modbus-getter", `{"server":"cl",`+cfg+`}`, svc), msg(t, `{}`))
		if err != nil {
			t.Fatal(err)
		}
		return firstOn(t, e, 0)
	}
	if got := get(`"dataType":"HoldingRegister","adr":"0","quantity":"2"`)["payload"]; !reflect.DeepEqual(got, []any{1234.0, 65535.0}) {
		t.Errorf("holding = %#v", got)
	}
	if got := get(`"dataType":"InputRegister","adr":"0","quantity":"3"`)["payload"]; !reflect.DeepEqual(got, []any{777.0, 778.0, 779.0}) {
		t.Errorf("input = %#v", got)
	}
	if got := get(`"dataType":"Coil","adr":"0","quantity":"2"`)["payload"]; !reflect.DeepEqual(got, []any{true, false}) {
		t.Errorf("coils = %#v", got)
	}
	if got := get(`"dataType":"DiscreteInput","adr":"0","quantity":"3"`)["payload"]; !reflect.DeepEqual(got, []any{true, false, true}) {
		t.Errorf("discrete = %#v", got)
	}
	// 10 and 11 hold float32 3.5 (0x4060, 0x0000).
	m := get(`"dataType":"HoldingRegister","adr":"10","quantity":"2"`)
	fn := build(t, "function", `{"func":"msg.payload = Buffer.from(msg.responseBuffer.buffer).readFloatBE(0); return msg;","outputs":1}`, svc)
	e, err := send(t, fn, engine.WrapMsg(m))
	if err != nil || e.on(0)[0].Payload() != 3.5 {
		t.Errorf("float32 through Buffer = %v, %v", e.on(0), err)
	}

	// Write, read back, put it as it was.
	w := build(t, "modbus-flex-write", `{"server":"cl"}`, svc)
	for _, v := range []string{"4321", "1234"} {
		e, err := send(t, w, msg(t, `{"payload":{"value":`+v+`,"fc":6,"unitid":1,"address":0,"quantity":1}}`))
		if err != nil || len(e.on(1)) != 0 {
			t.Fatalf("write %s: %v %v", v, err, e.on(1))
		}
		if v == "4321" {
			if got := get(`"dataType":"HoldingRegister","adr":"0","quantity":"1"`)["payload"]; !reflect.DeepEqual(got, []any{4321.0}) {
				t.Errorf("read back %#v", got)
			}
		}
	}
}
