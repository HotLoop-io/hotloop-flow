package nodes

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// The parts of a Sparkplug B payload that are more than a number: the packed
// array types, DataSet, Template, and the PropertySet a metric carries beside
// its value. They are written by hand against the schema, like the rest of the
// payload, and the tests decode what this writes with the Eclipse schema and
// write what this reads with the same schema. Sparkplug 3.0, section 6.4:
// Array Types are in the datatype list, DataSet is 6.4.7 to 6.4.9, Template is
// 6.4.10 and 6.4.11, PropertySet and PropertyValue are 6.4.4 and 6.4.5.

// spArrayElem is the element datatype of each array datatype.
var spArrayElem = map[uint32]uint32{
	spInt8Array: spInt8, spInt16Array: spInt16, spInt32Array: spInt32, spInt64Array: spInt64,
	spUInt8Array: spUInt8, spUInt16Array: spUInt16, spUInt32Array: spUInt32, spUInt64Array: spUInt64,
	spFloatArray: spFloat, spDoubleArray: spDouble, spBooleanArray: spBoolean,
	spStringArray: spString, spDateTimeArray: spDateTime,
}

// spBasicType says whether a datatype is one of the basic ones, Int8 to Text,
// which are the only ones a DataSet column, a Template parameter and a plain
// property value may have (6.4.7, 6.4.12 and 6.4.5).
func spBasicType(dt uint32) bool { return dt >= spInt8 && dt <= spText }

// spBasicTypeByName reads a datatype name that has to be a basic one.
func spBasicTypeByName(name string) (uint32, bool) {
	dt, ok := spTypeNames[name]
	return dt, ok && spBasicType(dt)
}

// spList reads a flow's list, which is a []any from JSON and may be a typed
// slice from a function node.
func spList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []byte, nil:
		return nil, false
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// ---------------------------------------------------------------------------
// Arrays
// ---------------------------------------------------------------------------

