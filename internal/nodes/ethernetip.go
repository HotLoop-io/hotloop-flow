package nodes

import (
	"bytes"
	"context"
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
	"github.com/danomagnum/gologix"
)

// EtherNet/IP and CIP tag access, for Logix controllers.
//
// Built on github.com/danomagnum/gologix (MIT), which does the encapsulation,
// the session, the Forward Open and the tag services. What an operator meets is
// here: the tag a flow names, what the value is read as, and what to do when
// the controller says no or says nothing.

func init() {
	registerEtherNetIPConfig()
	registerEtherNetIPRead()
	registerEtherNetIPWrite()
}

// enipMaxElements is the most array elements one read or write moves.
const enipMaxElements = 2000

type enipTarget interface {
	// do runs fn on a connected client, connecting first if there isn't one.
	do(ctx context.Context, fn func(ctx context.Context, c *gologix.Client) error) error
	address() string
	requestTimeout() time.Duration
}

// enipServer is the config node: one controller, one CIP connection, shared by
// every read and write node that points at it and used one request at a time.
type enipServer struct {
	host    string
	port    int
	path    string
	timeout time.Duration

	mu sync.Mutex
	cl *gologix.Client
}

type enipQuiet struct{}

func (enipQuiet) Debug(string, ...any) {}
func (enipQuiet) Error(string, ...any) {}
func (enipQuiet) Warn(string, ...any)  {}
func (enipQuiet) Info(string, ...any)  {}

func registerEtherNetIPConfig() {
	node.MustRegister(node.Descriptor{
		Type:     "hotloop-flow-ethernetip",
		Category: node.CategoryConfig,
		Color:    colorNetwork,
		Icon:     "network",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own EtherNet/IP connection, for tag access on ControlLogix, CompactLogix " +
				"and Micro800 controllers. It connects on the first request, drops the connection on any " +
				"failure that makes it untrustworthy, and reconnects on the next one.",
		},
		LabelProp: "name",
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "host", Kind: node.PropString, Label: "Host", Required: true, Placeholder: "10.20.1.40"},
			{Name: "port", Kind: node.PropNumber, Label: "Port", Default: 44818},
			{Name: "path", Kind: node.PropString, Label: "Path to the CPU", Default: "1,0",
				Help: "The route to the processor, as in a MSG instruction: 1,0 is the backplane and slot 0. A " +
					"ControlLogix in a different slot is 1,2. Leave it empty for a controller you reach directly, " +
					"such as a Micro800 or a CompactLogix on its own Ethernet port."},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 3,
				Help: "How long a request waits for the controller before the node gives up and drops the connection."},
		},
		Help: "One EtherNet/IP controller. Read and write nodes pointing at it share the connection and take " +
			"turns on it. PLC-5, SLC and MicroLogix, which speak PCCC and not CIP tags, are not supported.",
	}, newEtherNetIPServer)
}

func newEtherNetIPServer(def *node.Definition) (node.Node, error) {
	host := strings.TrimSpace(def.Node.PropString("host", ""))
	if host == "" {
		return nil, fmt.Errorf("host is required: the IP address or name of the controller")
	}
	if strings.ContainsAny(host, "/ ") {
		return nil, fmt.Errorf("host %q should be just an address like 10.20.1.40, with no scheme, path or port", host)
	}
	port := def.Node.PropInt("port", 44818)
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d is out of range, it has to be 1 to 65535", port)
	}
	path := strings.TrimSpace(def.Node.PropString("path", "1,0"))
	if path != "" {
		if _, err := gologix.ParsePath(path); err != nil {
			return nil, fmt.Errorf("path %q isn't a CIP route like 1,0: %v", path, err)
		}
	}
	secs := def.Node.PropFloat("timeout", 3)
	if secs <= 0 {
		return nil, fmt.Errorf("timeout has to be more than zero seconds")
	}
	return &enipServer{host: host, port: port, path: path, timeout: time.Duration(secs * float64(time.Second))}, nil
}

func (s *enipServer) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }
func (s *enipServer) address() string                                          { return net.JoinHostPort(s.host, strconv.Itoa(s.port)) }
func (s *enipServer) requestTimeout() time.Duration                            { return s.timeout }

