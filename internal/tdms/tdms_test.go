package tdms

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func tmp(t *testing.T) string { return filepath.Join(t.TempDir(), "run.tdms") }

func mustRead(t *testing.T, path string) *File {
	t.Helper()
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func f64(t *testing.T, f *File, g, c string) []float64 {
	t.Helper()
	ch := f.Channel(g, c)
	if ch == nil {
		t.Fatalf("no channel %s/%s", g, c)
	}
	v, ok := ch.Float64s()
	if !ok {
		t.Fatalf("%s/%s holds %T", g, c, ch.Values)
	}
	return v
}

// segment is one segment as it sits in a file.
type segment struct {
	start, end int
	toc        uint32
	rawOff     int
}

// segments walks a file's lead ins.
func segments(t *testing.T, b []byte) []segment {
	t.Helper()
	var out []segment
	for pos := 0; pos < len(b); {
		if len(b)-pos < leadLen || string(b[pos:pos+4]) != "TDSm" {
			t.Fatalf("no segment at %d", pos)
		}
		next := int(binary.LittleEndian.Uint64(b[pos+12:]))
		out = append(out, segment{
			start: pos, end: pos + leadLen + next,
			toc:    binary.LittleEndian.Uint32(b[pos+4:]),
			rawOff: int(binary.LittleEndian.Uint64(b[pos+20:])),
		})
		pos += leadLen + next
	}
	return out
}

// F1: log two groups of three channels with properties on the file, the groups
// and the channels, and a reader lists all of them with their properties.
func TestF1TwoGroupsOfThreeChannels(t *testing.T) {
	path := tmp(t)
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 1, 9, 30, 15, 123456789, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(w.SetFileProperty("title", "bench 4"))
	must(w.SetFileProperty("runs", 3))
	must(w.SetFileProperty("started", start))
	must(w.SetGroupProperty("Pressures", "units", "kPa"))
	must(w.SetGroupProperty("Temperatures", "calibrated", true))
	for _, g := range []string{"Pressures", "Temperatures"} {
		for i, c := range []string{"A", "B", "C"} {
			must(w.SetChannelProperty(g, c, "unit_string", g[:1]+"-"+c))
			must(w.SetChannelProperty(g, c, "wf_increment", 0.5))
			must(w.SetChannelProperty(g, c, "wf_start_time", start))
			must(w.SetChannelProperty(g, c, "wf_samples", int32(4)))
			must(w.SetChannelProperty(g, c, "gain", float32(i)+0.25))
		}
	}
	for step := 0; step < 2; step++ {
		var data []ChannelData
		for gi, g := range []string{"Pressures", "Temperatures"} {
			for ci, c := range []string{"A", "B", "C"} {
				base := float64(gi*100 + ci*10 + step*4)
				data = append(data, ChannelData{Group: g, Channel: c, Values: []float64{base, base + 1, base + 2, base + 3}})
			}
		}
		must(w.WriteChannels(data...))
	}
	must(w.Close())

	f := mustRead(t, path)
	if f.Info.Interrupted {
		t.Fatalf("interrupted: %s", f.Info.Reason)
	}
	if len(f.Groups) != 2 || f.Groups[0].Name != "Pressures" || f.Groups[1].Name != "Temperatures" {
		t.Fatalf("groups = %+v", f.Groups)
	}
	for k, want := range map[string]any{"title": "bench 4", "runs": int64(3), "started": start} {
		if v, ok := Prop(f.Properties, k); !ok || !reflect.DeepEqual(v, want) {
			t.Errorf("file property %s = %v (%T), want %v", k, v, v, want)
		}
	}
	if v, _ := Prop(f.Group("Pressures").Properties, "units"); v != "kPa" {
		t.Errorf("units = %v", v)
	}
	if v, _ := Prop(f.Group("Temperatures").Properties, "calibrated"); v != true {
		t.Errorf("calibrated = %v", v)
	}
	for gi, g := range f.Groups {
		if len(g.Channels) != 3 {
			t.Fatalf("%s has %d channels", g.Name, len(g.Channels))
		}
		for ci, c := range g.Channels {
			base := float64(gi*100 + ci*10)
			want := []float64{base, base + 1, base + 2, base + 3, base + 4, base + 5, base + 6, base + 7}
			if got, _ := c.Float64s(); !reflect.DeepEqual(got, want) {
				t.Errorf("%s/%s = %v, want %v", g.Name, c.Name, got, want)
			}
			if v, _ := Prop(c.Properties, "unit_string"); v != g.Name[:1]+"-"+c.Name {
				t.Errorf("unit_string = %v", v)
			}
			if v, _ := Prop(c.Properties, "wf_increment"); v != 0.5 {
				t.Errorf("wf_increment = %v", v)
			}
			if v, _ := Prop(c.Properties, "wf_start_time"); !reflect.DeepEqual(v, start) {
				t.Errorf("wf_start_time = %v, want %v", v, start)
			}
			if v, _ := Prop(c.Properties, "wf_samples"); v != int32(4) {
				t.Errorf("wf_samples = %v", v)
			}
			if v, _ := Prop(c.Properties, "gain"); v != float32(ci)+0.25 {
				t.Errorf("gain = %v", v)
			}
		}
	}
}

func TestEveryDataType(t *testing.T) {
	path := tmp(t)
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2030, 2, 3, 4, 5, 6, 7, time.UTC)
	vals := map[string]any{
		"i8": []int8{-1, 2}, "i16": []int16{-300, 300}, "i32": []int32{-70000, 70000}, "i64": []int64{-1 << 40, 1 << 40},
		"u8": []uint8{0, 255}, "u16": []uint16{0, 65535}, "u32": []uint32{0, 1 << 31}, "u64": []uint64{0, 1 << 63},
		"f32": []float32{1.5, -2.5}, "f64": []float64{math.Pi, math.Inf(-1)},
		"bool": []bool{true, false, true}, "str": []string{"", "a", "héllo", "it's"}, "time": []time.Time{t0, t0.Add(time.Second)},
	}
	names := []string{"i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "f32", "f64", "bool", "str", "time"}
	var data []ChannelData
	for _, n := range names {
		data = append(data, ChannelData{Group: "it's", Channel: n, Values: vals[n]})
	}
	if err := w.WriteChannels(data...); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChannels(data...); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f := mustRead(t, path)
	for _, n := range names {
		c := f.Channel("it's", n)
		if c == nil {
			t.Fatalf("no channel %s", n)
		}
		want := reflect.ValueOf(vals[n])
		two := reflect.AppendSlice(reflect.MakeSlice(want.Type(), 0, want.Len()*2), want)
		two = reflect.AppendSlice(two, want)
		if !reflect.DeepEqual(c.Values, two.Interface()) {
			t.Errorf("%s = %v, want %v", n, c.Values, two.Interface())
		}
	}
}

// Append across two writer sessions, with a type check that spans them.
func TestAppendAcrossSessions(t *testing.T) {
	path := tmp(t)
	for run := 0; run < 2; run++ {
		w, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.SetFileProperty("run", run); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteChannels(
			ChannelData{Group: "g", Channel: "x", Values: []float64{float64(run), float64(run) + 0.5}},
			ChannelData{Group: "g", Channel: "n", Values: []string{"run", "ok"}},
		); err != nil {
			t.Fatal(err)
		}
		if run == 1 {
			if err := w.WriteChannel("g", "x", []string{"no"}); err == nil {
				t.Fatal("a float channel took strings")
			}
			if err := w.WriteChannel("g", "late", []int32{9}); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	f := mustRead(t, path)
	if got := f64(t, f, "g", "x"); !reflect.DeepEqual(got, []float64{0, 0.5, 1, 1.5}) {
		t.Errorf("x = %v", got)
	}
	if got := f.Channel("g", "n").Values; !reflect.DeepEqual(got, []string{"run", "ok", "run", "ok"}) {
		t.Errorf("n = %v", got)
	}
	if v, _ := Prop(f.Properties, "run"); v != int64(1) {
		t.Errorf("run = %v, want the later value", v)
	}
	if len(f.Groups) != 1 {
		t.Errorf("groups = %d", len(f.Groups))
	}
	if got := f.Channel("g", "late").Values; !reflect.DeepEqual(got, []int32{9}) {
		t.Errorf("late = %v", got)
	}
}

// Meta data is written only when it changes: a repeat of the same write is a
// lead in and raw data and nothing else.
func TestSegmentsWithAndWithoutMeta(t *testing.T) {
	path := tmp(t)
	w, _ := Open(path)
	a := ChannelData{Group: "g", Channel: "a", Values: []float64{1, 2}}
	b := ChannelData{Group: "g", Channel: "b", Values: []float64{3, 4}}
	steps := []func() error{
		func() error { return w.WriteChannels(a, b) },                                                        // 0: everything new
		func() error { return w.WriteChannels(a, b) },                                                        // 1: nothing changed
		func() error { return w.WriteChannels(a, b) },                                                        // 2: nothing changed
		func() error { return w.SetChannelProperty("g", "a", "gain", 2.0) },                                  // staged
		func() error { return w.WriteChannels(a, b) },                                                        // 3: one property
		func() error { return w.WriteChannel("g", "a", []float64{1, 2, 3}) },                                 // 4: a list that dropped b
		func() error { return w.WriteChannels(ChannelData{Group: "g", Channel: "a", Values: []float64{7}}) }, // 5: a's count changed
		func() error {
			return w.WriteChannels(ChannelData{Group: "g", Channel: "a", Values: []float64{7}}, ChannelData{Group: "g", Channel: "c", Values: []bool{true}})
		}, // 6: a channel added at the end
	}
	for i, s := range steps {
		if err := s(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	segs := segments(t, raw)
	type flags struct{ meta, list, data bool }
	want := []flags{{true, true, true}, {false, false, true}, {false, false, true}, {true, false, true}, {true, true, true}, {true, false, true}, {true, false, true}}
	if len(segs) != len(want) {
		t.Fatalf("%d segments, want %d", len(segs), len(want))
	}
	for i, s := range segs {
		got := flags{s.toc&tocMeta != 0, s.toc&tocNewList != 0, s.toc&tocRaw != 0}
		if got != want[i] {
			t.Errorf("segment %d has %+v, want %+v", i, got, want[i])
		}
		if !got.meta && s.rawOff != 0 {
			t.Errorf("segment %d has no meta data and a raw data offset of %d", i, s.rawOff)
		}
	}
	f := mustRead(t, path)
	if got := f64(t, f, "g", "a"); !reflect.DeepEqual(got, []float64{1, 2, 1, 2, 1, 2, 1, 2, 1, 2, 3, 7, 7}) {
		t.Errorf("a = %v", got)
	}
	if got := f64(t, f, "g", "b"); !reflect.DeepEqual(got, []float64{3, 4, 3, 4, 3, 4, 3, 4}) {
		t.Errorf("b = %v", got)
	}
	if got := f.Channel("g", "c").Values; !reflect.DeepEqual(got, []bool{true}) {
		t.Errorf("c = %v", got)
	}
	if v, _ := Prop(f.Channel("g", "a").Properties, "gain"); v != 2.0 {
		t.Errorf("gain = %v", v)
	}
}

// A property set and never followed by data still reaches the file.
func TestPropertiesWithoutData(t *testing.T) {
	path := tmp(t)
	w, _ := Open(path)
	_ = w.SetFileProperty("title", "empty")
	_ = w.SetGroupProperty("g", "note", "no data")
	_ = w.SetChannelProperty("g", "c", "unit_string", "V")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f := mustRead(t, path)
	if v, _ := Prop(f.Properties, "title"); v != "empty" {
		t.Errorf("title = %v", v)
	}
	if v, _ := Prop(f.Group("g").Properties, "note"); v != "no data" {
		t.Errorf("note = %v", v)
	}
	c := f.Channel("g", "c")
	if c == nil || c.Type != TypeVoid {
		t.Fatalf("channel = %+v", c)
	}
	if v, _ := Prop(c.Properties, "unit_string"); v != "V" {
		t.Errorf("unit_string = %v", v)
	}
}

// A property set between two writes, on a channel that is being written, lands
// on it, and the data stays in line.
func TestPropertyBetweenWrites(t *testing.T) {
	path := tmp(t)
	w, _ := Open(path)
	_ = w.WriteChannel("g", "a", []float64{1})
	_ = w.SetChannelProperty("g", "a", "unit_string", "mm")
	_ = w.SetChannelProperty("g", "z", "unit_string", "s") // not written yet
	_ = w.SetGroupProperty("h", "k", "v")                  // a group with no channels
	_ = w.WriteChannel("g", "a", []float64{2})
	_ = w.Close()
	f := mustRead(t, path)
	if got := f64(t, f, "g", "a"); !reflect.DeepEqual(got, []float64{1, 2}) {
		t.Errorf("a = %v", got)
	}
	if v, _ := Prop(f.Channel("g", "a").Properties, "unit_string"); v != "mm" {
		t.Errorf("unit_string = %v", v)
	}
	if v, _ := Prop(f.Channel("g", "z").Properties, "unit_string"); v != "s" {
		t.Errorf("z unit_string = %v", v)
	}
	if v, _ := Prop(f.Group("h").Properties, "k"); v != "v" {
		t.Errorf("k = %v", v)
	}
}

// writeVaried makes a file of several different segments and returns the bytes
// and, for each segment boundary, the values every channel holds up to it.
func writeVaried(t *testing.T, path string) (ends []int, want []map[string]any) {
	t.Helper()
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{}
	snap := func() {
		st, _ := os.Stat(path)
		ends = append(ends, int(st.Size()))
		cp := map[string]any{}
		for k, v := range state {
			cp[k] = v
		}
		want = append(want, cp)
	}
	add := func(g, c string, v any) {
		k := g + "/" + c
		if old, ok := state[k]; ok {
			state[k] = reflect.AppendSlice(reflect.ValueOf(old), reflect.ValueOf(v)).Interface()
		} else {
			state[k] = v
		}
	}
	write := func(d ...ChannelData) {
		t.Helper()
		if err := w.WriteChannels(d...); err != nil {
			t.Fatal(err)
		}
		for _, x := range d {
			add(x.Group, x.Channel, x.Values)
		}
		snap()
	}
	_ = w.SetFileProperty("title", "varied")
	_ = w.SetGroupProperty("a", "k", "v")
	write(ChannelData{"a", "x", []float64{1, 2, 3}}, ChannelData{"a", "y", []int32{4, 5, 6}}, ChannelData{"b", "s", []string{"one", "", "three"}})
	write(ChannelData{"a", "x", []float64{4, 5, 6}}, ChannelData{"a", "y", []int32{7, 8, 9}}, ChannelData{"b", "s", []string{"four", "five", "six"}})
	_ = w.SetChannelProperty("a", "x", "unit_string", "V")
	write(ChannelData{"a", "x", []float64{7, 8, 9}}, ChannelData{"a", "y", []int32{1, 1, 1}}, ChannelData{"b", "s", []string{"a", "b", "c"}})
	write(ChannelData{"a", "x", []float64{10}}, ChannelData{"a", "y", []int32{2}}, ChannelData{"b", "s", []string{"d"}})
	write(ChannelData{"a", "x", []float64{11, 12}}, ChannelData{"b", "s", []string{"e", "f"}})
	write(ChannelData{"b", "s", []string{"g"}}, ChannelData{"a", "x", []float64{13}}, ChannelData{"c", "t", []bool{true, false}})
	write(ChannelData{"c", "t", []bool{true}}, ChannelData{"b", "s", []string{"h"}}, ChannelData{"a", "x", []float64{14}})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return ends, want
}

func sameData(t *testing.T, f *File, want map[string]any, label string) bool {
	t.Helper()
	got := map[string]any{}
	for _, g := range f.Groups {
		for _, c := range g.Channels {
			if c.Len() > 0 {
				got[g.Name+"/"+c.Name] = c.Values
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: channels with data %d, want %d", label, len(got), len(want))
		return false
	}
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("%s: %s = %v, want %v", label, k, got[k], v)
			return false
		}
	}
	return true
}

// F3: cut the file at every byte after the first segment. The reader never
// panics, never returns half of a segment, and returns exactly the samples of
// the segments that were whole.
func TestF3TruncateAtEveryByte(t *testing.T) {
	path := tmp(t)
	ends, want := writeVaried(t, path)
	full, _ := os.ReadFile(path)
	for cut := ends[0]; cut <= len(full); cut++ {
		exp := -1
		for i, e := range ends {
			if e <= cut {
				exp = i
			}
		}
		f, err := Read(bytes.NewReader(full[:cut]), int64(cut))
		if err != nil {
			t.Fatalf("cut at %d: %v", cut, err)
		}
		if !sameData(t, f, want[exp], "cut at "+strconv.Itoa(cut)) {
			return
		}
		wantInterrupted := cut != ends[exp]
		if f.Info.Interrupted != wantInterrupted {
			t.Fatalf("cut at %d: interrupted = %v, want %v (%s)", cut, f.Info.Interrupted, wantInterrupted, f.Info.Reason)
		}
		if f.Info.GoodEnd != int64(ends[exp]) {
			t.Fatalf("cut at %d: good end = %d, want %d", cut, f.Info.GoodEnd, ends[exp])
		}
	}
}

// The same cut inside the very first segment is a file with nothing in it, not
// an error, so a writer that died on its first write can start over.
func TestTruncateInsideFirstSegment(t *testing.T) {
	path := tmp(t)
	ends, _ := writeVaried(t, path)
	full, _ := os.ReadFile(path)
	for cut := 0; cut < ends[0]; cut++ {
		f, err := Read(bytes.NewReader(full[:cut]), int64(cut))
		if err != nil {
			t.Fatalf("cut at %d: %v", cut, err)
		}
		for _, g := range f.Groups {
			for _, c := range g.Channels {
				if c.Len() != 0 {
					t.Fatalf("cut at %d returned values for %s", cut, c.Name)
				}
			}
		}
		if cut > 0 && !f.Info.Interrupted {
			t.Fatalf("cut at %d is not marked interrupted", cut)
		}
	}
}

// The writer opens a cut file, drops the broken tail and carries on, and the
// result reads as the complete part plus what was written after.
func TestAppendAfterPowerCut(t *testing.T) {
	path := tmp(t)
	ends, want := writeVaried(t, path)
	full, _ := os.ReadFile(path)
	for _, cut := range []int{ends[2] + 7, ends[3] - 1, ends[4] + leadLen + 3, len(full) - 1, ends[0] - 5, 3} {
		exp := -1
		for i, e := range ends {
			if e <= cut {
				exp = i
			}
		}
		if err := os.WriteFile(path, full[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		w, err := Open(path)
		if err != nil {
			t.Fatalf("cut at %d: %v", cut, err)
		}
		if err := w.WriteChannel("a", "x", []float64{99}); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		f := mustRead(t, path)
		if f.Info.Interrupted {
			t.Fatalf("cut at %d: still interrupted after the append: %s", cut, f.Info.Reason)
		}
		exp2 := map[string]any{}
		if exp >= 0 {
			exp2 = want[exp]
		}
		var wx []float64
		if v, ok := exp2["a/x"]; ok {
			wx = append(wx, v.([]float64)...)
		}
		wx = append(wx, 99)
		if got := f64(t, f, "a", "x"); !reflect.DeepEqual(got, wx) {
			t.Fatalf("cut at %d: x = %v, want %v", cut, got, wx)
		}
		if exp >= 0 {
			if got := f.Channel("b", "s"); got != nil && got.Len() > 0 && !reflect.DeepEqual(got.Values, exp2["b/s"]) {
				t.Fatalf("cut at %d: s = %v, want %v", cut, got.Values, exp2["b/s"])
			}
		}
	}
}

// A segment that was cut but kept whole chunks (a file written by something
// that appends chunks to one segment) keeps them, and the writer repairs the
// lead in so the file reads the same afterward.
func TestRepairKeepsCompleteChunks(t *testing.T) {
	data, ends := incrementalFile()
	cut := ends[0] + 5 // one whole chunk of segment 1 and five bytes of the next
	path := tmp(t)
	if err := os.WriteFile(path, data[:cut], 0o600); err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, path)
	if got := ints(before.Channel("group", "channel1").Values); !eq(got, []int{1, 2, 3}) {
		t.Fatalf("before = %v", got)
	}
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.WriteChannel("group", "channel1", []int32{9})
	_ = w.Close()
	after := mustRead(t, path)
	if after.Info.Interrupted {
		t.Fatal(after.Info.Reason)
	}
	if got := ints(after.Channel("group", "channel1").Values); !eq(got, []int{1, 2, 3, 9}) {
		t.Fatalf("after = %v", got)
	}
}

func TestIndexFileIsOptional(t *testing.T) {
	path := tmp(t)
	w, err := Open(path, WithIndex())
	if err != nil {
		t.Fatal(err)
	}
	_ = w.SetFileProperty("a", "b")
	for i := 0; i < 3; i++ {
		_ = w.WriteChannels(ChannelData{"g", "x", []float64{float64(i)}}, ChannelData{"g", "n", []string{"s"}})
	}
	_ = w.Close()
	raw, _ := os.ReadFile(path)
	idx, err := os.ReadFile(path + "_index")
	if err != nil {
		t.Fatal(err)
	}
	// the index is the data file without raw data, with TDSh as the tag
	var wantIdx []byte
	for _, s := range segments(t, raw) {
		h := append([]byte(nil), raw[s.start:s.start+leadLen+s.rawOff]...)
		copy(h, "TDSh")
		wantIdx = append(wantIdx, h...)
	}
	if !bytes.Equal(idx, wantIdx) {
		t.Fatalf("index is %d bytes, want %d, or differs", len(idx), len(wantIdx))
	}
	base := mustRead(t, path)

	// gone, garbage, or the index of some other file: the data reads the same
	for name, content := range map[string][]byte{"missing": nil, "garbage": []byte("not an index at all"), "stale": idx[:len(idx)/2], "empty": {}} {
		if content == nil {
			_ = os.Remove(path + "_index")
		} else {
			_ = os.WriteFile(path+"_index", content, 0o600)
		}
		got := mustRead(t, path)
		if !reflect.DeepEqual(got.Groups, base.Groups) || !reflect.DeepEqual(got.Properties, base.Properties) {
			t.Errorf("%s index changed what was read", name)
		}
	}

	// a writer that comes back rebuilds the index from the data, stale or not
	_ = os.WriteFile(path+"_index", []byte("stale"), 0o600)
	w, err = Open(path, WithIndex())
	if err != nil {
		t.Fatal(err)
	}
	_ = w.WriteChannels(ChannelData{"g", "x", []float64{7}}, ChannelData{"g", "n", []string{"t"}})
	_ = w.Close()
	raw, _ = os.ReadFile(path)
	idx, _ = os.ReadFile(path + "_index")
	wantIdx = nil
	for _, s := range segments(t, raw) {
		h := append([]byte(nil), raw[s.start:s.start+leadLen+s.rawOff]...)
		copy(h, "TDSh")
		wantIdx = append(wantIdx, h...)
	}
	if !bytes.Equal(idx, wantIdx) {
		t.Fatal("the rebuilt index differs from the data file's segments")
	}
}

func TestCorruptLeadIn(t *testing.T) {
	good := metaExampleFile()
	cases := map[string][]byte{
		"text":        []byte("this is a text file, not a measurement file at all"),
		"bad tag":     append([]byte("XDSm"), good[4:]...),
		"bad version": append(append(append([]byte(nil), good[:8]...), 0x99, 0x99, 0, 0), good[12:]...),
		"offset past": func() []byte {
			b := append([]byte(nil), good...)
			binary.LittleEndian.PutUint64(b[20:], 1<<30) // raw data offset beyond the segment
			binary.LittleEndian.PutUint64(b[12:], uint64(len(good)-leadLen))
			return b
		}(),
	}
	for name, b := range cases {
		f, err := Read(bytes.NewReader(b), int64(len(b)))
		if err == nil {
			t.Errorf("%s: no error, file %+v", name, f)
			continue
		}
		if !errors.Is(err, ErrNotTDMS) && !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v is neither ErrNotTDMS nor ErrCorrupt", name, err)
		}
	}
	// the writer will not touch a file it cannot read
	path := tmp(t)
	_ = os.WriteFile(path, cases["text"], 0o600)
	if _, err := Open(path); err == nil {
		t.Error("the writer opened a text file")
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, cases["text"]) {
		t.Error("the writer changed a text file")
	}
}

// Garbage after good segments is the end of the data, not a failure.
func TestTrailingGarbage(t *testing.T) {
	data, ends := incrementalFile()
	for _, tail := range [][]byte{make([]byte, 100), []byte("junk junk junk junk junk junk junk junk")} {
		b := append(append([]byte(nil), data...), tail...)
		f, err := Read(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		if !f.Info.Interrupted || f.Info.GoodEnd != int64(ends[len(ends)-1]) {
			t.Errorf("info = %+v", f.Info)
		}
		if got := ints(f.Channel("group", "voltage").Values); len(got) != 15 {
			t.Errorf("voltage has %d values", len(got))
		}
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	for _, ts := range []time.Time{
		time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 1, 12, 0, 0, 999999999, time.UTC), time.Date(1900, 6, 7, 8, 9, 10, 1, time.UTC),
		time.Unix(1700000000, 123456789).UTC(),
	} {
		frac, secs := encodeTime(ts)
		if got := decodeTime(frac, secs); !got.Equal(ts) {
			t.Errorf("%v came back as %v", ts, got)
		}
	}
	// the document's epoch: 1904-01-01 is second zero
	if frac, secs := encodeTime(time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)); frac != 0 || secs != 0 {
		t.Errorf("1904-01-01 is %d %d", frac, secs)
	}
	// half a second is half of 2^64
	if frac, _ := encodeTime(time.Date(1904, 1, 1, 0, 0, 0, 500000000, time.UTC)); frac != 1<<63 {
		t.Errorf("half a second is %x", frac)
	}
}

func TestPaths(t *testing.T) {
	for _, c := range []struct{ g, c string }{{"Measured Data", "Amplitude Sweep"}, {"Dr. T's Events", "Time"}, {"a/b", "c'd''e"}, {"", "x"}} {
		p := channelPath(c.g, c.c)
		k, g, ch, err := parsePath(p)
		if err != nil || k != kindChannel || g != c.g || ch != c.c {
			t.Errorf("%q -> %v %q %q %v", p, k, g, ch, err)
		}
	}
	if got := channelPath("Dr. T's Events", "Time"); got != "/'Dr. T''s Events'/'Time'" {
		t.Errorf("path = %s", got)
	}
	if k, _, _, err := parsePath("/"); err != nil || k != kindFile {
		t.Errorf("/ -> %v %v", k, err)
	}
	for _, bad := range []string{"", "x", "/'a", "/'a'/", "/'a'/'b'/'c'", "/a", "/'a'x"} {
		if _, _, _, err := parsePath(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestWriterRefusesBadInput(t *testing.T) {
	w, _ := Open(tmp(t))
	defer w.Close()
	if err := w.WriteChannel("g", "c", []complex64{1}); err == nil {
		t.Error("took complex")
	}
	if err := w.WriteChannel("", "c", []float64{1}); err == nil {
		t.Error("took an empty group name")
	}
	if err := w.WriteChannels(ChannelData{"g", "c", []float64{1}}, ChannelData{"g", "c", []float64{2}}); err == nil {
		t.Error("took one channel twice")
	}
	if err := w.SetFileProperty("p", []float64{1}); err == nil {
		t.Error("took an array property")
	}
	if err := w.WriteChannel("g", "c", []float64{1}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChannel("g", "c", []int32{1}); err == nil {
		t.Error("changed a channel's type")
	}
}

// Larger blocks, written many times, read back whole.
func TestManySegments(t *testing.T) {
	path := tmp(t)
	w, _ := Open(path)
	var want []float64
	for i := 0; i < 500; i++ {
		block := make([]float64, 100)
		for j := range block {
			block[j] = float64(i*100 + j)
		}
		want = append(want, block...)
		if err := w.WriteChannel("g", "x", block); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	f := mustRead(t, path)
	if got := f64(t, f, "g", "x"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %d values", len(got))
	}
	raw, _ := os.ReadFile(path)
	if n := len(segments(t, raw)); n != 500 {
		t.Errorf("%d segments", n)
	}
}

// Found by the fuzzer: a string channel that claims more strings than its bytes
// could hold once overflowed the offset table's size and sliced out of range.
func TestStringCountPastItsBytes(t *testing.T) {
	for _, count := range []byte{0x02, 0x7F} {
		meta := cat(hx("01 00 00 00 // one object"), pathCh1, hx(
			"1C 00 00 00 // length of the index",
			"20 00 00 00 // string", "01 00 00 00 // dimension",
		), []byte{2, 0, 0, 0, 0, 0, 0, count}, hx(
			"0B 00 00 00 00 00 00 00 // total bytes, 11",
			"00 00 00 00 // no properties",
		))
		raw := make([]byte, 11)
		lead := hx("54 44 53 6D", "0E 00 00 00", "69 12 00 00")
		lead = binary.LittleEndian.AppendUint64(lead, uint64(len(meta)+len(raw)))
		lead = binary.LittleEndian.AppendUint64(lead, uint64(len(meta)))
		data := cat(lead, meta, raw)
		if _, err := Read(bytes.NewReader(data), int64(len(data))); err == nil || !errors.Is(err, ErrCorrupt) {
			t.Errorf("count byte %#x: err = %v", count, err)
		}
	}
}
