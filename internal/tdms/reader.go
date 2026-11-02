package tdms

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

const (
	tocMeta        = 1 << 1
	tocNewList     = 1 << 2
	tocRaw         = 1 << 3
	tocInterleaved = 1 << 5
	tocBig         = 1 << 6
	tocDAQmx       = 1 << 7

	leadLen   = 28
	versionV1 = 4712
	versionV2 = 4713

	idxNone    = 0xFFFFFFFF // the object has no raw data in this segment
	idxSame    = 0x00000000 // the object's raw data index is the one it had before
	noNextSize = ^uint64(0) // a segment that was never finished
)

var tagData = []byte("TDSm")

// rawIndex says what a channel's raw data is in a segment: its type, how many
// values, and for strings the total bytes (the offset table plus the text).
type rawIndex struct {
	dtype DataType
	count uint64
	size  uint64
}

// bytes is how much of a chunk the channel takes.
func (ix rawIndex) bytes() (uint64, bool) {
	if ix.dtype == TypeString {
		return ix.size, true
	}
	hi, lo := mul64(ix.count, uint64(ix.dtype.size()))
	return lo, hi == 0
}

func mul64(a, b uint64) (hi, lo uint64) {
	if a == 0 || b == 0 {
		return 0, 0
	}
	lo = a * b
	if lo/b != a {
		return 1, 0
	}
	return 0, lo
}

type object struct {
	path  string
	kind  int
	group *Group
	ch    *Channel
	idx   *rawIndex // the latest index this channel was given
}

func (o *object) props() *[]Property {
	switch o.kind {
	case kindGroup:
		return &o.group.Properties
	case kindChannel:
		return &o.ch.Properties
	}
	return nil
}

type update struct {
	path    string
	kind    int
	group   string
	channel string
	mode    int // 0 no raw data, 1 same index as before, 2 new index
	idx     rawIndex
	props   []Property
}

type decoder struct {
	ra     io.ReaderAt
	size   int64
	f      *File
	objs   map[string]*object
	active []*object
}

// ReadFile reads the TDMS file at path. The index file beside it, if there is
// one, is not needed and not looked at.
func ReadFile(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return Read(f, st.Size())
}

// Read reads a TDMS file of the given size.
//
// A file that ends in the middle of a segment, or has something that is not a
// segment after its last good one, is not an error: Read returns every complete
// chunk of data before the break and sets Info.Interrupted. A chunk is one
// round of values for every channel in a segment, so a value from a chunk that
// was only partly written is never returned. An error is a file that is not
// TDMS (ErrNotTDMS), contradicts itself in its first segment (ErrCorrupt), or
// uses what this package leaves out (ErrUnsupported). When an error comes after
// some segments were read, the File holds them.
func Read(ra io.ReaderAt, size int64) (*File, error) {
	d := &decoder{ra: ra, size: size, f: &File{}, objs: map[string]*object{}}
	d.f.Info.Size = size
	d.f.Info.patchAt = -1
	d.objs["/"] = &object{path: "/", kind: kindFile}
	pos := int64(0)
	for pos < size {
		end, err := d.segment(pos)
		if err != nil {
			if pos == 0 {
				return nil, err
			}
			return d.f, err
		}
		if end < 0 {
			break
		}
		pos = end
	}
	return d.f, nil
}

// stop ends the read at pos. In the first segment that is an error; later it is
// the end of what can be read.
func (d *decoder) stop(pos int64, kind error, why string) (int64, error) {
	return d.stopIf(pos, kind, why, false)
}

// stopIf is stop for a break that is only a cut when soft is true: a file that
// was cut off inside its first segment is an empty file, not a bad one.
func (d *decoder) stopIf(pos int64, kind error, why string, soft bool) (int64, error) {
	if pos == 0 && !soft {
		return -1, fmt.Errorf("%w: %s", kind, why)
	}
	d.f.Info.Interrupted = true
	d.f.Info.Reason = why
	return -1, nil
}

