//go:build cgo

package engine

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
	"github.com/cleat-team/cleat/wasm"
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
	cbTypeDefault            cbType = iota // deprecated fallback - returns 0

	// durable-call interface
	cbTypeDurableCallString                // (string,string,string) -> string
	cbTypeDurableCallRetry                 // (string,string,string,u64,u64,u64,u64,string) -> string
	cbTypeDurableCallHeartbeat             // (string,string,string,u64) -> string

	// durable-sleep interface
	cbTypeDurableSleep                     // (u64) -> u64
	cbTypeNow                              // () -> u64
	cbTypeRandom                           // () -> u64
	cbTypeDurableLog                       // (string) -> u64

	// durable-version interface
	cbTypeVersion                          // () -> u64
	cbTypeMinVersion                       // () -> u64

	// durable-lifecycle interface
	cbTypeDurableDefer                     // (string) -> string
	cbTypeContinueAsNew                    // (string) -> u64
	cbTypePollCancellation                 // () -> string

	// durable-signals interface
	cbTypeAwaitSignals                     // (string,u64,u32,u32,u32,u32) -> u64
	cbTypePollSignal                       // (string) -> string
	cbTypeSendSignalAndWait                // (string,string,string,u64) -> string
	cbTypeReplyToSignal                    // (string,string) -> u64
	cbTypeSignalWorkflow                   // (string,string,string) -> u64

	// durable-children interface
	cbTypeChildWorkflow                    // (string,string) -> string
	cbTypeAwaitChild                       // (string) -> string
	cbTypeAwaitAllChildren                 // (string) -> string
	cbTypeChildWorkflowWithOptions         // (string,string,u64,u64,string) -> string

	// durable-promises interface
	cbTypeCreatePromise                    // (string[,u64]) -> string (WIT has ttl-ms, handler doesn't)
	cbTypeAwaitPromise                     // (string,u64) -> string
	cbTypeResolvePromise                   // (string,string) -> u64
	cbTypeRejectPromise                    // (string,string) -> u64

	// durable-state interface
	cbTypeSetQueryState                    // (string,string) -> u64

	// durable-handlers interface
	cbTypeRegisterUpdateHandler            // (string) -> u64
	cbTypeRegisterQueryHandler             // (string) -> u64

	// durable-messaging interface
	cbTypeDurableSend                      // (string,string,string) -> u64
	cbTypeScheduleInvoke                   // (string,string,string,u64) -> u64

	// durable-identity interface
	cbTypeWorkflowID                       // () -> string
	cbTypeRunID                            // () -> string

	// plugin interface
	cbTypePluginCall                       // (string,string,string) -> string
	cbTypePluginCallStreaming              // (string,string,string) -> string

	// durable-lock interface
	cbTypeAcquireLock                      // (string,s64) -> s64
	cbTypeReleaseLock                      // (string) -> s64

	// durable-scope interface
	cbTypeSetScope                         // (string,string) -> string
	cbTypeGetScope                         // (u32,u32,u32,u32) -> u64
	cbTypeUUID                             // (string) -> string

	// durable-stream-state interface
	cbTypeSetState                         // (string,string) -> u64
	cbTypeGetState                         // (string) -> string
	cbTypeDeleteState                      // (string) -> u64
	cbTypeIncrState                        // (string,u64) -> u64
	cbTypeHasState                         // (string) -> u64
	cbTypeListState                        // (string) -> string

	// durable-extended-lifecycle interface
	cbTypeContinueAsNewVersioned           // (string,u32) -> u64
	cbTypeSideEffect                       // (string) -> string

	// durable-extended-children interface
	cbTypeChildWorkflowInSchema            // (string,string,string,u64,u64,string) -> string

	// durable-fetch interface
	cbTypeFetch                            // (string,string,string,string) -> string
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

// -- Go helper functions for reading/writing component vals ------------------

