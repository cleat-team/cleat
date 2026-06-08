// Package test_runner provides a Go test harness for AssemblyScript
// cleat workflows compiled to WASM.
//
// It loads a .wasm binary, instantiates it via wazero, wires up mock
// implementations for all 22 cleat host imports, and provides a clean
// API for Go integration tests.
//
// Usage:
//
//	import "github.com/cleat-team/cleat/packages/cleat-as/test_runner"
//
//	func TestPlaceOrder(t *testing.T) {
//	    env := test_runner.NewWASMTestEnv(t, "path/to/workflow.wasm")
//	    defer env.Close()
//
//	    // Stub external service calls
//	    env.OnCall("inventory", "Reserve", "").Return(`{"reservationID":"r1","totalCents":5000}`, nil)
//	    env.OnCall("payments", "Charge", "").Return(`{"chargeID":"c1"}`, nil)
//	    env.OnCall("shipping", "CreateShipment", "").Return(`{"trackingID":"t1"}`, nil)
//
//	    // Call the workflow
//	    result, err := env.CallWorkflow("place_order", `{"userID":"u1","items":[{"sku":"s1","quantity":2}]}`)
//
//	    // Assert
//	    if err != nil {
//	        t.Fatalf("workflow failed: %v", err)
//	    }
//	    env.AssertCalled(t, "inventory", "Reserve")
//	    env.AssertCalled(t, "payments", "Charge")
//	}
package test_runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

