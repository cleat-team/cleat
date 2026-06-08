//go:build cgo

package engine

// #cgo CFLAGS:-I/tmp/wasmtime-v45/wasmtime-v45.0.0-x86_64-linux-c-api/include -I/home/rcownie/go/pkg/mod/github.com/bytecodealliance/wasmtime-go/v44@v44.0.0/build/include
// #cgo linux,amd64 LDFLAGS:-L/tmp/wasmtime-v45/wasmtime-v45.0.0-x86_64-linux-c-api/lib -lwasmtime -lm -ldl -pthread
// #include <wasmtime.h>
// #include <wasmtime/component/val.h>
// #include <stdlib.h>
// #include <string.h>
//
// static void t_set_u64(wasmtime_component_val_t *v, uint64_t val) {
//     v->kind = WASMTIME_COMPONENT_U64;
//     v->of.u64 = val;
// }
// static void t_set_u32(wasmtime_component_val_t *v, uint32_t val) {
//     v->kind = WASMTIME_COMPONENT_U32;
//     v->of.u32 = val;
// }
// static void t_set_s64(wasmtime_component_val_t *v, int64_t val) {
//     v->kind = WASMTIME_COMPONENT_S64;
//     v->of.s64 = val;
// }
// static void t_set_s32(wasmtime_component_val_t *v, int32_t val) {
//     v->kind = WASMTIME_COMPONENT_S32;
//     v->of.s32 = val;
// }
// static void t_set_string(wasmtime_component_val_t *v, const char *s, size_t len) {
//     v->kind = WASMTIME_COMPONENT_STRING;
//     v->of.string.data = (char *)s;
//     v->of.string.size = len;
// }
// static uint64_t t_get_u64(const wasmtime_component_val_t *v) {
//     if (v->kind == WASMTIME_COMPONENT_U64) return v->of.u64;
//     if (v->kind == WASMTIME_COMPONENT_S64) return (uint64_t)v->of.s64;
//     return 0;
// }
// static uint32_t t_get_u32(const wasmtime_component_val_t *v) {
//     if (v->kind == WASMTIME_COMPONENT_U32) return v->of.u32;
//     if (v->kind == WASMTIME_COMPONENT_S32) return (uint32_t)v->of.s32;
//     return 0;
// }
// static const char *t_get_string(const wasmtime_component_val_t *v, size_t *len) {
//     if (v->kind == WASMTIME_COMPONENT_STRING) { *len = v->of.string.size; return v->of.string.data; }
//     *len = 0; return NULL;
// }
// static wasmtime_component_valkind_t t_get_kind(const wasmtime_component_val_t *v) {
//     return v->kind;
// }
import "C"

import (
	"context"
	"testing"
	"unsafe"

	"github.com/tetratelabs/wazero/api"
)

// ==========================================================================
// stubHostHandler — all 54 HostHandler methods, default return 0.
// ==========================================================================

type stubHostHandler struct{}

func (s stubHostHandler) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	return 0
}
func (s stubHostHandler) DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	return 0
}
func (s stubHostHandler) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	return 0
}
func (s stubHostHandler) ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64 {
	return 0
}
func (s stubHostHandler) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) ChildWorkflowInSchema(ctx context.Context, m api.Module, targetSchema, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) PollChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) AwaitAnyChild(ctx context.Context, m api.Module, runIDsJSON string, resultPtr, resultMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) DurableCallWithRetry(ctx context.Context, m api.Module, service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string, responsePtr, responseMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) Version(ctx context.Context) int64  { return 0 }
func (s stubHostHandler) MinVersion(ctx context.Context) int64 { return 0 }
func (s stubHostHandler) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	return 0
}
func (s stubHostHandler) Now(ctx context.Context) int64    { return 0 }
func (s stubHostHandler) Random(ctx context.Context) int64  { return 0 }
func (s stubHostHandler) CreatePromise(ctx context.Context, m api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) PluginCall(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) PluginCallStreaming(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	return 0
}
func (s stubHostHandler) SendSignalAndWait(ctx context.Context, m api.Module, targetRunID, signalName, payload string, timeoutMs int64, responsePtr, responseMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) ReplyToSignal(ctx context.Context, m api.Module, correlationID, response string) int64 {
	return 0
}
func (s stubHostHandler) SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64 {
	return 0
}
func (s stubHostHandler) SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	return 0
}
func (s stubHostHandler) ReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	return 0
}
func (s stubHostHandler) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	return 0
}
func (s stubHostHandler) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	return 0
}
func (s stubHostHandler) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	return 0
}
func (s stubHostHandler) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	return 0
}
func (s stubHostHandler) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	return 0
}
func (s stubHostHandler) SetState(ctx context.Context, m api.Module, key, value string) int64 {
	return 0
}
func (s stubHostHandler) GetState(ctx context.Context, m api.Module, key string, valuePtr, valueMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) DeleteState(ctx context.Context, m api.Module, key string) int64 {
	return 0
}
func (s stubHostHandler) IncrState(ctx context.Context, m api.Module, key string, delta int64) int64 {
	return 0
}
func (s stubHostHandler) HasState(ctx context.Context, m api.Module, key string) int64 { return 0 }
func (s stubHostHandler) ListState(ctx context.Context, m api.Module, prefix string, keysPtr, keysMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) RunDetached(ctx context.Context, m api.Module, name, inputJSON string) int64 {
	return 0
}
func (s stubHostHandler) Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) JsonParse(ctx context.Context, m api.Module, jsonPtr, jsonLen, outPtr, outMaxLen uint32) int64 {
	return 0
}
func (s stubHostHandler) JsonStringify(ctx context.Context, m api.Module, ptr, len, outPtr, outMaxLen uint32) int64 {
	return 0
}

