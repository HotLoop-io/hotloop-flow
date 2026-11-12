package nodes

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/netbind"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/simonvetter/modbus"
)

// The Modbus node types a Node-RED flow uses: modbus-client, modbus-read,
// modbus-getter, modbus-flex-getter, modbus-write, modbus-flex-write,
// modbus-server and modbustcp-server.
//
// They take Node-RED's type names and property names so a captured flow loads,
// runs and saves unchanged. The bytes on the wire come from
// github.com/simonvetter/modbus (MIT), the same library Flow's own Modbus nodes
// use. The nodes themselves are written here from the behavior the package
// documents; none of node-red-contrib-modbus's code is in them.

func init() {
	registerNRModbusClient()
	registerNRModbusTCPServerConfig()
	registerNRModbusNode(nrGetter)
	registerNRModbusNode(nrFlexGetter)
	registerNRModbusNode(nrRead)
	registerNRModbusNode(nrWrite)
	registerNRModbusNode(nrFlexWrite)
	registerNRModbusServer()
}

// ---------------------------------------------------------------------------
// The client config node
// ---------------------------------------------------------------------------

type nrModbusClient struct {
	serial     bool
	host       string
	port       int
	url        string
	serialPort string
	baud       uint
	dataBits   uint
	stopBits   uint
	parity     uint
	unit       uint8
	timeout    time.Duration
	iface      string // the network interface its sockets bind to, unless a request names another

	mu    sync.Mutex
	conns map[string]*nrConn // one connection for each interface a request has used
}

// nrConn is one connection to the device, on one interface. Each has its own
// lock, so a device that does not answer on one VLAN does not hold up another.
type nrConn struct {
	mu sync.Mutex
	cl *modbus.ModbusClient
}

var _ modbusTarget = (*nrModbusClient)(nil)

func registerNRModbusClient() {
	node.MustRegister(node.Descriptor{
		Type:      "modbus-client",
		Category:  node.CategoryConfig,
		Color:     colorNetwork,
		Icon:      "network",
		IsConfig:  true,
		LabelProp: "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "TCP, RTU over TCP and serial RTU clients. It connects on the first request, " +
				"drops the connection on any failure that makes it untrustworthy and reconnects on the " +
				"next one, so reconnectOnTimeout and reconnectTimeout have nothing to wait for. Requests " +
				"take turns on the one connection, so commandDelay, bufferCommands and the queue and " +
				"state log switches have no effect. Serial ASCII (RTU-BUFFERD is accepted, ASCII is not) " +
				"is not supported. networkInterface (not in Node-RED) binds the TCP socket to one " +
				"network interface, for a box whose port carries several VLANs with the same subnet.",
			UnsupportedProps: []string{"bufferCommands", "stateLogEnabled", "queueLogEnabled",
				"failureLogEnabled", "commandDelay", "reconnectOnTimeout", "reconnectTimeout",
				"parallelUnitIdsAllowed", "serialConnectionDelay", "serialAsciiResponseStartDelimiter"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "clienttype", Kind: node.PropSelect, Label: "Type", Default: "tcp", Options: []node.Option{
				{Value: "tcp", Label: "TCP"}, {Value: "serial", Label: "Serial"}}},
			{Name: "tcpHost", Kind: node.PropString, Label: "Host", Placeholder: "10.20.1.15"},
			{Name: "tcpPort", Kind: node.PropString, Label: "Port", Default: "502"},
			{Name: "tcpType", Kind: node.PropSelect, Label: "TCP type", Default: "DEFAULT", Options: []node.Option{
				{Value: "DEFAULT", Label: "Modbus TCP"}, {Value: "TELNET", Label: "RTU over TCP (telnet)"},
				{Value: "RTU-BUFFERD", Label: "RTU over TCP (buffered)"}}},
			{Name: "serialPort", Kind: node.PropString, Label: "Serial port", Default: "/dev/ttyUSB"},
			{Name: "serialType", Kind: node.PropString, Label: "Serial type", Default: "RTU-BUFFERD"},
			{Name: "serialBaudrate", Kind: node.PropString, Label: "Baud rate", Default: "9600"},
			{Name: "serialDatabits", Kind: node.PropString, Label: "Data bits", Default: "8"},
			{Name: "serialStopbits", Kind: node.PropString, Label: "Stop bits", Default: "1"},
			{Name: "serialParity", Kind: node.PropSelect, Label: "Parity", Default: "none", Options: []node.Option{
				{Value: "none", Label: "None"}, {Value: "even", Label: "Even"}, {Value: "odd", Label: "Odd"}}},
			{Name: "unit_id", Kind: node.PropString, Label: "Unit ID", Default: "1"},
			{Name: "clientTimeout", Kind: node.PropString, Label: "Timeout (ms)", Default: "1000"},
			networkInterfaceProp("The interface this device is on, such as enp2s0.14. Empty lets the host choose. A request from a node on a tab that sets " + NetworkInterfaceEnv + ", or a node with its own interface, uses that one instead, on a connection of its own."),
		},
		Help: "One Modbus device, over TCP or a serial port. The Modbus nodes pointing at it share one " +
			"connection for each network interface and take turns on it.",
	}, newNRModbusClient)
}