func (d *decoder) segment(pos int64) (int64, error) {
	remaining := d.size - pos
	if remaining < leadLen {
		head := make([]byte, remaining)
		if _, err := d.ra.ReadAt(head, pos); err != nil && err != io.EOF {
			return -1, err
		}
		if pos == 0 && len(head) <= len(tagData) && bytes.HasPrefix(tagData, head) {
			d.f.Info.Interrupted = true
			d.f.Info.Reason = "the file ends inside the first lead in"
			return -1, nil
		}
		if len(head) >= len(tagData) && !bytes.Equal(head[:4], tagData) {
			return d.stop(pos, ErrNotTDMS, "no TDSm tag")
		}
		return d.stopIf(pos, ErrCorrupt, "the file ends inside a lead in", true)
	}
	lead := make([]byte, leadLen)
	if _, err := d.ra.ReadAt(lead, pos); err != nil && err != io.EOF {
		return -1, err
	}
	if !bytes.Equal(lead[:4], tagData) {
		return d.stop(pos, ErrNotTDMS, fmt.Sprintf("no TDSm tag at byte %d", pos))
	}
	toc := binary.LittleEndian.Uint32(lead[4:8])
	var bo binary.ByteOrder = binary.LittleEndian
	if toc&tocBig != 0 {
		bo = binary.BigEndian
	}
	version := bo.Uint32(lead[8:12])
	next := bo.Uint64(lead[12:20])
	rawOff := bo.Uint64(lead[20:28])
	if version != versionV1 && version != versionV2 {
		return d.stop(pos, ErrCorrupt, fmt.Sprintf("version %d at byte %d is not one this package knows", version, pos))
	}
	if toc&tocDAQmx != 0 {
		return -1, fmt.Errorf("%w: raw data written by a data acquisition driver (segment at byte %d)", ErrUnsupported, pos)
	}
	body := uint64(remaining - leadLen)
	end := d.size
	interrupted := true
	switch {
	case next == noNextSize:
		// never finished: it runs to the end of the file
	case next > body:
		// promises more than the file holds
	default:
		end = pos + leadLen + int64(next)
		interrupted = false
		if rawOff > next {
			return d.stop(pos, ErrCorrupt, fmt.Sprintf("the raw data offset passes the end of the segment at byte %d", pos))
		}
	}
	if rawOff > body {
		// the cut fell inside the meta data
		return d.stopIf(pos, ErrCorrupt, fmt.Sprintf("the file ends inside the meta data of the segment at byte %d", pos), true)
	}
	metaStart := pos + leadLen
	dataStart := metaStart + int64(rawOff)

	var ups []update
	if toc&tocMeta != 0 && rawOff > 0 {
		meta := make([]byte, rawOff)
		if _, err := d.ra.ReadAt(meta, metaStart); err != nil && err != io.EOF {
			return -1, err
		}
		var err error
		ups, err = parseMeta(meta, bo)
		if err != nil {
			if isUnsupported(err) {
				return -1, err
			}
			return d.stop(pos, ErrCorrupt, fmt.Sprintf("meta data of the segment at byte %d: %v", pos, err))
		}
	}
	p, err := d.plan(ups, toc&tocNewList != 0, toc&tocInterleaved != 0)
	if err != nil {
		if isUnsupported(err) {
			return -1, err
		}
		return d.stop(pos, ErrCorrupt, fmt.Sprintf("the segment at byte %d: %v", pos, err))
	}

	var chunk uint64
	for _, o := range p.active {
		b, ok := p.cur(o).bytes()
		if !ok || chunk+b < chunk {
			return d.stop(pos, ErrCorrupt, fmt.Sprintf("a channel in the segment at byte %d is larger than any file", pos))
		}
		chunk += b
	}
	dataLen := uint64(end - dataStart)
	var nchunks uint64
	if toc&tocRaw != 0 && chunk > 0 {
		nchunks = dataLen / chunk
	}
	if interrupted && nchunks == 0 {
		// nothing in this segment is whole, so none of it is kept, meta data included
		return d.stopIf(pos, ErrCorrupt, fmt.Sprintf("the segment at byte %d was not finished and holds no complete data", pos), true)
	}
	d.commit(p)
	if nchunks > 0 {
		buf := make([]byte, chunk)
		for i := uint64(0); i < nchunks; i++ {
			if _, err := d.ra.ReadAt(buf, dataStart+int64(i*chunk)); err != nil && err != io.EOF {
				return -1, err
			}
			if err := d.decodeChunk(buf, bo, toc&tocInterleaved != 0); err != nil {
				return -1, fmt.Errorf("%w: %v", ErrCorrupt, err)
			}
		}
	}
	d.f.Info.Segments++
	if interrupted {
		d.f.Info.Interrupted = true
		d.f.Info.Reason = fmt.Sprintf("the segment at byte %d was not finished; its %d complete chunks were read", pos, nchunks)
		d.f.Info.GoodEnd = dataStart + int64(nchunks*chunk)
		d.f.Info.patchAt = pos
		d.f.Info.patchBig = toc&tocBig != 0
		return -1, nil
	}
	d.f.Info.GoodEnd = end
	return end, nil
}

type unsupportedErr struct{ what string }