var _ HostHandler = stubHostHandler{}

// ==========================================================================
// Custom handler types for dispatch test mocking
// All prefixed to avoid conflicts with existing engine types.
// ==========================================================================

type tDurationHandler struct {
	stubHostHandler
	sleepResult int64
}

func (h *tDurationHandler) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	return h.sleepResult
}

type tNowHandler struct {
	stubHostHandler
	nowResult int64
}

func (h *tNowHandler) Now(ctx context.Context) int64 { return h.nowResult }

type tRandomHandler struct {
	stubHostHandler
	randomResult int64
}

func (h *tRandomHandler) Random(ctx context.Context) int64 { return h.randomResult }

type tLogHandler struct {
	stubHostHandler
	logResult int64
}

func (h *tLogHandler) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	return h.logResult
}

type tVersionHandler struct {
	stubHostHandler
	versionResult int64
}

func (h *tVersionHandler) Version(ctx context.Context) int64 { return h.versionResult }

type tMinVersionHandler struct {
	stubHostHandler
	result int64
}

func (h *tMinVersionHandler) MinVersion(ctx context.Context) int64 { return h.result }

type tContinueAsNewHandler struct {
	stubHostHandler
	result int64
}

func (h *tContinueAsNewHandler) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	return h.result
}

type tReplyToSignalHandler struct {
	stubHostHandler
	result int64
}

func (h *tReplyToSignalHandler) ReplyToSignal(ctx context.Context, m api.Module, correlationID, response string) int64 {
	return h.result
}

type tSignalWorkflowHandler struct {
	stubHostHandler
	result int64
}

func (h *tSignalWorkflowHandler) SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64 {
	return h.result
}

type tResolvePromiseHandler struct {
	stubHostHandler
	result int64
}

func (h *tResolvePromiseHandler) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	return h.result
}

type tRejectPromiseHandler struct {
	stubHostHandler
	result int64
}

func (h *tRejectPromiseHandler) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	return h.result
}

type tSetQueryStateHandler struct {
	stubHostHandler
	result int64
}

func (h *tSetQueryStateHandler) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	return h.result
}

type tRegisterUpdateHandlerHandler struct {
	stubHostHandler
	result int64
}

func (h *tRegisterUpdateHandlerHandler) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	return h.result
}

type tDurableSendHandler struct {
	stubHostHandler
	result int64
}

func (h *tDurableSendHandler) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	return h.result
}

type tScheduleInvokeHandler struct {
	stubHostHandler
	result int64
}

func (h *tScheduleInvokeHandler) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	return h.result
}

type tReleaseLockHandler struct {
	stubHostHandler
	result int64
}

func (h *tReleaseLockHandler) ReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	return h.result
}

type tAcquireLockHandler struct {
	stubHostHandler
	result int64
}

func (h *tAcquireLockHandler) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	return h.result
}

type tSetStateHandler struct {
	stubHostHandler
	result int64
}

func (h *tSetStateHandler) SetState(ctx context.Context, m api.Module, key, value string) int64 {
	return h.result
}

type tDeleteStateHandler struct {
	stubHostHandler
	result int64
}

func (h *tDeleteStateHandler) DeleteState(ctx context.Context, m api.Module, key string) int64 {
	return h.result
}

type tIncrStateHandler struct {
	stubHostHandler
	result int64
}

func (h *tIncrStateHandler) IncrState(ctx context.Context, m api.Module, key string, delta int64) int64 {
	return h.result
}

type tHasStateHandler struct {
	stubHostHandler
	result int64
}

func (h *tHasStateHandler) HasState(ctx context.Context, m api.Module, key string) int64 {
	return h.result
}

type tSideEffectHandler struct {
	stubHostHandler
	result int64
}

func (h *tSideEffectHandler) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	return h.result
}

type tContinueAsNewVersionedHandler struct {
	stubHostHandler
	result int64
}

func (h *tContinueAsNewVersionedHandler) ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64 {
	return h.result
}

type tRegisterQueryHandlerHandler struct {
	stubHostHandler
	result int64
}

func (h *tRegisterQueryHandlerHandler) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	return h.result
}

type tDurableDeferHandler struct {
	stubHostHandler
	result int64
}

func (h *tDurableDeferHandler) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	return h.result
}

type tPollCancellationHandler struct {
	stubHostHandler
	result int64
}

func (h *tPollCancellationHandler) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	return h.result
}

type tDurableCallStringHandler struct {
	stubHostHandler
	result int64
}

func (h *tDurableCallStringHandler) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.result
}

