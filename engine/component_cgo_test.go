//go:build cgo

// Tests for the native wasmtime Component Model C API path; see the comment at
// the top of component_cgo.go. These used to build only under the opt-in
// wasmtime_component_cgo tag, which meant they ran nowhere: no CI job set it.

package engine

import (
	"sync"
	"testing"
	"unsafe"
)

func TestExtractStringFromPacked(t *testing.T) {
	buf := []byte("hello world!! more data here")
	tests := []struct {
		name   string
		packed int64
		buf    []byte
		want   string
	}{
		{"valid length within buffer", int64(5) << 40, buf, "hello"},
		{"zero length", 0, buf, ""},
		{"length exceeds buffer - clamped", int64(100) << 40, buf, string(buf)},
		{"empty buffer", int64(5) << 40, []byte{}, ""},
		{"negative packed value", -1, buf, string(buf)},
		{"full buffer", int64(len(buf)) << 40, buf, string(buf)},
		{"nil buffer", int64(5) << 40, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractStringFromPacked(tt.packed, tt.buf)
			if got != tt.want {
				t.Errorf("extractStringFromPacked(%d, %q) = %q, want %q", tt.packed, tt.buf, got, tt.want)
			}
		})
	}
}

func TestRegisterCBLookupCB(t *testing.T) {
	cbRegistry.Lock()
	cbRegistry.entries = make(map[uintptr]cbEntry)
	cbRegistry.Unlock()
	b1 := &wasmtimeBackend{}
	b2 := &wasmtimeBackend{}

	t.Run("sequential IDs", func(t *testing.T) {
		id1 := registerCB(b1, cbTypeDurableSleep)
		id2 := registerCB(b2, cbTypeNow)
		if id1 != 1 || id2 != 2 {
			t.Errorf("registerCB ids = %d, %d; want 1, 2", id1, id2)
		}
	})
	t.Run("lookup returns correct entry", func(t *testing.T) {
		id := registerCB(b1, cbTypeDurableLog)
		entry := lookupCB(id)
		if entry.backend != b1 || entry.typ != cbTypeDurableLog {
			t.Errorf("lookupCB = {%v, %v}, want {%v, %v}", entry.backend, entry.typ, b1, cbTypeDurableLog)
		}
	})
	t.Run("lookup unregistered returns zero", func(t *testing.T) {
		entry := lookupCB(99999)
		if entry.backend != nil || entry.typ != cbTypeDefault {
			t.Errorf("unregistered lookup = {%v, %v}, want nil, cbTypeDefault", entry.backend, entry.typ)
		}
	})
}

func TestRegisterCBConcurrent(t *testing.T) {
	cbRegistry.Lock()
	cbRegistry.entries = make(map[uintptr]cbEntry)
	cbRegistry.Unlock()
	b := &wasmtimeBackend{}
	var wg sync.WaitGroup
	n := 100
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); registerCB(b, cbTypeDurableSleep) }()
	}
	wg.Wait()
	cbRegistry.Lock()
	count := len(cbRegistry.entries)
	cbRegistry.Unlock()
	if count != n {
		t.Errorf("after %d concurrent registrations, got %d entries", n, count)
	}
}

func TestCbTypeConstants(t *testing.T) {
	if cbTypeDefault != 0 {
		t.Errorf("cbTypeDefault = %d, want 0", cbTypeDefault)
	}
	seen := make(map[cbType]bool)
	allTypes := []cbType{
		cbTypeDurableCallString, cbTypeDurableCallRetry, cbTypeDurableCallHeartbeat,
		cbTypeDurableSleep, cbTypeNow, cbTypeRandom, cbTypeDurableLog,
		cbTypeVersion, cbTypeMinVersion, cbTypeDurableDefer, cbTypeContinueAsNew, cbTypePollCancellation,
		cbTypeAwaitSignals, cbTypePollSignal, cbTypeSendSignalAndWait,
		cbTypeReplyToSignal, cbTypeSignalWorkflow,
		cbTypeChildWorkflow, cbTypeAwaitChild, cbTypeAwaitAllChildren,
		cbTypeChildWorkflowWithOptions,
		cbTypeCreatePromise, cbTypeAwaitPromise, cbTypeResolvePromise, cbTypeRejectPromise,
		cbTypeSetQueryState, cbTypeRegisterUpdateHandler, cbTypeRegisterQueryHandler,
		cbTypeDurableSend, cbTypeScheduleInvoke, cbTypeWorkflowID, cbTypeRunID,
		cbTypePluginCall, cbTypePluginCallStreaming, cbTypeAcquireLock, cbTypeReleaseLock,
		cbTypeSetScope, cbTypeGetScope, cbTypeUUID,
		cbTypeSetState, cbTypeGetState, cbTypeDeleteState,
		cbTypeIncrState, cbTypeHasState, cbTypeListState,
		cbTypeContinueAsNewVersioned, cbTypeSideEffect, cbTypeFetch,
	}
	for _, typ := range allTypes {
		if seen[typ] {
			t.Errorf("duplicate cbType value: %d", typ)
		}
		seen[typ] = true
	}
}