func (e *unsupportedErr) Error() string { return e.what }
func (e *unsupportedErr) Unwrap() error { return ErrUnsupported }

func isUnsupported(err error) bool {
	_, ok := err.(*unsupportedErr)
	return ok
}

func unsupported(format string, a ...any) error {
	return &unsupportedErr{what: "unsupported: " + fmt.Sprintf(format, a...)}
}

// byteReader reads the pieces of a meta data block and remembers the first
// thing that went wrong.
type byteReader struct {
	b   []byte
	o   binary.ByteOrder
	err error
}

func (r *byteReader) take(n uint64) []byte {
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.b)) {
		r.err = fmt.Errorf("meta data ends early")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *byteReader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return r.o.Uint32(b)
}

func (r *byteReader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return r.o.Uint64(b)
}

func (r *byteReader) str() string {
	n := r.u32()
	return string(r.take(uint64(n)))
}

func parseMeta(b []byte, o binary.ByteOrder) ([]update, error) {
	r := &byteReader{b: b, o: o}
	n := r.u32()
	if r.err != nil {
		return nil, r.err
	}
	// every object takes at least a path length, an index and a property count
	if uint64(n)*12 > uint64(len(r.b)) {
		return nil, fmt.Errorf("%d objects cannot fit in %d bytes", n, len(r.b))
	}
	ups := make([]update, 0, n)
	for i := uint32(0); i < n; i++ {
		var u update
		u.path = r.str()
		if r.err != nil {
			return nil, r.err
		}
		var err error
		u.kind, u.group, u.channel, err = parsePath(u.path)
		if err != nil {
			return nil, fmt.Errorf("%q is not an object path", u.path)
		}
		marker := r.u32()
		switch marker {
		case idxNone:
			u.mode = 0
		case idxSame:
			u.mode = 1
		case 0x1269, 0x126A:
			return nil, unsupported("raw data written by a data acquisition driver (%s)", u.path)
		default:
			dt := DataType(r.u32())
			dim := r.u32()
			count := r.u64()
			var size uint64
			if dt == TypeString {
				size = r.u64()
			}
			if r.err != nil {
				return nil, r.err
			}
			if dt == TypeDAQmx {
				return nil, unsupported("raw data written by a data acquisition driver (%s)", u.path)
			}
			if !channelType(dt) {
				return nil, unsupported("a %s channel (%s)", dt, u.path)
			}
			if dim != 1 {
				return nil, fmt.Errorf("%s has an array dimension of %d, and only 1 is valid", u.path, dim)
			}
			u.mode = 2
			u.idx = rawIndex{dtype: dt.normal(), count: count, size: size}
			if dt == TypeString && count > size/4 {
				return nil, fmt.Errorf("%s is %d strings in %d bytes", u.path, count, size)
			}
		}
		if u.mode != 0 && u.kind != kindChannel {
			return nil, fmt.Errorf("%s is not a channel and cannot hold raw data", u.path)
		}
		np := r.u32()
		if r.err != nil {
			return nil, r.err
		}
		if uint64(np)*8 > uint64(len(r.b)) {
			return nil, fmt.Errorf("%d properties cannot fit in %d bytes", np, len(r.b))
		}
		for j := uint32(0); j < np; j++ {
			name := r.str()
			dt := DataType(r.u32())
			if r.err != nil {
				return nil, r.err
			}
			v, err := readValue(r, dt)
			if err != nil {
				return nil, err
			}
			if r.err != nil {
				return nil, r.err
			}
			u.props = append(u.props, Property{Name: name, Type: dt.normal(), Value: v})
		}
		ups = append(ups, u)
	}
	return ups, nil
}

func channelType(dt DataType) bool {
	switch dt {
	case TypeString, TypeBool, TypeTime:
		return true
	}
	return dt.size() > 0
}

func readValue(r *byteReader, dt DataType) (any, error) {
	switch dt {
	case TypeString:
		return r.str(), nil
	case TypeBool:
		b := r.take(1)
		if b == nil {
			return nil, r.err
		}
		return b[0] != 0, nil
	case TypeTime:
		frac := r.u64()
		secs := int64(r.u64())
		return decodeTime(frac, secs), nil
	}
	if dt.size() == 0 {
		return nil, unsupported("a %s property", dt)
	}
	b := r.take(uint64(dt.size()))
	if b == nil {
		return nil, r.err
	}
	return scalar(dt, b, r.o), nil
}

