package nodes

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/robinson/gos7"
)

// Siemens S7 over ISO on TCP, port 102.
//
// Built on github.com/robinson/gos7 (BSD-3-Clause), which is a Go implementation
// of the protocol and has nothing of Snap7's C library in it. The library does
// the framing and the socket. What an operator meets is here: the Siemens
// address a flow writes, what the bytes in a data block mean, and what to do
// when the CPU says no or says nothing.

func init() {
	registerS7Config()
	registerS7Read()
	registerS7Write()
}

// The area codes and the bit word length gos7's multi-item calls take. They are
// the values of the S7 protocol itself.
const (
	s7AreaInputs  = 0x81
	s7AreaOutputs = 0x82
	s7AreaMerkers = 0x83
	s7AreaDB      = 0x84
	s7WordLenBit  = 0x01

	// s7MaxBytes is the most one read or write moves, however many telegrams
	// the library splits it into.
	s7MaxBytes = 4096
)

type s7Target interface {
	do(fn func(c gos7.Client) error) error
	address() string
	rackSlot() (int, int)
	requestTimeout() time.Duration
}

// s7Server is the config node: one CPU, one TCP connection, shared by every read
// and write node that points at it and used one request at a time.
type s7Server struct {
	host    string
	port    int
	rack    int
	slot    int
	timeout time.Duration

	mu sync.Mutex
	h  *gos7.TCPClientHandler
	c  gos7.Client
}

func registerS7Config() {
	node.MustRegister(node.Descriptor{
		Type:     "hotloop-flow-s7",
		Category: node.CategoryConfig,
		Color:    colorNetwork,
		Icon:     "network",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Siemens S7 connection, over ISO on TCP. It connects on the first " +
				"request, drops the connection on any failure that makes it untrustworthy, and reconnects " +
				"on the next one.",
		},
		LabelProp: "name",
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "host", Kind: node.PropString, Label: "Host", Required: true, Placeholder: "10.20.1.30"},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Default: 102},
			{Name: "rack", Kind: node.PropNumber, Label: "Rack", Default: 0,
				Help: "The rack the CPU is in, as the hardware configuration shows it."},
			{Name: "slot", Kind: node.PropNumber, Label: "Slot", Default: 1,
				Help: "The slot the CPU is in, as the hardware configuration shows it."},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 3,
				Help: "How long a request waits for the CPU before the node gives up and drops the connection."},
		},
		Help: "One Siemens S7 CPU. Read and write nodes pointing at it share the connection and take turns on it.\n\n" +
			"An S7-1200 or S7-1500 answers only when it has been set to permit it: in TIA Portal, " +
			"\"Permit access with PUT/GET communication from remote partner\" in the CPU's protection " +
			"settings, with the access level full, and \"Optimized block access\" turned off in the " +
			"properties of every data block you read or write. Only global data blocks can be reached.",
	}, newS7Server)
}

func newS7Server(def *node.Definition) (node.Node, error) {
	host := strings.TrimSpace(def.Node.PropString("host", ""))
	if host == "" {
		return nil, fmt.Errorf("host is required: the IP address or name of the S7 CPU")
	}
	if strings.ContainsAny(host, "/ :") {
		return nil, fmt.Errorf("host %q should be just an address like 10.20.1.30, with no scheme, path or port", host)
	}
	port := def.Node.PropInt("port", 102)
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range, it has to be 1 to 65535", port)
	}
	rack := def.Node.PropInt("rack", 0)
	if rack < 0 || rack > 7 {
		return nil, fmt.Errorf("rack %d is out of range, it has to be 0 to 7", rack)
	}
	slot := def.Node.PropInt("slot", 1)
	if slot < 0 || slot > 31 {
		return nil, fmt.Errorf("slot %d is out of range, it has to be 0 to 31", slot)
	}
	secs := def.Node.PropFloat("timeout", 3)
	if secs <= 0 {
		return nil, fmt.Errorf("timeout has to be more than zero seconds")
	}
	return &s7Server{host: host, port: port, rack: rack, slot: slot,
		timeout: time.Duration(secs * float64(time.Second))}, nil
}

func (s *s7Server) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }
func (s *s7Server) address() string                                          { return net.JoinHostPort(s.host, strconv.Itoa(s.port)) }
func (s *s7Server) rackSlot() (int, int)                                     { return s.rack, s.slot }
func (s *s7Server) requestTimeout() time.Duration                            { return s.timeout }

func (s *s7Server) Close(context.Context, bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.h != nil {
		err := s.h.Close()
		s.h, s.c = nil, nil
		return err
	}
	return nil
}

