package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cleat-team/cleat/plugin"
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

// ErrWasmtimeCGOUnavailable (backend_wasmtime_errors.go) is the sentinel
// backend_wasmtime_stub.go (build tag //go:build !cgo) returns. Unlike
// backend_wasmtime_test.go, this file carries no build tag, so it compiles
// both with and without CGO, and NewWasmtimeBackend can legitimately resolve
// to that stub -- a genuine "nobody asked for CGO" case, not a defect. But
// ci.yml's test-go/engine entry and its cluster-tests job both run with CGO
// on by default, where NewWasmtimeBackend resolves to the real
// backend_wasmtime.go implementation (the primary backend per CLAUDE.md);
// any other error there means wasmtime itself failed to initialise, which is
// a real defect masquerading as an absent optional resource. Checking
// errors.Is against the sentinel -- rather than guessing from a CGO_ENABLED
// env var, which reflects what the compiler saw and not necessarily the
// running binary, or matching the error's string, which is an implementation
// detail -- lets each site tell the two cases apart precisely and is the
// same check cmd/cleat-worker/main.go uses at startup.

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
		if errors.Is(err, ErrWasmtimeCGOUnavailable) {
			t.Skip("wasmtime backend requires CGO and this build has it disabled; falling back to wazero-only coverage")
		}
		t.Fatalf("wasmtime backend not available (CGO is enabled in this build, so this is a real init failure, not an absent optional resource): %v", err)
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
		if errors.Is(err, ErrWasmtimeCGOUnavailable) {
			t.Skip("wasmtime backend requires CGO and this build has it disabled; falling back to wazero-only coverage")
		}
		t.Fatalf("wasmtime backend not available (CGO is enabled in this build, so this is a real init failure, not an absent optional resource): %v", err)
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
		if errors.Is(err, ErrWasmtimeCGOUnavailable) {
			t.Skip("wasmtime backend requires CGO and this build has it disabled; falling back to wazero-only coverage")
		}
		t.Fatalf("wasmtime backend not available (CGO is enabled in this build, so this is a real init failure, not an absent optional resource): %v", err)
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

	// These read the enriched divergence detail off the *error*, not off the
	// result. They used to read it off the result, and tolerated an error with
	// a t.Logf("expected if divergence bails out") -- because a Go guest that
	// returned an error was handed back as a success whose result happened to
	// contain the message. IMPROVEMENT-PLAN 3.22 fixed that, so a divergence is
	// now the failure it always described itself as. The substance is
	// unchanged: the same two labels, in the same message, still have to reach
	// whoever is debugging the workflow.
	for _, tc := range []struct {
		name   string
		mutate func(*EventRecord)
	}{
		{"event_type_mismatch_enriched", func(r *EventRecord) { r.EventType = "sleep" }},
		{"service_mismatch_enriched", func(r *EventRecord) { r.Service = "different_service" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hist := make([]EventRecord, len(history))
			copy(hist, history)
			tc.mutate(&hist[0])

			caller := &mockCaller{}
			engine := NewEngine(rt, caller, WithBackend("go", backend))
			result, _, _, _, _, err := engine.Replay(ctx, wasmBytes, "place_order", input, hist)
			if err == nil {
				t.Fatalf("Replay of a diverging history succeeded, result = %q; a divergence is "+
					"a bug in the workflow code and running the same call again reproduces it", result)
			}
			for _, label := range []string{"actual request:", "expected request:"} {
				if !strings.Contains(err.Error(), label) {
					t.Errorf("divergence error missing %q: %v", label, err)
				}
			}
			if result != "" {
				t.Errorf("Replay returned both an error and the result %q", result)
			}
		})
	}
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
		if errors.Is(err, ErrWasmtimeCGOUnavailable) {
			t.Skip("wasmtime backend requires CGO and this build has it disabled; falling back to wazero-only coverage")
		}
		t.Fatalf("wasmtime backend not available (CGO is enabled in this build, so this is a real init failure, not an absent optional resource): %v", err)
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

// withWasmtimeBackend creates a wasmtime backend and returns an EngineOption
// that registers it for the "go" language. When wasmtime is not available
// (CGO disabled or libwasmtime.so not found), the test is skipped.
//
// This is used by integration tests that need the wasmtime backend to properly
// execute Go wasip1 modules, avoiding the wazero Runtime's stub cleat_poll_work
// which always returns 0 (causing "wasm trap: exit(code=0)").
func withWasmtimeBackend(t *testing.T) EngineOption {
	t.Helper()
	ctx := context.Background()
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		if errors.Is(err, ErrWasmtimeCGOUnavailable) {
			t.Skip("wasmtime backend requires CGO and this build has it disabled; falling back to wazero-only coverage")
		}
		t.Fatalf("wasmtime backend not available (CGO is enabled in this build, so this is a real init failure, not an absent optional resource): %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	return WithBackend("go", wt)
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
	return buildFixtureWasm(t, "basic")
}

// buildFixtureWasm compiles testdata/<fixture> to WASM with `cleat build` and
// returns the path of the resulting module.
func buildFixtureWasm(t *testing.T, fixture string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping WASM compilation in short mode")
	}

	cwd, _ := os.Getwd()
	// Resolve symlinks so Go toolchain module resolution works correctly.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	projectRoot := cwd
	if strings.HasSuffix(cwd, "/engine") {
		projectRoot = filepath.Dir(cwd)
	} else if strings.HasSuffix(cwd, "internal/host") {
		projectRoot = filepath.Dir(filepath.Dir(cwd))
	}

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "cleat"),
		"build", "--target", "go", "-o", tmpDir, filepath.Join(projectRoot, "testdata", fixture))
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

func (m *mockChildWorkflowStore) ResolveVersionByTag(ctx context.Context, workflowName string, tag string) (int, error) {
	return 0, nil
}

// ---------------------------------------------------------------------------
// Mock promise store for CreatePromise and AwaitPromise tests.
// ---------------------------------------------------------------------------

type mockPromiseStore struct {
	createErr error  // error to return from CreatePromise
	status    string // "resolved", "rejected", or "" (pending)
	result    string // promise result (for resolved)
	errMsg    string // error message (for rejected)
	getErr    error  // error to return from GetPromise
	// tracking
	lastCreatedWorkflowID  string
	lastCreatedPromiseName string
	lastCreatedPromiseID   string
	lastGetWorkflowID      string
	lastGetPromiseID       string
}

func (m *mockPromiseStore) CreatePromise(ctx context.Context, workflowID, promiseName, promiseID string) error {
	m.lastCreatedWorkflowID = workflowID
	m.lastCreatedPromiseName = promiseName
	m.lastCreatedPromiseID = promiseID
	return m.createErr
}

func (m *mockPromiseStore) ResolvePromise(ctx context.Context, workflowID, promiseID, result string) error {
	return nil
}

func (m *mockPromiseStore) RejectPromise(ctx context.Context, workflowID, promiseID, errMsg string) error {
	return nil
}

