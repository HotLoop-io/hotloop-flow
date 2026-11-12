package js

import (
	_ "embed"
	"sync"

	"github.com/dop251/goja"
)

// Buffer is the one Node.js global a Node-RED function leans on for Modbus
// work: plants build a float out of two registers with Buffer.alloc and
// writeUInt32BE, or read one back with readFloatBE. It is a Uint8Array with
// Node's methods on it, written for this sandbox in plain JavaScript. It reaches
// nothing outside the runtime.
//
//go:embed buffer.js
var bufferSource string

var (
	bufferOnce    sync.Once
	bufferProgram *goja.Program
	bufferErr     error
)

// installBuffer defines Buffer on a fresh runtime.
func installBuffer(vm *goja.Runtime) {
	bufferOnce.Do(func() {
		bufferProgram, bufferErr = goja.Compile("buffer.js", bufferSource, true)
	})
	if bufferErr != nil {
		panic("hotloop-flow: compiling the Buffer shim: " + bufferErr.Error())
	}
	if _, err := vm.RunProgram(bufferProgram); err != nil {
		panic("hotloop-flow: installing Buffer: " + err.Error())
	}
}