// TestingT is the minimal interface needed for test assertions.
// *testing.T satisfies this.
type TestingT interface {
	Fatalf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

// CallRecord records a single durable call made during a workflow execution.
type CallRecord struct {
	Service   string
	Operation string
	Request   string
	Response  string
	Err       error
}

// WASMTestEnv is a test environment for running AssemblyScript WASM workflows.
// It loads a WASM binary, instantiates it with mock host functions, and
// provides methods to stub calls, invoke workflows, and inspect call history.
type WASMTestEnv struct {
	t            TestingT
	ctx          context.Context
	wasmBytes    []byte
	module       api.Module
	runtime      wazero.Runtime
	callHistory  []CallRecord
	callStubs    []callStub
	mu           sync.Mutex
	nowMs        int64
	memory       []byte // scratch buffer for string I/O
}

// callStub stores a registered stub for a durable call.
type callStub struct {
	service   string
	operation string
	response  string
	err       error
}

// CallStubBuilder is returned by OnCall to configure a stub response.
type CallStubBuilder struct {
	env       *WASMTestEnv
	service   string
	operation string
}

// Return registers a stub that returns the given response and error.
func (b *CallStubBuilder) Return(response string, err error) *CallStubBuilder {
	b.env.mu.Lock()
	defer b.env.mu.Unlock()
	b.env.callStubs = append(b.env.callStubs, callStub{
		service:   b.service,
		operation: b.operation,
		response:  response,
		err:       err,
	})
	return b
}

// ReturnJSON marshals v as JSON and registers a stub returning that JSON.
func (b *CallStubBuilder) ReturnJSON(v interface{}, err error) *CallStubBuilder {
	data, marshalErr := json.Marshal(v)
	if marshalErr != nil {
		panic(fmt.Sprintf("test_runner: ReturnJSON marshal error: %v", marshalErr))
	}
	return b.Return(string(data), err)
}

// ---------------------------------------------------------------------------
// NewWASMTestEnv
// ---------------------------------------------------------------------------

// NewWASMTestEnv creates a new WASMTestEnv by loading the WASM binary at
// wasmPath and instantiating it with wazero. All cleat host imports are
// wired to mock implementations.
//
// Call env.Close() when done to release WASM resources.
func NewWASMTestEnv(t TestingT, wasmPath string) *WASMTestEnv {
	ctx := context.Background()

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("test_runner: reading WASM binary: %v", err)
		return nil
	}

	r := wazero.NewRuntime(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	env := &WASMTestEnv{
		t:         t,
		ctx:       ctx,
		wasmBytes: wasmBytes,
		runtime:   r,
		nowMs:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		memory:   make([]byte, 10*1024*1024+65536), // 10 MiB scratch + 64 KiB output
	}

	// Build the host module with all cleat imports.
	host := r.NewHostModuleBuilder("env")

	// cleat_sleep (param i64) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, durationMs int64) int64 {
		return env.durableSleep(durationMs)
	}).Export("cleat_sleep")

	// cleat_now (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module) int64 {
		return env.durableNow()
	}).Export("cleat_now")

	// cleat_log (param i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, msgPtr, msgLen int32) int64 {
		env.durableLog(msgPtr, msgLen)
		return 0
	}).Export("cleat_log")

	// cleat_call (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen int32) int64 {
		return env.durableCall(m, svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen)
	}).Export("cleat_call")

	// cleat_await_signals (param i32 i32 i64 i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		namesPtr, namesLen int32, timeoutMs int64,
		sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen int32) int64 {
		return env.durableAwaitSignals(m, namesPtr, namesLen, timeoutMs,
			sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen)
	}).Export("cleat_await_signals")

	// set_query_state (param i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		keyPtr, keyLen, valPtr, valLen int32) int64 {
		return 0 // no-op for testing
	}).Export("set_query_state")

	// cleat_random (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module) int64 {
		return 42
	}).Export("cleat_random")

	// cleat_version (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module) int64 {
		return 1
	}).Export("cleat_version")

	// cleat_min_version (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module) int64 {
		return 1
	}).Export("cleat_min_version")

	// cleat_defer (param i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		descPtr, descLen, deferIdPtr, deferIdMaxLen int32) int64 {
		// Write a defer ID to output buffer
		deferID := "test-defer-1"
		env.writeString(uint32(deferIdPtr), deferID)
		return encodeSimpleResult(int64(len(deferID)), 0)
	}).Export("cleat_defer")

	// cleat_poll_cancellation (param i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, reasonPtr, reasonMaxLen int32) int64 {
		return 0 // not cancelled
	}).Export("cleat_poll_cancellation")

	// cleat_poll_signal (param i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		namePtr, nameLen, payloadPtr, payloadMaxLen int32) int64 {
		return 0 // signal not found
	}).Export("cleat_poll_signal")

	// cleat_continue_as_new (param i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, inputPtr, inputLen int32) int64 {
		return 0 // no-op for testing
	}).Export("cleat_continue_as_new")

	// cleat_child_workflow (param i32 i32 i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		namePtr, nameLen, inputPtr, inputLen, runIdPtr, runIdMaxLen int32) int64 {
		name := env.readString(uint32(namePtr), uint32(nameLen))
		runID := fmt.Sprintf("test-child-%s", name)
		env.writeString(uint32(runIdPtr), runID)
		return encodeSimpleResult(int64(len(runID)), 0)
	}).Export("cleat_child_workflow")

	// cleat_await_child (param i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		runIdPtr, runIdLen, resultPtr, resultMaxLen int32) int64 {
		result := `{"status":"completed"}`
		env.writeString(uint32(resultPtr), result)
		return encodeSimpleResult(int64(len(result)), 0)
	}).Export("cleat_await_child")

	// cleat_create_promise (param i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		namePtr, nameLen, idOutPtr, idOutMax int32) int64 {
		promiseID := "test-promise-1"
		env.writeString(uint32(idOutPtr), promiseID)
		return encodeSimpleResult(int64(len(promiseID)), 0)
	}).Export("cleat_create_promise")

	// cleat_await_promise (param i32 i32 i64 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		idPtr, idLen int32, timeoutMs int64, resultOutPtr, resultOutMax int32) int64 {
		return 0 // timed out
	}).Export("cleat_await_promise")

	// cleat_register_update_handler (param i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, namePtr, nameLen int32) int64 {
		return 0
	}).Export("cleat_register_update_handler")

	// plugin_call (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		pnPtr, pnLen, fnPtr, fnLen, inpPtr, inpLen, respPtr, respMaxLen int32) int64 {
		return encodeCallResult(0, 0, 1) // error — no plugin stubs
	}).Export("plugin_call")

	// cleat_workflow_id (param i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, idPtr, idMaxLen int32) int64 {
		wfID := "test-workflow"
		env.writeString(uint32(idPtr), wfID)
		return encodeSimpleResult(int64(len(wfID)), 0)
	}).Export("cleat_workflow_id")

	// cleat_run_id (param i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, idPtr, idMaxLen int32) int64 {
		runID := "test-run"
		env.writeString(uint32(idPtr), runID)
		return encodeSimpleResult(int64(len(runID)), 0)
	}).Export("cleat_run_id")

	// cleat_send (param i32 i32 i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32) int64 {
		return 0
	}).Export("cleat_send")

	// cleat_schedule_invoke (param i32 i32 i32 i32 i32 i32 i64) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32, delayMs int64) int64 {
		return 0
	}).Export("cleat_schedule_invoke")

	// cleat_register_query_handler (param i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, namePtr, nameLen int32) int64 {
		return 0
	}).Export("cleat_register_query_handler")

	// cleat_call_retry (param ...) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32,
		maxAttempts, initialIntervalMs, backoff100x, maxIntervalMs int64,
		nrePtr, nreLen, respPtr, respMaxLen int32) int64 {
		return env.durableCall(m, svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen)
	}).Export("cleat_call_retry")

	// cleat_call_heartbeat (param ...) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen int32,
		heartbeatIntervalMs int64, respPtr, respMaxLen int32) int64 {
		return env.durableCall(m, svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen)
	}).Export("cleat_call_heartbeat")

	// cleat_await_all_children (param i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		runIdsPtr, runIdsLen, resultsPtr, resultsMaxLen int32) int64 {
		result := `[]`
		env.writeString(uint32(resultsPtr), result)
		return encodeSimpleResult(int64(len(result)), 0)
	}).Export("cleat_await_all_children")

	// cleat_resolve_promise (param i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		idPtr, idLen, valPtr, valLen int32) int64 {
		return 0
	}).Export("cleat_resolve_promise")

	// cleat_reject_promise (param i32 i32 i32 i32) (result i64)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module,
		idPtr, idLen, errPtr, errLen int32) int64 {
		return 0
	}).Export("cleat_reject_promise")

	// Instantiate the host module.
	if _, err := host.Instantiate(ctx); err != nil {
		t.Fatalf("test_runner: instantiating host module: %v", err)
		return nil
	}

	// Instantiate the WASM module.
	mod, err := r.Instantiate(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("test_runner: instantiating WASM module: %v", err)
		return nil
	}

	env.module = mod
	return env
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// CallWorkflow invokes a named export function on the WASM module with the
// given JSON input. Returns the output JSON and any error.
func (e *WASMTestEnv) CallWorkflow(name, inputJSON string) (string, error) {
	export := e.module.ExportedFunction(name)
	if export == nil {
		return "", fmt.Errorf("test_runner: workflow %q not found in WASM module", name)
	}

	// Write input JSON to the scratch buffer.
	inputBytes := []byte(inputJSON)
	scratchBase := uint32(10 * 1024 * 1024) // 0xA00000
	e.writeMemory(scratchBase, inputBytes)

	outOffset := scratchBase + 65536   // OUTPUT_OFFSET
	maxOutLen := uint32(65536)

	ctx := context.Background()
	results, err := export.Call(ctx, uint64(scratchBase), uint64(len(inputBytes)), uint64(outOffset), uint64(maxOutLen))
	if err != nil {
		return "", fmt.Errorf("test_runner: workflow %q call failed: %w", name, err)
	}

	if len(results) == 0 {
		return "", nil
	}

	result := results[0]

	// Check for suspend sentinel (1 << 62)
	if result&(1<<62) != 0 {
		return `{"status":"suspended"}`, nil
	}

	// Decode the result: low 32 bits = errCode, high 32 bits = actualLen
	errCode := uint32(result & 0xFFFFFFFF)
	actualLen := uint32((result >> 32) & 0xFFFFFFFF)

	if errCode != 0 {
		output := e.readString(outOffset, actualLen)
		return "", fmt.Errorf("test_runner: workflow %q returned error code %d: %s", name, errCode, output)
	}

	output := e.readString(outOffset, actualLen)
	return output, nil
}

