//go:build cgo && linux && zzmeasure

// A throwaway cgo helper for zz_measure_component_marginal_test.go. Meant to
// be deleted with it. cgo is not permitted in a _test.go file, so the two C
// calls the measurement needs live here.
package engine

/*
#cgo CFLAGS: -I${SRCDIR}/wasmtimeinc
#include <wasmtime.h>
#include <wasmtime/component/component.h>
#include <malloc.h>
#include <wasmtime/component/component.h>
*/
import "C"

// measureCompileOnce compiles wasm into the backend's component cache and
// releases the caller's reference, leaving only the cache's.
func measureCompileOnce(b *wasmtimeBackend, wasm []byte) error {
	c, err := b.acquireComponent(wasm)
	if err != nil {
		return err
	}
	C.wasmtime_component_delete(c)
	return nil
}

// measureMallocTrim asks glibc to return free heap to the OS.
func measureMallocTrim() { C.malloc_trim(0) }

// measureSerializedLen compiles wasm and returns the length of its serialized artifact.
func measureSerializedLen(b *wasmtimeBackend, wasm []byte) (int, error) {
	c, err := b.acquireComponent(wasm)
	if err != nil {
		return 0, err
	}
	defer C.wasmtime_component_delete(c)
	var vec C.wasm_byte_vec_t
	if e := C.wasmtime_component_serialize(c, &vec); e != nil {
		C.wasmtime_error_delete(e)
		return 0, errSerialize
	}
	n := int(vec.size)
	C.wasm_byte_vec_delete(&vec)
	return n, nil
}

var errSerialize = errorString("wasmtime_component_serialize failed")

type errorString string

func (e errorString) Error() string { return string(e) }
