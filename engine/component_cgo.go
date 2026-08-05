//go:build cgo

// This file (plus component_callbacks.go and cgo_test_helpers.go) calls the
// wasmtime Component Model C API directly via cgo, using types like
// wasmtime_component_val_t that github.com/bytecodealliance/wasmtime-go/v44
// does not expose through its Go bindings -- the module ships exactly one
// component-related Go file, config_feat_component_model.go, and it is a
// config flag.
//
// So these files need a `-I` to wasmtime's C headers, and cgo has no way to
// point one at another module: `${SRCDIR}` in a #cgo directive expands to
// *this* file's own directory, never to an imported module's, and the module
// cache path varies with GOMODCACHE. That is why the headers are vendored at
// engine/wasmtimeinc (see its README) -- vendoring is what makes the -I below
// expressible, and it is the whole reason this path can be a plain `cgo`
// build rather than an opt-in tag.
//
// It used to be an opt-in tag, wasmtime_component_cgo, and that was the
// defect: no build, CI job, Makefile or Dockerfile set it, so every build got
// the stub, and every Component Model guest -- which in practice means every
// Python workflow -- fell through to the hand-rolled decomposition path in
// backend_wasmtime.go, where componentize-py output stops at `undefined
// element: out of bounds table access` instantiating instance 52. The code
// here was correct the whole time; nothing compiled it. Same shape as §1.5,
// where a working execution fence reached no deployment for weeks because it
// sat behind a build tag the shipped image did not set. See IMPROVEMENT-PLAN
// §2.72 and §1.5/§2.28.
//
// No extra CGO_LDFLAGS is needed: wasmtime-go's own ffi.go already declares
// `#cgo LDFLAGS: -L${SRCDIR}/build/<platform> -lwasmtime ...`, and cgo LDFLAGS
// from every cgo-using package in the import graph are combined at final link
// time, so linking against libwasmtime "just works" once wasmtime-go itself is
// imported. That is also why wasmtime_headers_test.go exists: the headers here
// must describe the same library that link step pulls in, so they are diffed
// against the module on every test run rather than trusted to stay in step.

package engine

// #cgo CFLAGS: -I${SRCDIR}/wasmtimeinc
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
//     if (v->kind == WASMTIME_COMPONENT_S64) { return (uint64_t)v->of.s64; }
//     return 0;
// }
//
// static uint32_t component_val_get_u32(const wasmtime_component_val_t *v) {
//     if (v->kind == WASMTIME_COMPONENT_U32) { return v->of.u32; }
//     if (v->kind == WASMTIME_COMPONENT_S32) { return (uint32_t)v->of.s32; }
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
)

// -- engine ptr access -------------------------------------------------------
func getEnginePtr(engine *wasmtime.Engine) *C.wasm_engine_t {
	return (*C.wasm_engine_t)(*(*unsafe.Pointer)(unsafe.Pointer(engine)))
}

// -- component compilation ---------------------------------------------------
func componentCompile(engine *wasmtime.Engine, wasmBytes []byte) (*C.wasmtime_component_t, error) {
	var ptr *C.wasmtime_component_t
	var bufPtr *C.uint8_t
	if len(wasmBytes) > 0 {
		bufPtr = (*C.uint8_t)(unsafe.Pointer(&wasmBytes[0]))
	}
	err := C.component_compile(getEnginePtr(engine), bufPtr, C.size_t(len(wasmBytes)), &ptr)
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

// -- component export lookup -------------------------------------------------
func componentGetFunc(
	instance *C.wasmtime_component_instance_t, store wasmtime.Storelike, name string,
) (*C.wasmtime_component_func_t, error) {
	ctxPtr := store.Context()
	nameBytes := []byte(name)
	var namePtr *C.char
	if len(nameBytes) > 0 {
		namePtr = (*C.char)(unsafe.Pointer(&nameBytes[0]))
	}
	exportIdx := C.wasmtime_component_instance_get_export_index(instance, C.store_context(unsafe.Pointer(ctxPtr)), nil, namePtr, C.size_t(len(name)))
	if exportIdx == nil {
		return nil, fmt.Errorf("component export %q not found", name)
	}
	defer C.wasmtime_component_export_index_delete(exportIdx)
	var fn C.wasmtime_component_func_t
	if !C.wasmtime_component_instance_get_func(instance, C.store_context(unsafe.Pointer(ctxPtr)), exportIdx, &fn) {
		return nil, fmt.Errorf("component export %q not a function", name)
	}
	out := new(C.wasmtime_component_func_t)
	*out = fn
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
		var msg C.wasm_byte_vec_t
		C.get_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(err)
		return "", fmt.Errorf("component call: %s", s)
	}
	var resultLen C.size_t
	resultData := C.component_val_get_string(&result, &resultLen)
	if resultData == nil {
		return "", nil
	}
	return C.GoStringN(resultData, C.int(resultLen)), nil
}