type tChildWorkflowHandler struct {
	stubHostHandler
	result int64
}

func (h *tChildWorkflowHandler) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	return h.result
}

type tAwaitChildHandler struct {
	stubHostHandler
	result int64
}

func (h *tAwaitChildHandler) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	return h.result
}

type tCreatePromiseHandler struct {
	stubHostHandler
	result int64
}

func (h *tCreatePromiseHandler) CreatePromise(ctx context.Context, m api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64 {
	return h.result
}

type tAwaitPromiseHandler struct {
	stubHostHandler
	result int64
}

func (h *tAwaitPromiseHandler) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	return h.result
}

type tSetScopeHandler struct {
	stubHostHandler
	result int64
}

func (h *tSetScopeHandler) SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {
	return h.result
}

type tGetStateHandler struct {
	stubHostHandler
	result int64
}

func (h *tGetStateHandler) GetState(ctx context.Context, m api.Module, key string, valuePtr, valueMaxLen uint32) int64 {
	return h.result
}

type tDurableCallHeartbeatHandler struct {
	stubHostHandler
	result int64
}

func (h *tDurableCallHeartbeatHandler) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	return h.result
}

type tListStateHandler struct {
	stubHostHandler
	result int64
}

func (h *tListStateHandler) ListState(ctx context.Context, m api.Module, prefix string, keysPtr, keysMaxLen uint32) int64 {
	return h.result
}

type tWorkflowIDHandler struct {
	stubHostHandler
	result int64
}

func (h *tWorkflowIDHandler) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	return h.result
}

type tRunIDHandler struct {
	stubHostHandler
	result int64
}

func (h *tRunIDHandler) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	return h.result
}

// ==========================================================================
// CGO test helper functions
// ==========================================================================

func makeU64Arg(val uint64) C.wasmtime_component_val_t {
	var v C.wasmtime_component_val_t
	C.t_set_u64(&v, C.uint64_t(val))
	return v
}

func makeStringArg(s string) C.wasmtime_component_val_t {
	var v C.wasmtime_component_val_t
	cStr := C.CString(s)
	C.t_set_string(&v, cStr, C.size_t(len(s)))
	return v
}

func resultU64(results *C.wasmtime_component_val_t) uint64 {
	return uint64(C.t_get_u64(results))
}

func getResultString(results *C.wasmtime_component_val_t) string {
	var slen C.size_t
	sdata := C.t_get_string(results, &slen)
	return C.GoStringN(sdata, C.int(slen))
}

func newBackendForTest(h HostHandler) *wasmtimeBackend {
	return &wasmtimeBackend{handler: h}
}

// ==========================================================================
// CGO test logic functions (called from _test.go wrappers)
// ==========================================================================

func runTestArgPtr(t *testing.T) {
	var arr [3]C.wasmtime_component_val_t
	p0 := &arr[0]
	p1 := argPtr(p0, 1)
	p2 := argPtr(p0, 2)

	if p0 != &arr[0] {
		t.Error("argPtr(&arr[0], 0) != &arr[0]")
	}
	if p1 != &arr[1] {
		t.Error("argPtr(&arr[0], 1) != &arr[1]")
	}
	if p2 != &arr[2] {
		t.Error("argPtr(&arr[0], 2) != &arr[2]")
	}
}

func runTestReadStrArg(t *testing.T) {
	t.Run("valid string", func(t *testing.T) {
		var args [2]C.wasmtime_component_val_t
		cStr := C.CString("hello")
		defer C.free(unsafe.Pointer(cStr))
		C.t_set_string(&args[0], cStr, 5)
		s := readStrArg(&args[0], 0, 2)
		if s != "hello" {
			t.Errorf("readStrArg = %q, want %q", s, "hello")
		}
	})
	t.Run("empty string", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_string(&args[0], nil, 0)
		if s := readStrArg(&args[0], 0, 1); s != "" {
			t.Errorf("readStrArg = %q, want empty", s)
		}
	})
	t.Run("out of range", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		cStr := C.CString("hello")
		defer C.free(unsafe.Pointer(cStr))
		C.t_set_string(&args[0], cStr, 5)
		if s := readStrArg(&args[0], 1, 1); s != "" {
			t.Errorf("readStrArg OOB = %q, want empty", s)
		}
	})
	t.Run("wrong kind U64", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_u64(&args[0], 42)
		if s := readStrArg(&args[0], 0, 1); s != "" {
			t.Errorf("readStrArg on U64 = %q, want empty", s)
		}
	})
	t.Run("position > 0", func(t *testing.T) {
		var args [3]C.wasmtime_component_val_t
		cStr := C.CString("world")
		defer C.free(unsafe.Pointer(cStr))
		C.t_set_string(&args[1], cStr, 5)
		if s := readStrArg(&args[0], 1, 3); s != "world" {
			t.Errorf("readStrArg pos 1 = %q, want %q", s, "world")
		}
	})
}

