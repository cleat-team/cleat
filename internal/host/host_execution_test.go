package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcownie/cleat/internal/plugin"
)

// ---------------------------------------------------------------------------
// Additional mock types for execution tests
// ---------------------------------------------------------------------------

// errorCallerSimple is a ServiceCaller that returns a configurable error for
// every call.
type errorCallerSimple struct {
	err error
}

func (e *errorCallerSimple) Call(_ context.Context, _, _, _ string) (string, error) {
	return "", e.err
}

// mockPluginCaller is a PluginFunc that returns a fixed response.
func mockPluginCaller(_ context.Context, _ string) (string, error) {
	return `{"plugin_result":"ok"}`, nil
}

// ---------------------------------------------------------------------------
// freshCall tests
// ---------------------------------------------------------------------------

func TestFreshCall_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	caller := &mockCaller{}
	session := &execSession{
		engine:  &Engine{caller: caller},
		history: make([]EventRecord, 0),
	}

	result := session.freshCall(ctx, mod, "catalog", "LookupItem", `{"sku":"ABC-123"}`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode != 0 || callErrCode != 0 {
		t.Fatalf("expected success, got errCode=%d callErrCode=%d", errCode, callErrCode)
	}

	// Verify the event was recorded in history.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event in history, got %d", len(session.history))
	}
	rec := session.history[0]
	if rec.EventType != EventTypeCall {
		t.Errorf("expected Call event type, got %s", rec.EventType)
	}
	if rec.Service != "catalog" || rec.Op != "LookupItem" {
		t.Errorf("expected catalog.LookupItem, got %s.%s", rec.Service, rec.Op)
	}
	if rec.Response == "" {
		t.Error("expected non-empty response")
	}
	if rec.Err != "" {
		t.Errorf("expected empty error, got %q", rec.Err)
	}
	if session.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", session.stepCount)
	}

	// Verify the mock caller recorded the call.
	if len(caller.calls) != 1 {
		t.Errorf("expected 1 call in mock caller, got %d", len(caller.calls))
	}
}

func TestFreshCall_CallerError(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	caller := &errorCallerSimple{err: errors.New("service unavailable")}
	session := &execSession{
		engine:  &Engine{caller: caller},
		history: make([]EventRecord, 0),
	}

	result := session.freshCall(ctx, mod, "catalog", "LookupItem", `{}`, 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code when caller returns error")
	}

	// Error should be recorded in the event's Err field.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(session.history))
	}
	if session.history[0].Err != "service unavailable" {
		t.Errorf("expected error in event, got %q", session.history[0].Err)
	}
}

func TestFreshCall_Cancellation(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	sigStore := &mockSignalStore{cancelled: true, cancelReason: "user requested"}
	caller := &mockCaller{}
	session := &execSession{
		engine: &Engine{
			caller:      caller,
			signalStore: sigStore,
		},
		history: make([]EventRecord, 0),
	}

	result := session.freshCall(ctx, mod, "catalog", "LookupItem", `{}`, 0, 4096)
	_, callErrCode, errCode := decodeCallResult(result)
	if errCode == 0 {
		t.Error("expected error code when cancelled")
	}
	_ = callErrCode

	// No real call should have been made.
	if len(caller.calls) != 0 {
		t.Errorf("expected 0 real calls when cancelled, got %d", len(caller.calls))
	}
}

// ---------------------------------------------------------------------------
// DurableSleep fresh tests
// ---------------------------------------------------------------------------

func TestDurableSleep_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:    &Engine{caller: &mockCaller{}},
		history:   make([]EventRecord, 0),
		deferrals: make(map[string]string),
		nowMs:     1000000,
	}

	result := session.DurableSleep(ctx, mod, 5000)
	status, duration := decodeSleepResult(result)
	if status != sleepStatusSuspend {
		t.Errorf("expected sleep status suspend (%d), got %d", sleepStatusSuspend, status)
	}
	if duration != 5000 {
		t.Errorf("expected duration 5000, got %d", duration)
	}
	if session.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}

	// Sleep is local: no event recorded in history.
	if len(session.history) != 0 {
		t.Fatalf("expected 0 events (sleep is local), got %d", len(session.history))
	}
	// nowMs should advance by the sleep duration.
	if session.nowMs != 1000000+5000 {
		t.Errorf("expected nowMs=%d, got %d", 1000000+5000, session.nowMs)
	}
}

