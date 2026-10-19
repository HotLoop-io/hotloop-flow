package nodes

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// ---------------------------------------------------------------------------
// The payload, with no broker
// ---------------------------------------------------------------------------

func TestSparkplugPayloadRoundTripsEveryDatatypeItCanPublish(t *testing.T) {
	cases := []struct {
		typ  string
		in   any
		want any
	}{
		{"Int8", -5.0, -5.0}, {"Int8", 127.0, 127.0},
		{"Int16", -32768.0, -32768.0}, {"Int32", -2147483648.0, -2147483648.0},
		{"Int64", -9007199254740992.0, -9007199254740992.0}, {"Int64", "-9223372036854775808", "-9223372036854775808"},
		{"UInt8", 255.0, 255.0}, {"UInt16", 65535.0, 65535.0}, {"UInt32", 4294967295.0, 4294967295.0},
		{"UInt64", 9007199254740992.0, 9007199254740992.0}, {"UInt64", "18446744073709551615", "18446744073709551615"},
		{"Float", 1.5, 1.5}, {"Double", -12.375, -12.375}, {"Boolean", true, true}, {"Boolean", false, false},
		{"String", "Kessel 3 läuft", "Kessel 3 läuft"}, {"Text", "t", "t"},
		{"UUID", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "6ba7b810-9dad-11d1-80b4-00c04fd430c8"},
		{"DateTime", 1760800000000.0, 1760800000000.0}, {"Bytes", []byte{0, 1, 2, 255}, []byte{0, 1, 2, 255}},
	}
	for _, birth := range []bool{true, false} {
		for _, c := range cases {
			dt := spTypeNames[c.typ]
			m, err := spMetricFor("M", dt, c.in, birth, 1760800000123)
			if err != nil {
				t.Errorf("%s %v: %v", c.typ, c.in, err)
				continue
			}
			b, err := (&spPayload{Timestamp: 5, HasTimestamp: true, Metrics: []*spMetric{m}, Seq: 7, HasSeq: true}).encode()
			if err != nil {
				t.Fatal(err)
			}
			p, err := decodeSPPayload(b)
			if err != nil {
				t.Fatalf("%s: %v", c.typ, err)
			}
			if p.Seq != 7 || p.Timestamp != 5 || len(p.Metrics) != 1 {
				t.Fatalf("%s: payload came back as %+v", c.typ, p)
			}
			got := p.Metrics[0]
			// In data the datatype stays off the wire, and the host has it from the birth.
			if birth != got.HasDataType && !(dt == spInt64 || dt == spUInt64 || dt == spDateTime) {
				t.Errorf("%s birth=%v: datatype on the wire = %v", c.typ, birth, got.HasDataType)
			}
			if err := got.resolve(dt); err != nil {
				t.Errorf("%s: %v", c.typ, err)
			}
			if !reflect.DeepEqual(got.Value, c.want) {
				t.Errorf("%s birth=%v: %#v came back as %#v, want %#v", c.typ, birth, c.in, got.Value, c.want)
			}
		}
	}
}

func TestSparkplugNullIsNullAndNotZero(t *testing.T) {
	m, _ := spMetricFor("M", spDouble, nil, true, 0)
	b, _ := (&spPayload{Metrics: []*spMetric{m}}).encode()
	p, _ := decodeSPPayload(b)
	got := p.Metrics[0]
	_ = got.resolve(spDouble)
	if !got.IsNull || got.Value != nil || got.raw.kind != 0 {
		t.Errorf("a null should say is_null and carry no value: %+v", got)
	}
}

func TestSparkplugRefusesAValueThatDoesNotFit(t *testing.T) {
	for _, c := range []struct {
		typ  string
		in   any
		want string
	}{
		{"Int8", 200.0, "outside what Int8 holds"}, {"UInt16", -1.0, "outside what UInt16 holds"},
		{"Int32", 1.5, "whole number"}, {"Double", "hot", "isn't a number"}, {"Boolean", 3.0, "on or off"},
		{"Float", 1e300, "too big"}, {"UInt64", 1e20, "Past 2^53"}, {"Bytes", 4.0, "isn't bytes"},
		{"DateTime", "yesterday", "isn't a date"},
	} {
		_, err := spMetricFor("M", spTypeNames[c.typ], c.in, false, 0)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: error = %v, want %q", c.typ, c.in, err, c.want)
		}
	}
}

func TestSparkplugSmallIntegersTravelTheWayEveryoneElseSendsThem(t *testing.T) {
	// A negative Int8 is a sign extended 32 bit value in the uint32 field, which is
	// what Eclipse's own Java library writes. Anything that reads it back by
	// truncating to the datatype gets the number, and a reader that doesn't
	// truncate gets a value out of range, so the sign extension is not optional.
	m, _ := spMetricFor("M", spInt8, -5.0, true, 0)
	if m.raw.u != 0xFFFFFFFB {
		t.Errorf("Int8 -5 = %#x", m.raw.u)
	}
	// And a reader gets the number back out of a 32 bit field that was not extended.
	r := &spMetric{raw: spRaw{kind: 'u', u: 0xFB}}
	_ = r.resolve(spInt8)
	if r.Value != -5.0 {
		t.Errorf("Int8 0xFB read as %v", r.Value)
	}
}

func spBoolArrayBytes(bits ...bool) []byte {
	b := make([]byte, 4, 4+(len(bits)+7)/8)
	binary.LittleEndian.PutUint32(b, uint32(len(bits)))
	cur, n := byte(0), 0
	for _, v := range bits {
		if v {
			cur |= 1 << (7 - uint(n))
		}
		if n++; n == 8 {
			b, cur, n = append(b, cur), 0, 0
		}
	}
	if n > 0 {
		b = append(b, cur)
	}
	return b
}

func TestSparkplugArraysAreDecodedFromTheirPackedForm(t *testing.T) {
	le := binary.LittleEndian
	i32 := make([]byte, 8)
	le.PutUint32(i32, uint32(0xFFFFFFFE)) // -2
	le.PutUint32(i32[4:], 7)
	f64 := make([]byte, 16)
	le.PutUint64(f64, math.Float64bits(1.25))
	le.PutUint64(f64[8:], math.Float64bits(-2.5))
	for _, c := range []struct {
		dt   uint32
		in   []byte
		want []any
	}{
		{spInt32Array, i32, []any{-2.0, 7.0}},
		{spDoubleArray, f64, []any{1.25, -2.5}},
		{spUInt8Array, []byte{1, 2, 255}, []any{1.0, 2.0, 255.0}},
		{spBooleanArray, spBoolArrayBytes(true, false, true, true, false, false, false, false, true), []any{true, false, true, true, false, false, false, false, true}},
		{spStringArray, []byte("a\x00bb\x00"), []any{"a", "bb"}},
	} {
		got, err := spDecodeArray(c.dt, c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %#v, %v; want %#v", spTypeName(c.dt), got, err, c.want)
		}
	}
	if _, err := spDecodeArray(spInt32Array, []byte{1, 2, 3}); err == nil || !strings.Contains(err.Error(), "don't divide") {
		t.Errorf("a ragged array: %v", err)
	}
}

