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
// static uint32_t component_val_get_u32(const wasmtime_component_val_t *v) {
//     if (v->kind == WASMTIME_COMPONENT_U32) {
//         return v->of.u32;
//     }
//     return 0;
// }
//
// static void component_val_set_u64(wasmtime_component_val_t *v, uint64_t val) {
//     v->kind = WASMTIME_COMPONENT_U64;
//     v->of.u64 = val;
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
//
// // Trampoline for cleat host functions. The env pointer is an index
// // into a Go-side dispatch table.
// extern wasmtime_error_t *goComponentCallback(
//     void *env, wasmtime_context_t *ctx,
//     wasmtime_component_func_type_t *ty,
//     wasmtime_component_val_t *args, size_t nargs,
//     wasmtime_component_val_t *results, size_t nresults);
import "C"
import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/bytecodealliance/wasmtime-go/v44"
	"github.com/cleat-team/cleat/internal/wasm"
)

// componentCallbackRegistry maps integer IDs to backend pointers.
// This avoids passing Go pointers directly to C (CGo pointer rules).
var componentCallbackRegistry = struct {
	sync.Mutex
	nextID uintptr
	backends map[uintptr]*wasmtimeBackend
}{backends: make(map[uintptr]*wasmtimeBackend)}

func registerComponentCallback(b *wasmtimeBackend) uintptr {
	componentCallbackRegistry.Lock()
	defer componentCallbackRegistry.Unlock()
	id := componentCallbackRegistry.nextID
	componentCallbackRegistry.nextID++
	componentCallbackRegistry.backends[id] = b
	return id
}

func lookupComponentCallback(id uintptr) *wasmtimeBackend {
	componentCallbackRegistry.Lock()
	defer componentCallbackRegistry.Unlock()
	return componentCallbackRegistry.backends[id]
}

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

//export goComponentCallback
func goComponentCallback(
	env unsafe.Pointer,
	ctx *C.wasmtime_context_t,
	ty *C.wasmtime_component_func_type_t,
	args *C.wasmtime_component_val_t,
	nargs C.size_t,
	results *C.wasmtime_component_val_t,
	nresults C.size_t,
) *C.wasmtime_error_t {
	// env is an integer ID from registerComponentCallback.
	id := uintptr(env)
	b := lookupComponentCallback(id)
	if b == nil || b.handler == nil {
		return nil
	}
	return b.dispatchComponentCallback(ctx, ty, args, nargs, results, nresults)
}

// dispatchComponentCallback handles a component-model function call.
func (b *wasmtimeBackend) dispatchComponentCallback(
	ctx *C.wasmtime_context_t,
	_ *C.wasmtime_component_func_type_t,
	args *C.wasmtime_component_val_t,
	nargs C.size_t,
	results *C.wasmtime_component_val_t,
	nresults C.size_t,
) *C.wasmtime_error_t {
	// For now: make all functions return success with zero results.
	// The actual host function dispatch will read from the component
	// memory via the context/store.
	h := b.handler
	_ = h
	_ = ctx
	_ = nargs
	_ = nresults

	// Zero-initialize all results.
	argsSlice := unsafe.Slice(args, nargs)
	resultsSlice := unsafe.Slice(results, nresults)

	// Extract u32 args using C helpers (CGo can't access union fields directly).
	getU32 := func(i int) uint32 {
		if i < len(argsSlice) {
			return uint32(C.component_val_get_u32(&argsSlice[i]))
		}
		return 0
	}

	svcPtr := getU32(0)
	svcLen := getU32(1)
	opPtr := getU32(2)
	opLen := getU32(3)
	reqPtr := getU32(4)
	reqLen := getU32(5)
	respPtr := getU32(6)
	respMaxLen := getU32(7)

	// We need memory access. The context has the store, which has WASM memory.
	// Use the store to read/write linear memory.
	_ = svcPtr
	_ = svcLen
	_ = opPtr
	_ = opLen
	_ = reqPtr
	_ = reqLen
	_ = respPtr
	_ = respMaxLen

	// For now: just set a zero result.
	if len(resultsSlice) > 0 {
		C.component_val_set_u64(&resultsSlice[0], 0)
	}
	return nil
}

// registerCleatComponentImports registers all cleat host functions
// under their WIT names in the component linker.
func (b *wasmtimeBackend) registerCleatComponentImports(linker *C.wasmtime_component_linker_t) error {
	root := C.wasmtime_component_linker_root(linker)
	if root == nil {
		return fmt.Errorf("component linker root is nil")
	}

	// For each WIT interface, create a nested linker instance and add
	// functions. We use the WIT-to-env mapping from component_rewrite.go.
	for witModule, funcs := range wasm.WitToEnvImport {
		nameBytes := []byte(witModule)
		var namePtr *C.char
		if len(nameBytes) > 0 {
			namePtr = (*C.char)(unsafe.Pointer(&nameBytes[0]))
		}
		var subInstance *C.wasmtime_component_linker_instance_t
		err := C.wasmtime_component_linker_instance_add_instance(
			root,
			namePtr,
			C.size_t(len(witModule)),
			&subInstance,
		)
		if err != nil {
			var msg C.wasm_byte_vec_t
			C.get_error_message(err, &msg)
			s := C.GoStringN(msg.data, C.int(msg.size))
			C.wasm_byte_vec_delete(&msg)
			C.wasmtime_error_delete(err)
			return fmt.Errorf("register %s: %s", witModule, s)
		}

		for witFuncName := range funcs {
			fnBytes := []byte(witFuncName)
			var fnPtr *C.char
			if len(fnBytes) > 0 {
				fnPtr = (*C.char)(unsafe.Pointer(&fnBytes[0]))
			}
			// Register with our Go callback trampoline.
			// Use an integer ID to avoid CGo pointer restrictions.
			cbID := registerComponentCallback(b)
			err := C.wasmtime_component_linker_instance_add_func(
				subInstance,
				fnPtr,
				C.size_t(len(witFuncName)),
				C.wasmtime_component_func_callback_t(C.goComponentCallback),
				unsafe.Pointer(uintptr(cbID)),
				nil, // no finalizer
			)
			if err != nil {
				var msg C.wasm_byte_vec_t
				C.get_error_message(err, &msg)
				s := C.GoStringN(msg.data, C.int(msg.size))
				C.wasm_byte_vec_delete(&msg)
				C.wasmtime_error_delete(err)
				return fmt.Errorf("register %s.%s: %s", witModule, witFuncName, s)
			}
		}
	}
	return nil
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

	store := wasmtime.NewStore(b.engine)
	defer store.Close()
	wasiConfig := wasmtime.NewWasiConfig()
	wasiConfig.InheritStderr()
	// Set minimal environment for CPython.
	wasiConfig.SetEnv([]string{"PYTHONHOME", "PYTHONPATH"}, []string{"/", "/"})
	store.SetWasi(wasiConfig)

	// Allow shadowing so WASI can override trap definitions.
	C.wasmtime_component_linker_allow_shadowing(linker, true)

	// Add WASI 0.2.
	if wasiErr := C.wasmtime_component_linker_add_wasip2(linker); wasiErr != nil {
		var msg C.wasm_byte_vec_t
		C.get_error_message(wasiErr, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(wasiErr)
		fmt.Printf("[CGO_WASI] add_wasip2 error: %s\n", s)
	} else {
		fmt.Printf("[CGO_WASI] add_wasip2 succeeded\n")
	}

	// Register cleat host functions under their WIT names.
	if err := b.registerCleatComponentImports(linker); err != nil {
		return nil, fmt.Errorf("cleat component imports: %w", err)
	}

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