// OnCall registers a call stub and returns a builder for setting the response.
func (e *WASMTestEnv) OnCall(service, operation string, _ /* requestMatcher unused */ interface{}) *CallStubBuilder {
	return &CallStubBuilder{
		env:       e,
		service:   service,
		operation: operation,
	}
}

// AssertCalled fails the test if no call to the given service+operation
// appears in the call history.
func (e *WASMTestEnv) AssertCalled(t TestingT, service, operation string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range e.callHistory {
		if rec.Service == service && rec.Operation == operation {
			return
		}
	}
	t.Fatalf("test_runner: expected call to %s.%s was not made", service, operation)
}

// AssertNotCalled fails the test if a call to the given service+operation
// appears in the call history.
func (e *WASMTestEnv) AssertNotCalled(t TestingT, service, operation string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range e.callHistory {
		if rec.Service == service && rec.Operation == operation {
			t.Fatalf("test_runner: unexpected call to %s.%s was made (request: %s)",
				service, operation, rec.Request)
			return
		}
	}
}

// CallHistory returns a copy of all recorded calls.
func (e *WASMTestEnv) CallHistory() []CallRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]CallRecord, len(e.callHistory))
	copy(result, e.callHistory)
	return result
}

// Close releases WASM runtime resources.
func (e *WASMTestEnv) Close() {
	if e.module != nil {
		e.module.Close(e.ctx)
	}
	if e.runtime != nil {
		e.runtime.Close(e.ctx)
	}
}

// ---------------------------------------------------------------------------
// Internal: host function implementations
// ---------------------------------------------------------------------------

func (e *WASMTestEnv) durableNow() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nowMs
}

func (e *WASMTestEnv) durableSleep(durationMs int64) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nowMs += durationMs
	return 0 // completed
}

func (e *WASMTestEnv) durableLog(msgPtr, msgLen int32) {
	// No-op for testing
}

