package nodes

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// ---------------------------------------------------------------------------
// Arrays, DataSets, Templates and PropertySets, with no broker
// ---------------------------------------------------------------------------

func roundTripMetric(t *testing.T, typ string, v any) *spMetric {
	t.Helper()
	dt := spTypeNames[typ]
	m, err := spMetricFor("M", dt, v, true, 1760800000123)
	if err != nil {
		t.Fatalf("%s %v: %v", typ, v, err)
	}
	b, err := (&spPayload{Timestamp: 5, HasTimestamp: true, Metrics: []*spMetric{m}, Seq: 1, HasSeq: true}).encode()
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodeSPPayload(b)
	if err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	got := p.Metrics[0]
	if err := got.resolve(dt); err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	return got
}

func TestSparkplugArraysArePublishedAndComeBack(t *testing.T) {
	for _, c := range []struct {
		typ  string
		in   []any
		want []any
	}{
		{"Int8Array", []any{-128.0, 0.0, 127.0}, []any{-128.0, 0.0, 127.0}},
		{"Int16Array", []any{-32768.0, 32767.0}, []any{-32768.0, 32767.0}},
		{"Int32Array", []any{123456789.0, -987654321.0}, []any{123456789.0, -987654321.0}},
		{"Int64Array", []any{"-9223372036854775808", 5.0}, []any{"-9223372036854775808", 5.0}},
		{"UInt8Array", []any{0.0, 255.0}, []any{0.0, 255.0}},
		{"UInt16Array", []any{65535.0}, []any{65535.0}},
		{"UInt32Array", []any{4294967295.0, 1.0}, []any{4294967295.0, 1.0}},
		{"UInt64Array", []any{"18446744073709551615"}, []any{"18446744073709551615"}},
		{"FloatArray", []any{1.5, -2.25}, []any{1.5, -2.25}},
		{"DoubleArray", []any{math.Pi, -1e300}, []any{math.Pi, -1e300}},
		{"BooleanArray", []any{true, false, true, true, false, false, false, false, true}, []any{true, false, true, true, false, false, false, false, true}},
		{"BooleanArray", []any{}, []any{}},
		{"StringArray", []any{"a", "größer", ""}, []any{"a", "größer", ""}},
		{"DateTimeArray", []any{1760800000000.0, "2026-10-18T14:30:00Z"}, []any{1760800000000.0, 1792333800000.0}},
		{"Int32Array", []any{}, []any{}},
	} {
		got := roundTripMetric(t, c.typ, c.in)
		want := c.want
		if want == nil {
			want = []any{}
		}
		v, _ := got.Value.([]any)
		if v == nil {
			v = []any{}
		}
		if !reflect.DeepEqual(v, want) {
			t.Errorf("%s %v came back as %#v, want %#v", c.typ, c.in, got.Value, want)
		}
	}
	if m := roundTripMetric(t, "Int8Array", []any{1.0}); !m.HasDataType || m.DataType != spInt8Array {
		t.Errorf("an array in a birth names its datatype: %+v", m)
	}
}

func TestSparkplugArraysRefuseWhatDoesNotFitAndSayWhere(t *testing.T) {
	for _, c := range []struct {
		typ, want string
		in        any
	}{
		{"Int8Array", "item 1 of the list", []any{1.0, 200.0}},
		{"UInt16Array", "item 0 of the list", []any{-1.0}},
		{"Int32Array", "isn't a list", 5.0},
		{"Int32Array", "isn't a list", "1,2,3"},
		{"DoubleArray", "item 2 of the list is null", []any{1.0, 2.0, nil}},
		{"StringArray", "zero byte", []any{"a\x00b"}},
		{"BooleanArray", "item 1", []any{true, "maybe"}},
	} {
		_, err := spMetricFor("M", spTypeNames[c.typ], c.in, true, 0)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: %v, want it to say %q", c.typ, c.in, err, c.want)
		}
	}
}