// spEncodeArray packs a list into an array datatype's bytes: numbers
// little endian and back to back, booleans after a 32 bit count and eight to a
// byte from the top bit down, strings each ended by a zero byte.
func spEncodeArray(dt uint32, v any) ([]byte, error) {
	elem := spArrayElem[dt]
	list, ok := spList(v)
	if !ok {
		return nil, fmt.Errorf("%v (%s) isn't a list, and %s holds a list", v, describeType(v), spTypeName(dt))
	}
	le := binary.LittleEndian
	var b []byte
	if dt == spBooleanArray {
		b = make([]byte, 4, 4+(len(list)+7)/8)
		le.PutUint32(b, uint32(len(list)))
	}
	var cur byte
	for i, e := range list {
		if e == nil {
			return nil, fmt.Errorf("item %d of the list is null, and a %s can't hold a null", i, spTypeName(dt))
		}
		r, err := spValueToRaw(elem, e)
		if err != nil {
			return nil, fmt.Errorf("item %d of the list: %w", i, err)
		}
		switch dt {
		case spInt8Array, spUInt8Array:
			b = append(b, byte(r.u))
		case spInt16Array, spUInt16Array:
			b = le.AppendUint16(b, uint16(r.u))
		case spInt32Array, spUInt32Array:
			b = le.AppendUint32(b, uint32(r.u))
		case spInt64Array, spUInt64Array, spDateTimeArray:
			b = le.AppendUint64(b, r.u)
		case spFloatArray:
			b = le.AppendUint32(b, math.Float32bits(float32(r.f)))
		case spDoubleArray:
			b = le.AppendUint64(b, math.Float64bits(r.f))
		case spBooleanArray:
			if r.u != 0 {
				cur |= 1 << (7 - uint(i%8))
			}
			if i%8 == 7 {
				b, cur = append(b, cur), 0
			}
		case spStringArray:
			if strings.IndexByte(r.s, 0) >= 0 {
				return nil, fmt.Errorf("item %d of the list has a zero byte in it, and a StringArray ends each string with one", i)
			}
			b = append(append(b, r.s...), 0)
		}
	}
	if dt == spBooleanArray && len(list)%8 != 0 {
		b = append(b, cur)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// One scalar, wherever a message carries it
// ---------------------------------------------------------------------------

// spScalarField appends a raw value as the field a oneof of DataSetValue, a
// Parameter or a PropertyValue puts it in. The first number is the field for
// 32 bit integers, then 64 bit integers, floats, doubles, booleans, strings.
func spScalarField(b []byte, dt uint32, r spRaw, first protowire.Number) []byte {
	switch r.kind {
	case 'u':
		f := first
		if dt == spInt64 || dt == spUInt64 || dt == spDateTime {
			f = first + 1
		}
		b = protowire.AppendTag(b, f, protowire.VarintType)
		b = protowire.AppendVarint(b, r.u)
	case 'f':
		b = protowire.AppendTag(b, first+2, protowire.Fixed32Type)
		b = protowire.AppendFixed32(b, math.Float32bits(float32(r.f)))
	case 'd':
		b = protowire.AppendTag(b, first+3, protowire.Fixed64Type)
		b = protowire.AppendFixed64(b, math.Float64bits(r.f))
	case 'b':
		b = protowire.AppendTag(b, first+4, protowire.VarintType)
		b = protowire.AppendVarint(b, r.u)
	case 's':
		b = protowire.AppendTag(b, first+5, protowire.BytesType)
		b = protowire.AppendString(b, r.s)
	}
	return b
}

// spScalarRaw reads the six scalar fields back, given the field the 32 bit
// integer is in. It reports whether field num was one of them.
func spScalarRaw(first, num protowire.Number, typ protowire.Type, b []byte) (spRaw, int, bool, error) {
	if num < first || num > first+5 {
		return spRaw{}, 0, false, nil
	}
	switch num - first {
	case 0, 1, 4:
		if typ != protowire.VarintType {
			return spRaw{}, 0, false, nil
		}
		v, n := protowire.ConsumeVarint(b)
		if n < 0 {
			return spRaw{}, 0, true, protowire.ParseError(n)
		}
		kind := byte('u')
		if num-first == 4 {
			kind = 'b'
		}
		return spRaw{kind: kind, u: v}, n, true, nil
	case 2:
		if typ != protowire.Fixed32Type {
			return spRaw{}, 0, false, nil
		}
		v, n := protowire.ConsumeFixed32(b)
		if n < 0 {
			return spRaw{}, 0, true, protowire.ParseError(n)
		}
		return spRaw{kind: 'f', f: float64(math.Float32frombits(v))}, n, true, nil
	case 3:
		if typ != protowire.Fixed64Type {
			return spRaw{}, 0, false, nil
		}
		v, n := protowire.ConsumeFixed64(b)
		if n < 0 {
			return spRaw{}, 0, true, protowire.ParseError(n)
		}
		return spRaw{kind: 'd', f: math.Float64frombits(v)}, n, true, nil
	default:
		if typ != protowire.BytesType {
			return spRaw{}, 0, false, nil
		}
		v, n := protowire.ConsumeString(b)
		if n < 0 {
			return spRaw{}, 0, true, protowire.ParseError(n)
		}
		return spRaw{kind: 's', s: v}, n, true, nil
	}
}

// spScalarValue is the flow's value for a raw scalar of a basic datatype.
func spScalarValue(name string, dt uint32, r spRaw) (any, error) {
	m := &spMetric{Name: name, raw: r}
	if err := m.resolve(dt); err != nil {
		return nil, err
	}
	return m.Value, nil
}

// ---------------------------------------------------------------------------
// DataSet
// ---------------------------------------------------------------------------

func spDataSetBytes(v any) ([]byte, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%v (%s) isn't a DataSet. A DataSet is an object like {\"columns\": [\"Name\", \"Count\"], \"types\": [\"String\", \"Int32\"], \"rows\": [[\"a\", 1]]}", v, describeType(v))
	}
	cols, ok := spList(m["columns"])
	if !ok || len(cols) == 0 {
		return nil, fmt.Errorf("a DataSet needs \"columns\", a list of column names")
	}
	types, ok := spList(m["types"])
	if !ok || len(types) != len(cols) {
		return nil, fmt.Errorf("a DataSet needs \"types\", a list with one datatype for each of its %d columns", len(cols))
	}
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(len(cols)))
	for i, c := range cols {
		s, ok := c.(string)
		if !ok {
			return nil, fmt.Errorf("column %d: the name is %s, and has to be text", i, describeType(c))
		}
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendString(b, s)
	}
	dts := make([]uint32, len(types))
	for i, t := range types {
		name, _ := t.(string)
		dt, ok := spBasicTypeByName(name)
		if !ok {
			return nil, fmt.Errorf("column %d: type %v isn't one of Int8 to Text, the types a DataSet column may have", i, t)
		}
		dts[i] = dt
		b = protowire.AppendTag(b, 3, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(dt))
	}
	rows, _ := spList(m["rows"])
	for ri, r := range rows {
		cells, ok := spList(r)
		if !ok || len(cells) != len(cols) {
			return nil, fmt.Errorf("row %d has to be a list of %d values, one for each column", ri, len(cols))
		}
		var rb []byte
		for ci, c := range cells {
			if c == nil {
				return nil, fmt.Errorf("row %d, column %q: a DataSet value can't be null", ri, cols[ci])
			}
			raw, err := spValueToRaw(dts[ci], c)
			if err != nil {
				return nil, fmt.Errorf("row %d, column %q: %w", ri, cols[ci], err)
			}
			rb = protowire.AppendTag(rb, 1, protowire.BytesType)
			rb = protowire.AppendBytes(rb, spScalarField(nil, dts[ci], raw, 1))
		}
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendBytes(b, rb)
	}
	return b, nil
}