func (m *mockPromiseStore) GetPromise(ctx context.Context, workflowID, promiseID string) (string, string, string, error) {
	m.lastGetWorkflowID = workflowID
	m.lastGetPromiseID = promiseID
	if m.getErr != nil {
		return "", "", "", m.getErr
	}
	return m.status, m.result, m.errMsg, nil
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
	if s.isReplay {
		t.Error("expected replay to have ended")
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

// ---------------------------------------------------------------------------
// CreatePromise tests.
// ---------------------------------------------------------------------------

func TestCreatePromiseReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeCreatePromise,
		PromiseID: "abc-123",
	}}
	result := s.CreatePromise(context.Background(), nil, "my-promise", 0, 0)

	// packSimpleResult(0, 0) = 0
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 after advanceReplayStep, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true after replay match")
	}
}

func TestCreatePromiseReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type
	}}
	result := s.CreatePromise(context.Background(), nil, "my-promise", 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	// Fresh path runs after exitReplay, recording a create_promise event.
	if len(s.history) < 2 {
		t.Fatalf("expected at least 2 history entries (original + fresh event), got %d", len(s.history))
	}
	if s.history[1].EventType != EventTypeCreatePromise {
		t.Errorf("expected fresh event type create_promise, got %q", s.history[1].EventType)
	}
	if s.history[1].PromiseID == "" {
		t.Error("expected non-empty PromiseID in fresh event")
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestCreatePromiseReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // stepCount(0) >= len(history)(0) -> past end of history

	result := s.CreatePromise(context.Background(), nil, "my-promise", 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay (past end of history)")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	// Fresh path runs, recording a create_promise event.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry (fresh event), got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeCreatePromise {
		t.Errorf("expected EventTypeCreatePromise, got %q", s.history[0].EventType)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestCreatePromiseFresh(t *testing.T) {
	s := newTestExecSession()

	result := s.CreatePromise(context.Background(), nil, "my-promise", 0, 0)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeCreatePromise {
		t.Errorf("expected EventTypeCreatePromise, got %q", s.history[0].EventType)
	}
	if s.history[0].PromiseName != "my-promise" {
		t.Errorf("expected PromiseName 'my-promise', got %q", s.history[0].PromiseName)
	}
	if s.history[0].PromiseID == "" {
		t.Error("expected non-empty PromiseID (UUID generated)")
	}
}

func TestCreatePromiseFreshWithStore(t *testing.T) {
	mock := &mockPromiseStore{}
	s := newTestExecSession()
	s.engine.promiseStore = mock

	s.CreatePromise(context.Background(), nil, "my-promise", 0, 0)

	if mock.lastCreatedPromiseName != "my-promise" {
		t.Errorf("expected CreatePromise called with name 'my-promise', got %q", mock.lastCreatedPromiseName)
	}
	if mock.lastCreatedPromiseID == "" {
		t.Error("expected non-empty promiseID passed to CreatePromise")
	}
}

func TestCreatePromiseFreshStoreError(t *testing.T) {
	mock := &mockPromiseStore{createErr: fmt.Errorf("db down")}
	s := newTestExecSession()
	s.engine.promiseStore = mock

	result := s.CreatePromise(context.Background(), nil, "my-promise", 0, 0)

	// Store error is logged, not surfaced. Function should still succeed.
	if result != 0 {
		t.Errorf("expected 0 (error is logged, not surfaced), got %d", result)
	}
	if mock.lastCreatedPromiseName != "my-promise" {
		t.Error("expected CreatePromise called despite previous errors")
	}
}

// ---------------------------------------------------------------------------
// AwaitPromise tests.
// ---------------------------------------------------------------------------

func TestAwaitPromiseReplayResolved(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:          0,
		EventType:     EventTypePromiseResolved,
		PromiseID:     "abc-123",
		PromiseResult: `{"status":"ok"}`,
	}}
	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	// packAwaitPromiseResult(resultLen=0, timedOut=false, errCode=0)
	expected := packAwaitPromiseResult(0, false, 0)
	if result != expected {
		t.Errorf("expected %d, got %d", expected, result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestAwaitPromiseReplayRejected(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:         0,
		EventType:    EventTypePromiseRejected,
		PromiseID:    "abc-123",
		PromiseError: "bad request",
	}}
	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	// packAwaitPromiseResult(resultLen=0, timedOut=false, errCode=1)
	expected := packAwaitPromiseResult(0, false, 1)
	if result != expected {
		t.Errorf("expected %d, got %d", expected, result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestAwaitPromiseReplayAwaitThenFreshNoStore(t *testing.T) {
	s := newTestExecSession()
	s.engine.promiseStore = nil
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitPromise,
		PromiseID: "abc-123",
	}}
	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay from await_promise event")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	// Fresh path with no promiseStore -> suspend.
	expected := packAwaitPromiseResult(0, true, 0)
	if result != expected {
		t.Errorf("expected %d (suspend), got %d", expected, result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_promise(abc-123)") {
		t.Errorf("expected suspendErr reason containing 'await_promise(abc-123)', got %q", s.suspendErr.Reason)
	}
}

func TestAwaitPromiseReplayAwaitThenFreshResolved(t *testing.T) {
	mock := &mockPromiseStore{
		status: "resolved",
		result: `{"status":"ok"}`,
	}
	s := newTestExecSession()
	s.engine.promiseStore = mock
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitPromise,
		PromiseID: "abc-123",
	}}
	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	// Fresh path finds resolved promise.
	expected := packAwaitPromiseResult(0, false, 0)
	if result != expected {
		t.Errorf("expected %d (resolved), got %d", expected, result)
	}
	if mock.lastGetPromiseID != "abc-123" {
		t.Errorf("expected GetPromise called with 'abc-123', got %q", mock.lastGetPromiseID)
	}
}

func TestAwaitPromiseReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type - not a promise event
	}}

	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	// Divergence: exitReplay is NOT called (unlike CreatePromise).
	// isReplay stays true, fresh path runs which records await_promise and suspends.
	if !s.isReplay {
		t.Error("expected isReplay to remain true (exitReplay not called on mismatch)")
	}
	// Fresh path suspends since promiseStore is nil.
	expected := packAwaitPromiseResult(0, true, 0)
	if result != expected {
		t.Errorf("expected %d (suspend), got %d", expected, result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_promise(abc-123)") {
		t.Errorf("expected suspendErr reason containing 'await_promise(abc-123)', got %q", s.suspendErr.Reason)
	}
}

func TestAwaitPromiseReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // stepCount(0) >= len(history)(0)

	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay (past end of history)")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	// Fresh path suspends (no store).
	expected := packAwaitPromiseResult(0, true, 0)
	if result != expected {
		t.Errorf("expected %d (suspend), got %d", expected, result)
	}
}

func TestAwaitPromiseFreshResolved(t *testing.T) {
	mock := &mockPromiseStore{
		status: "resolved",
		result: `{"status":"ok"}`,
	}
	s := newTestExecSession()
	s.engine.promiseStore = mock

	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	expected := packAwaitPromiseResult(0, false, 0)
	if result != expected {
		t.Errorf("expected %d (resolved), got %d", expected, result)
	}
	if mock.lastGetPromiseID != "abc-123" {
		t.Errorf("expected GetPromise called with 'abc-123', got %q", mock.lastGetPromiseID)
	}
	// Should have recorded a promise_resolved event.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypePromiseResolved {
		t.Errorf("expected EventTypePromiseResolved, got %q", s.history[0].EventType)
	}
}