// ---------------------------------------------------------------------------
// DurableAwaitSignals fresh tests
// ---------------------------------------------------------------------------

func TestDurableAwaitSignals_NoStoreSuspends(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// No signal store → should record await and suspend.
	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
		nowMs:   1000000,
	}

	result := session.DurableAwaitSignals(ctx, mod, "payment", 30000, 0, 4096, 4096, 4096)
	if result == 0 {
		t.Error("expected non-zero result indicating suspend")
	}
	if session.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeAwaitSignals {
		t.Errorf("expected AwaitSignals event, got %s", session.history[0].EventType)
	}
}

func TestDurableAwaitSignals_WithPendingSignal(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	sigStore := &mockSignalStore{}
	sigStore.DeliverSignal(ctx, "", "payment", `{"paid":true}`)
	session := &execSession{
		engine: &Engine{
			caller:      &mockCaller{},
			signalStore: sigStore,
		},
		history: make([]EventRecord, 0),
	}

	// Signal is pending → should consume it immediately.
	result := session.DurableAwaitSignals(ctx, mod, "payment", 30000, 0, 4096, 4096, 4096)
	if result == 0 {
		t.Error("expected non-zero result indicating signal received")
	}
	if session.suspendErr != nil {
		t.Errorf("expected no suspend, got %v", session.suspendErr)
	}
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeSignalReceived {
		t.Errorf("expected SignalReceived event, got %s", session.history[0].EventType)
	}
}

// ---------------------------------------------------------------------------
// PluginCall fresh tests
// ---------------------------------------------------------------------------

func TestPluginCall_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	reg := NewPluginRegistry()
	reg.Register("myplugin", "DoSomething", mockPluginCaller)
	session := &execSession{
		engine: &Engine{
			pluginRegistry: reg,
			caller:         &mockCaller{},
		},
		history: make([]EventRecord, 0),
	}

	result := session.freshPluginCall(ctx, mod, "myplugin", "DoSomething", `{"key":"val"}`, 0, 4096)
	errCode, _ := decodeSimpleResult(result)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}

	// Event should be recorded.
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(session.history))
	}
	rec := session.history[0]
	if rec.EventType != EventTypePluginCall {
		t.Errorf("expected PluginCall event, got %s", rec.EventType)
	}
	if rec.PluginName != "myplugin" || rec.PluginFunc != "DoSomething" {
		t.Errorf("expected myplugin/DoSomething, got %s/%s", rec.PluginName, rec.PluginFunc)
	}
	if rec.PluginOutput != `{"plugin_result":"ok"}` {
		t.Errorf("expected plugin output, got %q", rec.PluginOutput)
	}
}

func TestPluginCall_FreshNoRegistry(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine: &Engine{
			pluginRegistry: nil, // no registry configured
			caller:         &mockCaller{},
		},
		history: make([]EventRecord, 0),
	}

	result := session.freshPluginCall(ctx, mod, "myplugin", "DoSomething", `{}`, 0, 4096)
	errCode, _ := decodeSimpleResult(result)
	if errCode == 0 {
		t.Error("expected error code when no plugin registry")
	}
}

func TestPluginCall_FreshFuncNotRegistered(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	reg := NewPluginRegistry()
	// Don't register the function.
	session := &execSession{
		engine: &Engine{
			pluginRegistry: reg,
			caller:         &mockCaller{},
		},
		history: make([]EventRecord, 0),
	}

	result := session.freshPluginCall(ctx, mod, "myplugin", "DoSomething", `{}`, 0, 4096)
	errCode, _ := decodeSimpleResult(result)
	if errCode == 0 {
		t.Error("expected error code when function not registered")
	}
}