func TestSparkplugADataSetOrTemplateThatIsntThereSaysSoInsteadOfInventing(t *testing.T) {
	// A metric whose datatype is DataSet and whose value field is something else.
	metric := []byte{0x0a, 0x01, 'D', 0x20, spDataSet, 0x72, 0x01, 'x'}
	pb := append([]byte{0x12, byte(len(metric))}, metric...)
	p, err := decodeSPPayload(pb)
	if err != nil {
		t.Fatal(err)
	}
	m := p.Metrics[0]
	if err := m.resolve(m.DataType); err == nil || !strings.Contains(err.Error(), "DataSet and carries none") {
		t.Errorf("%+v, %v", m, err)
	}
}

func TestSparkplugBadPayloadsAreRefusedWithAReason(t *testing.T) {
	for _, b := range [][]byte{{0x12}, {0x12, 0x05, 0x01}, {0xff, 0xff, 0xff}} {
		if _, err := decodeSPPayload(b); err == nil || !strings.Contains(err.Error(), "not a Sparkplug payload") {
			t.Errorf("%x: %v", b, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Configuration, and a flow test
// ---------------------------------------------------------------------------

func TestSparkplugConfigRefusesWhatCannotWork(t *testing.T) {
	svc := newTestServices()
	for _, c := range []struct{ typ, cfg, want string }{
		{"hotloop-flow-sparkplug", `{}`, "broker host is required"},
		{"hotloop-flow-sparkplug", `{"broker":"b","role":"both"}`, "not edge or host"},
		{"hotloop-flow-sparkplug", `{"broker":"b","role":"edge","edgeNodeId":"e"}`, "group ID is required"},
		{"hotloop-flow-sparkplug", `{"broker":"b","role":"edge","groupId":"g"}`, "edge node ID is required"},
		{"hotloop-flow-sparkplug", `{"broker":"b","role":"edge","groupId":"g/h","edgeNodeId":"e"}`, "can't contain +, / or #"},
		{"hotloop-flow-sparkplug", `{"broker":"b","role":"host"}`, "host application ID is required"},
		{"hotloop-flow-sparkplug", `{"broker":"b","role":"host","hostId":"h","groupId":"a#"}`, "can't contain"},
		{"hotloop-flow-sparkplug", `{"broker":"b","groupId":"g","edgeNodeId":"e","port":0}`, "out of range"},
		{"sparkplug out", `{}`, "no edge node selected"},
		{"sparkplug out", `{"server":"s"}`, "declare at least one metric"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"bdSeq","dataType":"Int64"}]}`, "belongs to Sparkplug itself"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"A","dataType":"Complex"}]}`, "not one of the types"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"A","dataType":"Double"},{"name":"A","dataType":"Double"}]}`, "declared twice"},
		{"sparkplug out", `{"server":"s","metrics":[{"name":"A","dataType":"UInt8","initial":"300"}]}`, "outside what UInt8 holds"},
		{"sparkplug out", `{"server":"s","device":"a/b","metrics":[{"name":"A","dataType":"Double"}]}`, "can't contain"},
		{"sparkplug in", `{}`, "no connection selected"},
		{"sparkplug command", `{"server":"s"}`, "is required"},
		{"sparkplug command", `{"server":"s","group":"g","edgeNode":"e/x"}`, "can't contain"},
	} {
		err := buildErr(t, c.typ, c.cfg, svc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: error = %v, want %q", c.typ, c.cfg, err, c.want)
		}
	}
}

func TestSparkplugOutAndCommandUnderAFlowTest(t *testing.T) {
	svc := newTestServices()
	si := &scriptedStandIn{}
	svc.standIn = si
	out := build(t, "sparkplug out", `{"server":"s","device":"press1","metrics":[
		{"name":"Temperature","dataType":"Double"},{"name":"Count","dataType":"Int32"}]}`, svc)

	if _, err := send(t, out, msg(t, `{"payload":{"Temperature":21.5,"Count":8}}`)); err != nil {
		t.Fatal(err)
	}
	sent := si.calls[0]["sent"].(map[string]any)
	if sent["op"] != "data" || sent["device"] != "press1" {
		t.Errorf("sent = %#v", sent)
	}
	ms := sent["metrics"].([]any)
	if len(ms) != 2 || ms[0].(map[string]any)["name"] != "Count" || ms[1].(map[string]any)["type"] != "Double" {
		t.Errorf("metrics = %#v", ms)
	}

	for _, c := range []struct{ body, want string }{
		{`{"payload":{"Nope":1}}`, `isn't declared on this node, which declares Temperature, Count`},
		{`{"payload":{"Count":2.5}}`, "whole number"},
		{`{"payload":{"Count":"many"}}`, "isn't a number"},
		{`{"payload":7}`, "has to be an object of metric names"},
		{`{"payload":{}}`, "names no metrics"},
		{`{"payload":[{"value":1}]}`, "has no name"},
	} {
		_, err := send(t, out, msg(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want %q", c.body, err, c.want)
		}
	}

	if _, err := send(t, out, msg(t, `{"payload":1,"sparkplug":{"death":true}}`)); err != nil {
		t.Errorf("a device death: %v", err)
	}
	if last := si.calls[len(si.calls)-1]["sent"].(map[string]any); last["op"] != "ddeath" {
		t.Errorf("the death was %#v", last)
	}

	cmd := build(t, "sparkplug command", `{"server":"s","group":"G","edgeNode":"E","device":"press1"}`, svc)
	if _, err := send(t, cmd, msg(t, `{"payload":{"Running":true,"SP":12.5}}`)); err != nil {
		t.Fatal(err)
	}
	cs := si.calls[len(si.calls)-1]["sent"].(map[string]any)
	if cs["op"] != "command" || cs["group"] != "G" || cs["device"] != "press1" {
		t.Errorf("command = %#v", cs)
	}
	if _, err := send(t, cmd, msg(t, `{"payload":[{"name":"X","value":1,"type":"Wrong"}]}`)); err == nil ||
		!strings.Contains(err.Error(), "not one of the types") {
		t.Errorf("a bad command type: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The wire, checked by an implementation that is not this one
// ---------------------------------------------------------------------------

// The schema is Eclipse Tahu's sparkplug_b.proto, compiled here by protocompile
// and read and written by Google's protobuf. The file is not part of this
// repository (it's EPL-2.0 and this isn't), so it's fetched, pinned to a
// commit and a hash, by CI and by hand:
//
//	HOTLOOP_FLOW_SPARKPLUG_PROTO=/path/to/sparkplug_b.proto go test ./internal/nodes/ -run Sparkplug

func sparkplugSchema(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	path := requireEnv(t, "HOTLOOP_FLOW_SPARKPLUG_PROTO")
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		ImportPaths: []string{filepath.Dir(path)},
	})}
	files, err := c.Compile(context.Background(), filepath.Base(path))
	if err != nil {
		t.Fatalf("compiling %s: %v", path, err)
	}
	md := files[0].Messages().ByName("Payload")
	if md == nil {
		t.Fatal("no Payload message in the schema")
	}
	return md
}