func newNRModbusClient(def *node.Definition) (node.Node, error) {
	p := def.Node
	c := &nrModbusClient{conns: map[string]*nrConn{}}
	c.iface = strings.TrimSpace(p.PropString("networkInterface", ""))
	if err := netbind.Validate(c.iface); err != nil {
		return nil, err
	}
	kind := strings.ToLower(p.PropString("clienttype", "tcp"))
	c.timeout = time.Duration(p.PropFloat("clientTimeout", 1000)) * time.Millisecond
	if c.timeout <= 0 {
		return nil, fmt.Errorf("clientTimeout has to be more than zero milliseconds")
	}
	unit := p.PropInt("unit_id", 1)
	if unit < 0 || unit > 255 {
		return nil, fmt.Errorf("unit_id %d is out of range, it has to be 0 to 255", unit)
	}
	c.unit = uint8(unit)

	switch kind {
	case "tcp":
		c.host = strings.TrimSpace(p.PropString("tcpHost", ""))
		if c.host == "" {
			return nil, fmt.Errorf("tcpHost is required: the IP address or name of the Modbus device")
		}
		if strings.ContainsAny(c.host, "/ ") {
			return nil, fmt.Errorf("tcpHost %q should be just an address like 10.20.1.15, with no scheme, path or port", c.host)
		}
		c.port = p.PropInt("tcpPort", 502)
		if c.port < 1 || c.port > 65535 {
			return nil, fmt.Errorf("tcpPort %d is out of range, it has to be 1 to 65535", c.port)
		}
		scheme := "tcp"
		switch strings.ToUpper(p.PropString("tcpType", "DEFAULT")) {
		case "DEFAULT", "":
		case "TELNET", "RTU-BUFFERD", "RTU-BUFFERED":
			scheme = "rtuovertcp"
		default:
			return nil, fmt.Errorf("tcpType %q is not supported, use DEFAULT, TELNET or RTU-BUFFERD", p.PropString("tcpType", ""))
		}
		c.url = scheme + "://" + net.JoinHostPort(c.host, strconv.Itoa(c.port))
	case "serial":
		c.serial = true
		c.serialPort = strings.TrimSpace(p.PropString("serialPort", ""))
		if c.serialPort == "" {
			return nil, fmt.Errorf("serialPort is required")
		}
		if t := strings.ToUpper(p.PropString("serialType", "RTU-BUFFERD")); strings.Contains(t, "ASCII") {
			return nil, fmt.Errorf("serial type %q is not supported: Flow speaks Modbus RTU on a serial port, not ASCII", t)
		}
		c.baud = uint(p.PropInt("serialBaudrate", 9600))
		c.dataBits = uint(p.PropInt("serialDatabits", 8))
		c.stopBits = uint(p.PropInt("serialStopbits", 1))
		switch strings.ToLower(p.PropString("serialParity", "none")) {
		case "none", "":
			c.parity = modbus.PARITY_NONE
		case "even":
			c.parity = modbus.PARITY_EVEN
		case "odd":
			c.parity = modbus.PARITY_ODD
		default:
			return nil, fmt.Errorf("serialParity %q is not none, even or odd", p.PropString("serialParity", ""))
		}
		c.url = "rtu://" + c.serialPort
	default:
		return nil, fmt.Errorf("clienttype %q is not tcp or serial", kind)
	}
	return c, nil
}

func (c *nrModbusClient) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (c *nrModbusClient) address() string {
	if c.serial {
		return c.serialPort
	}
	return net.JoinHostPort(c.host, strconv.Itoa(c.port))
}
func (c *nrModbusClient) defaultUnit() uint8            { return c.unit }
func (c *nrModbusClient) requestTimeout() time.Duration { return c.timeout }

func (c *nrModbusClient) Close(context.Context, bool) error {
	c.mu.Lock()
	conns := c.conns
	c.conns = map[string]*nrConn{}
	c.mu.Unlock()
	var first error
	for _, nc := range conns {
		nc.mu.Lock()
		if nc.cl != nil {
			if err := nc.cl.Close(); err != nil && first == nil {
				first = err
			}
			nc.cl = nil
		}
		nc.mu.Unlock()
	}
	return first
}

func (c *nrModbusClient) do(unit *uint8, fn func(*modbus.ModbusClient) error) error {
	return c.doOn("", unit, fn)
}

// ifaceTarget is a Modbus target that can open a connection on a named network
// interface, a request at a time.
type ifaceTarget interface {
	doOn(iface string, unit *uint8, fn func(*modbus.ModbusClient) error) error
}

// effectiveInterface is the interface a request uses: the one it names, else
// the client's own.
func (c *nrModbusClient) effectiveInterface(iface string) string {
	if iface != "" {
		return iface
	}
	return c.iface
}

// doOn runs fn on the connection for an interface, opening it first if there is
// none. Each interface has its own connection and its own lock.
func (c *nrModbusClient) doOn(iface string, unit *uint8, fn func(*modbus.ModbusClient) error) error {
	iface = c.effectiveInterface(iface)
	c.mu.Lock()
	nc := c.conns[iface]
	if nc == nil {
		nc = &nrConn{}
		c.conns[iface] = nc
	}
	c.mu.Unlock()

	nc.mu.Lock()
	defer nc.mu.Unlock()
	if nc.cl == nil {
		conf := &modbus.ClientConfiguration{
			URL:      c.url,
			Timeout:  c.timeout,
			Speed:    c.baud,
			DataBits: c.dataBits,
			StopBits: c.stopBits,
			Parity:   c.parity,
			Logger:   log.New(io.Discard, "", 0),
		}
		if iface != "" && !c.serial {
			conf.Dialer = func(network, address string, timeout time.Duration) (net.Conn, error) {
				return netbind.DialTimeout(iface, network, address, timeout)
			}
		}
		cl, err := modbus.NewClient(conf)
		if err != nil {
			return err
		}
		if err := cl.Open(); err != nil {
			return err
		}
		nc.cl = cl
	}
	u := c.unit
	if unit != nil {
		u = *unit
	}
	if err := nc.cl.SetUnitId(u); err != nil {
		return err
	}
	err := fn(nc.cl)
	if err != nil && !modbusKeepsConnection(err) {
		_ = nc.cl.Close()
		nc.cl = nil
	}
	return err
}

