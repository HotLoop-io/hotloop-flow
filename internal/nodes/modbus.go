package nodes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/simonvetter/modbus"
)

// Modbus TCP.
//
// Built on github.com/simonvetter/modbus (MIT). The library does the framing
// and the socket. Everything an operator actually meets is here: what the
// numbers on the wire mean, and what to do when the device says no or says
// nothing.

func init() {
	registerModbusConfig()
	registerModbusRead()
	registerModbusWrite()
}

// Modbus caps a single request. These are the specification's numbers (function
// codes 01, 02, 03, 04, 15 and 16), not this build's, and a device that can't
// take that many says so with an illegal data value.
const (
	modbusMaxReadBits   = 2000
	modbusMaxReadWords  = 125
	modbusMaxWriteBits  = 1968
	modbusMaxWriteWords = 123
)

// modbusTarget is how the read and write nodes reach their server config node.
type modbusTarget interface {
	// do runs fn on a live connection, connecting first if there isn't one,
	// with the unit id set for this request. A failure that says the
	// connection can't be trusted any more closes it, so the next request
	// starts from a fresh socket.
	do(unit *uint8, fn func(c *modbus.ModbusClient) error) error
	// address is host:port, for messages.
	address() string
	// defaultUnit is the unit id the server config carries.
	defaultUnit() uint8
	// timeoutFor is how long one request waits for the device.
	requestTimeout() time.Duration
}

// modbusServer is the config node: one device, one TCP connection, shared by
// every read and write node that points at it. Modbus has no way to interleave
// requests on a connection that isn't strict request and response, so the
// connection is used one request at a time.
type modbusServer struct {
	host    string
	port    int
	unit    uint8
	timeout time.Duration

	mu sync.Mutex
	cl *modbus.ModbusClient
}

func registerModbusConfig() {
	node.MustRegister(node.Descriptor{
		Type:     "hotloop-flow-modbus",
		Category: node.CategoryConfig,
		Color:    colorNetwork,
		Icon:     "network",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Modbus TCP connection. It connects on the first " +
				"request, drops the connection on any failure that makes it untrustworthy, " +
				"and reconnects on the next one.",
		},
		LabelProp: "name",
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "host", Kind: node.PropString, Label: "Host", Required: true,
				Placeholder: "10.20.1.15"},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Default: 502},
			{Name: "unitId", Kind: node.PropNumber, Label: "Unit ID", Default: 1,
				Help: "0 to 255. Most PLCs ignore it or want 1. A gateway in front of serial " +
					"devices uses it to pick the device."},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 3,
				Help: "How long a request waits for the device before the node gives up and " +
					"drops the connection."},
		},
		Help: "One Modbus TCP device. Read and write nodes pointing at it share the " +
			"connection and take turns on it.",
	}, newModbusServer)
}

func newModbusServer(def *node.Definition) (node.Node, error) {
	host := strings.TrimSpace(def.Node.PropString("host", ""))
	if host == "" {
		return nil, fmt.Errorf("host is required: the IP address or name of the Modbus device")
	}
	if strings.ContainsAny(host, "/ ") {
		return nil, fmt.Errorf("host %q should be just an address like 10.20.1.15, with no scheme, path or port", host)
	}
	port := def.Node.PropInt("port", 502)
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range, it has to be 1 to 65535", port)
	}
	unit := def.Node.PropInt("unitId", 1)
	if unit < 0 || unit > 255 {
		return nil, fmt.Errorf("unit ID %d is out of range, it has to be 0 to 255", unit)
	}
	secs := def.Node.PropFloat("timeout", 3)
	if secs <= 0 {
		return nil, fmt.Errorf("timeout has to be more than zero seconds")
	}
	return &modbusServer{
		host:    host,
		port:    port,
		unit:    uint8(unit),
		timeout: time.Duration(secs * float64(time.Second)),
	}, nil
}

func (s *modbusServer) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (s *modbusServer) address() string {
	return net.JoinHostPort(s.host, strconv.Itoa(s.port))
}

func (s *modbusServer) defaultUnit() uint8            { return s.unit }
func (s *modbusServer) requestTimeout() time.Duration { return s.timeout }

// Close drops the connection on redeploy or shutdown.
func (s *modbusServer) Close(context.Context, bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cl != nil {
		err := s.cl.Close()
		s.cl = nil
		return err
	}
	return nil
}

