//go:build cgo

package engine

import (
	"sync"
	"testing"
)

// ==========================================================================
// Group 1: Pure Go helpers
// ==========================================================================

func TestExtractStringFromPacked(t *testing.T) {
	buf := []byte("hello world!!...")
	tests := []struct {
		name     string
		packed   int64
		buf      []byte
		expected string
	}{
		{"valid length within buffer", 5 << 40, buf, "hello"},
		{"zero length", 0, buf, ""},
		{"length exceeds buffer (clamped)", int64(uint64(100) << 40), buf, string(buf)},
		{"empty buffer", 5 << 40, []byte{}, ""},
		{"negative packed (all bits set)", -1, buf, string(buf)},
		{"full buffer exact length", int64(uint64(len(buf)) << 40), buf, string(buf)},
		{"nil buffer", 5 << 40, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractStringFromPacked(tt.packed, tt.buf)
			if got != tt.expected {
				t.Errorf("extractStringFromPacked(%d, %v) = %q, want %q", tt.packed, tt.buf, got, tt.expected)
			}
		})
	}
}

func TestArgPtr(t *testing.T) { runTestArgPtr(t) }

// ==========================================================================
// Group 2: Callback registry
// ==========================================================================

func TestRegisterCBLookupCB(t *testing.T) {
	cbRegistry.Lock()
	baseCount := len(cbRegistry.entries)
	cbRegistry.Unlock()

	backend := &wasmtimeBackend{}
	id1 := registerCB(backend, cbTypeDurableSleep)
	id2 := registerCB(backend, cbTypeNow)

	if id1 != uintptr(baseCount+1) {
		t.Errorf("first registerCB ID = %d, want %d", id1, baseCount+1)
	}
	if id2 != uintptr(baseCount+2) {
		t.Errorf("second registerCB ID = %d, want %d", id2, baseCount+2)
	}

	entry1 := lookupCB(id1)
	if entry1.backend != backend || entry1.typ != cbTypeDurableSleep {
		t.Errorf("lookupCB(%d) = {backend=%v, typ=%v}", id1, entry1.backend, entry1.typ)
	}

	entry := lookupCB(99999)
	if entry.backend != nil || entry.typ != cbTypeDefault {
		t.Errorf("lookupCB(99999) = {backend=%v, typ=%v}, want {nil, cbTypeDefault}", entry.backend, entry.typ)
	}
}

func TestRegisterCBConcurrent(t *testing.T) {
	cbRegistry.Lock()
	baseCount := len(cbRegistry.entries)
	cbRegistry.Unlock()

	const n = 100
	backend := &wasmtimeBackend{}
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			registerCB(backend, cbTypeDurableSleep)
		}()
	}
	wg.Wait()

	cbRegistry.Lock()
	finalCount := len(cbRegistry.entries)
	cbRegistry.Unlock()

	if finalCount != baseCount+n {
		t.Errorf("after %d concurrent registerCB calls, count = %d, want %d", n, finalCount, baseCount+n)
	}
}

// ==========================================================================
// Group 3: Type system
// ==========================================================================

