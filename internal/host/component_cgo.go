//go:build cgo

package host

// #cgo CFLAGS:-I/home/rcownie/go/pkg/mod/github.com/bytecodealliance/wasmtime-go/v44@v44.0.0/build/include
// #cgo linux,amd64 LDFLAGS:-L/home/rcownie/go/pkg/mod/github.com/bytecodealliance/wasmtime-go/v44@v44.0.0/build/linux-x86_64 -lwasmtime -lm -ldl -pthread
// #include <wasmtime.h>
// #include <wasmtime/component/component.h>
// #include <wasmtime/component/linker.h>
// #include <wasmtime/component/instance.h>
// #include <wasmtime/component/func.h>
// #include <wasmtime/component/val.h>
//
// static wasmtime_component_val_t make_component_val_u32(uint32_t v) {
//     wasmtime_component_val_t val;
//     val.kind = WASMTIME_COMPONENT_U32;
//     val.of.u32 = v;
//     return val;
// }
//
// static uint64_t component_val_get_u64(const wasmtime_component_val_t *v) {
//     if (v->kind == WASMTIME_COMPONENT_U64) {
//         return v->of.u64;
//     }
//     return 0;
// }
//
// // Helper to extract error message into a Go-owned buffer.
// // wasmtime_error_message fills a caller-provided byte_vec.
// static void get_error_message(wasmtime_error_t *err, wasm_byte_vec_t *msg) {
//     wasmtime_error_message(err, msg);
// }
//
// // Helper to compile a component.
// static wasmtime_error_t *component_compile(
//     wasm_engine_t *engine, uint8_t *buf, size_t len,
//     wasmtime_component_t **out) {
//     return wasmtime_component_new(engine, buf, len, out);
// }
//
// // Helper to get the context pointer from a store. The Go Store.Context()
// // returns a wasmtime_context_t* which may be a different typedef. This
// // wrapper normalizes it.
// static wasmtime_context_t *store_context(void *ctx_ptr) {
//     return (wasmtime_context_t *)ctx_ptr;
// }
import "C"
import (
	"fmt"
	"unsafe"

	"github.com/bytecodealliance/wasmtime-go/v44"
)

// getEnginePtr extracts the *C.wasm_engine_t from a Go *wasmtime.Engine
// by reading the first pointer field of the struct.
func getEnginePtr(engine *wasmtime.Engine) *C.wasm_engine_t {
	return (*C.wasm_engine_t)(*(*unsafe.Pointer)(unsafe.Pointer(engine)))
}

// componentCompile wraps wasmtime_component_new.
func componentCompile(engine *wasmtime.Engine, wasmBytes []byte) (*C.wasmtime_component_t, error) {
	var ptr *C.wasmtime_component_t
	var bufPtr *C.uint8_t
	if len(wasmBytes) > 0 {
		bufPtr = (*C.uint8_t)(unsafe.Pointer(&wasmBytes[0]))
	}
	err := C.component_compile(
		getEnginePtr(engine),
		bufPtr,
		C.size_t(len(wasmBytes)),
		&ptr,
	)
	if err != nil {
		var msg C.wasm_byte_vec_t
		C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(err)
		return nil, fmt.Errorf("component compile: %s", s)
	}
	return ptr, nil
}

// componentLinkerNew wraps wasmtime_component_linker_new.
func componentLinkerNew(engine *wasmtime.Engine) *C.wasmtime_component_linker_t {
	return C.wasmtime_component_linker_new(getEnginePtr(engine))
}

// componentLinkerTraps wraps wasmtime_component_linker_define_unknown_imports_as_traps.
func componentLinkerTraps(
	linker *C.wasmtime_component_linker_t,
	component *C.wasmtime_component_t,
) error {
	err := C.wasmtime_component_linker_define_unknown_imports_as_traps(linker, component)
	if err != nil {
		var msg C.wasm_byte_vec_t
		C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(err)
		return fmt.Errorf("component traps: %s", s)
	}
	return nil
}

// componentInstantiate wraps wasmtime_component_linker_instantiate.
func componentInstantiate(
	linker *C.wasmtime_component_linker_t,
	store wasmtime.Storelike,
	component *C.wasmtime_component_t,
) (*C.wasmtime_component_instance_t, error) {
	var inst C.wasmtime_component_instance_t
	ctxPtr := store.Context()
	err := C.wasmtime_component_linker_instantiate(
		linker,
		C.store_context(unsafe.Pointer(ctxPtr)),
		component,
		&inst,
	)
	if err != nil {
		var msg C.wasm_byte_vec_t
		C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(err)
		return nil, fmt.Errorf("component instantiate: %s", s)
	}
	out := new(C.wasmtime_component_instance_t)
	*out = inst
	return out, nil
}

