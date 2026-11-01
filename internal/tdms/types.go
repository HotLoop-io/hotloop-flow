// Package tdms reads and writes TDMS files, the streaming measurement format
// that test benches and analysis tools use. It follows the public description of
// the file layout (NI, "TDMS File Format Internal Structure") and nothing else.
//
// A file holds one file object, any number of groups, and any number of channels
// in each group. Every object can carry properties. Only channels carry data,
// each as a one dimensional array of one type.
//
// What it handles: little endian and big endian segments, interleaved and
// contiguous raw data, meta data that is left out of a segment when it has not
// changed, a new object list, a last segment that was cut short, and an optional
// index file. What it does not: raw data written by a data acquisition driver
// (the DAQmx kinds), scaled or compressed data, extended precision floats, fixed
// point and complex numbers. A file that needs one of those is refused with
// ErrUnsupported and the name of the thing, never half read.
package tdms

import (
	"errors"
	"fmt"
	"math/bits"
	"time"
)

// Errors a caller can test with errors.Is.
var (
	// ErrNotTDMS is a file whose first four bytes are not the segment tag.
	ErrNotTDMS = errors.New("tdms: not a TDMS file")
	// ErrCorrupt is a file that starts like TDMS and then contradicts itself.
	ErrCorrupt = errors.New("tdms: corrupt file")
	// ErrUnsupported is a file that uses a part of the format this package leaves out.
	ErrUnsupported = errors.New("tdms: unsupported")
)

// DataType is the numeric type tag the format stores beside every property value
// and every channel's raw data.
type DataType uint32

// The data types of the format.
const (
	TypeVoid       DataType = 0
	TypeI8         DataType = 1
	TypeI16        DataType = 2
	TypeI32        DataType = 3
	TypeI64        DataType = 4
	TypeU8         DataType = 5
	TypeU16        DataType = 6
	TypeU32        DataType = 7
	TypeU64        DataType = 8
	TypeF32        DataType = 9
	TypeF64        DataType = 10
	TypeF80        DataType = 11
	TypeF32Unit    DataType = 0x19
	TypeF64Unit    DataType = 0x1A
	TypeF80Unit    DataType = 0x1B
	TypeString     DataType = 0x20
	TypeBool       DataType = 0x21
	TypeTime       DataType = 0x44
	TypeFixedPoint DataType = 0x4F
	TypeComplex32  DataType = 0x08000c
	TypeComplex64  DataType = 0x10000d
	TypeDAQmx      DataType = 0xFFFFFFFF
)

func (t DataType) String() string {
	switch t {
	case TypeVoid:
		return "void"
	case TypeI8:
		return "int8"
	case TypeI16:
		return "int16"
	case TypeI32:
		return "int32"
	case TypeI64:
		return "int64"
	case TypeU8:
		return "uint8"
	case TypeU16:
		return "uint16"
	case TypeU32:
		return "uint32"
	case TypeU64:
		return "uint64"
	case TypeF32, TypeF32Unit:
		return "float32"
	case TypeF64, TypeF64Unit:
		return "float64"
	case TypeF80, TypeF80Unit:
		return "extended float"
	case TypeString:
		return "string"
	case TypeBool:
		return "bool"
	case TypeTime:
		return "timestamp"
	case TypeFixedPoint:
		return "fixed point"
	case TypeComplex32:
		return "complex single"
	case TypeComplex64:
		return "complex double"
	case TypeDAQmx:
		return "DAQmx raw data"
	}
	return fmt.Sprintf("type 0x%X", uint32(t))
}

// size is the bytes one value takes in raw data, or 0 for a type this package
// cannot lay out (strings are variable length and handled on their own).
func (t DataType) size() int {
	switch t {
	case TypeI8, TypeU8, TypeBool:
		return 1
	case TypeI16, TypeU16:
		return 2
	case TypeI32, TypeU32, TypeF32, TypeF32Unit:
		return 4
	case TypeI64, TypeU64, TypeF64, TypeF64Unit:
		return 8
	case TypeTime:
		return 16
	}
	return 0
}

// normal folds the "with unit" float types into their plain ones: the format
// says they are float channels with a unit_string property.
func (t DataType) normal() DataType {
	switch t {
	case TypeF32Unit:
		return TypeF32
	case TypeF64Unit:
		return TypeF64
	}
	return t
}