// ---------------------------------------------------------------------------
// SideEffect fresh tests
// ---------------------------------------------------------------------------

func TestSideEffect_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	result := session.freshSideEffect(ctx, mod, `{"random":42}`, 0, 4096)
	errCode, _ := decodeSimpleResult(result)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeSideEffect {
		t.Errorf("expected SideEffect event, got %s", session.history[0].EventType)
	}
	if session.history[0].SideEffectResult != `{"random":42}` {
		t.Errorf("expected SideEffectResult=%q, got %q", `{"random":42}`, session.history[0].SideEffectResult)
	}
}

// ---------------------------------------------------------------------------
// ContinueAsNew fresh tests
// ---------------------------------------------------------------------------

func TestContinueAsNew_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	_ = session.ContinueAsNew(ctx, mod, `{"restart":true}`)
	if session.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if session.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", session.suspendErr.Reason)
	}
	if session.suspendErr.NewInput != `{"restart":true}` {
		t.Errorf("expected NewInput=%q, got %q", `{"restart":true}`, session.suspendErr.NewInput)
	}
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeContinueAsNew {
		t.Errorf("expected ContinueAsNew event, got %s", session.history[0].EventType)
	}
}

func TestContinueAsNewWithVersion_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	_ = session.ContinueAsNewWithVersion(ctx, mod, `{"upgrade":true}`, 3)
	if session.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if session.suspendErr.NewVersion != 3 {
		t.Errorf("expected NewVersion=3, got %d", session.suspendErr.NewVersion)
	}
	if session.suspendErr.NewInput != `{"upgrade":true}` {
		t.Errorf("expected NewInput=%q, got %q", `{"upgrade":true}`, session.suspendErr.NewInput)
	}
}

// ---------------------------------------------------------------------------
// Defer fresh tests
// ---------------------------------------------------------------------------

func TestDefer_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:    &Engine{caller: &mockCaller{}},
		history:   make([]EventRecord, 0),
		deferrals: make(map[string]string),
	}

	result := session.DurableDefer(ctx, mod, "cleanup resource", 0, 4096)
	if result == 0 {
		t.Error("expected non-zero result")
	}
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeDefer {
		t.Errorf("expected Defer event, got %s", session.history[0].EventType)
	}
	if session.history[0].DeferDescription != "cleanup resource" {
		t.Errorf("expected description 'cleanup resource', got %q", session.history[0].DeferDescription)
	}
	if len(session.deferrals) != 1 {
		t.Errorf("expected 1 deferral, got %d", len(session.deferrals))
	}
}

// ---------------------------------------------------------------------------
// CallWithRetry fresh tests (quick, no real sleep)
// ---------------------------------------------------------------------------

func TestCallWithRetry_FirstAttemptSucceeds(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	caller := &mockCaller{}
	session := &execSession{
		engine:  &Engine{caller: caller},
		history: make([]EventRecord, 0),
	}

	result := session.freshCallWithRetry(ctx, mod, "catalog", "LookupItem", `{"sku":"ABC-123"}`,
		3, 10, 200, 1000, "", 0, 4096)
	_, _, errCode := decodeCallResult(result)
	if errCode != 0 {
		t.Fatalf("expected success on first attempt, got errCode=%d", errCode)
	}
	if len(session.history) != 1 {
		t.Errorf("expected 1 event (success), got %d", len(session.history))
	}
}

// ---------------------------------------------------------------------------
// CreatePromise fresh test
// ---------------------------------------------------------------------------

func TestCreatePromise_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	result := session.CreatePromise(ctx, mod, "order-promise", 0, 4096)
	errCode, extra := decodeSimpleResult(result)
	if errCode != 0 {
		t.Fatalf("expected success, got errCode=%d", errCode)
	}
	if extra == 0 {
		t.Error("expected promise ID written to memory")
	}
	if len(session.history) != 1 {
		t.Fatalf("expected 1 event, got %d", len(session.history))
	}
	if session.history[0].EventType != EventTypeCreatePromise {
		t.Errorf("expected CreatePromise event, got %s", session.history[0].EventType)
	}
	if session.history[0].PromiseName != "order-promise" {
		t.Errorf("expected PromiseName='order-promise', got %q", session.history[0].PromiseName)
	}
}

