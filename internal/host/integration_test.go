package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cleat-team/cleat/internal/host/testutil"
)

// ---------------------------------------------------------------------------
// Test 1: Full pipeline — real DB + WASM compile + Engine execute + event
// recording + event loading + replay with zero real service calls.
// ---------------------------------------------------------------------------

// TestIntegrationFullPipeline exercises the complete cleat execution path:
// real PostgreSQL, WASM compilation, Engine execution, event persistence via
// AppendEventHistoryBatch, event loading via LoadEventHistory, and replay
// with verified zero real service calls.
func TestIntegrationFullPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	ctx := context.Background()
	runID := fmt.Sprintf("int-full-%d", time.Now().UnixNano())
	defName := "test-place-order"
	input := `{"UserID":"user-42","Cart":[{"SKU":"SKU-001","Quantity":2}]}`

	// Build WASM binary from testdata/basic.
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	// Insert workflow definition with the compiled WASM bytes.
	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points) VALUES ($1, $2, $3, $4)`,
		defName, 1, wasmBytes, `{place_order,cancel_order}`); err != nil {
		t.Fatalf("insert workflow_defs: %v", err)
	}

	// Insert a workflow instance in 'ready' status.
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, $2, $3, $4, $5)`,
		runID, defName, 1, "ready", input); err != nil {
		t.Fatalf("insert workflow_instances: %v", err)
	}

	// Clean up test data on exit.
	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
		db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)
	}()

	// Create the runtime and store.
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	store := NewPostgresStore(db)

	// ---- Step 1: Execute the workflow ----
	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected workflow suspension: %v", suspended.Reason)
	}
	if result == "" {
		t.Error("expected non-empty result from place_order")
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty event history")
	}
	t.Logf("Execution produced %d events, result=%q", len(history), result)

	// Verify the expected service calls match the place_order workflow.
	expectedServices := []string{"catalog", "inventory", "payments", "payments", "shipping", "notifications"}
	if len(history) != len(expectedServices) {
		t.Errorf("expected %d history events, got %d", len(expectedServices), len(history))
	}
	for i, svc := range expectedServices {
		if i >= len(history) {
			break
		}
		if history[i].Service != svc {
			t.Errorf("step %d: expected service %q, got %q", i, svc, history[i].Service)
		}
	}

	// ---- Step 2: Persist events to the database ----
	if err := store.AppendEventHistoryBatch(ctx, runID, history); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// ---- Step 3: Load events back from the database ----
	loadedHistory, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	// ---- Step 4: Verify loaded events match the original ----
	if len(loadedHistory) != len(history) {
		t.Errorf("history length mismatch: original=%d, loaded=%d", len(history), len(loadedHistory))
	}
	for i, rec := range history {
		if i >= len(loadedHistory) {
			t.Errorf("step %d: missing from loaded history", i)
			continue
		}
		if rec.EventType != loadedHistory[i].EventType {
			t.Errorf("step %d EventType: expected %q, got %q", i, rec.EventType, loadedHistory[i].EventType)
		}
		if rec.Service != loadedHistory[i].Service {
			t.Errorf("step %d Service: expected %q, got %q", i, rec.Service, loadedHistory[i].Service)
		}
		if rec.Op != loadedHistory[i].Op {
			t.Errorf("step %d Op: expected %q, got %q", i, rec.Op, loadedHistory[i].Op)
		}
		if rec.Response != loadedHistory[i].Response {
			t.Errorf("step %d Response: expected %q, got %q", i, rec.Response, loadedHistory[i].Response)
		}
	}

	// ---- Step 5: Replay from the loaded event history ----
	replayCaller := &mockCaller{}
	engine2 := NewEngine(rt, replayCaller)
	result2, _, suspended2, _, _, err := engine2.Replay(ctx, wasmBytes, "place_order", json.RawMessage(input), loadedHistory)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if suspended2 != nil {
		t.Fatalf("unexpected suspension during replay: %v", suspended2.Reason)
	}

	// ---- Step 6: Verify replay produced the same result ----
	if result != result2 {
		t.Errorf("replay result mismatch: original=%q, replay=%q", result, result2)
	}

	// ---- Step 7: Verify NO real service calls were made during replay ----
	if len(replayCaller.calls) > 0 {
		t.Errorf("replay made %d real service calls (expected 0)", len(replayCaller.calls))
	}

	t.Log("Full pipeline integration test passed")
}

