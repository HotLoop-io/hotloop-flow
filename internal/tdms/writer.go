package tdms

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Writer appends measurements to a TDMS file. Every call that writes data makes
// one segment, written whole in a single write with its true length in the
// lead in. A file that is cut short by a power loss therefore shows the cut: the
// segment promises more bytes than the file holds, and a reader keeps everything
// before it.
//
// Meta data (paths, properties and raw data indexes) goes into a segment only
// when it changed. A call that writes the same channels the last call did, with
// the same number of values and no new property, writes a lead in and raw data
// and nothing else.
type Writer struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	idx     *os.File
	sync    bool
	wantIdx bool
	off     int64
	broken  error

	groups  map[string]bool     // groups the file has an object for
	chans   map[string]DataType // channels the file has, by path, with their type
	last    map[string]rawIndex // the index each channel was last written with, this session
	active  []string            // the channels the last segment of this session wrote, in order
	fileObj bool                // the file object is in the file

	pend      map[string]*pending
	pendOrder []string
}

type pending struct {
	kind  int
	props []Property
}

// Option changes how a Writer works.
type Option func(*Writer)

// WithSync makes every segment reach the disk before the call returns, so that
// a power cut loses at most the call that was running.
func WithSync() Option { return func(w *Writer) { w.sync = true } }

// WithIndex keeps a .tdms_index file beside the data file: a copy of every
// segment's lead in and meta data, without the raw data, that makes opening a
// large file fast. Nothing that reads the data needs it.
func WithIndex() Option { return func(w *Writer) { w.wantIdx = true } }

// ChannelData is the values to add to one channel. Values is a slice: []float64,
// []float32, []int8 to []int64, []uint8 to []uint64, []bool, []string or
// []time.Time.
type ChannelData struct {
	Group   string
	Channel string
	Values  any
}