// modbustcp-server is the config node of node-red-contrib-modbustcp's read and
// write nodes. Nothing in a flow that uses the nodes here points at it, so it
// is accepted and holds its settings.
func registerNRModbusTCPServerConfig() {
	node.MustRegister(node.Descriptor{
		Type:      "modbustcp-server",
		Category:  node.CategoryConfig,
		Color:     colorNetwork,
		Icon:      "network",
		IsConfig:  true,
		LabelProp: "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Accepted so a flow that carries it loads and saves unchanged. The node types that " +
				"read from it (modbustcp-read and modbustcp-write) are not provided, so it connects to nothing. " +
				"Use modbus-client with the getter and write nodes.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "host", Kind: node.PropString, Label: "Host"},
			{Name: "port", Kind: node.PropString, Label: "Port", Default: "502"},
			{Name: "unit_id", Kind: node.PropString, Label: "Unit ID", Default: "1"},
			{Name: "reconnecttimeout", Kind: node.PropString, Label: "Reconnect timeout"},
		},
		Help: "Settings for a Modbus TCP device used by node-red-contrib-modbustcp's nodes.",
	}, func(*node.Definition) (node.Node, error) { return &nrPassiveConfig{}, nil })
}

type nrPassiveConfig struct{}

func (*nrPassiveConfig) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// Function codes the nodes issue.
const (
	fcReadCoils    = 1
	fcReadDiscrete = 2
	fcReadHolding  = 3
	fcReadInput    = 4
	fcWriteCoil    = 5
	fcWriteReg     = 6
	fcWriteCoils   = 15
	fcWriteRegs    = 16
)

type nrRequest struct {
	fc       int
	unit     *uint8
	address  int
	quantity int
	value    any // writes
}

type nrResult struct {
	words []uint16
	bits  []bool
}

func (r nrRequest) isWrite() bool {
	return r.fc == fcWriteCoil || r.fc == fcWriteReg || r.fc == fcWriteCoils || r.fc == fcWriteRegs
}

func (r nrRequest) areaName() string {
	switch r.fc {
	case fcReadCoils, fcWriteCoil, fcWriteCoils:
		return "coil"
	case fcReadDiscrete:
		return "discrete"
	case fcReadInput:
		return "input"
	}
	return "holding"
}

// nrFC maps a dataType name to the read function code.
func nrReadFC(dataType string) (int, error) {
	switch dataType {
	case "Coil":
		return fcReadCoils, nil
	case "DiscreteInput":
		return fcReadDiscrete, nil
	case "HoldingRegister", "":
		return fcReadHolding, nil
	case "InputRegister":
		return fcReadInput, nil
	}
	return 0, fmt.Errorf("dataType %q is not Coil, DiscreteInput, HoldingRegister or InputRegister", dataType)
}

// nrInt reads a number the way a flow writes one: a number, or a string of one.
func nrInt(v any, what string) (int, error) {
	switch t := v.(type) {
	case float64:
		return int(t), nil
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, fmt.Errorf("%s is empty", what)
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f), nil
		}
	}
	return 0, fmt.Errorf("%s has to be a number, got %v", what, v)
}

func nrUnit(v any) (*uint8, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return nil, nil
	}
	n, err := nrInt(v, "unitid")
	if err != nil {
		return nil, err
	}
	if n < 0 || n > 255 {
		return nil, fmt.Errorf("unitid %d is out of range, it has to be 0 to 255", n)
	}
	u := uint8(n)
	return &u, nil
}

func nrBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case float64:
		return t != 0, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "on":
			return true, nil
		case "false", "0", "off", "":
			return false, nil
		}
	}
	return false, fmt.Errorf("a coil takes true or false, got %v", v)
}

func nrWord(v any) (uint16, error) {
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case bool:
		if t {
			f = 1
		}
	case string:
		x, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, fmt.Errorf("a register takes a number, got %q", t)
		}
		f = x
	default:
		return 0, fmt.Errorf("a register takes a number, got %v", v)
	}
	if f < -32768 || f > 65535 {
		return 0, fmt.Errorf("%v does not fit in a 16 bit register (-32768 to 65535)", f)
	}
	// A negative number is the two's complement register, as a PLC holds it.
	return uint16(int32(f)), nil
}

func nrList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok
}