func (e *WASMTestEnv) durableCall(m api.Module,
	svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen, respPtr, respMaxLen int32) int64 {

	service := e.readString(uint32(svcPtr), uint32(svcLen))
	operation := e.readString(uint32(opPtr), uint32(opLen))
	request := e.readString(uint32(reqPtr), uint32(reqLen))

	e.mu.Lock()
	defer e.mu.Unlock()

	rec := CallRecord{
		Service:   service,
		Operation: operation,
		Request:   request,
	}

	// Find first matching stub.
	for i, stub := range e.callStubs {
		if stub.service == service && stub.operation == operation {
			// Consume the stub
			e.callStubs = append(e.callStubs[:i], e.callStubs[i+1:]...)
			rec.Response = stub.response
			if stub.err != nil {
				rec.Err = stub.err
				e.callHistory = append(e.callHistory, rec)
				return encodeCallResult(int64(len(stub.response)), 0, 1)
			}
			// Write response to output buffer
			e.writeString(uint32(respPtr), stub.response)
			e.callHistory = append(e.callHistory, rec)
			return encodeCallResult(int64(len(stub.response)), 0, 0)
		}
	}

	// No stub found.
	err := fmt.Errorf("test_runner: no stub registered for %s.%s (request: %q)",
		service, operation, request)
	rec.Err = err
	e.callHistory = append(e.callHistory, rec)
	return encodeCallResult(0, 0, 1)
}

func (e *WASMTestEnv) durableAwaitSignals(m api.Module,
	namesPtr, namesLen int32, timeoutMs int64,
	sigNamePtr, sigNameMaxLen, payloadPtr, payloadMaxLen int32) int64 {
	// Signal not found — return timed out.
	return encodeAwaitSignalsResult(0, 0, 1, 0)
}

func (e *WASMTestEnv) durableJSONParse(jsonPtr, jsonLen, outPtr, outMaxLen int32) int64 {
	input := e.readString(uint32(jsonPtr), uint32(jsonLen))
	if input == "" {
		return encodeSimpleResult(0, 1)
	}
	var v interface{}
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return encodeSimpleResult(0, 1)
	}
	normalized, err := json.Marshal(v)
	if err != nil {
		return encodeSimpleResult(0, 1)
	}
	e.writeString(uint32(outPtr), string(normalized))
	return encodeSimpleResult(int64(len(normalized)), 0)
}

func (e *WASMTestEnv) durableJSONStringify(ptr, length, outPtr, outMaxLen int32) int64 {
	input := e.readString(uint32(ptr), uint32(length))
	if input == "" {
		return encodeSimpleResult(0, 1)
	}
	var v interface{}
	if err := json.Unmarshal([]byte(input), &v); err != nil {
		return encodeSimpleResult(0, 1)
	}
	serialized, err := json.Marshal(v)
	if err != nil {
		return encodeSimpleResult(0, 1)
	}
	e.writeString(uint32(outPtr), string(serialized))
	return encodeSimpleResult(int64(len(serialized)), 0)
}

// ---------------------------------------------------------------------------
// Memory helpers
// ---------------------------------------------------------------------------

func (e *WASMTestEnv) readString(ptr, length uint32) string {
	if length == 0 {
		return ""
	}
	if int(ptr+length) > len(e.memory) {
		return ""
	}
	return string(e.memory[ptr : ptr+length])
}

func (e *WASMTestEnv) writeString(ptr uint32, s string) {
	data := []byte(s)
	e.writeMemory(ptr, data)
}

func (e *WASMTestEnv) writeMemory(ptr uint32, data []byte) {
	if int(ptr+uint32(len(data))) > len(e.memory) {
		return
	}
	copy(e.memory[ptr:], data)
}

// ---------------------------------------------------------------------------
// Result encoding (matching the Cleat ABI bit-packing)
// ---------------------------------------------------------------------------

// encodeSimpleResult: bits 0-7 = errCode, bits 32-63 = extra
func encodeSimpleResult(extra int64, errCode int64) int64 {
	return (extra << 32) | (errCode & 0xFF)
}

// encodeCallResult: bits 0-7 = errCode, bits 8-39 = callErrorCode, bits 40-63 = responseLen
func encodeCallResult(responseLen, callErrorCode, errCode int64) int64 {
	return (responseLen << 40) | ((callErrorCode & 0xFFFFFFFF) << 8) | (errCode & 0xFF)
}

// encodeAwaitSignalsResult: bits 0-15 = errCode, bits 16-31 = timedOut, bits 32-47 = payloadLen, bits 48-63 = sigNameLen
func encodeAwaitSignalsResult(sigNameLen, payloadLen, timedOut, errCode int64) int64 {
	return (sigNameLen << 48) | (payloadLen << 32) | ((timedOut & 0xFFFF) << 16) | (errCode & 0xFFFF)
}

// ensure WASMTestEnv implements the test runner interface.