// scalar reads one fixed size value.
func scalar(dt DataType, b []byte, o binary.ByteOrder) any {
	switch dt {
	case TypeI8:
		return int8(b[0])
	case TypeI16:
		return int16(o.Uint16(b))
	case TypeI32:
		return int32(o.Uint32(b))
	case TypeI64:
		return int64(o.Uint64(b))
	case TypeU8:
		return b[0]
	case TypeU16:
		return o.Uint16(b)
	case TypeU32:
		return o.Uint32(b)
	case TypeU64:
		return o.Uint64(b)
	case TypeF32, TypeF32Unit:
		return math.Float32frombits(o.Uint32(b))
	case TypeF64, TypeF64Unit:
		return math.Float64frombits(o.Uint64(b))
	case TypeBool:
		return b[0] != 0
	case TypeTime:
		return decodeTime(o.Uint64(b[:8]), int64(o.Uint64(b[8:16])))
	}
	return nil
}

// plan is what a segment will do to the file's objects, worked out without
// touching them, so a segment that turns out to be unreadable or unfinished
// leaves no mark.
type plan struct {
	ups    []update
	fresh  map[string]*object
	order  []*object
	idx    map[*object]rawIndex
	active []*object
}

func (p *plan) cur(o *object) rawIndex {
	if ix, ok := p.idx[o]; ok {
		return ix
	}
	return *o.idx
}

func (d *decoder) lookup(p *plan, u update) *object {
	if o, ok := d.objs[u.path]; ok {
		return o
	}
	if o, ok := p.fresh[u.path]; ok {
		return o
	}
	o := &object{path: u.path, kind: u.kind}
	switch u.kind {
	case kindGroup:
		o.group = &Group{Name: u.group}
	case kindChannel:
		o.ch = &Channel{Name: u.channel}
	}
	p.fresh[u.path] = o
	p.order = append(p.order, o)
	return o
}

func (d *decoder) plan(ups []update, newList, interleaved bool) (*plan, error) {
	p := &plan{ups: ups, fresh: map[string]*object{}, idx: map[*object]rawIndex{}}
	objs := make([]*object, len(ups))
	for i, u := range ups {
		o := d.lookup(p, u)
		objs[i] = o
		switch u.mode {
		case 2:
			if o.ch != nil && o.ch.Type != TypeVoid && o.ch.Type != u.idx.dtype {
				return nil, fmt.Errorf("%s was %s and is now %s, and a channel cannot change type", u.path, o.ch.Type, u.idx.dtype)
			}
			if prev, ok := p.idx[o]; ok && prev.dtype != u.idx.dtype {
				return nil, fmt.Errorf("%s is listed twice with two types", u.path)
			}
			p.idx[o] = u.idx
		case 1:
			if _, ok := p.idx[o]; !ok && o.idx == nil {
				return nil, fmt.Errorf("%s reuses an index it never had", u.path)
			}
		}
	}
	if newList {
		seen := map[*object]bool{}
		for i, u := range ups {
			if u.mode == 0 {
				continue
			}
			if seen[objs[i]] {
				return nil, fmt.Errorf("%s is listed twice", u.path)
			}
			seen[objs[i]] = true
			p.active = append(p.active, objs[i])
		}
	} else {
		p.active = append(p.active, d.active...)
		for i, u := range ups {
			o := objs[i]
			at := -1
			for j, a := range p.active {
				if a == o {
					at = j
				}
			}
			switch {
			case u.mode == 0 && at >= 0:
				p.active = append(p.active[:at], p.active[at+1:]...)
			case u.mode != 0 && at < 0:
				p.active = append(p.active, o)
			}
		}
	}
	if interleaved {
		var n uint64
		for i, o := range p.active {
			ix := p.cur(o)
			if ix.dtype.size() == 0 {
				return nil, fmt.Errorf("%s holds %s, which cannot be interleaved", o.path, ix.dtype)
			}
			if i == 0 {
				n = ix.count
			} else if ix.count != n {
				return nil, fmt.Errorf("interleaved channels must hold the same number of values")
			}
		}
	}
	return p, nil
}

func (d *decoder) ensureGroup(name string) *object {
	path := groupPath(name)
	if o, ok := d.objs[path]; ok {
		return o
	}
	o := &object{path: path, kind: kindGroup, group: &Group{Name: name}}
	d.objs[path] = o
	d.f.Groups = append(d.f.Groups, o.group)
	return o
}