// exec runs one request against a device.
func nrExec(svc node.Services, tgt modbusTarget, iface string, req nrRequest) (nrResult, string, error) {
	var res nrResult
	if req.quantity < 1 {
		return res, "", fmt.Errorf("quantity has to be 1 or more")
	}
	if req.address < 0 || req.address > 65535 {
		return res, "", fmt.Errorf("address %d is out of range, it has to be 0 to 65535", req.address)
	}
	isBits := req.areaName() == "coil" || req.areaName() == "discrete"
	if !req.isWrite() {
		limit, unitName := modbusMaxReadWords, "registers"
		if isBits {
			limit, unitName = modbusMaxReadBits, "bits"
		}
		if req.quantity > limit {
			return res, "", fmt.Errorf("that is %d %s in one request, and Modbus allows %d at most. Split it into more than one read",
				req.quantity, unitName, limit)
		}
	}
	if req.address+req.quantity > 65536 {
		return res, "", fmt.Errorf("address %d plus %d values runs past the end of the address space (65535)", req.address, req.quantity)
	}

	// Writes: turn the value into what goes on the wire before touching the device.
	var bits []bool
	var words []uint16
	if req.isWrite() {
		items, isList := nrList(req.value)
		if !isList {
			items = []any{req.value}
		}
		single := req.fc == fcWriteCoil || req.fc == fcWriteReg
		if single && (isList && len(items) != 1) {
			return res, "", fmt.Errorf("function code %d writes one value and got %d. Use %d for several", req.fc, len(items),
				map[int]int{fcWriteCoil: fcWriteCoils, fcWriteReg: fcWriteRegs}[req.fc])
		}
		if len(items) != req.quantity {
			if len(items) == 1 && !isList && !single {
				return res, "", fmt.Errorf("quantity is %d and the value is a single one. Send a list of %d values", req.quantity, req.quantity)
			}
			if !single {
				return res, "", fmt.Errorf("quantity is %d and the value list has %d", req.quantity, len(items))
			}
		}
		for _, it := range items {
			if isBits {
				b, err := nrBool(it)
				if err != nil {
					return res, "", err
				}
				bits = append(bits, b)
			} else {
				w, err := nrWord(it)
				if err != nil {
					return res, "", err
				}
				words = append(words, w)
			}
		}
	}

	what := fmt.Sprintf("function code %d at %d, quantity %d", req.fc, req.address, req.quantity)
	if ic, ok := tgt.(interface{ effectiveInterface(string) string }); ok {
		iface = ic.effectiveInterface(iface)
	}
	if iface != "" {
		what += " on network interface " + iface
	}
	if si := node.StandInOf(svc); si != nil {
		sent := map[string]any{"op": "read", "area": req.areaName(), "address": float64(req.address),
			"quantity": float64(req.quantity), "fc": float64(req.fc)}
		if req.isWrite() {
			sent["op"] = "write"
			if isBits {
				sent["values"] = boolsToAny(bits)
			} else {
				vs := make([]any, len(words))
				for i, w := range words {
					vs[i] = float64(w)
				}
				sent["values"] = vs
			}
		}
		reply, err := si.Call("modbus", sent)
		if err != nil {
			return res, "stand-in", fmt.Errorf("%s: %w", what, err)
		}
		if !req.isWrite() {
			if l, err := replyList(reply, "values"); err != nil {
				return res, "stand-in", err
			} else if l != nil {
				for _, v := range l {
					if isBits {
						b, _ := nrBool(v)
						res.bits = append(res.bits, b)
					} else {
						w, _ := nrWord(v)
						res.words = append(res.words, w)
					}
				}
			}
			for isBits && len(res.bits) < req.quantity {
				res.bits = append(res.bits, false)
			}
			for !isBits && len(res.words) < req.quantity {
				res.words = append(res.words, 0)
			}
		} else {
			res.bits, res.words = bits, words
		}
		return res, "stand-in", nil
	}

	addr := tgt.address()
	unit := tgt.defaultUnit()
	if req.unit != nil {
		unit = *req.unit
	}
	exec := func(c *modbus.ModbusClient) error {
		var err error
		a := uint16(req.address)
		q := uint16(req.quantity)
		switch req.fc {
		case fcReadCoils:
			res.bits, err = c.ReadCoils(a, q)
		case fcReadDiscrete:
			res.bits, err = c.ReadDiscreteInputs(a, q)
		case fcReadHolding:
			res.words, err = c.ReadRegisters(a, q, modbus.HOLDING_REGISTER)
		case fcReadInput:
			res.words, err = c.ReadRegisters(a, q, modbus.INPUT_REGISTER)
		case fcWriteCoil:
			err = c.WriteCoil(a, bits[0])
		case fcWriteCoils:
			err = c.WriteCoils(a, bits)
		case fcWriteReg:
			err = c.WriteRegister(a, words[0])
		case fcWriteRegs:
			err = c.WriteRegisters(a, words)
		default:
			err = fmt.Errorf("function code %d is not one of 1, 2, 3, 4, 5, 6, 15 or 16", req.fc)
		}
		return err
	}
	var err error
	if it, ok := tgt.(ifaceTarget); ok {
		err = it.doOn(iface, req.unit, exec)
	} else {
		err = tgt.do(req.unit, exec)
	}
	if err != nil {
		return res, addr, explainModbus(addr, unit, tgt.requestTimeout(), what, err)
	}
	if req.isWrite() {
		res.bits, res.words = bits, words
	}
	return res, addr, nil
}

// registerBytes is how a run of registers sits on the wire, big endian.
func registerBytes(ws []uint16) []byte {
	out := make([]byte, 0, len(ws)*2)
	for _, w := range ws {
		out = append(out, byte(w>>8), byte(w))
	}
	return out
}

