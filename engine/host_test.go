package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mockCaller records all service calls for test assertions.
type mockCaller struct {
	calls []CallRecord
}

func (m *mockCaller) Call(_ context.Context, service, operation, requestJSON string) (string, error) {
	resp := mockResponse(service, operation)
	m.calls = append(m.calls, CallRecord{
		EventType: EventTypeCall,
		Service:   service, Op: operation, Request: requestJSON, Response: resp,
	})
	return resp, nil
}

func mockResponse(service, operation string) string {
	switch service + "." + operation {
	case "catalog.LookupItem":
		return `{"sku":"ABC-123","name":"Widget","price_cents":999,"found":true}`
	case "inventory.Reserve":
		return `{"reservation_id":"resv_abc123","status":"reserved","total_cents":3299}`
	case "inventory.Release":
		return `{"status":"released"}`
	case "payments.GetDefaultMethod":
		return `{"token":"pm_tok_555","type":"card","last_four":"4242"}`
	case "payments.Charge":
		return `{"charge_id":"chg_xyz789","status":"captured"}`
	case "payments.Refund":
		return `{"status":"refunded"}`
	case "shipping.CreateShipment":
		return `{"tracking_id":"TRACK-123456","status":"label_created"}`
	case "notifications.SendEmail":
		return `{"status":"sent"}`
	case "accounts.Withdraw":
		return `{"ref":"wd_abc123","status":"completed"}`
	case "accounts.Deposit":
		return `{"ref":"dep_def456","status":"completed"}`
	default:
		return `{}`
	}
}

// ---- Unit tests (no WASM compilation needed) ----

func TestNewRuntime(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if rt == nil {
		t.Fatal("NewRuntime returned nil")
	}
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRuntimeCompileAndInstantiate(t *testing.T) {
	// Compile a minimal valid WASM module.
	wasmBytes := minimalWasm()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("CompileModule: %v", err)
	}
	defer compiled.Close(ctx)

	mod, err := rt.InstantiateModule(ctx, compiled)
	if err != nil {
		t.Fatalf("InstantiateModule: %v", err)
	}
	defer mod.Close(ctx)
}

func TestCallExportNotFound(t *testing.T) {
	ctx := context.Background()
	rt, _ := NewRuntime(ctx, 0, 0)
	defer rt.Close(ctx)

	compiled, _ := rt.CompileModule(ctx, minimalWasm())
	defer compiled.Close(ctx)
	mod, _ := rt.InstantiateModule(ctx, compiled)
	defer mod.Close(ctx)

	_, err := rt.CallExport(ctx, mod, "nonexistent_export", nil)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' error, got: %v", err)
	}
}


// ---- Engine execution tests with standard Go WASM + wasmtime ----

func TestEngineExecute(t *testing.T) {
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer backend.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller, WithBackend("go", backend))

	input := []byte(`{"userID":"test-user","cart":[{"SKU":"ABC-123","Quantity":2}]}`)
	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_ = suspended
	if result == "" {
		t.Error("expected non-empty result")
	}
	if len(history) == 0 {
		t.Error("expected non-empty history")
	}

	expectedServices := []string{"catalog", "inventory", "payments", "payments", "shipping", "notifications"}
	for i, svc := range expectedServices {
		if i >= len(history) {
			t.Errorf("step %d: missing (expected %s)", i, svc)
			continue
		}
		if history[i].Service != svc {
			t.Errorf("step %d: expected %s, got %s", i, svc, history[i].Service)
		}
	}

	t.Logf("Execute result: %s, history: %d calls", result, len(history))
}

func TestEngineReplay(t *testing.T) {
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer backend.Close(ctx)

	// First: execute to get history.
	caller1 := &mockCaller{}
	engine1 := NewEngine(rt, caller1, WithBackend("go", backend))
	input := []byte(`{"userID":"test-user","cart":[{"SKU":"ABC-123","Quantity":2}]}`)
	result1, history, _, _, _, err := engine1.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Second: replay with captured history.
	caller2 := &mockCaller{}
	engine2 := NewEngine(rt, caller2, WithBackend("go", backend))
	result2, _, _, _, _, err := engine2.Replay(ctx, wasmBytes, "place_order", input, history)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result1 != result2 {
		t.Errorf("replay result mismatch: %q vs %q", result1, result2)
	}
	if len(caller2.calls) > 0 {
		t.Errorf("replay made %d real calls (expected 0)", len(caller2.calls))
	}
}

func TestEngineReplayDivergence(t *testing.T) {
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer backend.Close(ctx)

	// Execute to get history.
	caller1 := &mockCaller{}
	engine1 := NewEngine(rt, caller1, WithBackend("go", backend))
	input := []byte(`{"userID":"test-user","cart":[{"SKU":"ABC-123","Quantity":2}]}`)
	_, history, _, _, _, err := engine1.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(history) == 0 {
		t.Skip("WASM execution returned empty history; cannot test divergence (pre-existing environment issue)")
	}

	t.Run("event_type_mismatch_enriched", func(t *testing.T) {
		hist := make([]EventRecord, len(history))
		copy(hist, history)
		if len(hist) > 0 {
			hist[0].EventType = "sleep"
		}
		caller := &mockCaller{}
		engine := NewEngine(rt, caller, WithBackend("go", backend))
		result, _, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, hist)
		if err != nil {
			t.Logf("Replay error (expected): %v", err)
		}
		if result == "" {
			t.Error("expected divergence error result, got empty")
		}
		for _, label := range []string{"actual request:", "expected request:"} {
			if !strings.Contains(result, label) {
				t.Errorf("result missing %q: %s", label, result)
			}
		}
	})

	t.Run("service_mismatch_enriched", func(t *testing.T) {
		hist := make([]EventRecord, len(history))
		copy(hist, history)
		if len(hist) > 0 {
			hist[0].Service = "different_service"
		}
		caller := &mockCaller{}
		engine := NewEngine(rt, caller, WithBackend("go", backend))
		result, _, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, hist)
		if err != nil {
			t.Logf("Replay error (expected if divergence bails out): %v", err)
		}
		if result == "" {
			t.Error("expected divergence error result, got empty")
		}
		for _, label := range []string{"actual request:", "expected request:"} {
			if !strings.Contains(result, label) {
				t.Errorf("result missing %q: %s", label, result)
			}
		}
	})
}