func spDataSetValue(b []byte) (map[string]any, error) {
	bad := func(what string, n int) error {
		return fmt.Errorf("not a Sparkplug DataSet: bad %s: %w", what, protowire.ParseError(n))
	}
	var cols []string
	var types []uint32
	var rawRows [][]byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, bad("tag", n)
		}
		b = b[n:]
		switch {
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return nil, bad("column", n)
			}
			cols, b = append(cols, v), b[n:]
		case num == 3 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, bad("type", n)
			}
			types, b = append(types, uint32(v)), b[n:]
		case num == 3 && typ == protowire.BytesType:
			// A packed list of types, which a writer other than the schema's
			// own default may send.
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("types", n)
			}
			b = b[n:]
			for len(v) > 0 {
				t, m := protowire.ConsumeVarint(v)
				if m < 0 {
					return nil, bad("type", m)
				}
				types, v = append(types, uint32(t)), v[m:]
			}
		case num == 4 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("row", n)
			}
			rawRows, b = append(rawRows, v), b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, bad("field", n)
			}
			b = b[n:]
		}
	}
	if len(types) != len(cols) {
		return nil, fmt.Errorf("not a Sparkplug DataSet: %d column names and %d column types", len(cols), len(types))
	}
	rows := make([]any, 0, len(rawRows))
	for ri, rb := range rawRows {
		row := make([]any, 0, len(cols))
		for len(rb) > 0 {
			num, typ, n := protowire.ConsumeTag(rb)
			if n < 0 {
				return nil, bad("row tag", n)
			}
			rb = rb[n:]
			if num != 1 || typ != protowire.BytesType {
				n := protowire.ConsumeFieldValue(num, typ, rb)
				if n < 0 {
					return nil, bad("row field", n)
				}
				rb = rb[n:]
				continue
			}
			cell, n := protowire.ConsumeBytes(rb)
			if n < 0 {
				return nil, bad("value", n)
			}
			rb = rb[n:]
			ci := len(row)
			if ci >= len(cols) {
				return nil, fmt.Errorf("not a Sparkplug DataSet: row %d has more values than the %d columns", ri, len(cols))
			}
			var raw spRaw
			for len(cell) > 0 {
				num, typ, n := protowire.ConsumeTag(cell)
				if n < 0 {
					return nil, bad("value tag", n)
				}
				cell = cell[n:]
				r, used, isScalar, err := spScalarRaw(1, num, typ, cell)
				if err != nil {
					return nil, fmt.Errorf("not a Sparkplug DataSet: bad value: %w", err)
				}
				if isScalar && used > 0 {
					raw, cell = r, cell[used:]
					continue
				}
				n = protowire.ConsumeFieldValue(num, typ, cell)
				if n < 0 {
					return nil, bad("value field", n)
				}
				cell = cell[n:]
			}
			v, err := spScalarValue(cols[ci], types[ci], raw)
			if err != nil {
				return nil, fmt.Errorf("DataSet row %d: %w", ri, err)
			}
			row = append(row, v)
		}
		if len(row) != len(cols) {
			return nil, fmt.Errorf("not a Sparkplug DataSet: row %d has %d values and the DataSet has %d columns", ri, len(row), len(cols))
		}
		rows = append(rows, row)
	}
	colAny := make([]any, len(cols))
	typeAny := make([]any, len(types))
	for i := range cols {
		colAny[i], typeAny[i] = cols[i], spTypeName(types[i])
	}
	return map[string]any{"columns": colAny, "types": typeAny, "rows": rows}, nil
}