func (s *modbusServer) do(unit *uint8, fn func(c *modbus.ModbusClient) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cl == nil {
		cl, err := modbus.NewClient(&modbus.ClientConfiguration{
			URL:     "tcp://" + s.address(),
			Timeout: s.timeout,
			// The library logs to stdout when it isn't given a logger. What
			// went wrong comes back as the error, which is the one place an
			// operator will look.
			Logger: log.New(io.Discard, "", 0),
		})
		if err != nil {
			return err
		}
		if err := cl.Open(); err != nil {
			return err
		}
		s.cl = cl
	}

	u := s.unit
	if unit != nil {
		u = *unit
	}
	if err := s.cl.SetUnitId(u); err != nil {
		return err
	}
	err := fn(s.cl)
	if err != nil && !modbusKeepsConnection(err) {
		_ = s.cl.Close()
		s.cl = nil
	}
	return err
}

// modbusKeepsConnection reports whether a failed request leaves the connection
// good to use. A device that answered with an exception code is alive and
// talking, and the request was wrong or refused. Anything else (no answer, a
// reset, a frame that doesn't parse, an answer to a different question) leaves
// the stream in a state nobody can vouch for, and a late reply on a reused
// connection answers the next request with the last one's data.
func modbusKeepsConnection(err error) bool {
	switch {
	case errors.Is(err, modbus.ErrIllegalFunction),
		errors.Is(err, modbus.ErrIllegalDataAddress),
		errors.Is(err, modbus.ErrIllegalDataValue),
		errors.Is(err, modbus.ErrServerDeviceFailure),
		errors.Is(err, modbus.ErrAcknowledge),
		errors.Is(err, modbus.ErrServerDeviceBusy),
		errors.Is(err, modbus.ErrMemoryParityError),
		errors.Is(err, modbus.ErrGWPathUnavailable),
		errors.Is(err, modbus.ErrGWTargetFailedToRespond):
		return true
	}
	return false
}

// explainModbus turns what the library and the socket say into what the
// operator does next.
func explainModbus(addr string, unit uint8, timeout time.Duration, what string, err error) error {
	if err == nil {
		return nil
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, modbus.ErrRequestTimedOut):
		return fmt.Errorf("%s: no answer from %s within %s. The device is silent. Check that it is "+
			"powered and reachable, that unit ID %d is the one it answers to, and that nothing on the "+
			"way drops the traffic. The connection was closed, and the next message tries again from "+
			"a fresh one", what, addr, timeout, unit)
	case errors.Is(err, modbus.ErrIllegalDataAddress):
		return fmt.Errorf("%s: %s rejected it with illegal data address. Addresses on the wire start "+
			"at 0, so register 40001 in a manual is address 0. Check the address and the quantity "+
			"against the device's register map", what, addr)
	case errors.Is(err, modbus.ErrIllegalFunction):
		return fmt.Errorf("%s: %s rejected it with illegal function. This device doesn't support that "+
			"operation on that kind of register. Some serve holding registers only, some are "+
			"read-only. Check the area against its register map", what, addr)
	case errors.Is(err, modbus.ErrIllegalDataValue):
		return fmt.Errorf("%s: %s rejected it with illegal data value. Either the value is outside "+
			"what that register accepts, or the quantity is more than the device takes in one "+
			"request. Try fewer values at a time", what, addr)
	case errors.Is(err, modbus.ErrServerDeviceFailure):
		return fmt.Errorf("%s: %s reported a device failure. It tried and couldn't. Look at the "+
			"device itself, its display or its diagnostics", what, addr)
	case errors.Is(err, modbus.ErrServerDeviceBusy), errors.Is(err, modbus.ErrAcknowledge):
		return fmt.Errorf("%s: %s is busy with a previous request. Poll it less often or retry", what, addr)
	case errors.Is(err, modbus.ErrGWPathUnavailable), errors.Is(err, modbus.ErrGWTargetFailedToRespond):
		return fmt.Errorf("%s: the gateway at %s can't reach unit %d behind it. Check the serial side: "+
			"the unit ID, the cable and the baud rate", what, addr, unit)
	case errors.Is(err, modbus.ErrBadUnitId):
		return fmt.Errorf("%s: %s answered as a different unit than the %d asked for. Check the unit ID",
			what, addr, unit)
	case errors.Is(err, modbus.ErrProtocolError), errors.Is(err, modbus.ErrShortFrame),
		errors.Is(err, modbus.ErrUnknownProtocolId), errors.Is(err, modbus.ErrBadTransactionId):
		return fmt.Errorf("%s: %s sent something that isn't valid Modbus TCP (%v). Check that port %s really "+
			"is a Modbus TCP server and not another service. The connection was closed", what, addr, err,
			portOf(addr))
	case errors.As(err, &dnsErr):
		return fmt.Errorf("%s: couldn't find a host called %q. Check the spelling, that this host can reach "+
			"its DNS, or use the device's IP address", what, dnsErr.Name)
	case errors.As(err, &opErr):
		if opErr.Timeout() {
			return fmt.Errorf("%s: couldn't reach %s, the connection attempt timed out. Check the IP "+
				"address, that this host has a route to it, and that a firewall isn't dropping port %s",
				what, addr, portOf(addr))
		}
		if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "refused") {
			return fmt.Errorf("%s: %s refused the connection. The host is up and nothing is listening "+
				"on port %s. Check the port, and that Modbus TCP is switched on in the device", what,
				addr, portOf(addr))
		}
		return fmt.Errorf("%s: lost the connection to %s (%v). Check the cable, the device and "+
			"anything between. The next message reconnects", what, addr, err)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%s: %s closed the connection on us. Many PLCs allow only a few Modbus "+
			"clients at once, so check nothing else has the slots. The next message reconnects", what, addr)
	}
	return fmt.Errorf("%s: %s: %w", what, addr, err)
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return "502"
}

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