func (s *s7Server) do(fn func(c gos7.Client) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.h == nil {
		h := gos7.NewTCPClientHandler(s.address(), s.rack, s.slot)
		h.Timeout = s.timeout
		// The connection is kept for as long as it works. The library's idle
		// close would race a request that is about to be sent.
		h.IdleTimeout = 0
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		start := time.Now()
		err := h.ConnectContext(ctx)
		cancel()
		if err != nil {
			_ = h.Close()
			// A CPU that takes the TCP connection and never answers the ISO
			// request comes back from the library as an invalid PDU, since it
			// replaces the timeout with that. Waiting out the whole timeout for
			// it is what gives it away.
			m := err.Error()
			if (strings.Contains(m, "Invalid PDU") || strings.Contains(m, "PDU negotiation")) && time.Since(start) >= s.timeout*9/10 {
				return fmt.Errorf("i/o timeout waiting for the S7 connection to be accepted")
			}
			return err
		}
		s.h, s.c = h, gos7.NewClient(h)
	}
	err := fn(s.c)
	if err != nil && !s7KeepsConnection(err) {
		_ = s.h.Close()
		s.h, s.c = nil, nil
	}
	return err
}

// s7KeepsConnection says whether a failed request leaves the connection good to
// use. The CPU's own refusals (an address that isn't there, a function it won't
// do) are answers, and the connection is fine. Anything else, a timeout, a
// reset, a telegram that doesn't parse, leaves the stream in a state nobody can
// vouch for, and a late reply on a reused connection would answer the next
// request with the last one's data.
func s7KeepsConnection(err error) bool {
	m := err.Error()
	return strings.HasPrefix(m, "CPU : ") && !strings.Contains(m, "total data exceeds the PDU size")
}

// explainS7 turns what the library and the socket say into what the operator
// does next.
func explainS7(addr string, rack, slot int, timeout time.Duration, what string, err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	var opErr *net.OpError
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return fmt.Errorf("%s: couldn't find a host called %q. Check the spelling, that this host can reach "+
			"its DNS, or use the CPU's IP address", what, dnsErr.Name)
	case strings.Contains(m, "errIsoConnect"), strings.Contains(m, "ISO : Connection Error"):
		return fmt.Errorf("%s: %s accepted the TCP connection and refused the S7 one. Rack %d and slot %d have "+
			"to be where the CPU is in the hardware configuration, and an S7-1200 or S7-1500 has to permit "+
			"PUT/GET access in its protection settings", what, addr, rack, slot)
	case strings.Contains(m, "PDU negotiation"), strings.Contains(m, "Invalid PDU"), strings.Contains(m, "invalid pdu"):
		return fmt.Errorf("%s: %s answered with something that isn't S7. Check that port %s really is an S7 "+
			"CPU and not another service. The connection was closed", what, addr, portOf(addr))
	case strings.Contains(m, "CPU : Address out of range"):
		return fmt.Errorf("%s: %s says the address is out of range. The data block doesn't exist, or the "+
			"offset is past its end. Check the DB number and the offset in the CPU's project, and that the "+
			"block is global with Optimized block access turned off", what, addr)
	case strings.Contains(m, "CPU : Item not available"):
		return fmt.Errorf("%s: %s says the item isn't available. The data block or the area isn't there, or "+
			"access to it is switched off. Check the address, and on an S7-1200 or S7-1500 that PUT/GET "+
			"access is permitted and the data block's Optimized block access is off", what, addr)
	case strings.Contains(m, "CPU : Function not authorized"):
		return fmt.Errorf("%s: %s is protected against this. Check the CPU's protection level, which has to "+
			"allow full access from outside for a read or a write", what, addr)
	case strings.Contains(m, "CPU : Data size mismatch"), strings.Contains(m, "CPU : Invalid Transport size"):
		return fmt.Errorf("%s: %s refused it as the wrong size or type of data (%s). Check the address and the "+
			"data type against the block's layout", what, addr, strings.TrimPrefix(m, "CPU : "))
	case strings.HasPrefix(m, "CPU : "):
		return fmt.Errorf("%s: %s refused it: %s", what, addr, strings.TrimPrefix(m, "CPU : "))
	case errors.As(err, &opErr) && opErr.Timeout(), strings.Contains(m, "i/o timeout"):
		return fmt.Errorf("%s: no answer from %s within %s. The CPU is silent. Check that it is powered and "+
			"reachable, and that nothing on the way drops the traffic. The connection was closed, and the next "+
			"message tries again from a fresh one", what, addr, timeout)
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(strings.ToLower(m), "refused"):
		return fmt.Errorf("%s: %s refused the connection. The host is up and nothing is listening on port %s. "+
			"Check the port, and that the S7 connection is switched on in the CPU", what, addr, portOf(addr))
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &opErr):
		return fmt.Errorf("%s: lost the connection to %s (%v). A CPU takes only a few S7 connections at a time, "+
			"so check nothing else has them. The next message reconnects", what, addr, err)
	}
	return fmt.Errorf("%s: %s: %w", what, addr, err)
}