// ---------------------------------------------------------------------------
// Template
// ---------------------------------------------------------------------------

// spMemberBytes encodes a template member or a property-less metric given as
// {name, type, value, properties}.
func spMemberMetric(path string, v any) (*spMetric, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is %s, and has to be an object like {name, type, value}", path, describeType(v))
	}
	name, _ := m["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("%s has no name", path)
	}
	tn, _ := m["type"].(string)
	dt, ok := spTypeNames[tn]
	if !ok {
		return nil, fmt.Errorf("%s (%s): type %q is not one of the types a metric can have", path, name, tn)
	}
	val := m["value"]
	if n, _ := m["isNull"].(bool); n {
		val = nil
	}
	mt, err := spMetricFor(name, dt, val, true, spNowMillis())
	if err != nil {
		return nil, fmt.Errorf("%s (%s): %w", path, name, err)
	}
	if p, ok := m["properties"]; ok && p != nil {
		pb, err := spPropsBytes(p)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): properties: %w", path, name, err)
		}
		mt.PropsBytes = pb
	}
	return mt, nil
}

func spTemplateBytes(v any) ([]byte, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%v (%s) isn't a Template. A Template is an object like {\"isDefinition\": true, \"metrics\": [{\"name\": \"Speed\", \"type\": \"Double\"}]}", v, describeType(v))
	}
	isDef, _ := m["isDefinition"].(bool)
	ref, _ := m["templateRef"].(string)
	if isDef && ref != "" {
		return nil, fmt.Errorf("a Template definition has no templateRef. templateRef is for an instance, to say which definition it is")
	}
	if !isDef && ref == "" {
		return nil, fmt.Errorf("a Template instance needs a templateRef, the name of the definition metric it is an instance of. Set isDefinition to true to declare a definition")
	}
	var b []byte
	if s, ok := m["version"].(string); ok && s != "" {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, s)
	}
	members, _ := spList(m["metrics"])
	for i, e := range members {
		mt, err := spMemberMetric(fmt.Sprintf("member %d", i), e)
		if err != nil {
			return nil, err
		}
		mb, err := mt.encode()
		if err != nil {
			return nil, err
		}
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, mb)
	}
	params, _ := spList(m["parameters"])
	for i, e := range params {
		pm, ok := e.(map[string]any)
		name, _ := pm["name"].(string)
		if !ok || name == "" {
			return nil, fmt.Errorf("parameter %d needs a name", i)
		}
		tn, _ := pm["type"].(string)
		dt, ok := spBasicTypeByName(tn)
		if !ok {
			return nil, fmt.Errorf("parameter %q: type %q isn't one of Int8 to Text, the types a parameter may have", name, tn)
		}
		var pb []byte
		pb = protowire.AppendTag(pb, 1, protowire.BytesType)
		pb = protowire.AppendString(pb, name)
		pb = protowire.AppendTag(pb, 2, protowire.VarintType)
		pb = protowire.AppendVarint(pb, uint64(dt))
		if pv := pm["value"]; pv != nil {
			raw, err := spValueToRaw(dt, pv)
			if err != nil {
				return nil, fmt.Errorf("parameter %q: %w", name, err)
			}
			pb = spScalarField(pb, dt, raw, 3)
		}
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, pb)
	}
	if ref != "" {
		b = protowire.AppendTag(b, 4, protowire.BytesType)
		b = protowire.AppendString(b, ref)
	}
	b = protowire.AppendTag(b, 5, protowire.VarintType)
	if isDef {
		b = protowire.AppendVarint(b, 1)
	} else {
		b = protowire.AppendVarint(b, 0)
	}
	return b, nil
}