func TestWitTypeMapNoDefaults(t *testing.T) {
	for module, funcs := range witTypeMap {
		for fn, typ := range funcs {
			if typ == cbTypeDefault {
				t.Errorf("witTypeMap[%s][%s] = cbTypeDefault (deprecated)", module, fn)
			}
		}
	}
}

func packStrLen(n int64) int64 { return n << 40 }

// packSimpleStrLen is packStrLen for the OTHER layout. packSimpleResult puts
// the written length at bits 32-63, packDurableCallResult at bits 40-63, and
// the dispatchers are split between them -- child-workflow and fetch decode
// the simple one. The tests below used to assert only "the result is non-nil",
// which both layouts satisfy from either packer, so the mismatch was invisible.
func packSimpleStrLen(n int64) int64 { return n << 32 }

// wantCallOutcome asserts the shape of a result<string, call-failure>.
//
// The old assertion was `cgotestHasResultString(resultPtr)`. It could not fail
// for any reason worth catching: a dispatcher that dropped an error and
// returned it as a success passes it, a dispatcher that returned the wrong
// length passes it, and after IMPROVEMENT-PLAN 3.110 a dispatcher that had not
// been converted at all would pass it too -- a bare string IS a non-nil
// string. It is the assertion this whole item is about, in miniature.
func wantCallOutcome(t *testing.T, resultPtr unsafe.Pointer, wantKind string, wantLen int, wantCode uint32) {
	t.Helper()
	kind, payload, code := cgotestReadCallOutcome(resultPtr)
	if kind != wantKind {
		t.Fatalf("result is %q, want %q.\n\n"+
			"An empty kind means the dispatcher wrote something that is not a "+
			"result<string, call-failure> at all -- most likely still "+
			"setResultString. The guest lifts by the WIT type, so that is a "+
			"value of the wrong shape at the ABI boundary.", kindOrNotAResult(kind), wantKind)
	}
	if len(payload) != wantLen {
		t.Errorf("payload is %d bytes, want %d -- the wrong length decoder for this layout?",
			len(payload), wantLen)
	}
	if code != wantCode {
		t.Errorf("call-error code = %d, want %d", code, wantCode)
	}
}

func kindOrNotAResult(kind string) string {
	if kind == "" {
		return "not a result"
	}
	return kind
}

// =============================================================================
// Comprehensive dispatch guard tests — nil handler
// =============================================================================

