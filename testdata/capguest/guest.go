//go:build wasip1

// A guest for the capability tests: it reports what the clock and the random
// source give it, so a test can tell a real one from the frozen one a guest
// gets without the capability.
//
// Built with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o guest.wasm ./testdata/capguest
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
	"unsafe"
)

var buffers = map[uintptr][]byte{}

//go:wasmexport hotloop_flow_alloc
func alloc(size uint32) uint32 {
	buf := make([]byte, size)
	ptr := uintptr(unsafe.Pointer(&buf[0]))
	buffers[ptr] = buf
	return uint32(ptr)
}

//go:wasmexport hotloop_flow_free
func free(ptr uint32, _ uint32) { delete(buffers, uintptr(ptr)) }

//go:wasmexport hotloop_flow_process
func process(ptr uint32, length uint32) uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	out, _ := json.Marshal(map[string]any{"send": [][]map[string]any{{{
		"unixNano": time.Now().UnixNano(),
		"random":   hex.EncodeToString(b[:]),
	}}}})
	buf := make([]byte, len(out))
	copy(buf, out)
	p := uintptr(unsafe.Pointer(&buf[0]))
	buffers[p] = buf
	return uint64(p)<<32 | uint64(len(buf))
}

func main() {}
