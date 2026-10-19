package nodes

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// The Sparkplug B payload, written against the schema in the Eclipse Tahu
// repository (sparkplug_b/sparkplug_b.proto) and the Sparkplug 3.0 specification
// rather than generated from it. It is the one message on the wire, small
// enough to write out by hand: a Payload with a timestamp, a list of Metrics, a
// sequence number, and a Metric with a name, an alias, a timestamp, a datatype,
// three flags and one of seven values. The tests decode what this writes with
// the real schema compiled at test time and a protobuf implementation that is
// not this one, which is what keeps a hand-written encoder honest.
//
// The array types, DataSet, Template and a metric's PropertySet are in
// sparkplug_complex.go. Not carried: a metric's metadata (the MetaData message
// for files and multi-part bytes). It is skipped over, not misread.

// Sparkplug datatypes, the enum in the schema.
const (
	spInt8     = 1
	spInt16    = 2
	spInt32    = 3
	spInt64    = 4
	spUInt8    = 5
	spUInt16   = 6
	spUInt32   = 7
	spUInt64   = 8
	spFloat    = 9
	spDouble   = 10
	spBoolean  = 11
	spString   = 12
	spDateTime = 13
	spText     = 14
	spUUID     = 15
	spDataSet  = 16
	spBytes    = 17
	spFile     = 18
	spTemplate = 19

	spInt8Array     = 22
	spInt16Array    = 23
	spInt32Array    = 24
	spInt64Array    = 25
	spUInt8Array    = 26
	spUInt16Array   = 27
	spUInt32Array   = 28
	spUInt64Array   = 29
	spFloatArray    = 30
	spDoubleArray   = 31
	spBooleanArray  = 32
	spStringArray   = 33
	spDateTimeArray = 34
)

// spTypeNames are the datatypes an edge node can declare, by the name the
// editor shows and the flow file stores.
var spTypeNames = map[string]uint32{
	"Int8": spInt8, "Int16": spInt16, "Int32": spInt32, "Int64": spInt64,
	"UInt8": spUInt8, "UInt16": spUInt16, "UInt32": spUInt32, "UInt64": spUInt64,
	"Float": spFloat, "Double": spDouble, "Boolean": spBoolean, "String": spString,
	"DateTime": spDateTime, "Text": spText, "UUID": spUUID, "Bytes": spBytes,
	"DataSet": spDataSet, "Template": spTemplate,
	"Int8Array": spInt8Array, "Int16Array": spInt16Array, "Int32Array": spInt32Array, "Int64Array": spInt64Array,
	"UInt8Array": spUInt8Array, "UInt16Array": spUInt16Array, "UInt32Array": spUInt32Array,
	"UInt64Array": spUInt64Array, "FloatArray": spFloatArray, "DoubleArray": spDoubleArray,
	"BooleanArray": spBooleanArray, "StringArray": spStringArray, "DateTimeArray": spDateTimeArray,
}

// spTypeName is the name of any datatype in the enum, for messages and for what
// a host puts on msg.sparkplug.
func spTypeName(dt uint32) string {
	for n, v := range spTypeNames {
		if v == dt {
			return n
		}
	}
	switch dt {
	case spDataSet:
		return "DataSet"
	case spFile:
		return "File"
	case spTemplate:
		return "Template"
	case 20:
		return "PropertySet"
	case 21:
		return "PropertySetList"
	case spInt8Array:
		return "Int8Array"
	case spInt16Array:
		return "Int16Array"
	case spInt32Array:
		return "Int32Array"
	case spInt64Array:
		return "Int64Array"
	case spUInt8Array:
		return "UInt8Array"
	case spUInt16Array:
		return "UInt16Array"
	case spUInt32Array:
		return "UInt32Array"
	case spUInt64Array:
		return "UInt64Array"
	case spFloatArray:
		return "FloatArray"
	case spDoubleArray:
		return "DoubleArray"
	case spBooleanArray:
		return "BooleanArray"
	case spStringArray:
		return "StringArray"
	case spDateTimeArray:
		return "DateTimeArray"
	case 0:
		return "Unknown"
	}
	return "Unknown(" + strconv.Itoa(int(dt)) + ")"
}