// ---------------------------------------------------------------------------
// Addresses
// ---------------------------------------------------------------------------

// s7Addr is a Siemens absolute address read apart.
type s7Addr struct {
	area   byte // 'D' data block, 'M' merkers, 'I' inputs, 'Q' outputs
	db     int
	byteAt int
	bit    int  // -1 when there is none
	letter byte // 'X' bit, 'B' byte, 'W' word, 'D' double word
	text   string
}

var s7AddrForm = regexp.MustCompile(`^(?:DB(\d+)\.DB([XBWD])(\d+)(?:\.([0-7]))?|([MIEQA])([BWD]?)(\d+)(?:\.([0-7]))?)$`)

// parseS7Address reads DB1.DBX0.0, DB1.DBW4, M10.2, MD20, I0.0, IB1, Q0.1 and
// QW4. E for Eingang and A for Ausgang are the German spellings of I and Q.
func parseS7Address(raw string) (s7Addr, error) {
	text := strings.ToUpper(strings.NewReplacer(" ", "", "\t", "").Replace(raw))
	m := s7AddrForm.FindStringSubmatch(text)
	if m == nil {
		return s7Addr{}, fmt.Errorf("address %q isn't one this node reads. Write it as DB1.DBX0.0 for a bit, "+
			"DB1.DBB0, DB1.DBW0 or DB1.DBD0 for a byte, word or double word, or M10.2, MW10, I0.0, IB0, "+
			"Q0.1 or QW0 for the merkers, inputs and outputs", raw)
	}
	a := s7Addr{bit: -1, text: text}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	if m[1] != "" {
		a.area, a.db, a.letter, a.byteAt = 'D', atoi(m[1]), m[2][0], atoi(m[3])
		if m[4] != "" {
			a.bit = atoi(m[4])
		}
		if a.db < 1 || a.db > 65535 {
			return s7Addr{}, fmt.Errorf("data block %d in %q is out of range, it has to be 1 to 65535", a.db, raw)
		}
	} else {
		switch m[5] {
		case "M":
			a.area = 'M'
		case "I", "E":
			a.area = 'I'
		default:
			a.area = 'Q'
		}
		a.byteAt = atoi(m[7])
		if m[8] != "" {
			a.bit = atoi(m[8])
		}
		a.letter = 'X'
		if m[6] != "" {
			a.letter = m[6][0]
		}
		if m[6] == "" && a.bit < 0 {
			return s7Addr{}, fmt.Errorf("address %q has no size. Write %sB%d for a byte, %sW%d for a word, "+
				"or %s%d.0 for a bit", raw, m[5], a.byteAt, m[5], a.byteAt, m[5], a.byteAt)
		}
	}
	if a.letter == 'X' && a.bit < 0 {
		return s7Addr{}, fmt.Errorf("address %q is a bit address and has to say which bit, like %s.0", raw, text)
	}
	if a.letter != 'X' && a.bit >= 0 {
		return s7Addr{}, fmt.Errorf("address %q names a bit and a %s size at once. A bit is X, as in DB1.DBX0.0",
			raw, map[byte]string{'B': "byte", 'W': "word", 'D': "double word"}[a.letter])
	}
	if a.byteAt > 2097151 {
		return s7Addr{}, fmt.Errorf("offset %d in %q is out of range (0 to 2097151)", a.byteAt, raw)
	}
	return a, nil
}

func (a s7Addr) area0() int {
	switch a.area {
	case 'D':
		return s7AreaDB
	case 'M':
		return s7AreaMerkers
	case 'I':
		return s7AreaInputs
	}
	return s7AreaOutputs
}

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

type s7Type struct {
	name string
	size int  // bytes, 0 for a string and 1 bit for a bool
	kind byte // 'b' bool, 'u' unsigned, 'i' signed, 'f' float, 's' string
}