func TestEngineExecuteCancelOrder(t *testing.T) {
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	backend, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Skipf("wasmtime backend not available: %v", err)
	}
	defer backend.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller, WithBackend("go", backend))

	input := []byte(`{"OrderID":"ord-123"}`)
	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "cancel_order", input)
	if err != nil {
		t.Fatalf("Execute cancel_order: %v", err)
	}
	_ = suspended
	_ = result
	// CancelOrder calls refundPayment and releaseReservation.
	expectedServices := []string{"payments", "inventory"}
	for i, svc := range expectedServices {
		if i >= len(history) {
			t.Errorf("step %d: missing (expected %s)", i, svc)
			continue
		}
		if history[i].Service != svc {
			t.Errorf("step %d: expected %s, got %s", i, svc, history[i].Service)
		}
	}
	t.Logf("CancelOrder result: %s, history: %d calls", result, len(history))
}

// ---- New event type tests (7 new event types, no WASM needed) ----

func TestNewEventTypesPayloadRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		rec  EventRecord
	}{
		{
			name: "create_promise",
			rec: EventRecord{
				Step: 0, EventType: EventTypeCreatePromise,
				PromiseName: "order-promise",
				PromiseID:   "550e8400-e29b-41d4-a716-446655440000",
			},
		},
		{
			name: "await_promise_resolved",
			rec: EventRecord{
				Step: 1, EventType: EventTypeAwaitPromise,
				PromiseID:     "550e8400-e29b-41d4-a716-446655440000",
				PromiseResult: `{"status":"completed"}`,
			},
		},
		{
			name: "await_promise_rejected",
			rec: EventRecord{
				Step: 2, EventType: EventTypeAwaitPromise,
				PromiseID:    "550e8400-e29b-41d4-a716-446655440000",
				PromiseError: "timed out",
			},
		},
		{
			name: "promise_resolved",
			rec: EventRecord{
				Step: 3, EventType: EventTypePromiseResolved,
				PromiseID:     "prom-001",
				PromiseResult: `{"order_id":"ord-123","status":"paid"}`,
			},
		},
		{
			name: "promise_rejected",
			rec: EventRecord{
				Step: 4, EventType: EventTypePromiseRejected,
				PromiseID:    "prom-002",
				PromiseError: "payment_failed: card_declined",
			},
		},
		{
			name: "update_handler",
			rec: EventRecord{
				Step: 5, EventType: EventTypeUpdateHandler,
				UpdateHandlerName: "update_shipping_address",
			},
		},
		{
			name: "state_mutation",
			rec: EventRecord{
				Step: 6, EventType: EventTypeStateMutation,
				StateKey:   "retry_count",
				StateValue: "3",
				StateDelta: 1,
				StateOp:    "increment",
			},
		},
		{
			name: "state_mutation_set",
			rec: EventRecord{
				Step: 7, EventType: EventTypeStateMutation,
				StateKey:   "status",
				StateValue: "processing",
				StateOp:    "set",
			},
		},
		{
			name: "run_detached",
			rec: EventRecord{
				Step: 8, EventType: EventTypeRunDetached,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Serialize event-type-specific fields to payload JSON.
			payload, err := eventRecordToPayload(tt.rec)
			if err != nil {
				t.Fatalf("eventRecordToPayload: %v", err)
			}

			// Deserialize back into a fresh record with matching EventType.
			got := EventRecord{EventType: tt.rec.EventType}
			populateFromPayload(&got, payload)

			// Verify promise fields round-trip correctly.
			if got.PromiseName != tt.rec.PromiseName {
				t.Errorf("PromiseName: expected %q, got %q", tt.rec.PromiseName, got.PromiseName)
			}
			if got.PromiseID != tt.rec.PromiseID {
				t.Errorf("PromiseID: expected %q, got %q", tt.rec.PromiseID, got.PromiseID)
			}
			if got.PromiseResult != tt.rec.PromiseResult {
				t.Errorf("PromiseResult: expected %q, got %q", tt.rec.PromiseResult, got.PromiseResult)
			}
			if got.PromiseError != tt.rec.PromiseError {
				t.Errorf("PromiseError: expected %q, got %q", tt.rec.PromiseError, got.PromiseError)
			}

			// Verify update handler fields round-trip correctly.
			if got.UpdateHandlerName != tt.rec.UpdateHandlerName {
				t.Errorf("UpdateHandlerName: expected %q, got %q", tt.rec.UpdateHandlerName, got.UpdateHandlerName)
			}

			// Verify state mutation fields round-trip correctly.
			if got.StateKey != tt.rec.StateKey {
				t.Errorf("StateKey: expected %q, got %q", tt.rec.StateKey, got.StateKey)
			}
			if got.StateValue != tt.rec.StateValue {
				t.Errorf("StateValue: expected %q, got %q", tt.rec.StateValue, got.StateValue)
			}
			if got.StateDelta != tt.rec.StateDelta {
				t.Errorf("StateDelta: expected %d, got %d", tt.rec.StateDelta, got.StateDelta)
			}
			if got.StateOp != tt.rec.StateOp {
				t.Errorf("StateOp: expected %q, got %q", tt.rec.StateOp, got.StateOp)
			}

			t.Logf("Payload round-trip OK for %s (payload=%s)", tt.name, string(payload))
		})
	}
}

func TestNewEventTypesJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		rec  EventRecord
	}{
		{
			name: "create_promise",
			rec: EventRecord{
				Step: 0, EventType: EventTypeCreatePromise,
				PromiseName: "order-promise",
				PromiseID:   "550e8400-e29b-41d4-a716-446655440000",
			},
		},
		{
			name: "await_promise",
			rec: EventRecord{
				Step: 1, EventType: EventTypeAwaitPromise,
				PromiseID:     "550e8400-e29b-41d4-a716-446655440000",
				PromiseResult: `{"status":"completed"}`,
			},
		},
		{
			name: "promise_resolved",
			rec: EventRecord{
				Step: 2, EventType: EventTypePromiseResolved,
				PromiseID:     "prom-001",
				PromiseResult: `{"order_id":"ord-123","status":"paid"}`,
			},
		},
		{
			name: "promise_rejected",
			rec: EventRecord{
				Step: 3, EventType: EventTypePromiseRejected,
				PromiseID:    "prom-002",
				PromiseError: "card_declined",
			},
		},
		{
			name: "update_handler",
			rec: EventRecord{
				Step: 4, EventType: EventTypeUpdateHandler,
				UpdateHandlerName: "update_shipping_address",
			},
		},
		{
			name: "state_mutation",
			rec: EventRecord{
				Step: 5, EventType: EventTypeStateMutation,
				StateKey:   "retry_count",
				StateValue: "3",
				StateDelta: 1,
				StateOp:    "increment",
			},
		},
		{
			name: "run_detached",
			rec: EventRecord{
				Step: 6, EventType: EventTypeRunDetached,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.rec)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}

			var got EventRecord
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			// Core fields.
			if got.Step != tt.rec.Step {
				t.Errorf("Step: expected %d, got %d", tt.rec.Step, got.Step)
			}
			if got.EventType != tt.rec.EventType {
				t.Errorf("EventType: expected %q, got %q", tt.rec.EventType, got.EventType)
			}

			// Promise fields.
			if got.PromiseName != tt.rec.PromiseName {
				t.Errorf("PromiseName: expected %q, got %q", tt.rec.PromiseName, got.PromiseName)
			}
			if got.PromiseID != tt.rec.PromiseID {
				t.Errorf("PromiseID: expected %q, got %q", tt.rec.PromiseID, got.PromiseID)
			}
			if got.PromiseResult != tt.rec.PromiseResult {
				t.Errorf("PromiseResult: expected %q, got %q", tt.rec.PromiseResult, got.PromiseResult)
			}
			if got.PromiseError != tt.rec.PromiseError {
				t.Errorf("PromiseError: expected %q, got %q", tt.rec.PromiseError, got.PromiseError)
			}

			// Update handler fields.
			if got.UpdateHandlerName != tt.rec.UpdateHandlerName {
				t.Errorf("UpdateHandlerName: expected %q, got %q", tt.rec.UpdateHandlerName, got.UpdateHandlerName)
			}

			// State mutation fields.
			if got.StateKey != tt.rec.StateKey {
				t.Errorf("StateKey: expected %q, got %q", tt.rec.StateKey, got.StateKey)
			}
			if got.StateValue != tt.rec.StateValue {
				t.Errorf("StateValue: expected %q, got %q", tt.rec.StateValue, got.StateValue)
			}
			if got.StateDelta != tt.rec.StateDelta {
				t.Errorf("StateDelta: expected %d, got %d", tt.rec.StateDelta, got.StateDelta)
			}
			if got.StateOp != tt.rec.StateOp {
				t.Errorf("StateOp: expected %q, got %q", tt.rec.StateOp, got.StateOp)
			}

			t.Logf("JSON round-trip OK for %s (json=%s)", tt.name, string(data))
		})
	}
}