// bitBytes packs coils the way a Modbus response does, first coil in bit 0.
func bitBytes(bs []bool) []byte {
	out := make([]byte, (len(bs)+7)/8)
	for i, b := range bs {
		if b {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The five client nodes
// ---------------------------------------------------------------------------

type nrKind int

const (
	nrGetter nrKind = iota
	nrFlexGetter
	nrRead
	nrWrite
	nrFlexWrite
)

func (k nrKind) typeName() string {
	return [...]string{"modbus-getter", "modbus-flex-getter", "modbus-read", "modbus-write", "modbus-flex-write"}[k]
}

type nrModbusNode struct {
	kind   nrKind
	cfgID  string
	svc    node.Services
	target modbusTarget

	// Configured request (getter, read, write).
	dataType string
	address  int
	quantity int
	unit     *uint8

	iface       string // the node's own network interface; a tab's or the client's applies when empty
	keepMsg     bool
	emptyOnFail bool
	showErrors  bool
	showStatus  bool
	topic       string

	// modbus-read polling.
	rate       time.Duration
	startDelay time.Duration

	mu     sync.Mutex
	timer  node.Timer
	closed bool
}

func registerNRModbusNode(kind nrKind) {
	common := []node.Prop{
		{Name: "name", Kind: node.PropString, Label: "Name"},
		{Name: "server", Kind: node.PropConfigRef, Label: "Server", ConfigType: "modbus-client", Required: true},
		{Name: "showStatusActivities", Kind: node.PropBool, Label: "Show activities in the status"},
		{Name: "showErrors", Kind: node.PropBool, Label: "Show errors"},
		{Name: "showWarnings", Kind: node.PropBool, Label: "Show warnings", Default: true},
		{Name: "emptyMsgOnFail", Kind: node.PropBool, Label: "Send an empty message on failure"},
		{Name: "keepMsgProperties", Kind: node.PropBool, Label: "Keep the incoming message's properties"},
		{Name: "delayOnStart", Kind: node.PropBool, Label: "Delay on start"},
		{Name: "startDelayTime", Kind: node.PropString, Label: "Start delay (seconds)"},
		networkInterfaceProp("Overrides the tab's " + NetworkInterfaceEnv + " and the client's interface for this node's requests."),
	}
	registerProps := []node.Prop{
		{Name: "dataType", Kind: node.PropSelect, Label: "Type", Default: "HoldingRegister", Options: []node.Option{
			{Value: "Coil", Label: "Coil"}, {Value: "DiscreteInput", Label: "Discrete input"},
			{Value: "HoldingRegister", Label: "Holding register"}, {Value: "InputRegister", Label: "Input register"}}},
		{Name: "adr", Kind: node.PropString, Label: "Address", Default: "0"},
		{Name: "quantity", Kind: node.PropString, Label: "Quantity", Default: "1"},
		{Name: "unitid", Kind: node.PropString, Label: "Unit ID", Help: "Empty uses the server's."},
	}
	var (
		props        []node.Prop
		outputs      = 2
		help, notes  string
		unsupported  = []string{"useIOFile", "ioFile", "useIOForPayload", "logIOActivities"}
		paletteLabel string
	)
	switch kind {
	case nrGetter:
		props = append(append([]node.Prop{}, common...), registerProps...)
		paletteLabel = "modbus getter"
		help = "Reads coils, discrete inputs, holding registers or input registers when a message arrives. " +
			"msg.payload is the list of values (numbers for registers, booleans for bits), msg.responseBuffer " +
			"holds the same as raw bytes, msg.input is the message that asked."
		notes = "Reads on a message, from the node's own address and quantity. The output carries msg.payload, " +
			"msg.responseBuffer ({data, buffer}), msg.input and msg.modbusRequest. A failure goes out the " +
			"second output with msg.error, and out the first as an empty message when emptyMsgOnFail is set. " +
			"The I/O file (useIOFile) is not supported, and a node that asks for one does not start."
	case nrFlexGetter:
		props = append([]node.Prop{}, common...)
		paletteLabel = "modbus flex getter"
		help = "Reads what the message says to read: msg.payload = {fc, unitid, address, quantity}, with fc 1, 2, 3 or 4."
		notes = "Takes {fc, unitid, address, quantity} from msg.payload. Output and failures as the Getter. " +
			"The I/O file (useIOFile) is not supported, and a node that asks for one does not start."
	case nrRead:
		props = append(append([]node.Prop{}, common...), registerProps...)
		props = append(props,
			node.Prop{Name: "topic", Kind: node.PropString, Label: "Topic"},
			node.Prop{Name: "rate", Kind: node.PropString, Label: "Poll rate", Default: "1"},
			node.Prop{Name: "rateUnit", Kind: node.PropSelect, Label: "Poll unit", Default: "s", Options: []node.Option{
				{Value: "ms", Label: "milliseconds"}, {Value: "s", Label: "seconds"},
				{Value: "m", Label: "minutes"}, {Value: "h", Label: "hours"}}})
		paletteLabel = "modbus read"
		help = "Reads on a timer, and on a message too. msg.topic is the node's topic. Output as the Getter."
		notes = "Polls every rate rateUnit once the flow starts (after startDelayTime when delayOnStart is " +
			"set), and reads when a message arrives. Output as the Getter, with msg.topic set from the node. " +
			"The I/O file (useIOFile) is not supported, and a node that asks for one does not start."
	case nrWrite:
		props = append(append([]node.Prop{}, common...), registerProps...)
		paletteLabel = "modbus write"
		help = "Writes msg.payload to coils or holding registers at the node's address. A list writes several."
		notes = "Writes msg.payload (a value, or a list the length of quantity) with function code 5, 6, 15 " +
			"or 16, whichever fits. The output is the incoming message with msg.responseBuffer, and failures " +
			"go out the second output."
		unsupported = []string{"logIOActivities"}
	case nrFlexWrite:
		props = append([]node.Prop{}, common...)
		paletteLabel = "modbus flex write"
		help = "Writes what the message says: msg.payload = {value, fc, unitid, address, quantity}, with fc 5, 6, 15 or 16."
		notes = "Takes {value, fc, unitid, address, quantity} from msg.payload. Output and failures as the Write node."
		unsupported = nil
	}
	notes += " networkInterface (not in Node-RED) binds this node's connection to one network interface. " +
		"Without it the node's tab's " + NetworkInterfaceEnv + ", then the client's interface, then the host's choice."
	node.MustRegister(node.Descriptor{
		Type:         kind.typeName(),
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      outputs,
		PaletteLabel: paletteLabel,
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level:            node.CompatPartial,
			Notes:            notes,
			UnsupportedProps: unsupported,
		},
		Props: props,
		Help:  help,
	}, func(def *node.Definition) (node.Node, error) { return newNRModbusNode(kind, def) })
}

func newNRModbusNode(kind nrKind, def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &nrModbusNode{
		kind:        kind,
		cfgID:       p.PropString("server", ""),
		svc:         def.Services,
		dataType:    p.PropString("dataType", "HoldingRegister"),
		keepMsg:     p.PropBool("keepMsgProperties", false),
		emptyOnFail: p.PropBool("emptyMsgOnFail", false),
		showErrors:  p.PropBool("showErrors", false),
		showStatus:  p.PropBool("showStatusActivities", false),
		topic:       p.PropString("topic", ""),
		iface:       strings.TrimSpace(p.PropString("networkInterface", "")),
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the Modbus client this node reads from or writes to")
	}
	if err := netbind.Validate(n.iface); err != nil {
		return nil, err
	}
	if p.PropBool("useIOFile", false) || p.PropBool("useIOForPayload", false) {
		return nil, fmt.Errorf("an I/O file (useIOFile) is not supported. Put the addresses in the node, or use the flex node and send them")
	}
	if kind == nrGetter || kind == nrRead || kind == nrWrite {
		var err error
		if n.address, err = nrInt(strOr(p.Raw["adr"], "0"), "adr"); err != nil {
			return nil, err
		}
		if n.quantity, err = nrInt(strOr(p.Raw["quantity"], "1"), "quantity"); err != nil {
			return nil, err
		}
		if n.unit, err = nrUnit(p.Raw["unitid"]); err != nil {
			return nil, err
		}
		if n.address < 0 || n.address > 65535 {
			return nil, fmt.Errorf("adr %d is out of range, it has to be 0 to 65535", n.address)
		}
		if n.quantity < 1 {
			return nil, fmt.Errorf("quantity has to be 1 or more")
		}
		if kind == nrWrite {
			if n.dataType != "Coil" && n.dataType != "HoldingRegister" {
				return nil, fmt.Errorf("a write node writes a Coil or a HoldingRegister, not %q", n.dataType)
			}
		} else if _, err := nrReadFC(n.dataType); err != nil {
			return nil, err
		}
	}
	if kind == nrRead {
		rate := p.PropFloat("rate", 1)
		unit := time.Second
		switch p.PropString("rateUnit", "s") {
		case "ms":
			unit = time.Millisecond
		case "s", "":
		case "m":
			unit = time.Minute
		case "h":
			unit = time.Hour
		default:
			return nil, fmt.Errorf("rateUnit %q is not ms, s, m or h", p.PropString("rateUnit", ""))
		}
		n.rate = time.Duration(rate * float64(unit))
		if n.rate < 0 {
			return nil, fmt.Errorf("rate cannot be negative")
		}
	}
	if p.PropBool("delayOnStart", false) {
		n.startDelay = time.Duration(p.PropFloat("startDelayTime", 0) * float64(time.Second))
	}
	return n, nil
}

func strOr(v any, def string) any {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return def
	}
	return v
}

func (n *nrModbusNode) tgt() (modbusTarget, error) {
	if n.target != nil {
		return n.target, nil
	}
	cfg, ok := n.svc.ConfigNode(n.cfgID)
	if !ok {
		if node.StandInOf(n.svc) != nil {
			return nil, nil
		}
		return nil, fmt.Errorf("the Modbus client %s is not running. Open this node and pick a server", n.cfgID)
	}
	t, ok := cfg.(modbusTarget)
	if !ok {
		return nil, fmt.Errorf("config node %s is not a Modbus client", n.cfgID)
	}
	n.target = t
	return t, nil
}

// request builds what to ask the device from the node and the message.
func (n *nrModbusNode) request(m *engine.Msg) (nrRequest, error) {
	switch n.kind {
	case nrGetter, nrRead:
		fc, _ := nrReadFC(n.dataType)
		return nrRequest{fc: fc, unit: n.unit, address: n.address, quantity: n.quantity}, nil
	case nrWrite:
		fc := fcWriteReg
		_, list := nrList(m.Payload())
		switch {
		case n.dataType == "Coil" && (n.quantity > 1 || list):
			fc = fcWriteCoils
		case n.dataType == "Coil":
			fc = fcWriteCoil
		case n.quantity > 1 || list:
			fc = fcWriteRegs
		}
		return nrRequest{fc: fc, unit: n.unit, address: n.address, quantity: n.quantity, value: m.Payload()}, nil
	}
	spec, ok := m.Payload().(map[string]any)
	if !ok {
		what := "{fc, unitid, address, quantity}"
		if n.kind == nrFlexWrite {
			what = "{value, fc, unitid, address, quantity}"
		}
		return nrRequest{}, fmt.Errorf("msg.payload has to be an object like %s, got %T", what, m.Payload())
	}
	var req nrRequest
	var err error
	if req.fc, err = nrInt(spec["fc"], "fc"); err != nil {
		return req, err
	}
	if req.address, err = nrInt(spec["address"], "address"); err != nil {
		return req, err
	}
	if req.quantity, err = nrInt(strOr(spec["quantity"], "1"), "quantity"); err != nil {
		return req, err
	}
	if req.unit, err = nrUnit(spec["unitid"]); err != nil {
		return req, err
	}
	switch n.kind {
	case nrFlexGetter:
		if req.fc < 1 || req.fc > 4 {
			return req, fmt.Errorf("fc %d is not a read: use 1, 2, 3 or 4", req.fc)
		}
	case nrFlexWrite:
		if req.fc != fcWriteCoil && req.fc != fcWriteReg && req.fc != fcWriteCoils && req.fc != fcWriteRegs {
			return req, fmt.Errorf("fc %d is not a write: use 5, 6, 15 or 16", req.fc)
		}
		v, has := spec["value"]
		if !has {
			return req, fmt.Errorf("msg.payload.value is missing")
		}
		req.value = v
	}
	return req, nil
}

func (n *nrModbusNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	n.run(m, out)
	return nil
}

// run does one request and sends the outcome. A failure is not returned: it
// goes out the second output, and to a Catch node when showErrors is on.
func (n *nrModbusNode) run(in *engine.Msg, out node.Emitter) {
	n.mu.Lock()
	defer n.mu.Unlock()
	req, err := n.request(in)
	if err != nil {
		n.fail(out, in, err)
		return
	}
	tgt, err := n.tgt()
	if err != nil {
		n.fail(out, in, err)
		return
	}
	iface := networkInterfaceFor(n.svc, n.iface)
	if err := netbind.Validate(iface); err != nil {
		n.fail(out, in, err)
		return
	}
	res, _, err := nrExec(n.svc, tgt, iface, req)
	if err != nil {
		n.fail(out, in, err)
		return
	}
	n.succeed(out, in, req, res)
}

func (n *nrModbusNode) base(in *engine.Msg) map[string]any {
	var data map[string]any
	if n.keepMsg {
		data = in.Clone().Data
	} else {
		data = map[string]any{}
		if id := in.ID(); id != "" {
			data[engine.PropMsgID] = id
		}
	}
	// A shallow copy of what came in, so the next node can see what asked.
	input := map[string]any{}
	for k, v := range in.Data {
		if k == "input" || k == "responseBuffer" {
			continue
		}
		input[k] = v
	}
	data["input"] = input
	return data
}

func (n *nrModbusNode) succeed(out node.Emitter, in *engine.Msg, req nrRequest, res nrResult) {
	data := n.base(in)
	var values []any
	var raw []byte
	if req.areaName() == "coil" || req.areaName() == "discrete" {
		values = boolsToAny(res.bits)
		raw = bitBytes(res.bits)
	} else {
		values = make([]any, len(res.words))
		for i, w := range res.words {
			values[i] = float64(w)
		}
		raw = registerBytes(res.words)
	}
	if req.isWrite() {
		// A write answers with what it was asked to write, and the payload
		// that came in stays as it was.
		if _, has := data["payload"]; !has {
			data["payload"] = in.Payload()
		}
	} else {
		data["payload"] = values
	}
	data["responseBuffer"] = map[string]any{"data": values, "buffer": raw}
	data["modbusRequest"] = map[string]any{
		"fc": float64(req.fc), "address": float64(req.address), "quantity": float64(req.quantity),
	}
	if n.topic != "" {
		data["topic"] = n.topic
	}
	if n.showStatus {
		out.Status(node.Status{Fill: "green", Shape: "dot", Text: "active"})
	}
	out.Send(0, engine.WrapMsg(data))
}

func (n *nrModbusNode) fail(out node.Emitter, in *engine.Msg, err error) {
	if n.showStatus || n.showErrors {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: truncate(err.Error(), 32)})
	}
	errData := n.base(in)
	errData["error"] = map[string]any{"message": err.Error(), "source": map[string]any{"id": n.cfgID, "type": n.kind.typeName()}}
	if n.topic != "" {
		errData["topic"] = n.topic
	}
	errMsg := engine.WrapMsg(errData)
	if n.emptyOnFail {
		empty := n.base(in)
		empty["payload"] = ""
		empty["error"] = errData["error"]
		out.Send(0, engine.WrapMsg(empty))
	}
	out.Send(1, errMsg)
	if n.showErrors {
		out.Error(err, in)
	}
}