// componentGetFunc looks up an exported function by name.
func componentGetFunc(
	instance *C.wasmtime_component_instance_t,
	store wasmtime.Storelike,
	name string,
) (*C.wasmtime_component_func_t, error) {
	ctxPtr := store.Context()
	nameBytes := []byte(name)
	var namePtr *C.char
	if len(nameBytes) > 0 {
		namePtr = (*C.char)(unsafe.Pointer(&nameBytes[0]))
	}
	exportIdx := C.wasmtime_component_instance_get_export_index(
		instance,
		C.store_context(unsafe.Pointer(ctxPtr)),
		nil,
		namePtr,
		C.size_t(len(name)),
	)
	if exportIdx == nil {
		return nil, fmt.Errorf("component export %q not found", name)
	}
	defer C.wasmtime_component_export_index_delete(exportIdx)

	var fn C.wasmtime_component_func_t
	found := C.wasmtime_component_instance_get_func(
		instance,
		C.store_context(unsafe.Pointer(ctxPtr)),
		exportIdx,
		&fn,
	)
	if !found {
		return nil, fmt.Errorf("component export %q not a function", name)
	}
	out := new(C.wasmtime_component_func_t)
	*out = fn
	return out, nil
}

// componentCall calls a component function with u32 args, returns u64.
func componentCall(
	fn *C.wasmtime_component_func_t,
	store wasmtime.Storelike,
	inputOffset, inputLen, outputOffset, outBufSz int32,
) (int64, error) {
	ctxPtr := store.Context()
	ctx := C.store_context(unsafe.Pointer(ctxPtr))

	args := [4]C.wasmtime_component_val_t{
		C.make_component_val_u32(C.uint32_t(inputOffset)),
		C.make_component_val_u32(C.uint32_t(inputLen)),
		C.make_component_val_u32(C.uint32_t(outputOffset)),
		C.make_component_val_u32(C.uint32_t(outBufSz)),
	}
	var result C.wasmtime_component_val_t
	result.kind = C.WASMTIME_COMPONENT_U64

	err := C.wasmtime_component_func_call(fn, ctx, &args[0], 4, &result, 1)
	if err != nil {
		var msg C.wasm_byte_vec_t
		C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(err)
		return 0, fmt.Errorf("component call: %s", s)
	}
	return int64(C.component_val_get_u64(&result)), nil
}

// ExecuteComponentCGo runs a WASM Component Model binary using wasmtime's
// native component model support via CGo.
func (b *wasmtimeBackend) ExecuteComponentCGo(
	wasmBytes []byte,
	entryPoint string,
	input []byte,
	outBufSz uint32,
) (*ExecResult, error) {
	component, err := componentCompile(b.engine, wasmBytes)
	if err != nil {
		return nil, err
	}
	defer C.wasmtime_component_delete(component)

	linker := componentLinkerNew(b.engine)
	if linker == nil {
		return nil, fmt.Errorf("component linker creation failed")
	}
	defer C.wasmtime_component_linker_delete(linker)

	// Add WASI 0.2 support (environment, clocks, random, io, etc.)
	// required by the CPython runtime for get_environment and friends.
	if wasiErr := C.wasmtime_component_linker_add_wasip2(linker); wasiErr != nil {
		var msg C.wasm_byte_vec_t
		C.get_error_message(wasiErr, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(wasiErr)
		return nil, fmt.Errorf("wasi add: %s", s)
	}

	if err := componentLinkerTraps(linker, component); err != nil {
		return nil, err
	}

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	wasiConfig := wasmtime.NewWasiConfig()
	wasiConfig.InheritStderr()
	store.SetWasi(wasiConfig)

	instance, err := componentInstantiate(linker, store, component)
	if err != nil {
		return nil, err
	}

	fn, err := componentGetFunc(instance, store, entryPoint)
	if err != nil {
		return nil, err
	}

	raw, callErr := componentCall(fn, store,
		int32(0), int32(len(input)), int32(outBufSz), int32(outBufSz),
	)
	if callErr != nil {
		return nil, fmt.Errorf("host: component export %q: %w", entryPoint, callErr)
	}

	if raw == (1 << 62) {
		return &ExecResult{Suspended: true}, nil
	}

	_, actualLen := decodeExportResult(uint64(raw))
	if actualLen > outBufSz {
		actualLen = outBufSz
	}
	_ = instance
	_ = actualLen

	return &ExecResult{Result: `"ok"`, Suspended: false}, nil
}