// spRaw is the one value a metric carries on the wire, before its datatype says
// what it means.
type spRaw struct {
	kind byte // 0 none, 'u' integer, 'f' float, 'd' double, 'b' bool, 's' string, 'x' bytes, 'D' a DataSet, 'T' a Template (those two hold the message, still encoded, in x)
	u    uint64
	f    float64
	s    string
	x    []byte
}

// spMetric is a metric as it travels. Value is what the datatype makes of the
// raw value, filled by decode and used by encode.
type spMetric struct {
	Name         string
	Alias        uint64
	HasAlias     bool
	Timestamp    uint64
	HasTimestamp bool
	DataType     uint32
	HasDataType  bool
	IsHistorical bool
	IsTransient  bool
	IsNull       bool

	// tplRef and tplDef are what a Template metric says it is, so that a birth
	// can be checked for an instance whose definition isn't in it.
	tplRef string
	tplDef bool

	// PropsBytes is the metric's PropertySet as it is written, and Properties is
	// what it decoded to, each value saying its own type.
	PropsBytes []byte
	Properties map[string]any

	raw spRaw
	// Value is the Flow-side value, nil when there is none.
	Value any
	// Unsupported names a datatype this build carries no value for.
	Unsupported string
}

// spPayload is a whole Sparkplug payload.
type spPayload struct {
	Timestamp    uint64
	HasTimestamp bool
	Metrics      []*spMetric
	Seq          uint64
	HasSeq       bool
	UUID         string
}

// spNowMillis is the clock Sparkplug uses everywhere, UTC milliseconds.
func spNowMillis() uint64 { return uint64(time.Now().UTC().UnixMilli()) }

// ---------------------------------------------------------------------------
// Encode
// ---------------------------------------------------------------------------