// spMetricAsMap is a decoded metric the way a Template or the host's event
// shows it.
func spMetricAsMap(mt *spMetric) map[string]any {
	e := map[string]any{"name": mt.Name, "value": mt.Value}
	if mt.HasAlias {
		e["alias"] = float64(mt.Alias)
	}
	if mt.HasDataType {
		e["type"] = spTypeName(mt.DataType)
	}
	if mt.HasTimestamp {
		e["timestamp"] = float64(mt.Timestamp)
	}
	if mt.IsNull {
		e["isNull"] = true
	}
	if mt.Properties != nil {
		e["properties"] = mt.Properties
	}
	if mt.Unsupported != "" {
		e["unsupported"] = mt.Unsupported
	}
	return e
}

func spTemplateValue(b []byte) (map[string]any, error) {
	bad := func(what string, n int) error {
		return fmt.Errorf("not a Sparkplug Template: bad %s: %w", what, protowire.ParseError(n))
	}
	out := map[string]any{"isDefinition": false}
	var members, params []any
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, bad("tag", n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return nil, bad("version", n)
			}
			out["version"], b = v, b[n:]
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("metric", n)
			}
			b = b[n:]
			mt, err := decodeSPMetric(v)
			if err != nil {
				return nil, err
			}
			if mt.HasDataType {
				if err := mt.resolve(mt.DataType); err != nil {
					return nil, err
				}
			} else {
				mt.Value = spInfer(mt.raw)
			}
			members = append(members, spMetricAsMap(mt))
		case num == 3 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("parameter", n)
			}
			b = b[n:]
			p, err := spParameterValue(v)
			if err != nil {
				return nil, err
			}
			params = append(params, p)
		case num == 4 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return nil, bad("templateRef", n)
			}
			out["templateRef"], b = v, b[n:]
		case num == 5 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, bad("isDefinition", n)
			}
			out["isDefinition"], b = v != 0, b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, bad("field", n)
			}
			b = b[n:]
		}
	}
	if members == nil {
		members = []any{}
	}
	out["metrics"] = members
	if params != nil {
		out["parameters"] = params
	}
	return out, nil
}

