// Package vi reads a LabVIEW VI file and says what it holds.
//
// NI does not publish the VI format. What is read here is the container (the
// RSRC file, whose layout the LabVIEW community has documented), the blocks in it
// that are plain data (the library name, the description, the version), and the
// front panel and block diagram, which are a compressed tree of tagged objects.
// The layout of that tree and the numbers of its tags and classes are from
// pylabview (MIT, see NOTICE); the block diagram is read from it as a graph of
// nodes, terminals and wires (diagram.go). Which primitive a primitive node is,
// and what a compound arithmetic node computes, is LabVIEW's own table and is not
// in the file, so those are named by number and reported as not mapped: a
// diagram rebuilt from a guess at them would run, and be wrong.
package vi

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// MaxFile is the largest VI read.
const MaxFile = 64 << 20

// Info is what a VI file says.
type Info struct {
	// Name is the file's own name where it carries one (the library it lives in is Library).
	Name        string `json:"name"`
	Library     string `json:"library,omitempty"`
	Description string `json:"description,omitempty"`
	// Version is the LabVIEW version text the file was saved by, such as "20.0".
	Version string `json:"version,omitempty"`
	// Types is how many data types the VI's type table holds: the types of its
	// terminals and of everything inside them. It is a measure of how much the VI
	// holds, not a terminal count.
	Types int `json:"types"`
	// PanelLabels are the names of the front panel's controls and indicators, in
	// the order the file holds them. Which is which is not read.
	PanelLabels []string `json:"panelLabels"`
	// SubVIs are the VIs the block diagram calls, by name.
	SubVIs []string `json:"subVIs"`
	// Texts are the other readable strings of the diagram: free labels, constants and format strings.
	Texts []string `json:"texts"`
	// Blocks are the resource names the file holds, for the record.
	Blocks []string `json:"blocks"`
	// Diagram is the block diagram as a graph: nodes, structures and wires. Nil
	// when the diagram could not be read, and Why says why.
	Diagram *Diagram `json:"diagram,omitempty"`
	Why     string   `json:"why,omitempty"`
}

// section is one entry of a block.
type section struct {
	id   uint32
	data []byte
}

// blocks reads the RSRC container into its blocks.
func blocks(b []byte) (map[string][]section, []string, error) {
	if len(b) > MaxFile {
		return nil, nil, fmt.Errorf("the file is %d MB; the limit is %d MB", len(b)>>20, MaxFile>>20)
	}
	if len(b) < 0x40 || string(b[:6]) != "RSRC\r\n" {
		return nil, nil, errors.New("this is not a LabVIEW file: it does not start with RSRC")
	}
	switch string(b[8:12]) {
	case "LVIN", "LVAR", "LVCC":
	default:
		return nil, nil, fmt.Errorf("this is a LabVIEW resource file of type %q; only VIs (LVIN) are read", string(b[8:12]))
	}
	be := binary.BigEndian
	infoOff, infoSize, dataOff, dataSize := be.Uint32(b[0x10:]), be.Uint32(b[0x14:]), be.Uint32(b[0x18:]), be.Uint32(b[0x1c:])
	if uint64(infoOff)+uint64(infoSize) > uint64(len(b)) || uint64(dataOff)+uint64(dataSize) > uint64(len(b)) || infoSize < 0x40 {
		return nil, nil, errors.New("the file is cut short or its table is damaged")
	}
	info := b[infoOff : infoOff+infoSize]
	// After a copy of the header: five whole numbers, then the count of blocks less one.
	if len(info) < 0x20+24 {
		return nil, nil, errors.New("the block table is too short")
	}
	n := int(be.Uint32(info[0x20+20:])) + 1
	if n < 1 || n > 4096 || 0x20+24+12*n > len(info) {
		return nil, nil, fmt.Errorf("the block table says %d blocks, which does not fit the file", n)
	}
	out := map[string][]section{}
	var order []string
	for i := 0; i < n; i++ {
		e := info[0x20+24+12*i:]
		name, cnt, off := string(e[:4]), int(be.Uint32(e[4:]))+1, int(be.Uint32(e[8:]))
		table := 0x34 + off
		if cnt < 1 || cnt > 65536 || table+20*cnt > len(info) {
			return nil, nil, fmt.Errorf("block %q points outside the file", name)
		}
		for k := 0; k < cnt; k++ {
			s := info[table+20*k:]
			start := uint64(dataOff) + uint64(be.Uint32(s[12:]))
			if start+4 > uint64(len(b)) {
				return nil, nil, fmt.Errorf("block %q points outside the file", name)
			}
			size := uint64(be.Uint32(b[start:]))
			if start+4+size > uint64(len(b)) {
				return nil, nil, fmt.Errorf("block %q is cut short", name)
			}
			out[name] = append(out[name], section{id: be.Uint32(s[0:]), data: b[start+4 : start+4+size]})
		}
		order = append(order, name)
	}
	return out, order, nil
}