// ---------------------------------------------------------------------------
// DispatchUpdate test
// ---------------------------------------------------------------------------

func TestDispatchUpdate_Valid(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, &mockCaller{},
		WithUpdateHandler(func(name, payload string) (string, error) {
			return `{"handled":true,"handler":"` + name + `"}`, nil
		}),
	)

	result, err := engine.DispatchUpdate(ctx, "update-shipping", `{"address":"new"}`)
	if err != nil {
		t.Fatalf("DispatchUpdate: %v", err)
	}
	if result != `{"handled":true,"handler":"update-shipping"}` {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestDispatchUpdate_NoHandler(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, &mockCaller{}) // no update handler
	_, err = engine.DispatchUpdate(ctx, "update-shipping", `{}`)
	if err == nil {
		t.Error("expected error when no update handler configured")
	}
}

// ---------------------------------------------------------------------------
// Engine.Execute / Replay with pre-built Rust WASM (if available)
// ---------------------------------------------------------------------------

// rustWasmPath returns the path to a pre-built Rust workflow WASM or skips the
// test if none is found and cargo is unavailable.
func rustWasmPath(t *testing.T) string {
	t.Helper()

	// Try locating the pre-built WASM relative to the project root.
	cwd, _ := os.Getwd()
	projectRoot := cwd
	if strings.HasSuffix(cwd, "internal/host") {
		projectRoot = filepath.Dir(filepath.Dir(cwd))
	}

	candidates := []string{
		filepath.Join(projectRoot, "examples", "rust-workflow", "target", "wasm32-wasip1", "release", "rust_workflow.wasm"),
	}

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Pre-built WASM not found; try building via cargo.
	// buildRustWasm is defined in rust_workflow_test.go.
	return buildRustWasm(t)
}

func TestEngineExecuteWithRustWasm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM execution test in short mode")
	}

	wasmPath := rustWasmPath(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	// Rust workflow expects snake_case JSON.
	input := []byte(`{"user_id":"rust-test","cart":[{"sku":"SKU-001","quantity":1}]}`)
	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected suspension: %v", suspended.Reason)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty event history")
	}

	// Filter durable_log events; verify the expected service calls.
	var callHistory []EventRecord
	for _, rec := range history {
		if rec.EventType != EventTypeDurableLog {
			callHistory = append(callHistory, rec)
		}
	}
	expectedCalls := []string{"inventory", "payments", "shipping", "notifications"}
	for i, svc := range expectedCalls {
		if i >= len(callHistory) {
			t.Errorf("step %d: missing (expected %s)", i, svc)
			continue
		}
		if callHistory[i].Service != svc {
			t.Errorf("step %d: expected service %s, got %s", i, svc, callHistory[i].Service)
		}
	}

	t.Logf("Rust workflow execute: result=%q, events=%d", result, len(history))
}