func (m *spMetric) encode() ([]byte, error) {
	var b []byte
	if m.Name != "" {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, m.Name)
	}
	if m.HasAlias {
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, m.Alias)
	}
	if m.HasTimestamp {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, m.Timestamp)
	}
	if m.HasDataType {
		b = protowire.AppendTag(b, 4, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(m.DataType))
	}
	if m.IsHistorical {
		b = protowire.AppendTag(b, 5, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	if m.IsTransient {
		b = protowire.AppendTag(b, 6, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	if m.IsNull {
		b = protowire.AppendTag(b, 7, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	if m.PropsBytes != nil {
		b = protowire.AppendTag(b, 9, protowire.BytesType)
		b = protowire.AppendBytes(b, m.PropsBytes)
	}
	if m.IsNull {
		return b, nil
	}
	switch m.raw.kind {
	case 0:
	case 'u':
		// Field 10 carries 32 bit integers and field 11 carries 64 bit ones.
		if m.DataType == spInt64 || m.DataType == spUInt64 || m.DataType == spDateTime {
			b = protowire.AppendTag(b, 11, protowire.VarintType)
		} else {
			b = protowire.AppendTag(b, 10, protowire.VarintType)
		}
		b = protowire.AppendVarint(b, m.raw.u)
	case 'f':
		b = protowire.AppendTag(b, 12, protowire.Fixed32Type)
		b = protowire.AppendFixed32(b, math.Float32bits(float32(m.raw.f)))
	case 'd':
		b = protowire.AppendTag(b, 13, protowire.Fixed64Type)
		b = protowire.AppendFixed64(b, math.Float64bits(m.raw.f))
	case 'b':
		b = protowire.AppendTag(b, 14, protowire.VarintType)
		b = protowire.AppendVarint(b, m.raw.u)
	case 's':
		b = protowire.AppendTag(b, 15, protowire.BytesType)
		b = protowire.AppendString(b, m.raw.s)
	case 'x':
		b = protowire.AppendTag(b, 16, protowire.BytesType)
		b = protowire.AppendBytes(b, m.raw.x)
	case 'D':
		b = protowire.AppendTag(b, 17, protowire.BytesType)
		b = protowire.AppendBytes(b, m.raw.x)
	case 'T':
		b = protowire.AppendTag(b, 18, protowire.BytesType)
		b = protowire.AppendBytes(b, m.raw.x)
	}
	return b, nil
}

func (p *spPayload) encode() ([]byte, error) {
	var b []byte
	if p.HasTimestamp {
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, p.Timestamp)
	}
	for _, m := range p.Metrics {
		mb, err := m.encode()
		if err != nil {
			return nil, err
		}
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, mb)
	}
	if p.HasSeq {
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, p.Seq)
	}
	if p.UUID != "" {
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendString(b, p.UUID)
	}
	return b, nil
}

// spValueToRaw turns a flow's value into the raw value a datatype carries, with
// a message that says what is wrong with it when it doesn't fit.
func spValueToRaw(dt uint32, v any) (spRaw, error) {
	whole := func(lo, hi float64) (float64, error) {
		f, ok := asFloat(v)
		if !ok {
			return 0, fmt.Errorf("%v (%s) isn't a number, and %s holds a number", v, describeType(v), spTypeName(dt))
		}
		if f != math.Trunc(f) {
			return 0, fmt.Errorf("%v isn't a whole number, and %s holds only whole numbers", f, spTypeName(dt))
		}
		if f < lo || f > hi {
			return 0, fmt.Errorf("%v is outside what %s holds (%v to %v)", f, spTypeName(dt), lo, hi)
		}
		return f, nil
	}
	switch dt {
	case spDataSet:
		x, err := spDataSetBytes(v)
		return spRaw{kind: 'D', x: x}, err
	case spTemplate:
		x, err := spTemplateBytes(v)
		return spRaw{kind: 'T', x: x}, err
	case spInt8:
		f, err := whole(math.MinInt8, math.MaxInt8)
		return spRaw{kind: 'u', u: uint64(uint32(int32(f)))}, err
	case spInt16:
		f, err := whole(math.MinInt16, math.MaxInt16)
		return spRaw{kind: 'u', u: uint64(uint32(int32(f)))}, err
	case spInt32:
		f, err := whole(math.MinInt32, math.MaxInt32)
		return spRaw{kind: 'u', u: uint64(uint32(int32(f)))}, err
	case spUInt8:
		f, err := whole(0, math.MaxUint8)
		return spRaw{kind: 'u', u: uint64(f)}, err
	case spUInt16:
		f, err := whole(0, math.MaxUint16)
		return spRaw{kind: 'u', u: uint64(f)}, err
	case spUInt32:
		f, err := whole(0, math.MaxUint32)
		return spRaw{kind: 'u', u: uint64(f)}, err
	case spInt64:
		if s, ok := v.(string); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return spRaw{}, fmt.Errorf("%q doesn't fit Int64", s)
			}
			return spRaw{kind: 'u', u: uint64(n)}, nil
		}
		f, err := whole(-(1 << 53), 1<<53)
		if err != nil {
			return spRaw{}, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		return spRaw{kind: 'u', u: uint64(int64(f))}, nil
	case spUInt64:
		if s, ok := v.(string); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
			if err != nil {
				return spRaw{}, fmt.Errorf("%q doesn't fit UInt64", s)
			}
			return spRaw{kind: 'u', u: n}, nil
		}
		f, err := whole(0, 1<<53)
		if err != nil {
			return spRaw{}, fmt.Errorf("%w. Past 2^53 pass the exact number as a string, since a flow's numbers lose digits there", err)
		}
		return spRaw{kind: 'u', u: uint64(f)}, nil
	case spFloat:
		f, ok := asFloat(v)
		if !ok {
			return spRaw{}, fmt.Errorf("%v (%s) isn't a number, and Float holds a number", v, describeType(v))
		}
		if g := float32(f); math.IsInf(float64(g), 0) && !math.IsInf(f, 0) {
			return spRaw{}, fmt.Errorf("%v is too big for a Float", f)
		}
		return spRaw{kind: 'f', f: f}, nil
	case spDouble:
		f, ok := asFloat(v)
		if !ok {
			return spRaw{}, fmt.Errorf("%v (%s) isn't a number, and Double holds a number", v, describeType(v))
		}
		return spRaw{kind: 'd', f: f}, nil
	case spBoolean:
		b, err := modbusBool(v)
		if err != nil {
			return spRaw{}, err
		}
		if b {
			return spRaw{kind: 'b', u: 1}, nil
		}
		return spRaw{kind: 'b'}, nil
	case spString, spText, spUUID:
		switch t := v.(type) {
		case string:
			return spRaw{kind: 's', s: t}, nil
		case []byte:
			return spRaw{kind: 's', s: string(t)}, nil
		}
		return spRaw{kind: 's', s: stringify(v)}, nil
	case spDateTime:
		switch t := v.(type) {
		case string:
			tm, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(t))
			if err != nil {
				return spRaw{}, fmt.Errorf("%q isn't a date. Use milliseconds since 1970, or a form like 2026-10-18T14:30:00Z", t)
			}
			return spRaw{kind: 'u', u: uint64(tm.UnixMilli())}, nil
		}
		f, err := whole(0, 1<<53)
		if err != nil {
			return spRaw{}, fmt.Errorf("%w. A DateTime is milliseconds since 1970", err)
		}
		return spRaw{kind: 'u', u: uint64(f)}, nil
	case spBytes, spFile:
		switch t := v.(type) {
		case []byte:
			return spRaw{kind: 'x', x: t}, nil
		case string:
			return spRaw{kind: 'x', x: []byte(t)}, nil
		}
		return spRaw{}, fmt.Errorf("%v (%s) isn't bytes or a string", v, describeType(v))
	}
	if _, ok := spArrayElem[dt]; ok {
		x, err := spEncodeArray(dt, v)
		return spRaw{kind: 'x', x: x}, err
	}
	return spRaw{}, fmt.Errorf("data type %s can't be written by this node", spTypeName(dt))
}