// schemaJSON reads bytes the way the schema reads them, as JSON.
func schemaJSON(t *testing.T, md protoreflect.MessageDescriptor, b []byte) map[string]any {
	t.Helper()
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(b, m); err != nil {
		t.Fatalf("the schema can't read what was published: %v", err)
	}
	j, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(j, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// schemaBytes writes a payload from JSON the way the schema writes it.
func schemaBytes(t *testing.T, md protoreflect.MessageDescriptor, js string) []byte {
	t.Helper()
	m := dynamicpb.NewMessage(md)
	if err := protojson.Unmarshal([]byte(js), m); err != nil {
		t.Fatalf("%s: %v", js, err)
	}
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSparkplugBytesAreWhatTheEclipseSchemaSays(t *testing.T) {
	md := sparkplugSchema(t)

	var ms []*spMetric
	add := func(name string, typ string, v any, birth bool) {
		m, err := spMetricFor(name, spTypeNames[typ], v, birth, 1760800000999)
		if err != nil {
			t.Fatal(err)
		}
		ms = append(ms, m)
	}
	add("bdSeq", "Int64", 3.0, true)
	add("Node Control/Rebirth", "Boolean", false, true)
	add("i8", "Int8", -5.0, true)
	add("u32", "UInt32", 4294967295.0, true)
	add("u64", "UInt64", "18446744073709551615", true)
	add("f", "Float", 1.5, true)
	add("d", "Double", -2.25, true)
	add("s", "String", "größer", true)
	add("by", "Bytes", []byte{9, 8, 7}, true)
	add("dt", "DateTime", 1760800000000.0, true)
	add("nul", "Double", nil, true)
	b, err := (&spPayload{Timestamp: 1760800000000, HasTimestamp: true, Metrics: ms, Seq: 0, HasSeq: true}).encode()
	if err != nil {
		t.Fatal(err)
	}
	got := schemaJSON(t, md, b)
	if got["timestamp"] != "1760800000000" || got["seq"] != "0" {
		t.Errorf("payload fields: %v", got)
	}
	want := map[string]map[string]any{
		"bdSeq":                {"datatype": 4.0, "longValue": "3"},
		"Node Control/Rebirth": {"datatype": 11.0, "booleanValue": false},
		"i8":                   {"datatype": 1.0, "intValue": 4294967291.0},
		"u32":                  {"datatype": 7.0, "intValue": 4294967295.0},
		"u64":                  {"datatype": 8.0, "longValue": "18446744073709551615"},
		"f":                    {"datatype": 9.0, "floatValue": 1.5},
		"d":                    {"datatype": 10.0, "doubleValue": -2.25},
		"s":                    {"datatype": 12.0, "stringValue": "größer"},
		"by":                   {"datatype": 17.0, "bytesValue": "CQgH"},
		"dt":                   {"datatype": 13.0, "longValue": "1760800000000"},
		"nul":                  {"datatype": 10.0, "isNull": true},
	}
	mets, _ := got["metrics"].([]any)
	if len(mets) != len(want) {
		t.Fatalf("%d metrics came out, want %d: %v", len(mets), len(want), got)
	}
	for _, e := range mets {
		m := e.(map[string]any)
		name := m["name"].(string)
		w := want[name]
		for k, v := range w {
			if !reflect.DeepEqual(m[k], v) {
				t.Errorf("%s: %s = %#v, the schema reads %#v", name, k, w[k], m[k])
			}
		}
		if m["timestamp"] != "1760800000999" {
			t.Errorf("%s: timestamp = %v", name, m["timestamp"])
		}
	}

	// Data leaves the datatype off, as the specification says it should.
	dm, _ := spMetricFor("d", spDouble, 3.5, false, 0)
	db, _ := (&spPayload{Timestamp: 1, HasTimestamp: true, Metrics: []*spMetric{dm}, Seq: 4, HasSeq: true}).encode()
	dj := schemaJSON(t, md, db)
	first := dj["metrics"].([]any)[0].(map[string]any)
	if _, has := first["datatype"]; has || first["doubleValue"] != 3.5 {
		t.Errorf("a data metric should carry a name and a value and no datatype: %v", first)
	}

	// And the other way: what the schema writes, this reads.
	in := schemaBytes(t, md, `{"timestamp":"1760800000000","seq":"9","metrics":[
		{"name":"Temp","alias":"17","datatype":10,"timestamp":"1760800000001","doubleValue":21.5},
		{"alias":"18","datatype":11,"booleanValue":true},
		{"name":"Neg","datatype":3,"intValue":4294967295},
		{"name":"Big","datatype":4,"longValue":"9223372036854775807"},
		{"name":"Hist","datatype":10,"isHistorical":true,"isTransient":true,"doubleValue":1},
		{"name":"DS","datatype":16,"datasetValue":{"numOfColumns":"1","columns":["a"],"types":[10],"rows":[]}},
		{"name":"Props","datatype":10,"doubleValue":2,"properties":{"keys":["Quality"],"values":[{"type":3,"intValue":192}]}}
	]}`)
	p, err := decodeSPPayload(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 9 || len(p.Metrics) != 7 {
		t.Fatalf("%+v", p)
	}
	byName := map[string]*spMetric{}
	for _, m := range p.Metrics {
		_ = m.resolve(m.DataType)
		byName[m.Name] = m
	}
	if m := byName["Temp"]; m.Alias != 17 || m.Value != 21.5 || m.Timestamp != 1760800000001 {
		t.Errorf("Temp: %+v", m)
	}
	if m := p.Metrics[1]; m.Name != "" || m.Alias != 18 || m.Value != true {
		t.Errorf("an alias-only metric: %+v", m)
	}
	if byName["Neg"].Value != -1.0 {
		t.Errorf("Int32 0xFFFFFFFF = %v", byName["Neg"].Value)
	}
	if byName["Big"].Value != "9223372036854775807" {
		t.Errorf("Int64 max = %v", byName["Big"].Value)
	}
	if m := byName["Hist"]; !m.IsHistorical || !m.IsTransient {
		t.Errorf("flags: %+v", m)
	}
	if m := byName["DS"]; m.Unsupported != "" || !reflect.DeepEqual(m.Value, map[string]any{
		"columns": []any{"a"}, "types": []any{"Double"}, "rows": []any{}}) {
		t.Errorf("a DataSet: %+v", m)
	}
	if m := byName["Props"]; m.Value != 2.0 || !reflect.DeepEqual(m.Properties, map[string]any{
		"Quality": map[string]any{"type": "Int32", "value": 192.0}}) {
		t.Errorf("a metric's property set is read with its value: %+v", m)
	}
}

// ---------------------------------------------------------------------------
// Against a real broker
// ---------------------------------------------------------------------------

type tapMsg struct {
	Topic    string
	QoS      byte
	Retained bool
	Payload  []byte
}

// spTap is an MQTT client that only listens, to see what is on the wire
// whatever the nodes think they sent.
type spTap struct {
	c    mqtt.Client
	mu   sync.Mutex
	msgs []tapMsg
}

func newTap(t *testing.T, broker string, filters ...string) *spTap {
	t.Helper()
	tap := &spTap{}
	o := mqtt.NewClientOptions().AddBroker("tcp://" + broker).SetClientID("flow-sp-tap-" + engine.GenerateID()).SetCleanSession(true)
	tap.c = mqtt.NewClient(o)
	if tok := tap.c.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("tap can't connect to %s: %v", broker, tok.Error())
	}
	for _, f := range filters {
		tok := tap.c.Subscribe(f, 1, func(_ mqtt.Client, m mqtt.Message) {
			tap.mu.Lock()
			tap.msgs = append(tap.msgs, tapMsg{m.Topic(), m.Qos(), m.Retained(), append([]byte(nil), m.Payload()...)})
			tap.mu.Unlock()
		})
		if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
			t.Fatalf("tap can't subscribe to %s: %v", f, tok.Error())
		}
	}
	t.Cleanup(func() { tap.c.Disconnect(100) })
	return tap
}

func (p *spTap) all() []tapMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tapMsg(nil), p.msgs...)
}

