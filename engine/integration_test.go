package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine/testutil"
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
	input := `{"userID":"user-42","cart":[{"sku":"SKU-001","quantity":2}]}`

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
	engine := NewEngine(rt, caller, withWasmtimeBackend(t))

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
		if normalizeJSON(rec.Response) != normalizeJSON(loadedHistory[i].Response) {
			t.Errorf("step %d Response: expected %q, got %q", i, rec.Response, loadedHistory[i].Response)
		}
	}

	// ---- Step 5: Replay from the loaded event history ----
	replayCaller := &mockCaller{}
	engine2 := NewEngine(rt, replayCaller, withWasmtimeBackend(t))
	result2, _, suspended2, _, _, err := engine2.Replay(ctx, wasmBytes, "place_order", json.RawMessage(input), loadedHistory)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if suspended2 != nil {
		t.Fatalf("unexpected suspension during replay: %v", suspended2.Reason)
	}

	// ---- Step 6: Verify replay produced the same result ----
	if normalizeJSON(result) != normalizeJSON(result2) {
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
	input := `{"orderID":"ord-123"}`

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
	engine := NewEngine(rt, caller, withWasmtimeBackend(t))

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
	engine2 := NewEngine(rt, replayCaller, withWasmtimeBackend(t))
	result2, _, _, _, _, err := engine2.Replay(ctx, wasmBytes, "cancel_order", json.RawMessage(input), loadedHistory)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if normalizeJSON(result) != normalizeJSON(result2) {
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
	input := `{"userID":"user-42","cart":[{"sku":"ABC-123","quantity":2}]}`

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
	engine := NewEngine(rt, caller, withWasmtimeBackend(t))

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
	//
	// This step exercises "atomic claim + delete", so it must call
	// PollAndClaimSignal, not PollSignal. Those are two distinct
	// SignalStore methods with two distinct, documented contracts:
	// PollSignal "checks for a delivered signal" (a plain, repeatable read
	// -- see TestPollSignal_NonDestructive in
	// store_test_groups_6_10_test.go and PostgresStore.PollSignal's doc
	// comment in store_signals.go) while PollAndClaimSignal "atomically
	// checks for AND CLAIMS" it (consumes it). This test used to call
	// PollSignal for both steps 3 and 4 and assert the consuming behavior
	// on it, which happened to pass only because PostgresStore.PollSignal
	// used to be implemented as a bug-for-bug copy of PollAndClaimSignal.
	payload, found, err := store.PollAndClaimSignal(ctx, runID, "payment_confirmed")
	if err != nil {
		t.Fatalf("PollAndClaimSignal: %v", err)
	}
	if !found {
		t.Error("expected PollAndClaimSignal to find the delivered signal")
	}
	if normalizeJSON(payload) != normalizeJSON(signalPayload) {
		t.Errorf("signal payload mismatch: expected=%q, got=%q", signalPayload, payload)
	}
	t.Logf("Signal delivered and polled successfully: %s", payload)

	// ---- Step 4: Claiming the same signal again returns not-found (consumed) ----
	_, found, err = store.PollAndClaimSignal(ctx, runID, "payment_confirmed")
	if err != nil {
		t.Fatalf("second PollAndClaimSignal: %v", err)
	}
	if found {
		t.Error("expected second PollAndClaimSignal to return not-found (signal was consumed)")
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
	engine2 := NewEngine(rt, replayCaller, withWasmtimeBackend(t))
	result2, _, _, _, _, err := engine2.Replay(ctx, wasmBytes, "place_order", json.RawMessage(input), loadedHistory)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if normalizeJSON(result) != normalizeJSON(result2) {
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
	input := `{"userID":"user-42","cart":[{"sku":"ABC-123","quantity":2}]}`

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
	engine := NewEngine(rt, caller, withWasmtimeBackend(t))
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
	engine2 := NewEngine(rt, replayCaller, withWasmtimeBackend(t))
	divResult, _, _, _, _, err := engine2.Replay(ctx, wasmBytes, "place_order", json.RawMessage(input), loadedHistory)
	// With wasmtime, divergence is detected within the workflow and returned
	// as the result (the engine does not surface it as an error).
	if err == nil && !strings.Contains(divResult, "replay divergence") {
		t.Error("expected divergence error from tampered history, got nil")
	}
	if err != nil {
		t.Logf("Divergence correctly detected (engine error): %v", err)
	}
	if strings.Contains(divResult, "replay divergence") {
		t.Logf("Divergence correctly detected (workflow result): %s", divResult)
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
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	}()

	store := NewPostgresStore(db)

	// event_history.workflow_id is a real foreign key into
	// workflow_instances(id) (migrations/postgres/001_schema.sql), so a real
	// instance must exist before appending events for it -- unlike the old
	// hand-maintained test schema in engine/testutil/schema.go, which used to
	// explicitly drop this FK "so tests insert events without workflow
	// instances".
	def := &WorkflowDef{
		Name:       "int-new-events-workflow",
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := store.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	if _, _, err := store.StartNewRun(ctx, runID, "int-new-events-workflow", 1, json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}

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
// workflow instances between tenants. This test must hit the actual store,
// not mock it -- it uses a real PostgreSQL database.
//
// This used to build its own ad hoc RLS policy at runtime
// (`CREATE POLICY cleat_rls_test ...`) over the plain superuser/owner
// connection that testDB(t) returns. That connection bypasses RLS
// unconditionally -- Postgres never applies RLS to a superuser, and bypasses
// it for the owning role too unless FORCE ROW LEVEL SECURITY is set -- so
// the test always saw every row regardless of the policy it had just
// created, and "tenant A: expected 1 workflow, got 2" was the inevitable
// result the moment CLEAT_TEST_DB pointed at a real (superuser) Postgres
// instead of being skipped. It also hand-duplicated a policy that the real
// migrations/postgres/001_schema.sql now already defines (tenant_isolation_instances).
//
// Fixed to do what the test name promises: exercise the *real* schema
// (including its real RLS policy) through a connection that cannot bypass
// RLS -- see testutil.OpenPostgresRLSTestDB -- and to create its fixture
// data through the store (which sets the `cleat.tenant_id` session variable
// the real policy checks), not through raw SQL that bypasses the store layer
// entirely.
func TestRLSTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping RLS integration test in short mode")
	}

	adminDB := testutil.TestDB(t, testutil.DialectPostgres)
	defer adminDB.Close()
	testutil.SetupFullSchema(t, adminDB, testutil.DialectPostgres)
	testutil.CleanupPostgresTestData(t, adminDB)

	appDB := testutil.OpenPostgresRLSTestDB(t, adminDB)
	defer appDB.Close()
	// Deferred after appDB.Close() so it runs first (defers are LIFO) --
	// i.e. before either connection closes, not after both.
	defer testutil.CleanupPostgresTestData(t, adminDB)

	ctx := context.Background()

	// Create two tenants with different UUIDs.
	tenantA := "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	tenantB := "bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb"

	storeA := NewPostgresStore(appDB).WithTenant(tenantA)
	storeB := NewPostgresStore(appDB).WithTenant(tenantB)

	def := &WorkflowDef{
		Name:       "rls-test-workflow",
		Version:    1,
		WASMBytes:  []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1,
		MinVersion: 1,
	}
	if err := storeA.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	// B needs its own definition of the same name: since D7 the FK on
	// workflow_instances carries tenant_id (IMPROVEMENT-PLAN 3.77). That two
	// tenants can hold one name at all is the change; before it, this second
	// deploy would have been refused.
	if err := storeB.DeployWorkflowDef(ctx, def); err != nil {
		t.Fatalf("DeployWorkflowDef(B): %v", err)
	}

	runIDA := fmt.Sprintf("rls-test-a-%d", time.Now().UnixNano())
	runIDB := fmt.Sprintf("rls-test-b-%d", time.Now().UnixNano())

	if _, _, err := storeA.StartNewRun(ctx, runIDA, "rls-test-workflow", 1, json.RawMessage(`{}`), "", tenantA, 0); err != nil {
		t.Fatalf("StartNewRun tenant A: %v", err)
	}
	if _, _, err := storeB.StartNewRun(ctx, runIDB, "rls-test-workflow", 1, json.RawMessage(`{}`), "", tenantB, 0); err != nil {
		t.Fatalf("StartNewRun tenant B: %v", err)
	}

	// ---- Test 1: Tenant A's store should only see tenant A's workflow ----
	wfsA, err := storeA.ClaimWorkflows(ctx, "worker-a", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows tenant A: %v", err)
	}

	if len(wfsA) != 1 {
		t.Errorf("tenant A: expected 1 workflow, got %d", len(wfsA))
	} else {
		if wfsA[0].ID != runIDA {
			t.Errorf("tenant A: expected workflow %q, got %q", runIDA, wfsA[0].ID)
		}
		if wfsA[0].TenantID != tenantA {
			t.Errorf("tenant A: expected tenant_id %q, got %q", tenantA, wfsA[0].TenantID)
		}
		// Verify tenant A did NOT see tenant B's workflow.
		for _, wf := range wfsA {
			if wf.ID == runIDB {
				t.Error("tenant A should not see tenant B's workflow")
			}
		}

		// Release tenant A's workflow so it doesn't affect the tenant B test.
		if err := storeA.ReleaseWorkflow(ctx, wfsA[0].ID, "worker-a", 0, time.Now()); err != nil {
			t.Fatalf("ReleaseWorkflow tenant A: %v", err)
		}
	}

	// ---- Test 2: Tenant B's store should only see tenant B's workflow ----
	wfsB, err := storeB.ClaimWorkflows(ctx, "worker-b", 10)
	if err != nil {
		t.Fatalf("ClaimWorkflows tenant B: %v", err)
	}

	if len(wfsB) != 1 {
		t.Errorf("tenant B: expected 1 workflow, got %d", len(wfsB))
	} else {
		if wfsB[0].ID != runIDB {
			t.Errorf("tenant B: expected workflow %q, got %q", runIDB, wfsB[0].ID)
		}
		if wfsB[0].TenantID != tenantB {
			t.Errorf("tenant B: expected tenant_id %q, got %q", tenantB, wfsB[0].TenantID)
		}
		// Verify tenant B did NOT see tenant A's workflow.
		for _, wf := range wfsB {
			if wf.ID == runIDA {
				t.Error("tenant B should not see tenant A's workflow")
			}
		}
	}

	// Only claim success, not merely "the test function reached the end" --
	// t.Failed() reflects every t.Error/t.Errorf recorded above. Logging
	// unconditionally here was the exact defect class this investigation was
	// looking for: integration_test.go used to print "RLS tenant isolation
	// test passed" even on a run that had just recorded two isolation
	// failures above.
	if !t.Failed() {
		t.Log("RLS tenant isolation test passed")
	}
}

// TestIntegrationWorkflowMaxDuration verifies that WithDefaultWorkflowTimeout
// stops a workflow that exceeds its wall-clock duration limit.
//
// This test was skipped for a long time because it never exercised its subject,
// and the two reasons are worth keeping written down.
//
// The workload was testdata/basic's LongRunning, which looped on
// h.DurableCall("noop", "", ""). The empty operation name was rejected by the
// host on the first iteration, so the loop body never ran and the call returned
// in ~200ms regardless of the iteration count. Retuning the count could not fix
// that. It is fixed at the source now (see IMPROVEMENT-PLAN.md 2.10), but
// LongRunning is still the wrong workload here: each durable call records an
// event costing ~2.9 KB of host memory, so spinning one for a whole second
// means ~170k calls and ~500 MB of heap.
//
// It also once passed on CI for a reason unrelated to its subject: `go test
// -race` slowed *instantiation* past the 1s budget, the trap reported 999.9ms,
// and the assertion was satisfied without the guest ever running long. The
// budget covers instantiation as well as execution, so that will happen to any
// limit tight enough for startup to consume it.
//
// So: the workload is testdata/spin, a pure arithmetic loop that allocates
// nothing and never enters the host, and the limit is set well clear of
// instantiation. The assertions below check that the fence fired *and* that the
// guest actually ran until it did -- an early return is the failure this test
// exists to catch.
func TestIntegrationWorkflowMaxDuration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// No database: Execute takes the WASM bytes directly, so the workflow_defs
	// and workflow_instances rows this test used to insert were never read.
	// Dropping them takes the test out of the "needs PostgreSQL" class.
	ctx := context.Background()

	wasmPath := buildFixtureWasm(t, "spin")
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("read WASM: %v", err)
	}

	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	defer rt.Close(ctx)

	// 2s rather than 1s so that instantiation (~175ms here, more under -race)
	// is a small fraction of the budget and the guest is certain to get a long
	// spin in. Iterations are set to something no machine finishes: measured at
	// ~5.5e8 iterations/sec, 1e11 would take roughly three minutes.
	const limit = 2 * time.Second
	const iterations = 100000000000

	engine := NewEngine(rt, &mockCaller{}, withWasmtimeBackend(t), WithDefaultWorkflowTimeout(limit))

	start := time.Now()
	res, _, _, _, _, err := engine.Execute(ctx, wasmBytes, "spin",
		json.RawMessage(fmt.Sprintf(`{"iterations":%d}`, iterations)))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected the execution-time limit to fire, got nil after %v (result %s)",
			elapsed, res)
	}

	// Identify the trap precisely rather than accepting any error. Matching on
	// loose substrings is how the previous version of this assertion accepted
	// "error 1" -- which is not a timeout signal at all -- as evidence of a
	// timeout.
	if !isExecutionInterruptTrap(err) {
		t.Fatalf("expected a wasmtime interrupt trap (epoch interruption), got: %v", err)
	}
	if !strings.Contains(err.Error(), "execution time limit exceeded") {
		t.Errorf("trap was an interrupt but the error does not name the limit: %v", err)
	}

	// The assertion that catches the original defect. When the workload
	// returned early -- LongRunning bailing out on its first iteration --
	// elapsed was ~200ms. Requiring the guest to have run for essentially the
	// whole budget is what distinguishes "the fence stopped a long workload"
	// from "the workload stopped by itself".
	//
	// The lower bound allows two epoch ticks of slack because the deadline is
	// expressed in whole ticks: configureStore computes
	// uint64(timeout / epochTickInterval), which truncates, so a 2s budget is
	// enforced at 1.95s. One tick for that, one for scheduling jitter.
	// 100ms = two 50ms epoch ticks. Spelled as a literal because
	// epochTickInterval lives behind //go:build cgo and this file compiles
	// without it; TestEpochTickIntervalMatchesDurationTestSlack pins the two
	// together so the literal cannot drift.
	const tickSlack = 100 * time.Millisecond
	minElapsed := limit - tickSlack
	if elapsed < minElapsed {
		t.Errorf("execution stopped after %v, before the %v limit could plausibly "+
			"have fired (expected at least %v). The guest returned early instead "+
			"of running long -- the fence is not what stopped it.",
			elapsed, limit, minElapsed)
	}

	// And the other direction: the fence must actually cut execution off,
	// rather than the workload happening to finish.
	if maxElapsed := limit + 10*time.Second; elapsed > maxElapsed {
		t.Errorf("execution took %v, well past the %v limit; the fence did not "+
			"interrupt promptly", elapsed, limit)
	}
}

// normalizeJSON unmarshals and re-marshals a JSON string to produce a
// canonical form for comparison, ignoring key ordering and whitespace.
func normalizeJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	compacted, _ := json.Marshal(v)
	return string(compacted)
}