func TestEngineReplayWithRustWasm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM replay test in short mode")
	}

	wasmPath := rustWasmPath(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// First execution to capture history.
	caller1 := &mockCaller{}
	engine1 := NewEngine(rt, caller1)
	input := []byte(`{"user_id":"replay-test","cart":[{"sku":"SKU-001","quantity":1}]}`)
	result1, history, _, _, _, err := engine1.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Replay with captured history.
	caller2 := &mockCaller{}
	engine2 := NewEngine(rt, caller2)
	result2, _, _, _, _, err := engine2.Replay(ctx, wasmBytes, "place_order", input, history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result1 != result2 {
		t.Errorf("replay result mismatch: %q vs %q", result1, result2)
	}
	if len(caller2.calls) > 0 {
		t.Errorf("replay made %d real service calls (expected 0)", len(caller2.calls))
	}

	t.Logf("Rust workflow replay OK: result=%q, real calls=%d", result2, len(caller2.calls))
}

// TestEngineExecute_CancelOrder tests the cancel_order export path with Rust WASM.
func TestEngineExecute_CancelOrderRust(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM cancellation test in short mode")
	}

	wasmPath := rustWasmPath(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	input := []byte(`{"user_id":"cancel-test","cart":[{"sku":"SKU-001","quantity":1}]}`)
	result, history, _, _, _, err := engine.Execute(ctx, wasmBytes, "cancel_order", input)
	if err != nil {
		t.Fatalf("Execute cancel_order: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result from cancel_order")
	}
	t.Logf("cancel_order result=%q, events=%d", result, len(history))
	for i, rec := range history {
		t.Logf("  step %d: %s.%s (err=%s)", i, rec.Service, rec.Op, rec.Err)
	}
}

// TestEngineDivergenceWithRustWasm verifies divergence detection with Rust WASM.
func TestEngineDivergenceWithRustWasm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM divergence test in short mode")
	}

	wasmPath := rustWasmPath(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Execute to get history.
	caller1 := &mockCaller{}
	engine1 := NewEngine(rt, caller1)
	input := []byte(`{"user_id":"div-test","cart":[{"sku":"SKU-001","quantity":1}]}`)
	_, history, _, _, _, err := engine1.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Tamper with the first call event's service name (skip durable_log events).
	for i := range history {
		if history[i].EventType == EventTypeCall {
			history[i].Service = "tampered_service"
			break
		}
	}

	caller2 := &mockCaller{}
	engine2 := NewEngine(rt, caller2)
	_, _, _, _, _, err = engine2.Replay(ctx, wasmBytes, "place_order", input, history)
	if err == nil {
		t.Error("expected divergence error")
	} else {
		t.Logf("Divergence correctly detected: %v", err)
	}

	// No real calls should have been made during failed replay.
	if len(caller2.calls) > 0 {
		t.Errorf("failed replay made %d real calls (expected 0)", len(caller2.calls))
	}
}

// ---------------------------------------------------------------------------
// Engine.ReplayCompiled tests
// ---------------------------------------------------------------------------

func TestEngineReplayCompiled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Rust WASM replay test in short mode")
	}

	wasmPath := rustWasmPath(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read Rust WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Pre-compile the module.
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	// Execute (fresh) using compiled module.
	caller1 := &mockCaller{}
	engine1 := NewEngine(rt, caller1)
	input := []byte(`{"user_id":"comp-test","cart":[{"sku":"SKU-001","quantity":1}]}`)
	result1, history, _, _, _, err := engine1.ExecuteCompiled(ctx, compiled, "place_order", input)
	if err != nil {
		t.Fatalf("ExecuteCompiled: %v", err)
	}

	// Replay using compiled module.
	caller2 := &mockCaller{}
	engine2 := NewEngine(rt, caller2)
	result2, _, _, _, _, err := engine2.ReplayCompiled(ctx, compiled, "place_order", input, history)
	if err != nil {
		t.Fatalf("ReplayCompiled: %v", err)
	}
	if result1 != result2 {
		t.Errorf("replay result mismatch: %q vs %q", result1, result2)
	}
	if len(caller2.calls) > 0 {
		t.Errorf("replay (compiled) made %d real calls", len(caller2.calls))
	}
}

// ---------------------------------------------------------------------------
// Version / MinVersion tests
// ---------------------------------------------------------------------------

func TestVersion(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	t.Run("no workflow state", func(t *testing.T) {
		engine := NewEngine(rt, &mockCaller{})
		session := &execSession{engine: engine}
		v := session.Version(ctx)
		if v != 1 {
			t.Errorf("expected default version 1, got %d", v)
		}
		mv := session.MinVersion(ctx)
		if mv != 1 {
			t.Errorf("expected default min version 1, got %d", mv)
		}
	})

	t.Run("with workflow state", func(t *testing.T) {
		ws := &mockWorkflowState{version: 5, minVersion: 2}
		engine := NewEngine(rt, &mockCaller{}, WithWorkflowState(ws))
		session := &execSession{engine: engine}
		v := session.Version(ctx)
		if v != 5 {
			t.Errorf("expected version 5, got %d", v)
		}
		mv := session.MinVersion(ctx)
		if mv != 2 {
			t.Errorf("expected min version 2, got %d", mv)
		}
	})
}