func TestAwaitPromiseFreshRejected(t *testing.T) {
	mock := &mockPromiseStore{
		status: "rejected",
		errMsg: "bad request",
	}
	s := newTestExecSession()
	s.engine.promiseStore = mock

	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	expected := packAwaitPromiseResult(0, false, 1)
	if result != expected {
		t.Errorf("expected %d (rejected), got %d", expected, result)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypePromiseRejected {
		t.Errorf("expected EventTypePromiseRejected, got %q", s.history[0].EventType)
	}
}

func TestAwaitPromiseFreshPending(t *testing.T) {
	mock := &mockPromiseStore{
		status: "", // pending - status not "resolved" and not "rejected"
	}
	s := newTestExecSession()
	s.engine.promiseStore = mock

	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	// Pending -> suspends.
	expected := packAwaitPromiseResult(0, true, 0)
	if result != expected {
		t.Errorf("expected %d (suspend), got %d", expected, result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_promise(abc-123)") {
		t.Errorf("expected suspendErr reason containing 'await_promise(abc-123)', got %q", s.suspendErr.Reason)
	}
	// Should have recorded an await_promise event.
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeAwaitPromise {
		t.Errorf("expected EventTypeAwaitPromise, got %q", s.history[0].EventType)
	}
}

func TestAwaitPromiseFreshStoreError(t *testing.T) {
	mock := &mockPromiseStore{
		getErr: fmt.Errorf("db down"),
	}
	s := newTestExecSession()
	s.engine.promiseStore = mock

	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	// Store error -> treated as pending, suspends.
	expected := packAwaitPromiseResult(0, true, 0)
	if result != expected {
		t.Errorf("expected %d (suspend), got %d", expected, result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
}

func TestAwaitPromiseFreshNilStore(t *testing.T) {
	s := newTestExecSession()
	s.engine.promiseStore = nil

	result := s.AwaitPromise(context.Background(), nil, "abc-123", 5000, 0, 0)

	// Nil store -> suspends immediately.
	expected := packAwaitPromiseResult(0, true, 0)
	if result != expected {
		t.Errorf("expected %d (suspend), got %d", expected, result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	// Verify timeout encoding: nowMs + timeoutMs
	expectedUntil := time.UnixMilli(s.nowMs).Add(time.Duration(5000) * time.Millisecond)
	if !s.suspendErr.Until.Equal(expectedUntil) {
		t.Errorf("expected Until=%v, got %v", expectedUntil, s.suspendErr.Until)
	}
}

// ---------------------------------------------------------------------------
// PollCancellation host function tests
// ---------------------------------------------------------------------------

// mockCancellationStore implements SignalStore with configurable
// PollCancellation return values.
type mockCancellationStore struct {
	cancelled bool
	reason    string
	err       error

	mu                sync.Mutex
	polledWorkflowIDs []string // captures every workflowID PollCancellation was called with
}

func (m *mockCancellationStore) PollCancellation(_ context.Context, workflowID string) (bool, string, error) {
	m.mu.Lock()
	m.polledWorkflowIDs = append(m.polledWorkflowIDs, workflowID)
	m.mu.Unlock()
	return m.cancelled, m.reason, m.err
}

// lastPolledWorkflowID returns the workflowID passed on the most recent
// PollCancellation call, or "" if it was never called.
func (m *mockCancellationStore) lastPolledWorkflowID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.polledWorkflowIDs) == 0 {
		return ""
	}
	return m.polledWorkflowIDs[len(m.polledWorkflowIDs)-1]
}

func (m *mockCancellationStore) DeliverSignal(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockCancellationStore) PollSignal(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}

func TestPollCancellationReplay(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true

	result := s.PollCancellation(context.Background(), nil, 0, 0)
	if result != 0 {
		t.Errorf("expected 0 in replay mode, got %d", result)
	}
}

func TestPollCancellationNoStore(t *testing.T) {
	s := newTestExecSession()

	result := s.PollCancellation(context.Background(), nil, 0, 0)
	if result != 0 {
		t.Errorf("expected 0 with no store, got %d", result)
	}
}

func TestPollCancellationNotCancelled(t *testing.T) {
	s := newTestExecSession()
	s.engine.workflowID = "wf-not-cancelled"
	store := &mockCancellationStore{cancelled: false}
	s.engine.signalStore = store

	result := s.PollCancellation(context.Background(), nil, 0, 0)
	if result != 0 {
		t.Errorf("expected 0 when not cancelled, got %d", result)
	}
	if got := store.lastPolledWorkflowID(); got != "wf-not-cancelled" {
		t.Errorf("expected PollCancellation to be called with workflowID %q, got %q", "wf-not-cancelled", got)
	}
}

func TestPollCancellationWithReason(t *testing.T) {
	s := newTestExecSession()
	s.engine.workflowID = "wf-with-reason"
	store := &mockCancellationStore{
		cancelled: true,
		reason:    "testing",
	}
	s.engine.signalStore = store

	buf := make([]byte, 256)
	ctx := contextWithRawMemBuf(context.Background(), buf)

	result := s.PollCancellation(ctx, nil, 0, 100)

	expected := int64(uint64(7)<<32 | 1) // len("testing")=7, cancelled=1
	if result != expected {
		t.Errorf("expected %d, got %d", expected, result)
	}

	written := string(buf[:7])
	if written != "testing" {
		t.Errorf("expected 'testing' in buffer, got %q", written)
	}
	if got := store.lastPolledWorkflowID(); got != "wf-with-reason" {
		t.Errorf("expected PollCancellation to be called with workflowID %q, got %q", "wf-with-reason", got)
	}
}

func TestPollCancellationEmptyReason(t *testing.T) {
	s := newTestExecSession()
	s.engine.workflowID = "wf-empty-reason"
	store := &mockCancellationStore{
		cancelled: true,
		reason:    "",
	}
	s.engine.signalStore = store

	result := s.PollCancellation(context.Background(), nil, 0, 0)

	expected := int64(1) // 0<<32 | 1
	if result != expected {
		t.Errorf("expected %d, got %d", expected, result)
	}
	if got := store.lastPolledWorkflowID(); got != "wf-empty-reason" {
		t.Errorf("expected PollCancellation to be called with workflowID %q, got %q", "wf-empty-reason", got)
	}
}

func TestPollCancellationStoreError(t *testing.T) {
	s := newTestExecSession()
	s.engine.workflowID = "wf-store-error"
	store := &mockCancellationStore{
		err: fmt.Errorf("db down"),
	}
	s.engine.signalStore = store

	result := s.PollCancellation(context.Background(), nil, 0, 0)
	if result != 0 {
		t.Errorf("expected 0 on store error, got %d", result)
	}
	if got := store.lastPolledWorkflowID(); got != "wf-store-error" {
		t.Errorf("expected PollCancellation to be called with workflowID %q, got %q", "wf-store-error", got)
	}
}

// ---------------------------------------------------------------------------
// ContinueAsNew / ContinueAsNewWithVersion tests.
// ---------------------------------------------------------------------------

func TestContinueAsNewReplayCachedResult(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeContinueAsNew,
		NewInput:  `{"v":1}`,
	}}

	result := s.ContinueAsNew(context.Background(), nil, `{"v":2}`)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if s.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", s.suspendErr.Reason)
	}
	if s.suspendErr.NewInput != `{"v":1}` {
		t.Errorf("expected NewInput from cached event '{\"v\":1}', got %q", s.suspendErr.NewInput)
	}
}

func TestContinueAsNewReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type — should be EventTypeContinueAsNew
	}}

	result := s.ContinueAsNew(context.Background(), nil, `{"v":1}`)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 after recordEvent, got %d", s.stepCount)
	}
	if len(s.history) != 2 {
		t.Fatalf("expected history len=2 (original + new ContinueAsNew), got %d", len(s.history))
	}
	last := s.history[len(s.history)-1]
	if last.EventType != EventTypeContinueAsNew {
		t.Errorf("expected EventTypeContinueAsNew, got %s", last.EventType)
	}
	if last.NewInput != `{"v":1}` {
		t.Errorf("expected NewInput='{\"v\":1}', got %q", last.NewInput)
	}
}

func TestContinueAsNewReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	// history empty — stepCount=0 >= len(history)=0

	result := s.ContinueAsNew(context.Background(), nil, `{"v":1}`)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 after recordEvent, got %d", s.stepCount)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected history len=1, got %d", len(s.history))
	}
	last := s.history[len(s.history)-1]
	if last.EventType != EventTypeContinueAsNew {
		t.Errorf("expected EventTypeContinueAsNew, got %s", last.EventType)
	}
	if last.NewInput != `{"v":1}` {
		t.Errorf("expected NewInput='{\"v\":1}', got %q", last.NewInput)
	}
}

func TestContinueAsNewFresh(t *testing.T) {
	s := newTestExecSession()
	// isReplay=false by default

	result := s.ContinueAsNew(context.Background(), nil, `{"v":1}`)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if s.isReplay {
		t.Error("expected isReplay to remain false")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected history len=1, got %d", len(s.history))
	}
	last := s.history[len(s.history)-1]
	if last.EventType != EventTypeContinueAsNew {
		t.Errorf("expected EventTypeContinueAsNew, got %s", last.EventType)
	}
	if last.NewInput != `{"v":1}` {
		t.Errorf("expected NewInput='{\"v\":1}', got %q", last.NewInput)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if s.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", s.suspendErr.Reason)
	}
	if s.suspendErr.NewInput != `{"v":1}` {
		t.Errorf("expected NewInput='{\"v\":1}', got %q", s.suspendErr.NewInput)
	}
}

func TestContinueAsNewFreshEmptyInput(t *testing.T) {
	s := newTestExecSession()

	result := s.ContinueAsNew(context.Background(), nil, "")

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if s.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", s.suspendErr.Reason)
	}
	if s.suspendErr.NewInput != "" {
		t.Errorf("expected empty NewInput, got %q", s.suspendErr.NewInput)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected history len=1, got %d", len(s.history))
	}
	last := s.history[len(s.history)-1]
	if last.NewInput != "" {
		t.Errorf("expected empty NewInput in event, got %q", last.NewInput)
	}
}

func TestContinueAsNewWithVersionReplayCachedResult(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:       0,
		EventType:  EventTypeContinueAsNew,
		NewInput:   `{"v":1}`,
		NewVersion: 3,
	}}

	result := s.ContinueAsNewWithVersion(context.Background(), nil, `{"v":2}`, 5)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if s.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", s.suspendErr.Reason)
	}
	if s.suspendErr.NewInput != `{"v":1}` {
		t.Errorf("expected NewInput from cached event '{\"v\":1}', got %q", s.suspendErr.NewInput)
	}
	if s.suspendErr.NewVersion != 3 {
		t.Errorf("expected NewVersion=3 from cached event, got %d", s.suspendErr.NewVersion)
	}
}

func TestContinueAsNewWithVersionReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type
	}}

	result := s.ContinueAsNewWithVersion(context.Background(), nil, `{"v":1}`, 2)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 after recordEvent, got %d", s.stepCount)
	}
	if len(s.history) != 2 {
		t.Fatalf("expected history len=2 (original + new ContinueAsNew), got %d", len(s.history))
	}
	last := s.history[len(s.history)-1]
	if last.EventType != EventTypeContinueAsNew {
		t.Errorf("expected EventTypeContinueAsNew, got %s", last.EventType)
	}
	if last.NewVersion != 2 {
		t.Errorf("expected NewVersion=2 in event, got %d", last.NewVersion)
	}
}

func TestContinueAsNewWithVersionReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	// history empty

	result := s.ContinueAsNewWithVersion(context.Background(), nil, `{"v":1}`, 2)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 after recordEvent, got %d", s.stepCount)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected history len=1, got %d", len(s.history))
	}
	last := s.history[len(s.history)-1]
	if last.EventType != EventTypeContinueAsNew {
		t.Errorf("expected EventTypeContinueAsNew, got %s", last.EventType)
	}
	if last.NewVersion != 2 {
		t.Errorf("expected NewVersion=2 in event, got %d", last.NewVersion)
	}
}

func TestContinueAsNewWithVersionFresh(t *testing.T) {
	s := newTestExecSession()

	result := s.ContinueAsNewWithVersion(context.Background(), nil, `{"v":1}`, 2)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected history len=1, got %d", len(s.history))
	}
	last := s.history[len(s.history)-1]
	if last.EventType != EventTypeContinueAsNew {
		t.Errorf("expected EventTypeContinueAsNew, got %s", last.EventType)
	}
	if last.NewInput != `{"v":1}` {
		t.Errorf("expected NewInput='{\"v\":1}', got %q", last.NewInput)
	}
	if last.NewVersion != 2 {
		t.Errorf("expected NewVersion=2, got %d", last.NewVersion)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if s.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", s.suspendErr.Reason)
	}
	if s.suspendErr.NewInput != `{"v":1}` {
		t.Errorf("expected NewInput='{\"v\":1}', got %q", s.suspendErr.NewInput)
	}
	if s.suspendErr.NewVersion != 2 {
		t.Errorf("expected NewVersion=2, got %d", s.suspendErr.NewVersion)
	}
}

func TestContinueAsNewWithVersionFreshZero(t *testing.T) {
	s := newTestExecSession()

	result := s.ContinueAsNewWithVersion(context.Background(), nil, `{"v":1}`, 0)

	if result != 0 {
		t.Errorf("expected result 0, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr to be set")
	}
	if s.suspendErr.Reason != "continue_as_new" {
		t.Errorf("expected reason 'continue_as_new', got %q", s.suspendErr.Reason)
	}
	if s.suspendErr.NewVersion != 0 {
		t.Errorf("expected NewVersion=0, got %d", s.suspendErr.NewVersion)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected history len=1, got %d", len(s.history))
	}
	last := s.history[len(s.history)-1]
	if last.NewVersion != 0 {
		t.Errorf("expected NewVersion=0 in event, got %d", last.NewVersion)
	}
}

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// SignalWorkflow tests.
// ---------------------------------------------------------------------------