func (d *decoder) commit(p *plan) {
	for _, o := range p.order {
		switch o.kind {
		case kindGroup:
			if _, ok := d.objs[o.path]; !ok {
				d.objs[o.path] = o
				d.f.Groups = append(d.f.Groups, o.group)
			}
		case kindChannel:
			g := d.ensureGroup(o.groupName())
			d.objs[o.path] = o
			g.group.Channels = append(g.group.Channels, o.ch)
		}
	}
	for _, u := range p.ups {
		o := d.objs[u.path]
		for _, pr := range u.props {
			setProp(d.propsOf(o), pr)
		}
		if u.mode == 2 {
			ix := u.idx
			o.idx = &ix
			if o.ch.Type == TypeVoid {
				o.ch.Type = ix.dtype
				o.ch.Values = emptyValues(ix.dtype)
			}
		}
	}
	d.active = p.active
}

func (o *object) groupName() string {
	_, g, _, _ := parsePath(o.path)
	return g
}

func (d *decoder) propsOf(o *object) *[]Property {
	if o.kind == kindFile {
		return &d.f.Properties
	}
	return o.props()
}

func setProp(list *[]Property, p Property) {
	for i := range *list {
		if (*list)[i].Name == p.Name {
			(*list)[i] = p
			return
		}
	}
	*list = append(*list, p)
}

func emptyValues(dt DataType) any {
	switch dt {
	case TypeI8:
		return []int8{}
	case TypeI16:
		return []int16{}
	case TypeI32:
		return []int32{}
	case TypeI64:
		return []int64{}
	case TypeU8:
		return []uint8{}
	case TypeU16:
		return []uint16{}
	case TypeU32:
		return []uint32{}
	case TypeU64:
		return []uint64{}
	case TypeF32:
		return []float32{}
	case TypeF64:
		return []float64{}
	case TypeString:
		return []string{}
	case TypeBool:
		return []bool{}
	case TypeTime:
		return []time.Time{}
	}
	return nil
}

// decodeChunk reads one chunk, laid out in the order of the active channels,
// either channel after channel or one value of each channel in turn.
func (d *decoder) decodeChunk(buf []byte, o binary.ByteOrder, interleaved bool) error {
	if interleaved {
		if len(d.active) == 0 {
			return nil
		}
		n := int(d.active[0].idx.count)
		at := 0
		for i := 0; i < n; i++ {
			for _, a := range d.active {
				sz := a.idx.dtype.size()
				appendValue(a.ch, a.idx.dtype, buf[at:at+sz], o)
				at += sz
			}
		}
		return nil
	}
	at := uint64(0)
	for _, a := range d.active {
		ix := a.idx
		b, _ := ix.bytes()
		blk := buf[at : at+b]
		at += b
		if ix.dtype == TypeString {
			if err := appendStrings(a.ch, blk, int(ix.count), o); err != nil {
				return fmt.Errorf("%s: %v", a.path, err)
			}
			continue
		}
		sz := ix.dtype.size()
		for i := uint64(0); i < ix.count; i++ {
			appendValue(a.ch, ix.dtype, blk[int(i)*sz:int(i)*sz+sz], o)
		}
	}
	return nil
}

func appendStrings(ch *Channel, blk []byte, n int, o binary.ByteOrder) error {
	if len(blk) < n*4 {
		return fmt.Errorf("string offsets run past the data")
	}
	text := blk[n*4:]
	prev := uint32(0)
	vals := ch.Values.([]string)
	for i := 0; i < n; i++ {
		end := o.Uint32(blk[i*4:])
		if end < prev || int(end) > len(text) {
			return fmt.Errorf("string %d ends at %d, outside the text", i, end)
		}
		vals = append(vals, string(text[prev:end]))
		prev = end
	}
	ch.Values = vals
	return nil
}

func appendValue(ch *Channel, dt DataType, b []byte, o binary.ByteOrder) {
	switch v := ch.Values.(type) {
	case []int8:
		ch.Values = append(v, int8(b[0]))
	case []int16:
		ch.Values = append(v, int16(o.Uint16(b)))
	case []int32:
		ch.Values = append(v, int32(o.Uint32(b)))
	case []int64:
		ch.Values = append(v, int64(o.Uint64(b)))
	case []uint8:
		ch.Values = append(v, b[0])
	case []uint16:
		ch.Values = append(v, o.Uint16(b))
	case []uint32:
		ch.Values = append(v, o.Uint32(b))
	case []uint64:
		ch.Values = append(v, o.Uint64(b))
	case []float32:
		ch.Values = append(v, math.Float32frombits(o.Uint32(b)))
	case []float64:
		ch.Values = append(v, math.Float64frombits(o.Uint64(b)))
	case []bool:
		ch.Values = append(v, b[0] != 0)
	case []time.Time:
		ch.Values = append(v, decodeTime(o.Uint64(b[:8]), int64(o.Uint64(b[8:16]))))
	}
}