type mockWorkflowState struct {
	version    int
	minVersion int
}

func (m *mockWorkflowState) Version() int    { return m.version }
func (m *mockWorkflowState) MinVersion() int { return m.minVersion }
func (m *mockWorkflowState) ChildVersion(name string) (int, bool) { return 0, false }

// ---------------------------------------------------------------------------
// Now / Random tests
// ---------------------------------------------------------------------------

func TestNowAndRandom(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	session := &execSession{
		engine: &Engine{caller: &mockCaller{}},
		nowMs:  999999,
	}

	n := session.Now(ctx)
	if n != 999999 {
		t.Errorf("expected Now()=999999, got %d", n)
	}

	r := session.Random(ctx)
	// Deterministic Random() uses SHA256 of fmt.Sprintf("%s:%d:%d", workflowID, stepCount, randomSeq).
	// For an empty workflowID, stepCount=0, randomSeq=0, the first 8 bytes of SHA256(":0:0") as int64.
	expected := int64(2123174926926331935)
	if r != expected {
		t.Errorf("expected Random()=%d, got %d", expected, r)
	}
}

// ---------------------------------------------------------------------------
// SetQueryState test
// ---------------------------------------------------------------------------

func TestSetQueryState(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:  &Engine{caller: &mockCaller{}},
		history: make([]EventRecord, 0),
	}

	_ = session.SetQueryState(ctx, mod, "order_id", "ord-123")
	_ = session.SetQueryState(ctx, mod, "status", "processing")

	if session.queryState == nil {
		t.Fatal("expected queryState to be non-nil")
	}
	if session.queryState["order_id"] != "ord-123" {
		t.Errorf("expected order_id=ord-123, got %q", session.queryState["order_id"])
	}
	if session.queryState["status"] != "processing" {
		t.Errorf("expected status=processing, got %q", session.queryState["status"])
	}
}

// ---------------------------------------------------------------------------
// DeferralsFromHistory test
// ---------------------------------------------------------------------------

