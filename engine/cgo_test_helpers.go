//go:build cgo

// Every symbol this file defines is only ever called from other _test.go
// files (component_cgo_test.go), so in spirit this is test-only code. It is
// deliberately NOT named *_test.go, though: Go (as of at least 1.26) rejects
// `import "C"` in any _test.go file outright ("use of cgo in test ... not
// supported"), for both `go build` and `go test`. So this file has to be an
// ordinary .go file to be usable at all.
//
// It used to carry the wasmtime_component_cgo tag to stay out of default
// builds. That tag is gone (see component_cgo.go), so these helpers now
// compile into ordinary builds. scripts/check-test-only-code.sh does not
// report them -- staticcheck's U1000 does not see through cgo -- which is
// worth knowing rather than relying on: the detector is blind here, so if
// these helpers ever grow a production caller by accident, nothing will say
// so.

package engine

// #include <wasmtime.h>
// #include <wasmtime/component/component.h>
// #include <wasmtime/component/linker.h>
// #include <wasmtime/component/instance.h>
// #include <wasmtime/component/func.h>
// #include <wasmtime/component/val.h>
// #include <stdlib.h>
//
// extern wasmtime_error_t *goComponentCallback(
//     void *env, wasmtime_context_t *ctx,
//     wasmtime_component_func_type_t *ty,
//     wasmtime_component_val_t *args, size_t nargs,
//     wasmtime_component_val_t *results, size_t nresults);
//
// // See cbid_as_env in component_callbacks.go. A separate copy because cgo
// // compiles each file's preamble into its own translation unit, so a static
// // function in one is not visible from another.
// static void *cbid_as_env_test(uintptr_t id) { return (void *)id; }
//
// static wasmtime_component_val_t cgotest_make_u64(uint64_t v) {
//     wasmtime_component_val_t val;
//     val.kind = WASMTIME_COMPONENT_U64;
//     val.of.u64 = v;
//     return val;
// }
//
// static wasmtime_component_val_t cgotest_make_u32(uint32_t v) {
//     wasmtime_component_val_t val;
//     val.kind = WASMTIME_COMPONENT_U32;
//     val.of.u32 = v;
//     return val;
// }
//
// static wasmtime_component_val_t cgotest_make_string(const char *s, size_t len) {
//     wasmtime_component_val_t val;
//     val.kind = WASMTIME_COMPONENT_STRING;
//     val.of.string.data = (char *)s;
//     val.of.string.size = len;
//     return val;
// }
//
// static uint64_t cgotest_val_get_u64(const wasmtime_component_val_t *v) {
//     if (v->kind == WASMTIME_COMPONENT_U64) { return v->of.u64; }
//     if (v->kind == WASMTIME_COMPONENT_S64) { return (uint64_t)v->of.s64; }
//     return 0;
// }
//
// static const char *cgotest_val_get_string(const wasmtime_component_val_t *v, size_t *len) {
//     if (v->kind == WASMTIME_COMPONENT_STRING) {
//         *len = v->of.string.size;
//         return v->of.string.data;
//     }
//     *len = 0;
//     return NULL;
// }
import "C"
import (
	"fmt"
	"unsafe"
)

func cgotestMakeStrArgs(strs ...string) (ptr unsafe.Pointer, count int, free func()) {
	vals := make([]C.wasmtime_component_val_t, len(strs))
	frees := make([]unsafe.Pointer, 0, len(strs))
	for i, s := range strs {
		cStr := C.CString(s)
		frees = append(frees, unsafe.Pointer(cStr))
		vals[i] = C.cgotest_make_string(cStr, C.size_t(len(s)))
	}
	if len(vals) == 0 {
		return nil, 0, func() {}
	}
	return unsafe.Pointer(&vals[0]), len(strs), func() {
		for _, p := range frees {
			C.free(p)
		}
	}
}

func cgotestMakeU64Args(vals ...uint64) (ptr unsafe.Pointer, count int, free func()) {
	v := make([]C.wasmtime_component_val_t, len(vals))
	for i, val := range vals {
		v[i] = C.cgotest_make_u64(C.uint64_t(val))
	}
	if len(v) == 0 {
		return nil, 0, func() {}
	}
	return unsafe.Pointer(&v[0]), len(vals), func() {}
}

func cgotestMakeU32Arg(v uint32) unsafe.Pointer {
	p := new(C.wasmtime_component_val_t)
	*p = C.cgotest_make_u32(C.uint32_t(v))
	return unsafe.Pointer(p)
}

func cgotestMakeMixedArgs(args ...any) (ptr unsafe.Pointer, count int, free func()) {
	vals := make([]C.wasmtime_component_val_t, len(args))
	frees := make([]unsafe.Pointer, 0)
	for i, arg := range args {
		switch v := arg.(type) {
		case string:
			cStr := C.CString(v)
			frees = append(frees, unsafe.Pointer(cStr))
			vals[i] = C.cgotest_make_string(cStr, C.size_t(len(v)))
		case uint64:
			vals[i] = C.cgotest_make_u64(C.uint64_t(v))
		case uint32:
			vals[i] = C.cgotest_make_u32(C.uint32_t(v))
		}
	}
	if len(vals) == 0 {
		return nil, 0, func() {}
	}
	return unsafe.Pointer(&vals[0]), len(args), func() {
		for _, p := range frees {
			C.free(p)
		}
	}
}