// -- callback registry -------------------------------------------------------

// cbType identifies what signature the callback should expect.
type cbType int

const (
	cbTypeDefault cbType = iota // deprecated fallback - returns 0

	// durable-call interface
	cbTypeDurableCallString    // (string,string,string) -> string
	cbTypeDurableCallRetry     // (string,string,string,u64,u64,u64,u64,string) -> string
	cbTypeDurableCallHeartbeat // (string,string,string,u64) -> string

	// durable-sleep interface
	cbTypeDurableSleep // (u64) -> u64
	cbTypeNow          // () -> u64
	cbTypeRandom       // () -> u64
	cbTypeDurableLog   // (string) -> u64

	// durable-version interface
	cbTypeVersion    // () -> u64
	cbTypeMinVersion // () -> u64

	// durable-lifecycle interface
	cbTypeDurableDefer     // (string) -> string
	cbTypeContinueAsNew    // (string) -> u64
	cbTypePollCancellation // () -> string

	// durable-signals interface
	cbTypeAwaitSignals      // (string,u64,u32,u32,u32,u32) -> u64
	cbTypePollSignal        // (string) -> string
	cbTypeSendSignalAndWait // (string,string,string,u64) -> string
	cbTypeReplyToSignal     // (string,string) -> u64
	cbTypeSignalWorkflow    // (string,string,string) -> u64

	// durable-children interface
	cbTypeChildWorkflow            // (string,string) -> string
	cbTypeAwaitChild               // (string) -> string
	cbTypeAwaitAllChildren         // (string) -> string
	cbTypeChildWorkflowWithOptions // (string,string,u64,u64,string) -> string

	// durable-promises interface
	cbTypeCreatePromise  // (string[,u64]) -> string (WIT has ttl-ms, handler doesn't)
	cbTypeAwaitPromise   // (string,u64) -> string
	cbTypeResolvePromise // (string,string) -> u64
	cbTypeRejectPromise  // (string,string) -> u64

	// durable-state interface
	cbTypeSetQueryState // (string,string) -> u64

	// durable-handlers interface
	cbTypeRegisterUpdateHandler // (string) -> u64
	cbTypeRegisterQueryHandler  // (string) -> u64

	// durable-messaging interface
	cbTypeDurableSend    // (string,string,string) -> u64
	cbTypeScheduleInvoke // (string,string,string,u64) -> u64

	// durable-identity interface
	cbTypeWorkflowID // () -> string
	cbTypeRunID      // () -> string

	// plugin interface
	cbTypePluginCall          // (string,string,string) -> string
	cbTypePluginCallStreaming // (string,string,string) -> string

	// durable-lock interface
	cbTypeAcquireLock // (string,s64) -> s64
	cbTypeReleaseLock // (string) -> s64

	// durable-scope interface
	cbTypeSetScope // (string,string) -> string
	cbTypeGetScope // (u32,u32,u32,u32) -> u64
	cbTypeUUID     // (string) -> string

	// durable-stream-state interface
	cbTypeSetState    // (string,string) -> u64
	cbTypeGetState    // (string) -> string
	cbTypeDeleteState // (string) -> u64
	cbTypeIncrState   // (string,u64) -> u64
	cbTypeHasState    // (string) -> u64
	cbTypeListState   // (string) -> string

	// durable-extended-lifecycle interface
	cbTypeContinueAsNewVersioned // (string,u32) -> u64
	cbTypeSideEffect             // (string) -> string

	// durable-extended-children interface
	cbTypeChildWorkflowInSchema // (string,string,string,u64,u64,string) -> string

	// durable-fetch interface
	cbTypeFetch // (string,string,string,string) -> string
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
	cbRegistry.Lock()
	defer cbRegistry.Unlock()
	id := uintptr(len(cbRegistry.entries) + 1)
	cbRegistry.entries[id] = cbEntry{backend: b, typ: typ}
	return id
}