func (s *enipServer) Close(context.Context, bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cl != nil {
		err := s.cl.Disconnect()
		s.cl = nil
		return err
	}
	return nil
}

func (s *enipServer) do(ctx context.Context, fn func(ctx context.Context, c *gologix.Client) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cl == nil {
		c := gologix.NewClient(s.address())
		c.Logger = enipQuiet{}
		c.SocketTimeout = s.timeout
		// A client that reconnects on its own would hide that the controller went
		// away. This node says so, and the next message opens a new one.
		c.AutoConnect = false
		if s.path == "" {
			c.Controller.Path = &bytes.Buffer{}
		} else {
			p, err := gologix.ParsePath(s.path)
			if err != nil {
				return err
			}
			c.Controller.Path = p
		}
		if err := c.Connect(); err != nil {
			_ = c.Disconnect()
			return err
		}
		s.cl = c
	}
	rctx, cancel := context.WithTimeout(ctx, s.timeout*2)
	defer cancel()
	start := time.Now()
	err := fn(rctx, s.cl)
	if err != nil && !s.cl.Connected() {
		s.cl = nil
		// The library gives up on a silent controller in more than one step,
		// the request and then the close it sends on the way out, and which
		// of them the error describes depends on timing. A connection that
		// died after a full timeout of waiting is a silent controller.
		m := err.Error()
		if time.Since(start) >= s.timeout*9/10 && !strings.Contains(m, "i/o timeout") &&
			!errors.As(err, new(*gologix.CIPError)) {
			err = fmt.Errorf("i/o timeout: %w", err)
		}
	}
	return err
}

var enipStatusForm = regexp.MustCompile(`status (\d+):`)

// explainEtherNetIP turns what the library and the socket say into what the
// operator does next.
func explainEtherNetIP(addr string, timeout time.Duration, what string, err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var cipErr *gologix.CIPError
	switch {
	case errors.As(err, &dnsErr):
		return fmt.Errorf("%s: couldn't find a host called %q. Check the spelling, that this host can reach "+
			"its DNS, or use the controller's IP address", what, dnsErr.Name)
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(strings.ToLower(m), "connection refused"):
		return fmt.Errorf("%s: %s refused the connection. The host is up and nothing is listening on port %s. "+
			"Check the port, and that EtherNet/IP is switched on in the controller", what, addr, portOf(addr))
	case strings.Contains(m, "i/o timeout"), strings.Contains(m, "deadline exceeded"), strings.Contains(m, "timed out"):
		return fmt.Errorf("%s: no answer from %s within %s. The controller is silent. Check that it is powered "+
			"and reachable, that the route to the CPU is right, and that nothing on the way drops the traffic. The "+
			"connection was closed, and the next message tries again from a fresh one", what, addr, timeout)
	case errors.As(err, &cipErr):
		return fmt.Errorf("%s: %s refused it (%v). Check the tag name, its scope, and that the data type is the tag's own", what, addr, cipErr)
	case strings.Contains(m, "PathSegmentError"), strings.Contains(m, "PathDestinationUnknown"), strings.Contains(m, "ObjectDoesNotExist"):
		return fmt.Errorf("%s: %s says there is no such tag. Check the spelling, that the tag is in the controller scope "+
			"(a program tag is written Program:MainProgram.Tag), and the array index", what, addr)
	case strings.Contains(m, "couldn't convert"), strings.Contains(m, "type mismatch"), strings.Contains(m, "does not match"):
		return fmt.Errorf("%s: %s holds a different data type than the node was set to (%v). Set the data type to "+
			"the tag's own", what, addr, err)
	case enipStatusForm.MatchString(m):
		code := enipStatusForm.FindStringSubmatch(m)[1]
		if code == "3" {
			return fmt.Errorf("%s: %s rejected the request with CIP status 3, invalid parameter value. The usual causes are a tag "+
				"that doesn't exist at this scope, a data type that isn't the tag's, and an array index past the end. Check "+
				"the tag name and the data type", what, addr)
		}
		return fmt.Errorf("%s: %s rejected the request with CIP status %s (%v)", what, addr, code, err)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), strings.Contains(m, "unexpected EOF"), strings.Contains(m, "forced disconnect"),
		errors.As(err, &opErr):
		return fmt.Errorf("%s: lost the connection to %s (%v). A controller takes only a limited number of EtherNet/IP "+
			"connections at a time, so check nothing else has them. The next message reconnects", what, addr, err)
	}
	return fmt.Errorf("%s: %s: %w", what, addr, err)
}