// heap is a front panel or block diagram heap, inflated: the block is a length,
// then zlib data that inflates to a length and the tag stream.
func heap(s []section) ([]byte, error) {
	if len(s) == 0 || len(s[0].data) < 6 {
		return nil, nil
	}
	r, err := zlib.NewReader(bytes.NewReader(s[0].data[4:]))
	if err != nil {
		return nil, fmt.Errorf("a heap does not inflate: %v", err)
	}
	defer r.Close()
	u, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("a heap does not inflate: %v", err)
	}
	if len(u) < 4 {
		return nil, nil
	}
	return u[4:], nil
}

var (
	printable = regexp.MustCompile(`[\x20-\x7e]{3,}`)
	formatStr = regexp.MustCompile(`^t?%[.0-9]*[a-zA-Z]$`)
)

// words are the readable strings of a heap. LabVIEW stores a name as a length
// byte and the text, so a name is often preceded by one byte that happens to be
// printable; those are trimmed.
func words(h []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range printable.FindAll(h, -1) {
		s := string(m)
		if formatStr.MatchString(s) || !looksLikeText(s) {
			continue
		}
		if s == "Pane" || s == "\"Pane" {
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// looksLikeText keeps a string that reads as words: mostly letters, digits and
// spaces, with a letter to start it. Bytes of the heap's own structure that
// happen to be printable do not pass.
func looksLikeText(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return false
	}
	letters, other := 0, 0
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '.', r == '_', r == '-', r == '(', r == ')', r == ',':
			letters++
		default:
			other++
		}
	}
	vowel := strings.ContainsAny(s, "aeiouAEIOU")
	return other*4 <= letters && vowel && letters >= 3
}

func pascal32(b []byte) string {
	if len(b) >= 4 {
		n := int(binary.BigEndian.Uint32(b))
		if n >= 0 && 4+n <= len(b) {
			return string(b[4 : 4+n])
		}
	}
	return ""
}

// Read reads a VI file. name is the file's name, used when the VI carries none.
func Read(b []byte, name string) (*Info, error) {
	bl, order, err := blocks(b)
	if err != nil {
		return nil, err
	}
	in := &Info{Name: name, Blocks: order, PanelLabels: []string{}, SubVIs: []string{}, Texts: []string{}}
	if s := bl["LIBN"]; len(s) > 0 && len(s[0].data) > 5 {
		d := s[0].data
		if l := int(d[4]); 5+l <= len(d) {
			in.Library = string(d[5 : 5+l])
		}
	}
	if s := bl["STRG"]; len(s) > 0 {
		in.Description = strings.TrimSpace(pascal32(s[0].data))
	}
	if s := bl["vers"]; len(s) > 0 {
		d := s[0].data
		if len(d) >= 8 {
			if l := int(d[6]); 7+l <= len(d) {
				in.Version = string(d[7 : 7+l])
			}
		}
	}
	if s := bl["VCTP"]; len(s) > 0 {
		in.Types = typeCount(s[0].data)
	}
	fp, err := heap(bl["FPHb"])
	if err != nil {
		return in, err
	}
	bd, err := heap(bl["BDHb"])
	if err != nil {
		return in, err
	}
	in.Diagram, in.Why = diagramOf(fp, bd, bl)
	for _, w := range words(fp) {
		in.PanelLabels = append(in.PanelLabels, clean(w))
	}
	for _, w := range words(bd) {
		w = clean(w)
		if strings.HasSuffix(strings.ToLower(w), ".vi") || strings.HasSuffix(strings.ToLower(w), ".vim") || strings.HasSuffix(strings.ToLower(w), ".ctl") {
			in.SubVIs = append(in.SubVIs, w)
		} else {
			in.Texts = append(in.Texts, w)
		}
	}
	return in, nil
}