func spParameterValue(b []byte) (map[string]any, error) {
	bad := func(what string, n int) error {
		return fmt.Errorf("not a Sparkplug Template: bad parameter %s: %w", what, protowire.ParseError(n))
	}
	var name string
	var dt uint32
	var raw spRaw
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, bad("tag", n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return nil, bad("name", n)
			}
			name, b = v, b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, bad("type", n)
			}
			dt, b = uint32(v), b[n:]
		default:
			r, used, ok, err := spScalarRaw(3, num, typ, b)
			if err != nil {
				return nil, fmt.Errorf("not a Sparkplug message: bad value: %w", err)
			}
			if ok && used > 0 {
				raw, b = r, b[used:]
				continue
			}
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, bad("field", n)
			}
			b = b[n:]
		}
	}
	p := map[string]any{"name": name, "type": spTypeName(dt)}
	if raw.kind != 0 {
		v, err := spScalarValue(name, dt, raw)
		if err != nil {
			return nil, fmt.Errorf("template parameter %q: %w", name, err)
		}
		p["value"] = v
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// PropertySet
// ---------------------------------------------------------------------------

// spPropTypeByName reads a property value's datatype name.
func spPropTypeByName(name string) (uint32, bool) {
	switch name {
	case "PropertySet":
		return 20, true
	case "PropertySetList":
		return 21, true
	}
	return spBasicTypeByName(name)
}

// spPropValueBytes encodes one property value. A bare value takes its type from
// what it is: a boolean, a string, a whole number that fits Int32 or Int64, any
// other number a Double, an object a PropertySet and a list of objects a
// PropertySetList. {"type": "...", "value": ...} says the type outright, which
// is how a UInt16 or a Float is sent, and how a null is.
func spPropValueBytes(key string, v any) ([]byte, error) {
	var dt uint32
	val := v
	isNull := v == nil
	if m, ok := v.(map[string]any); ok {
		if tn, has := m["type"].(string); has {
			t, ok := spPropTypeByName(tn)
			if !ok {
				return nil, fmt.Errorf("property %q: type %q isn't one of Int8 to Text, PropertySet or PropertySetList", key, tn)
			}
			dt, val = t, m["value"]
			n, _ := m["isNull"].(bool)
			isNull = val == nil || n
		}
	}
	if dt == 0 {
		switch t := v.(type) {
		case nil:
			dt = spString
		case bool:
			dt = spBoolean
		case string:
			dt = spString
		case map[string]any:
			dt = 20
		case []any:
			dt = 21
		default:
			f, ok := asFloat(v)
			switch {
			case !ok:
				return nil, fmt.Errorf("property %q: %v (%s) isn't a value a property can hold", key, t, describeType(v))
			case f == math.Trunc(f) && f >= math.MinInt32 && f <= math.MaxInt32:
				dt = spInt32
			case f == math.Trunc(f) && math.Abs(f) <= 1<<53:
				dt = spInt64
			default:
				dt = spDouble
			}
		}
	}
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(dt))
	if isNull {
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
		return b, nil
	}
	switch dt {
	case 20:
		sub, err := spPropsBytes(val)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", key, err)
		}
		b = protowire.AppendTag(b, 9, protowire.BytesType)
		b = protowire.AppendBytes(b, sub)
	case 21:
		list, ok := spList(val)
		if !ok {
			return nil, fmt.Errorf("property %q: a PropertySetList is a list of objects", key)
		}
		var lb []byte
		for i, it := range list {
			sub, err := spPropsBytes(it)
			if err != nil {
				return nil, fmt.Errorf("property %q, item %d: %w", key, i, err)
			}
			lb = protowire.AppendTag(lb, 1, protowire.BytesType)
			lb = protowire.AppendBytes(lb, sub)
		}
		b = protowire.AppendTag(b, 10, protowire.BytesType)
		b = protowire.AppendBytes(b, lb)
	default:
		raw, err := spValueToRaw(dt, val)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", key, err)
		}
		b = spScalarField(b, dt, raw, 3)
	}
	return b, nil
}

// spPropsBytes encodes a PropertySet from an object of keys and values.
func spPropsBytes(v any) ([]byte, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%v (%s) isn't a set of properties. It has to be an object like {\"Quality\": {\"type\": \"Int32\", \"value\": 192}}", v, describeType(v))
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, k)
	}
	for _, k := range keys {
		pv, err := spPropValueBytes(k, m[k])
		if err != nil {
			return nil, err
		}
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, pv)
	}
	return b, nil
}