// spMetricFor builds a metric from a flow's value, in a birth with its
// datatype and in data without it, as the specification asks.
func spMetricFor(name string, dt uint32, v any, birth bool, ts uint64) (*spMetric, error) {
	m := &spMetric{Name: name, Timestamp: ts, HasTimestamp: ts != 0}
	if birth {
		m.DataType, m.HasDataType = dt, true
	}
	if v == nil {
		m.IsNull = true
		if !birth {
			// A null in data still names its type, since is_null with no
			// datatype leaves a host to guess what became null.
			m.DataType, m.HasDataType = dt, true
		}
		return m, nil
	}
	raw, err := spValueToRaw(dt, v)
	if err != nil {
		return nil, err
	}
	m.raw = raw
	// 64 bit integers go in field 11 whether or not the datatype is on the wire.
	if !m.HasDataType && (dt == spInt64 || dt == spUInt64 || dt == spDateTime) {
		m.DataType = dt
	}
	m.Value = v
	if dt == spTemplate {
		tm, _ := v.(map[string]any)
		m.tplRef, _ = tm["templateRef"].(string)
		m.tplDef, _ = tm["isDefinition"].(bool)
	}
	return m, nil
}

// setProps gives the metric a PropertySet, or none.
func (m *spMetric) setProps(props map[string]any) error {
	if len(props) == 0 {
		return nil
	}
	b, err := spPropsBytes(props)
	if err != nil {
		return err
	}
	m.PropsBytes = b
	return nil
}

// ---------------------------------------------------------------------------
// Decode
// ---------------------------------------------------------------------------

func decodeSPPayload(b []byte) (*spPayload, error) {
	p := &spPayload{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("not a Sparkplug payload: %w", protowire.ParseError(n))
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, fmt.Errorf("not a Sparkplug payload: bad timestamp: %w", protowire.ParseError(n))
			}
			p.Timestamp, p.HasTimestamp = v, true
			b = b[n:]
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, fmt.Errorf("not a Sparkplug payload: bad metric: %w", protowire.ParseError(n))
			}
			m, err := decodeSPMetric(v)
			if err != nil {
				return nil, err
			}
			p.Metrics = append(p.Metrics, m)
			b = b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, fmt.Errorf("not a Sparkplug payload: bad seq: %w", protowire.ParseError(n))
			}
			p.Seq, p.HasSeq = v, true
			b = b[n:]
		case num == 4 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, fmt.Errorf("not a Sparkplug payload: bad uuid: %w", protowire.ParseError(n))
			}
			p.UUID = string(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, fmt.Errorf("not a Sparkplug payload: %w", protowire.ParseError(n))
			}
			b = b[n:]
		}
	}
	return p, nil
}