type mockSignalStore struct {
	deliverErr         error
	lastDeliverWFID    string
	lastDeliverName    string
	lastDeliverPayload string
}

func (m *mockSignalStore) DeliverSignal(_ context.Context, workflowID, signalName, payload string) error {
	m.lastDeliverWFID = workflowID
	m.lastDeliverName = signalName
	m.lastDeliverPayload = payload
	return m.deliverErr
}

func (m *mockSignalStore) PollSignal(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}

func (m *mockSignalStore) PollCancellation(_ context.Context, _ string) (bool, string, error) {
	return false, "", nil
}

func TestSignalWorkflowReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:          0,
		EventType:     EventTypeSignalReceived,
		RunID:         "target-run-123",
		SignalName:    "my-signal",
		SignalPayload: `{"x":1}`,
	}}
	result := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestSignalWorkflowReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type
	}}
	result := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	// Fresh path records EventTypeSignalReceived.
	if len(s.history) < 2 {
		t.Fatalf("expected at least 2 history entries, got %d", len(s.history))
	}
	if s.history[1].EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", s.history[1].EventType)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestSignalWorkflowReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // stepCount(0) >= len(0) → past end

	result := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", s.history[0].EventType)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestSignalWorkflowFresh(t *testing.T) {
	s := newTestExecSession()

	result := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	r := s.history[0]
	if r.EventType != EventTypeSignalReceived {
		t.Errorf("expected EventTypeSignalReceived, got %q", r.EventType)
	}
	if r.RunID != "target-run-123" {
		t.Errorf("expected RunID 'target-run-123', got %q", r.RunID)
	}
	if r.SignalName != "my-signal" {
		t.Errorf("expected SignalName 'my-signal', got %q", r.SignalName)
	}
	if r.SignalPayload != `{"x":1}` {
		t.Errorf("expected SignalPayload '{\"x\":1}', got %q", r.SignalPayload)
	}
}

func TestSignalWorkflowFreshWithStore(t *testing.T) {
	mock := &mockSignalStore{}
	s := newTestExecSession()
	s.engine.signalStore = mock

	s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)

	if mock.lastDeliverWFID != "target-run-123" {
		t.Errorf("expected DeliverSignal WFID 'target-run-123', got %q", mock.lastDeliverWFID)
	}
	if mock.lastDeliverName != "my-signal" {
		t.Errorf("expected signal name 'my-signal', got %q", mock.lastDeliverName)
	}
	if mock.lastDeliverPayload != `{"x":1}` {
		t.Errorf("expected payload '{\"x\":1}', got %q", mock.lastDeliverPayload)
	}
}

func TestSignalWorkflowStoreError(t *testing.T) {
	mock := &mockSignalStore{deliverErr: fmt.Errorf("db down")}
	s := newTestExecSession()
	s.engine.signalStore = mock

	result := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)

	// Store error is logged, not surfaced.
	if result != 0 {
		t.Errorf("expected 0 (error logged, not surfaced), got %d", result)
	}
	// Event still recorded.
	if len(s.history) != 1 || s.history[0].EventType != EventTypeSignalReceived {
		t.Error("expected event recorded despite store error")
	}
}

func TestSignalWorkflowAuthDenied(t *testing.T) {
	s := newTestExecSession()
	s.engine.requireSignalAuth = true
	s.engine.signalAuthCheck = func(_ context.Context, targetWorkflowID, callerDefName string) error {
		return fmt.Errorf("not authorized")
	}

	result := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)

	if result != errSignalAuthRequiredInt {
		t.Errorf("expected errSignalAuthRequiredInt (%d), got %d", errSignalAuthRequiredInt, result)
	}
	// No event should be recorded on auth failure.
	if len(s.history) != 0 {
		t.Errorf("expected 0 history entries on auth failure, got %d", len(s.history))
	}
}

func TestSignalWorkflowAuthAllowed(t *testing.T) {
	s := newTestExecSession()
	s.engine.requireSignalAuth = true
	s.engine.signalAuthCheck = func(_ context.Context, targetWorkflowID, callerDefName string) error {
		return nil
	}

	result := s.SignalWorkflow(context.Background(), nil, "target-run-123", "my-signal", `{"x":1}`)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 || s.history[0].EventType != EventTypeSignalReceived {
		t.Error("expected event recorded when auth allowed")
	}
}

// ---------------------------------------------------------------------------
// DurableScheduleInvoke tests.
// ---------------------------------------------------------------------------

func TestScheduleInvokeReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:       0,
		EventType:  EventTypeDurableScheduleInvoke,
		Service:    "my-svc",
		Op:         "my-op",
		Request:    `{}`,
		DurationMs: 5000,
	}}
	result := s.DurableScheduleInvoke(context.Background(), nil, "my-svc", "my-op", `{}`, 5000)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestScheduleInvokeReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // stepCount >= len → past end

	result := s.DurableScheduleInvoke(context.Background(), nil, "my-svc", "my-op", `{}`, 5000)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true (ScheduleInvoke does not exitReplay on past-end)")
	}
}

func TestScheduleInvokeFresh(t *testing.T) {
	s := newTestExecSession()

	result := s.DurableScheduleInvoke(context.Background(), nil, "my-svc", "my-op", `{"k":"v"}`, 10000)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	r := s.history[0]
	if r.EventType != EventTypeDurableScheduleInvoke {
		t.Errorf("expected EventTypeDurableScheduleInvoke, got %q", r.EventType)
	}
	if r.Service != "my-svc" {
		t.Errorf("expected Service 'my-svc', got %q", r.Service)
	}
	if r.Op != "my-op" {
		t.Errorf("expected Op 'my-op', got %q", r.Op)
	}
	if r.Request != `{"k":"v"}` {
		t.Errorf("expected Request '{\"k\":\"v\"}', got %q", r.Request)
	}
	if r.DurationMs != 10000 {
		t.Errorf("expected DurationMs=10000, got %d", r.DurationMs)
	}
}

func TestScheduleInvokeFreshNoCaller(t *testing.T) {
	// caller is nil → event recorded, no goroutine spawned.
	s := newTestExecSession()
	s.engine.caller = nil // already nil from newTestExecSession

	result := s.DurableScheduleInvoke(context.Background(), nil, "svc", "op", `{}`, 0)

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if len(s.history) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(s.history))
	}
}

// ---------------------------------------------------------------------------
// RegisterUpdateHandler tests.
// ---------------------------------------------------------------------------

func TestRegisterUpdateHandlerReplayMatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:              0,
		EventType:         EventTypeUpdateHandler,
		UpdateHandlerName: "my-handler",
	}}
	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

func TestRegisterUpdateHandlerReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type
	}}
	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if len(s.history) < 2 {
		t.Fatalf("expected at least 2 history entries, got %d", len(s.history))
	}
	if s.history[1].EventType != EventTypeUpdateHandler {
		t.Errorf("expected EventTypeUpdateHandler, got %q", s.history[1].EventType)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestRegisterUpdateHandlerReplayPastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = nil // past end

	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeUpdateHandler {
		t.Errorf("expected EventTypeUpdateHandler, got %q", s.history[0].EventType)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

// AwaitAnyChild tests.
// ---------------------------------------------------------------------------

func TestAwaitAnyChildReplayDivergence(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type — should be EventTypeAwaitAnyChild
	}}
	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

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

func TestAwaitAnyChildReplayCachedResult(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitAnyChild,
		Response:  `{"run_id":"run-1","result":"done"}`,
	}}
	result := s.AwaitAnyChild(context.Background(), nil, `["run-1","run-2"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected error code 0, got %d", errCode)
	}
	if !s.isReplay {
		t.Error("expected isReplay=true (cached result)")
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
}

func TestAwaitAnyChildReplaySuspendThenCompleted(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{
		{Step: 0, EventType: EventTypeAwaitAnyChild},
		{Step: 1, EventType: EventTypeAwaitAnyChild, Response: `{"run_id":"run-1","result":"done"}`},
	}
	result := s.AwaitAnyChild(context.Background(), nil, `["run-1","run-2"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected error code 0, got %d", errCode)
	}
	if !s.isReplay {
		t.Error("expected isReplay=true after consuming both events")
	}
	if s.stepCount != 2 {
		t.Errorf("expected stepCount=2 (both events consumed), got %d", s.stepCount)
	}
}

func TestAwaitAnyChildReplaySuspendNoReexec(t *testing.T) {
	mock := &mockChildWorkflowStore{completed: false}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: EventTypeAwaitAnyChild,
	}}

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_any_child") {
		t.Errorf("expected Reason containing 'await_any_child', got %q", s.suspendErr.Reason)
	}
}

func TestAwaitAnyChildReplayPastEnd(t *testing.T) {
	mock := &mockChildWorkflowStore{completed: false}
	s := newTestExecSession()
	s.engine.childWfStore = mock
	s.isReplay = true
	// empty history — stepCount >= len(history) triggers exitReplay

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
}

func TestAwaitAnyChildFreshCompleted(t *testing.T) {
	mock := &mockChildWorkflowStore{
		result:    "done",
		completed: true,
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected error code 0, got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	lastEvent := s.history[len(s.history)-1]
	if lastEvent.EventType != EventTypeAwaitAnyChild {
		t.Errorf("expected EventTypeAwaitAnyChild, got %s", lastEvent.EventType)
	}
	if lastEvent.Request != `["run-1"]` {
		t.Errorf("expected Request=%q, got %q", `["run-1"]`, lastEvent.Request)
	}
	if !strings.Contains(lastEvent.Response, `"run_id":"run-1"`) {
		t.Errorf("expected Response with run_id, got %q", lastEvent.Response)
	}
	if !strings.Contains(lastEvent.Response, `"result":"done"`) {
		t.Errorf("expected Response with result, got %q", lastEvent.Response)
	}
}

func TestAwaitAnyChildFreshStoreError(t *testing.T) {
	mock := &mockChildWorkflowStore{
		err: fmt.Errorf("db down"),
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected error code 0 (error is in JSON response), got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	lastEvent := s.history[len(s.history)-1]
	if lastEvent.EventType != EventTypeAwaitAnyChild {
		t.Errorf("expected EventTypeAwaitAnyChild, got %s", lastEvent.EventType)
	}
	if !strings.Contains(lastEvent.Response, `"error":"db down"`) {
		t.Errorf("expected Response to contain error, got %q", lastEvent.Response)
	}
	if !strings.Contains(lastEvent.Response, `"run_id":"run-1"`) {
		t.Errorf("expected Response with run_id, got %q", lastEvent.Response)
	}
}

func TestAwaitAnyChildFreshNoChildCompleted(t *testing.T) {
	mock := &mockChildWorkflowStore{completed: false}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
	if !strings.Contains(s.suspendErr.Reason, "await_any_child") {
		t.Errorf("expected Reason containing 'await_any_child', got %q", s.suspendErr.Reason)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 (empty event recorded), got %d", s.stepCount)
	}
}

func TestAwaitAnyChildFreshNilStore(t *testing.T) {
	s := newTestExecSession()
	// s.engine.childWfStore is nil — verify no panic, graceful suspend

	result := s.AwaitAnyChild(context.Background(), nil, `["run-1"]`, 0, 0)

	if result != packAwaitChildResultSuspend() {
		t.Errorf("expected suspend sentinel, got %d", result)
	}
	if s.suspendErr == nil {
		t.Fatal("expected suspendErr non-nil")
	}
}

func TestAwaitAnyChildFreshInvalidJSON(t *testing.T) {
	s := newTestExecSession()

	result := s.AwaitAnyChild(context.Background(), nil, "not valid json", 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected error code 1, got %d", errCode)
	}
}

func TestAwaitAnyChildFreshDeterministicOrdering(t *testing.T) {
	mock := &mockChildWorkflowStore{
		result:    "ok",
		completed: true,
	}
	s := newTestExecSession()
	s.engine.childWfStore = mock

	s.AwaitAnyChild(context.Background(), nil, `["c","a","b"]`, 0, 0)

	// "a" sorted first, so it should be polled first and returned immediately.
	if mock.gotRunID != "a" {
		t.Errorf("expected first polled runID='a' (sorted order), got %q", mock.gotRunID)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	lastEvent := s.history[len(s.history)-1]
	if lastEvent.EventType != EventTypeAwaitAnyChild {
		t.Errorf("expected EventTypeAwaitAnyChild, got %s", lastEvent.EventType)
	}
	// Request should preserve original (unsorted) order.
	if lastEvent.Request != `["c","a","b"]` {
		t.Errorf("expected Request to preserve original order, got %q", lastEvent.Request)
	}
}

// ---------------------------------------------------------------------------
// RegisterUpdateHandler tests.
// ---------------------------------------------------------------------------

func TestRegisterUpdateHandlerReplay_Match(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:              0,
		EventType:         EventTypeUpdateHandler,
		UpdateHandlerName: "my-handler",
	}}
	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1 after advanceReplayStep, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true after replay match")
	}
}