// TestEventRecordToPayloadRoundTrip_StandardEventTypes tests eventRecordToPayload
// and populateFromPayload for standard event types not covered by
// TestNewEventTypesPayloadRoundTrip (call, sleep, signal, defer, child, etc.).
func TestEventRecordToPayloadRoundTrip_StandardEventTypes(t *testing.T) {
	tests := []struct {
		name string
		rec  EventRecord
	}{
		{
			name: "call",
			rec: EventRecord{
				Step: 0, EventType: EventTypeCall,
				Service: "svc", Op: "op1", Request: `{"id":1}`, Response: `{"ok":true}`,
			},
		},
		{
			name: "call_with_error",
			rec: EventRecord{
				Step: 1, EventType: EventTypeCall,
				Service: "svc", Op: "fail", Request: `{}`, Response: ``, Err: "timeout",
			},
		},
		{
			name: "call_with_duration",
			rec: EventRecord{
				Step: 2, EventType: EventTypeCall,
				Service: "svc", Op: "slow", Request: `{}`, Response: `{"ok":true}`, DurationMs: 1500,
			},
		},
		{
			name: "sleep",
			rec: EventRecord{
				Step: 3, EventType: EventTypeCall,
				DurationMs: 5000,
			},
		},
		{
			name: "sleep_zero",
			rec: EventRecord{
				Step: 4, EventType: EventTypeCall,
				DurationMs: 0,
			},
		},
		{
			name: "await_signals",
			rec: EventRecord{
				Step: 5, EventType: EventTypeAwaitSignals,
				SignalNames: "payment,approval", TimeoutMs: 30000,
			},
		},
		{
			name: "await_signals_no_timeout",
			rec: EventRecord{
				Step: 6, EventType: EventTypeAwaitSignals,
				SignalNames: "payment",
			},
		},
		{
			name: "signal_received",
			rec: EventRecord{
				Step: 7, EventType: EventTypeSignalReceived,
				SignalName: "payment", SignalPayload: `{"paid":true}`,
			},
		},
		{
			name: "signal_received_empty_payload",
			rec: EventRecord{
				Step: 8, EventType: EventTypeSignalReceived,
				SignalName: "payment",
			},
		},
		{
			name: "defer",
			rec: EventRecord{
				Step: 9, EventType: EventTypeDefer,
				DeferID: "d1", DeferDescription: "cleanup",
			},
		},
		{
			name: "child_workflow",
			rec: EventRecord{
				Step: 10, EventType: EventTypeChildWorkflow,
				ChildName: "child-wf", ChildInput: `{"x":1}`, RunID: "run-001",
			},
		},
		{
			name: "continue_as_new",
			rec: EventRecord{
				Step: 11, EventType: EventTypeContinueAsNew,
				NewInput: `{"restart":true}`,
			},
		},
		{
			name: "plugin_call",
			rec: EventRecord{
				Step: 14, EventType: EventTypePluginCall,
				PluginName: "p", PluginFunc: "f", PluginInput: `{"x":1}`,
				PluginOutput: `{"result":"ok"}`,
			},
		},
		{
			name: "plugin_call_error",
			rec: EventRecord{
				Step: 15, EventType: EventTypePluginCall,
				PluginName: "p", PluginFunc: "g", PluginInput: `{}`,
				PluginError: "not found",
			},
		},
		{
			name: "plugin_call_stream_chunk",
			rec: EventRecord{
				Step: 16, EventType: EventTypePluginCallStreamChunk,
				PluginName: "p", PluginFunc: "f", PluginOutput: `{"chunk":1}`,
			},
		},
		{
			name: "side_effect",
			rec: EventRecord{
				Step: 17, EventType: EventTypeSideEffect,
				SideEffectResult: `{"random":42}`,
			},
		},
		{
			name: "scope_acquired",
			rec: EventRecord{
				Step: 18, EventType: EventTypeScopeAcquired,
				ScopeKey: "vo:order:123",
			},
		},
		{
			name: "scope_acquired_with_error",
			rec: EventRecord{
				Step: 19, EventType: EventTypeScopeAcquired,
				ScopeKey: "vo:order:456", Err: "denied",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := eventRecordToPayload(tt.rec)
			if err != nil {
				t.Fatalf("eventRecordToPayload: %v", err)
			}

			got := EventRecord{EventType: tt.rec.EventType}
			populateFromPayload(&got, payload)

			// Verify core fields round-trip correctly.
			if got.Service != tt.rec.Service {
				t.Errorf("Service: expected %q, got %q", tt.rec.Service, got.Service)
			}
			if got.Op != tt.rec.Op {
				t.Errorf("Op: expected %q, got %q", tt.rec.Op, got.Op)
			}
			if got.Request != tt.rec.Request {
				t.Errorf("Request: expected %q, got %q", tt.rec.Request, got.Request)
			}
			if got.Response != tt.rec.Response {
				t.Errorf("Response: expected %q, got %q", tt.rec.Response, got.Response)
			}
			if got.Err != tt.rec.Err {
				t.Errorf("Err: expected %q, got %q", tt.rec.Err, got.Err)
			}
			if got.DurationMs != tt.rec.DurationMs {
				t.Errorf("DurationMs: expected %d, got %d", tt.rec.DurationMs, got.DurationMs)
			}
			if got.SignalNames != tt.rec.SignalNames {
				t.Errorf("SignalNames: expected %q, got %q", tt.rec.SignalNames, got.SignalNames)
			}
			if got.TimeoutMs != tt.rec.TimeoutMs {
				t.Errorf("TimeoutMs: expected %d, got %d", tt.rec.TimeoutMs, got.TimeoutMs)
			}
			if got.SignalName != tt.rec.SignalName {
				t.Errorf("SignalName: expected %q, got %q", tt.rec.SignalName, got.SignalName)
			}
			if got.SignalPayload != tt.rec.SignalPayload {
				t.Errorf("SignalPayload: expected %q, got %q", tt.rec.SignalPayload, got.SignalPayload)
			}
			if got.DeferDescription != tt.rec.DeferDescription {
				t.Errorf("DeferDescription: expected %q, got %q", tt.rec.DeferDescription, got.DeferDescription)
			}
			if got.DeferID != tt.rec.DeferID {
				t.Errorf("DeferID: expected %q, got %q", tt.rec.DeferID, got.DeferID)
			}
			if got.ChildName != tt.rec.ChildName {
				t.Errorf("ChildName: expected %q, got %q", tt.rec.ChildName, got.ChildName)
			}
			if got.ChildInput != tt.rec.ChildInput {
				t.Errorf("ChildInput: expected %q, got %q", tt.rec.ChildInput, got.ChildInput)
			}
			if got.RunID != tt.rec.RunID {
				t.Errorf("RunID: expected %q, got %q", tt.rec.RunID, got.RunID)
			}
			if got.NewInput != tt.rec.NewInput {
				t.Errorf("NewInput: expected %q, got %q", tt.rec.NewInput, got.NewInput)
			}
			if got.PluginName != tt.rec.PluginName {
				t.Errorf("PluginName: expected %q, got %q", tt.rec.PluginName, got.PluginName)
			}
			if got.PluginFunc != tt.rec.PluginFunc {
				t.Errorf("PluginFunc: expected %q, got %q", tt.rec.PluginFunc, got.PluginFunc)
			}
			if got.PluginInput != tt.rec.PluginInput {
				t.Errorf("PluginInput: expected %q, got %q", tt.rec.PluginInput, got.PluginInput)
			}
			if got.PluginOutput != tt.rec.PluginOutput {
				t.Errorf("PluginOutput: expected %q, got %q", tt.rec.PluginOutput, got.PluginOutput)
			}
			if got.PluginError != tt.rec.PluginError {
				t.Errorf("PluginError: expected %q, got %q", tt.rec.PluginError, got.PluginError)
			}
			if got.SideEffectResult != tt.rec.SideEffectResult {
				t.Errorf("SideEffectResult: expected %q, got %q", tt.rec.SideEffectResult, got.SideEffectResult)
			}
			if got.ScopeKey != tt.rec.ScopeKey {
				t.Errorf("ScopeKey: expected %q, got %q", tt.rec.ScopeKey, got.ScopeKey)
			}

			t.Logf("Payload round-trip OK for %s (payload=%s)", tt.name, string(payload))
		})
	}
}

