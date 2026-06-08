package engine

import (
	"bytes"
	"context"
	"os/exec"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// parseWat converts WAT text to WASM bytes using wasm-tools parse.
func parseWat(t *testing.T, wat string) []byte {
	t.Helper()
	cmd := exec.Command("wasm-tools", "parse", "-")
	cmd.Stdin = bytes.NewBufferString(wat)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wasm-tools parse failed: %v\nstderr: %s\nWAT:\n%s", err, stderr.String(), wat)
	}
	return stdout.Bytes()
}

// skipIfNoWasmTools skips the test if wasm-tools is not available.
func skipIfNoWasmTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("wasm-tools"); err != nil {
		t.Skip("wasm-tools not available — skipping WASM host function test")
	}
}

// newTestRuntime creates a Runtime and returns it with a cleanup function.
func newTestRuntime(t *testing.T) (*Runtime, func()) {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt, func() { rt.Close(ctx) }
}

// instantiateTestModule compiles a WASM module and instantiates it using
// the Runtime's existing module config. The handler context is passed
// directly to Function.Call, which propagates to host functions.
func instantiateTestModule(t *testing.T, rt *Runtime, wasmBytes []byte) (api.Module, func()) {
	t.Helper()
	ctx := context.Background()
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		compiled.Close(ctx)
		t.Fatalf("InstantiateModule: %v", err)
	}
	return mod, func() {
		mod.Close(ctx)
		compiled.Close(ctx)
	}
}

// callExport calls an exported function on a WASM module and returns the first result.
func callExport(t *testing.T, mod api.Module, name string, ctx context.Context, params ...uint64) []uint64 {
	t.Helper()
	fn := mod.ExportedFunction(name)
	if fn == nil {
		t.Fatalf("export %q not found", name)
	}
	results, err := fn.Call(ctx, params...)
	if err != nil {
		t.Fatalf("call %q: %v", name, err)
	}
	return results
}

// writeMem writes a string to module memory at the given offset.
func writeMem(t *testing.T, mod api.Module, offset uint32, s string) {
	t.Helper()
	mem := mod.Memory()
	if mem == nil {
		t.Fatal("module has no memory")
	}
	ok := mem.Write(offset, []byte(s))
	if !ok {
		t.Fatalf("write to offset %d failed", offset)
	}
}

// testHostHandler implements HostHandler for testing host import closures.
// Each method returns the configured result value.
type testHostHandler struct {
	durableCallResult              int64
	durableSleepResult             int64
	durableAwaitSignalsResult      int64
	durableDeferResult             int64
	durableLogResult               int64
	pollCancellationResult         int64
	pollSignalResult               int64
	continueAsNewResult            int64
	continueAsNewWithVersionResult int64
	childWorkflowResult            int64
	childWorkflowWithOptionsResult int64
	childWorkflowInSchemaResult    int64
	awaitChildResult               int64
	awaitAllChildrenResult         int64
	pollChildResult                int64
	awaitAnyChildResult            int64
	durableCallWithRetryResult     int64
	durableCallWithHeartbeatResult int64
	versionResult                  int64
	minVersionResult               int64
	setQueryStateResult            int64
	nowResult                      int64
	randomResult                   int64
	createPromiseResult            int64
	awaitPromiseResult             int64
	pluginCallResult               int64
	pluginCallStreamingResult      int64
	registerUpdateHandlerResult    int64
	sendSignalAndWaitResult        int64
	replyToSignalResult            int64
	signalWorkflowResult           int64
	setScopeResult                 int64
	getScopeResult                 int64
	uuidResult                     int64
	acquireLockResult              int64
	releaseLockResult              int64
	sideEffectResult               int64
	workflowIDResult               int64
	runIDResult                    int64
	resolvePromiseResult           int64
	rejectPromiseResult            int64
	durableSendResult              int64
	durableScheduleInvokeResult    int64
	registerQueryHandlerResult     int64
	setStateResult                 int64
	getStateResult                 int64
	deleteStateResult              int64
	incrStateResult                int64
	hasStateResult                 int64
	listStateResult                int64
	runDetachedResult              int64
	fetchResult                    int64
	jsonParseResult                int64
	jsonStringifyResult            int64
}

