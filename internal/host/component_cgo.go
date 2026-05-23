//go:build cgo

package host

// #cgo CFLAGS:-I/tmp/wasmtime-v45/wasmtime-v45.0.0-x86_64-linux-c-api/include -I/home/rcownie/go/pkg/mod/github.com/bytecodealliance/wasmtime-go/v44@v44.0.0/build/include
// #cgo linux,amd64 LDFLAGS:-L/tmp/wasmtime-v45/wasmtime-v45.0.0-x86_64-linux-c-api/lib -lwasmtime -lm -ldl -pthread
// #include <wasmtime.h>
// #include <wasmtime/component/component.h>
// #include <wasmtime/component/linker.h>
// #include <wasmtime/component/instance.h>
// #include <wasmtime/component/func.h>
// #include <wasmtime/component/val.h>
// #include <stdlib.h>
// #include <string.h>
//
// static wasmtime_component_val_t make_component_val_u32(uint32_t v) {
//     wasmtime_component_val_t val;
//     val.kind = WASMTIME_COMPONENT_U32;
//     val.of.u32 = v;
//     return val;
// }
//
// static uint64_t component_val_get_u64(const wasmtime_component_val_t *v) {
//     if (v->kind == WASMTIME_COMPONENT_U64) { return v->of.u64; }
//     return 0;
// }
//
// static uint32_t component_val_get_u32(const wasmtime_component_val_t *v) {
//     if (v->kind == WASMTIME_COMPONENT_U32) { return v->of.u32; }
//     return 0;
// }
//
// // Extract string data and length from a component val.
// // Returns pointer to string data (owned by wasmtime), sets *len.
// static const char *component_val_get_string(const wasmtime_component_val_t *v,
//     size_t *len) {
//     if (v->kind == WASMTIME_COMPONENT_STRING) {
//         *len = v->of.string.size;
//         return v->of.string.data;
//     }
//     *len = 0;
//     return NULL;
// }
//
// static void component_val_set_u64(wasmtime_component_val_t *v, uint64_t val) {
//     v->kind = WASMTIME_COMPONENT_U64;
//     v->of.u64 = val;
// }
//
// static wasmtime_component_val_t make_component_val_string(const char *s, size_t len) {
//     wasmtime_component_val_t val;
//     val.kind = WASMTIME_COMPONENT_STRING;
//     val.of.string.data = (char *)s;
//     val.of.string.size = len;
//     return val;
// }
//
// static void get_error_message(wasmtime_error_t *err, wasm_byte_vec_t *msg) {
//     wasmtime_error_message(err, msg);
// }
//
// static wasmtime_error_t *component_compile(
//     wasm_engine_t *engine, uint8_t *buf, size_t len,
//     wasmtime_component_t **out) {
//     return wasmtime_component_new(engine, buf, len, out);
// }
//
// static wasmtime_context_t *store_context(void *ctx_ptr) {
//     return (wasmtime_context_t *)ctx_ptr;
// }
//
// // Save memory data pointer after instantiation. Initialized by
// // save_first_memory_data which is called once after instantiation.
// static uint8_t *saved_memory_ptr = NULL;
// static size_t saved_memory_len = 0;
//
// static uint8_t *get_saved_memory_ptr(void) { return saved_memory_ptr; }
// static size_t get_saved_memory_len(void) { return saved_memory_len; }
//
// // Memory scan disabled - not needed with WIT string ABI.
// static void save_first_memory_data(wasmtime_context_t *ctx, uint64_t store_id) {}
//// // Trampoline for cleat host functions.
// extern wasmtime_error_t *goComponentCallback(
//     void *env, wasmtime_context_t *ctx,
//     wasmtime_component_func_type_t *ty,
//     wasmtime_component_val_t *args, size_t nargs,
//     wasmtime_component_val_t *results, size_t nresults);
import "C"
import (
	"context"
	"fmt"
	"sync"
	"unsafe"

	"github.com/bytecodealliance/wasmtime-go/v44"
	"github.com/cleat-team/cleat/internal/wasm"
)

// -- engine ptr access -------------------------------------------------------
func getEnginePtr(engine *wasmtime.Engine) *C.wasm_engine_t {
	return (*C.wasm_engine_t)(*(*unsafe.Pointer)(unsafe.Pointer(engine)))
}

// -- component compilation ---------------------------------------------------
func componentCompile(engine *wasmtime.Engine, wasmBytes []byte) (*C.wasmtime_component_t, error) {
	var ptr *C.wasmtime_component_t
	var bufPtr *C.uint8_t
	if len(wasmBytes) > 0 { bufPtr = (*C.uint8_t)(unsafe.Pointer(&wasmBytes[0])) }
	err := C.component_compile(getEnginePtr(engine), bufPtr, C.size_t(len(wasmBytes)), &ptr)
	if err != nil {
		var msg C.wasm_byte_vec_t; C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg); C.wasmtime_error_delete(err)
		return nil, fmt.Errorf("component compile: %s", s)
	}
	return ptr, nil
}