// ---------------------------------------------------------------------------
// Data types
// ---------------------------------------------------------------------------

type enipType struct {
	name string
	kind byte // 'b' bool, 'u' unsigned, 'i' signed, 'f' float, 's' string
	bits int
}

var enipTypes = map[string]enipType{
	"bool":    {"bool", 'b', 1},
	"int8":    {"int8", 'i', 8},
	"uint8":   {"uint8", 'u', 8},
	"int16":   {"int16", 'i', 16},
	"uint16":  {"uint16", 'u', 16},
	"int32":   {"int32", 'i', 32},
	"uint32":  {"uint32", 'u', 32},
	"int64":   {"int64", 'i', 64},
	"uint64":  {"uint64", 'u', 64},
	"float32": {"float32", 'f', 32},
	"float64": {"float64", 'f', 64},
	"string":  {"string", 's', 0},
}

func enipTypeOptions() []node.Option {
	return []node.Option{
		{Value: "bool", Label: "BOOL"},
		{Value: "int8", Label: "SINT, signed 8 bit"},
		{Value: "uint8", Label: "BYTE, unsigned 8 bit"},
		{Value: "int16", Label: "INT, signed 16 bit"},
		{Value: "uint16", Label: "UINT, unsigned 16 bit"},
		{Value: "int32", Label: "DINT, signed 32 bit"},
		{Value: "uint32", Label: "UDINT, unsigned 32 bit"},
		{Value: "int64", Label: "LINT, signed 64 bit"},
		{Value: "uint64", Label: "LWORD, unsigned 64 bit"},
		{Value: "float32", Label: "REAL, 32 bit float"},
		{Value: "float64", Label: "LREAL, 64 bit float"},
		{Value: "string", Label: "STRING"},
	}
}

// enipConvert turns one value from a flow into the Go type gologix writes as
// the tag's CIP type.
func enipConvert(t enipType, v any) (any, error) {
	switch t.kind {
	case 'b':
		return modbusBool(v)
	case 's':
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%v (%s) isn't text, and a string holds text", v, describeType(v))
		}
		return s, nil
	}
	if t.bits == 64 && t.kind != 'f' {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			if t.kind == 'u' {
				n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%q doesn't fit uint64 (0 to 18446744073709551615)", s)
				}
				return n, nil
			}
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q doesn't fit int64", s)
			}
			return n, nil
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
	case "int8":
		if err := whole(math.MinInt8, math.MaxInt8); err != nil {
			return nil, err
		}
		return int8(f), nil
	case "uint8":
		if err := whole(0, math.MaxUint8); err != nil {
			return nil, err
		}
		return byte(f), nil
	case "int16":
		if err := whole(math.MinInt16, math.MaxInt16); err != nil {
			return nil, err
		}
		return int16(f), nil
	case "uint16":
		if err := whole(0, math.MaxUint16); err != nil {
			return nil, err
		}
		return uint16(f), nil
	case "int32":
		if err := whole(math.MinInt32, math.MaxInt32); err != nil {
			return nil, err
		}
		return int32(f), nil
	case "uint32":
		if err := whole(0, math.MaxUint32); err != nil {
			return nil, err
		}
		return uint32(f), nil
	case "int64":
		if err := whole(-(1 << 53), 1<<53); err != nil {
			return nil, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		return int64(f), nil
	case "uint64":
		if err := whole(0, 1<<53); err != nil {
			return nil, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		return uint64(f), nil
	case "float32":
		g := float32(f)
		if math.IsInf(float64(g), 0) && !math.IsInf(f, 0) {
			return nil, fmt.Errorf("%v is too big for a float32", f)
		}
		return g, nil
	}
	return f, nil
}