// modbusType is how a run of registers is read as a number.
type modbusType struct {
	name  string
	words int
}

var modbusTypes = map[string]modbusType{
	"uint16":  {"uint16", 1},
	"int16":   {"int16", 1},
	"uint32":  {"uint32", 2},
	"int32":   {"int32", 2},
	"float32": {"float32", 2},
	"uint64":  {"uint64", 4},
	"int64":   {"int64", 4},
	"float64": {"float64", 4},
}

func modbusTypeOptions() []node.Option {
	return []node.Option{
		{Value: "uint16", Label: "Unsigned 16 bit, one register"},
		{Value: "int16", Label: "Signed 16 bit, one register"},
		{Value: "uint32", Label: "Unsigned 32 bit, two registers"},
		{Value: "int32", Label: "Signed 32 bit, two registers"},
		{Value: "float32", Label: "Float 32 bit, two registers"},
		{Value: "uint64", Label: "Unsigned 64 bit, four registers"},
		{Value: "int64", Label: "Signed 64 bit, four registers"},
		{Value: "float64", Label: "Float 64 bit, four registers"},
	}
}

func modbusOrderProps() []node.Prop {
	return []node.Prop{
		{Name: "byteOrder", Kind: node.PropSelect, Label: "Byte order in a register", Default: "big",
			Options: []node.Option{
				{Value: "big", Label: "Big endian (AB), what the specification says"},
				{Value: "little", Label: "Little endian (BA)"},
			}},
		{Name: "wordOrder", Kind: node.PropSelect, Label: "Word order in a multi-register value", Default: "high",
			Options: []node.Option{
				{Value: "high", Label: "High word first (ABCD)"},
				{Value: "low", Label: "Low word first (CDAB)"},
			},
			Help: "Devices disagree about this one and the manual often doesn't say. If a 32 bit " +
				"value reads as nonsense, flip it."},
	}
}

// decodeModbusWords reads quantity values of type t out of raw registers.
func decodeModbusWords(words []uint16, t modbusType, little, lowFirst bool) ([]any, error) {
	if len(words)%t.words != 0 {
		return nil, fmt.Errorf("%d registers don't divide into %s values of %d registers each",
			len(words), t.name, t.words)
	}
	out := make([]any, 0, len(words)/t.words)
	for i := 0; i < len(words); i += t.words {
		chunk := make([]uint16, t.words)
		copy(chunk, words[i:i+t.words])
		for j := range chunk {
			if little {
				chunk[j] = chunk[j]<<8 | chunk[j]>>8
			}
		}
		if lowFirst {
			for a, b := 0, len(chunk)-1; a < b; a, b = a+1, b-1 {
				chunk[a], chunk[b] = chunk[b], chunk[a]
			}
		}
		var u uint64
		for _, w := range chunk {
			u = u<<16 | uint64(w)
		}
		switch t.name {
		case "uint16":
			out = append(out, float64(uint16(u)))
		case "int16":
			out = append(out, float64(int16(u)))
		case "uint32":
			out = append(out, float64(uint32(u)))
		case "int32":
			out = append(out, float64(int32(u)))
		case "float32":
			out = append(out, float64(math.Float32frombits(uint32(u))))
		case "float64":
			out = append(out, math.Float64frombits(u))
		case "uint64":
			out = append(out, bigInt(strconv.FormatUint(u, 10), float64(u), u <= 1<<53))
		case "int64":
			s := int64(u)
			out = append(out, bigInt(strconv.FormatInt(s, 10), float64(s), s >= -(1<<53) && s <= 1<<53))
		}
	}
	return out, nil
}