// ---------------------------------------------------------------------------
// Test 2: Multi-step sleep / suspend-resume cycle.
//
// The testdata/basic/order.go workflow does not use DurableSleep, so it
// completes in a single pass rather than suspending. This test exercises
// the persistence-and-replay cycle with a different entry point (cancel_order)
// to validate the multi-step execute-store-load-replay loop is correct.
// ---------------------------------------------------------------------------

// TestIntegrationMultiStepSleep tests the persistence and replay of a
// multi-step workflow execution. The testdata/basic workflow does not use
// DurableSleep, so it completes in a single pass. This test validates the
// execute-store-load-replay cycle for a multi-step workflow.
func TestIntegrationMultiStepSleep(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	ctx := context.Background()
	runID := fmt.Sprintf("int-multistep-%d", time.Now().UnixNano())
	defName := "test-multistep"
	input := `{"OrderID":"ord-123"}`

	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points) VALUES ($1, $2, $3, $4)`,
		defName, 1, wasmBytes, `{place_order,cancel_order}`); err != nil {
		t.Fatalf("insert workflow_defs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, $2, $3, $4, $5)`,
		runID, defName, 1, "ready", input); err != nil {
		t.Fatalf("insert workflow_instances: %v", err)
	}

	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
		db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)
	}()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	store := NewPostgresStore(db)

	// Execute the cancel_order workflow (refund + release = 2 service calls).
	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "cancel_order", json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute cancel_order: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected suspension in cancel_order: %v", suspended.Reason)
	}
	if len(history) < 2 {
		t.Errorf("expected at least 2 events for cancel_order, got %d", len(history))
	}
	t.Logf("cancel_order result=%q, events=%d", result, len(history))

	// Store events.
	if err := store.AppendEventHistoryBatch(ctx, runID, history); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// Load events.
	loadedHistory, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(loadedHistory) != len(history) {
		t.Errorf("history length mismatch: original=%d, loaded=%d", len(history), len(loadedHistory))
	}

	// Replay from loaded history.
	replayCaller := &mockCaller{}
	engine2 := NewEngine(rt, replayCaller)
	result2, _, _, _, _, err := engine2.Replay(ctx, wasmBytes, "cancel_order", json.RawMessage(input), loadedHistory)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if result != result2 {
		t.Errorf("replay result mismatch: %q vs %q", result, result2)
	}
	if len(replayCaller.calls) > 0 {
		t.Errorf("replay made %d real calls (expected 0)", len(replayCaller.calls))
	}

	t.Log("Multi-step sleep integration test passed")
}

// ---------------------------------------------------------------------------
// Test 3: Signal delivery and polling.
//
// The testdata/basic/order.go workflow does not use DurableAwaitSignals, so
// this test validates the signal infrastructure at the PostgresStore level:
// signal delivery, polling, atomic consumption, and the persistence+replay
// cycle alongside signal operations.
// ---------------------------------------------------------------------------