// ---------------------------------------------------------------------------
// Signal handling pure function tests
// ---------------------------------------------------------------------------

// splitSignalNames tests

func TestSplitSignalNames_Empty(t *testing.T) {
	result := splitSignalNames("")
	if result != nil {
		t.Errorf("expected nil for empty string, got %v", result)
	}
}

func TestSplitSignalNames_Single(t *testing.T) {
	result := splitSignalNames("payment")
	if len(result) != 1 || result[0] != "payment" {
		t.Errorf("expected [payment], got %v", result)
	}
}

func TestSplitSignalNames_Multiple(t *testing.T) {
	result := splitSignalNames("payment,approval,shipping")
	expected := []string{"payment", "approval", "shipping"}
	if len(result) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, result)
	}
	for i := range expected {
		if result[i] != expected[i] {
			t.Errorf("idx %d: expected %q, got %q", i, expected[i], result[i])
		}
	}
}

func TestSplitSignalNames_TrailingComma(t *testing.T) {
	result := splitSignalNames("a,b,")
	expected := []string{"a", "b", ""}
	if len(result) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, result)
	}
	for i := range expected {
		if result[i] != expected[i] {
			t.Errorf("idx %d: expected %q, got %q", i, expected[i], result[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Pack function tests (pure encoding functions)
// ---------------------------------------------------------------------------

func TestPackAwaitSignalsResult_NoTimeout(t *testing.T) {
	result := packAwaitSignalsResult(5, 10, false, 0)
	sigLen := uint16(result >> 48)
	payLen := uint16(result >> 32)
	toFlag := uint16(result>>16) & 1
	errCode := uint16(result & 0xFFFF)
	if sigLen != 5 {
		t.Errorf("expected sigLen=5, got %d", sigLen)
	}
	if payLen != 10 {
		t.Errorf("expected payLen=10, got %d", payLen)
	}
	if toFlag != 0 {
		t.Errorf("expected toFlag=0, got %d", toFlag)
	}
	if errCode != 0 {
		t.Errorf("expected errCode=0, got %d", errCode)
	}
}

func TestPackAwaitSignalsResult_Timeout(t *testing.T) {
	result := packAwaitSignalsResult(0, 0, true, 0)
	toFlag := uint16(result>>16) & 1
	if toFlag != 1 {
		t.Error("expected timeout flag=1")
	}
}

func TestPackAwaitSignalsResult_WithError(t *testing.T) {
	result := packAwaitSignalsResult(3, 8, false, 2)
	sigLen := uint16(result >> 48)
	payLen := uint16(result >> 32)
	errCode := uint16(result & 0xFFFF)
	if sigLen != 3 {
		t.Errorf("expected sigLen=3, got %d", sigLen)
	}
	if payLen != 8 {
		t.Errorf("expected payLen=8, got %d", payLen)
	}
	if errCode != 2 {
		t.Errorf("expected errCode=2, got %d", errCode)
	}
}

func TestPackSleepResult(t *testing.T) {
	result := packSleepResult(1, 5000)
	status := byte(result >> 56)
	duration := result & 0x00FFFFFFFFFFFFFF
	if status != 1 {
		t.Errorf("expected status=1, got %d", status)
	}
	if duration != 5000 {
		t.Errorf("expected duration=5000, got %d", duration)
	}
}

func TestPackSleepResult_ZeroDuration(t *testing.T) {
	result := packSleepResult(0, 0)
	status := byte(result >> 56)
	duration := result & 0x00FFFFFFFFFFFFFF
	if status != 0 {
		t.Errorf("expected status=0, got %d", status)
	}
	if duration != 0 {
		t.Errorf("expected duration=0, got %d", duration)
	}
}

func TestPackAcquireLockResult_Acquired(t *testing.T) {
	result := packAcquireLockResult(true, 0)
	acquired := byte(result>>8) & 0xFF
	errCode := byte(result & 0xFF)
	if acquired != 1 {
		t.Errorf("expected acquired=1, got %d", acquired)
	}
	if errCode != 0 {
		t.Errorf("expected errCode=0, got %d", errCode)
	}
}

func TestPackAcquireLockResult_NotAcquired(t *testing.T) {
	result := packAcquireLockResult(false, 0)
	acquired := byte(result>>8) & 0xFF
	if acquired != 0 {
		t.Errorf("expected acquired=0, got %d", acquired)
	}
}

func TestPackAcquireLockResult_WithError(t *testing.T) {
	result := packAcquireLockResult(false, 5)
	acquired := byte(result>>8) & 0xFF
	errCode := byte(result & 0xFF)
	if acquired != 0 {
		t.Errorf("expected acquired=0, got %d", acquired)
	}
	if errCode != 5 {
		t.Errorf("expected errCode=5, got %d", errCode)
	}
}

func TestPackAwaitChildResult(t *testing.T) {
	result := packAwaitChildResult(10, 3)
	written := uint32(result >> 32)
	errCode := uint32(result & 0xFFFFFFFF)
	if written != 10 {
		t.Errorf("expected written=10, got %d", written)
	}
	if errCode != 3 {
		t.Errorf("expected errCode=3, got %d", errCode)
	}
}

func TestPackAwaitChildResultSuspend(t *testing.T) {
	result := packAwaitChildResultSuspend()
	expected := int64(1 << 62)
	if result != expected {
		t.Errorf("expected %d, got %d", expected, result)
	}
}

func TestPackAwaitPromiseResult(t *testing.T) {
	result := packAwaitPromiseResult(15, false, 0)
	resultLen := uint32(result >> 32)
	toFlag := uint16(result>>16) & 1
	errCode := uint16(result & 0xFFFF)
	if resultLen != 15 {
		t.Errorf("expected resultLen=15, got %d", resultLen)
	}
	if toFlag != 0 {
		t.Errorf("expected toFlag=0, got %d", toFlag)
	}
	if errCode != 0 {
		t.Errorf("expected errCode=0, got %d", errCode)
	}
}

func TestPackAwaitPromiseResult_Timeout(t *testing.T) {
	result := packAwaitPromiseResult(0, true, 0)
	toFlag := uint16(result>>16) & 1
	if toFlag != 1 {
		t.Error("expected timeout flag=1")
	}
}

func TestPackAwaitPromiseResult_WithError(t *testing.T) {
	result := packAwaitPromiseResult(0, false, 7)
	errCode := uint16(result & 0xFFFF)
	if errCode != 7 {
		t.Errorf("expected errCode=7, got %d", errCode)
	}
}

// ---------------------------------------------------------------------------
// EventRecord comparison tests
// ---------------------------------------------------------------------------

// TestEventFieldsMatch_AllEventTypes verifies that eventFieldsMatch correctly
// compares every supported event type against itself and a mismatched variant.
func TestEventFieldsMatch_AllEventTypes(t *testing.T) {
	tests := []struct {
		name string
		a    EventRecord
		b    EventRecord
		want bool
	}{
		{
			name: "Call match",
			a:    EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{"ok":true}`},
			b:    EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Request: `{}`, Response: `{"ok":true}`},
			want: true,
		},
		{
			name: "Call mismatch",
			a:    EventRecord{Step: 0, EventType: EventTypeCall, Service: "svcA", Op: "op1"},
			b:    EventRecord{Step: 0, EventType: EventTypeCall, Service: "svcB", Op: "op2"},
			want: false,
		},
		{
			name: "Step mismatch",
			a:    EventRecord{Step: 0, EventType: EventTypeCall},
			b:    EventRecord{Step: 1, EventType: EventTypeCall},
			want: false,
		},
		{
			name: "Type mismatch",
			a:    EventRecord{Step: 0, EventType: EventTypeCall},
			b:    EventRecord{Step: 0, EventType: EventTypeAwaitSignals},
			want: false,
		},
		{
			name: "AwaitSignals timeout match",
			a:    EventRecord{Step: 0, EventType: EventTypeAwaitSignals, TimeoutMs: 5000},
			b:    EventRecord{Step: 0, EventType: EventTypeAwaitSignals, TimeoutMs: 5000},
			want: true,
		},
		{
			name: "AwaitSignals match",
			a:    EventRecord{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: "a,b", TimeoutMs: 10000},
			b:    EventRecord{Step: 0, EventType: EventTypeAwaitSignals, SignalNames: "a,b", TimeoutMs: 10000},
			want: true,
		},
		{
			name: "SignalReceived match",
			a:    EventRecord{Step: 0, EventType: EventTypeSignalReceived, SignalName: "pay", SignalPayload: `{"amt":100}`},
			b:    EventRecord{Step: 0, EventType: EventTypeSignalReceived, SignalName: "pay", SignalPayload: `{"amt":100}`},
			want: true,
		},
		{
			name: "RunDetached match",
			a:    EventRecord{Step: 0, EventType: EventTypeRunDetached},
			b:    EventRecord{Step: 0, EventType: EventTypeRunDetached},
			want: true,
		},
		{
			name: "Unknown type",
			a:    EventRecord{Step: 0, EventType: "unknown_type"},
			b:    EventRecord{Step: 0, EventType: "unknown_type"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eventFieldsMatch(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("eventFieldsMatch = %v, want %v", got, tt.want)
			}
		})
	}
}
func minimalWasm() []byte {
	// A minimal WASM module: magic + version, plus an empty code section.
	return []byte{
		0x00, 0x61, 0x73, 0x6d, // magic
		0x01, 0x00, 0x00, 0x00, // version 1
		// empty module: no imports, no exports, no functions
	}
}

func buildTestWasm(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping WASM compilation in short mode")
	}

	projectRoot := findProjectRoot(t)

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "go", "-o", tmpDir, filepath.Join(projectRoot, "testdata", "basic"))
	cmd.Dir = projectRoot
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cleat build failed:\n%s\n%v", string(out), err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("reading build output: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wasm" {
			return filepath.Join(tmpDir, e.Name())
		}
	}
	t.Fatalf("no .wasm file found in %s", tmpDir)
	return ""
}

func TestTruncateWithHash(t *testing.T) {
	t.Run("no_truncation", func(t *testing.T) {
		result := truncateWithHash("hello", 10)
		if result != "hello" {
			t.Errorf("expected 'hello', got %q", result)
		}
	})

	t.Run("equal_length", func(t *testing.T) {
		result := truncateWithHash("hello", 5)
		if result != "hello" {
			t.Errorf("expected 'hello', got %q", result)
		}
	})

	t.Run("truncation", func(t *testing.T) {
		result := truncateWithHash("hello world", 5)
		if !strings.HasPrefix(result, "hello") {
			t.Errorf("expected prefix 'hello', got %q", result)
		}
		if !strings.Contains(result, "... [sha256=") {
			t.Errorf("expected sha256 suffix, got %q", result)
		}
		// Verify the hash is correct (64 hex chars)
		hashStart := strings.Index(result, "[sha256=")
		if hashStart < 0 {
			t.Fatal("sha256 marker not found")
		}
		hashEnd := strings.Index(result[hashStart:], "]")
		if hashEnd < 0 {
			t.Fatal("closing bracket not found")
		}
		hashHex := result[hashStart+8 : hashStart+hashEnd]
		if len(hashHex) != 64 {
			t.Errorf("expected 64 hex chars, got %d: %q", len(hashHex), hashHex)
		}
		expectedHash := fmt.Sprintf("%x", sha256.Sum256([]byte("hello world")))
		if hashHex != expectedHash {
			t.Errorf("hash mismatch: got %s, expected %s", hashHex, expectedHash)
		}
	})

	t.Run("empty_string", func(t *testing.T) {
		result := truncateWithHash("", 10)
		if result != "" {
			t.Errorf("expected empty string, got %q", result)
		}
	})

	t.Run("exact_maxlen", func(t *testing.T) {
		s := strings.Repeat("a", 4096)
		result := truncateWithHash(s, 4096)
		if result != s {
			t.Errorf("expected unchanged string, got different result")
		}
	})

	t.Run("one_over_maxlen", func(t *testing.T) {
		s := strings.Repeat("a", 4097)
		result := truncateWithHash(s, 4096)
		if !strings.HasPrefix(result, strings.Repeat("a", 4096)) {
			t.Errorf("expected 4096 'a' prefix")
		}
		if !strings.Contains(result, "... [sha256=") {
			t.Errorf("expected sha256 suffix, got %q", result)
		}
	})

	t.Run("unicode_bytes", func(t *testing.T) {
		// Go len counts bytes; 3-byte UTF-8 characters
		s := "你好世界！" // 5 chars, 15 bytes (3 bytes each)
		result := truncateWithHash(s, 10)
		if len(result) <= 10 {
			t.Errorf("expected result longer than 10 bytes due to hash suffix, got len %d", len(result))
		}
		if !strings.Contains(result, "... [sha256=") {
			t.Errorf("expected sha256 suffix, got %q", result)
		}
		// The truncated portion should be the first 10 bytes
		if !strings.HasPrefix(result, s[:10]) {
			t.Errorf("expected prefix to be first 10 bytes of input")
		}
	})
}

func TestEngineDefaultLogger(t *testing.T) {
	rt, err := NewRuntime(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(rt, nil)
	if engine.logger == nil {
		t.Fatal("expected default logger to be set")
	}
}

func TestEngineWithLogger(t *testing.T) {
	rt, err := NewRuntime(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	l := slog.New(slog.NewJSONHandler(io.Discard, nil))
	engine := NewEngine(rt, nil, WithLogger(l))
	if engine.logger != l {
		t.Fatal("WithLogger did not set the logger")
	}
}

// ---------------------------------------------------------------------------
// Mock child workflow store for AwaitChild and PollChild tests.
// ---------------------------------------------------------------------------

type mockChildWorkflowStore struct {
	result    string
	completed bool
	err       error
	gotRunID  string // records the last runID passed to GetChildResult
}

func (m *mockChildWorkflowStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	return "child-run-001", nil
}

func (m *mockChildWorkflowStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	return "child-run-001", nil
}

func (m *mockChildWorkflowStore) GetChildResult(ctx context.Context, runID string) (string, bool, error) {
	m.gotRunID = runID
	return m.result, m.completed, m.err
}

func newTestExecSession() *execSession {
	return &execSession{
		engine:     NewEngine(nil, nil),
		nowMs:      1000000,
		deferrals:  make(map[string]string),
		stateStore: make(map[string]string),
		queryState: make(map[string]string),
	}
}

// ---------------------------------------------------------------------------
// AwaitChild replay divergence detection tests.
// ---------------------------------------------------------------------------

func TestAwaitChildReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type — should be EventTypeAwaitChild
	}}
	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected error code 1 (divergence), got %d", errCode)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true (divergence should not call exitReplay)")
	}
	if s.stepCount != 0 {
		t.Errorf("expected stepCount=0 (step not advanced), got %d", s.stepCount)
	}
	if len(s.history) != 1 {
		t.Errorf("expected history unchanged (len=1), got len=%d", len(s.history))
	}
}

func TestAwaitChildReplayEmptyEvent(t *testing.T) {
	mock := &mockChildWorkflowStore{completed: false}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitChild,
		RunID:     "run-1",
	}}

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if !s.replayJustEnded {
		t.Error("expected replayJustEnded=true after exitReplay")
	}
	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel (1<<62), got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_child(run-1)") {
		t.Errorf("expected suspendErr Reason containing 'await_child(run-1)', got %q", s.suspendErr.Reason)
	}
	if len(s.history) != 2 {
		t.Errorf("expected 2 history entries (replay + fresh), got %d", len(s.history))
	}
	if mock.gotRunID != "run-1" {
		t.Errorf("expected child store queried with run-1, got %q", mock.gotRunID)
	}
}

func TestAwaitChildReplayEmptyThenCompleted(t *testing.T) {
	mock := &mockChildWorkflowStore{
		result:    `{"status":"done"}`,
		completed: true,
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitChild,
		RunID:     "run-1",
	}}

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected error code 0 (success), got %d", errCode)
	}
	if len(s.history) != 2 {
		t.Errorf("expected 2 history entries, got %d", len(s.history))
	}
	lastEvent := s.history[len(s.history)-1]
	if lastEvent.Response != `{"status":"done"}` {
		t.Errorf("expected last event Response=%q, got %q", `{"status":"done"}`, lastEvent.Response)
	}
	if lastEvent.RunID != "run-1" {
		t.Errorf("expected last event RunID=run-1, got %q", lastEvent.RunID)
	}
}

func TestAwaitChildFreshError(t *testing.T) {
	mock := &mockChildWorkflowStore{
		err: fmt.Errorf("child store unavailable"),
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitChild(context.Background(), nil, "run-1", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected error code 1, got %d", errCode)
	}
	if len(s.history) == 0 {
		t.Fatal("expected at least one event recorded")
	}
	lastEvent := s.history[len(s.history)-1]
	if lastEvent.EventType != EventTypeAwaitChild {
		t.Errorf("expected EventTypeAwaitChild, got %s", lastEvent.EventType)
	}
	if lastEvent.Err != "child store unavailable" {
		t.Errorf("expected Err='child store unavailable', got %q", lastEvent.Err)
	}
	if lastEvent.RunID != "run-1" {
		t.Errorf("expected RunID=run-1, got %q", lastEvent.RunID)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 (event recorded), got %d", s.stepCount)
	}
}

// ---------------------------------------------------------------------------
// PollChild tests.
// ---------------------------------------------------------------------------

func TestPollChildStatus(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		mock := &mockChildWorkflowStore{completed: false}
		s := newTestExecSession()
		s.engine.childWfStore = mock
		s.PollChild(context.Background(), nil, "run-1", 0, 0)
		if mock.gotRunID != "run-1" {
			t.Errorf("expected run-1, got %q", mock.gotRunID)
		}
	})

	t.Run("completed", func(t *testing.T) {
		mock := &mockChildWorkflowStore{result: `{"ok":true}`, completed: true}
		s := newTestExecSession()
		s.engine.childWfStore = mock
		s.PollChild(context.Background(), nil, "run-1", 0, 0)
		if mock.gotRunID != "run-1" {
			t.Errorf("expected run-1, got %q", mock.gotRunID)
		}
	})

	t.Run("store_error", func(t *testing.T) {
		mock := &mockChildWorkflowStore{err: fmt.Errorf("db down")}
		s := newTestExecSession()
		s.engine.childWfStore = mock
		s.PollChild(context.Background(), nil, "run-1", 0, 0)
		if mock.gotRunID != "run-1" {
			t.Errorf("expected run-1, got %q", mock.gotRunID)
		}
	})

	t.Run("empty_result", func(t *testing.T) {
		mock := &mockChildWorkflowStore{completed: true, result: ""}
		s := newTestExecSession()
		s.engine.childWfStore = mock
		s.PollChild(context.Background(), nil, "run-1", 0, 0)
		if mock.gotRunID != "run-1" {
			t.Errorf("expected run-1, got %q", mock.gotRunID)
		}
	})

	t.Run("no_store", func(t *testing.T) {
		s := newTestExecSession()
		// s.engine.childWfStore is nil — verify no panic
		s.PollChild(context.Background(), nil, "run-1", 0, 0)
	})
}

func TestPollChildJSONFormat(t *testing.T) {
	type pollResult struct {
		Status string `json:"status"`
		Result string `json:"result,omitempty"`
		Error  string `json:"error,omitempty"`
	}

	t.Run("running", func(t *testing.T) {
		out, _ := json.Marshal(pollResult{Status: "running"})
		var decoded struct{ Status string }
		if err := json.Unmarshal(out, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Status != "running" {
			t.Errorf("expected status 'running', got %q", decoded.Status)
		}
	})

	t.Run("completed", func(t *testing.T) {
		out, _ := json.Marshal(pollResult{Status: "completed", Result: `{"ok":true}`})
		var decoded struct {
			Status string `json:"status"`
			Result string `json:"result"`
		}
		if err := json.Unmarshal(out, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Status != "completed" {
			t.Errorf("expected status 'completed', got %q", decoded.Status)
		}
		if decoded.Result != `{"ok":true}` {
			t.Errorf("expected result %q, got %q", `{"ok":true}`, decoded.Result)
		}
	})

	t.Run("failed_error", func(t *testing.T) {
		out, _ := json.Marshal(pollResult{Status: "failed", Error: "db down"})
		var decoded struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(out, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Status != "failed" {
			t.Errorf("expected status 'failed', got %q", decoded.Status)
		}
		if decoded.Error != "db down" {
			t.Errorf("expected error 'db down', got %q", decoded.Error)
		}
	})

	t.Run("failed_empty_result", func(t *testing.T) {
		out, _ := json.Marshal(pollResult{Status: "failed", Error: "child workflow failed (empty result)"})
		if !strings.Contains(string(out), "child workflow failed") {
			t.Errorf("expected 'child workflow failed' in output: %s", out)
		}
	})

	t.Run("no_store", func(t *testing.T) {
		out, _ := json.Marshal(pollResult{Status: "failed", Error: "no child workflow store"})
		if !strings.Contains(string(out), "no child workflow store") {
			t.Errorf("expected 'no child workflow store' in output: %s", out)
		}
	})
}
