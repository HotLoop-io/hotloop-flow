package js

import (
	"reflect"
	"testing"
)

func TestBufferBuildsAndReadsAFloatTheWayAPlantDoes(t *testing.T) {
	p := compile(t, `
		let buf = Buffer.alloc(4);
		buf.writeUInt16BE(17087, 0);
		buf.writeUInt16BE(13107, 2);
		return {payload: buf.readFloatBE(0), hex: buf.toString("hex"), n: buf.length};`, Limits{})
	res, err := run(t, p, nil, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	m := res.ByPort[0][0]
	got, _, _ := m.Get("payload")
	if f, ok := got.(float64); !ok || float32(f) != float32(95.6) {
		t.Fatalf("float = %#v, want 95.6", got)
	}
	if hex, _, _ := m.Get("hex"); hex != "42bf3333" {
		t.Fatalf("hex = %v", hex)
	}
}

func TestBufferWritesIntsAndFloatsInBothByteOrders(t *testing.T) {
	p := compile(t, `
		const b = Buffer.alloc(16);
		b.writeUInt32BE(0x12345678, 0);
		b.writeInt32BE(-2, 4);
		b.writeFloatBE(1.5, 8);
		b.writeUInt16LE(0x0102, 12);
		return {payload: [b.readUInt32BE(0), b.readInt32BE(4), b.readFloatBE(8), b.readUInt16LE(12), b.readUInt16BE(12), b.readUInt32LE(0)]};`, Limits{})
	res, err := run(t, p, nil, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	got := payloadOf(t, res.ByPort[0][0])
	want := []any{float64(0x12345678), float64(-2), 1.5, float64(0x0102), float64(0x0201), float64(0x78563412)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestBufferFromAByteSliceInAMessageAndBackOut(t *testing.T) {
	p := compile(t, `
		const b = Buffer.from(msg.payload);
		const out = Buffer.alloc(2);
		out.writeUInt16BE(b.readUInt16BE(2));
		return {payload: out, s: Buffer.from("héllo").toString(), b64: Buffer.from("hi").toString("base64"), isBuf: Buffer.isBuffer(b)};`, Limits{})
	res, err := run(t, p, []byte{1, 2, 0xAB, 0xCD}, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	m := res.ByPort[0][0]
	if got := payloadOf(t, m); !reflect.DeepEqual(got, []byte{0xAB, 0xCD}) {
		t.Fatalf("payload = %#v, want the two bytes as a byte slice", got)
	}
	if s, _, _ := m.Get("s"); s != "héllo" {
		t.Fatalf("utf8 round trip = %v", s)
	}
	if s, _, _ := m.Get("b64"); s != "aGk=" {
		t.Fatalf("base64 = %v", s)
	}
	if v, _, _ := m.Get("isBuf"); v != true {
		t.Fatalf("isBuffer = %v", v)
	}
}

func TestBufferRefusesAReadPastTheEnd(t *testing.T) {
	p := compile(t, `return {payload: Buffer.alloc(2).readUInt32BE(0)};`, Limits{})
	if _, err := run(t, p, nil, 1, newServices()); err == nil {
		t.Fatal("a read past the end of a Buffer should throw, as it does in Node")
	}
}

func TestBufferSliceSharesMemoryAndConcatCopies(t *testing.T) {
	p := compile(t, `
		const a = Buffer.from([1,2,3,4]);
		const s = a.slice(1,3); s[0] = 9;
		const c = Buffer.concat([a, Buffer.from([5])]);
		return {payload: [a[1], c.length, c[4], s.length]};`, Limits{})
	res, err := run(t, p, nil, 1, newServices())
	if err != nil {
		t.Fatal(err)
	}
	want := []any{float64(9), float64(5), float64(5), float64(2)}
	if got := payloadOf(t, res.ByPort[0][0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}
