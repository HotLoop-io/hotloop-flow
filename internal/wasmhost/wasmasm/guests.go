package wasmasm

// The guests tests are built from. Every one implements the host's ABI: it
// exports hotloop_flow_alloc, which hands back one fixed buffer at 4096, and
// hotloop_flow_process. Responses live at respAt, well clear of the request.

const (
	reqAt  = 4096
	respAt = 60000
)

var (
	allocType   = FuncType{Params: []byte{I32}, Results: []byte{I32}}
	processType = FuncType{Params: []byte{I32, I32}, Results: []byte{I64}}
)

func base(process Func, imports []Import, extraTypes []FuncType, data []Data) Module {
	m := Module{
		Types:   append([]FuncType{allocType, processType}, extraTypes...),
		Imports: imports,
		MemMin:  1,
		Data:    data,
	}
	n := uint32(len(imports))
	m.Funcs = []Func{
		{Type: 0, Body: I32Const(reqAt)},
		process,
	}
	m.Exports = []Export{
		{Name: "hotloop_flow_alloc", Func: n},
		{Name: "hotloop_flow_process", Func: n + 1},
	}
	return m
}

// Echo answers every message with the same response JSON.
func Echo(response string) []byte {
	return base(Func{Type: 1, Body: I64Const(Pack(respAt, uint32(len(response))))}, nil, nil,
		[]Data{{Offset: respAt, Bytes: []byte(response)}}).Bytes()
}

// Spin never returns. It loops without calling anything, which is the loop only
// the runtime's own checks can stop.
func Spin() []byte {
	body := Cat([]byte{0x03, 0x40, 0x0c, 0x00, 0x0b}, I64Const(0))
	return base(Func{Type: 1, Body: body}, nil, nil, nil).Bytes()
}

// Hog grows its memory a page at a time until the host says no, then writes
// one byte past the end, which is what a guest that ignores a failed
// memory.grow does.
func Hog() []byte {
	body := Cat(
		// loop: if memory.grow(1) != -1 { continue }
		[]byte{0x03, 0x40}, I32Const(1), []byte{0x40, 0x00}, I32Const(-1), []byte{0x47, 0x0d, 0x00, 0x0b},
		// memory.size << 16, the first address past the end; store 1 there
		[]byte{0x3f, 0x00}, I32Const(16), []byte{0x74}, I32Const(1), []byte{0x36, 0x02, 0x00},
		I64Const(0),
	)
	return base(Func{Type: 1, Body: body}, nil, nil, nil).Bytes()
}

// Trap executes unreachable.
func Trap() []byte {
	return base(Func{Type: 1, Body: Cat([]byte{0x00}, I64Const(0))}, nil, nil, nil).Bytes()
}

// ImportsNothingItShould imports env.reach_out, which the host never provides.
func ImportsNothingItShould() []byte {
	imports := []Import{{Module: "env", Name: "reach_out", Type: 2}}
	return base(Func{Type: 1, Body: I64Const(0)}, imports, []FuncType{{}}, nil).Bytes()
}

// ContextSet writes valueJSON under key in the given scope (0 flow, 1 global),
// traps if the host refuses, and answers with response.
func ContextSet(scope int32, key, valueJSON, response string) []byte {
	const keyAt, valAt = 61000, 61500
	setType := FuncType{Params: []byte{I32, I32, I32, I32, I32}, Results: []byte{I32}}
	imports := []Import{{Module: "hotloop_flow", Name: "context_set", Type: 2}}
	body := Cat(
		I32Const(scope), I32Const(keyAt), I32Const(int32(len(key))),
		I32Const(valAt), I32Const(int32(len(valueJSON))), Call(0),
		[]byte{0x04, 0x40, 0x00, 0x0b}, // if the result is not 0, trap
		I64Const(Pack(respAt, uint32(len(response)))),
	)
	return base(Func{Type: 1, Body: body}, imports, []FuncType{setType}, []Data{
		{Offset: respAt, Bytes: []byte(response)},
		{Offset: keyAt, Bytes: []byte(key)},
		{Offset: valAt, Bytes: []byte(valueJSON)},
	}).Bytes()
}

// ContextGet reads key from the given scope and answers with
// {"send":[[{"payload":<the value>}]]}, trapping if the host says no.
func ContextGet(scope int32, key string) []byte {
	const prefix = `{"send":[[{"payload":`
	const suffix = `}]]}`
	const keyAt, sufAt = 61000, 62000
	getType := FuncType{Params: []byte{I32, I32, I32, I32, I32}, Results: []byte{I32}}
	imports := []Import{{Module: "hotloop_flow", Name: "context_get", Type: 2}}
	p, s := int32(len(prefix)), int32(len(suffix))
	body := Cat(
		I32Const(scope), I32Const(keyAt), I32Const(int32(len(key))),
		I32Const(respAt+p), I32Const(1000), Call(0), []byte{0x21, 0x00}, // local 0 = length
		[]byte{0x20, 0x00}, I32Const(0), []byte{0x48, 0x04, 0x40, 0x00, 0x0b}, // length < 0: trap
		// copy the suffix after the value
		I32Const(respAt+p), []byte{0x20, 0x00, 0x6a}, I32Const(sufAt), I32Const(s), []byte{0xfc, 0x0a, 0x00, 0x00},
		// (respAt << 32) | length + len(prefix) + len(suffix)
		I64Const(int64(respAt)<<32), []byte{0x20, 0x00, 0xad, 0x84}, I64Const(int64(p+s)), []byte{0x7c},
	)
	return base(Func{Type: 1, Locals: 1, Body: body}, imports, []FuncType{getType}, []Data{
		{Offset: respAt, Bytes: []byte(prefix)},
		{Offset: keyAt, Bytes: []byte(key)},
		{Offset: sufAt, Bytes: []byte(suffix)},
	}).Bytes()
}