// bigInt is a 64 bit integer as a flow can carry it. A flow's numbers are
// doubles, which hold integers exactly only up to 2^53. Past that the exact
// value is delivered as a decimal string, because a rounded counter that
// looks right is worse than a string that is obviously a different shape.
func bigInt(decimal string, f float64, exact bool) any {
	if exact {
		return f
	}
	return decimal
}

// encodeModbusValue turns one number from a flow into the registers of type t.
func encodeModbusValue(v any, t modbusType, little, lowFirst bool) ([]uint16, error) {
	var u uint64
	switch t.name {
	case "uint64", "int64":
		if s, ok := v.(string); ok && len(strings.TrimSpace(s)) > 0 {
			// The exact form a read hands out past 2^53 comes back in the same way.
			if t.name == "uint64" {
				n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%q doesn't fit uint64 (0 to 18446744073709551615)", s)
				}
				u = n
			} else {
				n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%q doesn't fit int64", s)
				}
				u = uint64(n)
			}
			break
		}
		fallthrough
	default:
		f, ok := asFloat(v)
		if !ok {
			return nil, fmt.Errorf("%v (%s) isn't a number, and %s registers hold a number", v, describeType(v), t.name)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			if t.name != "float32" && t.name != "float64" {
				return nil, fmt.Errorf("%v can't be written to a %s register", f, t.name)
			}
		}
		var err error
		if u, err = modbusBits(f, t); err != nil {
			return nil, err
		}
	}

	chunk := make([]uint16, t.words)
	for i := t.words - 1; i >= 0; i-- {
		chunk[i] = uint16(u)
		u >>= 16
	}
	if lowFirst {
		for a, b := 0, len(chunk)-1; a < b; a, b = a+1, b-1 {
			chunk[a], chunk[b] = chunk[b], chunk[a]
		}
	}
	if little {
		for i := range chunk {
			chunk[i] = chunk[i]<<8 | chunk[i]>>8
		}
	}
	return chunk, nil
}

func modbusBits(f float64, t modbusType) (uint64, error) {
	integral := func(lo, hi float64) error {
		if f != math.Trunc(f) {
			return fmt.Errorf("%v isn't a whole number, and a %s register holds only whole numbers. "+
				"Round it first, or use a float type if the device has one", f, t.name)
		}
		if f < lo || f > hi {
			return fmt.Errorf("%v is outside what a %s register holds (%v to %v)", f, t.name, lo, hi)
		}
		return nil
	}
	switch t.name {
	case "uint16":
		if err := integral(0, math.MaxUint16); err != nil {
			return 0, err
		}
		return uint64(f), nil
	case "int16":
		if err := integral(math.MinInt16, math.MaxInt16); err != nil {
			return 0, err
		}
		return uint64(uint16(int16(f))), nil
	case "uint32":
		if err := integral(0, math.MaxUint32); err != nil {
			return 0, err
		}
		return uint64(f), nil
	case "int32":
		if err := integral(math.MinInt32, math.MaxInt32); err != nil {
			return 0, err
		}
		return uint64(uint32(int32(f))), nil
	case "uint64":
		if err := integral(0, 1<<53); err != nil {
			return 0, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		return uint64(f), nil
	case "int64":
		if err := integral(-(1 << 53), 1<<53); err != nil {
			return 0, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		return uint64(int64(f)), nil
	case "float32":
		g := float32(f)
		if math.IsInf(float64(g), 0) && !math.IsInf(f, 0) {
			return 0, fmt.Errorf("%v is too big for a float32 register", f)
		}
		return uint64(math.Float32bits(g)), nil
	case "float64":
		return math.Float64bits(f), nil
	}
	return 0, fmt.Errorf("unknown data type %q", t.name)
}

// modbusBool reads a coil value out of whatever a flow carries.
func modbusBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "on":
			return true, nil
		case "false", "0", "off":
			return false, nil
		}
	default:
		if f, ok := asFloat(v); ok && (f == 0 || f == 1) {
			return f == 1, nil
		}
	}
	return false, fmt.Errorf("%v (%s) can't be a coil value. A coil is on or off: use true or false, or 1 or 0",
		v, describeType(v))
}

// ---------------------------------------------------------------------------
// Areas
// ---------------------------------------------------------------------------

func modbusReadAreas() []node.Option {
	return []node.Option{
		{Value: "holding", Label: "Holding registers (function 3)"},
		{Value: "input", Label: "Input registers (function 4)"},
		{Value: "coil", Label: "Coils (function 1)"},
		{Value: "discrete", Label: "Discrete inputs (function 2)"},
	}
}

func modbusAreaName(a string) string {
	switch a {
	case "holding":
		return "holding register"
	case "input":
		return "input register"
	case "coil":
		return "coil"
	case "discrete":
		return "discrete input"
	}
	return a
}