// Start begins polling for a modbus-read node.
func (n *nrModbusNode) Start(ctx context.Context, out node.Emitter) error {
	if n.kind != nrRead || n.rate <= 0 {
		return nil
	}
	clock := node.ClockOf(n.svc)
	var tick func()
	tick = func() {
		n.mu.Lock()
		closed := n.closed
		n.mu.Unlock()
		if closed || ctx.Err() != nil {
			return
		}
		n.run(engine.NewMsg(), out)
		n.mu.Lock()
		if !n.closed {
			n.timer = clock.AfterFunc(n.rate, tick)
		}
		n.mu.Unlock()
	}
	first := n.startDelay
	n.mu.Lock()
	n.timer = clock.AfterFunc(first, tick)
	n.mu.Unlock()
	return nil
}

func (n *nrModbusNode) Close(context.Context, bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	if n.timer != nil {
		n.timer.Stop()
	}
	return nil
}

// ---------------------------------------------------------------------------
// modbus-server
// ---------------------------------------------------------------------------

type nrModbusServer struct {
	host, port string
	delay      time.Duration
	svc        node.Services

	mu       sync.Mutex
	coils    []bool
	discrete []bool
	holding  []uint16
	input    []uint16
	srv      *modbus.ModbusServer
	out      node.Emitter
}

