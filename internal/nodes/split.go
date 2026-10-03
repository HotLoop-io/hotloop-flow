// Ported from Node-RED 5.0.7 @node-red/nodes core/sequence/17-split.js
// (SplitNode) (Apache-2.0), Copyright JS Foundation and other contributors,
// http://js.foundation. Modified.

package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Split, ported from Node-RED 5's 17-split.js so a sequence comes out with the
// same msg.parts a Node-RED Join, Sort or Batch expects: the separator in
// parts.ch so a Join can put it back, the previous parts pushed onto a stack
// under parts.parts when a sequence is split again, and parts.property when
// the split wasn't of payload.
//
// Streaming mode is for data that arrives in pieces, a serial port or a TCP
// socket handing over whatever bytes it has. A delimited line, or a fixed
// length record, can straddle two messages, so the unfinished end of one
// message is carried over and finished by the next, and the index keeps
// counting up across them. A stream has no end, so its messages carry no
// count.
//
// Three differences from Node-RED, each on purpose. An object is split in
// sorted key order, because a Go map has no order and a sequence that came out
// differently every run would be worse. A string is split by length in
// characters, not UTF-16 code units, so a degree sign is never cut in half. And
// a payload Split can't split is an error a Catch node sees, instead of being
// dropped without a word.

type splitNode struct {
	property  string
	spltType  string // str, bin, len
	delim     string // str
	delimBin  []byte // bin
	delimRaw  []any  // bin, as configured, which is what parts.ch carries
	length    int    // len
	arraySplt int
	stream    bool
	addname   string

	// Streaming state. The runtime hands a node one message at a time, so
	// none of this needs a lock.
	c         int
	remainder string
	buffer    []byte
}

func registerSplit() {
	node.MustRegister(node.Descriptor{
		Type:         "split",
		Category:     node.CategorySequence,
		Color:        colorSequence,
		Icon:         "split",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "split",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Splits arrays, objects, strings and buffers, by delimiter, by a byte " +
				"sequence or by length, including streaming mode, which carries an unfinished " +
				"piece over to the next message. msg.parts matches Node-RED's, separator and " +
				"nested sequences included. Three deliberate differences: an object is split " +
				"in sorted key order, because Go maps have none; a string is split by length " +
				"in characters rather than UTF-16 code units, so no character is cut in half; " +
				"and a payload that cannot be split raises an error instead of vanishing.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropString, Label: "Split", Default: "payload"},
			{Name: "splt", Kind: node.PropString, Label: "Split using", Default: `\n`,
				Help: "For strings and buffers: the delimiter, a JSON array of byte values, or a length."},
			{Name: "spltType", Kind: node.PropSelect, Label: "Split type", Default: "str",
				Options: []node.Option{
					{Value: "str", Label: "Delimiter"},
					{Value: "bin", Label: "Byte sequence"},
					{Value: "len", Label: "Fixed length"},
				}},
			{Name: "stream", Kind: node.PropBool, Label: "Handle as a stream of messages"},
			{Name: "arraySplt", Kind: node.PropNumber, Label: "Array chunk size", Default: 1},
			{Name: "addname", Kind: node.PropString, Label: "Copy the key to",
				Help: "For objects: a message property to copy each key to. Leave empty for none."},
		},
		Help: "Splits a message into a sequence of messages: one per array element, " +
			"one per object key, or one per delimited chunk of a string or buffer.",
	}, newSplit)
}