var s7Types = map[string]s7Type{
	"bool":    {"bool", 0, 'b'},
	"uint8":   {"uint8", 1, 'u'},
	"int8":    {"int8", 1, 'i'},
	"uint16":  {"uint16", 2, 'u'},
	"int16":   {"int16", 2, 'i'},
	"uint32":  {"uint32", 4, 'u'},
	"int32":   {"int32", 4, 'i'},
	"uint64":  {"uint64", 8, 'u'},
	"int64":   {"int64", 8, 'i'},
	"float32": {"float32", 4, 'f'},
	"float64": {"float64", 8, 'f'},
	"string":  {"string", 0, 's'},
}

func s7TypeOptions() []node.Option {
	return []node.Option{
		{Value: "bool", Label: "Bool, one bit (BOOL)"},
		{Value: "uint8", Label: "Unsigned 8 bit (BYTE, USINT)"},
		{Value: "int8", Label: "Signed 8 bit (SINT)"},
		{Value: "uint16", Label: "Unsigned 16 bit (WORD, UINT)"},
		{Value: "int16", Label: "Signed 16 bit (INT)"},
		{Value: "uint32", Label: "Unsigned 32 bit (DWORD, UDINT)"},
		{Value: "int32", Label: "Signed 32 bit (DINT)"},
		{Value: "uint64", Label: "Unsigned 64 bit (LWORD, ULINT)"},
		{Value: "int64", Label: "Signed 64 bit (LINT)"},
		{Value: "float32", Label: "Float 32 bit (REAL)"},
		{Value: "float64", Label: "Float 64 bit (LREAL)"},
		{Value: "string", Label: "String (STRING)"},
	}
}

// checkLetter makes sure the size letter in an address and the data type agree.
// A DBW read as a float would read four bytes where the address says two, and
// that is a mistake worth stopping before it reaches a PLC.
func (a s7Addr) checkLetter(t s7Type) error {
	var want string
	switch t.kind {
	case 'b':
		want = "X"
	case 's':
		want = "B"
	default:
		switch t.size {
		case 1:
			want = "B"
		case 2:
			want = "W"
		case 4:
			want = "D"
		case 8:
			want = "BD"
		}
	}
	if strings.IndexByte(want, a.letter) >= 0 {
		return nil
	}
	prefix := string(a.text[0])
	if a.area == 'D' {
		prefix = "DB" + strconv.Itoa(a.db) + ".DB"
	}
	letters := map[byte]string{'X': "a bit", 'B': "a byte", 'W': "a word", 'D': "a double word"}
	suggest := prefix + string(want[0]) + strconv.Itoa(a.byteAt)
	if want == "X" {
		suggest += ".0"
		if a.area != 'D' {
			suggest = prefix + strconv.Itoa(a.byteAt) + ".0"
		}
	}
	return fmt.Errorf("%s addresses %s and data type %s doesn't fit that. Use %s", a.text, letters[a.letter], t.name, suggest)
}