func decodeSPMetric(b []byte) (*spMetric, error) {
	m := &spMetric{}
	bad := func(what string, n int) error {
		return fmt.Errorf("not a Sparkplug payload: bad metric %s: %w", what, protowire.ParseError(n))
	}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, bad("tag", n)
		}
		b = b[n:]
		varint := func() (uint64, error) {
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return 0, bad("value", n)
			}
			b = b[n:]
			return v, nil
		}
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return nil, bad("name", n)
			}
			m.Name, b = v, b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, err := varint()
			if err != nil {
				return nil, err
			}
			m.Alias, m.HasAlias = v, true
		case num == 3 && typ == protowire.VarintType:
			v, err := varint()
			if err != nil {
				return nil, err
			}
			m.Timestamp, m.HasTimestamp = v, true
		case num == 4 && typ == protowire.VarintType:
			v, err := varint()
			if err != nil {
				return nil, err
			}
			m.DataType, m.HasDataType = uint32(v), true
		case num == 5 && typ == protowire.VarintType:
			v, err := varint()
			if err != nil {
				return nil, err
			}
			m.IsHistorical = v != 0
		case num == 6 && typ == protowire.VarintType:
			v, err := varint()
			if err != nil {
				return nil, err
			}
			m.IsTransient = v != 0
		case num == 7 && typ == protowire.VarintType:
			v, err := varint()
			if err != nil {
				return nil, err
			}
			m.IsNull = v != 0
		case (num == 10 || num == 11) && typ == protowire.VarintType:
			v, err := varint()
			if err != nil {
				return nil, err
			}
			m.raw = spRaw{kind: 'u', u: v}
		case num == 12 && typ == protowire.Fixed32Type:
			v, n := protowire.ConsumeFixed32(b)
			if n < 0 {
				return nil, bad("float", n)
			}
			m.raw, b = spRaw{kind: 'f', f: float64(math.Float32frombits(v))}, b[n:]
		case num == 13 && typ == protowire.Fixed64Type:
			v, n := protowire.ConsumeFixed64(b)
			if n < 0 {
				return nil, bad("double", n)
			}
			m.raw, b = spRaw{kind: 'd', f: math.Float64frombits(v)}, b[n:]
		case num == 14 && typ == protowire.VarintType:
			v, err := varint()
			if err != nil {
				return nil, err
			}
			m.raw = spRaw{kind: 'b', u: v}
		case num == 15 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return nil, bad("string", n)
			}
			m.raw, b = spRaw{kind: 's', s: v}, b[n:]
		case num == 16 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("bytes", n)
			}
			m.raw, b = spRaw{kind: 'x', x: append([]byte(nil), v...)}, b[n:]
		case num == 9 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("properties", n)
			}
			props, err := spPropsValue(v)
			if err != nil {
				return nil, err
			}
			m.PropsBytes, m.Properties, b = append([]byte{}, v...), props, b[n:]
		case (num == 17 || num == 18) && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("value", n)
			}
			kind := byte('D')
			if num == 18 {
				kind = 'T'
			}
			m.raw, b = spRaw{kind: kind, x: append([]byte{}, v...)}, b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, bad("field", n)
			}
			b = b[n:]
		}
	}
	return m, nil
}

// resolve fills Value from the raw value and the datatype, the metric's own or
// the one the birth said for its name. It returns an error for a value that
// doesn't match the datatype, which a host should show and not guess at.
func (m *spMetric) resolve(dt uint32) error {
	if m.IsNull || m.Unsupported != "" {
		m.Value = nil
		return nil
	}
	r := m.raw
	switch dt {
	case spInt8:
		m.Value = float64(int8(r.u))
	case spInt16:
		m.Value = float64(int16(r.u))
	case spInt32:
		m.Value = float64(int32(r.u))
	case spUInt8:
		m.Value = float64(uint8(r.u))
	case spUInt16:
		m.Value = float64(uint16(r.u))
	case spUInt32:
		m.Value = float64(uint32(r.u))
	case spInt64:
		s := int64(r.u)
		m.Value = bigInt(strconv.FormatInt(s, 10), float64(s), s >= -(1<<53) && s <= 1<<53)
	case spUInt64:
		m.Value = bigInt(strconv.FormatUint(r.u, 10), float64(r.u), r.u <= 1<<53)
	case spDateTime:
		m.Value = bigInt(strconv.FormatUint(r.u, 10), float64(r.u), r.u <= 1<<53)
	case spFloat, spDouble:
		if r.kind != 'f' && r.kind != 'd' {
			return fmt.Errorf("metric %q is a %s and carries no floating point value", m.Name, spTypeName(dt))
		}
		m.Value = r.f
	case spBoolean:
		m.Value = r.u != 0
	case spString, spText, spUUID:
		if r.kind != 's' {
			return fmt.Errorf("metric %q is a %s and carries no string", m.Name, spTypeName(dt))
		}
		m.Value = r.s
	case spBytes, spFile:
		m.Value = r.x
	case spDataSet:
		if r.kind != 'D' {
			return fmt.Errorf("metric %q is a DataSet and carries none", m.Name)
		}
		v, err := spDataSetValue(r.x)
		if err != nil {
			return fmt.Errorf("metric %q: %w", m.Name, err)
		}
		m.Value = v
	case spTemplate:
		if r.kind != 'T' {
			return fmt.Errorf("metric %q is a Template and carries none", m.Name)
		}
		v, err := spTemplateValue(r.x)
		if err != nil {
			return fmt.Errorf("metric %q: %w", m.Name, err)
		}
		m.Value = v
	case spInt8Array, spInt16Array, spInt32Array, spInt64Array, spUInt8Array, spUInt16Array, spUInt32Array,
		spUInt64Array, spFloatArray, spDoubleArray, spBooleanArray, spStringArray, spDateTimeArray:
		v, err := spDecodeArray(dt, r.x)
		if err != nil {
			return fmt.Errorf("metric %q: %w", m.Name, err)
		}
		m.Value = v
	default:
		// Unknown datatype: nothing sensible to say, and nothing to guess.
		m.Value, m.Unsupported = nil, spTypeName(dt)
	}
	return nil
}