// spPropsValue decodes a PropertySet into an object whose every value says its
// own type, so nothing about a property is guessed on the way back.
func spPropsValue(b []byte) (map[string]any, error) {
	bad := func(what string, n int) error {
		return fmt.Errorf("not a Sparkplug PropertySet: bad %s: %w", what, protowire.ParseError(n))
	}
	var keys []string
	var vals [][]byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, bad("tag", n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, n := protowire.ConsumeString(b)
			if n < 0 {
				return nil, bad("key", n)
			}
			keys, b = append(keys, v), b[n:]
		case num == 2 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("value", n)
			}
			vals, b = append(vals, v), b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, bad("field", n)
			}
			b = b[n:]
		}
	}
	if len(keys) != len(vals) {
		return nil, fmt.Errorf("not a Sparkplug PropertySet: %d keys and %d values, and there has to be the same number of each", len(keys), len(vals))
	}
	out := make(map[string]any, len(keys))
	for i, k := range keys {
		v, err := spPropValueValue(k, vals[i])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func spPropValueValue(key string, b []byte) (map[string]any, error) {
	bad := func(what string, n int) error {
		return fmt.Errorf("not a Sparkplug PropertySet: property %q: bad %s: %w", key, what, protowire.ParseError(n))
	}
	var dt uint32
	var hasType, isNull bool
	var raw spRaw
	var sub, list []byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, bad("tag", n)
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, bad("type", n)
			}
			dt, hasType, b = uint32(v), true, b[n:]
		case num == 2 && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return nil, bad("is_null", n)
			}
			isNull, b = v != 0, b[n:]
		case num == 9 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("PropertySet", n)
			}
			sub, b = v, b[n:]
			if sub == nil {
				sub = []byte{}
			}
		case num == 10 && typ == protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return nil, bad("PropertySetList", n)
			}
			list, b = v, b[n:]
			if list == nil {
				list = []byte{}
			}
		default:
			r, used, ok, err := spScalarRaw(3, num, typ, b)
			if err != nil {
				return nil, fmt.Errorf("not a Sparkplug message: bad value: %w", err)
			}
			if ok && used > 0 {
				raw, b = r, b[used:]
				continue
			}
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return nil, bad("field", n)
			}
			b = b[n:]
		}
	}
	if !hasType {
		switch {
		case sub != nil:
			dt = 20
		case list != nil:
			dt = 21
		default:
			dt = uint32(spInfer0(raw))
		}
	}
	out := map[string]any{"type": spTypeName(dt)}
	if isNull {
		out["value"] = nil
		out["isNull"] = true
		return out, nil
	}
	switch {
	case dt == 20:
		m, err := spPropsValue(sub)
		if err != nil {
			return nil, err
		}
		out["value"] = m
	case dt == 21:
		var items []any
		for len(list) > 0 {
			num, typ, n := protowire.ConsumeTag(list)
			if n < 0 {
				return nil, bad("list tag", n)
			}
			list = list[n:]
			if num != 1 || typ != protowire.BytesType {
				n := protowire.ConsumeFieldValue(num, typ, list)
				if n < 0 {
					return nil, bad("list field", n)
				}
				list = list[n:]
				continue
			}
			v, n := protowire.ConsumeBytes(list)
			if n < 0 {
				return nil, bad("list item", n)
			}
			list = list[n:]
			m, err := spPropsValue(v)
			if err != nil {
				return nil, err
			}
			items = append(items, m)
		}
		if items == nil {
			items = []any{}
		}
		out["value"] = items
	default:
		v, err := spScalarValue(key, dt, raw)
		if err != nil {
			return nil, err
		}
		out["value"] = v
	}
	return out, nil
}

// spInfer0 is the datatype of a property value that didn't say, by how it was
// carried.
func spInfer0(r spRaw) int {
	switch r.kind {
	case 'f':
		return spFloat
	case 'd':
		return spDouble
	case 'b':
		return spBoolean
	case 's':
		return spString
	}
	return spInt64
}