func s7Decode(t s7Type, b []byte) any {
	switch t.kind {
	case 'u':
		switch t.size {
		case 1:
			return float64(b[0])
		case 2:
			return float64(binary.BigEndian.Uint16(b))
		case 4:
			return float64(binary.BigEndian.Uint32(b))
		}
		u := binary.BigEndian.Uint64(b)
		return bigInt(strconv.FormatUint(u, 10), float64(u), u <= 1<<53)
	case 'i':
		switch t.size {
		case 1:
			return float64(int8(b[0]))
		case 2:
			return float64(int16(binary.BigEndian.Uint16(b)))
		case 4:
			return float64(int32(binary.BigEndian.Uint32(b)))
		}
		s := int64(binary.BigEndian.Uint64(b))
		return bigInt(strconv.FormatInt(s, 10), float64(s), s >= -(1<<53) && s <= 1<<53)
	case 'f':
		if t.size == 4 {
			return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
		}
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return nil
}

func s7Encode(t s7Type, v any) ([]byte, error) {
	b := make([]byte, t.size)
	if t.size == 8 && (t.kind == 'u' || t.kind == 'i') {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			if t.kind == 'u' {
				n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%q doesn't fit uint64 (0 to 18446744073709551615)", s)
				}
				binary.BigEndian.PutUint64(b, n)
				return b, nil
			}
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q doesn't fit int64", s)
			}
			binary.BigEndian.PutUint64(b, uint64(n))
			return b, nil
		}
	}
	f, ok := asFloat(v)
	if !ok {
		return nil, fmt.Errorf("%v (%s) isn't a number, and %s holds a number", v, describeType(v), t.name)
	}
	whole := func(lo, hi float64) error {
		if f != math.Trunc(f) {
			return fmt.Errorf("%v isn't a whole number, and %s holds only whole numbers. Round it first, or use a float type", f, t.name)
		}
		if f < lo || f > hi {
			return fmt.Errorf("%v is outside what %s holds (%v to %v)", f, t.name, lo, hi)
		}
		return nil
	}
	switch t.name {
	case "uint8":
		if err := whole(0, math.MaxUint8); err != nil {
			return nil, err
		}
		b[0] = byte(f)
	case "int8":
		if err := whole(math.MinInt8, math.MaxInt8); err != nil {
			return nil, err
		}
		b[0] = byte(int8(f))
	case "uint16":
		if err := whole(0, math.MaxUint16); err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint16(b, uint16(f))
	case "int16":
		if err := whole(math.MinInt16, math.MaxInt16); err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint16(b, uint16(int16(f)))
	case "uint32":
		if err := whole(0, math.MaxUint32); err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint32(b, uint32(f))
	case "int32":
		if err := whole(math.MinInt32, math.MaxInt32); err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint32(b, uint32(int32(f)))
	case "uint64":
		if err := whole(0, 1<<53); err != nil {
			return nil, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		binary.BigEndian.PutUint64(b, uint64(f))
	case "int64":
		if err := whole(-(1 << 53), 1<<53); err != nil {
			return nil, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		binary.BigEndian.PutUint64(b, uint64(int64(f)))
	case "float32":
		g := float32(f)
		if math.IsInf(float64(g), 0) && !math.IsInf(f, 0) {
			return nil, fmt.Errorf("%v is too big for a float32", f)
		}
		binary.BigEndian.PutUint32(b, math.Float32bits(g))
	case "float64":
		binary.BigEndian.PutUint64(b, math.Float64bits(f))
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// Reading and writing a span of bytes
// ---------------------------------------------------------------------------

func s7ReadBytes(c gos7.Client, a s7Addr, at, n int) ([]byte, error) {
	buf := make([]byte, n)
	var err error
	switch a.area {
	case 'D':
		err = c.AGReadDB(a.db, at, n, buf)
	case 'M':
		err = c.AGReadMB(at, n, buf)
	case 'I':
		err = c.AGReadEB(at, n, buf)
	default:
		err = c.AGReadAB(at, n, buf)
	}
	return buf, err
}

func s7WriteBytes(c gos7.Client, a s7Addr, at int, data []byte) error {
	switch a.area {
	case 'D':
		return c.AGWriteDB(a.db, at, len(data), data)
	case 'M':
		return c.AGWriteMB(at, len(data), data)
	case 'I':
		return c.AGWriteEB(at, len(data), data)
	}
	return c.AGWriteAB(at, len(data), data)
}

// s7WriteBits sets single bits with the protocol's own bit write, which changes
// only that bit and leaves the rest of the byte as the PLC has it. The library
// takes the address of such an item as a bit number from the start of the area.
// Each bit is a telegram of its own: a PLC answers a telegram with several
// items, but a simulator may not, and one bit at a time works with both.
func s7WriteBits(c gos7.Client, a s7Addr, bits []bool) error {
	for i, on := range bits {
		v := byte(0)
		if on {
			v = 1
		}
		at := a.byteAt*8 + a.bit + i
		items := []gos7.S7DataItem{{Area: a.area0(), WordLen: s7WordLenBit, DBNumber: a.db,
			Start: at, Amount: 1, Data: []byte{v}}}
		if err := c.AGWriteMulti(items, 1); err != nil {
			return err
		}
		if items[0].Error != "" {
			return fmt.Errorf("CPU : %s", items[0].Error)
		}
	}
	return nil
}

// s7Info is what a node leaves on msg.s7.
func s7Info(addr string, a s7Addr, dataType string, quantity int) map[string]any {
	m := map[string]any{
		"server":   addr,
		"address":  a.text,
		"quantity": float64(quantity),
		"dataType": dataType,
	}
	return m
}

func s7Target0(svc node.Services, cfgID string, cached *s7Target) (s7Target, error) {
	if *cached != nil {
		return *cached, nil
	}
	cfg, ok := svc.ConfigNode(cfgID)
	if !ok {
		return nil, fmt.Errorf("the S7 server config %s is not running. Open this node and pick a server", cfgID)
	}
	t, ok := cfg.(s7Target)
	if !ok {
		return nil, fmt.Errorf("config node %s is not an S7 server", cfgID)
	}
	*cached = t
	return t, nil
}

func s7AddressTypes() []string {
	return []string{node.TypeStr, node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeEnv, node.TypeJSONata}
}

func s7AddressFrom(tv TypedValue, ec EvalContext) (s7Addr, error) {
	v, ok, err := tv.Eval(ec)
	if err != nil {
		return s7Addr{}, fmt.Errorf("address: %w", err)
	}
	s, isStr := v.(string)
	if !ok || v == nil || !isStr || strings.TrimSpace(s) == "" {
		return s7Addr{}, fmt.Errorf("the address resolved to nothing. Set it on the node, or put it on the message, as text like DB1.DBW4")
	}
	return parseS7Address(s)
}

// ---------------------------------------------------------------------------
// s7 read
// ---------------------------------------------------------------------------

type s7ReadNode struct {
	cfgID    string
	address  TypedValue
	quantity TypedValue
	dtype    s7Type
	svc      node.Services
	target   s7Target
}

func registerS7Read() {
	node.MustRegister(node.Descriptor{
		Type:         "s7 read",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "s7 read",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Siemens S7 read. Not the same configuration as the community " +
				"node-red-contrib-s7, which does not run here.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server", ConfigType: "hotloop-flow-s7", Required: true},
			{Name: "address", Kind: node.PropTypedInput, Label: "Address", Required: true, Default: "DB1.DBW0",
				TypeProp: "addressType", TypedInputTypes: s7AddressTypes(), Placeholder: "DB1.DBW0",
				Help: "DB1.DBX0.0 is a bit, DB1.DBB0 a byte, DB1.DBW0 a word and DB1.DBD0 a double word. M, I and Q " +
					"are the merkers, inputs and outputs: M10.2, MW10, I0.0, QB4. E and A work for I and Q."},
			{Name: "dataType", Kind: node.PropSelect, Label: "Data type", Default: "int16", Options: s7TypeOptions()},
			{Name: "quantity", Kind: node.PropTypedInput, Label: "How many values", Default: "1", Required: true,
				TypeProp: "quantityType", TypedInputTypes: modbusNumTypes(),
				Help: "Values one after another from the address. Bits count on from the bit named."},
		},
		Help: "Reads a Siemens S7 CPU when a message arrives. The value is msg.payload, a single value for a " +
			"quantity of 1 and a list otherwise. msg.s7 says where it came from.\n\n" +
			"A string is an S7 STRING: a maximum length byte, a length byte, then the characters. A 64 bit " +
			"integer past 2^53 comes out as a string of its exact digits, because a flow's numbers can't hold it.",
	}, newS7Read)
}

func newS7Read(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &s7ReadNode{
		cfgID:    p.PropString("server", ""),
		address:  ReadTypedValue(p.Raw, "address", "addressType", node.TypeStr),
		quantity: ReadTypedValue(p.Raw, "quantity", "quantityType", node.TypeNum),
		svc:      def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the S7 CPU to read from")
	}
	if n.quantity.Value == "" {
		n.quantity.Value = "1"
	}
	dt := p.PropString("dataType", "int16")
	t, ok := s7Types[dt]
	if !ok {
		return nil, fmt.Errorf("data type %q is not one of bool, uint8, int8, uint16, int16, uint32, int32, uint64, int64, float32, float64 or string", dt)
	}
	n.dtype = t
	for _, tv := range []TypedValue{n.address, n.quantity} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	if n.address.Type == node.TypeStr {
		// A fixed address is checked now rather than at the first message.
		a, err := parseS7Address(n.address.Value)
		if err != nil {
			return nil, err
		}
		if err := a.checkLetter(t); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *s7ReadNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *s7ReadNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	a, err := s7AddressFrom(n.address, ec)
	if err != nil {
		return n.fail(out, err)
	}
	if err := a.checkLetter(n.dtype); err != nil {
		return n.fail(out, err)
	}
	quantity, err := modbusIntFrom(n.quantity, ec, "quantity", 1, s7MaxBytes*8)
	if err != nil {
		return n.fail(out, err)
	}
	if n.dtype.kind == 's' && quantity != 1 {
		return n.fail(out, fmt.Errorf("a string is read one at a time, and the quantity is %d", quantity))
	}
	span := quantity * n.dtype.size
	if n.dtype.kind == 'b' {
		span = (a.bit + quantity + 7) / 8
	}
	if span > s7MaxBytes {
		return n.fail(out, fmt.Errorf("that is %d bytes in one read, and this node reads %d at most. Split it into more than one read",
			span, s7MaxBytes))
	}

	what := fmt.Sprintf("reading %d %s at %s", quantity, plural(quantity, n.dtype.name), a.text)
	var values []any
	addr := "stand-in"
	if si := node.StandInOf(n.svc); si != nil {
		reply, err := si.Call("s7", map[string]any{"op": "read", "address": a.text,
			"dataType": n.dtype.name, "quantity": float64(quantity)})
		if err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
		if values, err = s7ScriptedValues(reply, quantity, n.dtype.kind); err != nil {
			return n.fail(out, err)
		}
	} else {
		tgt, err := s7Target0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		addr = tgt.address()
		err = tgt.do(func(c gos7.Client) error {
			var err error
			values, err = n.read(c, a, quantity, span)
			return err
		})
		if err != nil {
			rack, slot := tgt.rackSlot()
			return n.fail(out, explainS7(addr, rack, slot, tgt.requestTimeout(), what, err))
		}
	}

	if quantity == 1 {
		m.SetPayload(values[0])
	} else {
		m.SetPayload(values)
	}
	m.Data["s7"] = s7Info(addr, a, n.dtype.name, quantity)
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(statusValue(values), 32)})
	out.Send(0, m)
	return nil
}