func TestSparkplugDataSetsAreCarriedWithTheirTypes(t *testing.T) {
	in := map[string]any{
		"columns": []any{"Name", "Count", "Ratio", "Big", "On", "When", "Small"},
		"types":   []any{"String", "Int32", "Double", "UInt64", "Boolean", "DateTime", "Int8"},
		"rows": []any{
			[]any{"first", -4.0, 0.5, "18446744073709551615", true, 1760800000000.0, -3.0},
			[]any{"zweite größe", 2147483647.0, -1e10, 7.0, false, 0.0, 127.0},
		},
	}
	got := roundTripMetric(t, "DataSet", in)
	want := map[string]any{
		"columns": in["columns"], "types": in["types"],
		"rows": []any{
			[]any{"first", -4.0, 0.5, "18446744073709551615", true, 1760800000000.0, -3.0},
			[]any{"zweite größe", 2147483647.0, -1e10, 7.0, false, 0.0, 127.0},
		},
	}
	if !reflect.DeepEqual(got.Value, want) {
		t.Errorf("DataSet came back as %#v, want %#v", got.Value, want)
	}
	empty := roundTripMetric(t, "DataSet", map[string]any{"columns": []any{"a"}, "types": []any{"Float"}})
	if !reflect.DeepEqual(empty.Value, map[string]any{"columns": []any{"a"}, "types": []any{"Float"}, "rows": []any{}}) {
		t.Errorf("a DataSet with no rows: %#v", empty.Value)
	}

	for _, c := range []struct {
		want string
		in   any
	}{
		{"isn't a DataSet", 5.0},
		{"needs \"columns\"", map[string]any{"types": []any{"Int32"}}},
		{"one datatype for each of its 2 columns", map[string]any{"columns": []any{"a", "b"}, "types": []any{"Int32"}}},
		{"isn't one of Int8 to Text", map[string]any{"columns": []any{"a"}, "types": []any{"Bytes"}}},
		{"isn't one of Int8 to Text", map[string]any{"columns": []any{"a"}, "types": []any{"DataSet"}}},
		{"row 1 has to be a list of 2 values", map[string]any{"columns": []any{"a", "b"}, "types": []any{"Int32", "Int32"}, "rows": []any{[]any{1.0, 2.0}, []any{3.0}}}},
		{"can't be null", map[string]any{"columns": []any{"a"}, "types": []any{"Int32"}, "rows": []any{[]any{nil}}}},
		{"is outside what Int8 holds", map[string]any{"columns": []any{"a"}, "types": []any{"Int8"}, "rows": []any{[]any{300.0}}}},
	} {
		_, err := spMetricFor("DS", spDataSet, c.in, true, 0)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want it to say %q", c.in, err, c.want)
		}
	}
}

func TestSparkplugTemplatesAreCarriedAsDefinitionsAndInstances(t *testing.T) {
	def := map[string]any{
		"isDefinition": true, "version": "1.2",
		"parameters": []any{map[string]any{"name": "Rated", "type": "Int32", "value": 1800.0}, map[string]any{"name": "Site", "type": "String"}},
		"metrics": []any{
			map[string]any{"name": "Speed", "type": "Double", "value": 0.0},
			map[string]any{"name": "Running", "type": "Boolean"},
			map[string]any{"name": "Profile", "type": "Int16Array", "value": []any{1.0, 2.0}},
		},
	}
	got := roundTripMetric(t, "Template", def).Value.(map[string]any)
	if got["isDefinition"] != true || got["version"] != "1.2" || got["templateRef"] != nil {
		t.Errorf("definition fields: %#v", got)
	}
	params := got["parameters"].([]any)
	if len(params) != 2 || !reflect.DeepEqual(params[0], map[string]any{"name": "Rated", "type": "Int32", "value": 1800.0}) ||
		!reflect.DeepEqual(params[1], map[string]any{"name": "Site", "type": "String"}) {
		t.Errorf("parameters: %#v", params)
	}
	mem := got["metrics"].([]any)
	if len(mem) != 3 {
		t.Fatalf("members: %#v", mem)
	}
	speed := mem[0].(map[string]any)
	if speed["name"] != "Speed" || speed["type"] != "Double" || speed["value"] != 0.0 {
		t.Errorf("Speed: %#v", speed)
	}
	if r := mem[1].(map[string]any); r["isNull"] != true || r["value"] != nil {
		t.Errorf("a member with no value is null, not zero: %#v", r)
	}
	if p := mem[2].(map[string]any); !reflect.DeepEqual(p["value"], []any{1.0, 2.0}) || p["type"] != "Int16Array" {
		t.Errorf("an array member: %#v", p)
	}

	inst := map[string]any{
		"templateRef": "Motor", "isDefinition": false,
		"parameters": []any{map[string]any{"name": "Rated", "type": "Int32", "value": 3600.0}},
		"metrics": []any{
			map[string]any{"name": "Speed", "type": "Double", "value": 1750.5},
			map[string]any{"name": "Inner", "type": "Template", "value": map[string]any{
				"templateRef": "Bearing", "metrics": []any{map[string]any{"name": "Temp", "type": "Float", "value": 41.5}}}},
		},
	}
	gi := roundTripMetric(t, "Template", inst).Value.(map[string]any)
	if gi["templateRef"] != "Motor" || gi["isDefinition"] != false {
		t.Errorf("instance fields: %#v", gi)
	}
	inner := gi["metrics"].([]any)[1].(map[string]any)["value"].(map[string]any)
	if inner["templateRef"] != "Bearing" || inner["metrics"].([]any)[0].(map[string]any)["value"] != 41.5 {
		t.Errorf("a template inside a template: %#v", inner)
	}

	for _, c := range []struct {
		want string
		in   any
	}{
		{"isn't a Template", "Motor"},
		{"has no templateRef", map[string]any{"isDefinition": true, "templateRef": "Motor"}},
		{"needs a templateRef", map[string]any{"metrics": []any{}}},
		{"type \"Quaternion\" is not one of", map[string]any{"templateRef": "M", "metrics": []any{map[string]any{"name": "a", "type": "Quaternion"}}}},
		{"member 0 has no name", map[string]any{"templateRef": "M", "metrics": []any{map[string]any{"type": "Double"}}}},
		{"parameter \"p\": type \"Bytes\" isn't one of", map[string]any{"templateRef": "M", "parameters": []any{map[string]any{"name": "p", "type": "Bytes"}}}},
		{"member 0 (a): 999 is outside what UInt8 holds", map[string]any{"templateRef": "M", "metrics": []any{map[string]any{"name": "a", "type": "UInt8", "value": 999.0}}}},
	} {
		_, err := spMetricFor("T", spTemplate, c.in, true, 0)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want it to say %q", c.in, err, c.want)
		}
	}
}