func runTestReadU64Arg(t *testing.T) {
	t.Run("uint64 value", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_u64(&args[0], 42)
		if v := readU64Arg(&args[0], 0, 1); v != 42 {
			t.Errorf("readU64Arg = %d, want 42", v)
		}
	})
	t.Run("int64 via S64", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_s64(&args[0], -1)
		maxU64 := uint64(0xFFFFFFFFFFFFFFFF)
		if v := readU64Arg(&args[0], 0, 1); v != maxU64 {
			t.Errorf("readU64Arg(S64 -1) = %d, want %d", v, maxU64)
		}
	})
	t.Run("zero", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_u64(&args[0], 0)
		if v := readU64Arg(&args[0], 0, 1); v != 0 {
			t.Errorf("readU64Arg = %d, want 0", v)
		}
	})
	t.Run("max uint64", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		maxU64 := uint64(0xFFFFFFFFFFFFFFFF)
		C.t_set_u64(&args[0], C.uint64_t(maxU64))
		if v := readU64Arg(&args[0], 0, 1); v != maxU64 {
			t.Errorf("readU64Arg max = %d, want %d", v, maxU64)
		}
	})
	t.Run("out of range", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_u64(&args[0], 42)
		if v := readU64Arg(&args[0], 1, 1); v != 0 {
			t.Errorf("readU64Arg OOB = %d, want 0", v)
		}
	})
	t.Run("wrong kind STRING", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		cStr := C.CString("test")
		defer C.free(unsafe.Pointer(cStr))
		C.t_set_string(&args[0], cStr, 4)
		if v := readU64Arg(&args[0], 0, 1); v != 0 {
			t.Errorf("readU64Arg on STRING = %d, want 0", v)
		}
	})
}

func runTestReadU32Arg(t *testing.T) {
	t.Run("uint32 value", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_u32(&args[0], 12345)
		if v := readU32Arg(&args[0], 0, 1); v != 12345 {
			t.Errorf("readU32Arg = %d, want 12345", v)
		}
	})
	t.Run("int32 via S32", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_s32(&args[0], -1)
		maxU32 := uint32(0xFFFFFFFF)
		if v := readU32Arg(&args[0], 0, 1); v != maxU32 {
			t.Errorf("readU32Arg(S32 -1) = %d, want %d", v, maxU32)
		}
	})
	t.Run("zero", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_u32(&args[0], 0)
		if v := readU32Arg(&args[0], 0, 1); v != 0 {
			t.Errorf("readU32Arg = %d, want 0", v)
		}
	})
	t.Run("max uint32", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		maxU32 := uint32(0xFFFFFFFF)
		C.t_set_u32(&args[0], C.uint32_t(maxU32))
		if v := readU32Arg(&args[0], 0, 1); v != maxU32 {
			t.Errorf("readU32Arg max = %d, want %d", v, maxU32)
		}
	})
	t.Run("out of range", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_u32(&args[0], 42)
		if v := readU32Arg(&args[0], 1, 1); v != 0 {
			t.Errorf("readU32Arg OOB = %d, want 0", v)
		}
	})
	t.Run("wrong kind U64", func(t *testing.T) {
		var args [1]C.wasmtime_component_val_t
		C.t_set_u64(&args[0], 99)
		if v := readU32Arg(&args[0], 0, 1); v != 0 {
			t.Errorf("readU32Arg on U64 = %d, want 0", v)
		}
	})
}

func runTestSetResultU64(t *testing.T) {
	t.Run("write value", func(t *testing.T) {
		var results [1]C.wasmtime_component_val_t
		setResultU64(&results[0], 1, 42)
		if v := C.t_get_u64(&results[0]); v != 42 {
			t.Errorf("setResultU64 got %d, want 42", v)
		}
		if kind := C.t_get_kind(&results[0]); kind != C.WASMTIME_COMPONENT_U64 {
			t.Errorf("kind = %d, want WASMTIME_COMPONENT_U64", kind)
		}
	})
	t.Run("nresults=0 skips", func(t *testing.T) {
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 99)
		setResultU64(&results[0], 0, 42)
		if v := C.t_get_u64(&results[0]); v != 99 {
			t.Errorf("setResultU64 wrote %d, want unchanged 99", v)
		}
	})
}

func runTestSetResultString(t *testing.T) {
	t.Run("write string", func(t *testing.T) {
		var results [1]C.wasmtime_component_val_t
		setResultString(&results[0], 1, "hello")
		if kind := C.t_get_kind(&results[0]); kind != C.WASMTIME_COMPONENT_STRING {
			t.Errorf("kind = %d, want WASMTIME_COMPONENT_STRING", kind)
		}
		var slen C.size_t
		sdata := C.t_get_string(&results[0], &slen)
		if got := C.GoStringN(sdata, C.int(slen)); got != "hello" {
			t.Errorf("setResultString = %q, want %q", got, "hello")
		}
	})
	t.Run("nresults=0 skips", func(t *testing.T) {
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 99)
		setResultString(&results[0], 0, "hello")
		if kind := C.t_get_kind(&results[0]); kind == C.WASMTIME_COMPONENT_STRING {
			t.Error("setResultString with nresults=0 should not write")
		}
	})
	t.Run("empty string", func(t *testing.T) {
		var results [1]C.wasmtime_component_val_t
		setResultString(&results[0], 1, "")
		var slen C.size_t
		C.t_get_string(&results[0], &slen)
		if int(slen) != 0 {
			t.Errorf("empty string length = %d, want 0", slen)
		}
	})
}