func registerNRModbusServer() {
	node.MustRegister(node.Descriptor{
		Type:         "modbus-server",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      5,
		PaletteLabel: "modbus server",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "A Modbus TCP server over four buffers (coils, holding registers, input registers, " +
				"discrete inputs) of the sizes the node sets. A message with msg.payload = {register, address, value} " +
				"sets values, where register is holding, coils, input or discrete and value is one value or a " +
				"list. A client's write comes out the first output (holding registers) or the second (coils). " +
				"The third and fourth outputs belong to input registers and discrete inputs, which a client " +
				"cannot write, so they send nothing, and the fifth carries errors. Requests on every unit id are answered " +
				"from the same buffers. Serial is not supported.",
			UnsupportedProps: []string{"logEnabled", "showStatusActivities"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "logEnabled", Kind: node.PropBool, Label: "Log"},
			{Name: "hostname", Kind: node.PropString, Label: "Host", Default: "0.0.0.0", Placeholder: "0.0.0.0"},
			{Name: "serverPort", Kind: node.PropString, Label: "Port", Default: "10502"},
			{Name: "responseDelay", Kind: node.PropString, Label: "Response delay", Default: "100"},
			{Name: "delayUnit", Kind: node.PropSelect, Label: "Delay unit", Default: "ms", Options: []node.Option{
				{Value: "ms", Label: "milliseconds"}, {Value: "s", Label: "seconds"}}},
			{Name: "coilsBufferSize", Kind: node.PropString, Label: "Coils", Default: "1024"},
			{Name: "holdingBufferSize", Kind: node.PropString, Label: "Holding registers", Default: "1024"},
			{Name: "inputBufferSize", Kind: node.PropString, Label: "Input registers", Default: "1024"},
			{Name: "discreteBufferSize", Kind: node.PropString, Label: "Discrete inputs", Default: "1024"},
			{Name: "showErrors", Kind: node.PropBool, Label: "Show errors"},
			{Name: "showStatusActivities", Kind: node.PropBool, Label: "Show activities in the status"},
		},
		Help: "A Modbus TCP server a flow can read and write, for a simulator or a bridge.",
	}, newNRModbusServer)
}