func lookupCB(id uintptr) cbEntry {
	cbRegistry.Lock()
	defer cbRegistry.Unlock()
	return cbRegistry.entries[id]
}

// -- Go helper functions for reading/writing component vals ------------------

// argPtr returns a pointer to the i-th component val in the args array.
func argPtr(args *C.wasmtime_component_val_t, i int) *C.wasmtime_component_val_t {
	return (*C.wasmtime_component_val_t)(unsafe.Add(unsafe.Pointer(args), uintptr(i)*unsafe.Sizeof(*args)))
}

// readStrArg returns the i-th argument as a Go string, or "" if out of range.
func readStrArg(args *C.wasmtime_component_val_t, i int, nargs C.size_t) string {
	if int(nargs) <= i {
		return ""
	}
	a := argPtr(args, i)
	var slen C.size_t
	sdata := C.component_val_get_string(a, &slen)
	if sdata == nil {
		return ""
	}
	return C.GoStringN(sdata, C.int(slen))
}

// readU64Arg returns the i-th argument as a uint64, or 0 if out of range.
// Handles both WASMTIME_COMPONENT_U64 and WASMTIME_COMPONENT_S64 kinds.
func readU64Arg(args *C.wasmtime_component_val_t, i int, nargs C.size_t) uint64 {
	if int(nargs) <= i {
		return 0
	}
	a := argPtr(args, i)
	return uint64(C.component_val_get_u64(a))
}

// readU32Arg returns the i-th argument as a uint32, or 0 if out of range.
// Handles both WASMTIME_COMPONENT_U32 and WASMTIME_COMPONENT_S32 kinds.
func readU32Arg(args *C.wasmtime_component_val_t, i int, nargs C.size_t) uint32 {
	if int(nargs) <= i {
		return 0
	}
	a := argPtr(args, i)
	return uint32(C.component_val_get_u32(a))
}

// setResultU64 sets the first result value to a u64.
func setResultU64(results *C.wasmtime_component_val_t, nresults C.size_t, val uint64) {
	if int(nresults) < 1 {
		return
	}
	r := (*C.wasmtime_component_val_t)(unsafe.Pointer(results))
	C.component_val_set_u64(r, C.uint64_t(val))
}

// setResultString sets the first result value to a WIT string.
// The C string memory is intentionally leaked -- wasmtime reads it after the
// callback returns and the host cannot free it.
func setResultString(results *C.wasmtime_component_val_t, nresults C.size_t, s string) {
	if int(nresults) < 1 {
		return
	}
	cStr := C.CString(s)
	r := (*C.wasmtime_component_val_t)(unsafe.Pointer(results))
	*r = C.make_component_val_string(cStr, C.size_t(len(s)))
}

// extractStringFromPacked decodes the output string from a handler's packed
// return value. The packed format is:
//
//	(responseLen << 40) | (callErrorCode << 8) | errCode
func extractStringFromPacked(packed int64, buf []byte) string {
	r := uint64(packed)
	actualLen := uint32((r >> 40) & 0xFFFFFF)
	if actualLen > uint32(len(buf)) {
		actualLen = uint32(len(buf))
	}
	return string(buf[:actualLen])
}