func TestRegisterUpdateHandlerReplay_Mismatch(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:      0,
		EventType: "call", // wrong type
	}}
	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if len(s.history) < 2 {
		t.Fatalf("expected at least 2 history entries, got %d", len(s.history))
	}
	if s.history[1].EventType != EventTypeUpdateHandler {
		t.Errorf("expected fresh event type update_handler, got %q", s.history[1].EventType)
	}
	if s.history[1].UpdateHandlerName != "my-handler" {
		t.Errorf("expected UpdateHandlerName='my-handler', got %q", s.history[1].UpdateHandlerName)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestRegisterUpdateHandlerReplay_PastEnd(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	// empty history triggers exitReplay

	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if s.isReplay {
		t.Error("expected isReplay=false after exitReplay")
	}
	if s.isReplay {
		t.Error("expected replay to have ended")
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypeUpdateHandler {
		t.Errorf("expected fresh event type update_handler, got %q", s.history[0].EventType)
	}
	if s.history[0].UpdateHandlerName != "my-handler" {
		t.Errorf("expected UpdateHandlerName='my-handler', got %q", s.history[0].UpdateHandlerName)
	}
	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
}

func TestRegisterUpdateHandlerFresh(t *testing.T) {
	s := newTestExecSession()

	result := s.RegisterUpdateHandler(context.Background(), nil, "my-handler")

	if result != 0 {
		t.Errorf("expected 0, got %d", result)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	rec := s.history[0]
	if rec.EventType != EventTypeUpdateHandler {
		t.Errorf("expected EventTypeUpdateHandler, got %s", rec.EventType)
	}
	if rec.UpdateHandlerName != "my-handler" {
		t.Errorf("expected UpdateHandlerName='my-handler', got %q", rec.UpdateHandlerName)
	}
}

// ---------------------------------------------------------------------------
// PluginCallStreaming tests.
// ---------------------------------------------------------------------------

func TestPluginCallStreamingReplay_MultipleChunks(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{
		{Step: 0, EventType: EventTypePluginCallStreamChunk,
			PluginOutput: "chunk1", StreamChunkIndex: 0},
		{Step: 1, EventType: EventTypePluginCallStreamChunk,
			PluginOutput: "chunk2", StreamChunkIndex: 1, StreamFinish: true},
	}
	result := s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode=0, got %d", errCode)
	}
	if s.stepCount != 2 {
		t.Errorf("expected stepCount=2, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay=true")
	}
}

// Covers the in-memory branch only. This assigns s.history directly, so its
// StreamFinish is a field the store could not produce until IMPROVEMENT-PLAN
// 3.96 -- the flag was written by neither eventRecordToPayload nor a column,
// and this test passed throughout. engine/stream_finish_persistence_test.go
// is the version that puts the same record through the real encoder and
// decoder first, and it is the one that fails when the flag is not persisted.
func TestPluginCallStreamingReplay_StreamError(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:             0,
		EventType:        EventTypePluginCallStreamChunk,
		PluginOutput:     "plugin_call_streaming: boom",
		StreamChunkIndex: 0,
		StreamFinish:     true,
	}}
	result := s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode=1 (stream error), got %d", errCode)
	}
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay=true")
	}
}

func TestPluginCallStreamingReplay_EmptyHistory(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	// empty history — no chunks to replay

	result := s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode=0 (empty result), got %d", errCode)
	}
	if s.stepCount != 0 {
		t.Errorf("expected stepCount=0 (no events consumed), got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay=true")
	}
}

func TestPluginCallStreamingFresh_Success(t *testing.T) {
	psr := NewPluginStreamRegistry()
	psr.Register("test-plugin", "Echo", func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		ch := make(chan plugin.StreamEvent, 2)
		ch <- plugin.StreamEvent{Index: 0, Content: "hello"}
		ch <- plugin.StreamEvent{Index: 1, Content: "world", Finish: true}
		close(ch)
		return ch, nil
	})

	s := newTestExecSession()
	s.engine.pluginStreamRegistry = psr

	result := s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 0 {
		t.Errorf("expected errCode=0, got %d", errCode)
	}
	if s.stepCount != 2 {
		t.Errorf("expected stepCount=2 (two chunks), got %d", s.stepCount)
	}
	if len(s.history) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(s.history))
	}
	if s.history[0].EventType != EventTypePluginCallStreamChunk {
		t.Errorf("expected EventTypePluginCallStreamChunk in history[0], got %s", s.history[0].EventType)
	}
	if s.history[1].EventType != EventTypePluginCallStreamChunk {
		t.Errorf("expected EventTypePluginCallStreamChunk in history[1], got %s", s.history[1].EventType)
	}
	if !s.history[1].StreamFinish {
		t.Error("expected second chunk to have StreamFinish=true")
	}
}

func TestPluginCallStreamingFresh_NoRegistry(t *testing.T) {
	s := newTestExecSession()
	// s.engine.pluginStreamRegistry is nil

	result := s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode=1 (no registry), got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].StreamFinish != true {
		t.Error("expected StreamFinish=true on error event")
	}
	if s.history[0].StreamChunkIndex != 0 {
		t.Errorf("expected StreamChunkIndex=0, got %d", s.history[0].StreamChunkIndex)
	}
}

func TestPluginCallStreamingFresh_FuncNotFound(t *testing.T) {
	psr := NewPluginStreamRegistry()
	// "Echo" is not registered

	s := newTestExecSession()
	s.engine.pluginStreamRegistry = psr

	result := s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode=1 (not found), got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].StreamFinish != true {
		t.Error("expected StreamFinish=true on error event")
	}
}

func TestPluginCallStreamingFresh_FuncError(t *testing.T) {
	psr := NewPluginStreamRegistry()
	psr.Register("test-plugin", "Echo", func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		return nil, fmt.Errorf("plugin init failure")
	})

	s := newTestExecSession()
	s.engine.pluginStreamRegistry = psr

	result := s.PluginCallStreaming(context.Background(), nil, "test-plugin", "Echo", `{}`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode=1 (func error), got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(s.history))
	}
	if s.history[0].StreamFinish != true {
		t.Error("expected StreamFinish=true on error event")
	}
}

// ---- SuspendError.Error() ----