// enipSlice makes the Go slice gologix reads an array into or writes one from.
func enipSlice(t enipType, n int) any {
	switch t.name {
	case "bool":
		return make([]bool, n)
	case "int8":
		return make([]int8, n)
	case "uint8":
		return make([]byte, n)
	case "int16":
		return make([]int16, n)
	case "uint16":
		return make([]uint16, n)
	case "int32":
		return make([]int32, n)
	case "uint32":
		return make([]uint32, n)
	case "int64":
		return make([]int64, n)
	case "uint64":
		return make([]uint64, n)
	case "float32":
		return make([]float32, n)
	case "float64":
		return make([]float64, n)
	}
	return make([]string, n)
}

// enipPtr is a pointer to a zero value of the tag's Go type, for a single read.
func enipPtr(t enipType) any {
	switch t.name {
	case "bool":
		return new(bool)
	case "int8":
		return new(int8)
	case "uint8":
		return new(byte)
	case "int16":
		return new(int16)
	case "uint16":
		return new(uint16)
	case "int32":
		return new(int32)
	case "uint32":
		return new(uint32)
	case "int64":
		return new(int64)
	case "uint64":
		return new(uint64)
	case "float32":
		return new(float32)
	case "float64":
		return new(float64)
	}
	return new(string)
}

// enipFlowValue is what a flow gets for one element read from a tag.
func enipFlowValue(v any) any {
	switch t := v.(type) {
	case bool, string:
		return t
	case int8:
		return float64(t)
	case byte:
		return float64(t)
	case int16:
		return float64(t)
	case uint16:
		return float64(t)
	case int32:
		return float64(t)
	case uint32:
		return float64(t)
	case int64:
		return bigInt(strconv.FormatInt(t, 10), float64(t), t >= -(1<<53) && t <= 1<<53)
	case uint64:
		return bigInt(strconv.FormatUint(t, 10), float64(t), t <= 1<<53)
	case float32:
		return float64(t)
	case float64:
		return t
	}
	return v
}

func enipValues(read any, single bool) []any {
	if single {
		switch p := read.(type) {
		case *bool:
			return []any{enipFlowValue(*p)}
		case *int8:
			return []any{enipFlowValue(*p)}
		case *byte:
			return []any{enipFlowValue(*p)}
		case *int16:
			return []any{enipFlowValue(*p)}
		case *uint16:
			return []any{enipFlowValue(*p)}
		case *int32:
			return []any{enipFlowValue(*p)}
		case *uint32:
			return []any{enipFlowValue(*p)}
		case *int64:
			return []any{enipFlowValue(*p)}
		case *uint64:
			return []any{enipFlowValue(*p)}
		case *float32:
			return []any{enipFlowValue(*p)}
		case *float64:
			return []any{enipFlowValue(*p)}
		case *string:
			return []any{enipFlowValue(*p)}
		}
		return nil
	}
	var out []any
	switch s := read.(type) {
	case []bool:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []int8:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []byte:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []int16:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []uint16:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []int32:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []uint32:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []int64:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []uint64:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []float32:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []float64:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	case []string:
		for _, v := range s {
			out = append(out, enipFlowValue(v))
		}
	}
	return out
}

// enipList builds the slice to write from converted elements.
func enipList(t enipType, items []any) any {
	s := enipSlice(t, len(items))
	switch d := s.(type) {
	case []bool:
		for i, v := range items {
			d[i] = v.(bool)
		}
	case []int8:
		for i, v := range items {
			d[i] = v.(int8)
		}
	case []byte:
		for i, v := range items {
			d[i] = v.(byte)
		}
	case []int16:
		for i, v := range items {
			d[i] = v.(int16)
		}
	case []uint16:
		for i, v := range items {
			d[i] = v.(uint16)
		}
	case []int32:
		for i, v := range items {
			d[i] = v.(int32)
		}
	case []uint32:
		for i, v := range items {
			d[i] = v.(uint32)
		}
	case []int64:
		for i, v := range items {
			d[i] = v.(int64)
		}
	case []uint64:
		for i, v := range items {
			d[i] = v.(uint64)
		}
	case []float32:
		for i, v := range items {
			d[i] = v.(float32)
		}
	case []float64:
		for i, v := range items {
			d[i] = v.(float64)
		}
	case []string:
		for i, v := range items {
			d[i] = v.(string)
		}
	}
	return s
}