func TestDispatchGuardsNilHandler(t *testing.T) {
	b := &wasmtimeBackend{}
	strPtr, _, freeStr := cgotestMakeStrArgs("a")
	defer freeStr()
	u64Ptr, _, freeU64 := cgotestMakeU64Args(1)
	defer freeU64()
	resultPtr := cgotestAllocResult()

	tests := []struct {
		name   string
		isStr  bool
		method int
		ptr    unsafe.Pointer
		nargs  int
	}{
		{"DurableCallString", true, 0, strPtr, 3},
		{"DurableCallRetry", true, 1, strPtr, 8},
		{"DurableCallHeartbeat", true, 2, strPtr, 4},
		{"DurableDefer", true, 3, strPtr, 1},
		{"ChildWorkflow", true, 4, strPtr, 2},
		{"CreatePromise", true, 5, strPtr, 1},
		{"PluginCall", true, 6, strPtr, 3},
		{"SetScope", true, 7, strPtr, 2},
		{"GetState", true, 8, strPtr, 1},
		{"ListState", true, 9, strPtr, 1},
		{"PollCancellation", true, 10, nil, 0},
		{"WorkflowID", true, 11, nil, 0},
		{"RunID", true, 12, nil, 0},
		{"UUID", true, 13, strPtr, 1},
		{"SideEffect", true, 14, strPtr, 1},
		{"Fetch", true, 15, strPtr, 4},
		{"AwaitChild", true, 17, strPtr, 1},
		{"AwaitAllChildren", true, 18, strPtr, 1},
		{"PluginCallStreaming", true, 19, strPtr, 3},
		{"ChildWorkflowWithOptions", true, 20, strPtr, 5},
		{"SendSignalAndWait", true, 21, strPtr, 4},
		{"AwaitPromise", true, 22, strPtr, 2},
		{"PollSignal", true, 23, strPtr, 1},
		{"DurableSleep", false, 0, u64Ptr, 1},
		{"Now", false, 1, nil, 0},
		{"Random", false, 2, nil, 0},
		{"Version", false, 3, nil, 0},
		{"MinVersion", false, 4, nil, 0},
		{"DurableLog", false, 5, strPtr, 1},
		{"ContinueAsNew", false, 6, strPtr, 1},
		{"ResolvePromise", false, 7, strPtr, 2},
		{"RejectPromise", false, 8, strPtr, 2},
		{"SetQueryState", false, 9, strPtr, 2},
		{"DurableSend", false, 10, strPtr, 3},
		{"SetState", false, 11, strPtr, 2},
		{"IncrState", false, 12, strPtr, 2},
		{"HasState", false, 13, strPtr, 1},
		{"DeleteState", false, 14, strPtr, 1},
		{"AcquireLock", false, 15, strPtr, 2},
		{"ReleaseLock", false, 16, strPtr, 1},
		{"SignalWorkflow", false, 17, strPtr, 3},
		{"ReplyToSignal", false, 18, strPtr, 2},
		{"ScheduleInvoke", false, 19, strPtr, 4},
		{"RegisterUpdateHandler", false, 20, strPtr, 1},
		{"RegisterQueryHandler", false, 21, strPtr, 1},
		{"ContinueAsNewVersioned", false, 22, strPtr, 2},
		{"GetScope", false, 23, u64Ptr, 4},
		{"AwaitSignals", false, 24, strPtr, 6},
		{"ComponentDefault", false, 25, nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.isStr {
				err = b.cgotestDispatchStr(tt.method, tt.ptr, tt.nargs, resultPtr)
			} else {
				err = b.cgotestDispatchU64(tt.method, tt.ptr, tt.nargs, resultPtr)
			}
			if err != nil {
				t.Errorf("got error, want nil (nil handler should be safe)")
			}
		})
	}
}

// =============================================================================
// Comprehensive guard tests — insufficient args
// =============================================================================