// -- Go callback trampoline --------------------------------------------------
//
//export goComponentCallback
func goComponentCallback(
	env unsafe.Pointer, ctx *C.wasmtime_context_t,
	ty *C.wasmtime_component_func_type_t,
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	entry := lookupCB(uintptr(env))
	if entry.backend == nil || entry.backend.handler == nil {
		return nil
	}
	switch entry.typ {
	case cbTypeDurableCallString:
		return entry.backend.dispatchDurableCallString(args, nargs, results, nresults)
	case cbTypeDurableCallRetry:
		return entry.backend.dispatchDurableCallRetry(args, nargs, results, nresults)
	case cbTypeDurableCallHeartbeat:
		return entry.backend.dispatchDurableCallHeartbeat(args, nargs, results, nresults)
	case cbTypeDurableSleep:
		return entry.backend.dispatchDurableSleep(args, nargs, results, nresults)
	case cbTypeNow:
		return entry.backend.dispatchNow(args, nargs, results, nresults)
	case cbTypeRandom:
		return entry.backend.dispatchRandom(args, nargs, results, nresults)
	case cbTypeDurableLog:
		return entry.backend.dispatchDurableLog(args, nargs, results, nresults)
	case cbTypeVersion:
		return entry.backend.dispatchVersion(args, nargs, results, nresults)
	case cbTypeMinVersion:
		return entry.backend.dispatchMinVersion(args, nargs, results, nresults)
	case cbTypeDurableDefer:
		return entry.backend.dispatchDurableDefer(args, nargs, results, nresults)
	case cbTypeContinueAsNew:
		return entry.backend.dispatchContinueAsNew(args, nargs, results, nresults)
	case cbTypePollCancellation:
		return entry.backend.dispatchPollCancellation(args, nargs, results, nresults)
	case cbTypeAwaitSignals:
		return entry.backend.dispatchAwaitSignals(args, nargs, results, nresults)
	case cbTypePollSignal:
		return entry.backend.dispatchPollSignal(args, nargs, results, nresults)
	case cbTypeSendSignalAndWait:
		return entry.backend.dispatchSendSignalAndWait(args, nargs, results, nresults)
	case cbTypeReplyToSignal:
		return entry.backend.dispatchReplyToSignal(args, nargs, results, nresults)
	case cbTypeSignalWorkflow:
		return entry.backend.dispatchSignalWorkflow(args, nargs, results, nresults)
	case cbTypeChildWorkflow:
		return entry.backend.dispatchChildWorkflow(args, nargs, results, nresults)
	case cbTypeAwaitChild:
		return entry.backend.dispatchAwaitChild(args, nargs, results, nresults)
	case cbTypeAwaitAllChildren:
		return entry.backend.dispatchAwaitAllChildren(args, nargs, results, nresults)
	case cbTypeChildWorkflowWithOptions:
		return entry.backend.dispatchChildWorkflowWithOptions(args, nargs, results, nresults)
	case cbTypeCreatePromise:
		return entry.backend.dispatchCreatePromise(args, nargs, results, nresults)
	case cbTypeAwaitPromise:
		return entry.backend.dispatchAwaitPromise(args, nargs, results, nresults)
	case cbTypeResolvePromise:
		return entry.backend.dispatchResolvePromise(args, nargs, results, nresults)
	case cbTypeRejectPromise:
		return entry.backend.dispatchRejectPromise(args, nargs, results, nresults)
	case cbTypeSetQueryState:
		return entry.backend.dispatchSetQueryState(args, nargs, results, nresults)
	case cbTypeRegisterUpdateHandler:
		return entry.backend.dispatchRegisterUpdateHandler(args, nargs, results, nresults)
	case cbTypeRegisterQueryHandler:
		return entry.backend.dispatchRegisterQueryHandler(args, nargs, results, nresults)
	case cbTypeDurableSend:
		return entry.backend.dispatchDurableSend(args, nargs, results, nresults)
	case cbTypeScheduleInvoke:
		return entry.backend.dispatchScheduleInvoke(args, nargs, results, nresults)
	case cbTypeWorkflowID:
		return entry.backend.dispatchWorkflowID(args, nargs, results, nresults)
	case cbTypeRunID:
		return entry.backend.dispatchRunID(args, nargs, results, nresults)
	case cbTypePluginCall:
		return entry.backend.dispatchPluginCall(args, nargs, results, nresults)
	case cbTypePluginCallStreaming:
		return entry.backend.dispatchPluginCallStreaming(args, nargs, results, nresults)
	case cbTypeAcquireLock:
		return entry.backend.dispatchAcquireLock(args, nargs, results, nresults)
	case cbTypeReleaseLock:
		return entry.backend.dispatchReleaseLock(args, nargs, results, nresults)
	case cbTypeSetScope:
		return entry.backend.dispatchSetScope(args, nargs, results, nresults)
	case cbTypeGetScope:
		return entry.backend.dispatchGetScope(args, nargs, results, nresults)
	case cbTypeUUID:
		return entry.backend.dispatchUUID(args, nargs, results, nresults)
	case cbTypeSetState:
		return entry.backend.dispatchSetState(args, nargs, results, nresults)
	case cbTypeGetState:
		return entry.backend.dispatchGetState(args, nargs, results, nresults)
	case cbTypeDeleteState:
		return entry.backend.dispatchDeleteState(args, nargs, results, nresults)
	case cbTypeIncrState:
		return entry.backend.dispatchIncrState(args, nargs, results, nresults)
	case cbTypeHasState:
		return entry.backend.dispatchHasState(args, nargs, results, nresults)
	case cbTypeListState:
		return entry.backend.dispatchListState(args, nargs, results, nresults)
	case cbTypeContinueAsNewVersioned:
		return entry.backend.dispatchContinueAsNewVersioned(args, nargs, results, nresults)
	case cbTypeSideEffect:
		return entry.backend.dispatchSideEffect(args, nargs, results, nresults)
	case cbTypeChildWorkflowInSchema:
		return entry.backend.dispatchChildWorkflowInSchema(args, nargs, results, nresults)
	case cbTypeFetch:
		return entry.backend.dispatchFetch(args, nargs, results, nresults)
	default:
		// Deprecated fallback -- unregistered functions return 0.
		return entry.backend.dispatchComponentDefault(args, nargs, results, nresults)
	}
}