func (h *testHostHandler) DurableCall(ctx context.Context, m api.Module, service, operation, requestJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.durableCallResult
}
func (h *testHostHandler) DurableSleep(ctx context.Context, m api.Module, durationMs int64) int64 {
	return h.durableSleepResult
}
func (h *testHostHandler) DurableAwaitSignals(ctx context.Context, m api.Module, signalNames string, timeoutMs int64, sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen uint32) int64 {
	return h.durableAwaitSignalsResult
}
func (h *testHostHandler) DurableDefer(ctx context.Context, m api.Module, description string, deferIDPtr, deferIDMaxLen uint32) int64 {
	return h.durableDeferResult
}
func (h *testHostHandler) DurableLog(ctx context.Context, m api.Module, message string) int64 {
	return h.durableLogResult
}
func (h *testHostHandler) PollCancellation(ctx context.Context, m api.Module, reasonPtr, reasonMaxLen uint32) int64 {
	return h.pollCancellationResult
}
func (h *testHostHandler) PollSignal(ctx context.Context, m api.Module, signalName string, payloadPtr, payloadMaxLen uint32) int64 {
	return h.pollSignalResult
}
func (h *testHostHandler) ContinueAsNew(ctx context.Context, m api.Module, newInputJSON string) int64 {
	return h.continueAsNewResult
}
func (h *testHostHandler) ContinueAsNewWithVersion(ctx context.Context, m api.Module, newInputJSON string, newVersion int) int64 {
	return h.continueAsNewWithVersionResult
}
func (h *testHostHandler) ChildWorkflow(ctx context.Context, m api.Module, name, inputJSON string, runIDPtr, runIDMaxLen uint32) int64 {
	return h.childWorkflowResult
}
func (h *testHostHandler) ChildWorkflowWithOptions(ctx context.Context, m api.Module, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return h.childWorkflowWithOptionsResult
}
func (h *testHostHandler) ChildWorkflowInSchema(ctx context.Context, m api.Module, targetSchema, name, inputJSON string, version int64, priority int64, parentClosePolicy string, runIDPtr, runIDMaxLen uint32) int64 {
	return h.childWorkflowInSchemaResult
}
func (h *testHostHandler) AwaitChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	return h.awaitChildResult
}
func (h *testHostHandler) AwaitAllChildren(ctx context.Context, m api.Module, runIDsJSON string, resultsPtr, resultsMaxLen uint32) int64 {
	return h.awaitAllChildrenResult
}
func (h *testHostHandler) PollChild(ctx context.Context, m api.Module, runID string, resultPtr, resultMaxLen uint32) int64 {
	return h.pollChildResult
}
func (h *testHostHandler) AwaitAnyChild(ctx context.Context, m api.Module, runIDsJSON string, resultPtr, resultMaxLen uint32) int64 {
	return h.awaitAnyChildResult
}
func (h *testHostHandler) DurableCallWithRetry(ctx context.Context, m api.Module, service, operation, requestJSON string, maxAttempts, initialIntervalMs, backoffCoefficient100x, maxIntervalMs int64, nonRetryableErrorsJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.durableCallWithRetryResult
}
func (h *testHostHandler) DurableCallWithHeartbeat(ctx context.Context, m api.Module, service, operation, requestJSON string, heartbeatIntervalMs int64, responsePtr, responseMaxLen uint32) int64 {
	return h.durableCallWithHeartbeatResult
}
func (h *testHostHandler) Version(ctx context.Context) int64 {
	return h.versionResult
}
func (h *testHostHandler) MinVersion(ctx context.Context) int64 {
	return h.minVersionResult
}
func (h *testHostHandler) SetQueryState(ctx context.Context, m api.Module, key, value string) int64 {
	return h.setQueryStateResult
}
func (h *testHostHandler) Now(ctx context.Context) int64 {
	return h.nowResult
}
func (h *testHostHandler) Random(ctx context.Context) int64 {
	return h.randomResult
}
func (h *testHostHandler) CreatePromise(ctx context.Context, m api.Module, name string, promiseIDPtr, promiseIDMaxLen uint32) int64 {
	return h.createPromiseResult
}
func (h *testHostHandler) AwaitPromise(ctx context.Context, m api.Module, promiseID string, timeoutMs int64, resultPtr, resultMaxLen uint32) int64 {
	return h.awaitPromiseResult
}
func (h *testHostHandler) PluginCall(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.pluginCallResult
}
func (h *testHostHandler) PluginCallStreaming(ctx context.Context, m api.Module, pluginName, functionName, inputJSON string, responsePtr, responseMaxLen uint32) int64 {
	return h.pluginCallStreamingResult
}
func (h *testHostHandler) RegisterUpdateHandler(ctx context.Context, m api.Module, name string) int64 {
	return h.registerUpdateHandlerResult
}
func (h *testHostHandler) SendSignalAndWait(ctx context.Context, m api.Module, targetRunID, signalName, payload string, timeoutMs int64, responsePtr, responseMaxLen uint32) int64 {
	return h.sendSignalAndWaitResult
}
func (h *testHostHandler) ReplyToSignal(ctx context.Context, m api.Module, correlationID, response string) int64 {
	return h.replyToSignalResult
}
func (h *testHostHandler) SignalWorkflow(ctx context.Context, m api.Module, targetRunID, signalName, payload string) int64 {
	return h.signalWorkflowResult
}
func (h *testHostHandler) SetScope(ctx context.Context, m api.Module, objectType, instanceKey string, prevScopePtr, prevScopeMaxLen uint32) int64 {
	return h.setScopeResult
}
func (h *testHostHandler) GetScope(ctx context.Context, m api.Module, objTypePtr, objTypeMaxLen, instKeyPtr, instKeyMaxLen uint32) int64 {
	return h.getScopeResult
}
func (h *testHostHandler) UUID(ctx context.Context, m api.Module, seed string, uuidPtr, uuidMaxLen uint32) int64 {
	return h.uuidResult
}
func (h *testHostHandler) AcquireLock(ctx context.Context, m api.Module, key string, ttlMs int64) int64 {
	return h.acquireLockResult
}
func (h *testHostHandler) ReleaseLock(ctx context.Context, m api.Module, key string) int64 {
	return h.releaseLockResult
}
func (h *testHostHandler) SideEffect(ctx context.Context, m api.Module, computedResult string, respPtr, respMaxLen uint32) int64 {
	return h.sideEffectResult
}
func (h *testHostHandler) WorkflowID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	return h.workflowIDResult
}
func (h *testHostHandler) RunID(ctx context.Context, m api.Module, idPtr, idMaxLen uint32) int64 {
	return h.runIDResult
}
func (h *testHostHandler) ResolvePromise(ctx context.Context, m api.Module, promiseID, value string) int64 {
	return h.resolvePromiseResult
}
func (h *testHostHandler) RejectPromise(ctx context.Context, m api.Module, promiseID, errMsg string) int64 {
	return h.rejectPromiseResult
}
func (h *testHostHandler) DurableSend(ctx context.Context, m api.Module, service, operation, requestJSON string) int64 {
	return h.durableSendResult
}
func (h *testHostHandler) DurableScheduleInvoke(ctx context.Context, m api.Module, service, operation, requestJSON string, delayMs int64) int64 {
	return h.durableScheduleInvokeResult
}
func (h *testHostHandler) RegisterQueryHandler(ctx context.Context, m api.Module, name string) int64 {
	return h.registerQueryHandlerResult
}
func (h *testHostHandler) SetState(ctx context.Context, m api.Module, key, value string) int64 {
	return h.setStateResult
}
func (h *testHostHandler) GetState(ctx context.Context, m api.Module, key string, valuePtr, valueMaxLen uint32) int64 {
	return h.getStateResult
}
func (h *testHostHandler) DeleteState(ctx context.Context, m api.Module, key string) int64 {
	return h.deleteStateResult
}
func (h *testHostHandler) IncrState(ctx context.Context, m api.Module, key string, delta int64) int64 {
	return h.incrStateResult
}
func (h *testHostHandler) HasState(ctx context.Context, m api.Module, key string) int64 {
	return h.hasStateResult
}
func (h *testHostHandler) ListState(ctx context.Context, m api.Module, prefix string, keysPtr, keysMaxLen uint32) int64 {
	return h.listStateResult
}
func (h *testHostHandler) RunDetached(ctx context.Context, m api.Module, name, inputJSON string) int64 {
	return h.runDetachedResult
}
func (h *testHostHandler) Fetch(ctx context.Context, m api.Module, method, url, headersJSON, body string, responsePtr, responseMaxLen uint32) int64 {
	return h.fetchResult
}
func (h *testHostHandler) JsonParse(ctx context.Context, m api.Module, jsonPtr, jsonLen, outPtr, outMaxLen uint32) int64 {
	return h.jsonParseResult
}
func (h *testHostHandler) JsonStringify(ctx context.Context, m api.Module, ptr, len, outPtr, outMaxLen uint32) int64 {
	return h.jsonStringifyResult
}

