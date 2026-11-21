// The tagged tree a front panel or block diagram is stored as.
//
// The stream layout and the numbers of the tags and classes used here are from
// pylabview (https://github.com/mefistotelis/pylabview, MIT, Copyright (C) 2013
// Jessica Creighton, Copyright (C) 2019-2020 Mefistotelis), commit 5f20e23de6a3.
// This file is a Go reader written for FlowLab; see NOTICE.

package vi

import (
	"errors"
	"fmt"
)

// Tags of the heap that this reader looks at. A tag number means a field name
// only inside the object class that holds it, and these are the ones of the
// general table that block diagram objects use.
const (
	tagText         = 3   // OF__text, inside a textHair
	tagClump        = 27  // OF__clumpNum
	tagDCO          = 48  // OF__dco
	tagDiagramList  = 63  // OF__diagramList
	tagDCOAgg       = 49  // OF__dcoAgg
	tagDDO          = 52  // OF__ddo
	tagFirstNodeIdx = 83  // OF__firstNodeIdx
	tagLabel        = 127 // OF__label
	tagNodeList     = 164 // OF__nodeList
	tagObjFlags     = 172 // OF__objFlags
	tagParamIdx     = 189 // OF__paramIdx
	tagPrimIndex    = 202 // OF__primIndex
	tagPrimResID    = 203 // OF__primResID
	tagRoot         = 214 // OF__root
	tagSignalList   = 233 // OF__signalList
	tagTermList     = 268 // OF__termList
	tagTunnelList   = 280 // OF__tunnelList
	tagTextRec      = 270 // OF__textRec
	tagTypeDesc     = 283 // OF__typeDesc
	tagZPlaneList   = 303 // OF__zPlaneList
	tagConstValue   = 589 // OF__ConstValue
)

// System tags and attributes.
const (
	sysObject      = -3
	sysArray       = -4
	sysReference   = -5
	sysArrayElem   = -6
	sysRootObject  = -7
	attrClass      = -2
	attrUID        = -3
	scopeOpen      = 0
	scopeLeaf      = 1
	scopeClose     = 2
	classTextHair  = 1
	classLabel     = 10
	classBDConst   = 19
	classFPDCO     = 18
	classTerm      = 21
	classFPTerm    = 22
	classSignal    = 23
	classDiag      = 27
	classSRN       = 29
	classForLoop   = 32
	classWhileLoop = 33
	classPrim      = 47
	classIUse      = 49
	classCpdArith  = 108
)

// hnode is one tag of the heap.
type hnode struct {
	tag     int32
	attrs   map[int32]int32
	content []byte
	// leaf is true for a tag that holds a value and no children.
	leaf bool
	kids []*hnode
	up   *hnode
}

// class is the object class the tag carries, or -1 for none.
func (n *hnode) class() int32 {
	if c, ok := n.attrs[attrClass]; ok {
		return c
	}
	return -1
}

// uid is the object number, or -1.
func (n *hnode) uid() int32 {
	if c, ok := n.attrs[attrUID]; ok {
		return c
	}
	return -1
}

// field is the first child with this tag number.
func (n *hnode) field(tag int32) *hnode {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.tag == tag {
			return k
		}
	}
	return nil
}

// int is a leaf's value as a big-endian signed number.
func (n *hnode) int() (int64, bool) {
	if n == nil || len(n.content) == 0 || len(n.content) > 8 {
		return 0, false
	}
	v := int64(int8(n.content[0]))
	for _, b := range n.content[1:] {
		v = v<<8 | int64(b)
	}
	return v, true
}

type reader struct {
	b   []byte
	pos int
}

func (r *reader) need(n int) error {
	if r.pos+n > len(r.b) {
		return errors.New("the heap is cut short")
	}
	return nil
}

func (r *reader) u8() (int, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := int(r.b[r.pos])
	r.pos++
	return v, nil
}