func TestSparkplugPropertySetsRideWithTheMetric(t *testing.T) {
	props := map[string]any{
		"engUnit": "degC",
		"Quality": map[string]any{"type": "Int32", "value": 192.0},
		"high":    99.5,
		"count":   7.0,
		"small":   map[string]any{"type": "UInt16", "value": 65535.0},
		"on":      true,
		"none":    map[string]any{"type": "String", "value": nil},
		"nested":  map[string]any{"a": "x", "b": map[string]any{"type": "Float", "value": 1.5}},
		"list":    []any{map[string]any{"p": 1.0}, map[string]any{"p": 2.0}},
	}
	m, err := spMetricFor("Temp", spDouble, 21.5, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.setProps(props); err != nil {
		t.Fatal(err)
	}
	b, _ := (&spPayload{Metrics: []*spMetric{m}}).encode()
	p, err := decodeSPPayload(b)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Metrics[0]
	_ = got.resolve(spDouble)
	if got.Value != 21.5 {
		t.Errorf("the value with properties on it: %v", got.Value)
	}
	want := map[string]any{
		"engUnit": map[string]any{"type": "String", "value": "degC"},
		"Quality": map[string]any{"type": "Int32", "value": 192.0},
		"high":    map[string]any{"type": "Double", "value": 99.5},
		"count":   map[string]any{"type": "Int32", "value": 7.0},
		"small":   map[string]any{"type": "UInt16", "value": 65535.0},
		"on":      map[string]any{"type": "Boolean", "value": true},
		"none":    map[string]any{"type": "String", "value": nil, "isNull": true},
		"nested": map[string]any{"type": "PropertySet", "value": map[string]any{
			"a": map[string]any{"type": "String", "value": "x"},
			"b": map[string]any{"type": "Float", "value": 1.5}}},
		"list": map[string]any{"type": "PropertySetList", "value": []any{
			map[string]any{"p": map[string]any{"type": "Int32", "value": 1.0}},
			map[string]any{"p": map[string]any{"type": "Int32", "value": 2.0}}}},
	}
	if !reflect.DeepEqual(got.Properties, want) {
		t.Errorf("properties came back as\n%#v\nwant\n%#v", got.Properties, want)
	}
	// What comes back can go out again unchanged.
	m2, _ := spMetricFor("Temp", spDouble, 21.5, true, 1)
	if err := m2.setProps(got.Properties); err != nil {
		t.Fatalf("sending back what was read: %v", err)
	}
	if string(m2.PropsBytes) != string(got.PropsBytes) {
		t.Errorf("a PropertySet read and sent again changed on the way")
	}

	for _, c := range []struct {
		want string
		in   any
	}{
		{"isn't a set of properties", "x"},
		{"type \"Bytes\" isn't one of", map[string]any{"k": map[string]any{"type": "Bytes", "value": "x"}}},
		{"property \"k\": 300 is outside what Int8 holds", map[string]any{"k": map[string]any{"type": "Int8", "value": 300.0}}},
		{"property \"k\": a PropertySetList is a list of objects", map[string]any{"k": map[string]any{"type": "PropertySetList", "value": "x"}}},
	} {
		_, err := spPropsBytes(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want it to say %q", c.in, err, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// The Eclipse schema reads what these write, and writes what these read
// ---------------------------------------------------------------------------

func TestSparkplugComplexValuesAreWhatTheEclipseSchemaSays(t *testing.T) {
	md := sparkplugSchema(t)

	add := func(name, typ string, v any, props map[string]any) *spMetric {
		m, err := spMetricFor(name, spTypeNames[typ], v, true, 1760800000999)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.setProps(props); err != nil {
			t.Fatal(err)
		}
		return m
	}
	ms := []*spMetric{
		add("ints", "Int32Array", []any{123456789.0, 987654321.0}, nil),
		add("flags", "BooleanArray", []any{true, false, true}, nil),
		add("names", "StringArray", []any{"a", "bb"}, nil),
		add("table", "DataSet", map[string]any{
			"columns": []any{"n", "v"}, "types": []any{"String", "Double"},
			"rows": []any{[]any{"x", 1.5}, []any{"y", -2.0}}}, nil),
		add("Motor", "Template", map[string]any{"isDefinition": true, "version": "2",
			"parameters": []any{map[string]any{"name": "Rated", "type": "Int32", "value": 1800.0}},
			"metrics":    []any{map[string]any{"name": "Speed", "type": "Double", "value": 0.0}}}, nil),
		add("M1", "Template", map[string]any{"templateRef": "Motor",
			"metrics": []any{map[string]any{"name": "Speed", "type": "Double", "value": 1200.5}}}, nil),
		add("Temp", "Double", 21.5, map[string]any{"engUnit": "degC", "Quality": map[string]any{"type": "Int32", "value": 192.0}}),
	}
	b, err := (&spPayload{Timestamp: 1760800000000, HasTimestamp: true, Metrics: ms, Seq: 0, HasSeq: true}).encode()
	if err != nil {
		t.Fatal(err)
	}
	got := schemaJSON(t, md, b)
	byName := map[string]map[string]any{}
	for _, e := range got["metrics"].([]any) {
		m := e.(map[string]any)
		byName[m["name"].(string)] = m
	}
	// Int32Array [123456789, 987654321] little endian: 15CD5B07 B168DE3A. The
	// specification's own example, in 6.4.2 of the Array Types.
	if byName["ints"]["datatype"] != 24.0 || byName["ints"]["bytesValue"] != "Fc1bB7Fo3jo=" {
		t.Errorf("Int32Array on the wire: %v", byName["ints"])
	}
	if byName["flags"]["datatype"] != 32.0 || byName["flags"]["bytesValue"] != "AwAAAKA=" {
		t.Errorf("BooleanArray on the wire: %v", byName["flags"])
	}
	if byName["names"]["datatype"] != 33.0 || byName["names"]["bytesValue"] != "YQBiYgA=" {
		t.Errorf("StringArray on the wire: %v", byName["names"])
	}
	ds, _ := byName["table"]["datasetValue"].(map[string]any)
	if byName["table"]["datatype"] != 16.0 || ds["numOfColumns"] != "2" || !reflect.DeepEqual(ds["columns"], []any{"n", "v"}) ||
		!reflect.DeepEqual(ds["types"], []any{12.0, 10.0}) {
		t.Errorf("DataSet header on the wire: %v", byName["table"])
	}
	rows := ds["rows"].([]any)
	r0 := rows[0].(map[string]any)["elements"].([]any)
	if len(rows) != 2 || r0[0].(map[string]any)["stringValue"] != "x" || r0[1].(map[string]any)["doubleValue"] != 1.5 {
		t.Errorf("DataSet rows on the wire: %v", rows)
	}
	def, _ := byName["Motor"]["templateValue"].(map[string]any)
	if byName["Motor"]["datatype"] != 19.0 || def["isDefinition"] != true || def["version"] != "2" || def["templateRef"] != nil {
		t.Errorf("Template definition on the wire: %v", byName["Motor"])
	}
	if p := def["parameters"].([]any)[0].(map[string]any); p["name"] != "Rated" || p["type"] != 3.0 || p["intValue"] != 1800.0 {
		t.Errorf("Template parameter on the wire: %v", p)
	}
	inst, _ := byName["M1"]["templateValue"].(map[string]any)
	if inst["templateRef"] != "Motor" || inst["isDefinition"] != false {
		t.Errorf("Template instance on the wire: %v", inst)
	}
	if sp := inst["metrics"].([]any)[0].(map[string]any); sp["name"] != "Speed" || sp["datatype"] != 10.0 || sp["doubleValue"] != 1200.5 {
		t.Errorf("Template member on the wire: %v", sp)
	}
	pr, _ := byName["Temp"]["properties"].(map[string]any)
	pv := pr["values"].([]any)
	if !reflect.DeepEqual(pr["keys"], []any{"Quality", "engUnit"}) ||
		pv[0].(map[string]any)["type"] != 3.0 || pv[0].(map[string]any)["intValue"] != 192.0 ||
		pv[1].(map[string]any)["type"] != 12.0 || pv[1].(map[string]any)["stringValue"] != "degC" {
		t.Errorf("PropertySet on the wire: %v", pr)
	}

	// And what the schema writes, this reads.
	in := schemaBytes(t, md, `{"timestamp":"1","seq":"2","metrics":[
		{"name":"tbl","datatype":16,"datasetValue":{"numOfColumns":"3","columns":["a","b","c"],"types":[3,12,11],
			"rows":[{"elements":[{"intValue":4294967295},{"stringValue":"z"},{"booleanValue":true}]},
			        {"elements":[{"intValue":5},{"stringValue":""},{"booleanValue":false}]}]}},
		{"name":"M","datatype":19,"templateValue":{"version":"v","isDefinition":false,"templateRef":"Motor",
			"metrics":[{"name":"S","datatype":9,"floatValue":2.5},{"name":"Arr","datatype":30,"bytesValue":"AAAAPwAAAEA="}],
			"parameters":[{"name":"P","type":12,"stringValue":"hello"},{"name":"Q","type":10,"doubleValue":0.25}]}},
		{"name":"Prop","datatype":10,"doubleValue":1,"properties":{"keys":["a","nest","lst","n"],"values":[
			{"type":9,"floatValue":1.5},
			{"type":20,"propertysetValue":{"keys":["i"],"values":[{"type":4,"longValue":"9007199254740993"}]}},
			{"type":21,"propertysetsValue":{"propertyset":[{"keys":["k"],"values":[{"type":11,"booleanValue":true}]}]}},
			{"type":12,"isNull":true}]}}
	]}`)
	p, err := decodeSPPayload(in)
	if err != nil {
		t.Fatal(err)
	}
	read := map[string]*spMetric{}
	for _, m := range p.Metrics {
		if err := m.resolve(m.DataType); err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		read[m.Name] = m
	}
	wantTbl := map[string]any{"columns": []any{"a", "b", "c"}, "types": []any{"Int32", "String", "Boolean"},
		"rows": []any{[]any{-1.0, "z", true}, []any{5.0, "", false}}}
	if !reflect.DeepEqual(read["tbl"].Value, wantTbl) {
		t.Errorf("a DataSet the schema wrote: %#v", read["tbl"].Value)
	}
	tpl := read["M"].Value.(map[string]any)
	if tpl["templateRef"] != "Motor" || tpl["version"] != "v" || tpl["isDefinition"] != false {
		t.Errorf("a Template the schema wrote: %#v", tpl)
	}
	mem := tpl["metrics"].([]any)
	if mem[0].(map[string]any)["value"] != 2.5 || !reflect.DeepEqual(mem[1].(map[string]any)["value"], []any{0.5, 2.0}) {
		t.Errorf("the members of a Template the schema wrote: %#v", mem)
	}
	if !reflect.DeepEqual(tpl["parameters"], []any{
		map[string]any{"name": "P", "type": "String", "value": "hello"}, map[string]any{"name": "Q", "type": "Double", "value": 0.25}}) {
		t.Errorf("the parameters of a Template the schema wrote: %#v", tpl["parameters"])
	}
	if !reflect.DeepEqual(read["Prop"].Properties, map[string]any{
		"a":    map[string]any{"type": "Float", "value": 1.5},
		"nest": map[string]any{"type": "PropertySet", "value": map[string]any{"i": map[string]any{"type": "Int64", "value": "9007199254740993"}}},
		"lst":  map[string]any{"type": "PropertySetList", "value": []any{map[string]any{"k": map[string]any{"type": "Boolean", "value": true}}}},
		"n":    map[string]any{"type": "String", "value": nil, "isNull": true},
	}) {
		t.Errorf("a PropertySet the schema wrote: %#v", read["Prop"].Properties)
	}
}

// ---------------------------------------------------------------------------
// Through a real broker, edge node to host
// ---------------------------------------------------------------------------

func TestSparkplugEdgeNodePublishesArraysDataSetsTemplatesAndPropertiesToAHost(t *testing.T) {
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/#")
	r.host("h", "host-"+r.group, "")
	r.edge("e", "line5", r.broker)
	host := r.start("sparkplug in", `{"server":"h"}`)

	motorDef := `{"isDefinition":true,"version":"1","parameters":[{"name":"Rated","type":"Int32","value":1800}],` +
		`"metrics":[{"name":"Speed","type":"Double","value":0}]}`
	motorInst := `{"templateRef":"Motor","parameters":[{"name":"Rated","type":"Int32","value":3600}],` +
		`"metrics":[{"name":"Speed","type":"Double","value":10.5}]}`
	table := `{"columns":["Part","Qty"],"types":["String","UInt16"],"rows":[["bolt",40],["nut",65535]]}`
	node1 := r.start("sparkplug out", mustJSON(t, map[string]any{"server": "e", "metrics": []any{
		map[string]any{"name": "Motor", "dataType": "Template", "initial": motorDef},
		map[string]any{"name": "Samples", "dataType": "DoubleArray", "initial": "[1.5, 2.5]"},
		map[string]any{"name": "Flags", "dataType": "BooleanArray", "initial": "[true,false]"},
		map[string]any{"name": "Table", "dataType": "DataSet", "initial": table},
		map[string]any{"name": "Level", "dataType": "Double", "initial": "3.5",
			"properties": `{"engUnit":"m","Quality":{"type":"Int32","value":192}}`},
	}}))
	dev := r.start("sparkplug out", mustJSON(t, map[string]any{"server": "e", "device": "m1", "metrics": []any{
		map[string]any{"name": "M1", "dataType": "Template", "initial": motorInst},
		map[string]any{"name": "Counts", "dataType": "UInt32Array", "initial": "[1,2,3]"},
	}}))

	r.eventually("the host to see both births", 20*time.Second, func() bool {
		return spFind(host.e, "NBIRTH", nil) != nil && spFind(host.e, "DBIRTH", nil) != nil
	})
	nb := spFind(host.e, "NBIRTH", nil)
	pl := nb.Payload().(map[string]any)
	if !reflect.DeepEqual(pl["Samples"], []any{1.5, 2.5}) || !reflect.DeepEqual(pl["Flags"], []any{true, false}) {
		t.Errorf("arrays in the NBIRTH: %#v", pl)
	}
	if !reflect.DeepEqual(pl["Table"], map[string]any{"columns": []any{"Part", "Qty"}, "types": []any{"String", "UInt16"},
		"rows": []any{[]any{"bolt", 40.0}, []any{"nut", 65535.0}}}) {
		t.Errorf("DataSet in the NBIRTH: %#v", pl["Table"])
	}
	if d, _ := pl["Motor"].(map[string]any); d["isDefinition"] != true || d["version"] != "1" {
		t.Errorf("Template definition in the NBIRTH: %#v", pl["Motor"])
	}
	var level map[string]any
	for _, e := range spField(nb, "metrics").([]any) {
		if m := e.(map[string]any); m["name"] == "Level" {
			level = m
		}
	}
	if !reflect.DeepEqual(level["properties"], map[string]any{
		"engUnit": map[string]any{"type": "String", "value": "m"},
		"Quality": map[string]any{"type": "Int32", "value": 192.0}}) {
		t.Errorf("properties in the NBIRTH: %#v", level)
	}
	db := spFind(host.e, "DBIRTH", nil)
	inst := db.Payload().(map[string]any)["M1"].(map[string]any)
	if inst["templateRef"] != "Motor" || inst["isDefinition"] != false ||
		inst["metrics"].([]any)[0].(map[string]any)["value"] != 10.5 {
		t.Errorf("Template instance in the DBIRTH: %#v", inst)
	}
	t.Logf("host read the NBIRTH: Samples %v, Flags %v, Table %v, Level properties %v", pl["Samples"], pl["Flags"], pl["Table"], level["properties"])
	t.Logf("host read the DBIRTH: M1 %v, Counts %v", inst, db.Payload().(map[string]any)["Counts"])

	// Data: a new array, a changed DataSet and a subset of an instance's members.
	if _, err := send(t, node1.n, msg(t, `{"payload":[{"name":"Samples","value":[9.5,8.5,7.5]},
		{"name":"Level","value":4.5,"properties":{"Quality":{"type":"Int32","value":500}}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := send(t, dev.n, msg(t, `{"payload":{"M1":{"templateRef":"Motor","metrics":[{"name":"Speed","type":"Double","value":99.25}]},"Counts":[4,5]}}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("NDATA and DDATA", 10*time.Second, func() bool {
		return spFind(host.e, "NDATA", nil) != nil && spFind(host.e, "DDATA", nil) != nil
	})
	nd := spFind(host.e, "NDATA", nil).Payload().(map[string]any)
	if !reflect.DeepEqual(nd["Samples"], []any{9.5, 8.5, 7.5}) || nd["Level"] != 4.5 {
		t.Errorf("NDATA = %#v", nd)
	}
	dd := spFind(host.e, "DDATA", nil).Payload().(map[string]any)
	if !reflect.DeepEqual(dd["Counts"], []any{4.0, 5.0}) ||
		dd["M1"].(map[string]any)["metrics"].([]any)[0].(map[string]any)["value"] != 99.25 {
		t.Errorf("DDATA = %#v", dd)
	}
	// The data message for a metric names no datatype on the wire, and the host still read the array.
	ndWire, _ := tap.find(onTopic("/NDATA/line5"))
	if !strings.Contains(string(ndWire.Payload), "Level") {
		t.Errorf("NDATA on the wire: %q", ndWire.Payload)
	}

	if os.Getenv("HOTLOOP_FLOW_SPARKPLUG_PROTO") != "" {
		schema := sparkplugSchema(t)
		nbWire, _ := tap.find(onTopic("/NBIRTH/line5"))
		j := schemaJSON(t, schema, nbWire.Payload)
		kinds := map[string]float64{}
		for _, e := range j["metrics"].([]any) {
			m := e.(map[string]any)
			kinds[m["name"].(string)], _ = m["datatype"].(float64)
		}
		if kinds["Samples"] != 31 || kinds["Flags"] != 32 || kinds["Table"] != 16 || kinds["Motor"] != 19 {
			t.Errorf("the Eclipse schema reads these datatypes off the NBIRTH: %v", kinds)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSparkplugTemplateInstanceNeedsItsDefinitionInTheNBirth(t *testing.T) {
	r := newSpRig(t)
	r.edge("e", "line6", r.broker)
	inst := `{"templateRef":"Motor","metrics":[{"name":"Speed","type":"Double","value":1}]}`
	r.start("sparkplug out", mustJSON(t, map[string]any{"server": "e", "metrics": []any{
		map[string]any{"name": "M1", "dataType": "Template", "initial": inst}}}))
	srv := r.svc.configs["e"].(*spServer)
	r.eventually("the birth to be refused", 15*time.Second, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return strings.Contains(srv.lastErr, "has no definition by that name")
	})

	// A definition is only for the NBIRTH.
	r2 := newSpRig(t)
	r2.edge("e", "line7", r2.broker)
	def := `{"isDefinition":true,"metrics":[{"name":"Speed","type":"Double"}]}`
	r2.start("sparkplug out", mustJSON(t, map[string]any{"server": "e", "metrics": []any{
		map[string]any{"name": "Level", "dataType": "Double", "initial": "1"}}}))
	r2.start("sparkplug out", mustJSON(t, map[string]any{"server": "e", "device": "d1", "metrics": []any{
		map[string]any{"name": "Motor", "dataType": "Template", "initial": def}}}))
	srv2 := r2.svc.configs["e"].(*spServer)
	r2.eventually("the birth to be refused", 15*time.Second, func() bool {
		srv2.mu.Lock()
		defer srv2.mu.Unlock()
		return strings.Contains(srv2.lastErr, "only allowed in the edge node's own birth")
	})
}

// ---------------------------------------------------------------------------
// The primary host application, and a second broker
// ---------------------------------------------------------------------------

func stateJSON(online bool, ts int64) []byte {
	b, _ := json.Marshal(map[string]any{"online": online, "timestamp": ts})
	return b
}

func countTopic(tap *spTap, suffix string) int {
	n := 0
	for _, m := range tap.all() {
		if strings.HasSuffix(m.Topic, suffix) {
			n++
		}
	}
	return n
}

func bdSeqOf(t *testing.T, b []byte) float64 {
	t.Helper()
	p, err := decodeSPPayload(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range p.Metrics {
		if m.Name == spBdSeqName {
			_ = m.resolve(spInt64)
			return m.Value.(float64)
		}
	}
	t.Fatalf("no bdSeq in %x", b)
	return -1
}

// clearState takes a retained STATE off a broker when the test is over, so the
// next one starts with none.
func (p *spTap) clearState(t *testing.T, host string) {
	t.Cleanup(func() { p.c.Publish("spBv1.0/STATE/"+host, 1, true, []byte{}).WaitTimeout(2 * time.Second) })
}

func TestSparkplugEdgeNodeWaitsForItsPrimaryHostBeforeItIsBorn(t *testing.T) {
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/#")
	hostID := "primary-" + r.group
	tap.clearState(t, hostID)
	r.config("e", `"role":"edge","groupId":"`+r.group+`","edgeNodeId":"line3","primaryHost":"`+hostID+`"`, r.broker)
	out := r.start("sparkplug out", `{"server":"e","metrics":[{"name":"Level","dataType":"Double","initial":"1"}]}`)

	// Connected, subscribed, and quiet: no host has said it is online.
	time.Sleep(2500 * time.Millisecond)
	if n := countTopic(tap, "/NBIRTH/line3"); n != 0 {
		t.Fatalf("%d NBIRTH went out with no primary host online", n)
	}
	srv := r.svc.configs["e"].(*spServer)
	if srv.isReady() {
		t.Error("the edge node says it is born")
	}
	if _, err := send(t, out.n, msg(t, `{"payload":{"Level":2}}`)); err == nil || !strings.Contains(err.Error(), "waiting for primary host "+hostID) {
		t.Errorf("data before the birth should say what it is waiting for: %v", err)
	}
	t.Logf("2.5 s with no primary host: NBIRTH count %d, publish says: waiting for primary host", countTopic(tap, "/NBIRTH/line3"))

	// An offline STATE is not the host being up.
	tap.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(false, 1000))
	time.Sleep(1200 * time.Millisecond)
	if n := countTopic(tap, "/NBIRTH/line3"); n != 0 {
		t.Fatalf("%d NBIRTH went out while the host was offline", n)
	}

	// The host comes up: now it is born.
	tap.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(true, 2000))
	nb := tap.await(t, "the NBIRTH once the host is online", 15*time.Second, onTopic("/NBIRTH/line3"))
	if !srv.isReady() {
		r.eventually("ready", 5*time.Second, srv.isReady)
	}
	t.Logf("host online, NBIRTH followed on %s", nb.Topic)
}

func TestSparkplugEdgeNodeLeavesWhenItsPrimaryHostGoesOfflineAndIgnoresALateDeath(t *testing.T) {
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/#")
	hostID := "primary-" + r.group
	tap.clearState(t, hostID)
	tap.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(true, 5000))
	r.config("e", `"role":"edge","groupId":"`+r.group+`","edgeNodeId":"line4","primaryHost":"`+hostID+`"`, r.broker)
	r.start("sparkplug out", `{"server":"e","metrics":[{"name":"Level","dataType":"Double","initial":"1"}]}`)
	first := tap.await(t, "the NBIRTH", 15*time.Second, onTopic("/NBIRTH/line4"))
	bd0 := bdSeqOf(t, first.Payload)

	// A death certificate older than the last online STATE is some old session's.
	tap.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(false, 4000))
	time.Sleep(1500 * time.Millisecond)
	if n := countTopic(tap, "/NDEATH/line4"); n != 0 {
		t.Fatalf("an offline STATE older than the online one made the edge node leave (%d NDEATH)", n)
	}

	// A current one is the host going away: NDEATH, and the edge node starts over.
	tap.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(false, 6000))
	nd := tap.await(t, "the NDEATH", 10*time.Second, onTopic("/NDEATH/line4"))
	if bd := bdSeqOf(t, nd.Payload); bd != bd0 {
		t.Errorf("the NDEATH carries bdSeq %v, the birth had %v", bd, bd0)
	}
	if nd.QoS != 1 || nd.Retained {
		t.Errorf("NDEATH QoS %d retained %v, the specification says QoS 1 and not retained", nd.QoS, nd.Retained)
	}
	time.Sleep(3500 * time.Millisecond)
	if n := countTopic(tap, "/NBIRTH/line4"); n != 1 {
		t.Fatalf("%d NBIRTH after the host went offline, want it to wait for the host (1 so far)", n)
	}

	// The host is back, with a newer timestamp: born again, with the next bdSeq.
	tap.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(true, 7000))
	r.eventually("a second NBIRTH", 20*time.Second, func() bool { return countTopic(tap, "/NBIRTH/line4") == 2 })
	var births []tapMsg
	for _, m := range tap.all() {
		if strings.HasSuffix(m.Topic, "/NBIRTH/line4") {
			births = append(births, m)
		}
	}
	if bd1 := bdSeqOf(t, births[1].Payload); bd1 != bd0+1 {
		t.Errorf("the second birth has bdSeq %v, want %v", bd1, bd0+1)
	}
	if n := countTopic(tap, "/NDEATH/line4"); n != 1 {
		t.Errorf("%d NDEATH in all, want exactly the one", n)
	}
}

func TestSparkplugEdgeNodeMovesToTheBrokerWhereThePrimaryHostIs(t *testing.T) {
	brokerB := requireEnv(t, "HOTLOOP_FLOW_TEST_MQTT2")
	old := spStateWait
	spStateWait = 1200 * time.Millisecond
	t.Cleanup(func() { spStateWait = old })

	r := newSpRig(t)
	tapA := newTap(t, r.broker, "spBv1.0/#")
	tapB := newTap(t, brokerB, "spBv1.0/#")
	hostID := "primary-" + r.group
	tapA.clearState(t, hostID)
	tapB.clearState(t, hostID)
	hostB, portB := splitHostPort(t, brokerB)
	r.config("e", `"role":"edge","groupId":"`+r.group+`","edgeNodeId":"line8","primaryHost":"`+hostID+
		`","moreBrokers":[{"host":"`+hostB+`","port":`+portB+`}]`, r.broker)
	r.start("sparkplug out", `{"server":"e","metrics":[{"name":"Level","dataType":"Double","initial":"1"}]}`)

	// The host is online on B only. Broker A has never heard of it.
	tapB.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(true, 100))
	tapB.await(t, "the NBIRTH on broker B", 30*time.Second, onTopic("/NBIRTH/line8"))
	if n := countTopic(tapA, "/NBIRTH/line8"); n != 0 {
		t.Errorf("%d NBIRTH on broker A, where the primary host never was", n)
	}
	t.Logf("host only on B: NBIRTH on B %d, on A %d", countTopic(tapB, "/NBIRTH/line8"), countTopic(tapA, "/NBIRTH/line8"))

	// The host loses B and is online on A: the edge node follows it.
	tapA.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(true, 200))
	tapB.publish(t, "spBv1.0/STATE/"+hostID, 1, true, stateJSON(false, 300))
	tapB.await(t, "the NDEATH on broker B", 10*time.Second, onTopic("/NDEATH/line8"))
	tapA.await(t, "the NBIRTH on broker A", 30*time.Second, onTopic("/NBIRTH/line8"))
	if n := countTopic(tapB, "/NBIRTH/line8"); n != 1 {
		t.Errorf("%d NBIRTH on B in all", n)
	}
	t.Logf("host moved to A: NDEATH on B %d, NBIRTH on A %d", countTopic(tapB, "/NDEATH/line8"), countTopic(tapA, "/NBIRTH/line8"))
}

func TestSparkplugEdgeNodeGoesToTheNextBrokerWhenTheFirstIsGone(t *testing.T) {
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/#")
	host, port := splitHostPort(t, r.broker)
	// Nothing listens on port 1.
	n := build(t, "hotloop-flow-sparkplug", `{"broker":"127.0.0.1","port":1,"role":"edge","groupId":"`+r.group+
		`","edgeNodeId":"line10","moreBrokers":[{"host":"`+host+`","port":`+port+`}]}`, r.svc)
	r.svc.configs["e"] = n
	r.start("sparkplug out", `{"server":"e","metrics":[{"name":"Level","dataType":"Double","initial":"1"}]}`)
	tap.await(t, "the NBIRTH on the second broker", 30*time.Second, onTopic("/NBIRTH/line10"))
	if got := n.(*spServer).endpoint(); got != r.broker && got != host+":"+port {
		t.Errorf("the edge node is on %s", got)
	}
	t.Logf("first broker refused, born on %s", n.(*spServer).endpoint())
}

func TestSparkplugBrokerAndPrimaryHostSettingsAreChecked(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ typ, cfg, want string }{
		{"hotloop-flow-sparkplug", `{"role":"host","hostId":"h","broker":"b","moreBrokers":[{"host":"c","port":1883}]}`, "one broker"},
		{"hotloop-flow-sparkplug", `{"role":"host","hostId":"h","broker":"b","primaryHost":"p"}`, "one broker"},
		{"hotloop-flow-sparkplug", `{"role":"edge","groupId":"g","edgeNodeId":"e","broker":"b","primaryHost":"a/b"}`, "primary host application ID"},
		{"hotloop-flow-sparkplug", `{"role":"edge","groupId":"g","edgeNodeId":"e","broker":"b","port":1883,"moreBrokers":[{"host":"b","port":1883}]}`, "listed twice"},
		{"hotloop-flow-sparkplug", `{"role":"edge","groupId":"g","edgeNodeId":"e","broker":"b","moreBrokers":[{"host":"c","port":70000}]}`, "out of range"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"A","dataType":"Int32Array","initial":"[1,2"}]}`, "isn't JSON"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"A","dataType":"Int8Array","initial":"[1,500]"}]}`, "item 1 of the list"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"A","dataType":"DataSet","initial":"{\"columns\":[\"a\"]}"}]}`, "one datatype for each"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"A","dataType":"Double","properties":"[1]"}]}`, "aren't a JSON object"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"A","dataType":"Double","properties":"{\"k\":{\"type\":\"Nope\",\"value\":1}}"}]}`, "type \"Nope\" isn't one of"},
	} {
		err := buildErr(t, c.typ, c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: error = %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
	_ = engine.GenerateID
}
