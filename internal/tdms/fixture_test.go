package tdms

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// hx turns lines of hex bytes into bytes. Anything after // on a line is a
// comment, and every line below says which field its bytes are.
func hx(lines ...string) []byte {
	var out []byte
	for _, l := range lines {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		for _, tok := range strings.Fields(l) {
			v, err := strconv.ParseUint(tok, 16, 8)
			if err != nil {
				panic("bad hex " + tok + " in " + l)
			}
			out = append(out, byte(v))
		}
	}
	return out
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// i32s is raw data: little endian 32 bit integers.
func i32s(v ...int) []byte {
	var out []byte
	for _, x := range v {
		out = binary.LittleEndian.AppendUint32(out, uint32(x))
	}
	return out
}

func seq(from, to int) []int {
	var out []int
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// Paths, as the document writes them, as bytes.
var (
	pathCh1  = hx("13 00 00 00 // length of the path, 19", "2F 27 67 72 6F 75 70 27 2F 27 63 68 61 6E 6E 65 6C 31 27 // /'group'/'channel1'")
	pathCh2  = hx("13 00 00 00 // length of the path, 19", "2F 27 67 72 6F 75 70 27 2F 27 63 68 61 6E 6E 65 6C 32 27 // /'group'/'channel2'")
	pathVolt = hx("12 00 00 00 // length of the path, 18", "2F 27 67 72 6F 75 70 27 2F 27 76 6F 6C 74 61 67 65 27 // /'group'/'voltage'")
)

// index32 is a raw data index for n 32 bit integers.
func index32(n byte) []byte {
	return hx(
		"14 00 00 00 // length of the index information, 20",
		"03 00 00 00 // data type 3, a 32 bit integer",
		"01 00 00 00 // array dimension, always 1",
		fmt.Sprintf("%02X", n)+" 00 00 00 00 00 00 00 // number of values",
	)
}

// incrementalFile is the five segments of the document's "Incremental Meta
// Information Example", written out byte for byte. Its numbers are the
// document's: the first segment's next segment offset is 0xA7 because the second
// writing iteration appended a second chunk of raw data to it.
func incrementalFile() (file []byte, ends []int) {
	var f []byte
	add := func(b []byte) { f = append(f, b...) }
	end := func() { ends = append(ends, len(f)) }

	// segment 1: channel1 and channel2, three values each, two chunks
	add(hx(
		"54 44 53 6D // tag TDSm",
		"0E 00 00 00 // ToC: meta data, new object list, raw data",
		"69 12 00 00 // version 4713",
		"A7 00 00 00 00 00 00 00 // next segment offset 167: 119 of meta data and two chunks of 24",
		"77 00 00 00 00 00 00 00 // raw data offset 119",
		"02 00 00 00 // two objects",
	))
	add(pathCh1)
	add(index32(3))
	add(hx(
		"01 00 00 00 // one property",
		"04 00 00 00 70 72 6F 70 // name: prop",
		"20 00 00 00 // type 0x20, a string",
		"05 00 00 00 76 61 6C 69 64 // value: valid",
	))
	add(pathCh2)
	add(index32(3))
	add(hx("00 00 00 00 // no properties"))
	add(cat(i32s(1, 2, 3), i32s(4, 5, 6)))
	end() // after the first chunk of segment 1, at len-12... recorded below
	add(cat(i32s(1, 2, 3), i32s(4, 5, 6)))
	end()

	// segment 2: only the property prop on channel1 changes, to error
	add(hx(
		"54 44 53 6D", "0A 00 00 00 // ToC: meta data, raw data", "69 12 00 00",
		"50 00 00 00 00 00 00 00 // next segment offset 80",
		"38 00 00 00 00 00 00 00 // raw data offset 56",
		"01 00 00 00 // one object",
	))
	add(pathCh1)
	add(hx(
		"00 00 00 00 // the raw data index is the one it was",
		"01 00 00 00 // one property",
		"04 00 00 00 70 72 6F 70 20 00 00 00 // prop, a string",
		"05 00 00 00 65 72 72 6F 72 // value: error",
	))
	add(cat(i32s(1, 2, 3), i32s(4, 5, 6)))
	end()

	// segment 3: a new channel, voltage, with five values
	add(hx(
		"54 44 53 6D", "0A 00 00 00", "69 12 00 00",
		"5E 00 00 00 00 00 00 00 // next segment offset 94",
		"32 00 00 00 00 00 00 00 // raw data offset 50",
		"01 00 00 00 // one object",
	))
	add(pathVolt)
	add(index32(5))
	add(hx("00 00 00 00 // no properties"))
	add(cat(i32s(1, 2, 3), i32s(4, 5, 6), i32s(7, 8, 9, 10, 11)))
	end()

	// segment 4: channel2 now has 27 values
	add(hx(
		"54 44 53 6D", "0A 00 00 00", "69 12 00 00",
		"BF 00 00 00 00 00 00 00 // next segment offset 191",
		"33 00 00 00 00 00 00 00 // raw data offset 51",
		"01 00 00 00 // one object",
	))
	add(pathCh2)
	add(index32(27))
	add(hx("00 00 00 00 // no properties"))
	add(cat(i32s(1, 2, 3), i32s(seq(1, 27)...), i32s(7, 8, 9, 10, 11)))
	end()

	// segment 5: channel2 is gone, which needs a new object list
	add(hx(
		"54 44 53 6D", "0E 00 00 00 // ToC: meta data, new object list, raw data", "69 12 00 00",
		"61 00 00 00 00 00 00 00 // next segment offset 97",
		"41 00 00 00 00 00 00 00 // raw data offset 65",
		"02 00 00 00 // two objects",
	))
	add(pathCh1)
	add(hx("00 00 00 00 // same raw data index", "00 00 00 00 // no properties"))
	add(pathVolt)
	add(hx("00 00 00 00 // same raw data index", "00 00 00 00 // no properties"))
	add(cat(i32s(1, 2, 3), i32s(7, 8, 9, 10, 11)))
	end()
	return f, ends
}

func ints(v any) []int {
	var out []int
	for _, x := range v.([]int32) {
		out = append(out, int(x))
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// metaExampleFile is the document's "Meta Data" example: a group with a string
// and an integer property, and a channel of two 32 bit integers. The document
// gives the meta data; the lead in and the raw data (1 and 2, from its layout
// example) are mine, with the offsets worked out by hand below.
func metaExampleFile() []byte {
	meta := hx(
		"02 00 00 00 // number of objects",
		"08 00 00 00 // length of the first object path",
		"2F 27 47 72 6F 75 70 27 // /'Group'",
		"FF FF FF FF // no raw data",
		"02 00 00 00 // two properties",
		"04 00 00 00 70 72 6F 70 // prop",
		"20 00 00 00 // string",
		"05 00 00 00 76 61 6C 75 65 // value",
		"03 00 00 00 6E 75 6D // num",
		"03 00 00 00 // 32 bit integer",
		"0A 00 00 00 // 10",
		"13 00 00 00 // length of the second path",
		"2F 27 47 72 6F 75 70 27 2F 27 43 68 61 6E 6E 65 6C 31 27 // /'Group'/'Channel1'",
		"14 00 00 00 // length of index information",
		"03 00 00 00 // data type, 32 bit integer",
		"01 00 00 00 // dimension",
		"02 00 00 00 00 00 00 00 // two values",
		"00 00 00 00 // no properties",
	)
	raw := i32s(1, 2)
	lead := hx("54 44 53 6D // tag", "0E 00 00 00 // ToC: meta, new list, raw", "69 12 00 00 // version")
	lead = binary.LittleEndian.AppendUint64(lead, uint64(len(meta)+len(raw))) // next segment offset
	lead = binary.LittleEndian.AppendUint64(lead, uint64(len(meta)))          // raw data offset
	return cat(lead, meta, raw)
}

// interleavedFile has the document's interleaved example: channel1 is 1, 2, 3
// and channel2 is 4, 5, 6, stored as 1, 4, 2, 5, 3, 6.
func interleavedFile() []byte {
	meta := cat(hx("02 00 00 00 // two objects"), pathCh1, index32(3), hx("00 00 00 00"), pathCh2, index32(3), hx("00 00 00 00"))
	raw := i32s(1, 4, 2, 5, 3, 6)
	lead := hx("54 44 53 6D", "2E 00 00 00 // ToC: meta, new list, raw, interleaved", "69 12 00 00")
	lead = binary.LittleEndian.AppendUint64(lead, uint64(len(meta)+len(raw)))
	lead = binary.LittleEndian.AppendUint64(lead, uint64(len(meta)))
	return cat(lead, meta, raw)
}

// bigEndianFile is the same two channels with the big endian bit set: every
// number but the ToC is written most significant byte first.
func bigEndianFile() []byte {
	idx := hx(
		"00 00 00 14 // length of the index information, 20",
		"00 00 00 03 // 32 bit integer", "00 00 00 01 // dimension",
		"00 00 00 00 00 00 00 03 // three values",
	)
	meta := cat(
		hx("00 00 00 02 // two objects"),
		hx("00 00 00 13", "2F 27 67 72 6F 75 70 27 2F 27 63 68 61 6E 6E 65 6C 31 27"), idx,
		hx("00 00 00 01 // one property", "00 00 00 04 70 72 6F 70 // prop", "00 00 00 03 // 32 bit integer", "00 00 00 2A // 42"),
		hx("00 00 00 13", "2F 27 67 72 6F 75 70 27 2F 27 63 68 61 6E 6E 65 6C 32 27"), idx, hx("00 00 00 00"),
	)
	raw := hx("00 00 00 01 00 00 00 02 00 00 00 03 // channel1", "00 00 00 04 00 00 00 05 00 00 00 06 // channel2")
	lead := hx(
		"54 44 53 6D // tag",
		"4E 00 00 00 // ToC, always little endian: meta, new list, raw, big endian",
		"00 00 12 69 // version 4713, big endian",
	)
	lead = binary.BigEndian.AppendUint64(lead, uint64(len(meta)+len(raw)))
	lead = binary.BigEndian.AppendUint64(lead, uint64(len(meta)))
	return cat(lead, meta, raw)
}

func TestFixtureMetaExample(t *testing.T) {
	f, err := Read(bytes.NewReader(metaExampleFile()), int64(len(metaExampleFile())))
	if err != nil {
		t.Fatal(err)
	}
	g := f.Group("Group")
	if g == nil {
		t.Fatal("no group Group")
	}
	if v, _ := Prop(g.Properties, "prop"); v != "value" {
		t.Errorf("prop = %v", v)
	}
	if v, _ := Prop(g.Properties, "num"); v != int32(10) {
		t.Errorf("num = %v (%T)", v, v)
	}
	c := g.Channel("Channel1")
	if c == nil || c.Type != TypeI32 || !eq(ints(c.Values), []int{1, 2}) {
		t.Fatalf("channel = %+v", c)
	}
}

func TestFixtureIncremental(t *testing.T) {
	data, _ := incrementalFile()
	f, err := Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if f.Info.Interrupted || f.Info.Segments != 5 {
		t.Fatalf("info = %+v", f.Info)
	}
	g := f.Group("group")
	ch1, ch2, volt := g.Channel("channel1"), g.Channel("channel2"), g.Channel("voltage")
	want1 := []int{1, 2, 3, 1, 2, 3, 1, 2, 3, 1, 2, 3, 1, 2, 3, 1, 2, 3}
	want2 := append([]int{4, 5, 6, 4, 5, 6, 4, 5, 6, 4, 5, 6}, seq(1, 27)...)
	wantV := append(append(seq(7, 11), seq(7, 11)...), seq(7, 11)...)
	if !eq(ints(ch1.Values), want1) {
		t.Errorf("channel1 = %v", ints(ch1.Values))
	}
	if !eq(ints(ch2.Values), want2) {
		t.Errorf("channel2 = %v", ints(ch2.Values))
	}
	if !eq(ints(volt.Values), wantV) {
		t.Errorf("voltage = %v", ints(volt.Values))
	}
	if v, _ := Prop(ch1.Properties, "prop"); v != "error" {
		t.Errorf("prop = %v, want the later value, error", v)
	}
}

func TestFixtureInterleaved(t *testing.T) {
	data := interleavedFile()
	f, err := Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	g := f.Group("group")
	if !eq(ints(g.Channel("channel1").Values), []int{1, 2, 3}) || !eq(ints(g.Channel("channel2").Values), []int{4, 5, 6}) {
		t.Fatalf("got %v and %v", g.Channel("channel1").Values, g.Channel("channel2").Values)
	}
}

func TestFixtureBigEndian(t *testing.T) {
	data := bigEndianFile()
	f, err := Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	g := f.Group("group")
	if !eq(ints(g.Channel("channel1").Values), []int{1, 2, 3}) || !eq(ints(g.Channel("channel2").Values), []int{4, 5, 6}) {
		t.Fatalf("got %v and %v", g.Channel("channel1").Values, g.Channel("channel2").Values)
	}
	if v, _ := Prop(g.Channel("channel1").Properties, "prop"); v != int32(42) {
		t.Errorf("prop = %v", v)
	}
}

// The document's DAQmx example starts a raw data index with 69 12 00 00. A file
// that needs it is refused by name, not misread.
func TestFixtureDAQmxRefused(t *testing.T) {
	meta := cat(hx("01 00 00 00 // one object"), pathCh1, hx(
		"69 12 00 00 // the raw data index starts with 0x1269: DAQmx raw data with a format changing scaler",
		"FF FF FF FF 01 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 // the rest, which is not read",
	))
	lead := hx("54 44 53 6D", "0E 00 00 00", "69 12 00 00")
	lead = binary.LittleEndian.AppendUint64(lead, uint64(len(meta)))
	lead = binary.LittleEndian.AppendUint64(lead, uint64(len(meta)))
	data := cat(lead, meta)
	_, err := Read(bytes.NewReader(data), int64(len(data)))
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("err = %v", err)
	}
}