// TestIntegrationSignalAndResume tests signal delivery and polling through the
// PostgresStore's SignalStore implementation. It validates that signals can be
// delivered, polled, are consumed exactly once, and that the standard
// persistence+replay cycle works alongside signal operations.
func TestIntegrationSignalAndResume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	ctx := context.Background()
	runID := fmt.Sprintf("int-signal-%d", time.Now().UnixNano())
	defName := "test-signal"
	input := `{"UserID":"user-42","Cart":[{"SKU":"ABC-123","Quantity":2}]}`

	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points) VALUES ($1, $2, $3, $4)`,
		defName, 1, wasmBytes, `{place_order,cancel_order}`); err != nil {
		t.Fatalf("insert workflow_defs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, $2, $3, $4, $5)`,
		runID, defName, 1, "ready", input); err != nil {
		t.Fatalf("insert workflow_instances: %v", err)
	}

	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_signals WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
		db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)
	}()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	store := NewPostgresStore(db)

	// ---- Step 1: Execute a basic workflow to completion ----
	caller := &mockCaller{}
	engine := NewEngine(rt, caller)

	result, history, suspended, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if suspended != nil {
		t.Fatalf("unexpected suspension: %v", suspended.Reason)
	}
	t.Logf("Execution produced %d events, result=%q", len(history), result)

	// ---- Step 2: Deliver a signal through the store ----
	signalPayload := `{"status":"approved","reason":"payment_received"}`
	if err := store.DeliverSignal(ctx, runID, "payment_confirmed", signalPayload); err != nil {
		t.Fatalf("DeliverSignal: %v", err)
	}

	// ---- Step 3: Poll the signal back (atomic claim + delete) ----
	payload, found, err := store.PollSignal(ctx, runID, "payment_confirmed")
	if err != nil {
		t.Fatalf("PollSignal: %v", err)
	}
	if !found {
		t.Error("expected PollSignal to find the delivered signal")
	}
	if payload != signalPayload {
		t.Errorf("signal payload mismatch: expected=%q, got=%q", signalPayload, payload)
	}
	t.Logf("Signal delivered and polled successfully: %s", payload)

	// ---- Step 4: Polling the same signal again returns not-found (consumed) ----
	_, found, err = store.PollSignal(ctx, runID, "payment_confirmed")
	if err != nil {
		t.Fatalf("second PollSignal: %v", err)
	}
	if found {
		t.Error("expected second PollSignal to return not-found (signal was consumed)")
	}

	// ---- Step 5: Persist the execution history ----
	if err := store.AppendEventHistoryBatch(ctx, runID, history); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// ---- Step 6: Load and replay ----
	loadedHistory, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(loadedHistory) != len(history) {
		t.Errorf("history length mismatch: original=%d, loaded=%d", len(history), len(loadedHistory))
	}

	replayCaller := &mockCaller{}
	engine2 := NewEngine(rt, replayCaller)
	result2, _, _, _, _, err := engine2.Replay(ctx, wasmBytes, "place_order", json.RawMessage(input), loadedHistory)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if result != result2 {
		t.Errorf("replay result mismatch: %q vs %q", result, result2)
	}
	if len(replayCaller.calls) > 0 {
		t.Errorf("replay made %d real service calls (expected 0)", len(replayCaller.calls))
	}

	t.Log("Signal and resume integration test passed")
}

// ---------------------------------------------------------------------------
// Test 4: Replay divergence detection with tampered event history.
// ---------------------------------------------------------------------------

// TestIntegrationReplayDivergence verifies that tampering with persisted
// event history causes a replay divergence error. It runs a workflow to
// completion, persists the events, loads them back, tampers with a service
// name, and verifies that replay detects the mismatch.
func TestIntegrationReplayDivergence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	ctx := context.Background()
	runID := fmt.Sprintf("int-divergence-%d", time.Now().UnixNano())
	defName := "test-divergence"
	input := `{"UserID":"user-42","Cart":[{"SKU":"ABC-123","Quantity":2}]}`

	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points) VALUES ($1, $2, $3, $4)`,
		defName, 1, wasmBytes, `{place_order,cancel_order}`); err != nil {
		t.Fatalf("insert workflow_defs: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, $2, $3, $4, $5)`,
		runID, defName, 1, "ready", input); err != nil {
		t.Fatalf("insert workflow_instances: %v", err)
	}

	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
		db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)
	}()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	store := NewPostgresStore(db)

	// ---- Step 1: Execute to completion ----
	caller := &mockCaller{}
	engine := NewEngine(rt, caller)
	_, history, _, _, _, err := engine.Execute(ctx, wasmBytes, "place_order", json.RawMessage(input))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Logf("Execution produced %d events", len(history))

	// ---- Step 2: Persist events to the database ----
	if err := store.AppendEventHistoryBatch(ctx, runID, history); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// ---- Step 3: Load events back from the database ----
	loadedHistory, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}
	if len(loadedHistory) == 0 {
		t.Fatal("expected non-empty loaded history")
	}

	// ---- Step 4: Tamper with the loaded history ----
	// Change the first event's service and operation to something the
	// workflow won't call, forcing a replay divergence.
	if len(loadedHistory) > 0 {
		loadedHistory[0].Service = "tampered_service"
		loadedHistory[0].Op = "tampered_operation"
	}

	// ---- Step 5: Attempt replay with tampered history ----
	replayCaller := &mockCaller{}
	engine2 := NewEngine(rt, replayCaller)
	_, _, _, _, _, err = engine2.Replay(ctx, wasmBytes, "place_order", json.RawMessage(input), loadedHistory)
	if err == nil {
		t.Error("expected divergence error from tampered history, got nil")
	} else {
		t.Logf("Divergence correctly detected: %v", err)
	}

	// Verify NO real calls were made during the failed replay attempt.
	if len(replayCaller.calls) > 0 {
		t.Errorf("failed replay made %d real service calls (expected 0)", len(replayCaller.calls))
	}

	t.Log("Replay divergence integration test passed")
}

