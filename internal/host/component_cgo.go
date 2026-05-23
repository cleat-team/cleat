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

// -- component func call (string ABI) ----------------------------------------
func componentCall(
	fn *C.wasmtime_component_func_t, store wasmtime.Storelike,
	input string,
) (string, error) {
	ctx := C.store_context(unsafe.Pointer(store.Context()))
	cInput := C.CString(input)
	defer C.free(unsafe.Pointer(cInput))
	args := [1]C.wasmtime_component_val_t{
		C.make_component_val_string(cInput, C.size_t(len(input))),
	}
	var result C.wasmtime_component_val_t
	result.kind = C.WASMTIME_COMPONENT_STRING
	err := C.wasmtime_component_func_call(fn, ctx, &args[0], 1, &result, 1)
	if err != nil {
		var msg C.wasm_byte_vec_t; C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg); C.wasmtime_error_delete(err)
		return "", fmt.Errorf("component call: %s", s)
	}
	var resultLen C.size_t
	resultData := C.component_val_get_string(&result, &resultLen)
	if resultData == nil { return "", nil }
	return C.GoStringN(resultData, C.int(resultLen)), nil
}

// -- callback registry -------------------------------------------------------

// cbType identifies what signature the callback should expect.
type cbType int

const (
	cbTypeDefault            cbType = iota // u32 ptr/len args -> u64 result
	cbTypeDurableCallString                // (string, string, string) -> string
)

type cbEntry struct {
	backend *wasmtimeBackend
	typ     cbType
}

var cbRegistry = struct {
	sync.Mutex
	entries map[uintptr]cbEntry
	store   wasmtime.Storelike
	storeID uint64
}{entries: make(map[uintptr]cbEntry)}

func registerCB(b *wasmtimeBackend, typ cbType) uintptr {
	cbRegistry.Lock(); defer cbRegistry.Unlock()
	id := uintptr(len(cbRegistry.entries) + 1)
	cbRegistry.entries[id] = cbEntry{backend: b, typ: typ}
	return id
}

func lookupCB(id uintptr) cbEntry {
	cbRegistry.Lock(); defer cbRegistry.Unlock()
	return cbRegistry.entries[id]
}

// Debug: track callback invocations.

// -- Go callback trampoline --------------------------------------------------
//export goComponentCallback
func goComponentCallback(
	env unsafe.Pointer, ctx *C.wasmtime_context_t,
	ty *C.wasmtime_component_func_type_t,
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	entry := lookupCB(uintptr(env))
	if entry.backend == nil || entry.backend.handler == nil { return nil }
	switch entry.typ {
	case cbTypeDurableCallString:
		return entry.backend.dispatchDurableCallString(ctx, args, nargs, results, nresults)
	default:
		return entry.backend.dispatchComponentDefault(ctx, args, nargs, results, nresults)
	}
}

// dispatchDurableCallString handles (string, string, string) -> string.
func (b *wasmtimeBackend) dispatchDurableCallString(
	_ *C.wasmtime_context_t,
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil { return nil }

	readStrArg := func(i int) string {
		a := (*C.wasmtime_component_val_t)(unsafe.Add(unsafe.Pointer(args), uintptr(i)*unsafe.Sizeof(*args)))
		var slen C.size_t
		sdata := C.component_val_get_string(a, &slen)
		if sdata == nil { return "" }
		return C.GoStringN(sdata, C.int(slen))
	}
	svc := readStrArg(0)
	op := readStrArg(1)
	req := readStrArg(2)


	// DurableCall writes response to a buffer and returns packed i64.
	// We extract the response string from the buffer.
	buf := make([]byte, 65536)
	packed := b.handler.DurableCall(ctxWithMem(context.Background(), buf), nil, svc, op, req, 0, 65536)
	r := uint64(packed); actualLen := uint32((r >> 40) & 0xFFFFFF)
	if actualLen > uint32(len(buf)) { actualLen = uint32(len(buf)) }
	response := string(buf[:actualLen])


	// Return response as WIT string.
	if int(nresults) > 0 {
		cResp := C.CString(response)
		// Leaked -- wasmtime reads after callback returns; must not free.
		resultsPtr := (*C.wasmtime_component_val_t)(unsafe.Pointer(results))
		*resultsPtr = C.make_component_val_string(cResp, C.size_t(len(response)))
	}
	return nil
}

// dispatchComponentDefault handles u32 ptr/len -> u64 functions (fallback).
func (b *wasmtimeBackend) dispatchComponentDefault(
	_ *C.wasmtime_context_t,
	args *C.wasmtime_component_val_t, _ C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	// Return error for now -- these functions aren't wired yet.
	if int(nresults) > 0 {
		resultsPtr := (*C.wasmtime_component_val_t)(unsafe.Pointer(results))
		C.component_val_set_u64(resultsPtr, C.uint64_t(0))
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
			fnType := cbTypeDefault
			if witModule == "cleat:host-calls/durable-call" && witFuncName == "durable-call" {
				fnType = cbTypeDurableCallString
			}
			fnBytes := []byte(witFuncName)
			var fnPtr *C.char
			if len(fnBytes) > 0 { fnPtr = (*C.char)(unsafe.Pointer(&fnBytes[0])) }
			cbID := registerCB(b, fnType)
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

	resultStr, callErr := componentCall(fn, store, string(input))
	if callErr != nil { return nil, fmt.Errorf("host: component export %q: %w", entryPoint, callErr) }


	// Check for suspension sentinel from the Python wrapper.
	if resultStr == "__CLEAT_SUSPEND__" {
		return &ExecResult{Suspended: true}, nil
	}
	_ = instance
	_ = outBufSz
	if resultStr == "" {
		return &ExecResult{Result: `"ok"`, Suspended: false}, nil
	}
	return &ExecResult{Result: resultStr, Suspended: false}, nil
}