func TestSuspendError_Error_WithoutUntil(t *testing.T) {
	e := &SuspendError{Reason: "timeout"}
	got := e.Error()
	if want := "cleat: suspend: timeout"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSuspendError_Error_WithUntil(t *testing.T) {
	tm := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	e := &SuspendError{Reason: "sleep", Until: tm}
	got := e.Error()
	if !strings.Contains(got, "cleat: suspend until") || !strings.Contains(got, "sleep") {
		t.Errorf("got %q, expected 'suspend until' and 'sleep'", got)
	}
}

// ---- DeferralsFromHistory ----

func TestDeferralsFromHistory_Empty(t *testing.T) {
	defs := DeferralsFromHistory(nil)
	if len(defs) != 0 {
		t.Errorf("expected empty map, got %d entries", len(defs))
	}
	defs = DeferralsFromHistory([]EventRecord{})
	if len(defs) != 0 {
		t.Errorf("expected empty map, got %d entries", len(defs))
	}
}

func TestDeferralsFromHistory_DefersOnly(t *testing.T) {
	history := []EventRecord{
		{EventType: EventTypeDefer, DeferID: "d1", DeferDescription: "cleanup temp files"},
		{EventType: EventTypeDefer, DeferID: "d2", DeferDescription: "release lock"},
	}
	defs := DeferralsFromHistory(history)
	if len(defs) != 2 {
		t.Fatalf("expected 2 defers, got %d", len(defs))
	}
	if defs["d1"] != "cleanup temp files" {
		t.Errorf("d1: got %q, want %q", defs["d1"], "cleanup temp files")
	}
	if defs["d2"] != "release lock" {
		t.Errorf("d2: got %q, want %q", defs["d2"], "release lock")
	}
}

func TestDeferralsFromHistory_MixedEvents(t *testing.T) {
	history := []EventRecord{
		{EventType: "call", Service: "svc"},
		{EventType: EventTypeDefer, DeferID: "d1", DeferDescription: "cleanup"},
		{EventType: "sleep"},
		{EventType: EventTypeDefer, DeferID: "d2", DeferDescription: "notify"},
	}
	defs := DeferralsFromHistory(history)
	if len(defs) != 2 {
		t.Fatalf("expected 2 defers, got %d", len(defs))
	}
	if defs["d1"] != "cleanup" {
		t.Errorf("d1: got %q, want 'cleanup'", defs["d1"])
	}
	if defs["d2"] != "notify" {
		t.Errorf("d2: got %q, want 'notify'", defs["d2"])
	}
}

// ---- DispatchUpdate ----

func TestDispatchUpdate_NilHandler(t *testing.T) {
	engine := NewEngine(nil, nil)
	_, err := engine.DispatchUpdate(context.Background(), "update1", `{"key":"val"}`)
	if err == nil {
		t.Fatal("expected error for nil handler")
	}
	if !strings.Contains(err.Error(), "no update handler configured") {
		t.Errorf("error should mention missing handler: %v", err)
	}
}

func TestDispatchUpdate_ValidHandler(t *testing.T) {
	handler := func(name, payload string) (string, error) {
		return `{"result":"` + name + `"}`, nil
	}
	engine := NewEngine(nil, nil, WithUpdateHandler(handler))
	result, err := engine.DispatchUpdate(context.Background(), "myUpdate", `{"x":1}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `{"result":"myUpdate"}`; result != want {
		t.Errorf("got %q, want %q", result, want)
	}
}

// ---- invokeStepCallback ----

func TestInvokeStepCallback_NilCallback(t *testing.T) {
	s := newTestExecSession()
	s.stepCount = 5
	ok := s.invokeStepCallback(context.Background(), nil)
	if !ok {
		t.Error("expected true with nil callback")
	}
	if s.stepCount != 5 {
		t.Errorf("stepCount should not change: got %d", s.stepCount)
	}
}

func TestInvokeStepCallback_ReplayNext(t *testing.T) {
	var calledStep int
	var calledRec *EventRecord
	var calledQS map[string]string
	cb := WithReplayStepCallback(func(step int, rec *EventRecord, qs map[string]string) ReplayStepAction {
		calledStep = step
		calledRec = rec
		calledQS = qs
		return ReplayNext
	})
	s := newTestExecSession()
	s.engine.stepCallback = nil // clear, then apply
	cb(s.engine)
	s.stepCallback = s.engine.stepCallback
	s.stepCount = 7
	s.queryState["key"] = "val"
	rec := &EventRecord{Service: "test-svc"}

	ok := s.invokeStepCallback(context.Background(), rec)
	if !ok {
		t.Error("expected true for ReplayNext")
	}
	if calledStep != 6 { // stepCount-1
		t.Errorf("expected step 6, got %d", calledStep)
	}
	if calledRec != rec {
		t.Error("callback should receive the EventRecord")
	}
	if calledQS["key"] != "val" {
		t.Error("queryState snapshot missing entries")
	}
	// Verify snapshot is a copy — modifying returned qs doesn't affect session.
	if len(calledQS) > 0 {
		calledQS["key"] = "modified"
		if s.queryState["key"] == "modified" {
			t.Error("modifying snapshot should not mutate session queryState")
		}
	}
}

func TestInvokeStepCallback_ReplayQuit(t *testing.T) {
	cancelCalled := false
	cb := WithReplayStepCallback(func(step int, rec *EventRecord, qs map[string]string) ReplayStepAction {
		return ReplayQuit
	})
	s := newTestExecSession()
	s.engine.stepCallback = nil
	cb(s.engine)
	s.stepCallback = s.engine.stepCallback
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.stepCancel = func() { cancelCalled = true }
	s.stepCount = 3

	ok := s.invokeStepCallback(ctx, nil)
	if ok {
		t.Error("expected false for ReplayQuit")
	}
	if !cancelCalled {
		t.Error("expected stepCancel to be called on ReplayQuit")
	}
}

// ---- DurableDefer ----

func TestDurableDefer_Fresh(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = false
	s.DurableDefer(context.Background(), nil, "cleanup temp files", 0, 0)
	if len(s.history) != 1 {
		t.Fatalf("expected 1 event recorded, got %d", len(s.history))
	}
	rec := s.history[0]
	if rec.EventType != EventTypeDefer {
		t.Errorf("expected EventTypeDefer, got %s", rec.EventType)
	}
	if rec.DeferDescription != "cleanup temp files" {
		t.Errorf("got description %q, want %q", rec.DeferDescription, "cleanup temp files")
	}
	if len(s.deferrals) != 1 {
		t.Fatalf("expected 1 deferral in map, got %d", len(s.deferrals))
	}
	if s.deferrals[rec.DeferID] != "cleanup temp files" {
		t.Errorf("deferral map has wrong description: %q", s.deferrals[rec.DeferID])
	}
}

func TestDurableDefer_Replay(t *testing.T) {
	s := newTestExecSession()
	s.isReplay = true
	s.history = []EventRecord{{
		Step:             0,
		EventType:        EventTypeDefer,
		DeferID:          "defer-99",
		DeferDescription: "release lock",
	}}
	s.DurableDefer(context.Background(), nil, "ignored", 0, 0)
	if s.stepCount != 1 {
		t.Errorf("expected stepCount=1, got %d", s.stepCount)
	}
	if !s.isReplay {
		t.Error("expected isReplay to remain true")
	}
}

// ---- FreshPluginCallStreaming — call guard rejection ----

func TestPluginCallStreamingFresh_CallGuardRejection(t *testing.T) {
	psr := NewPluginStreamRegistry()
	psr.Register("secure-plugin", "GetSecrets", func(ctx context.Context, inputJSON string) (<-chan plugin.StreamEvent, error) {
		ch := make(chan plugin.StreamEvent, 1)
		ch <- plugin.StreamEvent{Index: 0, Content: "secret", Finish: true}
		close(ch)
		return ch, nil
	})

	guard := NewPluginCallGuard()
	guard.Allow("caller-plugin", []string{"other-plugin"}) // NOT allowed to call secure-plugin

	s := newTestExecSession()
	s.engine.pluginStreamRegistry = psr
	s.engine.pluginCallGuard = guard
	s.callerPluginName = "caller-plugin"

	result := s.PluginCallStreaming(context.Background(), nil, "secure-plugin", "GetSecrets", `{}`, 0, 0)

	errCode := uint32(result & 0xFFFFFFFF)
	if errCode != 1 {
		t.Errorf("expected errCode=1 (call guard rejection), got %d", errCode)
	}
	if len(s.history) != 1 {
		t.Fatalf("expected 1 history entry (error event), got %d", len(s.history))
	}
	if s.history[0].StreamFinish != true {
		t.Error("expected StreamFinish=true on guard rejection event")
	}
	if s.history[0].EventType != EventTypePluginCallStreamChunk {
		t.Errorf("expected EventTypePluginCallStreamChunk, got %s", s.history[0].EventType)
	}
}