// ---------------------------------------------------------------------------
// Test 5: Persistence round-trip for the 7 new event types.
//
// Creates EventRecords for create_promise, await_promise, promise_resolved,
// promise_rejected, update_handler, state_mutation, and run_detached, persists
// them via AppendEventHistoryBatch, loads them back via LoadEventHistory, and
// verifies all fields (including the promise fields from fix #26) survive the
// round-trip.
// ---------------------------------------------------------------------------

// TestIntegrationNewEventTypesPersistenceRoundTrip verifies that all 7 new
// event types can be persisted to and loaded from PostgreSQL with complete
// field fidelity, with special focus on the promise fields (promise_id,
// promise_result, promise_error, promise_name) that were the subject of the
// #26 copy-paste bug fix.
func TestIntegrationNewEventTypesPersistenceRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	ctx := context.Background()
	runID := fmt.Sprintf("int-new-events-%d", time.Now().UnixNano())

	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
	}()

	store := NewPostgresStore(db)

	// Create one EventRecord for each of the 7 new event types.
	events := []EventRecord{
		{
			Step: 0, EventType: EventTypeCreatePromise,
			PromiseName: "test-promise",
			PromiseID:   "prom-001",
		},
		{
			Step: 1, EventType: EventTypeAwaitPromise,
			PromiseID:     "prom-001",
			PromiseResult: `{"ok":true}`,
		},
		{
			Step: 2, EventType: EventTypePromiseResolved,
			PromiseID:     "prom-001",
			PromiseResult: `{"ok":true,"value":42}`,
		},
		{
			Step: 3, EventType: EventTypePromiseRejected,
			PromiseID:    "prom-002",
			PromiseError: "rejected: timeout",
		},
		{
			Step: 4, EventType: EventTypeUpdateHandler,
			UpdateHandlerName: "update-status",
		},
		{
			Step: 5, EventType: EventTypeStateMutation,
			StateKey:   "counter",
			StateValue: "42",
			StateDelta: 1,
			StateOp:    "increment",
		},
		{
			Step: 6, EventType: EventTypeRunDetached,
		},
	}

	// ---- Step 1: Persist events to the database ----
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("AppendEventHistoryBatch: %v", err)
	}

	// ---- Step 2: Load events back from the database ----
	loaded, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	// ---- Step 3: Verify loaded events match original ----
	if len(loaded) != len(events) {
		t.Fatalf("history length mismatch: inserted=%d, loaded=%d", len(events), len(loaded))
	}

	for i, expected := range events {
		got := loaded[i]

		// Core fields.
		if got.Step != expected.Step {
			t.Errorf("step %d Step: expected %d, got %d", i, expected.Step, got.Step)
		}
		if got.EventType != expected.EventType {
			t.Errorf("step %d EventType: expected %q, got %q", i, expected.EventType, got.EventType)
		}

		// Promise fields (the focus of the #26 copy-paste bug fix).
		if got.PromiseName != expected.PromiseName {
			t.Errorf("step %d PromiseName: expected %q, got %q", i, expected.PromiseName, got.PromiseName)
		}
		if got.PromiseID != expected.PromiseID {
			t.Errorf("step %d PromiseID: expected %q, got %q", i, expected.PromiseID, got.PromiseID)
		}
		if got.PromiseResult != expected.PromiseResult {
			t.Errorf("step %d PromiseResult: expected %q, got %q", i, expected.PromiseResult, got.PromiseResult)
		}
		if got.PromiseError != expected.PromiseError {
			t.Errorf("step %d PromiseError: expected %q, got %q", i, expected.PromiseError, got.PromiseError)
		}

		// Update handler fields.
		if got.UpdateHandlerName != expected.UpdateHandlerName {
			t.Errorf("step %d UpdateHandlerName: expected %q, got %q", i, expected.UpdateHandlerName, got.UpdateHandlerName)
		}

		// State mutation fields.
		if got.StateKey != expected.StateKey {
			t.Errorf("step %d StateKey: expected %q, got %q", i, expected.StateKey, got.StateKey)
		}
		if got.StateValue != expected.StateValue {
			t.Errorf("step %d StateValue: expected %q, got %q", i, expected.StateValue, got.StateValue)
		}
		if got.StateDelta != expected.StateDelta {
			t.Errorf("step %d StateDelta: expected %d, got %d", i, expected.StateDelta, got.StateDelta)
		}
		if got.StateOp != expected.StateOp {
			t.Errorf("step %d StateOp: expected %q, got %q", i, expected.StateOp, got.StateOp)
		}

		t.Logf("Step %d (%s): all fields match", i, expected.EventType)
	}

	t.Log("New event types persistence round-trip integration test passed")
}