// ==========================================================================
// Dispatch method test logic
// ==========================================================================

func runTestDispatchDurableSleep(t *testing.T) {
	t.Run("mock result", func(t *testing.T) {
		b := newBackendForTest(&tDurationHandler{sleepResult: 42})
		var args [1]C.wasmtime_component_val_t
		args[0] = makeU64Arg(100)
		var results [1]C.wasmtime_component_val_t
		b.dispatchDurableSleep(&args[0], 1, &results[0], 1)
		if got := resultU64(&results[0]); got != 42 {
			t.Errorf("dispatchDurableSleep = %d, want 42", got)
		}
	})
	t.Run("nil handler", func(t *testing.T) {
		b := &wasmtimeBackend{handler: nil}
		var args [1]C.wasmtime_component_val_t
		args[0] = makeU64Arg(100)
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 99)
		b.dispatchDurableSleep(&args[0], 1, &results[0], 1)
		if got := resultU64(&results[0]); got != 99 {
			t.Errorf("nil handler should not write, got %d", got)
		}
	})
	t.Run("insufficient args", func(t *testing.T) {
		b := newBackendForTest(&tDurationHandler{sleepResult: 99})
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 77)
		b.dispatchDurableSleep(nil, 0, &results[0], 1)
		if got := resultU64(&results[0]); got != 77 {
			t.Errorf("insufficient args should not write, got %d", got)
		}
	})
}

func runTestDispatchNow(t *testing.T) {
	b := newBackendForTest(&tNowHandler{nowResult: 1700000000000})
	var results [1]C.wasmtime_component_val_t
	b.dispatchNow(nil, 0, &results[0], 1)
	if got := resultU64(&results[0]); got != 1700000000000 {
		t.Errorf("dispatchNow = %d, want 1700000000000", got)
	}
	t.Run("nil handler", func(t *testing.T) {
		b := &wasmtimeBackend{handler: nil}
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 5)
		b.dispatchNow(nil, 0, &results[0], 1)
		if got := resultU64(&results[0]); got != 5 {
			t.Error("nil handler should not write")
		}
	})
}

func runTestDispatchRandom(t *testing.T) {
	b := newBackendForTest(&tRandomHandler{randomResult: 777})
	var results [1]C.wasmtime_component_val_t
	b.dispatchRandom(nil, 0, &results[0], 1)
	if got := resultU64(&results[0]); got != 777 {
		t.Errorf("dispatchRandom = %d, want 777", got)
	}
	t.Run("nil handler", func(t *testing.T) {
		b := &wasmtimeBackend{handler: nil}
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 5)
		b.dispatchRandom(nil, 0, &results[0], 1)
		if got := resultU64(&results[0]); got != 5 {
			t.Error("nil handler should not write")
		}
	})
}

func runTestDispatchDurableLog(t *testing.T) {
	t.Run("mock result", func(t *testing.T) {
		b := newBackendForTest(&tLogHandler{logResult: 1})
		var args [1]C.wasmtime_component_val_t
		args[0] = makeStringArg("test message")
		var results [1]C.wasmtime_component_val_t
		b.dispatchDurableLog(&args[0], 1, &results[0], 1)
		if got := resultU64(&results[0]); got != 1 {
			t.Errorf("dispatchDurableLog = %d, want 1", got)
		}
	})
	t.Run("nil handler", func(t *testing.T) {
		b := &wasmtimeBackend{handler: nil}
		var args [1]C.wasmtime_component_val_t
		args[0] = makeStringArg("msg")
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 5)
		b.dispatchDurableLog(&args[0], 1, &results[0], 1)
		if got := resultU64(&results[0]); got != 5 {
			t.Error("nil handler should not write")
		}
	})
	t.Run("insufficient args", func(t *testing.T) {
		b := newBackendForTest(&tLogHandler{logResult: 1})
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 5)
		b.dispatchDurableLog(nil, 0, &results[0], 1)
		if got := resultU64(&results[0]); got != 5 {
			t.Error("insufficient args should not write")
		}
	})
}

func runTestDispatchVersion(t *testing.T) {
	b := newBackendForTest(&tVersionHandler{versionResult: 42})
	var results [1]C.wasmtime_component_val_t
	b.dispatchVersion(nil, 0, &results[0], 1)
	if got := resultU64(&results[0]); got != 42 {
		t.Errorf("dispatchVersion = %d, want 42", got)
	}
	t.Run("nil handler", func(t *testing.T) {
		b := &wasmtimeBackend{handler: nil}
		var results [1]C.wasmtime_component_val_t
		C.t_set_u64(&results[0], 5)
		b.dispatchVersion(nil, 0, &results[0], 1)
		if got := resultU64(&results[0]); got != 5 {
			t.Error("nil handler should not write")
		}
	})
}

func runTestDispatchMinVersion(t *testing.T) {
	b := newBackendForTest(&tMinVersionHandler{result: 3})
	var results [1]C.wasmtime_component_val_t
	b.dispatchMinVersion(nil, 0, &results[0], 1)
	if got := resultU64(&results[0]); got != 3 {
		t.Errorf("dispatchMinVersion = %d, want 3", got)
	}
}