// modbusOperand is an address or a quantity, typed so a flow can compute it.
func modbusIntFrom(tv TypedValue, ec EvalContext, what string, lo, hi int) (int, error) {
	v, ok, err := tv.Eval(ec)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	if !ok || v == nil {
		return 0, fmt.Errorf("%s resolved to nothing. Set it on the node or put it on the message", what)
	}
	f, isNum := asFloat(v)
	if !isNum || f != math.Trunc(f) {
		return 0, fmt.Errorf("%s is %v, and has to be a whole number", what, v)
	}
	if f < float64(lo) || f > float64(hi) {
		return 0, fmt.Errorf("%s is %v, and has to be %d to %d", what, f, lo, hi)
	}
	return int(f), nil
}

func modbusUnitFrom(raw string) (*uint8, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 255 {
		return nil, fmt.Errorf("unit ID %q has to be a number from 0 to 255, or empty to use the server's", raw)
	}
	u := uint8(n)
	return &u, nil
}

func modbusTarget0(svc node.Services, cfgID string, cached *modbusTarget) (modbusTarget, error) {
	if *cached != nil {
		return *cached, nil
	}
	cfg, ok := svc.ConfigNode(cfgID)
	if !ok {
		return nil, fmt.Errorf("the Modbus server config %s is not running. Open this node and pick a server", cfgID)
	}
	t, ok := cfg.(modbusTarget)
	if !ok {
		return nil, fmt.Errorf("config node %s is not a Modbus server", cfgID)
	}
	*cached = t
	return t, nil
}

// modbusInfo is what a node leaves on msg.modbus so the next node knows where
// the value came from.
func modbusInfo(addr string, unit uint8, area string, address, quantity int, dataType string) map[string]any {
	m := map[string]any{
		"server":   addr,
		"unitId":   float64(unit),
		"area":     area,
		"address":  float64(address),
		"quantity": float64(quantity),
	}
	if dataType != "" {
		m["dataType"] = dataType
	}
	return m
}

// ---------------------------------------------------------------------------
// modbus read
// ---------------------------------------------------------------------------

type modbusReadNode struct {
	cfgID    string
	area     string
	address  TypedValue
	quantity TypedValue
	dtype    modbusType
	little   bool
	lowFirst bool
	unit     *uint8
	svc      node.Services
	target   modbusTarget
}

func registerModbusRead() {
	node.MustRegister(node.Descriptor{
		Type:         "modbus read",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "modbus read",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Modbus TCP read. Not the same configuration as the community " +
				"node-red-contrib-modbus, which does not run here.",
		},
		Props: modbusProps{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "hotloop-flow-modbus", Required: true},
			{Name: "area", Kind: node.PropSelect, Label: "Read", Default: "holding", Options: modbusReadAreas()},
			{Name: "address", Kind: node.PropTypedInput, Label: "Address", Default: "0", Required: true,
				TypeProp: "addressType", TypedInputTypes: modbusNumTypes(),
				Help: "Zero based, as it goes on the wire. Register 40001 in a manual is address 0."},
			{Name: "quantity", Kind: node.PropTypedInput, Label: "How many values", Default: "1", Required: true,
				TypeProp: "quantityType", TypedInputTypes: modbusNumTypes(),
				Help: "Values, not registers. Two float32 values read four registers."},
			{Name: "dataType", Kind: node.PropSelect, Label: "Data type", Default: "uint16",
				Options: modbusTypeOptions(), Help: "Registers only. Coils and discrete inputs are true or false."},
		}.withOrder(
			node.Prop{Name: "unitId", Kind: node.PropString, Label: "Unit ID",
				Help: "Empty uses the server's."},
		),
		Help: "Reads coils, discrete inputs, holding registers or input registers from a Modbus TCP " +
			"device when a message arrives. The value is msg.payload, a single value for a quantity " +
			"of 1 and a list otherwise. msg.modbus says where it came from.\n\n" +
			"A 64 bit integer past 2^53 comes out as a string of its exact digits, because a flow's " +
			"numbers can't hold it.",
	}, newModbusRead)
}

type modbusProps []node.Prop

func (p modbusProps) withOrder(extra ...node.Prop) []node.Prop {
	out := append([]node.Prop(nil), p...)
	out = append(out, modbusOrderProps()...)
	return append(out, extra...)
}

func modbusNumTypes() []string {
	return []string{node.TypeNum, node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeEnv, node.TypeJSONata}
}