func newNRModbusServer(def *node.Definition) (node.Node, error) {
	p := def.Node
	s := &nrModbusServer{svc: def.Services}
	s.host = strings.TrimSpace(p.PropString("hostname", "0.0.0.0"))
	if s.host == "" {
		s.host = "0.0.0.0"
	}
	s.port = strconv.Itoa(p.PropInt("serverPort", 10502))
	if pn := p.PropInt("serverPort", 10502); pn < 1 || pn > 65535 {
		return nil, fmt.Errorf("serverPort %d is out of range, it has to be 1 to 65535", pn)
	}
	delay := p.PropFloat("responseDelay", 0)
	if p.PropString("delayUnit", "ms") == "s" {
		delay *= 1000
	}
	s.delay = time.Duration(delay * float64(time.Millisecond))
	size := func(key string) (int, error) {
		n := p.PropInt(key, 1024)
		if n < 1 || n > 65536 {
			return 0, fmt.Errorf("%s %d is out of range, it has to be 1 to 65536", key, n)
		}
		return n, nil
	}
	var err error
	var nc, nh, ni, nd int
	if nc, err = size("coilsBufferSize"); err != nil {
		return nil, err
	}
	if nh, err = size("holdingBufferSize"); err != nil {
		return nil, err
	}
	if ni, err = size("inputBufferSize"); err != nil {
		return nil, err
	}
	if nd, err = size("discreteBufferSize"); err != nil {
		return nil, err
	}
	s.coils, s.holding, s.input, s.discrete = make([]bool, nc), make([]uint16, nh), make([]uint16, ni), make([]bool, nd)
	return s, nil
}

var errServerNoAddress = modbus.ErrIllegalDataAddress

type nrServerHandler struct{ s *nrModbusServer }

func (h nrServerHandler) pause() {
	if h.s.delay > 0 {
		time.Sleep(h.s.delay)
	}
}

func (h nrServerHandler) HandleCoils(req *modbus.CoilsRequest) ([]bool, error) {
	h.pause()
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	end := int(req.Addr) + int(req.Quantity)
	if end > len(s.coils) {
		return nil, errServerNoAddress
	}
	if req.IsWrite {
		copy(s.coils[req.Addr:end], req.Args)
		s.emitWrite(1, "coils", int(req.Addr), req.UnitId, boolsToAny(req.Args))
		return nil, nil
	}
	return append([]bool(nil), s.coils[req.Addr:end]...), nil
}

func (h nrServerHandler) HandleDiscreteInputs(req *modbus.DiscreteInputsRequest) ([]bool, error) {
	h.pause()
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	end := int(req.Addr) + int(req.Quantity)
	if end > len(s.discrete) {
		return nil, errServerNoAddress
	}
	return append([]bool(nil), s.discrete[req.Addr:end]...), nil
}

func (h nrServerHandler) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	h.pause()
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	end := int(req.Addr) + int(req.Quantity)
	if end > len(s.holding) {
		return nil, errServerNoAddress
	}
	if req.IsWrite {
		copy(s.holding[req.Addr:end], req.Args)
		vals := make([]any, len(req.Args))
		for i, w := range req.Args {
			vals[i] = float64(w)
		}
		s.emitWrite(0, "holding", int(req.Addr), req.UnitId, vals)
		return nil, nil
	}
	return append([]uint16(nil), s.holding[req.Addr:end]...), nil
}

func (h nrServerHandler) HandleInputRegisters(req *modbus.InputRegistersRequest) ([]uint16, error) {
	h.pause()
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	end := int(req.Addr) + int(req.Quantity)
	if end > len(s.input) {
		return nil, errServerNoAddress
	}
	return append([]uint16(nil), s.input[req.Addr:end]...), nil
}

// emitWrite sends a client's write out the node. The caller holds s.mu.
func (s *nrModbusServer) emitWrite(port int, register string, address int, unit uint8, values []any) {
	if s.out == nil {
		return
	}
	var payload any = values
	if len(values) == 1 {
		payload = values[0]
	}
	s.out.Send(port, engine.WrapMsg(map[string]any{
		"payload": map[string]any{"register": register, "address": float64(address),
			"value": payload, "unitid": float64(unit)},
	}))
}

func (s *nrModbusServer) Start(ctx context.Context, out node.Emitter) error {
	s.mu.Lock()
	s.out = out
	s.mu.Unlock()
	if node.StandInOf(s.svc) != nil {
		return nil
	}
	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL:     "tcp://" + net.JoinHostPort(s.host, s.port),
		Timeout: 5 * time.Minute,
		Logger:  log.New(io.Discard, "", 0),
	}, nrServerHandler{s})
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("cannot listen on %s:%s: %w. Check that this host has that address and nothing else holds the port",
			s.host, s.port, err)
	}
	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "listening " + s.port})
	return nil
}

func (s *nrModbusServer) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	spec, ok := m.Payload().(map[string]any)
	if !ok {
		out.Send(4, engine.WrapMsg(map[string]any{"error": map[string]any{
			"message": "msg.payload has to be an object like {register, address, value}"}}))
		return nil
	}
	address, err := nrInt(spec["address"], "address")
	if err != nil {
		return err
	}
	items, isList := nrList(spec["value"])
	if !isList {
		items = []any{spec["value"]}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reg, _ := spec["register"].(string)
	set := func(size int, f func(i int, v any) error) error {
		if address < 0 || address+len(items) > size {
			return fmt.Errorf("address %d with %d values runs past the %d the %s buffer holds", address, len(items), size, reg)
		}
		for i, v := range items {
			if err := f(address+i, v); err != nil {
				return err
			}
		}
		return nil
	}
	switch reg {
	case "holding", "":
		err = set(len(s.holding), func(i int, v any) error { w, e := nrWord(v); s.holding[i] = w; return e })
	case "input":
		err = set(len(s.input), func(i int, v any) error { w, e := nrWord(v); s.input[i] = w; return e })
	case "coils", "coil":
		err = set(len(s.coils), func(i int, v any) error { b, e := nrBool(v); s.coils[i] = b; return e })
	case "discrete":
		err = set(len(s.discrete), func(i int, v any) error { b, e := nrBool(v); s.discrete[i] = b; return e })
	default:
		err = fmt.Errorf("register %q is not holding, coils, input or discrete", reg)
	}
	if err != nil {
		return err
	}
	return nil
}

func (s *nrModbusServer) Close(context.Context, bool) error {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.out = nil
	s.mu.Unlock()
	if srv != nil {
		return srv.Stop()
	}
	return nil
}