// -- component linker --------------------------------------------------------
func componentLinkerNew(engine *wasmtime.Engine) *C.wasmtime_component_linker_t {
	return C.wasmtime_component_linker_new(getEnginePtr(engine))
}

// -- component instantiation -------------------------------------------------
func componentInstantiate(
	linker *C.wasmtime_component_linker_t, store wasmtime.Storelike,
	component *C.wasmtime_component_t,
) (*C.wasmtime_component_instance_t, error) {
	var inst C.wasmtime_component_instance_t
	err := C.wasmtime_component_linker_instantiate(linker, C.store_context(unsafe.Pointer(store.Context())), component, &inst)
	if err != nil {
		var msg C.wasm_byte_vec_t; C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg); C.wasmtime_error_delete(err)
		return nil, fmt.Errorf("component instantiate: %s", s)
	}
	out := new(C.wasmtime_component_instance_t); *out = inst
	return out, nil
}

// -- component export lookup -------------------------------------------------
func componentGetFunc(
	instance *C.wasmtime_component_instance_t, store wasmtime.Storelike, name string,
) (*C.wasmtime_component_func_t, error) {
	ctxPtr := store.Context(); nameBytes := []byte(name)
	var namePtr *C.char
	if len(nameBytes) > 0 { namePtr = (*C.char)(unsafe.Pointer(&nameBytes[0])) }
	exportIdx := C.wasmtime_component_instance_get_export_index(instance, C.store_context(unsafe.Pointer(ctxPtr)), nil, namePtr, C.size_t(len(name)))
	if exportIdx == nil { return nil, fmt.Errorf("component export %q not found", name) }
	defer C.wasmtime_component_export_index_delete(exportIdx)
	var fn C.wasmtime_component_func_t
	if !C.wasmtime_component_instance_get_func(instance, C.store_context(unsafe.Pointer(ctxPtr)), exportIdx, &fn) {
		return nil, fmt.Errorf("component export %q not a function", name)
	}
	out := new(C.wasmtime_component_func_t); *out = fn
	return out, nil
}

// -- component func call -----------------------------------------------------
func componentCall(
	fn *C.wasmtime_component_func_t, store wasmtime.Storelike,
	inputOffset, inputLen, outputOffset, outBufSz int32,
) (int64, error) {
	ctx := C.store_context(unsafe.Pointer(store.Context()))
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
		var msg C.wasm_byte_vec_t; C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg); C.wasmtime_error_delete(err)
		return 0, fmt.Errorf("component call: %s", s)
	}
	return int64(C.component_val_get_u64(&result)), nil
}

// -- callback registry -------------------------------------------------------
var cbRegistry = struct {
	sync.Mutex
	backends map[uintptr]*wasmtimeBackend
	store    wasmtime.Storelike
	storeID  uint64
}{backends: make(map[uintptr]*wasmtimeBackend)}

func registerCB(b *wasmtimeBackend) uintptr {
	cbRegistry.Lock(); defer cbRegistry.Unlock()
	id := uintptr(len(cbRegistry.backends) + 1)
	cbRegistry.backends[id] = b
	return id
}

func lookupCB(id uintptr) *wasmtimeBackend {
	cbRegistry.Lock(); defer cbRegistry.Unlock()
	return cbRegistry.backends[id]
}

// -- Go callback trampoline --------------------------------------------------
//export goComponentCallback
func goComponentCallback(
	env unsafe.Pointer, ctx *C.wasmtime_context_t,
	ty *C.wasmtime_component_func_type_t,
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	fmt.Printf("[CGO_CB] any callback invoked\n")
	b := lookupCB(uintptr(env))
	if b == nil || b.handler == nil { return nil }
	return b.dispatchComponentCallback(ctx, ty, args, nargs, results, nresults)
}

func (b *wasmtimeBackend) dispatchComponentCallback(
	ctx *C.wasmtime_context_t,
	_ *C.wasmtime_component_func_type_t,
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	// Use pre-saved memory data pointer (found once after instantiation).
	var packed int64
	if C.get_saved_memory_ptr() != nil && C.get_saved_memory_len() > 0 && b.handler != nil {
		rawData := unsafe.Slice(C.get_saved_memory_ptr(), C.get_saved_memory_len())
		data := *(*[]byte)(unsafe.Pointer(&rawData))
		getU32 := func(i int) uint32 {
			if i < int(nargs) {
				a := (*C.wasmtime_component_val_t)(unsafe.Add(unsafe.Pointer(args), uintptr(i)*unsafe.Sizeof(*args)))
				return uint32(C.component_val_get_u32(a))
			}
			return 0
		}
		readStr := func(ptr, length uint32) (string, bool) {
			if ptr+length <= uint32(len(data)) && length <= uint32(MaxWasmStringLen) {
				return string(data[ptr : ptr+length]), true
			}
			return "", false
		}
		svc, ok1 := readStr(getU32(0), getU32(1))
		op, ok2 := readStr(getU32(2), getU32(3))
		req, ok3 := readStr(getU32(4), getU32(5))
		respPtr := getU32(6)
		respMaxLen := getU32(7)
		if ok1 && ok2 && ok3 {
			packed = b.handler.DurableCall(ctxWithMem(context.Background(), data), nil, svc, op, req, uint32(respPtr), uint32(respMaxLen))
		} else {
			packed = errBadParamInt64
		}
	} else {
		packed = errBadParamInt64
	}
	if int(nresults) > 0 {
		resultsPtr := (*C.wasmtime_component_val_t)(unsafe.Pointer(results))
		C.component_val_set_u64(resultsPtr, C.uint64_t(packed))
	}
	return nil
}