func enipTarget0(svc node.Services, cfgID string, cached *enipTarget) (enipTarget, error) {
	if *cached != nil {
		return *cached, nil
	}
	cfg, ok := svc.ConfigNode(cfgID)
	if !ok {
		return nil, fmt.Errorf("the EtherNet/IP config %s is not running. Open this node and pick a controller", cfgID)
	}
	t, ok := cfg.(enipTarget)
	if !ok {
		return nil, fmt.Errorf("config node %s is not an EtherNet/IP controller", cfgID)
	}
	*cached = t
	return t, nil
}

func enipInfo(addr, tag, dataType string, quantity int) map[string]any {
	return map[string]any{"server": addr, "tag": tag, "dataType": dataType, "quantity": float64(quantity)}
}

func enipTagTypes() []string {
	return []string{node.TypeStr, node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeEnv, node.TypeJSONata}
}

func enipTagFrom(tv TypedValue, ec EvalContext) (string, error) {
	v, ok, err := tv.Eval(ec)
	if err != nil {
		return "", fmt.Errorf("tag: %w", err)
	}
	s, isStr := v.(string)
	s = strings.TrimSpace(s)
	if !ok || v == nil || !isStr || s == "" {
		return "", fmt.Errorf("the tag resolved to nothing. Set it on the node, or put it on the message, as text like Pump1.Speed")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return "", fmt.Errorf("tag %q has a space in it, and a Logix tag name has none", s)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// ethernetip read
// ---------------------------------------------------------------------------

type enipReadNode struct {
	cfgID    string
	tag      TypedValue
	quantity TypedValue
	dtype    enipType
	svc      node.Services
	target   enipTarget
}

func registerEtherNetIPRead() {
	node.MustRegister(node.Descriptor{
		Type:         "ethernetip read",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "ethernetip read",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own EtherNet/IP tag read. Not the same configuration as the community " +
				"node-red-contrib-cip-ethernet-ip, which does not run here.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Controller", ConfigType: "hotloop-flow-ethernetip", Required: true},
			{Name: "tag", Kind: node.PropTypedInput, Label: "Tag", Required: true, Default: "",
				TypeProp: "tagType", TypedInputTypes: enipTagTypes(), Placeholder: "Pump1.Speed",
				Help: "A controller tag, a member (Pump1.Speed), an array element (Samples[4]) or a bit (Status.3). A " +
					"program tag is Program:MainProgram.Tag."},
			{Name: "dataType", Kind: node.PropSelect, Label: "Data type", Default: "int32", Options: enipTypeOptions(),
				Help: "The tag's own type. A tag read as another type is refused by the controller."},
			{Name: "quantity", Kind: node.PropTypedInput, Label: "How many values", Default: "1", Required: true,
				TypeProp: "quantityType", TypedInputTypes: modbusNumTypes(),
				Help: "More than one reads that many array elements, from the one named."},
		},
		Help: "Reads a tag from a Logix controller when a message arrives. The value is msg.payload, a single value for a " +
			"quantity of 1 and a list otherwise. msg.ethernetip says where it came from.\n\n" +
			"A 64 bit integer past 2^53 comes out as a string of its exact digits, because a flow's numbers can't hold it.",
	}, newEtherNetIPRead)
}

func newEtherNetIPRead(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &enipReadNode{
		cfgID:    p.PropString("server", ""),
		tag:      ReadTypedValue(p.Raw, "tag", "tagType", node.TypeStr),
		quantity: ReadTypedValue(p.Raw, "quantity", "quantityType", node.TypeNum),
		svc:      def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no controller selected: pick the EtherNet/IP controller to read from")
	}
	if n.quantity.Value == "" {
		n.quantity.Value = "1"
	}
	dt := p.PropString("dataType", "int32")
	t, ok := enipTypes[dt]
	if !ok {
		return nil, fmt.Errorf("data type %q is not one of bool, int8, uint8, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string", dt)
	}
	n.dtype = t
	for _, tv := range []TypedValue{n.tag, n.quantity} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	if n.tag.Type == node.TypeStr && strings.TrimSpace(n.tag.Value) == "" {
		return nil, fmt.Errorf("tag is required: the name of the tag to read, like Pump1.Speed")
	}
	return n, nil
}