func TestDeferralsFromHistory(t *testing.T) {
	history := []EventRecord{
		{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup db"},
		{Step: 1, EventType: EventTypeCall, Service: "s", Op: "o"},
		{Step: 2, EventType: EventTypeDefer, DeferID: "defer-1", DeferDescription: "close file"},
	}

	defs := DeferralsFromHistory(history)
	if len(defs) != 2 {
		t.Fatalf("expected 2 defers, got %d", len(defs))
	}
	if defs["defer-0"] != "cleanup db" {
		t.Errorf("expected defer-0='cleanup db', got %q", defs["defer-0"])
	}
	if defs["defer-1"] != "close file" {
		t.Errorf("expected defer-1='close file', got %q", defs["defer-1"])
	}
}

// ---------------------------------------------------------------------------
// UUID test
// ---------------------------------------------------------------------------

func TestUUID_Deterministic(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	session := &execSession{
		engine:     &Engine{caller: &mockCaller{}},
		workflowID: "wf-test",
	}
	mem := mod.Memory()

	readUUID := func() string {
		// Write the seed at a different offset so we can compare results.
		session.UUID(ctx, mod, "seed-1", 0, 4096)
		data, _ := mem.Read(0, 36) // UUIDs are 36 chars
		return string(data)
	}

	// Same seed should produce the same UUID.
	u1 := readUUID()
	u2 := readUUID()
	if u1 != u2 {
		t.Errorf("expected deterministic UUID from same seed, got %q vs %q", u1, u2)
	}

	// Different seed should produce different UUID.
	session.UUID(ctx, mod, "seed-2", 0, 4096)
	u3data, _ := mem.Read(0, 36)
	u3 := string(u3data)
	if u1 == u3 {
		t.Error("expected different UUID from different seed")
	}
}

// ---------------------------------------------------------------------------
// PollCancellation test
// ---------------------------------------------------------------------------

func TestPollCancellation_Fresh(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	t.Run("not cancelled", func(t *testing.T) {
		session := &execSession{
			engine: &Engine{
				caller:      &mockCaller{},
				signalStore: &mockSignalStore{cancelled: false},
			},
		}
		result := session.PollCancellation(ctx, mod, 0, 4096)
		if result != 0 {
			t.Errorf("expected 0 (not cancelled), got %d", result)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		session := &execSession{
			engine: &Engine{
				caller:      &mockCaller{},
				signalStore: &mockSignalStore{cancelled: true, cancelReason: "timeout"},
			},
		}
		result := session.PollCancellation(ctx, mod, 0, 4096)
		if result == 0 {
			t.Error("expected non-zero (cancelled)")
		}
	})
}

func TestPollCancellation_Replay(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)
	mod := newTestModule(t, rt)

	// During replay, PollCancellation always returns 0 (not cancelled).
	session := &execSession{
		engine: &Engine{
			caller:      &mockCaller{},
			signalStore: &mockSignalStore{cancelled: true},
		},
		isReplay: true,
	}
	result := session.PollCancellation(ctx, mod, 0, 4096)
	if result != 0 {
		t.Errorf("expected 0 during replay, got %d", result)
	}
}

// ---------------------------------------------------------------------------
// PluginStreamRegistry tests
// ---------------------------------------------------------------------------

func TestPluginStreamRegistry(t *testing.T) {
	psr := NewPluginStreamRegistry()

	// Register a streaming function.
	err := psr.Register("test-plugin", "StreamData", func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		ch := make(chan plugin.StreamEvent, 1)
		ch <- plugin.StreamEvent{Content: `{"chunk":1}`, Finish: true}
		close(ch)
		return ch, nil
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Verify it's registered.
	if !psr.Has("test-plugin", "StreamData") {
		t.Error("expected Has to return true")
	}

	// Duplicate registration should fail.
	err = psr.Register("test-plugin", "StreamData", nil)
	if err == nil {
		t.Error("expected error on duplicate registration")
	}

	// Lookup non-existent.
	_, ok := psr.Lookup("test-plugin", "NonExistent")
	if ok {
		t.Error("expected Lookup to return false for non-existent function")
	}

	// RegisterStream via plugin.StreamFuncRegistry interface.
	err = psr.RegisterStream("test-plugin", plugin.FuncOptions{Name: "StreamMore"}, func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		ch := make(chan plugin.StreamEvent, 1)
		close(ch)
		return ch, nil
	})
	if err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	if !psr.Has("test-plugin", "StreamMore") {
		t.Error("expected Has(StreamMore) to return true after RegisterStream")
	}
}

// ---------------------------------------------------------------------------
// PluginRegistry tests
// ---------------------------------------------------------------------------

func TestPluginRegistry(t *testing.T) {
	pr := NewPluginRegistry()

	// Lookup on empty registry.
	_, _, ok := pr.Lookup("p", "f")
	if ok {
		t.Error("expected Lookup to return false on empty registry")
	}

	// Register a function.
	err := pr.Register("myplugin", "DoIt", mockPluginCaller)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !pr.Has("myplugin", "DoIt") {
		t.Error("expected Has to return true")
	}

	// RegisterIdempotent.
	err = pr.RegisterIdempotent("myplugin", "ReadOnly", mockPluginCaller)
	if err != nil {
		t.Fatalf("RegisterIdempotent: %v", err)
	}
	fn, idempotent, ok := pr.Lookup("myplugin", "ReadOnly")
	if !ok {
		t.Fatal("expected Lookup to return true")
	}
	if !idempotent {
		t.Error("expected idempotent=true")
	}
	_ = fn

	// Duplicate registration should fail.
	err = pr.Register("myplugin", "DoIt", mockPluginCaller)
	if err == nil {
		t.Error("expected error on duplicate registration")
	}
}

// ---------------------------------------------------------------------------
// Engine error propagation test
// ---------------------------------------------------------------------------

func TestEngineExecute_NoWasm(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, &mockCaller{})

	// Execute with invalid WASM bytes should return an error.
	_, _, _, _, _, err = engine.Execute(ctx, []byte("not-wasm"), "place_order", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error when executing with invalid WASM")
	} else {
		t.Logf("Invalid WASM correctly rejected: %v", err)
	}
}