// Property is a name, a type and a value on the file, a group or a channel.
// Value is a Go value of the type: int8 to uint64, float32, float64, string,
// bool or time.Time.
type Property struct {
	Name  string
	Type  DataType
	Value any
}

// File is everything a read found.
type File struct {
	Properties []Property
	Groups     []*Group
	Info       Info
}

// Group is a named set of channels.
type Group struct {
	Name       string
	Properties []Property
	Channels   []*Channel
}

// Channel is one array of values. Values is a slice of the Go type for Type:
// []int8 through []uint64, []float32, []float64, []string, []bool or []time.Time.
// A channel with properties and no data has Type TypeVoid and a nil Values.
type Channel struct {
	Name       string
	Type       DataType
	Properties []Property
	Values     any
}

// Info says how the read went.
type Info struct {
	// Segments is how many segments were read, including a last one that was cut
	// short but still held at least one complete chunk of data.
	Segments int
	// Interrupted is true when the file ends in the middle of a segment, or
	// something after the last good segment could not be read. Everything before
	// that point is returned.
	Interrupted bool
	// Reason says what ended the read early.
	Reason string
	// GoodEnd is the byte offset where the last complete piece of data ends. A
	// writer that appends resumes here.
	GoodEnd int64
	// Size is the size of the file that was read.
	Size int64

	patchAt  int64 // start of a segment whose next segment offset needs rewriting, or -1
	patchBig bool
}

// Group returns the named group, or nil.
func (f *File) Group(name string) *Group {
	for _, g := range f.Groups {
		if g.Name == name {
			return g
		}
	}
	return nil
}

// Channel returns the named channel of the named group, or nil.
func (f *File) Channel(group, name string) *Channel {
	if g := f.Group(group); g != nil {
		return g.Channel(name)
	}
	return nil
}

// Channel returns the named channel, or nil.
func (g *Group) Channel(name string) *Channel {
	for _, c := range g.Channels {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Prop returns the named property's value.
func Prop(props []Property, name string) (any, bool) {
	for _, p := range props {
		if p.Name == name {
			return p.Value, true
		}
	}
	return nil, false
}

// Len is the number of values the channel holds.
func (c *Channel) Len() int {
	switch v := c.Values.(type) {
	case []int8:
		return len(v)
	case []int16:
		return len(v)
	case []int32:
		return len(v)
	case []int64:
		return len(v)
	case []uint8:
		return len(v)
	case []uint16:
		return len(v)
	case []uint32:
		return len(v)
	case []uint64:
		return len(v)
	case []float32:
		return len(v)
	case []float64:
		return len(v)
	case []string:
		return len(v)
	case []bool:
		return len(v)
	case []time.Time:
		return len(v)
	}
	return 0
}

// Float64s returns a numeric channel's values as float64, and false for a
// channel that is not numeric.
func (c *Channel) Float64s() ([]float64, bool) {
	var out []float64
	switch v := c.Values.(type) {
	case []float64:
		return append(out, v...), true
	case []float32:
		for _, x := range v {
			out = append(out, float64(x))
		}
	case []int8:
		for _, x := range v {
			out = append(out, float64(x))
		}
	case []int16:
		for _, x := range v {
			out = append(out, float64(x))
		}
	case []int32:
		for _, x := range v {
			out = append(out, float64(x))
		}
	case []int64:
		for _, x := range v {
			out = append(out, float64(x))
		}
	case []uint8:
		for _, x := range v {
			out = append(out, float64(x))
		}
	case []uint16:
		for _, x := range v {
			out = append(out, float64(x))
		}
	case []uint32:
		for _, x := range v {
			out = append(out, float64(x))
		}
	case []uint64:
		for _, x := range v {
			out = append(out, float64(x))
		}
	default:
		return nil, false
	}
	return out, true
}

// The format counts time from 1904-01-01 UTC in seconds plus a fraction of 2^-64
// of a second.
const epochOffset = 2082844800 // seconds from 1904-01-01 to 1970-01-01

func encodeTime(t time.Time) (frac uint64, secs int64) {
	// round the fraction up, so that turning it back into nanoseconds lands on the
	// nanosecond it came from
	frac, rem := bits.Div64(uint64(t.Nanosecond()), 0, 1_000_000_000)
	if rem != 0 {
		frac++
	}
	return frac, t.Unix() + epochOffset
}

func decodeTime(frac uint64, secs int64) time.Time {
	ns, _ := bits.Mul64(frac, 1_000_000_000)
	return time.Unix(secs-epochOffset, int64(ns)).UTC()
}