func TestDispatchGuardsInsufficientArgs(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 0}}
	resultPtr := cgotestAllocResult()

	tests := []struct {
		name   string
		isStr  bool
		method int
	}{
		{"DurableCallString(0<3)", true, 0},
		{"DurableCallRetry(0<8)", true, 1},
		{"DurableCallHeartbeat(0<4)", true, 2},
		{"DurableDefer(0<1)", true, 3},
		{"ChildWorkflow(0<2)", true, 4},
		{"CreatePromise(0<1)", true, 5},
		{"PluginCall(0<3)", true, 6},
		{"SetScope(0<2)", true, 7},
		{"GetState(0<1)", true, 8},
		{"ListState(0<1)", true, 9},
		{"AwaitChild(0<1)", true, 17},
		{"AwaitAllChildren(0<1)", true, 18},
		{"PluginCallStreaming(0<3)", true, 19},
		{"ChildWorkflowWithOptions(0<5)", true, 20},
		{"SendSignalAndWait(0<4)", true, 21},
		{"AwaitPromise(0<2)", true, 22},
		{"PollSignal(0<1)", true, 23},
		{"UUID(0<1)", true, 13},
		{"SideEffect(0<1)", true, 14},
		{"Fetch(0<4)", true, 15},
		{"DurableSleep(0<1)", false, 0},
		{"DurableLog(0<1)", false, 5},
		{"ContinueAsNew(0<1)", false, 6},
		{"ResolvePromise(0<2)", false, 7},
		{"RejectPromise(0<2)", false, 8},
		{"SetQueryState(0<2)", false, 9},
		{"DurableSend(0<3)", false, 10},
		{"SetState(0<2)", false, 11},
		{"IncrState(0<2)", false, 12},
		{"HasState(0<1)", false, 13},
		{"DeleteState(0<1)", false, 14},
		{"AcquireLock(0<2)", false, 15},
		{"ReleaseLock(0<1)", false, 16},
		{"SignalWorkflow(0<3)", false, 17},
		{"ReplyToSignal(0<2)", false, 18},
		{"ScheduleInvoke(0<4)", false, 19},
		{"RegisterUpdateHandler(0<1)", false, 20},
		{"RegisterQueryHandler(0<1)", false, 21},
		{"ContinueAsNewVersioned(0<2)", false, 22},
		{"GetScope(0<4)", false, 23},
		{"AwaitSignals(0<6)", false, 24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.isStr {
				err = b.cgotestDispatchStr(tt.method, nil, 0, resultPtr)
			} else {
				err = b.cgotestDispatchU64(tt.method, nil, 0, resultPtr)
			}
			if err != nil {
				t.Errorf("got error, want nil (insufficient args should be safe)")
			}
		})
	}
}

// =============================================================================
// U64 dispatch tests
// =============================================================================

func TestDispatchDurableSleep(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 42}}
	argsPtr, _, freeArgs := cgotestMakeU64Args(1000)
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(0, argsPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 42 {
		t.Errorf("result = %d, want 42", got)
	}
}

func TestDispatchNow(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 99}}
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(1, nil, 0, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 99 {
		t.Errorf("result = %d, want 99", got)
	}
}

func TestDispatchRandom(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 7}}
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(2, nil, 0, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 7 {
		t.Errorf("result = %d, want 7", got)
	}
}

func TestDispatchVersion(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 3}}
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(3, nil, 0, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 3 {
		t.Errorf("result = %d, want 3", got)
	}
}

func TestDispatchMinVersion(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 2}}
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(4, nil, 0, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 2 {
		t.Errorf("result = %d, want 2", got)
	}
}