func (n *s7ReadNode) read(c gos7.Client, a s7Addr, quantity, span int) ([]any, error) {
	t := n.dtype
	switch t.kind {
	case 'b':
		b, err := s7ReadBytes(c, a, a.byteAt, span)
		if err != nil {
			return nil, err
		}
		out := make([]any, quantity)
		for i := range out {
			bit := a.bit + i
			out[i] = b[bit/8]&(1<<(uint(bit)%8)) != 0
		}
		return out, nil
	case 's':
		head, err := s7ReadBytes(c, a, a.byteAt, 2)
		if err != nil {
			return nil, err
		}
		maxLen, curLen := int(head[0]), int(head[1])
		if curLen > maxLen {
			return nil, fmt.Errorf("CPU : the string at %s says it holds %d characters and has room for %d. Check that the address is a STRING and not other data", a.text, curLen, maxLen)
		}
		if curLen == 0 {
			return []any{""}, nil
		}
		b, err := s7ReadBytes(c, a, a.byteAt+2, curLen)
		if err != nil {
			return nil, err
		}
		return []any{string(b)}, nil
	}
	b, err := s7ReadBytes(c, a, a.byteAt, span)
	if err != nil {
		return nil, err
	}
	out := make([]any, quantity)
	for i := range out {
		out[i] = s7Decode(t, b[i*t.size:(i+1)*t.size])
	}
	return out, nil
}