func newModbusRead(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &modbusReadNode{
		cfgID:    p.PropString("server", ""),
		area:     p.PropString("area", "holding"),
		address:  ReadTypedValue(p.Raw, "address", "addressType", node.TypeNum),
		quantity: ReadTypedValue(p.Raw, "quantity", "quantityType", node.TypeNum),
		little:   p.PropString("byteOrder", "big") == "little",
		lowFirst: p.PropString("wordOrder", "high") == "low",
		svc:      def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the Modbus device to read from")
	}
	// A flow file that leaves a field out gets the editor's default only if
	// the editor wrote it. Built from a hand-written or imported entry, the
	// node supplies its own.
	if n.address.Value == "" {
		n.address.Value = "0"
	}
	if n.quantity.Value == "" {
		n.quantity.Value = "1"
	}
	switch n.area {
	case "holding", "input", "coil", "discrete":
	default:
		return nil, fmt.Errorf("area %q is not one of holding, input, coil or discrete", n.area)
	}
	dt := p.PropString("dataType", "uint16")
	t, ok := modbusTypes[dt]
	if !ok {
		return nil, fmt.Errorf("data type %q is not one of uint16, int16, uint32, int32, float32, uint64, int64 or float64", dt)
	}
	n.dtype = t
	for _, tv := range []TypedValue{n.address, n.quantity} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	u, err := modbusUnitFrom(p.PropString("unitId", ""))
	if err != nil {
		return nil, err
	}
	n.unit = u
	return n, nil
}

func (n *modbusReadNode) isBits() bool { return n.area == "coil" || n.area == "discrete" }

func (n *modbusReadNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	address, err := modbusIntFrom(n.address, ec, "address", 0, 65535)
	if err != nil {
		return n.fail(out, err)
	}
	quantity, err := modbusIntFrom(n.quantity, ec, "quantity", 1, 65535)
	if err != nil {
		return n.fail(out, err)
	}

	words := quantity
	if !n.isBits() {
		words = quantity * n.dtype.words
	}
	limit := modbusMaxReadWords
	unitName := "registers"
	if n.isBits() {
		limit, unitName = modbusMaxReadBits, "bits"
	}
	if words > limit {
		return n.fail(out, fmt.Errorf("that is %d %s in one request, and Modbus allows %d at most. "+
			"Split it into more than one read", words, unitName, limit))
	}
	if address+words > 65536 {
		return n.fail(out, fmt.Errorf("address %d plus %d %s runs past the end of the address space (65535)",
			address, words, unitName))
	}

	tgt, err := modbusTarget0(n.svc, n.cfgID, &n.target)
	if err != nil && node.StandInOf(n.svc) == nil {
		return n.fail(out, err)
	}

	what := fmt.Sprintf("reading %d %s at %d", quantity, plural(quantity, modbusAreaName(n.area)), address)
	var values []any
	var addr string
	unit := uint8(1)

	if si := node.StandInOf(n.svc); si != nil {
		sent := map[string]any{"op": "read", "area": n.area, "address": float64(address),
			"quantity": float64(quantity)}
		if !n.isBits() {
			sent["dataType"] = n.dtype.name
		}
		reply, err := si.Call("modbus", sent)
		if err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
		if values, err = n.scriptedValues(reply, quantity); err != nil {
			return n.fail(out, err)
		}
		addr = "stand-in"
	} else {
		addr, unit = tgt.address(), tgt.defaultUnit()
		if n.unit != nil {
			unit = *n.unit
		}
		err = tgt.do(n.unit, func(c *modbus.ModbusClient) error {
			switch n.area {
			case "coil":
				bs, err := c.ReadCoils(uint16(address), uint16(quantity))
				values = boolsToAny(bs)
				return err
			case "discrete":
				bs, err := c.ReadDiscreteInputs(uint16(address), uint16(quantity))
				values = boolsToAny(bs)
				return err
			}
			rt := modbus.HOLDING_REGISTER
			if n.area == "input" {
				rt = modbus.INPUT_REGISTER
			}
			ws, err := c.ReadRegisters(uint16(address), uint16(words), rt)
			if err != nil {
				return err
			}
			values, err = decodeModbusWords(ws, n.dtype, n.little, n.lowFirst)
			return err
		})
		if err != nil {
			return n.fail(out, explainModbus(addr, unit, tgt.requestTimeout(), what, err))
		}
	}

	dt := ""
	if !n.isBits() {
		dt = n.dtype.name
	}
	if quantity == 1 {
		m.SetPayload(values[0])
	} else {
		m.SetPayload(values)
	}
	m.Data["modbus"] = modbusInfo(addr, unit, n.area, address, quantity, dt)
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(statusValue(values), 32)})
	out.Send(0, m)
	return nil
}