func runTestDispatchContinueAsNew(t *testing.T) {
	b := newBackendForTest(&tContinueAsNewHandler{result: 1})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("{}")
	var results [1]C.wasmtime_component_val_t
	b.dispatchContinueAsNew(&args[0], 1, &results[0], 1)
	if got := resultU64(&results[0]); got != 1 {
		t.Errorf("dispatchContinueAsNew = %d, want 1", got)
	}
}

func runTestDispatchReplyToSignal(t *testing.T) {
	b := newBackendForTest(&tReplyToSignalHandler{result: 0})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("corr-1")
	args[1] = makeStringArg("ok")
	var results [1]C.wasmtime_component_val_t
	b.dispatchReplyToSignal(&args[0], 2, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchReplyToSignal = %d, want 0", got)
	}
}

func runTestDispatchSignalWorkflow(t *testing.T) {
	b := newBackendForTest(&tSignalWorkflowHandler{result: 0})
	var args [3]C.wasmtime_component_val_t
	args[0] = makeStringArg("wf-1")
	args[1] = makeStringArg("sig")
	args[2] = makeStringArg("{}")
	var results [1]C.wasmtime_component_val_t
	b.dispatchSignalWorkflow(&args[0], 3, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchSignalWorkflow = %d, want 0", got)
	}
}

func runTestDispatchResolvePromise(t *testing.T) {
	b := newBackendForTest(&tResolvePromiseHandler{result: 1})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("prom-1")
	args[1] = makeStringArg("val")
	var results [1]C.wasmtime_component_val_t
	b.dispatchResolvePromise(&args[0], 2, &results[0], 1)
	if got := resultU64(&results[0]); got != 1 {
		t.Errorf("dispatchResolvePromise = %d, want 1", got)
	}
}

func runTestDispatchRejectPromise(t *testing.T) {
	b := newBackendForTest(&tRejectPromiseHandler{result: 1})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("prom-1")
	args[1] = makeStringArg("err")
	var results [1]C.wasmtime_component_val_t
	b.dispatchRejectPromise(&args[0], 2, &results[0], 1)
	if got := resultU64(&results[0]); got != 1 {
		t.Errorf("dispatchRejectPromise = %d, want 1", got)
	}
}

func runTestDispatchSetQueryState(t *testing.T) {
	b := newBackendForTest(&tSetQueryStateHandler{result: 0})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("key")
	args[1] = makeStringArg("val")
	var results [1]C.wasmtime_component_val_t
	b.dispatchSetQueryState(&args[0], 2, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchSetQueryState = %d, want 0", got)
	}
}