// s7ScriptedValues reads what a flow test said the CPU answered.
func s7ScriptedValues(reply map[string]any, quantity int, kind byte) ([]any, error) {
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
		switch kind {
		case 'b':
			out[i] = false
		case 's':
			out[i] = ""
		default:
			out[i] = float64(0)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// s7 write
// ---------------------------------------------------------------------------

type s7WriteNode struct {
	cfgID   string
	address TypedValue
	value   TypedValue
	dtype   s7Type
	svc     node.Services
	target  s7Target
}

func registerS7Write() {
	node.MustRegister(node.Descriptor{
		Type:         "s7 write",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "s7 write",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own Siemens S7 write. Not the same configuration as the community " +
				"node-red-contrib-s7, which does not run here.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server", ConfigType: "hotloop-flow-s7", Required: true},
			{Name: "address", Kind: node.PropTypedInput, Label: "Address", Required: true, Default: "DB1.DBW0",
				TypeProp: "addressType", TypedInputTypes: s7AddressTypes(), Placeholder: "DB1.DBW0",
				Help: "DB1.DBX0.0 is a bit, DB1.DBB0 a byte, DB1.DBW0 a word and DB1.DBD0 a double word. M, I and Q " +
					"are the merkers, inputs and outputs: M10.2, MW10, I0.0, QB4. E and A work for I and Q."},
			{Name: "value", Kind: node.PropTypedInput, Label: "Value", Default: "payload", Required: true,
				TypeProp: "valueType",
				TypedInputTypes: []string{node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeNum,
					node.TypeBool, node.TypeStr, node.TypeJSON, node.TypeEnv, node.TypeJSONata},
				Help: "A list writes consecutive values, or consecutive bits, starting at the address."},
			{Name: "dataType", Kind: node.PropSelect, Label: "Data type", Default: "int16", Options: s7TypeOptions()},
		},
		Help: "Writes to a Siemens S7 CPU when a message arrives, then passes the message on. " +
			"This changes the machine. There is no confirmation step and no read back: the message " +
			"that arrives at the output means the CPU acknowledged the write.\n\n" +
			"A bit is written with the protocol's own bit write, which changes that bit and no other in " +
			"its byte. A string is written into an S7 STRING that is already there: the node reads its " +
			"maximum length, refuses text that is longer, and writes the length and the characters.",
	}, newS7Write)
}

