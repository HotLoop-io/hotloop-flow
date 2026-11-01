package tdms

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// FuzzReader feeds the reader anything. It must not panic, must not run out of
// memory on a file that claims a huge array, and what it returns must be
// consistent: every channel's values are the slice type its data type names.
func FuzzReader(f *testing.F) {
	inc, ends := incrementalFile()
	f.Add(inc)
	f.Add(inc[:ends[1]+9])
	f.Add(metaExampleFile())
	f.Add(interleavedFile())
	f.Add(bigEndianFile())
	dir := f.TempDir()
	p := filepath.Join(dir, "seed.tdms")
	w, _ := Open(p)
	_ = w.SetFileProperty("t", "x")
	_ = w.SetChannelProperty("g", "c", "when", time.Unix(1700000000, 5).UTC())
	_ = w.WriteChannels(ChannelData{"g", "c", []float64{1, 2}}, ChannelData{"g", "s", []string{"a", "bc"}}, ChannelData{"h", "b", []bool{true}})
	_ = w.WriteChannels(ChannelData{"g", "c", []float64{3}}, ChannelData{"g", "s", []string{"d"}}, ChannelData{"h", "b", []bool{false}})
	_ = w.Close()
	if b, err := os.ReadFile(p); err == nil {
		f.Add(b)
		f.Add(b[:len(b)-3])
	}
	f.Add([]byte("TDSm"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Read(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			if !errors.Is(err, ErrNotTDMS) && !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnsupported) {
				t.Fatalf("an error that is none of the three: %v", err)
			}
			if file == nil {
				return
			}
		}
		if file.Info.GoodEnd < 0 || file.Info.GoodEnd > int64(len(data)) {
			t.Fatalf("good end %d of %d", file.Info.GoodEnd, len(data))
		}
		for _, g := range file.Groups {
			for _, c := range g.Channels {
				if want := emptyValues(c.Type); want != nil && reflect.TypeOf(c.Values) != reflect.TypeOf(want) {
					t.Fatalf("%s holds %T for %s", c.Name, c.Values, c.Type)
				}
				if c.Type == TypeVoid && c.Values != nil {
					t.Fatalf("%s has values and no type", c.Name)
				}
			}
		}
		// what the reader got is what a writer can resume from
		if file.Info.Interrupted {
			if _, err := Read(bytes.NewReader(data[:file.Info.GoodEnd]), file.Info.GoodEnd); err != nil && file.Info.GoodEnd > 0 {
				t.Fatalf("the part before the break does not read on its own: %v", err)
			}
		}
	})
}

func sameFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Float64bits(a[i]) != math.Float64bits(b[i]) {
			return false
		}
	}
	return true
}

// FuzzRoundTrip writes values and properties built from the fuzzer's inputs,
// twice (so there are two segments, one with meta data and one maybe without),
// and reads back exactly what went in.
func FuzzRoundTrip(f *testing.F) {
	f.Add("Group", "Chan", "prop", "value", int64(7), 1.5, uint8(4), int64(1700000000))
	f.Add("Dr. T's Events", "Time", "it's", "", int64(-1), math.NaN(), uint8(0), int64(0))
	f.Add("a/b", "c''d", "\xff\xfe", "\x00end\x00", int64(math.MinInt64), math.Inf(1), uint8(255), int64(-5))
	f.Fuzz(func(t *testing.T, group, channel, pname, pval string, i int64, x float64, n uint8, secs int64) {
		if group == "" || channel == "" || channel == group+"2" {
			return
		}
		count := int(n) % 17
		fl := make([]float64, count)
		in := make([]int64, count)
		st := make([]string, count)
		bl := make([]bool, count)
		tm := make([]time.Time, count)
		for k := 0; k < count; k++ {
			fl[k] = x * float64(k+1)
			in[k] = i + int64(k)
			st[k] = pval[:k%(len(pval)+1)]
			bl[k] = (i+int64(k))%2 == 0
			tm[k] = time.Unix(secs%100000000000, int64(k)*7+int64(uint64(i)%1000)).UTC()
		}
		path := filepath.Join(t.TempDir(), "f.tdms")
		w, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		when := time.Unix(secs%100000000000, int64(uint64(i)%1000000000)).UTC()
		props := []any{pval, i, x, i%2 == 0, when, int32(i), uint8(i), float32(x)}
		for k, v := range props {
			name := pname + string(rune('a'+k))
			if err := w.SetFileProperty(name, v); err != nil {
				t.Fatal(err)
			}
			if err := w.SetGroupProperty(group, name, v); err != nil {
				t.Fatal(err)
			}
			if err := w.SetChannelProperty(group, channel, name, v); err != nil {
				t.Fatal(err)
			}
		}
		write := func() {
			err := w.WriteChannels(
				ChannelData{group, channel, fl},
				ChannelData{group, channel + "i", in},
				ChannelData{group, channel + "s", st},
				ChannelData{group, channel + "b", bl},
				ChannelData{group, channel + "t", tm},
			)
			if err != nil {
				t.Fatal(err)
			}
		}
		write()
		write()
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		file, err := ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if file.Info.Interrupted {
			t.Fatalf("interrupted: %s", file.Info.Reason)
		}
		g := file.Group(group)
		if g == nil {
			t.Fatalf("no group %q", group)
		}
		twice := func(v any) any {
			rv := reflect.ValueOf(v)
			return reflect.AppendSlice(reflect.AppendSlice(reflect.MakeSlice(rv.Type(), 0, rv.Len()*2), rv), rv).Interface()
		}
		if count > 0 {
			if got, _ := g.Channel(channel).Float64s(); !sameFloats(got, twice(fl).([]float64)) {
				t.Fatalf("floats %v, want %v", got, twice(fl))
			}
			for suffix, want := range map[string]any{"i": in, "s": st, "b": bl, "t": tm} {
				got := g.Channel(channel + suffix).Values
				if !reflect.DeepEqual(got, twice(want)) {
					t.Fatalf("%s: %v, want %v", suffix, got, twice(want))
				}
			}
		}
		for _, list := range [][]Property{file.Properties, g.Properties, g.Channel(channel).Properties} {
			for k, v := range props {
				got, ok := Prop(list, pname+string(rune('a'+k)))
				if !ok {
					t.Fatalf("property %d missing", k)
				}
				if fv, isf := v.(float64); isf {
					if math.Float64bits(got.(float64)) != math.Float64bits(fv) {
						t.Fatalf("property %d: %v, want %v", k, got, v)
					}
					continue
				}
				if fv, isf := v.(float32); isf {
					if math.Float32bits(got.(float32)) != math.Float32bits(fv) {
						t.Fatalf("property %d: %v, want %v", k, got, v)
					}
					continue
				}
				if !reflect.DeepEqual(got, v) {
					t.Fatalf("property %d: %v (%T), want %v (%T)", k, got, got, v, v)
				}
			}
		}
	})
}