func newSplit(def *node.Definition) (node.Node, error) {
	n := &splitNode{
		property: orDefault(def.Node.PropString("property", ""), engine.PropPayload),
		spltType: orDefault(def.Node.PropString("spltType", ""), "str"),
		stream:   def.Node.PropBool("stream", false),
		addname:  def.Node.PropString("addname", ""),
	}
	splt := def.Node.PropString("splt", "")
	if raw, ok := def.Node.Raw["splt"].(float64); ok {
		splt = strconv.FormatFloat(raw, 'f', -1, 64)
	}
	switch n.spltType {
	case "str":
		if splt == "" {
			splt = `\n`
		}
		n.delim = unescapeSequenceString(splt)
	case "bin":
		if err := json.Unmarshal([]byte(splt), &n.delimRaw); err != nil || n.delimRaw == nil {
			return nil, fmt.Errorf("invalid split property: a byte sequence is a JSON array of byte values, got %q", splt)
		}
		for _, v := range n.delimRaw {
			f, ok := v.(float64)
			if !ok {
				return nil, fmt.Errorf("invalid split property: %v is not a byte value", v)
			}
			n.delimBin = append(n.delimBin, byte(int(f)))
		}
	case "len":
		l, err := jsParseInt(splt)
		if err != nil || l < 1 {
			return nil, fmt.Errorf("invalid split property: invalid split length: %s", splt)
		}
		n.length = l
	default:
		return nil, fmt.Errorf("invalid split property: unknown split type %q", n.spltType)
	}

	n.arraySplt = 1
	if raw, present := def.Node.Raw["arraySplt"]; present && raw != nil {
		a, err := jsParseInt(fmt.Sprint(raw))
		if err != nil || a < 1 {
			return nil, fmt.Errorf("invalid split property: invalid array split length: %v", raw)
		}
		n.arraySplt = a
	}
	return n, nil
}