// =============================================================================
// dispatch methods (one per cbType)
// =============================================================================

// ---------------------------------------------------------------------------
// durable-call interface
// ---------------------------------------------------------------------------
func (b *wasmtimeBackend) ExecuteComponentCGo(
	wasmBytes []byte, entryPoint string, input []byte, outBufSz uint32,
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
	if _, err := b.configureStore(context.Background(), store); err != nil {
		return nil, err
	}
	wasiConfig := wasmtime.NewWasiConfig()
	wasiConfig.InheritStderr()
	wasiConfig.SetEnv([]string{"PYTHONHOME", "PYTHONPATH"}, []string{"/", "/"})
	store.SetWasi(wasiConfig)

	C.wasmtime_component_linker_allow_shadowing(linker, true)

	if wasiErr := C.wasmtime_component_linker_add_wasip2(linker); wasiErr != nil {
		var msg C.wasm_byte_vec_t
		C.get_error_message(wasiErr, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(wasiErr)
		return nil, fmt.Errorf("wasi add: %s", s)
	}

	if err := b.registerCleatComponentImports(linker); err != nil {
		return nil, fmt.Errorf("cleat component imports: %w", err)
	}

	instance, err := componentInstantiate(linker, store, component)
	if err != nil {
		return nil, err
	}

	// Save store info and scan for memory once.
	cbRegistry.Lock()
	cbRegistry.store = store
	cbRegistry.storeID = uint64(instance.store_id)
	cbRegistry.Unlock()
	C.save_first_memory_data(C.store_context(unsafe.Pointer(store.Context())), C.uint64_t(instance.store_id))

	fn, err := componentGetFunc(instance, store, entryPoint)
	if err != nil {
		return nil, fmt.Errorf("component get func: %w", err)
	}

	resultStr, callErr := componentCall(fn, store, string(input))
	if callErr != nil {
		return nil, fmt.Errorf("host: component export %q: %w", entryPoint, callErr)
	}

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