// argPtr returns a pointer to the i-th component val in the args array.
func argPtr(args *C.wasmtime_component_val_t, i int) *C.wasmtime_component_val_t {
	return (*C.wasmtime_component_val_t)(unsafe.Add(unsafe.Pointer(args), uintptr(i)*unsafe.Sizeof(*args)))
}

// readStrArg returns the i-th argument as a Go string, or "" if out of range.
func readStrArg(args *C.wasmtime_component_val_t, i int, nargs C.size_t) string {
	if int(nargs) <= i { return "" }
	a := argPtr(args, i)
	var slen C.size_t
	sdata := C.component_val_get_string(a, &slen)
	if sdata == nil { return "" }
	return C.GoStringN(sdata, C.int(slen))
}

// readU64Arg returns the i-th argument as a uint64, or 0 if out of range.
// Handles both WASMTIME_COMPONENT_U64 and WASMTIME_COMPONENT_S64 kinds.
func readU64Arg(args *C.wasmtime_component_val_t, i int, nargs C.size_t) uint64 {
	if int(nargs) <= i { return 0 }
	a := argPtr(args, i)
	return uint64(C.component_val_get_u64(a))
}

// readU32Arg returns the i-th argument as a uint32, or 0 if out of range.
// Handles both WASMTIME_COMPONENT_U32 and WASMTIME_COMPONENT_S32 kinds.
func readU32Arg(args *C.wasmtime_component_val_t, i int, nargs C.size_t) uint32 {
	if int(nargs) <= i { return 0 }
	a := argPtr(args, i)
	return uint32(C.component_val_get_u32(a))
}

// setResultU64 sets the first result value to a u64.
func setResultU64(results *C.wasmtime_component_val_t, nresults C.size_t, val uint64) {
	if int(nresults) < 1 { return }
	r := (*C.wasmtime_component_val_t)(unsafe.Pointer(results))
	C.component_val_set_u64(r, C.uint64_t(val))
}