func TestCbTypeConstants(t *testing.T) {
	if cbTypeDefault != 0 {
		t.Errorf("cbTypeDefault = %d, want 0", cbTypeDefault)
	}

	seen := make(map[cbType]string)
	types := []struct {
		name string
		val  cbType
	}{
		{"cbTypeDefault", cbTypeDefault},
		{"cbTypeDurableCallString", cbTypeDurableCallString},
		{"cbTypeDurableCallRetry", cbTypeDurableCallRetry},
		{"cbTypeDurableCallHeartbeat", cbTypeDurableCallHeartbeat},
		{"cbTypeDurableSleep", cbTypeDurableSleep},
		{"cbTypeNow", cbTypeNow},
		{"cbTypeRandom", cbTypeRandom},
		{"cbTypeDurableLog", cbTypeDurableLog},
		{"cbTypeVersion", cbTypeVersion},
		{"cbTypeMinVersion", cbTypeMinVersion},
		{"cbTypeDurableDefer", cbTypeDurableDefer},
		{"cbTypeContinueAsNew", cbTypeContinueAsNew},
		{"cbTypePollCancellation", cbTypePollCancellation},
		{"cbTypeAwaitSignals", cbTypeAwaitSignals},
		{"cbTypePollSignal", cbTypePollSignal},
		{"cbTypeSendSignalAndWait", cbTypeSendSignalAndWait},
		{"cbTypeReplyToSignal", cbTypeReplyToSignal},
		{"cbTypeSignalWorkflow", cbTypeSignalWorkflow},
		{"cbTypeChildWorkflow", cbTypeChildWorkflow},
		{"cbTypeAwaitChild", cbTypeAwaitChild},
		{"cbTypeAwaitAllChildren", cbTypeAwaitAllChildren},
		{"cbTypeChildWorkflowWithOptions", cbTypeChildWorkflowWithOptions},
		{"cbTypeCreatePromise", cbTypeCreatePromise},
		{"cbTypeAwaitPromise", cbTypeAwaitPromise},
		{"cbTypeResolvePromise", cbTypeResolvePromise},
		{"cbTypeRejectPromise", cbTypeRejectPromise},
		{"cbTypeSetQueryState", cbTypeSetQueryState},
		{"cbTypeRegisterUpdateHandler", cbTypeRegisterUpdateHandler},
		{"cbTypeRegisterQueryHandler", cbTypeRegisterQueryHandler},
		{"cbTypeDurableSend", cbTypeDurableSend},
		{"cbTypeScheduleInvoke", cbTypeScheduleInvoke},
		{"cbTypeWorkflowID", cbTypeWorkflowID},
		{"cbTypeRunID", cbTypeRunID},
		{"cbTypePluginCall", cbTypePluginCall},
		{"cbTypePluginCallStreaming", cbTypePluginCallStreaming},
		{"cbTypeAcquireLock", cbTypeAcquireLock},
		{"cbTypeReleaseLock", cbTypeReleaseLock},
		{"cbTypeSetScope", cbTypeSetScope},
		{"cbTypeGetScope", cbTypeGetScope},
		{"cbTypeUUID", cbTypeUUID},
		{"cbTypeSetState", cbTypeSetState},
		{"cbTypeGetState", cbTypeGetState},
		{"cbTypeDeleteState", cbTypeDeleteState},
		{"cbTypeIncrState", cbTypeIncrState},
		{"cbTypeHasState", cbTypeHasState},
		{"cbTypeListState", cbTypeListState},
		{"cbTypeContinueAsNewVersioned", cbTypeContinueAsNewVersioned},
		{"cbTypeSideEffect", cbTypeSideEffect},
		{"cbTypeChildWorkflowInSchema", cbTypeChildWorkflowInSchema},
		{"cbTypeFetch", cbTypeFetch},
	}

	for _, tt := range types {
		if prev, ok := seen[tt.val]; ok {
			t.Errorf("duplicate cbType value %d: %s and %s", tt.val, prev, tt.name)
		}
		seen[tt.val] = tt.name
	}

	if len(seen) != len(types) {
		t.Errorf("expected %d unique cbType values, got %d", len(types), len(seen))
	}
}

func TestWitTypeMapNoDefaults(t *testing.T) {
	for mod, funcs := range witTypeMap {
		for fn, typ := range funcs {
			if typ == cbTypeDefault {
				t.Errorf("witTypeMap[%q][%q] = cbTypeDefault", mod, fn)
			}
		}
	}
}

func TestWitTypeMapCoverage(t *testing.T) {
	types := make(map[cbType]bool)
	for _, funcs := range witTypeMap {
		for _, typ := range funcs {
			types[typ] = true
		}
	}
	if len(types) < 40 {
		t.Errorf("witTypeMap covers %d cbTypes, want >= 40", len(types))
	}
}

// ==========================================================================
// Group 4: CGO struct read helpers
// ==========================================================================

func TestReadStrArg(t *testing.T) { runTestReadStrArg(t) }
func TestReadU64Arg(t *testing.T) { runTestReadU64Arg(t) }
func TestReadU32Arg(t *testing.T) { runTestReadU32Arg(t) }

// ==========================================================================
// Group 5: CGO struct write helpers
// ==========================================================================

func TestSetResultU64(t *testing.T)    { runTestSetResultU64(t) }
func TestSetResultString(t *testing.T) { runTestSetResultString(t) }

// ==========================================================================
// Group 6: Dispatch method tests
// ==========================================================================