// Open opens the file at path for appending, creating it when it is not there.
//
// A file that already holds TDMS data is read first. If it ends in a segment that
// was never finished, the writer keeps the complete chunks of that segment, drops
// the rest, and goes on from there. A file that is not TDMS is refused, and left
// alone.
func Open(path string, opts ...Option) (*Writer, error) {
	w := &Writer{
		path:   path,
		groups: map[string]bool{},
		chans:  map[string]DataType{},
		last:   map[string]rawIndex{},
		pend:   map[string]*pending{},
	}
	for _, o := range opts {
		o(w)
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w.f = f
	w.off = st.Size()
	if st.Size() > 0 {
		cur, err := Read(f, st.Size())
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("%s cannot be appended to: %w", path, err)
		}
		if cur.Info.Interrupted {
			if err := w.repair(cur.Info); err != nil {
				_ = f.Close()
				return nil, err
			}
		}
		w.fileObj = cur.Info.GoodEnd > 0
		for _, g := range cur.Groups {
			w.groups[g.Name] = true
			for _, c := range g.Channels {
				if c.Type != TypeVoid {
					w.chans[channelPath(g.Name, c.Name)] = c.Type
				}
			}
		}
	}
	if w.wantIdx {
		if err := w.openIndex(); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return w, nil
}

// repair drops what a reader would not return: the unfinished tail of a file.
func (w *Writer) repair(in Info) error {
	if in.patchAt >= 0 {
		var o binary.ByteOrder = binary.LittleEndian
		if in.patchBig {
			o = binary.BigEndian
		}
		var b [8]byte
		o.PutUint64(b[:], uint64(in.GoodEnd-in.patchAt-leadLen))
		if _, err := w.f.WriteAt(b[:], in.patchAt+12); err != nil {
			return err
		}
	}
	if err := w.f.Truncate(in.GoodEnd); err != nil {
		return err
	}
	w.off = in.GoodEnd
	return nil
}

// SetFileProperty sets a property of the file object. The value is written with
// the next segment. Any Go integer, float32, float64, bool, string or time.Time
// will do; an int is written as a 64 bit integer.
func (w *Writer) SetFileProperty(name string, value any) error {
	return w.setProp("/", kindFile, name, value)
}

// SetGroupProperty sets a property of a group, which is created if it is new.
func (w *Writer) SetGroupProperty(group, name string, value any) error {
	return w.setProp(groupPath(group), kindGroup, name, value)
}

// SetChannelProperty sets a property of a channel, such as unit_string or one of
// the waveform properties wf_start_time, wf_increment and wf_samples.
func (w *Writer) SetChannelProperty(group, channel, name string, value any) error {
	return w.setProp(channelPath(group, channel), kindChannel, name, value)
}

func (w *Writer) setProp(path string, kind int, name string, value any) error {
	p, err := propertyOf(name, value)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken != nil {
		return w.broken
	}
	pe, ok := w.pend[path]
	if !ok {
		pe = &pending{kind: kind}
		w.pend[path] = pe
		w.pendOrder = append(w.pendOrder, path)
	}
	setProp(&pe.props, p)
	return nil
}

func propertyOf(name string, v any) (Property, error) {
	switch x := v.(type) {
	case int:
		return Property{Name: name, Type: TypeI64, Value: int64(x)}, nil
	case int8:
		return Property{Name: name, Type: TypeI8, Value: x}, nil
	case int16:
		return Property{Name: name, Type: TypeI16, Value: x}, nil
	case int32:
		return Property{Name: name, Type: TypeI32, Value: x}, nil
	case int64:
		return Property{Name: name, Type: TypeI64, Value: x}, nil
	case uint:
		return Property{Name: name, Type: TypeU64, Value: uint64(x)}, nil
	case uint8:
		return Property{Name: name, Type: TypeU8, Value: x}, nil
	case uint16:
		return Property{Name: name, Type: TypeU16, Value: x}, nil
	case uint32:
		return Property{Name: name, Type: TypeU32, Value: x}, nil
	case uint64:
		return Property{Name: name, Type: TypeU64, Value: x}, nil
	case float32:
		return Property{Name: name, Type: TypeF32, Value: x}, nil
	case float64:
		return Property{Name: name, Type: TypeF64, Value: x}, nil
	case string:
		return Property{Name: name, Type: TypeString, Value: x}, nil
	case bool:
		return Property{Name: name, Type: TypeBool, Value: x}, nil
	case time.Time:
		return Property{Name: name, Type: TypeTime, Value: x}, nil
	}
	return Property{}, fmt.Errorf("tdms: property %q has a %T, and a property is a number, a string, a bool or a time", name, v)
}

// WriteChannel adds values to one channel as one segment.
func (w *Writer) WriteChannel(group, channel string, values any) error {
	return w.WriteChannels(ChannelData{Group: group, Channel: channel, Values: values})
}

// WriteChannels adds values to several channels as one segment. The channels'
// values stay together in the file, so a reader that sees the segment sees all
// of them, and one that does not sees none.
func (w *Writer) WriteChannels(data ...ChannelData) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken != nil {
		return w.broken
	}
	if w.f == nil {
		return fmt.Errorf("tdms: the writer is closed")
	}
	if len(data) == 0 {
		return nil
	}
	type item struct {
		path  string
		group string
		idx   rawIndex
		raw   []byte
	}
	items := make([]item, 0, len(data))
	seen := map[string]bool{}
	for _, d := range data {
		if d.Group == "" || d.Channel == "" {
			return fmt.Errorf("tdms: a channel needs a group name and a channel name")
		}
		path := channelPath(d.Group, d.Channel)
		if seen[path] {
			return fmt.Errorf("tdms: channel %s is in this write twice", path)
		}
		seen[path] = true
		dt, raw, count, size, err := encodeValues(d.Values)
		if err != nil {
			return fmt.Errorf("tdms: channel %s: %w", path, err)
		}
		if old, ok := w.chans[path]; ok && old != dt {
			return fmt.Errorf("tdms: channel %s holds %s and cannot take %s", path, old, dt)
		}
		items = append(items, item{path: path, group: d.Group, idx: rawIndex{dtype: dt, count: count, size: size}, raw: raw})
	}

	paths := make([]string, len(items))
	for i, it := range items {
		paths[i] = it.path
	}
	prefix := len(w.active) <= len(paths)
	if prefix {
		for i, p := range w.active {
			if paths[i] != p {
				prefix = false
				break
			}
		}
	}
	newList := !prefix || w.active == nil
	var meta objectList
	inBatch := map[string]bool{}
	for _, p := range paths {
		inBatch[p] = true
	}

	// the file object first, then every group the file does not have an object for
	// yet (a channel is only valid inside a group that has one), then property
	// changes on groups, then the channels
	if !w.fileObj || w.pend["/"] != nil {
		meta.add("/", idxNone, w.takeProps("/"))
	}
	var groupOrder []string
	for _, it := range items {
		groupOrder = append(groupOrder, it.group)
	}
	for _, path := range w.pendOrder {
		if pe := w.pend[path]; pe != nil && pe.kind != kindFile {
			groupOrder = append(groupOrder, groupName(path))
		}
	}
	for _, g := range groupOrder {
		if !w.groups[g] {
			w.groups[g] = true
			meta.add(groupPath(g), idxNone, w.takeProps(groupPath(g)))
		}
	}
	for _, path := range w.pendOrder {
		if pe := w.pend[path]; pe != nil && pe.kind == kindGroup {
			meta.add(path, idxNone, w.takeProps(path))
		}
	}
	for _, it := range items {
		changed := newList
		if prev, ok := w.last[it.path]; !ok || prev != it.idx || !contains(w.active, it.path) {
			changed = true
		}
		props := w.takeProps(it.path)
		switch {
		case changed:
			meta.addIndexed(it.path, it.idx, props)
		case len(props) > 0:
			meta.add(it.path, idxSame, props)
		}
	}
	for _, path := range w.pendOrder {
		if pe := w.pend[path]; pe != nil && pe.kind == kindChannel {
			// a channel that carries no data in this write: properties only
			meta.add(path, idxNone, w.takeProps(path))
		}
	}
	w.dropSpent()

	var raw bytes.Buffer
	for _, it := range items {
		raw.Write(it.raw)
	}
	toc := uint32(0)
	if newList {
		toc |= tocNewList
	}
	if meta.n > 0 {
		toc |= tocMeta
	}
	if raw.Len() > 0 {
		toc |= tocRaw
	}
	if toc&(tocMeta|tocRaw) == 0 {
		return nil
	}
	if err := w.writeSegment(toc, meta.bytes(), raw.Bytes()); err != nil {
		return err
	}
	for _, it := range items {
		w.chans[it.path] = it.idx.dtype
		w.last[it.path] = it.idx
	}
	w.active = paths
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func groupName(path string) string {
	_, g, _, _ := parsePath(path)
	return g
}

// dropSpent forgets the order of properties that have been written.
func (w *Writer) dropSpent() {
	keep := w.pendOrder[:0]
	for _, p := range w.pendOrder {
		if w.pend[p] != nil {
			keep = append(keep, p)
		}
	}
	w.pendOrder = keep
}

// takeProps hands over and forgets the properties waiting for an object.
func (w *Writer) takeProps(path string) []Property {
	pe := w.pend[path]
	if pe == nil {
		return nil
	}
	delete(w.pend, path)
	return pe.props
}

// Flush writes any properties that are still waiting, as a segment of meta data
// alone.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked()
}