func runTestDispatchRegisterUpdateHandler(t *testing.T) {
	b := newBackendForTest(&tRegisterUpdateHandlerHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("handler1")
	var results [1]C.wasmtime_component_val_t
	b.dispatchRegisterUpdateHandler(&args[0], 1, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchRegisterUpdateHandler = %d, want 0", got)
	}
}

func runTestDispatchDurableSend(t *testing.T) {
	b := newBackendForTest(&tDurableSendHandler{result: 0})
	var args [3]C.wasmtime_component_val_t
	args[0] = makeStringArg("svc")
	args[1] = makeStringArg("op")
	args[2] = makeStringArg("{}")
	var results [1]C.wasmtime_component_val_t
	b.dispatchDurableSend(&args[0], 3, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchDurableSend = %d, want 0", got)
	}
}

func runTestDispatchScheduleInvoke(t *testing.T) {
	b := newBackendForTest(&tScheduleInvokeHandler{result: 0})
	var args [4]C.wasmtime_component_val_t
	args[0] = makeStringArg("svc")
	args[1] = makeStringArg("op")
	args[2] = makeStringArg("{}")
	args[3] = makeU64Arg(5000)
	var results [1]C.wasmtime_component_val_t
	b.dispatchScheduleInvoke(&args[0], 4, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchScheduleInvoke = %d, want 0", got)
	}
}

func runTestDispatchReleaseLock(t *testing.T) {
	b := newBackendForTest(&tReleaseLockHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("lock1")
	var results [1]C.wasmtime_component_val_t
	b.dispatchReleaseLock(&args[0], 1, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchReleaseLock = %d, want 0", got)
	}
}

func runTestDispatchAcquireLock(t *testing.T) {
	b := newBackendForTest(&tAcquireLockHandler{result: 1})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("lock1")
	args[1] = makeU64Arg(1000)
	var results [1]C.wasmtime_component_val_t
	b.dispatchAcquireLock(&args[0], 2, &results[0], 1)
	if got := resultU64(&results[0]); got != 1 {
		t.Errorf("dispatchAcquireLock = %d, want 1", got)
	}
}

func runTestDispatchSetState(t *testing.T) {
	b := newBackendForTest(&tSetStateHandler{result: 0})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("k")
	args[1] = makeStringArg("v")
	var results [1]C.wasmtime_component_val_t
	b.dispatchSetState(&args[0], 2, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchSetState = %d, want 0", got)
	}
}

func runTestDispatchDeleteState(t *testing.T) {
	b := newBackendForTest(&tDeleteStateHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("k")
	var results [1]C.wasmtime_component_val_t
	b.dispatchDeleteState(&args[0], 1, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchDeleteState = %d, want 0", got)
	}
}

func runTestDispatchIncrState(t *testing.T) {
	b := newBackendForTest(&tIncrStateHandler{result: 5})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("counter")
	args[1] = makeU64Arg(1)
	var results [1]C.wasmtime_component_val_t
	b.dispatchIncrState(&args[0], 2, &results[0], 1)
	if got := resultU64(&results[0]); got != 5 {
		t.Errorf("dispatchIncrState = %d, want 5", got)
	}
}

func runTestDispatchHasState(t *testing.T) {
	b := newBackendForTest(&tHasStateHandler{result: 1})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("k")
	var results [1]C.wasmtime_component_val_t
	b.dispatchHasState(&args[0], 1, &results[0], 1)
	if got := resultU64(&results[0]); got != 1 {
		t.Errorf("dispatchHasState = %d, want 1", got)
	}
}

func runTestDispatchSideEffect(t *testing.T) {
	b := newBackendForTest(&tSideEffectHandler{result: int64(len("result") << 40)})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("input")
	var results [1]C.wasmtime_component_val_t
	b.dispatchSideEffect(&args[0], 1, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchSideEffect kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchContinueAsNewVersioned(t *testing.T) {
	b := newBackendForTest(&tContinueAsNewVersionedHandler{result: 0})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("{}")
	C.t_set_u32(&args[1], C.uint32_t(2))
	var results [1]C.wasmtime_component_val_t
	b.dispatchContinueAsNewVersioned(&args[0], 2, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchContinueAsNewVersioned = %d, want 0", got)
	}
}

// ==========================================================================
// goComponentCallback tests
// ==========================================================================

func runTestGoComponentCallbackNilHandler(t *testing.T) {
	backend := &wasmtimeBackend{handler: nil}
	id := registerCB(backend, cbTypeDurableSleep)

	err := goComponentCallback(
		unsafe.Pointer(uintptr(id)),
		nil, nil, nil, 0, nil, 0,
	)

	if err != nil {
		t.Errorf("goComponentCallback nil handler returned error: %v", err)
	}
}

func runTestGoComponentCallbackDispatch(t *testing.T) {
	backend := &wasmtimeBackend{handler: &tNowHandler{nowResult: 999}}
	id := registerCB(backend, cbTypeNow)

	var results [1]C.wasmtime_component_val_t
	err := goComponentCallback(
		unsafe.Pointer(uintptr(id)),
		nil, nil, nil, 0, &results[0], 1,
	)

	if err != nil {
		t.Errorf("goComponentCallback dispatch error: %v", err)
	}
	if got := resultU64(&results[0]); got != 999 {
		t.Errorf("goComponentCallback dispatch = %d, want 999", got)
	}
}

func runTestGoComponentCallbackUnregistered(t *testing.T) {
	err := goComponentCallback(
		unsafe.Pointer(uintptr(99999)),
		nil, nil, nil, 0, nil, 0,
	)
	if err != nil {
		t.Errorf("unregistered callback returned error: %v", err)
	}
}

func runTestGCCallbackSleep(t *testing.T) {
	b := &wasmtimeBackend{handler: &tDurationHandler{sleepResult: 123}}
	id := registerCB(b, cbTypeDurableSleep)

	var args [1]C.wasmtime_component_val_t
	args[0] = makeU64Arg(500)
	var results [1]C.wasmtime_component_val_t

	err := goComponentCallback(
		unsafe.Pointer(uintptr(id)),
		nil, nil, &args[0], 1, &results[0], 1,
	)
	if err != nil {
		t.Errorf("goComponentCallback sleep dispatch error: %v", err)
	}
	if got := resultU64(&results[0]); got != 123 {
		t.Errorf("goComponentCallback sleep result = %d, want 123", got)
	}
}

func runTestGCCallbackDurableLog(t *testing.T) {
	b := &wasmtimeBackend{handler: &tLogHandler{logResult: 7}}
	id := registerCB(b, cbTypeDurableLog)

	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("hello log")
	var results [1]C.wasmtime_component_val_t

	err := goComponentCallback(
		unsafe.Pointer(uintptr(id)),
		nil, nil, &args[0], 1, &results[0], 1,
	)
	if err != nil {
		t.Errorf("goComponentCallback log dispatch error: %v", err)
	}
	if got := resultU64(&results[0]); got != 7 {
		t.Errorf("goComponentCallback log result = %d, want 7", got)
	}
}

func runTestGCCallbackRandom(t *testing.T) {
	b := &wasmtimeBackend{handler: &tRandomHandler{randomResult: 42}}
	id := registerCB(b, cbTypeRandom)

	var results [1]C.wasmtime_component_val_t
	err := goComponentCallback(
		unsafe.Pointer(uintptr(id)),
		nil, nil, nil, 0, &results[0], 1,
	)
	if err != nil {
		t.Errorf("goComponentCallback random dispatch error: %v", err)
	}
	if got := resultU64(&results[0]); got != 42 {
		t.Errorf("goComponentCallback random result = %d, want 42", got)
	}
}

// ==========================================================================
// Additional dispatch method tests for coverage
// ==========================================================================

func runTestDispatchRegisterQueryHandler(t *testing.T) {
	b := newBackendForTest(&tRegisterQueryHandlerHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("handler1")
	var results [1]C.wasmtime_component_val_t
	b.dispatchRegisterQueryHandler(&args[0], 1, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchRegisterQueryHandler = %d, want 0", got)
	}
}

func runTestDispatchComponentDefault(t *testing.T) {
	b := &wasmtimeBackend{handler: nil}
	var results [1]C.wasmtime_component_val_t
	b.dispatchComponentDefault(nil, 0, &results[0], 1)
	if got := resultU64(&results[0]); got != 0 {
		t.Errorf("dispatchComponentDefault = %d, want 0", got)
	}
}

func runTestDispatchDurableDefer(t *testing.T) {
	b := newBackendForTest(&tDurableDeferHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("desc")
	var results [1]C.wasmtime_component_val_t
	b.dispatchDurableDefer(&args[0], 1, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchDurableDefer kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchPollCancellation(t *testing.T) {
	b := newBackendForTest(&tPollCancellationHandler{result: 0})
	var results [1]C.wasmtime_component_val_t
	b.dispatchPollCancellation(nil, 0, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchPollCancellation kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchDurableCallString(t *testing.T) {
	b := newBackendForTest(&tDurableCallStringHandler{result: 0})
	var args [3]C.wasmtime_component_val_t
	args[0] = makeStringArg("svc")
	args[1] = makeStringArg("op")
	args[2] = makeStringArg("{}")
	var results [1]C.wasmtime_component_val_t
	b.dispatchDurableCallString(&args[0], 3, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchDurableCallString kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchChildWorkflow(t *testing.T) {
	b := newBackendForTest(&tChildWorkflowHandler{result: 0})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("name")
	args[1] = makeStringArg("{}")
	var results [1]C.wasmtime_component_val_t
	b.dispatchChildWorkflow(&args[0], 2, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchChildWorkflow kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchAwaitChild(t *testing.T) {
	b := newBackendForTest(&tAwaitChildHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("run-1")
	var results [1]C.wasmtime_component_val_t
	b.dispatchAwaitChild(&args[0], 1, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchAwaitChild kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchCreatePromise(t *testing.T) {
	b := newBackendForTest(&tCreatePromiseHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("promise1")
	var results [1]C.wasmtime_component_val_t
	b.dispatchCreatePromise(&args[0], 1, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchCreatePromise kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchAwaitPromise(t *testing.T) {
	b := newBackendForTest(&tAwaitPromiseHandler{result: 0})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("prom-1")
	args[1] = makeU64Arg(1000)
	var results [1]C.wasmtime_component_val_t
	b.dispatchAwaitPromise(&args[0], 2, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchAwaitPromise kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchSetScope(t *testing.T) {
	b := newBackendForTest(&tSetScopeHandler{result: 0})
	var args [2]C.wasmtime_component_val_t
	args[0] = makeStringArg("type")
	args[1] = makeStringArg("key")
	var results [1]C.wasmtime_component_val_t
	b.dispatchSetScope(&args[0], 2, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchSetScope kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchGetState(t *testing.T) {
	b := newBackendForTest(&tGetStateHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("key")
	var results [1]C.wasmtime_component_val_t
	b.dispatchGetState(&args[0], 1, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchGetState kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchDurableCallHeartbeat(t *testing.T) {
	b := newBackendForTest(&tDurableCallHeartbeatHandler{result: 0})
	var args [4]C.wasmtime_component_val_t
	args[0] = makeStringArg("svc")
	args[1] = makeStringArg("op")
	args[2] = makeStringArg("{}")
	args[3] = makeU64Arg(5000)
	var results [1]C.wasmtime_component_val_t
	b.dispatchDurableCallHeartbeat(&args[0], 4, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchDurableCallHeartbeat kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchListState(t *testing.T) {
	b := newBackendForTest(&tListStateHandler{result: 0})
	var args [1]C.wasmtime_component_val_t
	args[0] = makeStringArg("prefix")
	var results [1]C.wasmtime_component_val_t
	b.dispatchListState(&args[0], 1, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchListState kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchWorkflowID(t *testing.T) {
	b := newBackendForTest(&tWorkflowIDHandler{result: 0})
	var results [1]C.wasmtime_component_val_t
	b.dispatchWorkflowID(nil, 0, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchWorkflowID kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}

func runTestDispatchRunID(t *testing.T) {
	b := newBackendForTest(&tRunIDHandler{result: 0})
	var results [1]C.wasmtime_component_val_t
	b.dispatchRunID(nil, 0, &results[0], 1)
	kind := C.t_get_kind(&results[0])
	if kind != C.WASMTIME_COMPONENT_STRING {
		t.Errorf("dispatchRunID kind = %d, want WASMTIME_COMPONENT_STRING", kind)
	}
}