func (n *modbusReadNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

// scriptedValues reads what a flow test said the device answered. A list under
// "values", or one value under "value". Silence from the script is zeros, a
// device that answered and held nothing interesting.
func (n *modbusReadNode) scriptedValues(reply map[string]any, quantity int) ([]any, error) {
	if l, err := replyList(reply, "values"); err != nil {
		return nil, err
	} else if l != nil {
		if len(l) != quantity {
			return nil, fmt.Errorf("the scripted reply has %d values, and the read asked for %d", len(l), quantity)
		}
		return l, nil
	}
	if v, ok := reply["value"]; ok && v != nil {
		if quantity != 1 {
			return nil, fmt.Errorf("the scripted reply has one value, and the read asked for %d. Use values", quantity)
		}
		return []any{v}, nil
	}
	out := make([]any, quantity)
	for i := range out {
		if n.isBits() {
			out[i] = false
		} else {
			out[i] = float64(0)
		}
	}
	return out, nil
}

func boolsToAny(bs []bool) []any {
	out := make([]any, len(bs))
	for i, b := range bs {
		out[i] = b
	}
	return out
}

// statusValue is a value short enough for a canvas badge.
func statusValue(values []any) string {
	if len(values) == 1 {
		return fmt.Sprint(values[0])
	}
	return fmt.Sprintf("%d values", len(values))
}

// ---------------------------------------------------------------------------
// modbus write
// ---------------------------------------------------------------------------

type modbusWriteNode struct {
	cfgID    string
	area     string
	address  TypedValue
	value    TypedValue
	dtype    modbusType
	little   bool
	lowFirst bool
	unit     *uint8
	svc      node.Services
	target   modbusTarget
	gate     *hotloopGate
}

func registerModbusWrite() {
	node.MustRegister(node.Descriptor{
		Type:         "modbus write",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "modbus write",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Modbus TCP write. Not the same configuration as the community " +
				"node-red-contrib-modbus, which does not run here.",
		},
		Props: modbusProps{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "hotloop-flow-modbus", Required: true},
			{Name: "area", Kind: node.PropSelect, Label: "Write", Default: "holding",
				Options: []node.Option{
					{Value: "holding", Label: "Holding registers (function 6 or 16)"},
					{Value: "coil", Label: "Coils (function 5 or 15)"},
				}},
			{Name: "address", Kind: node.PropTypedInput, Label: "Address", Default: "0", Required: true,
				TypeProp: "addressType", TypedInputTypes: modbusNumTypes(),
				Help: "Zero based, as it goes on the wire. Register 40001 in a manual is address 0."},
			{Name: "value", Kind: node.PropTypedInput, Label: "Value", Default: "payload", Required: true,
				TypeProp: "valueType",
				TypedInputTypes: []string{node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeNum,
					node.TypeBool, node.TypeJSON, node.TypeEnv, node.TypeJSONata},
				Help: "A list writes consecutive registers, or coils, starting at the address."},
			{Name: "dataType", Kind: node.PropSelect, Label: "Data type", Default: "uint16",
				Options: modbusTypeOptions(), Help: "Registers only. A coil is true or false."},
		}.withOrder(
			node.Prop{Name: "unitId", Kind: node.PropString, Label: "Unit ID",
				Help: "Empty uses the server's."},
			hotloopGateProps()[0], hotloopGateProps()[1],
		),
		Help: "Writes to a Modbus TCP device when a message arrives, then passes the message on. " +
			"This changes the device. There is no confirmation step and no read back: the message " +
			"that arrives at the output means the device acknowledged the write.\n\n" +
			"One register is written with function 6 and more than one with function 16. One coil " +
			"is function 5 and more than one is function 15." + hotloopGateHelp,
	}, newModbusWrite)
}

func newModbusWrite(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &modbusWriteNode{
		cfgID:    p.PropString("server", ""),
		area:     p.PropString("area", "holding"),
		address:  ReadTypedValue(p.Raw, "address", "addressType", node.TypeNum),
		value:    ReadTypedValue(p.Raw, "value", "valueType", node.TypeMsg),
		little:   p.PropString("byteOrder", "big") == "little",
		lowFirst: p.PropString("wordOrder", "high") == "low",
		svc:      def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the Modbus device to write to")
	}
	if n.address.Value == "" {
		n.address.Value = "0"
	}
	if n.value.Value == "" {
		n.value = TypedValue{Type: node.TypeMsg, Value: "payload"}
	}
	if n.area != "holding" && n.area != "coil" {
		return nil, fmt.Errorf("area %q is not holding or coil. Input registers and discrete inputs are read-only in Modbus", n.area)
	}
	dt := p.PropString("dataType", "uint16")
	t, ok := modbusTypes[dt]
	if !ok {
		return nil, fmt.Errorf("data type %q is not one of uint16, int16, uint32, int32, float32, uint64, int64 or float64", dt)
	}
	n.dtype = t
	for _, tv := range []TypedValue{n.address, n.value} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	u, err := modbusUnitFrom(p.PropString("unitId", ""))
	if err != nil {
		return nil, err
	}
	n.unit = u
	g, gerr := newHotloopGate(def)
	if gerr != nil {
		return nil, gerr
	}
	n.gate = g
	return n, nil
}