func (w *Writer) flushLocked() error {
	if w.broken != nil {
		return w.broken
	}
	if w.f == nil {
		return nil
	}
	if len(w.pend) == 0 && w.fileObj {
		return nil
	}
	var meta objectList
	if !w.fileObj || w.pend["/"] != nil {
		meta.add("/", idxNone, w.takeProps("/"))
	}
	for _, path := range w.pendOrder {
		if pe := w.pend[path]; pe != nil {
			g := groupName(path)
			if !w.groups[g] {
				w.groups[g] = true
				meta.add(groupPath(g), idxNone, w.takeProps(groupPath(g)))
			}
		}
	}
	for _, path := range w.pendOrder {
		pe := w.pend[path]
		if pe == nil {
			continue
		}
		marker := uint32(idxNone)
		if pe.kind == kindChannel && contains(w.active, path) {
			marker = idxSame
		}
		meta.add(path, marker, w.takeProps(path))
	}
	w.pendOrder = nil
	if meta.n == 0 {
		return nil
	}
	return w.writeSegment(tocMeta, meta.bytes(), nil)
}

// Close writes what is waiting and closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.flushLocked()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.f = nil
	if w.idx != nil {
		if cerr := w.idx.Close(); err == nil {
			err = cerr
		}
		w.idx = nil
	}
	return err
}