func newS7Write(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &s7WriteNode{
		cfgID:   p.PropString("server", ""),
		address: ReadTypedValue(p.Raw, "address", "addressType", node.TypeStr),
		value:   ReadTypedValue(p.Raw, "value", "valueType", node.TypeMsg),
		svc:     def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected: pick the S7 CPU to write to")
	}
	if n.value.Value == "" {
		n.value = TypedValue{Type: node.TypeMsg, Value: "payload"}
	}
	dt := p.PropString("dataType", "int16")
	t, ok := s7Types[dt]
	if !ok {
		return nil, fmt.Errorf("data type %q is not one of bool, uint8, int8, uint16, int16, uint32, int32, uint64, int64, float32, float64 or string", dt)
	}
	n.dtype = t
	for _, tv := range []TypedValue{n.address, n.value} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	if n.address.Type == node.TypeStr {
		a, err := parseS7Address(n.address.Value)
		if err != nil {
			return nil, err
		}
		if err := a.checkLetter(t); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *s7WriteNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *s7WriteNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	a, err := s7AddressFrom(n.address, ec)
	if err != nil {
		return n.fail(out, err)
	}
	if err := a.checkLetter(n.dtype); err != nil {
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
	items, isList := raw.([]any)
	if !isList {
		items = []any{raw}
	}
	if len(items) == 0 {
		return n.fail(out, fmt.Errorf("the value is an empty list, so there is nothing to write"))
	}
	t := n.dtype
	if t.kind == 's' && len(items) != 1 {
		return n.fail(out, fmt.Errorf("a string is written one at a time, and the value is a list of %d", len(items)))
	}

	var bits []bool
	var data []byte
	var text string
	switch t.kind {
	case 'b':
		for i, it := range items {
			b, err := modbusBool(it)
			if err != nil {
				return n.fail(out, fmt.Errorf("value %d: %w", i, err))
			}
			bits = append(bits, b)
		}
		if len(bits) > s7MaxBytes*8 {
			return n.fail(out, fmt.Errorf("that is %d bits in one write, and this node writes %d at most", len(bits), s7MaxBytes*8))
		}
	case 's':
		s, isStr := items[0].(string)
		if !isStr {
			return n.fail(out, fmt.Errorf("%v (%s) isn't text, and a string holds text", items[0], describeType(items[0])))
		}
		if len(s) > 254 {
			return n.fail(out, fmt.Errorf("that is %d bytes of text, and an S7 STRING holds 254 at most", len(s)))
		}
		text = s
	default:
		for i, it := range items {
			b, err := s7Encode(t, it)
			if err != nil {
				if isList {
					return n.fail(out, fmt.Errorf("value %d: %w", i, err))
				}
				return n.fail(out, err)
			}
			data = append(data, b...)
		}
		if len(data) > s7MaxBytes {
			return n.fail(out, fmt.Errorf("that is %d bytes in one write, and this node writes %d at most. Split it into more than one write",
				len(data), s7MaxBytes))
		}
	}

	count := len(items)
	what := fmt.Sprintf("writing %d %s at %s", count, plural(count, t.name), a.text)
	addr := "stand-in"
	if si := node.StandInOf(n.svc); si != nil {
		if _, err := si.Call("s7", map[string]any{"op": "write", "address": a.text,
			"dataType": t.name, "values": items}); err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
	} else {
		tgt, err := s7Target0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		addr = tgt.address()
		err = tgt.do(func(c gos7.Client) error {
			switch t.kind {
			case 'b':
				return s7WriteBits(c, a, bits)
			case 's':
				head, err := s7ReadBytes(c, a, a.byteAt, 1)
				if err != nil {
					return err
				}
				if len(text) > int(head[0]) {
					return fmt.Errorf("CPU : the text is %d bytes and the string at %s has room for %d", len(text), a.text, head[0])
				}
				return s7WriteBytes(c, a, a.byteAt+1, append([]byte{byte(len(text))}, text...))
			}
			return s7WriteBytes(c, a, a.byteAt, data)
		})
		if err != nil {
			rack, slot := tgt.rackSlot()
			return n.fail(out, explainS7(addr, rack, slot, tgt.requestTimeout(), what, err))
		}
	}

	m.Data["s7"] = s7Info(addr, a, t.name, count)
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate("wrote "+statusValue(items), 32)})
	out.Send(0, m)
	return nil
}