func (n *modbusWriteNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *modbusWriteNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	address, err := modbusIntFrom(n.address, ec, "address", 0, 65535)
	if err != nil {
		return n.fail(out, err)
	}
	raw, ok, err := n.value.Eval(ec)
	if err != nil {
		return n.fail(out, fmt.Errorf("value: %w", err))
	}
	if !ok || raw == nil {
		return n.fail(out, fmt.Errorf("value resolved to nothing, so there is nothing to write. "+
			"Check the Value field, which reads %s %q", n.value.Type, n.value.Value))
	}
	if n.gate != nil {
		return n.gate.write(ctx, m, raw, out, n.fail)
	}
	items, isList := raw.([]any)
	if !isList {
		items = []any{raw}
	}
	if len(items) == 0 {
		return n.fail(out, fmt.Errorf("the value is an empty list, so there is nothing to write"))
	}

	var bits []bool
	var words []uint16
	if n.area == "coil" {
		for i, it := range items {
			b, err := modbusBool(it)
			if err != nil {
				return n.fail(out, fmt.Errorf("value %d: %w", i, err))
			}
			bits = append(bits, b)
		}
		if len(bits) > modbusMaxWriteBits {
			return n.fail(out, fmt.Errorf("that is %d coils in one request, and Modbus allows %d at most. "+
				"Split it into more than one write", len(bits), modbusMaxWriteBits))
		}
		if address+len(bits) > 65536 {
			return n.fail(out, fmt.Errorf("address %d plus %d coils runs past the end of the address space (65535)", address, len(bits)))
		}
	} else {
		for i, it := range items {
			w, err := encodeModbusValue(it, n.dtype, n.little, n.lowFirst)
			if err != nil {
				if isList {
					return n.fail(out, fmt.Errorf("value %d: %w", i, err))
				}
				return n.fail(out, err)
			}
			words = append(words, w...)
		}
		if len(words) > modbusMaxWriteWords {
			return n.fail(out, fmt.Errorf("that is %d registers in one request, and Modbus allows %d at most. "+
				"Split it into more than one write", len(words), modbusMaxWriteWords))
		}
		if address+len(words) > 65536 {
			return n.fail(out, fmt.Errorf("address %d plus %d registers runs past the end of the address space (65535)", address, len(words)))
		}
	}

	count := len(items)
	what := fmt.Sprintf("writing %d %s at %d", count, plural(count, modbusAreaName(n.area)), address)
	var addr string
	unit := uint8(1)

	if si := node.StandInOf(n.svc); si != nil {
		sent := map[string]any{"op": "write", "area": n.area, "address": float64(address)}
		if n.area == "coil" {
			sent["values"] = boolsToAny(bits)
		} else {
			sent["dataType"] = n.dtype.name
			ws := make([]any, len(words))
			for i, w := range words {
				ws[i] = float64(w)
			}
			sent["registers"] = ws
			sent["values"] = items
		}
		if _, err := si.Call("modbus", sent); err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
		addr = "stand-in"
	} else {
		tgt, err := modbusTarget0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		addr, unit = tgt.address(), tgt.defaultUnit()
		if n.unit != nil {
			unit = *n.unit
		}
		err = tgt.do(n.unit, func(c *modbus.ModbusClient) error {
			switch {
			case n.area == "coil" && len(bits) == 1:
				return c.WriteCoil(uint16(address), bits[0])
			case n.area == "coil":
				return c.WriteCoils(uint16(address), bits)
			case len(words) == 1:
				return c.WriteRegister(uint16(address), words[0])
			}
			return c.WriteRegisters(uint16(address), words)
		})
		if err != nil {
			return n.fail(out, explainModbus(addr, unit, tgt.requestTimeout(), what, err))
		}
	}

	dt := ""
	if n.area != "coil" {
		dt = n.dtype.name
	}
	m.Data["modbus"] = modbusInfo(addr, unit, n.area, address, count, dt)
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate("wrote "+statusValue(items), 32)})
	out.Send(0, m)
	return nil
}