// unescapeSequenceString turns the escapes Node-RED's text fields store
// literally into the characters they mean, the same set Split and Join accept.
// \e is in that set and turns into a plain "e", because that is what "\e"
// means in a JavaScript string, and a flow written against that is relying on
// it.
func unescapeSequenceString(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\e`, "e", `\f`, "\f", `\0`, "\x00").Replace(s)
}

// jsParseInt reads a leading integer the way JavaScript's parseInt does:
// leading space allowed, trailing junk ignored.
func jsParseInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	digits := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == digits {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return strconv.Atoi(s[:end])
}

func (n *splitNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	value, ok, err := m.Get(n.property)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no %s to split", n.property)
	}

	// A message that is already part of a sequence keeps that sequence on a
	// stack, so a Join can rebuild the outer one after rebuilding this one.
	parts := map[string]any{}
	if old, has := m.Data[engine.PropParts]; has {
		parts["parts"] = old
	}
	parts["id"] = engine.GenerateID()
	if n.property != engine.PropPayload {
		parts["property"] = n.property
	}
	m.Data[engine.PropParts] = parts
	// Every piece is a message of its own and gets an id of its own.
	delete(m.Data, engine.PropMsgID)

	send := func(v any) error {
		if err := m.Set(n.property, v); err != nil {
			return err
		}
		out.Send(0, m.Clone())
		return nil
	}

	switch t := value.(type) {
	case string:
		return n.splitString(m, parts, t, send)
	case []any:
		return n.splitArray(parts, t, send)
	case map[string]any:
		return n.splitObject(m, parts, t, send)
	case []byte:
		return n.splitBuffer(parts, t, send)
	case engine.ImmutableBytes:
		return n.splitBuffer(parts, t, send)
	default:
		return fmt.Errorf("cannot split a %s of type %T", n.property, value)
	}
}

func (n *splitNode) splitString(_ *engine.Msg, parts map[string]any, value string, send func(any) error) error {
	value = n.remainder + value
	parts["type"] = "string"

	if n.spltType == "len" {
		parts["ch"] = ""
		parts["len"] = float64(n.length)
		runes := []rune(value)
		count := (len(runes) + n.length - 1) / n.length
		if !n.stream {
			parts["count"] = float64(count)
			n.c = 0
		}
		pos := 0
		for i := 0; i < count-1; i++ {
			parts["index"] = float64(n.c)
			n.c++
			if err := send(string(runes[pos : pos+n.length])); err != nil {
				return err
			}
			pos += n.length
		}
		rest := runes[pos:]
		if !n.stream || len(rest) == n.length {
			parts["index"] = float64(n.c)
			n.c++
			n.remainder = ""
			return send(string(rest))
		}
		n.remainder = string(rest)
		return nil
	}

	var pieces []string
	if n.spltType == "bin" {
		pieces = strings.Split(value, string(n.delimBin))
		parts["ch"] = n.delimRaw
	} else {
		pieces = strings.Split(value, n.delim)
		parts["ch"] = n.delim
	}
	return n.sendPieces(parts, len(pieces), func(i int) any { return pieces[i] }, func(last int) {
		n.remainder = pieces[last]
	}, send)
}

// sendPieces is Node-RED's sendArray: every piece but the last goes out, and
// the last either goes out too and ends the sequence, or in streaming mode is
// kept to be finished by the next message.
func (n *splitNode) sendPieces(parts map[string]any, count int, piece func(int) any, keep func(int), send func(any) error) error {
	for i := 0; i < count-1; i++ {
		parts["index"] = float64(n.c)
		n.c++
		if !n.stream {
			parts["count"] = float64(count)
		}
		if err := send(piece(i)); err != nil {
			return err
		}
	}
	if n.stream {
		keep(count - 1)
		return nil
	}
	parts["index"] = float64(n.c)
	n.c++
	parts["count"] = float64(count)
	n.c = 0
	return send(piece(count - 1))
}

func (n *splitNode) splitArray(parts map[string]any, value []any, send func(any) error) error {
	parts["type"] = "array"
	count := (len(value) + n.arraySplt - 1) / n.arraySplt
	parts["count"] = float64(count)
	parts["len"] = float64(n.arraySplt)
	for i := range count {
		lo := i * n.arraySplt
		hi := min(lo+n.arraySplt, len(value))
		var chunk any = append([]any(nil), value[lo:hi]...)
		if n.arraySplt == 1 {
			chunk = value[lo]
		}
		parts["index"] = float64(i)
		if err := send(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (n *splitNode) splitObject(m *engine.Msg, parts map[string]any, value map[string]any, send func(any) error) error {
	parts["type"] = "object"
	keys := make([]string, 0, len(value))
	for k := range value {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if n.addname != "" {
			if err := m.Set(n.addname, k); err != nil {
				return err
			}
		}
		parts["key"] = k
		parts["index"] = float64(i)
		parts["count"] = float64(len(keys))
		if err := send(value[k]); err != nil {
			return err
		}
	}
	return nil
}

func (n *splitNode) splitBuffer(parts map[string]any, value []byte, send func(any) error) error {
	buff := append(append([]byte(nil), n.buffer...), value...)
	parts["type"] = "buffer"

	if n.spltType == "len" {
		count := (len(buff) + n.length - 1) / n.length
		if !n.stream {
			parts["count"] = float64(count)
			n.c = 0
		}
		parts["len"] = float64(n.length)
		pos := 0
		for i := 0; i < count-1; i++ {
			parts["index"] = float64(n.c)
			n.c++
			if err := send(append([]byte(nil), buff[pos:pos+n.length]...)); err != nil {
				return err
			}
			pos += n.length
		}
		rest := buff[pos:]
		if !n.stream || len(rest) == n.length {
			parts["index"] = float64(n.c)
			n.c++
			n.buffer = nil
			return send(append([]byte(nil), rest...))
		}
		n.buffer = append([]byte(nil), rest...)
		return nil
	}

	delim := []byte(n.delim)
	if n.spltType == "bin" {
		delim = n.delimBin
		parts["ch"] = n.delimRaw
	} else {
		parts["ch"] = n.delim
	}
	pieces := bytes.Split(buff, delim)
	// Node-RED counts a trailing empty piece and then never sends it, so a
	// buffer ending in its delimiter makes a sequence a Join waits on forever.
	// The count here is the number of pieces that actually go out.
	sendLast := len(pieces[len(pieces)-1]) > 0
	if !n.stream {
		count := len(pieces) - 1
		if sendLast {
			count++
		}
		parts["count"] = float64(count)
		n.c = 0
	}
	for _, p := range pieces[:len(pieces)-1] {
		parts["index"] = float64(n.c)
		n.c++
		if err := send(append([]byte(nil), p...)); err != nil {
			return err
		}
	}
	last := pieces[len(pieces)-1]
	if !n.stream && sendLast {
		parts["index"] = float64(n.c)
		n.c++
		n.buffer = nil
		return send(append([]byte(nil), last...))
	}
	n.buffer = append([]byte(nil), last...)
	if !n.stream {
		n.buffer = nil
	}
	return nil
}