func (w *Writer) writeSegment(toc uint32, meta, raw []byte) error {
	seg := make([]byte, leadLen, leadLen+len(meta)+len(raw))
	copy(seg, tagData)
	binary.LittleEndian.PutUint32(seg[4:], toc)
	binary.LittleEndian.PutUint32(seg[8:], versionV2)
	binary.LittleEndian.PutUint64(seg[12:], uint64(len(meta)+len(raw)))
	binary.LittleEndian.PutUint64(seg[20:], uint64(len(meta)))
	seg = append(seg, meta...)
	seg = append(seg, raw...)
	if _, err := w.f.WriteAt(seg, w.off); err != nil {
		// put the file back the way it was, so a retry does not stack on a half segment
		if terr := w.f.Truncate(w.off); terr != nil {
			w.broken = fmt.Errorf("tdms: writing %s failed (%v) and the file could not be cut back (%v)", w.path, err, terr)
			return w.broken
		}
		return err
	}
	if w.sync {
		if err := w.f.Sync(); err != nil {
			w.broken = err
			return err
		}
	}
	w.off += int64(len(seg))
	w.fileObj = true
	if w.idx != nil {
		head := append([]byte(nil), seg[:leadLen+len(meta)]...)
		copy(head, "TDSh")
		if _, err := w.idx.Write(head); err != nil {
			// the index is only a help; stop keeping it rather than fail the data
			_ = w.idx.Close()
			w.idx = nil
		}
	}
	return nil
}

// objectList builds the object part of a segment's meta data.
type objectList struct {
	n   uint32
	buf bytes.Buffer
}

func putU32(b *bytes.Buffer, v uint32) {
	var x [4]byte
	binary.LittleEndian.PutUint32(x[:], v)
	b.Write(x[:])
}

func putU64(b *bytes.Buffer, v uint64) {
	var x [8]byte
	binary.LittleEndian.PutUint64(x[:], v)
	b.Write(x[:])
}

func putStr(b *bytes.Buffer, s string) {
	putU32(b, uint32(len(s)))
	b.WriteString(s)
}

// add writes an object whose raw data index is a marker: none, or the same.
func (l *objectList) add(path string, marker uint32, props []Property) {
	l.n++
	putStr(&l.buf, path)
	putU32(&l.buf, marker)
	putProps(&l.buf, props)
}

// addIndexed writes a channel with its raw data index in full.
func (l *objectList) addIndexed(path string, ix rawIndex, props []Property) {
	l.n++
	putStr(&l.buf, path)
	if ix.dtype == TypeString {
		putU32(&l.buf, 0x1C)
	} else {
		putU32(&l.buf, 0x14)
	}
	putU32(&l.buf, uint32(ix.dtype))
	putU32(&l.buf, 1)
	putU64(&l.buf, ix.count)
	if ix.dtype == TypeString {
		putU64(&l.buf, ix.size)
	}
	putProps(&l.buf, props)
}

func (l *objectList) bytes() []byte {
	if l.n == 0 {
		return nil
	}
	var out bytes.Buffer
	putU32(&out, l.n)
	out.Write(l.buf.Bytes())
	return out.Bytes()
}