func TestDispatchDurableLog(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("log msg")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(5, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchContinueAsNew(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("new input")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(6, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchResolvePromise(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("promise-id", "value")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(7, strPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchRejectPromise(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 2}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("promise-id", "error msg")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(8, strPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 2 {
		t.Errorf("result = %d, want 2", got)
	}
}

func TestDispatchSetQueryState(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("key", "value")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(9, strPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchDurableSend(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("svc", "op", "req")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(10, strPtr, 3, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchSetState(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("key", "value")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(11, strPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchIncrState(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 5}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("counter-key", uint64(10))
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(12, argsPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 5 {
		t.Errorf("result = %d, want 5", got)
	}
}

func TestDispatchHasState(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("key")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(13, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchDeleteState(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("key")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(14, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchAcquireLock(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("lock-key", uint64(5000))
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(15, argsPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchReleaseLock(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("lock-key")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(16, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchSignalWorkflow(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("target", "signal", "payload")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(17, strPtr, 3, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchReplyToSignal(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("corr-id", "response")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(18, strPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchScheduleInvoke(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("svc", "op", "req", uint64(5000))
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(19, argsPtr, 4, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchRegisterUpdateHandler(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("handler-name")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(20, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchRegisterQueryHandler(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("handler-name")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(21, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchContinueAsNewVersioned(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("new-input", uint32(2))
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(22, argsPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchGetScope(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 1}}
	u64Ptr, _, freeU64 := cgotestMakeU64Args(0, 0, 0, 0)
	defer freeU64()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(23, u64Ptr, 4, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 1 {
		t.Errorf("result = %d, want 1", got)
	}
}

func TestDispatchAwaitSignals(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 42}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("sig1,sig2", uint64(0), uint64(0), uint64(0), uint64(0), uint64(0))
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(24, argsPtr, 6, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 42 {
		t.Errorf("result = %d, want 42", got)
	}
}

// =============================================================================
// String dispatch tests
// =============================================================================

func TestDispatchDurableCallString(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(5)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("svc", "op", "req")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(0, strPtr, 3, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "ok", 5, 0)
}

func TestDispatchDurableCallRetry(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(5)}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("svc", "op", "req", uint64(0), uint64(0), uint64(0), uint64(0), "none")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(1, argsPtr, 8, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "ok", 5, 0)
}

func TestDispatchDurableCallHeartbeat(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(5)}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("svc", "op", "req", uint64(5000))
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(2, argsPtr, 4, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "ok", 5, 0)
}

// TestDispatchDurableCallStopIsNotAResponse is the case the eight tests above
// could not express before IMPROVEMENT-PLAN 3.110, because there was nothing to
// express it with.
//
// The host refuses a body call past the frontier of a defer segment by
// returning callSuspendSentinel (engine/durablecalls.go). The old dispatcher
// ran that word through extractStringFromPacked, which read responseLen=0 out
// of it and handed the guest "" -- an ordinary EMPTY SUCCESSFUL RESPONSE. The
// Python SDK had no way to tell that from a service that returned nothing, so
// a stopped workflow carried on and reported itself complete (3.83's defect,
// measured on a real component in 3.110).
//
// Asserting "suspended" rather than "not ok" is deliberate: `err(failed{...})`
// would also be not-ok, and it is the wrong answer -- a stop is not a failure,
// and an SDK that raised its error class for one would report a terminated
// workflow as FAILED and never drain its defer table.
func TestDispatchDurableCallStopIsNotAResponse(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: callSuspendSentinel}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("svc", "op", "req")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(0, strPtr, 3, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "suspended", 0, 0)
}

// TestDispatchDurableCallFailureIsNotAResponse is the other half, and the one
// that had been broken for longer.
//
// cleat.wit documented a failure as a response string prefixed with
// "__CLEAT_ERROR__:" and the Python SDK checked for that prefix in six places.
// Nothing in the host has ever written it -- so a failed call arrived as an
// ordinary successful response holding the error text, and its callErrorCode,
// which decides retryable from permanent, was discarded on the way.
//
// The code is asserted, not just the kind. Dropping it is exactly what the old
// dispatcher did, and a test that only checked "this is an err" would pass a
// dispatcher that reported every failure as unclassified.
func TestDispatchDurableCallFailureIsNotAResponse(t *testing.T) {
	// responseLen=7 ("timeout" would be in the buffer), callErrorCode=1
	// (CallErrorTimeout), errCode=1.
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packDurableCallResult(7, 1, 1)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("svc", "op", "req")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(0, strPtr, 3, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "failed", 7, 1)
}

func TestDispatchDurableDefer(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(10)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("desc")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(3, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchChildWorkflow(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packSimpleStrLen(20)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("wf-name", "input")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(4, strPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "ok", 20, 0)
}

func TestDispatchCreatePromise(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(8)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("promise")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(5, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchPluginCall(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(15)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("plugin", "func", "input")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(6, strPtr, 3, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "ok", 15, 0)
}

func TestDispatchSetScope(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(12)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("type", "key")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(7, strPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchGetState(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(10)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("key")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(8, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchListState(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(10)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("prefix")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(9, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchPollCancellation(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(5)}}
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(10, nil, 0, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchWorkflowID(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(20)}}
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(11, nil, 0, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchRunID(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(20)}}
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(12, nil, 0, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchUUID(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(36)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("seed")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(13, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchSideEffect(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(8)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("result")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(14, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchFetch(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packSimpleStrLen(10)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("GET", "http://x", "{}", "")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(15, strPtr, 4, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "ok", 10, 0)
}

func TestDispatchAwaitChild(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(8)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("run-id")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(17, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchAwaitAllChildren(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(10)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs(`["id1","id2"]`)
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(18, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchPluginCallStreaming(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(15)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("plugin", "func", "input")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(19, strPtr, 3, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "ok", 15, 0)
}

func TestDispatchChildWorkflowWithOptions(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packSimpleStrLen(20)}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("wf-name", "input", uint64(0), uint64(0), "policy")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(20, argsPtr, 5, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantCallOutcome(t, resultPtr, "ok", 20, 0)
}

func TestDispatchSendSignalAndWait(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(10)}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("target", "signal", "payload", uint64(5000))
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(21, argsPtr, 4, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchAwaitPromise(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(8)}}
	argsPtr, _, freeArgs := cgotestMakeMixedArgs("promise-id", uint64(1000))
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(22, argsPtr, 2, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

func TestDispatchPollSignal(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: packStrLen(6)}}
	strPtr, _, freeArgs := cgotestMakeStrArgs("signal")
	defer freeArgs()
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchStr(23, strPtr, 1, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cgotestHasResultString(resultPtr) {
		t.Error("expected non-nil string result")
	}
}

// =============================================================================
// dispatchComponentDefault
// =============================================================================

func TestDispatchComponentDefault(t *testing.T) {
	b := &wasmtimeBackend{}
	resultPtr := cgotestAllocResult()
	if err := b.cgotestDispatchU64(25, nil, 0, resultPtr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cgotestReadResultU64(resultPtr); got != 0 {
		t.Errorf("result = %d, want 0", got)
	}
}

// TestCgotestMakeU32Arg was moved here from backend_wasmtime_test.go: it
// exercises cgotestMakeU32Arg from cgo_test_helpers.go, which is only
// defined under the wasmtime_component_cgo build tag.
func TestCgotestMakeU32Arg(t *testing.T) {
	p := cgotestMakeU32Arg(42)
	if p == nil {
		t.Fatal("expected non-nil pointer")
	}
}

// =============================================================================
// goComponentCallback
// =============================================================================

func TestGoComponentCallbackNilHandler(t *testing.T) {
	b := &wasmtimeBackend{}
	id := registerCB(b, cbTypeNow)
	err := cgotestGoComponentCallback(id, nil, 0, nil)
	if err != nil {
		t.Errorf("goComponentCallback with nil handler returned error")
	}
}

func TestGoComponentCallbackNilBackend(t *testing.T) {
	entry := cbEntry{backend: nil, typ: cbTypeDurableSleep}
	id := registerCB(entry.backend, entry.typ)
	err := cgotestGoComponentCallback(id, nil, 0, nil)
	if err != nil {
		t.Errorf("goComponentCallback with nil backend returned error")
	}
}

func TestGoComponentCallbackDispatchDefault(t *testing.T) {
	b := &wasmtimeBackend{handler: &mockHostHandler{ret: 0}}
	id := registerCB(b, cbTypeDefault)
	resultPtr := cgotestAllocResult()
	err := cgotestGoComponentCallback(id, nil, 0, resultPtr)
	if err != nil {
		t.Errorf("goComponentCallback with default type returned error")
	}
	if got := cgotestReadResultU64(resultPtr); got != 0 {
		t.Errorf("result = %d, want 0", got)
	}
}

func TestGoComponentCallbackMissingFromRegistry(t *testing.T) {
	err := cgotestGoComponentCallback(uintptr(999999), nil, 0, nil)
	if err != nil {
		t.Errorf("goComponentCallback with missing ID returned error")
	}
}

// =============================================================================
// Benchmarks
// =============================================================================

func BenchmarkExtractStringFromPacked(b *testing.B) {
	buf := make([]byte, 65536)
	for i := range buf {
		buf[i] = 'x'
	}
	packed := int64(100) << 40
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		extractStringFromPacked(packed, buf)
	}
}

func BenchmarkRegisterCB(b *testing.B) {
	cbRegistry.Lock()
	cbRegistry.entries = make(map[uintptr]cbEntry)
	cbRegistry.Unlock()
	backend := &wasmtimeBackend{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		registerCB(backend, cbTypeDurableSleep)
	}
}

func BenchmarkLookupCB(b *testing.B) {
	cbRegistry.Lock()
	cbRegistry.entries = make(map[uintptr]cbEntry)
	cbRegistry.Unlock()
	backend := &wasmtimeBackend{}
	id := registerCB(backend, cbTypeDurableSleep)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lookupCB(id)
	}
}
