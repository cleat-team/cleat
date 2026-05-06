package host

import (
	"context"
	"encoding/json"
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
	default:
		return `{}`
	}
}

// ---- Unit tests (no WASM compilation needed) ----

func TestNewRuntime(t *testing.T) {
	ctx := context.Background()
	rt, err := NewRuntime(ctx)
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
	rt, err := NewRuntime(ctx)
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
	rt, _ := NewRuntime(ctx)
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

// ---- Engine replay/divergence tests ----

func TestEngineExecute(t *testing.T) {
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	input := []byte(`{"UserID":"test-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)
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
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// First: execute to get history.
	caller1 := &mockCaller{}
	engine1 := NewEngine(rt, caller1)
	input := []byte(`{"UserID":"test-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)
	result1, history, _, _, _, err := engine1.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Second: replay with captured history.
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
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Execute to get history.
	caller1 := &mockCaller{}
	engine1 := NewEngine(rt, caller1)
	input := []byte(`{"UserID":"test-user","Cart":[{"SKU":"ABC-123","Quantity":2}]}`)
	_, history, _, _, _, err := engine1.Execute(ctx, wasmBytes, "place_order", input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Tamper with history to cause divergence.
	if len(history) > 0 {
		history[0].Service = "different_service"
	}

	caller2 := &mockCaller{}
	engine2 := NewEngine(rt, caller2)
	_, _, _, _, _, err = engine2.Replay(ctx, wasmBytes, "place_order", input, history)
	if err == nil {
		t.Error("expected divergence error")
	} else {
		t.Logf("Divergence detected: %v", err)
	}
}

func TestEngineExecuteCancelOrder(t *testing.T) {
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	ctx := context.Background()
	rt, err := NewRuntime(ctx)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	input := []byte(`{"OrderID":"ord-123"}`)
	result, history, _, _, _, err := engine.Execute(ctx, wasmBytes, "cancel_order", input)
	if err != nil {
		t.Fatalf("Execute cancel_order: %v", err)
	}
	t.Logf("cancel_order result: %s, history: %d calls", result, len(history))
	if len(history) < 2 {
		t.Errorf("expected at least 2 calls for cancel_order, got %d", len(history))
	}
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

// ---- Helpers ----

// minimalWasm returns a minimal valid WASM module that can be loaded by wazero.
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
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo not installed — skipping WASM integration test")
	}

	cwd, _ := os.Getwd()
	projectRoot := cwd
	if strings.HasSuffix(cwd, "internal/host") {
		projectRoot = filepath.Dir(filepath.Dir(cwd))
	}

	tmpDir := t.TempDir()
	cmd := exec.Command("go", "run", filepath.Join(projectRoot, "cmd", "durable"),
		"build", "--target", "tinygo", "-o", tmpDir, filepath.Join(projectRoot, "testdata", "basic"))
	cmd.Dir = projectRoot

	// tinygo needs GOROOT and TINYGOROOT.
	cmd.Env = os.Environ()
	if goroot := os.Getenv("GOROOT"); goroot != "" {
		cmd.Env = append(cmd.Env, "GOROOT="+goroot)
	}
	if tinygoroot := os.Getenv("TINYGOROOT"); tinygoroot != "" {
		cmd.Env = append(cmd.Env, "TINYGOROOT="+tinygoroot)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("durable build failed:\n%s\n%v", string(out), err)
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