func (p *spTap) find(pred func(tapMsg) bool) (tapMsg, bool) {
	for _, m := range p.all() {
		if pred(m) {
			return m, true
		}
	}
	return tapMsg{}, false
}

func (p *spTap) await(t *testing.T, what string, within time.Duration, pred func(tapMsg) bool) tapMsg {
	t.Helper()
	var got tapMsg
	waitUntilTrue(t, what, within, func() bool {
		m, ok := p.find(pred)
		got = m
		return ok
	})
	return got
}

func (p *spTap) publish(t *testing.T, topic string, qos byte, retain bool, b []byte) {
	t.Helper()
	tok := p.c.Publish(topic, qos, retain, b)
	if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("publishing %s: %v", topic, tok.Error())
	}
}

func onTopic(suffix string) func(tapMsg) bool {
	return func(m tapMsg) bool { return strings.HasSuffix(m.Topic, suffix) }
}

type spRig struct {
	t      *testing.T
	svc    *credServices
	broker string
	group  string
}

func newSpRig(t *testing.T) *spRig {
	addr := requireEnv(t, "HOTLOOP_FLOW_TEST_MQTT")
	return &spRig{t: t, svc: &credServices{testServices: newTestServices(), configs: map[string]node.Node{}},
		broker: addr, group: "g" + strings.ToLower(engine.GenerateID()[:8])}
}

func (r *spRig) config(id, body string, broker string) *spServer {
	r.t.Helper()
	host, port := splitHostPort(r.t, broker)
	n := build(r.t, "hotloop-flow-sparkplug", `{"broker":"`+host+`","port":`+port+`,`+body+`}`, r.svc)
	r.svc.configs[id] = n
	r.t.Cleanup(func() { _ = n.(node.Closer).Close(context.Background(), false) })
	return n.(*spServer)
}

func (r *spRig) edge(id, edgeNode string, broker string) *spServer {
	return r.config(id, `"role":"edge","groupId":"`+r.group+`","edgeNodeId":"`+edgeNode+`"`, broker)
}

func (r *spRig) host(id, hostID string, extra string) *spServer {
	return r.config(id, `"role":"host","hostId":"`+hostID+`","groupId":"`+r.group+`"`+extra, r.broker)
}

type started struct {
	n node.Node
	e *testEmitter
}

func (r *spRig) start(typ, cfg string) started {
	r.t.Helper()
	n := build(r.t, typ, cfg, r.svc)
	e := newTestEmitter()
	if s, ok := n.(node.Starter); ok {
		if err := s.Start(context.Background(), e); err != nil {
			r.t.Fatalf("starting %s: %v", typ, err)
		}
	}
	r.t.Cleanup(func() {
		if c, ok := n.(node.Closer); ok {
			_ = c.Close(context.Background(), false)
		}
	})
	// Births are QoS 0 and not retained, and a host that subscribes after one has
	// gone by never sees it and has nothing to ask a rebirth about. Under load
	// the host connects slower than the edge node, so a host is waited for until
	// it is subscribed and has published its STATE, before anything is born.
	if in, ok := n.(*spInNode); ok && in.srv != nil && in.srv.role == spRoleHost {
		r.eventually("the host to be online", 20*time.Second, in.srv.isReady)
	}
	return started{n, e}
}

func (r *spRig) eventually(what string, within time.Duration, cond func() bool) {
	r.t.Helper()
	waitUntilTrue(r.t, what, within, cond)
}

func spMsgs(e *testEmitter) []*engine.Msg { return e.on(0) }

func spType(m *engine.Msg) string {
	sp, _ := m.Data["sparkplug"].(map[string]any)
	s, _ := sp["type"].(string)
	return s
}

func spFind(e *testEmitter, typ string, pred func(*engine.Msg) bool) *engine.Msg {
	for _, m := range spMsgs(e) {
		if spType(m) == typ && (pred == nil || pred(m)) {
			return m
		}
	}
	return nil
}

func spField(m *engine.Msg, k string) any {
	sp, _ := m.Data["sparkplug"].(map[string]any)
	return sp[k]
}