// spDecodeArray reads the packed array types. Numbers are little endian and
// back to back. Booleans open with a 32 bit count and then pack eight to a
// byte. Strings are null terminated, one after another.
func spDecodeArray(dt uint32, b []byte) ([]any, error) {
	le := binary.LittleEndian
	width := map[uint32]int{spInt8Array: 1, spUInt8Array: 1, spInt16Array: 2, spUInt16Array: 2,
		spInt32Array: 4, spUInt32Array: 4, spFloatArray: 4, spInt64Array: 8, spUInt64Array: 8,
		spDoubleArray: 8, spDateTimeArray: 8}
	switch dt {
	case spBooleanArray:
		if len(b) < 4 {
			return nil, fmt.Errorf("a BooleanArray needs a 4 byte count and has %d bytes", len(b))
		}
		n := int(le.Uint32(b))
		bits := b[4:]
		if n > len(bits)*8 {
			return nil, fmt.Errorf("a BooleanArray of %d values needs %d bytes and has %d", n, (n+7)/8, len(bits))
		}
		out := make([]any, n)
		for i := range out {
			out[i] = bits[i/8]&(1<<(7-uint(i%8))) != 0
		}
		return out, nil
	case spStringArray:
		var out []any
		for len(b) > 0 {
			i := 0
			for i < len(b) && b[i] != 0 {
				i++
			}
			if i == len(b) {
				return nil, fmt.Errorf("a StringArray element has no terminating zero")
			}
			out = append(out, string(b[:i]))
			b = b[i+1:]
		}
		return out, nil
	}
	w := width[dt]
	if w == 0 || len(b)%w != 0 {
		return nil, fmt.Errorf("%d bytes don't divide into %s values of %d bytes", len(b), spTypeName(dt), w)
	}
	out := make([]any, 0, len(b)/w)
	for i := 0; i < len(b); i += w {
		switch dt {
		case spInt8Array:
			out = append(out, float64(int8(b[i])))
		case spUInt8Array:
			out = append(out, float64(b[i]))
		case spInt16Array:
			out = append(out, float64(int16(le.Uint16(b[i:]))))
		case spUInt16Array:
			out = append(out, float64(le.Uint16(b[i:])))
		case spInt32Array:
			out = append(out, float64(int32(le.Uint32(b[i:]))))
		case spUInt32Array:
			out = append(out, float64(le.Uint32(b[i:])))
		case spFloatArray:
			out = append(out, float64(math.Float32frombits(le.Uint32(b[i:]))))
		case spInt64Array:
			s := int64(le.Uint64(b[i:]))
			out = append(out, bigInt(strconv.FormatInt(s, 10), float64(s), s >= -(1<<53) && s <= 1<<53))
		case spUInt64Array, spDateTimeArray:
			u := le.Uint64(b[i:])
			out = append(out, bigInt(strconv.FormatUint(u, 10), float64(u), u <= 1<<53))
		case spDoubleArray:
			out = append(out, math.Float64frombits(le.Uint64(b[i:])))
		}
	}
	return out, nil
}
