// Stamps each message with the time it passed through, as a HotLoop Flow
// plugin written in TinyGo.
//
// It asks for the clock capability. Without the operator granting it the flow
// that uses it does not deploy, and a guest that somehow ran without it would
// see a stand-in clock that starts in 2022, not the plant's.
//
// Built with:
//
//	tinygo build -target=wasip1 -buildmode=c-shared -o plugin.wasm .
package main

import (
	"encoding/json"
	"time"
	"unsafe"
)

// keep holds every buffer handed across the boundary. TinyGo's collector knows
// nothing about the host holding a pointer into linear memory, so anything the
// host is about to read or has just been given stays reachable here until the
// host gives it back.
var keep = map[uintptr][]byte{}

type request struct {
	Msg    map[string]any `json:"msg"`
	Config map[string]any `json:"config"`
}

type status struct {
	Fill  string `json:"fill,omitempty"`
	Shape string `json:"shape,omitempty"`
	Text  string `json:"text,omitempty"`
}

type response struct {
	Send   [][]map[string]any `json:"send,omitempty"`
	Status *status            `json:"status,omitempty"`
	Error  string             `json:"error,omitempty"`
}

//go:wasmexport hotloop_flow_alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	buf := make([]byte, size)
	p := uintptr(unsafe.Pointer(&buf[0]))
	keep[p] = buf
	return uint32(p)
}

//go:wasmexport hotloop_flow_free
func free(ptr uint32, _ uint32) { delete(keep, uintptr(ptr)) }

//go:wasmexport hotloop_flow_process
func process(ptr uint32, length uint32) uint64 {
	in := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
	var req request
	if err := json.Unmarshal(in, &req); err != nil {
		return reply(response{Error: "could not read the request: " + err.Error()})
	}

	field, _ := req.Config["field"].(string)
	if field == "" {
		field = "stampedAt"
	}

	now := time.Now().UTC()
	req.Msg[field] = now.Format(time.RFC3339Nano)
	req.Msg[field+"Unix"] = now.UnixMilli()

	return reply(response{
		Send:   [][]map[string]any{{req.Msg}},
		Status: &status{Fill: "green", Shape: "dot", Text: now.Format("15:04:05")},
	})
}

// reply writes a response into memory the host can read and packs where it is:
// the offset in the high 32 bits, the length in the low 32.
func reply(r response) uint64 {
	out, err := json.Marshal(r)
	if err != nil {
		return 0
	}
	buf := make([]byte, len(out))
	copy(buf, out)
	p := uintptr(unsafe.Pointer(&buf[0]))
	keep[p] = buf
	return uint64(p)<<32 | uint64(len(buf))
}

func main() {}