// compile-time check: testHostHandler implements HostHandler
var _ HostHandler = (*testHostHandler)(nil)

// =============================================================================
// Phase B1: Handler context helpers
// =============================================================================

func TestWithHandler_Roundtrip(t *testing.T) {
	ctx := context.Background()
	h := &testHostHandler{nowResult: 42}
	ctx2 := withHandler(ctx, h)
	got := handlerFromContext(ctx2)
	if got != h {
		t.Error("handlerFromContext did not return the stored handler")
	}
	if got.Now(ctx) != 42 {
		t.Error("wrong result from stored handler")
	}
}

func TestHandlerFromContext_PanicsOnMissing(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic from handlerFromContext with no handler in context")
		}
	}()
	handlerFromContext(context.Background())
}

// =============================================================================
// Phase B2: Zero-arg host functions
// =============================================================================

func TestCleatNow(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_now" (func $now (result i64)))
		(memory 1)
		(func (export "test_now") (result i64)
			call $now
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	h := &testHostHandler{nowResult: 12345}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_now", ctx)
	if results[0] != 12345 {
		t.Errorf("cleat_now returned %d, want 12345", results[0])
	}
}

func TestCleatVersion(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_version" (func $version (result i64)))
		(memory 1)
		(func (export "test_version") (result i64)
			call $version
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	h := &testHostHandler{versionResult: 99}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_version", ctx)
	if results[0] != 99 {
		t.Errorf("cleat_version returned %d, want 99", results[0])
	}
}

func TestCleatRandom(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_random" (func $random (result i64)))
		(memory 1)
		(func (export "test_random") (result i64)
			call $random
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	h := &testHostHandler{randomResult: 777}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_random", ctx)
	if results[0] != 777 {
		t.Errorf("cleat_random returned %d, want 777", results[0])
	}
}

// =============================================================================
// Phase B3: Single-arg host functions
// =============================================================================

func TestCleatSleep(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_sleep" (func $sleep (param i64) (result i64)))
		(memory 1)
		(func (export "test_sleep") (param i64) (result i64)
			local.get 0
			call $sleep
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	h := &testHostHandler{durableSleepResult: 1}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_sleep", ctx, 5000)
	if results[0] != 1 {
		t.Errorf("cleat_sleep returned %d, want 1", results[0])
	}
}

func TestCleatLog(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_log" (func $log (param i32 i32) (result i64)))
		(memory 1)
		(func (export "test_log") (param i32 i32) (result i64)
			local.get 0
			local.get 1
			call $log
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	writeMem(t, mod, 0, "hello world")
	h := &testHostHandler{durableLogResult: 0}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_log", ctx, 0, 11)
	if results[0] != 0 {
		t.Errorf("cleat_log returned %d, want 0", results[0])
	}
}

func TestCleatContinueAsNew(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_continue_as_new" (func $can (param i32 i32) (result i64)))
		(memory 1)
		(func (export "test_can") (param i32 i32) (result i64)
			local.get 0
			local.get 1
			call $can
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	writeMem(t, mod, 0, `{"v":1}`)
	h := &testHostHandler{continueAsNewResult: 0}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_can", ctx, 0, 7)
	if results[0] != 0 {
		t.Errorf("cleat_continue_as_new returned %d, want 0", results[0])
	}
}

// =============================================================================
// Phase B4: Multi-arg host functions
// =============================================================================

func TestCleatCall(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_call" (func $call (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)))
		(memory 1)
		(func (export "test_call") (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
			local.get 0
			local.get 1
			local.get 2
			local.get 3
			local.get 4
			local.get 5
			local.get 6
			local.get 7
			call $call
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	// Write service, operation, and request strings to memory.
	// Layout: svc at 0 (len 8), op at 64 (len 8), req at 128 (len 7)
	writeMem(t, mod, 0, "MySvc.v1")
	writeMem(t, mod, 64, "MyOp.v1")
	writeMem(t, mod, 128, `{"k":1}`)

	h := &testHostHandler{durableCallResult: 0}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_call", ctx,
		0, 8,    // svcPtr, svcLen ("MySvc.v1" = 8 bytes)
		64, 7,   // opPtr, opLen ("MyOp.v1" = 7 bytes)
		128, 7,  // reqPtr, reqLen (`{"k":1}` = 7 bytes)
		0, 0,    // respPtr, respMaxLen
	)
	if results[0] != 0 {
		t.Errorf("cleat_call returned %d, want 0", results[0])
	}
}

func TestSetQueryState(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "set_query_state" (func $sqs (param i32 i32 i32 i32) (result i64)))
		(memory 1)
		(func (export "test_sqs") (param i32 i32 i32 i32) (result i64)
			local.get 0
			local.get 1
			local.get 2
			local.get 3
			call $sqs
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	writeMem(t, mod, 0, "myKey")
	writeMem(t, mod, 64, "myValue")
	h := &testHostHandler{setQueryStateResult: 0}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_sqs", ctx, 0, 5, 64, 7)
	if results[0] != 0 {
		t.Errorf("set_query_state returned %d, want 0", results[0])
	}
}

func TestCleatCreatePromise(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_create_promise" (func $cp (param i32 i32 i32 i32) (result i64)))
		(memory 1)
		(func (export "test_cp") (param i32 i32 i32 i32) (result i64)
			local.get 0
			local.get 1
			local.get 2
			local.get 3
			call $cp
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	writeMem(t, mod, 0, "promise1")
	h := &testHostHandler{createPromiseResult: 0}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_cp", ctx, 0, 8, 128, 64)
	if results[0] != 0 {
		t.Errorf("cleat_create_promise returned %d, want 0", results[0])
	}
}

func TestCleatSetState(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_set_state" (func $ss (param i32 i32 i32 i32) (result i64)))
		(memory 1)
		(func (export "test_ss") (param i32 i32 i32 i32) (result i64)
			local.get 0
			local.get 1
			local.get 2
			local.get 3
			call $ss
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	writeMem(t, mod, 0, "stateKey")
	writeMem(t, mod, 64, "stateVal")
	h := &testHostHandler{setStateResult: 0}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_ss", ctx, 0, 8, 64, 8)
	if results[0] != 0 {
		t.Errorf("cleat_set_state returned %d, want 0", results[0])
	}
}

func TestCleatAcquireLock(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_acquire_lock" (func $al (param i32 i32 i64) (result i64)))
		(memory 1)
		(func (export "test_al") (param i32 i32 i64) (result i64)
			local.get 0
			local.get 1
			local.get 2
			call $al
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	writeMem(t, mod, 0, "lockKey1")
	h := &testHostHandler{acquireLockResult: 1}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_al", ctx, 0, 8, 5000)
	if results[0] != 1 {
		t.Errorf("cleat_acquire_lock returned %d, want 1", results[0])
	}
}

// =============================================================================
// Phase B5: Error paths
// =============================================================================

func TestBadParam_InvalidServiceName(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_poll_signal" (func $ps (param i32 i32 i32 i32) (result i64)))
		(memory 1)
		(func (export "test_ps") (param i32 i32 i32 i32) (result i64)
			local.get 0
			local.get 1
			local.get 2
			local.get 3
			call $ps
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	// Write a name with invalid characters (spaces and special chars).
	// readServiceName returns ("", false) for invalid chars.
	writeMem(t, mod, 0, "bad name!")
	h := &testHostHandler{pollSignalResult: 42}
	ctx := withHandler(context.Background(), h)
	results := callExport(t, mod, "test_ps", ctx, 0, 9, 64, 32)
	if results[0] != errBadParam {
		t.Errorf("expected errBadParam (0x%x), got 0x%x", errBadParam, results[0])
	}
}

func TestBadParam_StringTooLong(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_log" (func $log (param i32 i32) (result i64)))
		(memory 1)
		(func (export "test_log") (param i32 i32) (result i64)
			local.get 0
			local.get 1
			call $log
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	// Lower MaxWasmStringLen so we can test the too-long path.
	orig := MaxWasmStringLen
	MaxWasmStringLen = 5
	defer func() { MaxWasmStringLen = orig }()

	h := &testHostHandler{durableLogResult: 42}
	ctx := withHandler(context.Background(), h)
	// The string "hello world" is 11 bytes, exceeding the limit of 5.
	writeMem(t, mod, 0, "hello world")
	results := callExport(t, mod, "test_log", ctx, 0, 11)
	if results[0] != errBadParam {
		t.Errorf("expected errBadParam (0x%x) for too-long string, got 0x%x", errBadParam, results[0])
	}
}

func TestBadParam_EmptyString(t *testing.T) {
	skipIfNoWasmTools(t)
	rt, cleanup := newTestRuntime(t)
	defer cleanup()

	wat := `(module
		(import "env" "cleat_log" (func $log (param i32 i32) (result i64)))
		(memory 1)
		(func (export "test_log") (param i32 i32) (result i64)
			local.get 0
			local.get 1
			call $log
		)
	)`
	wasmBytes := parseWat(t, wat)
	mod, cleanup2 := instantiateTestModule(t, rt, wasmBytes)
	defer cleanup2()

	h := &testHostHandler{durableLogResult: 42}
	ctx := withHandler(context.Background(), h)
	// Pass len=0 which should fail readWasmStringValidated.
	results := callExport(t, mod, "test_log", ctx, 0, 0)
	if results[0] != errBadParam {
		t.Errorf("expected errBadParam (0x%x) for empty string, got 0x%x", errBadParam, results[0])
	}
}