// setResultString sets the first result value to a WIT string.
// The C string memory is intentionally leaked -- wasmtime reads it after the
// callback returns and the host cannot free it.
func setResultString(results *C.wasmtime_component_val_t, nresults C.size_t, s string) {
	if int(nresults) < 1 { return }
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

// dispatchDurableCallString handles (string, string, string) -> string.
func (b *wasmtimeBackend) dispatchDurableCallString(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil { return nil }
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.DurableCall(ctxWithMem(context.Background(), buf), nil, svc, op, req, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchDurableCallRetry handles (string,string,string,u64,u64,u64,u64,string) -> string.
func (b *wasmtimeBackend) dispatchDurableCallRetry(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 8 || b.handler == nil { return nil }
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)
	maxAttempts := int64(readU64Arg(args, 3, nargs))
	initialInterval := int64(readU64Arg(args, 4, nargs))
	backoffCoeff := int64(readU64Arg(args, 5, nargs))
	maxInterval := int64(readU64Arg(args, 6, nargs))
	nonRetryable := readStrArg(args, 7, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.DurableCallWithRetry(ctxWithMem(context.Background(), buf), nil,
		svc, op, req, maxAttempts, initialInterval, backoffCoeff, maxInterval, nonRetryable, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchDurableCallHeartbeat handles (string,string,string,u64) -> string.
func (b *wasmtimeBackend) dispatchDurableCallHeartbeat(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil { return nil }
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)
	heartbeatInterval := int64(readU64Arg(args, 3, nargs))

	buf := make([]byte, 65536)
	packed := b.handler.DurableCallWithHeartbeat(ctxWithMem(context.Background(), buf), nil,
		svc, op, req, heartbeatInterval, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-sleep interface
// ---------------------------------------------------------------------------

// dispatchDurableSleep handles (u64) -> u64.
func (b *wasmtimeBackend) dispatchDurableSleep(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	durationMs := int64(readU64Arg(args, 0, nargs))
	r := b.handler.DurableSleep(context.Background(), nil, durationMs)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchNow handles () -> u64.
func (b *wasmtimeBackend) dispatchNow(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil { return nil }
	r := b.handler.Now(context.Background())
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchRandom handles () -> u64.
func (b *wasmtimeBackend) dispatchRandom(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil { return nil }
	r := b.handler.Random(context.Background())
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchDurableLog handles (string) -> u64.
func (b *wasmtimeBackend) dispatchDurableLog(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	msg := readStrArg(args, 0, nargs)
	r := b.handler.DurableLog(context.Background(), nil, msg)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-version interface
// ---------------------------------------------------------------------------

// dispatchVersion handles () -> u64.
func (b *wasmtimeBackend) dispatchVersion(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil { return nil }
	r := b.handler.Version(context.Background())
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchMinVersion handles () -> u64.
func (b *wasmtimeBackend) dispatchMinVersion(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil { return nil }
	r := b.handler.MinVersion(context.Background())
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-lifecycle interface
// ---------------------------------------------------------------------------

// dispatchDurableDefer handles (string) -> string.
func (b *wasmtimeBackend) dispatchDurableDefer(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	desc := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.DurableDefer(ctxWithMem(context.Background(), buf), nil, desc, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchContinueAsNew handles (string) -> u64.
func (b *wasmtimeBackend) dispatchContinueAsNew(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	input := readStrArg(args, 0, nargs)
	r := b.handler.ContinueAsNew(context.Background(), nil, input)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchPollCancellation handles () -> string.
func (b *wasmtimeBackend) dispatchPollCancellation(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil { return nil }

	buf := make([]byte, 65536)
	packed := b.handler.PollCancellation(ctxWithMem(context.Background(), buf), nil, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-signals interface
// ---------------------------------------------------------------------------

// dispatchAwaitSignals handles (string,u64,u32,u32,u32,u32) -> u64.
// Output buffers are created via ctxWithMem; only the u64 return is surfaced.
func (b *wasmtimeBackend) dispatchAwaitSignals(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 6 || b.handler == nil { return nil }
	names := readStrArg(args, 0, nargs)
	timeoutMs := int64(readU64Arg(args, 1, nargs))

	// Create a double-sized buffer for two output params.
	buf := make([]byte, 131072)
	r := b.handler.DurableAwaitSignals(ctxWithMem(context.Background(), buf), nil,
		names, timeoutMs, 0, 65536, 65536, 65536)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchPollSignal handles (string) -> string.
func (b *wasmtimeBackend) dispatchPollSignal(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	name := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.PollSignal(ctxWithMem(context.Background(), buf), nil, name, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchSendSignalAndWait handles (string,string,string,u64) -> string.
func (b *wasmtimeBackend) dispatchSendSignalAndWait(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil { return nil }
	target := readStrArg(args, 0, nargs)
	sigName := readStrArg(args, 1, nargs)
	payload := readStrArg(args, 2, nargs)
	timeoutMs := int64(readU64Arg(args, 3, nargs))

	buf := make([]byte, 65536)
	packed := b.handler.SendSignalAndWait(ctxWithMem(context.Background(), buf), nil,
		target, sigName, payload, timeoutMs, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchReplyToSignal handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchReplyToSignal(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	correlationID := readStrArg(args, 0, nargs)
	response := readStrArg(args, 1, nargs)
	r := b.handler.ReplyToSignal(context.Background(), nil, correlationID, response)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchSignalWorkflow handles (string,string,string) -> u64.
func (b *wasmtimeBackend) dispatchSignalWorkflow(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil { return nil }
	target := readStrArg(args, 0, nargs)
	sigName := readStrArg(args, 1, nargs)
	payload := readStrArg(args, 2, nargs)
	r := b.handler.SignalWorkflow(context.Background(), nil, target, sigName, payload)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-children interface
// ---------------------------------------------------------------------------

// dispatchChildWorkflow handles (string,string) -> string.
func (b *wasmtimeBackend) dispatchChildWorkflow(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	name := readStrArg(args, 0, nargs)
	input := readStrArg(args, 1, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ChildWorkflow(ctxWithMem(context.Background(), buf), nil, name, input, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchAwaitChild handles (string) -> string.
func (b *wasmtimeBackend) dispatchAwaitChild(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	runID := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.AwaitChild(ctxWithMem(context.Background(), buf), nil, runID, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchAwaitAllChildren handles (string) -> string.
func (b *wasmtimeBackend) dispatchAwaitAllChildren(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	runIDsJSON := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.AwaitAllChildren(ctxWithMem(context.Background(), buf), nil, runIDsJSON, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchChildWorkflowWithOptions handles (string,string,u64,u64,string) -> string.
func (b *wasmtimeBackend) dispatchChildWorkflowWithOptions(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 5 || b.handler == nil { return nil }
	name := readStrArg(args, 0, nargs)
	input := readStrArg(args, 1, nargs)
	version := int64(readU64Arg(args, 2, nargs))
	priority := int64(readU64Arg(args, 3, nargs))
	policy := readStrArg(args, 4, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ChildWorkflowWithOptions(ctxWithMem(context.Background(), buf), nil,
		name, input, version, priority, policy, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-promises interface
// ---------------------------------------------------------------------------

// dispatchCreatePromise handles (string[,u64]) -> string.
// WIT defines a second arg (ttl-ms: u64) which the handler does not accept.
func (b *wasmtimeBackend) dispatchCreatePromise(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	name := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.CreatePromise(ctxWithMem(context.Background(), buf), nil, name, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchAwaitPromise handles (string,u64) -> string.
func (b *wasmtimeBackend) dispatchAwaitPromise(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	id := readStrArg(args, 0, nargs)
	timeoutMs := int64(readU64Arg(args, 1, nargs))

	buf := make([]byte, 65536)
	packed := b.handler.AwaitPromise(ctxWithMem(context.Background(), buf), nil, id, timeoutMs, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchResolvePromise handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchResolvePromise(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	id := readStrArg(args, 0, nargs)
	val := readStrArg(args, 1, nargs)
	r := b.handler.ResolvePromise(context.Background(), nil, id, val)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchRejectPromise handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchRejectPromise(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	id := readStrArg(args, 0, nargs)
	errMsg := readStrArg(args, 1, nargs)
	r := b.handler.RejectPromise(context.Background(), nil, id, errMsg)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-state interface
// ---------------------------------------------------------------------------

// dispatchSetQueryState handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchSetQueryState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	key := readStrArg(args, 0, nargs)
	val := readStrArg(args, 1, nargs)
	r := b.handler.SetQueryState(context.Background(), nil, key, val)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-handlers interface
// ---------------------------------------------------------------------------

// dispatchRegisterUpdateHandler handles (string) -> u64.
func (b *wasmtimeBackend) dispatchRegisterUpdateHandler(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	name := readStrArg(args, 0, nargs)
	r := b.handler.RegisterUpdateHandler(context.Background(), nil, name)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchRegisterQueryHandler handles (string) -> u64.
func (b *wasmtimeBackend) dispatchRegisterQueryHandler(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	name := readStrArg(args, 0, nargs)
	r := b.handler.RegisterQueryHandler(context.Background(), nil, name)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-messaging interface
// ---------------------------------------------------------------------------

// dispatchDurableSend handles (string,string,string) -> u64.
func (b *wasmtimeBackend) dispatchDurableSend(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil { return nil }
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)
	r := b.handler.DurableSend(context.Background(), nil, svc, op, req)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchScheduleInvoke handles (string,string,string,u64) -> u64.
func (b *wasmtimeBackend) dispatchScheduleInvoke(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil { return nil }
	svc := readStrArg(args, 0, nargs)
	op := readStrArg(args, 1, nargs)
	req := readStrArg(args, 2, nargs)
	delayMs := int64(readU64Arg(args, 3, nargs))
	r := b.handler.DurableScheduleInvoke(context.Background(), nil, svc, op, req, delayMs)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-identity interface
// ---------------------------------------------------------------------------

// dispatchWorkflowID handles () -> string.
func (b *wasmtimeBackend) dispatchWorkflowID(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil { return nil }

	buf := make([]byte, 65536)
	packed := b.handler.WorkflowID(ctxWithMem(context.Background(), buf), nil, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchRunID handles () -> string.
func (b *wasmtimeBackend) dispatchRunID(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if b.handler == nil { return nil }

	buf := make([]byte, 65536)
	packed := b.handler.RunID(ctxWithMem(context.Background(), buf), nil, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// plugin interface
// ---------------------------------------------------------------------------

// dispatchPluginCall handles (string,string,string) -> string.
func (b *wasmtimeBackend) dispatchPluginCall(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil { return nil }
	pluginName := readStrArg(args, 0, nargs)
	funcName := readStrArg(args, 1, nargs)
	input := readStrArg(args, 2, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.PluginCall(ctxWithMem(context.Background(), buf), nil, pluginName, funcName, input, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchPluginCallStreaming handles (string,string,string) -> string.
func (b *wasmtimeBackend) dispatchPluginCallStreaming(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 3 || b.handler == nil { return nil }
	pluginName := readStrArg(args, 0, nargs)
	funcName := readStrArg(args, 1, nargs)
	input := readStrArg(args, 2, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.PluginCallStreaming(ctxWithMem(context.Background(), buf), nil, pluginName, funcName, input, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-lock interface
// ---------------------------------------------------------------------------

// dispatchAcquireLock handles (string,s64) -> s64.
func (b *wasmtimeBackend) dispatchAcquireLock(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	key := readStrArg(args, 0, nargs)
	ttlMs := int64(readU64Arg(args, 1, nargs))
	r := b.handler.AcquireLock(context.Background(), nil, key, ttlMs)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchReleaseLock handles (string) -> s64.
func (b *wasmtimeBackend) dispatchReleaseLock(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	key := readStrArg(args, 0, nargs)
	r := b.handler.ReleaseLock(context.Background(), nil, key)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// ---------------------------------------------------------------------------
// durable-scope interface
// ---------------------------------------------------------------------------

// dispatchSetScope handles (string,string) -> string.
func (b *wasmtimeBackend) dispatchSetScope(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	objType := readStrArg(args, 0, nargs)
	instKey := readStrArg(args, 1, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.SetScope(ctxWithMem(context.Background(), buf), nil, objType, instKey, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchGetScope handles (u32,u32,u32,u32) -> u64.
// Output buffers are created via ctxWithMem; only the u64 return is surfaced.
func (b *wasmtimeBackend) dispatchGetScope(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil { return nil }

	// Create a double-sized buffer for two output params.
	buf := make([]byte, 131072)
	r := b.handler.GetScope(ctxWithMem(context.Background(), buf), nil, 0, 65536, 65536, 65536)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchUUID handles (string) -> string.
func (b *wasmtimeBackend) dispatchUUID(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	seed := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.UUID(ctxWithMem(context.Background(), buf), nil, seed, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-stream-state interface
// ---------------------------------------------------------------------------

// dispatchSetState handles (string,string) -> u64.
func (b *wasmtimeBackend) dispatchSetState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	key := readStrArg(args, 0, nargs)
	val := readStrArg(args, 1, nargs)
	r := b.handler.SetState(context.Background(), nil, key, val)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchGetState handles (string) -> string.
func (b *wasmtimeBackend) dispatchGetState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	key := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.GetState(ctxWithMem(context.Background(), buf), nil, key, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// dispatchDeleteState handles (string) -> u64.
func (b *wasmtimeBackend) dispatchDeleteState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	key := readStrArg(args, 0, nargs)
	r := b.handler.DeleteState(context.Background(), nil, key)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchIncrState handles (string,u64) -> u64.
func (b *wasmtimeBackend) dispatchIncrState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	key := readStrArg(args, 0, nargs)
	delta := int64(readU64Arg(args, 1, nargs))
	r := b.handler.IncrState(context.Background(), nil, key, delta)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchHasState handles (string) -> u64.
func (b *wasmtimeBackend) dispatchHasState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	key := readStrArg(args, 0, nargs)
	r := b.handler.HasState(context.Background(), nil, key)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchListState handles (string) -> string.
func (b *wasmtimeBackend) dispatchListState(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	prefix := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ListState(ctxWithMem(context.Background(), buf), nil, prefix, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-extended-lifecycle interface
// ---------------------------------------------------------------------------

// dispatchContinueAsNewVersioned handles (string,u32) -> u64.
func (b *wasmtimeBackend) dispatchContinueAsNewVersioned(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 2 || b.handler == nil { return nil }
	input := readStrArg(args, 0, nargs)
	newVersion := int(readU32Arg(args, 1, nargs))
	r := b.handler.ContinueAsNewWithVersion(context.Background(), nil, input, newVersion)
	setResultU64(results, nresults, uint64(r))
	return nil
}

// dispatchSideEffect handles (string) -> string.
func (b *wasmtimeBackend) dispatchSideEffect(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 1 || b.handler == nil { return nil }
	result := readStrArg(args, 0, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.SideEffect(ctxWithMem(context.Background(), buf), nil, result, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-extended-children interface
// ---------------------------------------------------------------------------

// dispatchChildWorkflowInSchema handles (string,string,string,u64,u64,string) -> string.
func (b *wasmtimeBackend) dispatchChildWorkflowInSchema(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 6 || b.handler == nil { return nil }
	schema := readStrArg(args, 0, nargs)
	name := readStrArg(args, 1, nargs)
	input := readStrArg(args, 2, nargs)
	version := int64(readU64Arg(args, 3, nargs))
	priority := int64(readU64Arg(args, 4, nargs))
	policy := readStrArg(args, 5, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.ChildWorkflowInSchema(ctxWithMem(context.Background(), buf), nil,
		schema, name, input, version, priority, policy, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// durable-fetch interface
// ---------------------------------------------------------------------------

// dispatchFetch handles (string,string,string,string) -> string.
func (b *wasmtimeBackend) dispatchFetch(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	if int(nargs) < 4 || b.handler == nil { return nil }
	method := readStrArg(args, 0, nargs)
	url := readStrArg(args, 1, nargs)
	headers := readStrArg(args, 2, nargs)
	body := readStrArg(args, 3, nargs)

	buf := make([]byte, 65536)
	packed := b.handler.Fetch(ctxWithMem(context.Background(), buf), nil, method, url, headers, body, 0, 65536)
	response := extractStringFromPacked(packed, buf)

	setResultString(results, nresults, response)
	return nil
}

// ---------------------------------------------------------------------------
// deprecated fallback
// ---------------------------------------------------------------------------

// dispatchComponentDefault is the deprecated fallback for unregistered functions.
// It returns 0 for all results.
func (b *wasmtimeBackend) dispatchComponentDefault(
	args *C.wasmtime_component_val_t, nargs C.size_t,
	results *C.wasmtime_component_val_t, nresults C.size_t,
) *C.wasmtime_error_t {
	setResultU64(results, nresults, 0)
	return nil
}

// -- WIT module/function -> cbType map ---------------------------------------

// witTypeMap maps WIT module-name / function-name pairs to their cbType,
// used by registerCleatComponentImports to register each import correctly.
var witTypeMap = map[string]map[string]cbType{
	"cleat:host-calls/durable-call": {
		"durable-call":             cbTypeDurableCallString,
		"durable-call-retry":       cbTypeDurableCallRetry,
		"durable-call-heartbeat":   cbTypeDurableCallHeartbeat,
	},
	"cleat:host-calls/durable-sleep": {
		"durable-sleep":  cbTypeDurableSleep,
		"durable-now":    cbTypeNow,
		"durable-random": cbTypeRandom,
		"durable-log":    cbTypeDurableLog,
	},
	"cleat:host-calls/durable-version": {
		"durable-version":     cbTypeVersion,
		"durable-min-version": cbTypeMinVersion,
	},
	"cleat:host-calls/durable-lifecycle": {
		"durable-defer":             cbTypeDurableDefer,
		"durable-continue-as-new":   cbTypeContinueAsNew,
		"durable-poll-cancellation": cbTypePollCancellation,
	},
	"cleat:host-calls/durable-signals": {
		"durable-await-signals":        cbTypeAwaitSignals,
		"durable-poll-signal":          cbTypePollSignal,
		"durable-send-signal-and-wait": cbTypeSendSignalAndWait,
		"durable-reply-to-signal":      cbTypeReplyToSignal,
		"durable-signal-workflow":      cbTypeSignalWorkflow,
	},
	"cleat:host-calls/durable-children": {
		"durable-child-workflow":             cbTypeChildWorkflow,
		"durable-await-child":                cbTypeAwaitChild,
		"durable-await-all-children":         cbTypeAwaitAllChildren,
		"durable-child-workflow-with-options": cbTypeChildWorkflowWithOptions,
	},
	"cleat:host-calls/durable-promises": {
		"durable-create-promise":  cbTypeCreatePromise,
		"durable-await-promise":   cbTypeAwaitPromise,
		"durable-resolve-promise": cbTypeResolvePromise,
		"durable-reject-promise":  cbTypeRejectPromise,
	},
	"cleat:host-calls/durable-state": {
		"set-query-state": cbTypeSetQueryState,
	},
	"cleat:host-calls/durable-handlers": {
		"durable-register-update-handler": cbTypeRegisterUpdateHandler,
		"durable-register-query-handler":  cbTypeRegisterQueryHandler,
	},
	"cleat:host-calls/durable-messaging": {
		"durable-send":            cbTypeDurableSend,
		"durable-schedule-invoke": cbTypeScheduleInvoke,
	},
	"cleat:host-calls/durable-identity": {
		"durable-workflow-id": cbTypeWorkflowID,
		"durable-run-id":      cbTypeRunID,
	},
	"cleat:host-calls/plugin": {
		"plugin-call":           cbTypePluginCall,
		"plugin-call-streaming": cbTypePluginCallStreaming,
	},
	"cleat:host-calls/durable-lock": {
		"durable-acquire-lock": cbTypeAcquireLock,
		"durable-release-lock": cbTypeReleaseLock,
	},
	"cleat:host-calls/durable-scope": {
		"set-scope": cbTypeSetScope,
		"get-scope": cbTypeGetScope,
		"uuid":      cbTypeUUID,
	},
	"cleat:host-calls/durable-stream-state": {
		"set-state":    cbTypeSetState,
		"get-state":    cbTypeGetState,
		"delete-state": cbTypeDeleteState,
		"incr-state":   cbTypeIncrState,
		"has-state":    cbTypeHasState,
		"list-state":   cbTypeListState,
	},
	"cleat:host-calls/durable-extended-lifecycle": {
		"continue-as-new-versioned": cbTypeContinueAsNewVersioned,
		"side-effect":               cbTypeSideEffect,
	},
	"cleat:host-calls/durable-extended-children": {
		"child-workflow-in-schema": cbTypeChildWorkflowInSchema,
	},
	"cleat:host-calls/durable-fetch": {
		"fetch": cbTypeFetch,
	},
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
			// Look up the cbType from witTypeMap, falling back to cbTypeDefault.
			fnType := cbTypeDefault
			if moduleTypes, ok := witTypeMap[witModule]; ok {
				if t, ok := moduleTypes[witFuncName]; ok {
					fnType = t
				}
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
