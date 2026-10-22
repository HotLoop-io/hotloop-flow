// Package wasmasm assembles small WebAssembly modules from code, for tests.
//
// The sandbox tests need guests that misbehave in exact ways: loop forever,
// grow memory until the ceiling says no, import something the host never
// offered. A guest compiled from Go or Rust carries megabytes of runtime and
// misbehaves however its compiler decides. These are a few dozen bytes each,
// the instructions are in the test that uses them, and nothing needs a
// toolchain. Nothing outside tests imports this package.
package wasmasm

// Value types.
const (
	I32 byte = 0x7f
	I64 byte = 0x7e
)

// FuncType is a function signature.
type FuncType struct {
	Params  []byte
	Results []byte
}

// Import is a function imported from another module, by its type index.
type Import struct {
	Module, Name string
	Type         uint32
}

// Func is a function defined in the module. Body is raw instructions without
// the final end, and Locals is the number of i32 locals beyond the parameters.
type Func struct {
	Type   uint32
	Locals uint32
	Body   []byte
}

// Export names a function by its index in the function index space, imports
// first.
type Export struct {
	Name string
	Func uint32
}

// Data is bytes placed in memory when the module is instantiated.
type Data struct {
	Offset uint32
	Bytes  []byte
}

// Module is everything a guest is made of. Memory is one memory, exported as
// "memory", of MemMin pages with no declared maximum, so the host's ceiling is
// the only limit there is.
type Module struct {
	Types   []FuncType
	Imports []Import
	Funcs   []Func
	Exports []Export
	MemMin  uint32
	Data    []Data
}

func uleb(v uint32) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

// I32Const encodes i32.const, which takes a signed LEB128.
func I32Const(v int32) []byte {
	out := []byte{0x41}
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

// I64Const encodes i64.const, which takes a signed LEB128.
func I64Const(v int64) []byte {
	out := []byte{0x42}
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

// Call encodes a call to the function at index i.
func Call(i uint32) []byte { return append([]byte{0x10}, uleb(i)...) }

// Cat joins instruction sequences.
func Cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func name(s string) []byte { return append(uleb(uint32(len(s))), s...) }

func vec(items [][]byte) []byte {
	out := uleb(uint32(len(items)))
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

func section(id byte, body []byte) []byte {
	return append(append([]byte{id}, uleb(uint32(len(body)))...), body...)
}

// Bytes assembles the module.
func (m Module) Bytes() []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	var types [][]byte
	for _, t := range m.Types {
		b := []byte{0x60}
		b = append(b, uleb(uint32(len(t.Params)))...)
		b = append(b, t.Params...)
		b = append(b, uleb(uint32(len(t.Results)))...)
		b = append(b, t.Results...)
		types = append(types, b)
	}
	out = append(out, section(1, vec(types))...)

	if len(m.Imports) > 0 {
		var imps [][]byte
		for _, i := range m.Imports {
			b := append(name(i.Module), name(i.Name)...)
			b = append(b, 0x00)
			b = append(b, uleb(i.Type)...)
			imps = append(imps, b)
		}
		out = append(out, section(2, vec(imps))...)
	}

	var funcs [][]byte
	for _, f := range m.Funcs {
		funcs = append(funcs, uleb(f.Type))
	}
	out = append(out, section(3, vec(funcs))...)

	out = append(out, section(5, vec([][]byte{append([]byte{0x00}, uleb(m.MemMin)...)}))...)

	var exps [][]byte
	for _, e := range m.Exports {
		b := append(name(e.Name), 0x00)
		exps = append(exps, append(b, uleb(e.Func)...))
	}
	exps = append(exps, append(append(name("memory"), 0x02), 0x00))
	out = append(out, section(7, vec(exps))...)

	var code [][]byte
	for _, f := range m.Funcs {
		var body []byte
		if f.Locals > 0 {
			body = append(body, uleb(1)...)
			body = append(body, uleb(f.Locals)...)
			body = append(body, I32)
		} else {
			body = append(body, uleb(0)...)
		}
		body = append(body, f.Body...)
		body = append(body, 0x0b)
		code = append(code, append(uleb(uint32(len(body))), body...))
	}
	out = append(out, section(10, vec(code))...)

	if len(m.Data) > 0 {
		var data [][]byte
		for _, d := range m.Data {
			b := []byte{0x00}
			b = append(b, I32Const(int32(d.Offset))...)
			b = append(b, 0x0b)
			b = append(b, uleb(uint32(len(d.Bytes)))...)
			b = append(b, d.Bytes...)
			data = append(data, b)
		}
		out = append(out, section(11, vec(data))...)
	}
	return out
}

// Pack is the i64 a process function returns: offset high, length low.
func Pack(ptr, length uint32) int64 { return int64(uint64(ptr)<<32 | uint64(length)) }