func (n *enipReadNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *enipReadNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	tag, err := enipTagFrom(n.tag, ec)
	if err != nil {
		return n.fail(out, err)
	}
	quantity, err := modbusIntFrom(n.quantity, ec, "quantity", 1, enipMaxElements)
	if err != nil {
		return n.fail(out, err)
	}
	if n.dtype.kind == 's' && quantity != 1 {
		return n.fail(out, fmt.Errorf("a string is read one at a time, and the quantity is %d", quantity))
	}

	what := fmt.Sprintf("reading %d %s from %s", quantity, plural(quantity, n.dtype.name), tag)
	var values []any
	addr := "stand-in"
	if si := node.StandInOf(n.svc); si != nil {
		reply, err := si.Call("ethernetip", map[string]any{"op": "read", "tag": tag,
			"dataType": n.dtype.name, "quantity": float64(quantity)})
		if err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
		if values, err = s7ScriptedValues(reply, quantity, n.dtype.kind); err != nil {
			return n.fail(out, err)
		}
	} else {
		tgt, err := enipTarget0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		addr = tgt.address()
		err = tgt.do(ctx, func(rctx context.Context, c *gologix.Client) error {
			if quantity == 1 {
				p := enipPtr(n.dtype)
				if err := c.ReadWithContext(rctx, tag, p); err != nil {
					return err
				}
				values = enipValues(p, true)
				return nil
			}
			s := enipSlice(n.dtype, quantity)
			if err := c.ReadWithContext(rctx, tag, s); err != nil {
				return err
			}
			values = enipValues(s, false)
			return nil
		})
		if err != nil {
			return n.fail(out, explainEtherNetIP(addr, tgt.requestTimeout(), what, err))
		}
		if len(values) != quantity {
			return n.fail(out, fmt.Errorf("%s: %s answered with %d values, and %d were asked for", what, addr, len(values), quantity))
		}
	}

	if quantity == 1 {
		m.SetPayload(values[0])
	} else {
		m.SetPayload(values)
	}
	m.Data["ethernetip"] = enipInfo(addr, tag, n.dtype.name, quantity)
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate(statusValue(values), 32)})
	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// ethernetip write
// ---------------------------------------------------------------------------

type enipWriteNode struct {
	cfgID  string
	tag    TypedValue
	value  TypedValue
	dtype  enipType
	svc    node.Services
	target enipTarget
}

func registerEtherNetIPWrite() {
	node.MustRegister(node.Descriptor{
		Type:         "ethernetip write",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		Align:        "right",
		PaletteLabel: "ethernetip write",
		LabelProp:    "name",
		StandIn:      true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "HotLoop Flow's own EtherNet/IP tag write. Not the same configuration as the community " +
				"node-red-contrib-cip-ethernet-ip, which does not run here.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Controller", ConfigType: "hotloop-flow-ethernetip", Required: true},
			{Name: "tag", Kind: node.PropTypedInput, Label: "Tag", Required: true, Default: "",
				TypeProp: "tagType", TypedInputTypes: enipTagTypes(), Placeholder: "Pump1.Speed",
				Help: "A controller tag, a member (Pump1.Speed), an array element (Samples[4]) or a bit (Status.3). A " +
					"program tag is Program:MainProgram.Tag."},
			{Name: "value", Kind: node.PropTypedInput, Label: "Value", Default: "payload", Required: true,
				TypeProp: "valueType",
				TypedInputTypes: []string{node.TypeMsg, node.TypeFlow, node.TypeGlobal, node.TypeNum,
					node.TypeBool, node.TypeStr, node.TypeJSON, node.TypeEnv, node.TypeJSONata},
				Help: "A list writes consecutive array elements, starting at the one named."},
			{Name: "dataType", Kind: node.PropSelect, Label: "Data type", Default: "int32", Options: enipTypeOptions(),
				Help: "The tag's own type. A tag written as another type is refused by the controller."},
		},
		Help: "Writes a tag in a Logix controller when a message arrives, then passes the message on. " +
			"This changes the machine. There is no confirmation step and no read back: the message " +
			"that arrives at the output means the controller acknowledged the write.",
	}, newEtherNetIPWrite)
}