func putProps(b *bytes.Buffer, props []Property) {
	putU32(b, uint32(len(props)))
	for _, p := range props {
		putStr(b, p.Name)
		putU32(b, uint32(p.Type))
		switch v := p.Value.(type) {
		case string:
			putStr(b, v)
		case bool:
			if v {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
		case time.Time:
			frac, secs := encodeTime(v)
			putU64(b, frac)
			putU64(b, uint64(secs))
		default:
			putScalar(b, v)
		}
	}
}

func putScalar(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case int8:
		b.WriteByte(byte(x))
	case uint8:
		b.WriteByte(x)
	case int16:
		var t [2]byte
		binary.LittleEndian.PutUint16(t[:], uint16(x))
		b.Write(t[:])
	case uint16:
		var t [2]byte
		binary.LittleEndian.PutUint16(t[:], x)
		b.Write(t[:])
	case int32:
		putU32(b, uint32(x))
	case uint32:
		putU32(b, x)
	case int64:
		putU64(b, uint64(x))
	case uint64:
		putU64(b, x)
	case float32:
		putU32(b, math.Float32bits(x))
	case float64:
		putU64(b, math.Float64bits(x))
	}
}

// encodeValues lays a channel's values out as raw data.
func encodeValues(v any) (dt DataType, raw []byte, count, size uint64, err error) {
	var b bytes.Buffer
	switch x := v.(type) {
	case []int8:
		dt, count = TypeI8, uint64(len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []int16:
		dt, count = TypeI16, uint64(len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []int32:
		dt, count = TypeI32, uint64(len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []int64:
		dt, count = TypeI64, uint64(len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []uint8:
		dt, count = TypeU8, uint64(len(x))
		b.Write(x)
	case []uint16:
		dt, count = TypeU16, uint64(len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []uint32:
		dt, count = TypeU32, uint64(len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []uint64:
		dt, count = TypeU64, uint64(len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []float32:
		dt, count = TypeF32, uint64(len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []float64:
		dt, count = TypeF64, uint64(len(x))
		b.Grow(8 * len(x))
		for _, e := range x {
			putScalar(&b, e)
		}
	case []bool:
		dt, count = TypeBool, uint64(len(x))
		for _, e := range x {
			if e {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
		}
	case []time.Time:
		dt, count = TypeTime, uint64(len(x))
		for _, e := range x {
			frac, secs := encodeTime(e)
			putU64(&b, frac)
			putU64(&b, uint64(secs))
		}
	case []string:
		dt, count = TypeString, uint64(len(x))
		total := uint64(0)
		for _, s := range x {
			total += uint64(len(s))
		}
		if total > math.MaxUint32 {
			return 0, nil, 0, 0, fmt.Errorf("more than 4 GiB of text in one write")
		}
		end := uint32(0)
		for _, s := range x {
			end += uint32(len(s))
			putU32(&b, end)
		}
		for _, s := range x {
			b.WriteString(s)
		}
		size = uint64(b.Len())
	default:
		return 0, nil, 0, 0, fmt.Errorf("a channel takes a slice of numbers, bools, strings or times, not a %T", v)
	}
	return dt, b.Bytes(), count, size, nil
}

// openIndex starts the index file over from the data file: a copy of the lead in
// and meta data of every segment already there, with the TDSh tag. It starts over
// every time, so an index left by an earlier writer, or by something else, can
// never be stale. When it cannot be made it is left out; it only ever speeds
// reads up.
func (w *Writer) openIndex() error {
	ip := w.path + "_index"
	idx, err := os.OpenFile(ip, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o640)
	if err != nil {
		return nil
	}
	pos := int64(0)
	lead := make([]byte, leadLen)
	for pos < w.off {
		if _, err := w.f.ReadAt(lead, pos); err != nil || string(lead[:4]) != "TDSm" {
			break
		}
		var o binary.ByteOrder = binary.LittleEndian
		if binary.LittleEndian.Uint32(lead[4:])&tocBig != 0 {
			o = binary.BigEndian
		}
		next, rawOff := o.Uint64(lead[12:]), o.Uint64(lead[20:])
		if next == noNextSize || rawOff > next || pos+leadLen+int64(next) > w.off {
			break
		}
		head := make([]byte, leadLen+rawOff)
		if _, err := w.f.ReadAt(head, pos); err != nil {
			break
		}
		copy(head, "TDSh")
		if _, err := idx.Write(head); err != nil {
			break
		}
		pos += leadLen + int64(next)
	}
	if pos != w.off {
		_ = idx.Close()
		_ = os.Remove(ip)
		return nil
	}
	w.idx = idx
	return nil
}
