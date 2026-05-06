package host

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// setupFullTestSchema creates the complete database schema needed for full
// pipeline integration tests. It drops any existing tables (including those
// created by testDB's minimal schema) and recreates them with all columns
// used by PostgresStore.
func setupFullTestSchema(t *testing.T, db *sql.DB) {
	t.Helper()

	dropQueries := []string{
		`DROP TABLE IF EXISTS workflow_signals CASCADE`,
		`DROP TABLE IF EXISTS event_history CASCADE`,
		`DROP TABLE IF EXISTS workflow_instances CASCADE`,
		`DROP TABLE IF EXISTS workflow_defs CASCADE`,
		`DROP TABLE IF EXISTS workflow_schedules CASCADE`,
	}
	for _, q := range dropQueries {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("drop table: %v", err)
		}
	}

	createQueries := []string{
		`CREATE TABLE workflow_defs (
			name TEXT NOT NULL,
			version INTEGER NOT NULL,
			wasm_bytes BYTEA NOT NULL,
			entry_points TEXT[] NOT NULL DEFAULT '{}',
			min_version INTEGER NOT NULL DEFAULT 0,
			namespace TEXT NOT NULL DEFAULT 'default',
			max_history_length INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (name, version)
		)`,
		`CREATE TABLE workflow_instances (
			id TEXT PRIMARY KEY,
			def_name TEXT NOT NULL,
			def_version INTEGER NOT NULL DEFAULT 1,
			status TEXT NOT NULL DEFAULT 'ready',
			input JSONB NOT NULL DEFAULT '{}'::jsonb,
			assigned_to TEXT,
			heartbeat_at TIMESTAMPTZ,
			next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			completed_at TIMESTAMPTZ,
			result JSONB,
			error_msg TEXT,
			parent_workflow_id TEXT,
			namespace TEXT NOT NULL DEFAULT 'default',
			trace_id TEXT,
			query_state JSONB DEFAULT '{}'::jsonb,
			task_queue TEXT NOT NULL DEFAULT 'default',
			cancellation_requested BOOLEAN NOT NULL DEFAULT false,
			cancellation_reason TEXT
		)`,
		`CREATE TABLE event_history (
			workflow_id TEXT NOT NULL,
			step INTEGER NOT NULL,
			event_type TEXT NOT NULL DEFAULT 'call',
			service TEXT,
			operation TEXT,
			request JSONB,
			response JSONB,
			error TEXT,
			duration_ms BIGINT,
			signal_names TEXT,
			timeout_ms BIGINT,
			signal_name TEXT,
			signal_payload JSONB,
			defer_description TEXT,
			defer_id TEXT,
			child_name TEXT,
			child_input JSONB,
			run_id TEXT,
			new_input JSONB,
			plugin_name TEXT,
			plugin_func TEXT,
			plugin_input JSONB,
			plugin_output JSONB,
			plugin_error TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (workflow_id, step)
		)`,
		`CREATE TABLE workflow_signals (
			workflow_id TEXT NOT NULL,
			signal_name TEXT NOT NULL,
			payload JSONB NOT NULL DEFAULT '{}'::jsonb,
			delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (workflow_id, signal_name)
		)`,
	}
	for _, q := range createQueries {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 1: Full pipeline — real DB + WASM compile + Engine execute + event
// recording + event loading + replay with zero real service calls.
// ---------------------------------------------------------------------------

// TestIntegrationFullPipeline exercises the complete durable execution path:
// real PostgreSQL, WASM compilation, Engine execution, event persistence via
// AppendEventHistoryBatch, event loading via LoadEventHistory, and replay
// with verified zero real service calls.
func TestIntegrationFullPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	db := testDB(t)
	defer db.Close()
	setupFullTestSchema(t, db)

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
	rt, err := NewRuntime(ctx)
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

	db := testDB(t)
	defer db.Close()
	setupFullTestSchema(t, db)

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

	rt, err := NewRuntime(ctx)
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

	db := testDB(t)
	defer db.Close()
	setupFullTestSchema(t, db)

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

	rt, err := NewRuntime(ctx)
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

	db := testDB(t)
	defer db.Close()
	setupFullTestSchema(t, db)

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

	rt, err := NewRuntime(ctx)
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