func newEtherNetIPWrite(def *node.Definition) (node.Node, error) {
	p := def.Node
	n := &enipWriteNode{
		cfgID: p.PropString("server", ""),
		tag:   ReadTypedValue(p.Raw, "tag", "tagType", node.TypeStr),
		value: ReadTypedValue(p.Raw, "value", "valueType", node.TypeMsg),
		svc:   def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no controller selected: pick the EtherNet/IP controller to write to")
	}
	if n.value.Value == "" {
		n.value = TypedValue{Type: node.TypeMsg, Value: "payload"}
	}
	dt := p.PropString("dataType", "int32")
	t, ok := enipTypes[dt]
	if !ok {
		return nil, fmt.Errorf("data type %q is not one of bool, int8, uint8, int16, uint16, int32, uint32, int64, uint64, float32, float64 or string", dt)
	}
	n.dtype = t
	for _, tv := range []TypedValue{n.tag, n.value} {
		if err := tv.Check(); err != nil {
			return nil, err
		}
	}
	if n.tag.Type == node.TypeStr && strings.TrimSpace(n.tag.Value) == "" {
		return nil, fmt.Errorf("tag is required: the name of the tag to write, like Pump1.Speed")
	}
	return n, nil
}

func (n *enipWriteNode) fail(out node.Emitter, err error) error {
	out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
	return err
}

func (n *enipWriteNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	ec := EvalContext{Msg: m, Services: n.svc, Ctx: ctx}
	tag, err := enipTagFrom(n.tag, ec)
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
	items, isList := raw.([]any)
	if !isList {
		items = []any{raw}
	}
	if len(items) == 0 {
		return n.fail(out, fmt.Errorf("the value is an empty list, so there is nothing to write"))
	}
	if len(items) > enipMaxElements {
		return n.fail(out, fmt.Errorf("that is %d values in one write, and this node writes %d at most", len(items), enipMaxElements))
	}
	if n.dtype.kind == 's' && len(items) != 1 {
		return n.fail(out, fmt.Errorf("a string is written one at a time, and the value is a list of %d", len(items)))
	}
	conv := make([]any, len(items))
	for i, it := range items {
		c, err := enipConvert(n.dtype, it)
		if err != nil {
			if isList {
				return n.fail(out, fmt.Errorf("value %d: %w", i, err))
			}
			return n.fail(out, err)
		}
		conv[i] = c
	}

	count := len(items)
	what := fmt.Sprintf("writing %d %s to %s", count, plural(count, n.dtype.name), tag)
	addr := "stand-in"
	if si := node.StandInOf(n.svc); si != nil {
		if _, err := si.Call("ethernetip", map[string]any{"op": "write", "tag": tag,
			"dataType": n.dtype.name, "values": items}); err != nil {
			return n.fail(out, fmt.Errorf("%s: %w", what, err))
		}
	} else {
		tgt, err := enipTarget0(n.svc, n.cfgID, &n.target)
		if err != nil {
			return n.fail(out, err)
		}
		addr = tgt.address()
		err = tgt.do(ctx, func(rctx context.Context, c *gologix.Client) error {
			if count == 1 {
				return c.WriteWithContext(rctx, tag, conv[0])
			}
			return c.WriteWithContext(rctx, tag, enipList(n.dtype, conv))
		})
		if err != nil {
			return n.fail(out, explainEtherNetIP(addr, tgt.requestTimeout(), what, err))
		}
	}

	m.Data["ethernetip"] = enipInfo(addr, tag, n.dtype.name, count)
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: truncate("wrote "+statusValue(items), 32)})
	out.Send(0, m)
	return nil
}