func cgotestAllocResult() unsafe.Pointer {
	return unsafe.Pointer(new(C.wasmtime_component_val_t))
}

func cgotestReadResultU64(r unsafe.Pointer) uint64 {
	return uint64(C.cgotest_val_get_u64((*C.wasmtime_component_val_t)(r)))
}

func cgotestHasResultString(r unsafe.Pointer) bool {
	var slen C.size_t
	return C.cgotest_val_get_string((*C.wasmtime_component_val_t)(r), &slen) != nil
}

func (b *wasmtimeBackend) cgotestDispatchStr(method int, argsPtr unsafe.Pointer, nargs int, resultPtr unsafe.Pointer) error {
	dispatch := map[int]func(*C.wasmtime_component_val_t, C.size_t, *C.wasmtime_component_val_t, C.size_t) *C.wasmtime_error_t{
		0:  b.dispatchDurableCallString,
		1:  b.dispatchDurableCallRetry,
		2:  b.dispatchDurableCallHeartbeat,
		3:  b.dispatchDurableDefer,
		4:  b.dispatchChildWorkflow,
		5:  b.dispatchCreatePromise,
		6:  b.dispatchPluginCall,
		7:  b.dispatchSetScope,
		8:  b.dispatchGetState,
		9:  b.dispatchListState,
		10: b.dispatchPollCancellation,
		11: b.dispatchWorkflowID,
		12: b.dispatchRunID,
		13: b.dispatchUUID,
		14: b.dispatchSideEffect,
		15: b.dispatchFetch,
		17: b.dispatchAwaitChild,
		18: b.dispatchAwaitAllChildren,
		19: b.dispatchPluginCallStreaming,
		20: b.dispatchChildWorkflowWithOptions,
		21: b.dispatchSendSignalAndWait,
		22: b.dispatchAwaitPromise,
		23: b.dispatchPollSignal,
	}[method]
	err := dispatch((*C.wasmtime_component_val_t)(argsPtr), C.size_t(nargs), (*C.wasmtime_component_val_t)(resultPtr), 1)
	if err != nil {
		var msg C.wasm_byte_vec_t
		C.wasmtime_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(err)
		return fmt.Errorf("dispatch error: %s", s)
	}
	return nil
}

func (b *wasmtimeBackend) cgotestDispatchU64(method int, argsPtr unsafe.Pointer, nargs int, resultPtr unsafe.Pointer) error {
	dispatch := map[int]func(*C.wasmtime_component_val_t, C.size_t, *C.wasmtime_component_val_t, C.size_t) *C.wasmtime_error_t{
		0:  b.dispatchDurableSleep,
		1:  b.dispatchNow,
		2:  b.dispatchRandom,
		3:  b.dispatchVersion,
		4:  b.dispatchMinVersion,
		5:  b.dispatchDurableLog,
		6:  b.dispatchContinueAsNew,
		7:  b.dispatchResolvePromise,
		8:  b.dispatchRejectPromise,
		9:  b.dispatchSetQueryState,
		10: b.dispatchDurableSend,
		11: b.dispatchSetState,
		12: b.dispatchIncrState,
		13: b.dispatchHasState,
		14: b.dispatchDeleteState,
		15: b.dispatchAcquireLock,
		16: b.dispatchReleaseLock,
		17: b.dispatchSignalWorkflow,
		18: b.dispatchReplyToSignal,
		19: b.dispatchScheduleInvoke,
		20: b.dispatchRegisterUpdateHandler,
		21: b.dispatchRegisterQueryHandler,
		22: b.dispatchContinueAsNewVersioned,
		23: b.dispatchGetScope,
		24: b.dispatchAwaitSignals,
		25: b.dispatchComponentDefault,
	}[method]
	err := dispatch((*C.wasmtime_component_val_t)(argsPtr), C.size_t(nargs), (*C.wasmtime_component_val_t)(resultPtr), 1)
	if err != nil {
		var msg C.wasm_byte_vec_t
		C.wasmtime_error_message(err, &msg)
		s := C.GoStringN(msg.data, C.int(msg.size))
		C.wasm_byte_vec_delete(&msg)
		C.wasmtime_error_delete(err)
		return fmt.Errorf("dispatch error: %s", s)
	}
	return nil
}

// cgotestGoComponentCallback invokes the callback exactly as wasmtime does,
// taking the registry id rather than a pointer.
//
// It takes a uintptr and casts in C for the same reason
// component_callbacks.go's cbid_as_env does: the id is a token, and spelling
// it as a Go unsafe.Pointer is both a vet finding and an invalid Go pointer in
// a pointer-typed slot. Callers used to write unsafe.Pointer(id) at each call
// site, which put four copies of that in the test file.
func cgotestGoComponentCallback(cbID uintptr, argsPtr unsafe.Pointer, nargs int, resultsPtr unsafe.Pointer) *C.wasmtime_error_t {
	return C.goComponentCallback(C.cbid_as_env_test(C.uintptr_t(cbID)), nil, nil, (*C.wasmtime_component_val_t)(argsPtr), C.size_t(nargs), (*C.wasmtime_component_val_t)(resultsPtr), 1)
}