// clean drops the one leading byte of a name that was a length or a flag and
// came out printable.
func clean(w string) string {
	if len(w) > 3 {
		c := w[0]
		isAlnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !isAlnum && c != ' ' && c != '(' {
			return clean(w[1:])
		}
	}
	return w
}

// typeCount is the type table's own count of its entries.
func typeCount(d []byte) int {
	if len(d) < 4 {
		return 0
	}
	return int(binary.BigEndian.Uint32(d))
}

// Unmapped is what a VI holds that a FlowLab diagram does not take from it, and why.
func (in *Info) Unmapped() []string {
	var u []string
	if in.Diagram == nil {
		why := in.Why
		if why == "" {
			why = "it is not in the file"
		}
		u = append(u, "the block diagram's wires, primitives and structures: "+why)
	} else {
		_, d := in.diagramTabs("x")
		u = append(u, d...)
		u = append(u,
			"the front panel's default values, ranges and number formats: only each control's name, whether it is a control or an indicator, and its kind (number, boolean, text) come across",
			"the settings inside the nodes (a compound arithmetic node's operation, a structure's case selector values, a loop's conditions)")
	}
	u = append(u,
		"the front panel's layout, fonts, colors and decorations",
		"the icon and the connector pane's terminal pattern",
	)
	if len(in.SubVIs) > 0 && in.Diagram == nil {
		u = append(u, fmt.Sprintf("the %d VI(s) the diagram calls (%s): a call is named here and its contents are another file", len(in.SubVIs), strings.Join(in.SubVIs, ", ")))
	}
	return u
}

// Flows is the flow file for an import: one flow that says what was read, in a
// comment node a person sees on the canvas, so a VI that came in is never
// mistaken for a diagram that came in. It is a starting page to draw the
// FlowLab diagram on, with the VI's own names beside it.
func (in *Info) Flows() []map[string]any {
	id := "vi-" + slug(in.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "Read from the LabVIEW VI %s", in.Name)
	if in.Version != "" {
		fmt.Fprintf(&b, " (saved by LabVIEW %s)", in.Version)
	}
	b.WriteString(".\n")
	if in.Description != "" {
		b.WriteString("\n" + in.Description + "\n")
	}
	if len(in.PanelLabels) > 0 {
		b.WriteString("\nFront panel names: " + strings.Join(in.PanelLabels, ", ") + "\n")
	}
	if len(in.SubVIs) > 0 {
		b.WriteString("\nCalls: " + strings.Join(in.SubVIs, ", ") + "\n")
	}
	b.WriteString("\nNot imported:\n")
	for _, u := range in.Unmapped() {
		b.WriteString("- " + u + "\n")
	}
	flows := []map[string]any{
		{"id": id, "type": "tab", "label": clipLabel(strings.TrimSuffix(in.Name, ".vi")), "disabled": false, "info": b.String()},
		{"id": id + "-note", "type": "comment", "z": id, "name": "From LabVIEW: " + clipLabel(in.Name), "info": b.String(), "x": 200, "y": 60, "wires": []any{}},
	}
	if in.Diagram != nil {
		tabs, _ := in.diagramTabs(id + "-bd")
		flows = append(flows, tabs...)
	}
	return flows
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(strings.TrimSuffix(s, ".vi")), "-"), "-")
	if s == "" {
		return "vi"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func clipLabel(s string) string {
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

// diagramOf reads the block diagram's graph from the inflated heaps.
func diagramOf(fp, bd []byte, bl map[string][]section) (*Diagram, string) {
	if len(bd) == 0 {
		return nil, "the VI has no block diagram heap"
	}
	bt, err := parseHeap(bd)
	if err != nil {
		return nil, "the block diagram heap does not read: " + err.Error()
	}
	var ft *hnode = &hnode{}
	if len(fp) > 0 {
		if ft, err = parseHeap(fp); err != nil {
			ft = &hnode{}
		}
	}
	d, _, err := readDiagram(bt, ft, readTypes(bl["VCTP"], bl["DTHP"]))
	if err != nil {
		return nil, err.Error()
	}
	return d, ""
}