func TestEngineExecute_EmptyWasm(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	engine := NewEngine(rt, &mockCaller{})

	// Execute with minimal WASM (no functions) should fail when calling the
	// export.
	wasmBytes := wasmWithMemory()
	_, _, _, _, _, err = engine.Execute(ctx, wasmBytes, "nonexistent_entry", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for missing entry point")
	} else {
		t.Logf("Missing entry point correctly rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// splitSignalNames test
// ---------------------------------------------------------------------------

func TestSplitSignalNames(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"sig1", []string{"sig1"}},
		{"sig1,sig2", []string{"sig1", "sig2"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{"single,", []string{"single", ""}},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("split(%q)", tt.input), func(t *testing.T) {
			got := splitSignalNames(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("length: got %d, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// EventType constants sanity check
// ---------------------------------------------------------------------------

func TestEventTypeConstants(t *testing.T) {
	// Verify all event type constants are non-empty and unique.
	types := []EventType{
		EventTypeCall, EventTypeAwaitSignals, EventTypeSignalReceived,
		EventTypeDefer, EventTypeChildWorkflow, EventTypeAwaitChild, EventTypeContinueAsNew,
		EventTypeHeartbeat, EventTypeAwaitAllChildren, EventTypePluginCall,
		EventTypeCreatePromise, EventTypeAwaitPromise, EventTypePromiseResolved,
		EventTypePromiseRejected, EventTypeUpdateHandler, EventTypeStateMutation,
		EventTypeRunDetached, EventTypePluginCallStreamChunk, EventTypeAcquireLock,
		EventTypeReleaseLock, EventTypeSideEffect, EventTypeScopeAcquired,
	}

	seen := make(map[EventType]bool)
	for _, et := range types {
		if et == "" {
			t.Error("found empty EventType constant")
		}
		if seen[et] {
			t.Errorf("duplicate EventType: %q", et)
		}
		seen[et] = true
	}
}

// ---------------------------------------------------------------------------
// CallRecord alias compile-time check
// ---------------------------------------------------------------------------

func TestCallRecordAlias(t *testing.T) {
	// Verify that CallRecord is assignable from EventRecord.
	var cr CallRecord
	var er EventRecord
	cr = er
	er = cr
	_ = cr
	_ = er
}

// ---------------------------------------------------------------------------
// stripCompactedEvents test
// ---------------------------------------------------------------------------

func TestStripCompactedEvents(t *testing.T) {
	events := []EventRecord{
		{Step: 0, EventType: EventTypeCall, Service: "s1"},
		{Step: 1, EventType: EventTypeCall, Service: "s2"},
		{Step: 2, EventType: EventTypeCall, Service: "s3"},
	}

	// compactedStep=0 → no stripping.
	got0 := stripCompactedEvents(events, 0)
	if len(got0) != 3 {
		t.Errorf("expected 3 events with compactedStep=0, got %d", len(got0))
	}

	// compactedStep=1 → first event stripped.
	got1 := stripCompactedEvents(events, 1)
	if len(got1) != 2 {
		t.Errorf("expected 2 events with compactedStep=1, got %d", len(got1))
	}
	if got1[0].Service != "s2" {
		t.Errorf("expected first event after strip to be s2, got %s", got1[0].Service)
	}

	// compactedStep > len(events) → returns full slice.
	got3 := stripCompactedEvents(events, 10)
	if len(got3) != 3 {
		t.Errorf("expected 3 events when compactedStep > length, got %d", len(got3))
	}
}
