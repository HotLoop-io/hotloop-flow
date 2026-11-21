package vi

import (
	"bytes"
	"compress/zlib"
	"io"
)

// typeTable is the VI's list of data types and the two maps that take a heap
// type number to one of them. The layout is from pylabview (MIT, see NOTICE).
type typeTable struct {
	flat  []tdesc
	top   []int
	count int
	shift int
}

// tdesc is one type descriptor: its code (a number kind, a boolean, a string,
// an array, a cluster) and its data.
type tdesc struct {
	code int
	data []byte
}

// Type codes of a type descriptor that are read.
const (
	tdInt8    = 0x01
	tdInt16   = 0x02
	tdInt32   = 0x03
	tdInt64   = 0x04
	tdUInt8   = 0x05
	tdUInt16  = 0x06
	tdUInt32  = 0x07
	tdUInt64  = 0x08
	tdFloat32 = 0x09
	tdFloat64 = 0x0A
	tdBoolean = 0x21
	tdString  = 0x30
	tdArray   = 0x40
	tdCluster = 0x50
)

func u2p2(b []byte, pos int) (int, int, bool) {
	if pos+2 > len(b) {
		return 0, pos, false
	}
	v := int(b[pos])<<8 | int(b[pos+1])
	pos += 2
	if v&0x8000 != 0 {
		if pos+2 > len(b) {
			return 0, pos, false
		}
		v = (v&0x7fff)<<16 | int(b[pos])<<8 | int(b[pos+1])
		pos += 2
	}
	return v, pos, true
}

// readTypes reads the consolidated type table (VCTP) and the diagram's slice of it (DTHP).
func readTypes(vctp, dthp []section) *typeTable {
	t := &typeTable{}
	if len(dthp) > 0 {
		d := dthp[0].data
		if c, p, ok := u2p2(d, 0); ok {
			if s, _, ok := u2p2(d, p); ok {
				t.count, t.shift = c, s
			}
		}
	}
	if len(vctp) == 0 || len(vctp[0].data) < 8 {
		return t
	}
	zr, err := zlib.NewReader(bytes.NewReader(vctp[0].data[4:]))
	if err != nil {
		return t
	}
	defer zr.Close()
	b, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil || len(b) < 4 {
		return t
	}
	n := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	pos := 4
	for i := 0; i < n; i++ {
		l, p, ok := u2p2(b, pos)
		if !ok || l < 4 || pos+l > len(b) || p+2 > len(b) {
			return t
		}
		t.flat = append(t.flat, tdesc{code: int(b[p+1]), data: b[p+2 : pos+l]})
		pos += l
	}
	cnt, p, ok := u2p2(b, pos)
	if !ok {
		return t
	}
	for i := 0; i < cnt; i++ {
		v, np, ok := u2p2(b, p)
		if !ok {
			break
		}
		t.top = append(t.top, v)
		p = np
	}
	return t
}

// heapType is the type descriptor of a heap's type number, or nil.
func (t *typeTable) heapType(id int64) *tdesc {
	if t == nil || id < 1 || int(id) > t.count {
		return nil
	}
	i := t.shift + int(id) - 1
	if i < 0 || i >= len(t.top) || t.top[i] < 0 || t.top[i] >= len(t.flat) {
		return nil
	}
	return &t.flat[t.top[i]]
}