func TestSparkplugEdgeNodeAndHostTalkThroughARealBroker(t *testing.T) {
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/#")

	r.host("h", "host-"+r.group, "")
	r.edge("e", "line3", r.broker)
	host := r.start("sparkplug in", `{"server":"h"}`)
	node1 := r.start("sparkplug out", `{"server":"e","metrics":[{"name":"Uptime","dataType":"UInt32","initial":"5"}]}`)
	dev := r.start("sparkplug out", `{"server":"e","device":"press1","metrics":[
		{"name":"Temperature","dataType":"Double","initial":"20.5"},{"name":"Running","dataType":"Boolean","initial":"false"},
		{"name":"Count","dataType":"Int32","initial":"7"},{"name":"Note","dataType":"String"}]}`)

	// Born: one NBIRTH and then one DBIRTH, in that order, seen by the host.
	r.eventually("the host to see both births", 20*time.Second, func() bool {
		return spFind(host.e, "NBIRTH", nil) != nil && spFind(host.e, "DBIRTH", nil) != nil
	})
	nb := spFind(host.e, "NBIRTH", nil)
	db := spFind(host.e, "DBIRTH", nil)
	if spField(nb, "seq") != 0.0 || spField(db, "seq") != 1.0 || spField(nb, "bdSeq") != 0.0 {
		t.Errorf("NBIRTH seq %v bdSeq %v, DBIRTH seq %v", spField(nb, "seq"), spField(nb, "bdSeq"), spField(db, "seq"))
	}
	if !reflect.DeepEqual(nb.Payload(), map[string]any{"bdSeq": 0.0, "Node Control/Rebirth": false, "Uptime": 5.0}) {
		t.Errorf("NBIRTH payload = %#v", nb.Payload())
	}
	if !reflect.DeepEqual(db.Payload(), map[string]any{"Temperature": 20.5, "Running": false, "Count": 7.0, "Note": nil}) {
		t.Errorf("DBIRTH payload = %#v", db.Payload())
	}
	if db.Topic() != "spBv1.0/"+r.group+"/DBIRTH/line3/press1" || spField(db, "device") != "press1" {
		t.Errorf("DBIRTH topic %q", db.Topic())
	}
	t.Logf("host saw %s seq %v bdSeq %v payload %v", nb.Topic(), spField(nb, "seq"), spField(nb, "bdSeq"), nb.Payload())
	t.Logf("host saw %s seq %v payload %v", db.Topic(), spField(db, "seq"), db.Payload())

	// Data: only what was in the message, with the next sequence number.
	if _, err := send(t, dev.n, msg(t, `{"payload":{"Temperature":22.25,"Count":8}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := send(t, node1.n, msg(t, `{"payload":[{"name":"Uptime","value":6,"timestamp":1760800000000}]}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("DDATA and NDATA to reach the host", 10*time.Second, func() bool {
		return spFind(host.e, "DDATA", nil) != nil && spFind(host.e, "NDATA", nil) != nil
	})
	dd, nd := spFind(host.e, "DDATA", nil), spFind(host.e, "NDATA", nil)
	if !reflect.DeepEqual(dd.Payload(), map[string]any{"Temperature": 22.25, "Count": 8.0}) || spField(dd, "seq") != 2.0 {
		t.Errorf("DDATA = %#v seq %v", dd.Payload(), spField(dd, "seq"))
	}
	if !reflect.DeepEqual(nd.Payload(), map[string]any{"Uptime": 6.0}) || spField(nd, "seq") != 3.0 {
		t.Errorf("NDATA = %#v seq %v", nd.Payload(), spField(nd, "seq"))
	}
	if ms := spField(nd, "metrics").([]any); ms[0].(map[string]any)["timestamp"] != 1760800000000.0 {
		t.Errorf("the metric timestamp the message gave should be kept: %v", ms[0])
	}

	// A device dies on purpose and is born again by its next message.
	if _, err := send(t, dev.n, msg(t, `{"sparkplug":{"death":true}}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("DDEATH", 10*time.Second, func() bool { return spFind(host.e, "DDEATH", nil) != nil })
	if stale := spField(spFind(host.e, "DDEATH", nil), "stale").([]any); len(stale) != 4 || stale[0] != "Count" {
		t.Errorf("a device's death should mark all four metrics stale, got %v", stale)
	}
	if _, err := send(t, dev.n, msg(t, `{"payload":{"Running":true}}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("a second DBIRTH and the data behind it", 10*time.Second, func() bool {
		n := 0
		for _, m := range spMsgs(host.e) {
			if spType(m) == "DBIRTH" {
				n++
			}
		}
		return n == 2 && spFind(host.e, "DDATA", func(m *engine.Msg) bool { return m.Payload().(map[string]any)["Running"] == true }) != nil
	})
	rebirthed := spMsgs(host.e)
	var seqs []float64
	for _, m := range rebirthed {
		if s, ok := spField(m, "seq").(float64); ok {
			seqs = append(seqs, s)
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != math.Mod(seqs[i-1]+1, 256) {
			t.Errorf("sequence numbers have to run on by one: %v", seqs)
			break
		}
	}
	t.Logf("sequence numbers seen by the host, in order: %v", seqs)
	for _, m := range spMsgs(host.e) {
		if spField(m, "outOfOrder") == true {
			t.Errorf("nothing was lost, so nothing should be out of order: %s", m.Topic())
		}
	}

	// What was on the wire, QoS and retain included, is what the specification says.
	for _, w := range []struct {
		suffix string
		qos    byte
	}{{"/NBIRTH/line3", 0}, {"/DBIRTH/line3/press1", 0}, {"/NDATA/line3", 0}, {"/DDATA/line3/press1", 0}, {"/DDEATH/line3/press1", 0}} {
		m, ok := tap.find(onTopic(w.suffix))
		if !ok {
			t.Errorf("nothing was published on %s", w.suffix)
			continue
		}
		if m.QoS != w.qos || m.Retained {
			t.Errorf("%s: QoS %d retain %v, the specification says QoS %d and no retain", w.suffix, m.QoS, m.Retained, w.qos)
		}
	}
	if md := os.Getenv("HOTLOOP_FLOW_SPARKPLUG_PROTO"); md != "" {
		schema := sparkplugSchema(t)
		m := tap.await(t, "the message on the wire", 10*time.Second, onTopic("/NBIRTH/line3"))
		j := schemaJSON(t, schema, m.Payload)
		names := map[string]bool{}
		for _, e := range j["metrics"].([]any) {
			names[e.(map[string]any)["name"].(string)] = true
		}
		if !names["bdSeq"] || !names["Node Control/Rebirth"] || !names["Uptime"] || j["seq"] != "0" {
			t.Errorf("the Eclipse schema reads this NBIRTH as %v", j)
		}
	}
}

func TestSparkplugNDEATHComesFromTheBrokerAndTheNextBirthHasANewBdSeq(t *testing.T) {
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/#")
	r.host("h", "host-"+r.group, "")
	line := newFlakyLine(t, r.broker)
	r.edge("e", "line4", line.addr())
	host := r.start("sparkplug in", `{"server":"h"}`)
	r.start("sparkplug out", `{"server":"e","metrics":[{"name":"Uptime","dataType":"UInt32","initial":"1"}]}`)

	r.eventually("the first NBIRTH", 20*time.Second, func() bool { return spFind(host.e, "NBIRTH", nil) != nil })
	if bd := spField(spFind(host.e, "NBIRTH", nil), "bdSeq"); bd != 0.0 {
		t.Fatalf("the first session's bdSeq = %v", bd)
	}

	// The connection dies with no goodbye. The broker publishes the will.
	line.dropAll()
	r.eventually("the broker's NDEATH", 20*time.Second, func() bool { return spFind(host.e, "NDEATH", nil) != nil })
	nd := spFind(host.e, "NDEATH", nil)
	if spField(nd, "bdSeq") != 0.0 {
		t.Errorf("the NDEATH should carry the bdSeq of the session that died: %v", spField(nd, "bdSeq"))
	}
	if _, has := nd.Data["sparkplug"].(map[string]any)["seq"]; has {
		t.Errorf("an NDEATH has no sequence number")
	}
	if stale := spField(nd, "stale").([]any); len(stale) == 0 {
		t.Errorf("an NDEATH marks the node's metrics stale: %v", stale)
	}
	wm := tap.await(t, "the will on the wire", 5*time.Second, onTopic("/NDEATH/line4"))
	if wm.QoS != 1 || wm.Retained {
		t.Errorf("the will goes out at QoS 1 and not retained, got QoS %d retain %v", wm.QoS, wm.Retained)
	}
	wp, err := decodeSPPayload(wm.Payload)
	if err != nil || len(wp.Metrics) != 1 || wp.Metrics[0].Name != "bdSeq" || wp.HasSeq {
		t.Errorf("an NDEATH carries the bdSeq metric and nothing else: %+v, %v", wp, err)
	}

	// The edge node reconnects by itself, with the next bdSeq.
	r.eventually("a second NBIRTH", 30*time.Second, func() bool {
		n := 0
		for _, m := range spMsgs(host.e) {
			if spType(m) == "NBIRTH" {
				n++
			}
		}
		return n == 2
	})
	var births []*engine.Msg
	for _, m := range spMsgs(host.e) {
		if spType(m) == "NBIRTH" {
			births = append(births, m)
		}
	}
	if spField(births[1], "bdSeq") != 1.0 || spField(births[1], "seq") != 0.0 {
		t.Errorf("the second session: bdSeq %v seq %v", spField(births[1], "bdSeq"), spField(births[1], "seq"))
	}
	t.Logf("edge node cut off: NDEATH bdSeq %v, reborn with bdSeq %v", spField(nd, "bdSeq"), spField(births[1], "bdSeq"))
}

func TestSparkplugHostAsksForARebirthAndCommandsReachTheEdgeNode(t *testing.T) {
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/#")
	r.host("h", "host-"+r.group, "")
	r.edge("e", "line5", r.broker)
	host := r.start("sparkplug in", `{"server":"h"}`)
	cmd := r.start("sparkplug command", `{"server":"h","group":"`+r.group+`","edgeNode":"line5","device":"press1"}`)
	nodeCmd := r.start("sparkplug command", `{"server":"h","group":"`+r.group+`","edgeNode":"line5"}`)
	r.start("sparkplug out", `{"server":"e","device":"press1","metrics":[
		{"name":"Running","dataType":"Boolean"},{"name":"Setpoint","dataType":"Float","initial":"10"}]}`)
	edgeIn := r.start("sparkplug in", `{"server":"e","device":"press1"}`)

	r.eventually("the first births", 20*time.Second, func() bool { return spFind(host.e, "DBIRTH", nil) != nil })

	// A DCMD, typed from what the host saw in the DBIRTH, reaches the flow on the edge node.
	if _, err := send(t, cmd.n, msg(t, `{"payload":{"Setpoint":42.5,"Running":true}}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("the command at the edge node", 10*time.Second, func() bool { return len(spMsgs(edgeIn.e)) > 0 })
	got := spMsgs(edgeIn.e)[0]
	if !reflect.DeepEqual(got.Payload(), map[string]any{"Setpoint": 42.5, "Running": true}) || spType(got) != "DCMD" || spField(got, "device") != "press1" {
		t.Errorf("command = %#v %v", got.Payload(), got.Data["sparkplug"])
	}
	cm := tap.await(t, "the DCMD on the wire", 5*time.Second, onTopic("/DCMD/line5/press1"))
	if cm.QoS != 0 || cm.Retained {
		t.Errorf("a command goes out at QoS 0 and not retained: %d %v", cm.QoS, cm.Retained)
	}
	if cp, err := decodeSPPayload(cm.Payload); err != nil || cp.HasSeq || !cp.HasTimestamp {
		t.Errorf("a command has a timestamp and no sequence number: %+v %v", cp, err)
	}
	t.Logf("host sent a DCMD, the edge node's flow received %v", got.Payload())

	// Asking for a rebirth: the same bdSeq, sequence back to zero, the devices again.
	before := len(spMsgs(host.e))
	if _, err := send(t, nodeCmd.n, msg(t, `{"payload":{"Node Control/Rebirth":true}}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("a second NBIRTH and DBIRTH after the request", 15*time.Second, func() bool {
		n, d := 0, 0
		for _, m := range spMsgs(host.e)[before:] {
			switch spType(m) {
			case "NBIRTH":
				n++
			case "DBIRTH":
				d++
			}
		}
		return n == 1 && d == 1
	})
	var nb *engine.Msg
	for _, m := range spMsgs(host.e)[before:] {
		if spType(m) == "NBIRTH" {
			nb = m
		}
	}
	if spField(nb, "bdSeq") != 0.0 || spField(nb, "seq") != 0.0 {
		t.Errorf("a rebirth keeps the bdSeq and starts the sequence over: bdSeq %v seq %v", spField(nb, "bdSeq"), spField(nb, "seq"))
	}
	if len(spMsgs(edgeIn.e)) != 1 {
		t.Errorf("the rebirth request is the edge node's to answer, and isn't passed to the flow")
	}
	t.Logf("after the host's Node Control/Rebirth: NBIRTH bdSeq %v seq %v, then the DBIRTH", spField(nb, "bdSeq"), spField(nb, "seq"))

	// A metric the host never saw born can't be guessed at, and the error says what to do.
	if _, err := send(t, cmd.n, msg(t, `{"payload":{"Mystery":1}}`)); err == nil ||
		!strings.Contains(err.Error(), "hasn't seen metric") || !strings.Contains(err.Error(), "\"type\"") {
		t.Errorf("an unknown metric: %v", err)
	}
	if _, err := send(t, cmd.n, msg(t, `{"payload":[{"name":"Mystery","value":1,"type":"Int16"}]}`)); err != nil {
		t.Errorf("with its type given it goes: %v", err)
	}
}

func TestSparkplugHostApplicationStateIsRetainedAndItsWillSaysOffline(t *testing.T) {
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/STATE/#")
	line := newFlakyLine(t, r.broker)
	hostID := "host-" + r.group
	host := r.config("h", `"role":"host","hostId":"`+hostID+`","groupId":"`+r.group+`"`, line.addr())
	_ = host
	in := r.start("sparkplug in", `{"server":"h","state":true}`)

	birth := tap.await(t, "STATE online", 20*time.Second, func(m tapMsg) bool {
		return strings.HasSuffix(m.Topic, "/STATE/"+hostID) && strings.Contains(string(m.Payload), `"online":true`)
	})
	if birth.QoS != 1 {
		t.Errorf("the STATE birth goes out at QoS 1, got %d", birth.QoS)
	}
	var st map[string]any
	_ = json.Unmarshal(birth.Payload, &st)
	ts, _ := st["timestamp"].(float64)
	if ts < float64(time.Now().Add(-time.Minute).UnixMilli()) || len(st) != 2 {
		t.Errorf("STATE birth = %s", birth.Payload)
	}
	t.Logf("STATE birth on the wire: %s", birth.Payload)

	// A late subscriber is handed the retained STATE: a new edge node sees the host online.
	late := newTap(t, r.broker, "spBv1.0/STATE/"+hostID)
	lm := late.await(t, "the retained STATE", 5*time.Second, func(m tapMsg) bool { return m.Retained })
	if !strings.Contains(string(lm.Payload), `"online":true`) {
		t.Errorf("retained STATE = %s", lm.Payload)
	}

	// The connection dies. The will is the offline certificate, with the same timestamp.
	line.dropAll()
	death := tap.await(t, "the STATE will", 20*time.Second, func(m tapMsg) bool {
		return strings.HasSuffix(m.Topic, "/STATE/"+hostID) && strings.Contains(string(m.Payload), `"online":false`)
	})
	var ds map[string]any
	_ = json.Unmarshal(death.Payload, &ds)
	if ds["timestamp"] != st["timestamp"] {
		t.Errorf("the will's timestamp %v is not the birth's %v", ds["timestamp"], st["timestamp"])
	}
	t.Logf("STATE will on the wire: %s", death.Payload)

	// Reconnected, it says online again with a new timestamp, and the flow saw the whole run.
	tap.await(t, "STATE online again", 30*time.Second, func(m tapMsg) bool {
		var s map[string]any
		_ = json.Unmarshal(m.Payload, &s)
		return strings.HasSuffix(m.Topic, "/STATE/"+hostID) && s["online"] == true && s["timestamp"] != st["timestamp"]
	})
	if len(spMsgs(in.e)) < 2 {
		t.Errorf("the In node with State on sees host STATE messages, saw %d", len(spMsgs(in.e)))
	}

	// Somebody delivers our own offline certificate: say online at once.
	seen := len(tap.all())
	tap.publish(t, "spBv1.0/STATE/"+hostID, 1, true, []byte(`{"online":false,"timestamp":1}`))
	waitUntilTrue(t, "the host to correct a false offline", 10*time.Second, func() bool {
		for _, m := range tap.all()[seen:] {
			if !m.Retained && strings.Contains(string(m.Payload), `"online":true`) {
				return true
			}
		}
		return false
	})
}

func TestSparkplugHostReadsAnotherImplementationsAliasesGapsAndStrangers(t *testing.T) {
	md := sparkplugSchema(t)
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/"+r.group+"/NCMD/#")
	r.host("h", "host-"+r.group, `,"reorderTimeout":0`)
	host := r.start("sparkplug in", `{"server":"h"}`)
	r.eventually("the host to be online", 10*time.Second, func() bool {
		return host.n.(*spInNode).srv != nil && host.n.(*spInNode).srv.isReady()
	})
	base := "spBv1.0/" + r.group + "/"

	// An edge node from somebody else, that uses aliases and a different int encoding.
	tap.publish(t, base+"NBIRTH/vendor1", 0, false, schemaBytes(t, md, `{"timestamp":"1760800000000","seq":"0","metrics":[
		{"name":"bdSeq","datatype":4,"longValue":"5"},
		{"name":"Node Control/Rebirth","datatype":11,"booleanValue":false},
		{"name":"Boiler/Temp","alias":"1","datatype":10,"doubleValue":80.5},
		{"name":"Boiler/On","alias":"2","datatype":11,"booleanValue":true},
		{"name":"Boiler/Level","alias":"3","datatype":3,"intValue":4294967294}]}`))
	r.eventually("the NBIRTH", 10*time.Second, func() bool { return spFind(host.e, "NBIRTH", nil) != nil })
	nb := spFind(host.e, "NBIRTH", nil)
	if !reflect.DeepEqual(nb.Payload(), map[string]any{"bdSeq": 5.0, "Node Control/Rebirth": false, "Boiler/Temp": 80.5, "Boiler/On": true, "Boiler/Level": -2.0}) {
		t.Errorf("NBIRTH = %#v", nb.Payload())
	}

	// Data that names its metrics by alias only, and carries no datatype.
	tap.publish(t, base+"NDATA/vendor1", 0, false, schemaBytes(t, md, `{"timestamp":"1760800001000","seq":"1","metrics":[
		{"alias":"1","timestamp":"1760800001000","doubleValue":81.25},{"alias":"3","intValue":4294967290}]}`))
	r.eventually("the NDATA", 10*time.Second, func() bool { return spFind(host.e, "NDATA", nil) != nil })
	nd := spFind(host.e, "NDATA", nil)
	if !reflect.DeepEqual(nd.Payload(), map[string]any{"Boiler/Temp": 81.25, "Boiler/Level": -6.0}) {
		t.Errorf("an alias-only NDATA resolved to %#v", nd.Payload())
	}
	t.Logf("alias-only NDATA from another implementation resolved to %v", nd.Payload())

	// A gap: sequence 2 never arrives. With no reorder timeout the host asks at once.
	tap.publish(t, base+"NDATA/vendor1", 0, false, schemaBytes(t, md, `{"timestamp":"1760800002000","seq":"3","metrics":[{"alias":"1","doubleValue":82}]}`))
	cmd := tap.await(t, "a rebirth request", 10*time.Second, onTopic("/NCMD/vendor1"))
	if cmd.QoS != 0 || cmd.Retained {
		t.Errorf("an NCMD goes out at QoS 0 and not retained: %d %v", cmd.QoS, cmd.Retained)
	}
	j := schemaJSON(t, md, cmd.Payload)
	mets := j["metrics"].([]any)
	if len(mets) != 1 || mets[0].(map[string]any)["name"] != "Node Control/Rebirth" || mets[0].(map[string]any)["booleanValue"] != true ||
		mets[0].(map[string]any)["datatype"] != 11.0 {
		t.Errorf("the Eclipse schema reads the rebirth request as %v", j)
	}
	if _, has := j["seq"]; has {
		t.Errorf("an NCMD has no sequence number: %v", j)
	}
	gap := spFind(host.e, "NDATA", func(m *engine.Msg) bool { return spField(m, "seq") == 3.0 })
	if gap == nil || spField(gap, "outOfOrder") != true {
		t.Errorf("the message after the gap is delivered and marked out of order: %v", gap)
	}
	t.Logf("gap in the sequence: the host sent an NCMD asking for 'Node Control/Rebirth', and the Eclipse schema reads it as %v", mets[0])

	// A node the host never saw born sends data. It asks for a birth.
	before := len(tap.all())
	tap.publish(t, base+"NDATA/stranger", 0, false, schemaBytes(t, md, `{"timestamp":"1","seq":"4","metrics":[{"name":"X","datatype":10,"doubleValue":1}]}`))
	tap.await(t, "a rebirth request to the stranger", 10*time.Second, onTopic("/NCMD/stranger"))
	if len(tap.all()) <= before {
		t.Errorf("no request went out")
	}
	// An NDEATH, with a bdSeq, marks what the birth listed as stale.
	tap.publish(t, base+"NDEATH/vendor1", 1, false, schemaBytes(t, md, `{"metrics":[{"name":"bdSeq","datatype":4,"longValue":"5"}]}`))
	r.eventually("the NDEATH", 10*time.Second, func() bool { return spFind(host.e, "NDEATH", nil) != nil })
	if stale := spField(spFind(host.e, "NDEATH", nil), "stale").([]any); len(stale) != 5 {
		t.Errorf("stale = %v", stale)
	}
}

func TestSparkplugAGapThatFillsInWithinTheReorderTimeoutCostsNoRebirth(t *testing.T) {
	md := sparkplugSchema(t)
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/"+r.group+"/NCMD/#")
	r.host("h", "host-"+r.group, `,"reorderTimeout":1.5`)
	host := r.start("sparkplug in", `{"server":"h"}`)
	r.eventually("the host to be online", 10*time.Second, func() bool { return host.n.(*spInNode).srv.isReady() })
	base := "spBv1.0/" + r.group + "/"
	pub := func(typ string, seq int) {
		tap.publish(t, base+typ+"/v2", 0, false, schemaBytes(t, md, fmt.Sprintf(
			`{"timestamp":"1","seq":"%d","metrics":[{"name":"A","datatype":3,"intValue":%d}]}`, seq, seq)))
	}
	pub("NBIRTH", 0)
	r.eventually("the birth", 10*time.Second, func() bool { return spFind(host.e, "NBIRTH", nil) != nil })
	pub("NDATA", 2) // 1 is missing
	r.eventually("the early message", 10*time.Second, func() bool { return spFind(host.e, "NDATA", nil) != nil })
	time.Sleep(400 * time.Millisecond)
	pub("NDATA", 1) // and here it is
	pub("NDATA", 3)
	r.eventually("both", 10*time.Second, func() bool {
		n := 0
		for _, m := range spMsgs(host.e) {
			if spType(m) == "NDATA" {
				n++
			}
		}
		return n == 3
	})
	time.Sleep(2500 * time.Millisecond)
	if _, ok := tap.find(onTopic("/NCMD/v2")); ok {
		t.Errorf("the missing message arrived inside the reorder timeout, so no rebirth should have been asked for")
	}

	// And a gap that is never filled is asked about once the timeout passes.
	pub("NDATA", 6)
	tap.await(t, "a rebirth request after the timeout", 10*time.Second, onTopic("/NCMD/v2"))
}

func TestSparkplugHostStartedAfterTheEdgeNodeGetsItsBirthByAsking(t *testing.T) {
	r := newSpRig(t)
	r.edge("e", "line6", r.broker)
	r.start("sparkplug out", `{"server":"e","device":"d1","metrics":[{"name":"V","dataType":"Double","initial":"1"}]}`)
	dev := r.start("sparkplug out", `{"server":"e","metrics":[{"name":"U","dataType":"Int32","initial":"2"}]}`)
	r.eventually("the edge node to be born", 20*time.Second, func() bool { return r.svc.configs["e"].(*spServer).isReady() })

	// The host comes up later and has never seen a birth.
	r.host("h", "host-"+r.group, "")
	host := r.start("sparkplug in", `{"server":"h"}`)
	r.eventually("the host to be online", 10*time.Second, func() bool { return host.n.(*spInNode).srv.isReady() })
	if _, err := send(t, dev.n, msg(t, `{"payload":{"U":3}}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("a birth the host had to ask for", 20*time.Second, func() bool {
		return spFind(host.e, "NBIRTH", nil) != nil && spFind(host.e, "DBIRTH", nil) != nil
	})
	nb := spFind(host.e, "NBIRTH", nil)
	if nb.Payload().(map[string]any)["U"] != 3.0 {
		t.Errorf("the rebirth carries the current values: %v", nb.Payload())
	}
	t.Logf("host had never seen a birth, saw NDATA, asked, and got NBIRTH with %v", nb.Payload())
}

func TestSparkplugWhatTheEdgeNodeCannotDoItSaysSo(t *testing.T) {
	r := newSpRig(t)
	// An edge node that isn't connected: the broker is a closed port.
	r.edge("e", "line7", "127.0.0.1:1")
	out := r.start("sparkplug out", `{"server":"e","metrics":[{"name":"A","dataType":"Double"}]}`)
	_, err := send(t, out.n, msg(t, `{"payload":{"A":1}}`))
	if err == nil || !strings.Contains(err.Error(), "so nothing was published") {
		t.Errorf("not connected: %v", err)
	}
	r.eventually("the badge to say why", 10*time.Second, func() bool {
		out.e.mu.Lock()
		defer out.e.mu.Unlock()
		for _, s := range out.e.statuses {
			if s.Fill == "red" && strings.Contains(s.Text, "refused") {
				return true
			}
		}
		return false
	})
	srv := r.svc.configs["e"].(*spServer)
	srv.mu.Lock()
	why := srv.lastErr
	srv.mu.Unlock()
	if !strings.Contains(why, "refused the connection") || !strings.Contains(why, "Nothing is listening") {
		t.Errorf("the reason = %q", why)
	}
	t.Logf("operator sees: %s", why)

	// Two nodes for one device is refused, and says so.
	r.edge("e2", "line8", r.broker)
	r.start("sparkplug out", `{"server":"e2","device":"d","metrics":[{"name":"A","dataType":"Double"}]}`)
	dup := build(t, "sparkplug out", `{"server":"e2","device":"d","metrics":[{"name":"B","dataType":"Double"}]}`, r.svc)
	if err := dup.(node.Starter).Start(context.Background(), newTestEmitter()); err == nil || !strings.Contains(err.Error(), "already publishes device d") {
		t.Errorf("a second node for one device: %v", err)
	}
	// An Out node on a host connection says to use an edge connection.
	r.host("h", "host-"+r.group, "")
	wrong := build(t, "sparkplug out", `{"server":"h","metrics":[{"name":"A","dataType":"Double"}]}`, r.svc)
	if err := wrong.(node.Starter).Start(context.Background(), newTestEmitter()); err == nil || !strings.Contains(err.Error(), "is a host application") {
		t.Errorf("an Out node on a host connection: %v", err)
	}
}

func (s *spServer) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func TestSparkplugAliasesBothWays(t *testing.T) {
	md := sparkplugSchema(t)
	r := newSpRig(t)
	tap := newTap(t, r.broker, "spBv1.0/#")
	r.host("h", "host-"+r.group, "")
	r.config("e", `"role":"edge","groupId":"`+r.group+`","edgeNodeId":"line9","useAliases":true`, r.broker)
	host := r.start("sparkplug in", `{"server":"h"}`)
	cmd := r.start("sparkplug command", `{"server":"h","group":"`+r.group+`","edgeNode":"line9","device":"d1"}`)
	r.start("sparkplug out", `{"server":"e","metrics":[{"name":"Uptime","dataType":"UInt32","initial":"1"}]}`)
	dev := r.start("sparkplug out", `{"server":"e","device":"d1","metrics":[
		{"name":"Temp","dataType":"Double","initial":"1"},{"name":"SP","dataType":"Double","initial":"2"}]}`)
	edgeIn := r.start("sparkplug in", `{"server":"e","device":"d1"}`)
	r.eventually("the births", 20*time.Second, func() bool { return spFind(host.e, "DBIRTH", nil) != nil })

	// The births carry names and numbers, and not a number for the rebirth metric.
	nb := tap.await(t, "the message on the wire", 10*time.Second, onTopic("/NBIRTH/line9"))
	nj := schemaJSON(t, md, nb.Payload)
	aliases := map[string]any{}
	for _, e := range nj["metrics"].([]any) {
		m := e.(map[string]any)
		aliases[m["name"].(string)] = m["alias"]
	}
	if aliases["Uptime"] != "1" || aliases["Node Control/Rebirth"] != nil {
		t.Errorf("NBIRTH aliases = %v. Uptime should be 1 and the rebirth metric has none", aliases)
	}
	db := tap.await(t, "the message on the wire", 10*time.Second, onTopic("/DBIRTH/line9/d1"))
	dj := schemaJSON(t, md, db.Payload)
	for _, e := range dj["metrics"].([]any) {
		m := e.(map[string]any)
		if m["alias"] == nil || m["name"] == nil {
			t.Errorf("a DBIRTH metric carries both a name and an alias: %v", m)
		}
	}
	if dj["metrics"].([]any)[0].(map[string]any)["alias"] != "2" || dj["metrics"].([]any)[1].(map[string]any)["alias"] != "3" {
		t.Errorf("aliases are one edge node wide, node first: %v", dj["metrics"])
	}

	// Data carries the number only, and the host puts the name back.
	if _, err := send(t, dev.n, msg(t, `{"payload":{"Temp":55.5}}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("the DDATA", 10*time.Second, func() bool { return spFind(host.e, "DDATA", nil) != nil })
	if got := spFind(host.e, "DDATA", nil).Payload(); !reflect.DeepEqual(got, map[string]any{"Temp": 55.5}) {
		t.Errorf("the host should have the name back: %v", got)
	}
	dd := tap.await(t, "the message on the wire", 10*time.Second, onTopic("/DDATA/line9/d1"))
	first := schemaJSON(t, md, dd.Payload)["metrics"].([]any)[0].(map[string]any)
	if first["name"] != nil || first["alias"] != "2" || first["datatype"] != nil || first["doubleValue"] != 55.5 {
		t.Errorf("a data metric is an alias and a value: %v", first)
	}

	// And the host commands by number, because the birth gave one.
	if _, err := send(t, cmd.n, msg(t, `{"payload":{"SP":88.5}}`)); err != nil {
		t.Fatal(err)
	}
	r.eventually("the command at the edge node", 10*time.Second, func() bool { return len(spMsgs(edgeIn.e)) > 0 })
	if got := spMsgs(edgeIn.e)[0].Payload(); !reflect.DeepEqual(got, map[string]any{"SP": 88.5}) {
		t.Errorf("the edge node should have the name back: %v", got)
	}
	cm := tap.await(t, "the message on the wire", 10*time.Second, onTopic("/DCMD/line9/d1"))
	cj := schemaJSON(t, md, cm.Payload)["metrics"].([]any)[0].(map[string]any)
	if cj["name"] != nil || cj["alias"] != "3" || cj["doubleValue"] != 88.5 {
		t.Errorf("a command on the wire is an alias and a value: %v", cj)
	}
	t.Logf("with aliases: DDATA on the wire is %v, the DCMD is %v", first, cj)
}