// ---------------------------------------------------------------------------
// Test 6: RLS tenant isolation.
//
// Enables Row-Level Security on workflow_instances, creates two tenants with
// different tenant_ids, inserts workflows for both, then verifies that a store
// scoped to tenant A only sees tenant A's workflows and tenant B's workflows
// are invisible (and vice versa).
// ---------------------------------------------------------------------------

// TestRLSTenantIsolation verifies that Row-Level Security correctly isolates
// workflow instances between tenants. This test must hit the actual store, not
// mock it -- it uses a real PostgreSQL database.
func TestRLSTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping RLS integration test in short mode")
	}

	db := testDB(t)
	defer db.Close()

	ctx := context.Background()

	// Ensure tenant_id column exists on workflow_instances. testDB creates the
	// column via ALTER TABLE ADD COLUMN IF NOT EXISTS, but a previous test that
	// used setupFullTestSchema may have dropped and recreated the table without
	// the column, so we add it here to be safe.
	if _, err := db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS tenant_id TEXT`); err != nil {
		t.Fatalf("add tenant_id column: %v", err)
	}

	// Enable RLS on workflow_instances.
	if _, err := db.Exec(`ALTER TABLE workflow_instances ENABLE ROW LEVEL SECURITY`); err != nil {
		t.Fatalf("enable RLS: %v", err)
	}

	// Create the tenant isolation policy. When the cleat.tenant_id session
	// variable is set (via set_config), the policy filters rows by tenant_id.
	// If the variable is not set, it falls back to the default tenant UUID.
	if _, err := db.Exec(`DROP POLICY IF EXISTS cleat_rls_test ON workflow_instances`); err != nil {
		t.Fatalf("drop existing policy: %v", err)
	}
	if _, err := db.Exec(`CREATE POLICY cleat_rls_test ON workflow_instances
		FOR ALL
		USING (tenant_id = COALESCE(current_setting('cleat.tenant_id', true), '00000000-0000-0000-0000-000000000000')::text)`); err != nil {
		t.Fatalf("create RLS policy: %v", err)
	}

	// Cleanup: disable RLS (DDL, not subject to RLS), drop the policy, then
	// delete test rows. Using t.Cleanup ensures this runs even on t.Fatalf.
	t.Cleanup(func() {
		db.Exec(`ALTER TABLE workflow_instances DISABLE ROW LEVEL SECURITY`)
		db.Exec(`DROP POLICY IF EXISTS cleat_rls_test ON workflow_instances`)
		db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'rls-test-%'`)
	})

	// Create two tenants with different UUIDs.
	tenantA := "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	tenantB := "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"

	runID_A := fmt.Sprintf("rls-test-a-%d", time.Now().UnixNano())
	runID_B := fmt.Sprintf("rls-test-b-%d", time.Now().UnixNano())

	// Insert workflow instances for both tenants using direct SQL. INSERT
	// bypasses the RLS USING clause, so this works regardless of the current
	// RLS session variable. Set next_wake_at in the past so ClaimWorkflows
	// immediately qualifies them.
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id, next_wake_at)
		VALUES ($1, 'wf-tenant-a', 1, 'ready', '{}', $2, now() - interval '1 hour')`,
		runID_A, tenantA); err != nil {
		t.Fatalf("insert tenant A workflow: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, tenant_id, next_wake_at)
		VALUES ($1, 'wf-tenant-b', 1, 'ready', '{}', $2, now() - interval '1 hour')`,
		runID_B, tenantB); err != nil {
		t.Fatalf("insert tenant B workflow: %v", err)
	}

	// ---- Test 1: Tenant A's store should only see tenant A's workflow ----
	storeA := NewPostgresStore(db).WithTenant(tenantA)
	wfsA, err := storeA.ClaimWorkflows(ctx, "worker-a", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows tenant A: %v", err)
	}

	if len(wfsA) != 1 {
		t.Errorf("tenant A: expected 1 workflow, got %d", len(wfsA))
	} else {
		if wfsA[0].ID != runID_A {
			t.Errorf("tenant A: expected workflow %q, got %q", runID_A, wfsA[0].ID)
		}
		if wfsA[0].TenantID != tenantA {
			t.Errorf("tenant A: expected tenant_id %q, got %q", tenantA, wfsA[0].TenantID)
		}
		// Verify tenant A did NOT see tenant B's workflow.
		for _, wf := range wfsA {
			if wf.ID == runID_B {
				t.Error("tenant A should not see tenant B's workflow")
			}
		}

		// Release tenant A's workflow so it doesn't affect the tenant B test.
		if err := storeA.ReleaseWorkflow(ctx, wfsA[0].ID, "worker-a", 0, time.Now()); err != nil {
			t.Fatalf("ReleaseWorkflow tenant A: %v", err)
		}
	}

	// ---- Test 2: Tenant B's store should only see tenant B's workflow ----
	storeB := NewPostgresStore(db).WithTenant(tenantB)
	wfsB, err := storeB.ClaimWorkflows(ctx, "worker-b", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows tenant B: %v", err)
	}

	if len(wfsB) != 1 {
		t.Errorf("tenant B: expected 1 workflow, got %d", len(wfsB))
	} else {
		if wfsB[0].ID != runID_B {
			t.Errorf("tenant B: expected workflow %q, got %q", runID_B, wfsB[0].ID)
		}
		if wfsB[0].TenantID != tenantB {
			t.Errorf("tenant B: expected tenant_id %q, got %q", tenantB, wfsB[0].TenantID)
		}
		// Verify tenant B did NOT see tenant A's workflow.
		for _, wf := range wfsB {
			if wf.ID == runID_A {
				t.Error("tenant B should not see tenant A's workflow")
			}
		}
	}

	t.Log("RLS tenant isolation test passed")
}