func (r *reader) be(n int) (int64, error) {
	if err := r.need(n); err != nil {
		return 0, err
	}
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(r.b[r.pos+i])
	}
	r.pos += n
	return int64(v), nil
}

// u124 is a number of 8, 8+16 or 8+32 bits, by the first byte.
func (r *reader) u124() (int64, error) {
	v, err := r.u8()
	if err != nil {
		return 0, err
	}
	switch v {
	case 255:
		return r.be(2)
	case 254:
		return r.be(4)
	}
	return int64(v), nil
}

// s124 is a signed number of 8, 8+16 or 8+32 bits.
func (r *reader) s124() (int64, error) {
	v, err := r.u8()
	if err != nil {
		return 0, err
	}
	switch int8(v) {
	case -128:
		x, err := r.be(2)
		return int64(int16(x)), err
	case -127:
		x, err := r.be(4)
		return int64(int32(x)), err
	}
	return int64(int8(v)), nil
}

// s24 is a signed 16 bit number, or 16 bits of -32768 and then 32 bits.
func (r *reader) s24() (int64, error) {
	x, err := r.be(2)
	if err != nil {
		return 0, err
	}
	if int16(x) == -0x8000 {
		y, err := r.be(4)
		return int64(int32(y)), err
	}
	return int64(int16(x)), err
}

// maxNodes bounds a heap, so a damaged file cannot make the reader allocate without end.
const maxNodes = 4_000_000

// parseHeap reads the tag stream of a heap into a tree. The root is a stand-in
// whose children are the heap's top tags.
func parseHeap(b []byte) (*hnode, error) {
	root := &hnode{tag: 0}
	cur := root
	r := &reader{b: b}
	count := 0
	for r.pos < len(b) {
		count++
		if count > maxNodes {
			return nil, errors.New("the heap has more tags than a VI can")
		}
		c0, err := r.u8()
		if err != nil {
			return nil, err
		}
		c1, err := r.u8()
		if err != nil {
			return nil, err
		}
		sizeSpec, hasAttr, scope := (c0>>5)&7, (c0>>4)&1, (c0>>2)&3
		raw := c1 | (c0&3)<<8
		var tag int32
		if raw == 1023 {
			v, err := r.be(4)
			if err != nil {
				return nil, err
			}
			tag = int32(v)
		} else {
			tag = int32(raw - 31)
		}
		if scope == scopeClose {
			if cur.up == nil {
				return nil, errors.New("the heap closes a tag that was not open")
			}
			cur = cur.up
			continue
		}
		n := &hnode{tag: tag, up: cur, leaf: scope == scopeLeaf}
		if hasAttr != 0 {
			cnt, err := r.u124()
			if err != nil {
				return nil, err
			}
			if cnt < 0 || cnt > 64 {
				return nil, fmt.Errorf("a tag has %d attributes", cnt)
			}
			n.attrs = map[int32]int32{}
			for i := int64(0); i < cnt; i++ {
				id, err := r.s124()
				if err != nil {
					return nil, err
				}
				v, err := r.s24()
				if err != nil {
					return nil, err
				}
				n.attrs[int32(id)] = int32(v)
			}
		}
		size := 0
		switch {
		case sizeSpec == 0 || sizeSpec == 7:
		case sizeSpec <= 4:
			size = sizeSpec
		case sizeSpec == 6:
			v, err := r.u124()
			if err != nil {
				return nil, err
			}
			size = int(v)
		default:
			return nil, fmt.Errorf("a tag has the size code %d", sizeSpec)
		}
		if size > 0 {
			if err := r.need(size); err != nil {
				return nil, err
			}
			n.content = r.b[r.pos : r.pos+size]
			r.pos += size
		}
		cur.kids = append(cur.kids, n)
		if scope == scopeOpen {
			cur = n
		}
	}
	if cur != root {
		return nil, errors.New("the heap does not close every tag")
	}
	return root, nil
}