func TestDispatchDurableSleep(t *testing.T)           { runTestDispatchDurableSleep(t) }
func TestDispatchNow(t *testing.T)                    { runTestDispatchNow(t) }
func TestDispatchRandom(t *testing.T)                 { runTestDispatchRandom(t) }
func TestDispatchDurableLog(t *testing.T)             { runTestDispatchDurableLog(t) }
func TestDispatchVersion(t *testing.T)                { runTestDispatchVersion(t) }
func TestDispatchMinVersion(t *testing.T)             { runTestDispatchMinVersion(t) }
func TestDispatchContinueAsNew(t *testing.T)          { runTestDispatchContinueAsNew(t) }
func TestDispatchReplyToSignal(t *testing.T)          { runTestDispatchReplyToSignal(t) }
func TestDispatchSignalWorkflow(t *testing.T)         { runTestDispatchSignalWorkflow(t) }
func TestDispatchResolvePromise(t *testing.T)         { runTestDispatchResolvePromise(t) }
func TestDispatchRejectPromise(t *testing.T)          { runTestDispatchRejectPromise(t) }
func TestDispatchSetQueryState(t *testing.T)          { runTestDispatchSetQueryState(t) }
func TestDispatchRegisterUpdateHandler(t *testing.T)  { runTestDispatchRegisterUpdateHandler(t) }
func TestDispatchDurableSend(t *testing.T)            { runTestDispatchDurableSend(t) }
func TestDispatchScheduleInvoke(t *testing.T)         { runTestDispatchScheduleInvoke(t) }
func TestDispatchReleaseLock(t *testing.T)            { runTestDispatchReleaseLock(t) }
func TestDispatchAcquireLock(t *testing.T)            { runTestDispatchAcquireLock(t) }
func TestDispatchSetState(t *testing.T)               { runTestDispatchSetState(t) }
func TestDispatchDeleteState(t *testing.T)            { runTestDispatchDeleteState(t) }
func TestDispatchIncrState(t *testing.T)              { runTestDispatchIncrState(t) }
func TestDispatchHasState(t *testing.T)               { runTestDispatchHasState(t) }
func TestDispatchSideEffect(t *testing.T)             { runTestDispatchSideEffect(t) }
func TestDispatchContinueAsNewVersioned(t *testing.T) { runTestDispatchContinueAsNewVersioned(t) }
func TestDispatchRegisterQueryHandler(t *testing.T)    { runTestDispatchRegisterQueryHandler(t) }
func TestDispatchComponentDefault(t *testing.T)        { runTestDispatchComponentDefault(t) }
func TestDispatchDurableDefer(t *testing.T)            { runTestDispatchDurableDefer(t) }
func TestDispatchPollCancellation(t *testing.T)        { runTestDispatchPollCancellation(t) }
func TestDispatchDurableCallString(t *testing.T)       { runTestDispatchDurableCallString(t) }
func TestDispatchChildWorkflow(t *testing.T)           { runTestDispatchChildWorkflow(t) }
func TestDispatchAwaitChild(t *testing.T)              { runTestDispatchAwaitChild(t) }
func TestDispatchCreatePromise(t *testing.T)           { runTestDispatchCreatePromise(t) }
func TestDispatchAwaitPromise(t *testing.T)            { runTestDispatchAwaitPromise(t) }
func TestDispatchSetScope(t *testing.T)                { runTestDispatchSetScope(t) }
func TestDispatchGetState(t *testing.T)                { runTestDispatchGetState(t) }
func TestDispatchDurableCallHeartbeat(t *testing.T)    { runTestDispatchDurableCallHeartbeat(t) }
func TestDispatchListState(t *testing.T)               { runTestDispatchListState(t) }
func TestDispatchWorkflowID(t *testing.T)              { runTestDispatchWorkflowID(t) }
func TestDispatchRunID(t *testing.T)                   { runTestDispatchRunID(t) }

// ==========================================================================
// Group 7: goComponentCallback tests
// ==========================================================================

func TestGoComponentCallbackNilHandler(t *testing.T)           { runTestGoComponentCallbackNilHandler(t) }
func TestGoComponentCallbackDispatch(t *testing.T)             { runTestGoComponentCallbackDispatch(t) }
func TestGoComponentCallbackUnregistered(t *testing.T)         { runTestGoComponentCallbackUnregistered(t) }
func TestGoComponentCallbackDispatchDurableSleep(t *testing.T) { runTestGCCallbackSleep(t) }
func TestGoComponentCallbackDispatchDurableLog(t *testing.T)   { runTestGCCallbackDurableLog(t) }
func TestGoComponentCallbackDispatchRandom(t *testing.T)       { runTestGCCallbackRandom(t) }