// TestIntegrationWorkflowMaxDuration verifies that the WithDefaultWorkflowTimeout
// option cancels execution when a workflow exceeds its wall-clock duration limit.
func TestIntegrationWorkflowMaxDuration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	db := testutil.TestDB(t, testutil.DialectPostgres)
	defer db.Close()
	testutil.SetupFullSchema(t, db, testutil.DialectPostgres)

	ctx := context.Background()
	runID := fmt.Sprintf("int-timeout-%d", time.Now().UnixNano())
	defName := "test-timeout-workflow"

	// Build WASM from testdata/basic (includes LongRunning export).
	wasmPath := buildTestWasm(t)
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	// Insert workflow definition.
	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points) VALUES ($1, $2, $3, $4)`,
		defName, 1, wasmBytes, `{long_running}`); err != nil {
		t.Fatalf("insert workflow_defs: %v", err)
	}

	// Insert a workflow instance in 'ready' status.
	// 500000 HostCall iterations takes ~5s wall-clock time, beyond the 1s timeout.
	input := `{"iterations":500000}`
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input) VALUES ($1, $2, $3, $4, $5)`,
		runID, defName, 1, "ready", input); err != nil {
		t.Fatalf("insert workflow_instances: %v", err)
	}

	defer func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
		db.Exec(`DELETE FROM workflow_defs WHERE name = $1`, defName)
	}()

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// Create engine with a 1-second timeout (well below 5s execution time).
	engine := NewEngine(rt, &mockCaller{}, WithDefaultWorkflowTimeout(1*time.Second))

	_, _, _, _, _, err = engine.Execute(ctx, wasmBytes, "long_running", json.RawMessage(input))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got: %v", err)
	}

	t.Log("Workflow max duration integration test passed")
}