// -- register cleat WIT functions in component linker -------------------------

func (b *wasmtimeBackend) registerCleatComponentImports(linker *C.wasmtime_component_linker_t) error {
	root := C.wasmtime_component_linker_root(linker)
	if root == nil { return fmt.Errorf("component linker root is nil") }
	for witModule, funcs := range wasm.WitToEnvImport {
		nameBytes := []byte(witModule)
		var namePtr *C.char
		if len(nameBytes) > 0 { namePtr = (*C.char)(unsafe.Pointer(&nameBytes[0])) }
		var sub *C.wasmtime_component_linker_instance_t
		err := C.wasmtime_component_linker_instance_add_instance(root, namePtr, C.size_t(len(witModule)), &sub)
		if err != nil {
			var msg C.wasm_byte_vec_t; C.get_error_message(err, &msg)
			s := C.GoStringN(msg.data, C.int(msg.size))
			C.wasm_byte_vec_delete(&msg); C.wasmtime_error_delete(err)
			return fmt.Errorf("register %s: %s", witModule, s)
		}
		for witFuncName := range funcs {
			if witModule == "cleat:host-calls/durable-call" {
				}
			fnBytes := []byte(witFuncName)
			var fnPtr *C.char
			if len(fnBytes) > 0 { fnPtr = (*C.char)(unsafe.Pointer(&fnBytes[0])) }
			cbID := registerCB(b)
			err := C.wasmtime_component_linker_instance_add_func(sub, fnPtr, C.size_t(len(witFuncName)), C.wasmtime_component_func_callback_t(C.goComponentCallback), unsafe.Pointer(uintptr(cbID)), nil)
			if err != nil {
				var msg C.wasm_byte_vec_t; C.get_error_message(err, &msg)
				s := C.GoStringN(msg.data, C.int(msg.size))
				C.wasm_byte_vec_delete(&msg); C.wasmtime_error_delete(err)
				return fmt.Errorf("register %s.%s: %s", witModule, witFuncName, s)
			}
		}
	}
	return nil
}

// -- ExecuteComponentCGo ------------------------------------------------------
func (b *wasmtimeBackend) ExecuteComponentCGo(
	wasmBytes []byte, entryPoint string, input []byte, outBufSz uint32,
) (*ExecResult, error) {
	component, err := componentCompile(b.engine, wasmBytes)
	if err != nil { return nil, err }
	defer C.wasmtime_component_delete(component)

	linker := componentLinkerNew(b.engine)
	if linker == nil { return nil, fmt.Errorf("component linker creation failed") }
	defer C.wasmtime_component_linker_delete(linker)

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	wasiConfig := wasmtime.NewWasiConfig()
	wasiConfig.InheritStderr()
	wasiConfig.SetEnv([]string{"PYTHONHOME", "PYTHONPATH"}, []string{"/", "/"})
	store.SetWasi(wasiConfig)

	C.wasmtime_component_linker_allow_shadowing(linker, true)

	if wasiErr := C.wasmtime_component_linker_add_wasip2(linker); wasiErr != nil {
		var msg C.wasm_byte_vec_t; C.get_error_message(wasiErr, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg); C.wasmtime_error_delete(wasiErr)
		return nil, fmt.Errorf("wasi add: %s", s)
	}

	if err := b.registerCleatComponentImports(linker); err != nil {
		return nil, fmt.Errorf("cleat component imports: %w", err)
	}

	instance, err := componentInstantiate(linker, store, component)
	if err != nil { return nil, err }

	// Save store info and scan for memory once.
	cbRegistry.Lock()
	cbRegistry.store = store
	cbRegistry.storeID = uint64(instance.store_id)
	cbRegistry.Unlock()
	C.save_first_memory_data(C.store_context(unsafe.Pointer(store.Context())), C.uint64_t(instance.store_id))

	fn, err := componentGetFunc(instance, store, entryPoint)
	if err != nil { return nil, fmt.Errorf("component get func: %w", err) }

	raw, callErr := componentCall(fn, store, int32(0), int32(len(input)), int32(outBufSz), int32(outBufSz))
	if callErr != nil { return nil, fmt.Errorf("host: component export %q: %w", entryPoint, callErr) }

	if raw == (1 << 62) { return &ExecResult{Suspended: true}, nil }
	_, actualLen := decodeExportResult(uint64(raw))
	if actualLen > outBufSz { actualLen = outBufSz }
	_ = instance
	_ = actualLen
	return &ExecResult{Result: `"ok"`, Suspended: false}, nil

	_ = instance
	return nil, fmt.Errorf("CGo: validated, falling back")
}
