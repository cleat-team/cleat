package integrity

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	host "github.com/rcownie/cleat/internal/host"

	_ "github.com/lib/pq"
)

// testDB returns a database connection for integrity tests.
// It skips the test if no database is available or in short mode.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integrity test in short mode")
	}
	dsn := os.Getenv("CLEAT_TEST_DB")
	if dsn == "" {
		dsn = "postgres://localhost:5432/cleat?sslmode=disable"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("Skipping: no database available: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping: cannot ping database: %v", err)
	}

	// Clean up any leftover test data from previous runs.
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'int-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'int-%'`)

	// Ensure full schema exists.
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_defs (
		name TEXT NOT NULL, version INTEGER NOT NULL,
		wasm_bytes BYTEA NOT NULL, entry_points TEXT[] NOT NULL DEFAULT '{}',
		min_version INTEGER NOT NULL DEFAULT 0, namespace TEXT NOT NULL DEFAULT 'default',
		max_history_length INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (name, version))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_instances (
		id TEXT PRIMARY KEY, def_name TEXT NOT NULL, def_version INTEGER NOT NULL DEFAULT 1,
		status TEXT NOT NULL DEFAULT 'ready', input JSONB NOT NULL DEFAULT '{}',
		assigned_to TEXT, heartbeat_at TIMESTAMPTZ,
		next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(), completed_at TIMESTAMPTZ,
		result JSONB, error_msg TEXT, parent_workflow_id TEXT,
		namespace TEXT NOT NULL DEFAULT 'default', trace_id TEXT,
		query_state JSONB DEFAULT '{}', task_queue TEXT NOT NULL DEFAULT 'default',
		cancellation_requested BOOLEAN NOT NULL DEFAULT false,
		cancellation_reason TEXT, sticky_worker_id TEXT)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS event_history (
		workflow_id TEXT NOT NULL, step INTEGER NOT NULL,
		event_type TEXT NOT NULL DEFAULT 'call',
		service TEXT, operation TEXT, request JSONB, response JSONB, error TEXT,
		duration_ms BIGINT, signal_names TEXT, timeout_ms BIGINT,
		signal_name TEXT, signal_payload JSONB, defer_description TEXT,
		defer_id TEXT, child_name TEXT, child_input JSONB, run_id TEXT,
		new_input JSONB, plugin_name TEXT, plugin_func TEXT,
		plugin_input JSONB, plugin_output JSONB, plugin_error TEXT,
		promise_name TEXT, promise_id TEXT, promise_result TEXT, promise_error TEXT,
		payload JSONB,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (workflow_id, step))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_signals (
		workflow_id TEXT NOT NULL, signal_name TEXT NOT NULL,
		payload JSONB NOT NULL DEFAULT '{}',
		delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (workflow_id, signal_name))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_promises (
		workflow_id TEXT NOT NULL, promise_id TEXT NOT NULL,
		promise_name TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
		result JSONB, error_msg TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(), resolved_at TIMESTAMPTZ,
		PRIMARY KEY (workflow_id, promise_id))`)
	db.Exec(`CREATE TABLE IF NOT EXISTS workflow_schedules (
		name TEXT PRIMARY KEY, def_name TEXT NOT NULL,
		entry_point TEXT NOT NULL DEFAULT '', cron_expression TEXT NOT NULL,
		input JSONB NOT NULL DEFAULT '{}', enabled BOOLEAN NOT NULL default true,
		next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_run_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	db.Exec(`CREATE TABLE IF NOT EXISTS concurrency_keys (
		key_hash BYTEA PRIMARY KEY, key_text TEXT NOT NULL,
		workflow_id TEXT NOT NULL, acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at TIMESTAMPTZ NOT NULL)`)
	db.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	// Add columns added via schema migrations that may not be in minimal tables.
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compaction_state JSONB`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compacted_at TIMESTAMPTZ`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS compaction_step INTEGER`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS tenant_id TEXT`)
	db.Exec(`ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS sticky_worker_id TEXT`)
	db.Exec(`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS payload JSONB`)
	return db
}

// createTestWorkflow creates a test workflow instance and returns its ID.
func createTestWorkflow(t *testing.T, db *sql.DB, store *host.PostgresStore, ctx context.Context) string {
	t.Helper()
	runID := fmt.Sprintf("int-eh-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	})
	return runID
}

// TestEventHistoryConsistencyAfterFault inserts events, simulates a fault
// (duplicate insert), and verifies history remains consistent and readable.
func TestEventHistoryConsistencyAfterFault(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := host.NewPostgresStore(db)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert a batch of events.
	events := []host.EventRecord{
		{Step: 0, EventType: host.EventTypeCall, Service: "svc1", Op: "op1", Request: `{"a":1}`, Response: `{"ok":true}`},
		{Step: 1, EventType: host.EventTypeCall, Service: "svc1", Op: "op2", Request: `{"b":2}`, Response: `{"ok":true}`},
		{Step: 2, EventType: host.EventTypeSleep, DurationMs: 100},
	}

	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Simulate a fault: duplicate insert of the same batch.
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("duplicate append: %v", err)
	}

	// Simulate a partial write: insert a subset with overlapping steps.
	partial := []host.EventRecord{
		{Step: 1, EventType: host.EventTypeCall, Service: "svc1", Op: "op2", Request: `{"b":2}`, Response: `{"ok":true}`},
		{Step: 3, EventType: host.EventTypeCall, Service: "svc2", Op: "op3", Request: `{}`, Response: `{}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, partial); err != nil {
		t.Fatalf("partial append: %v", err)
	}

	// Load and verify history is consistent.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	// Should have exactly 4 unique events (steps 0, 1, 2, 3) — no duplicates.
	if len(history) != 4 {
		t.Errorf("expected 4 events after fault simulation, got %d", len(history))
	}

	// Verify step order.
	for i, ev := range history {
		if ev.Step != i {
			t.Errorf("step %d: expected Step=%d, got Step=%d", i, i, ev.Step)
		}
	}

	// Verify event content.
	if history[0].Service != "svc1" || history[0].Op != "op1" {
		t.Errorf("event 0: expected svc1/op1, got %s/%s", history[0].Service, history[0].Op)
	}
	if history[1].Service != "svc1" || history[1].Op != "op2" {
		t.Errorf("event 1: expected svc1/op2, got %s/%s", history[1].Service, history[1].Op)
	}
	if history[2].EventType != host.EventTypeSleep || history[2].DurationMs != 100 {
		t.Errorf("event 2: expected sleep/100, got %s/%d", history[2].EventType, history[2].DurationMs)
	}
}

// TestEventHistoryGaps verifies that gap detection works — if events are
// missing, LoadEventHistory returns exactly the events that exist.
func TestEventHistoryGaps(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := host.NewPostgresStore(db)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert events with gaps (steps 0, 2, 4 — missing 1 and 3).
	events := []host.EventRecord{
		{Step: 0, EventType: host.EventTypeCall, Service: "svc", Op: "step0", Request: `{}`, Response: `{}`},
		{Step: 2, EventType: host.EventTypeCall, Service: "svc", Op: "step2", Request: `{}`, Response: `{}`},
		{Step: 4, EventType: host.EventTypeCall, Service: "svc", Op: "step4", Request: `{}`, Response: `{}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Load history — should return exactly the events that exist, ordered by step.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	if len(history) != 3 {
		t.Errorf("expected 3 events (with gaps), got %d", len(history))
	}

	// Verify the returned steps are 0, 2, 4.
	expectedSteps := []int{0, 2, 4}
	for i, ev := range history {
		if ev.Step != expectedSteps[i] {
			t.Errorf("event %d: expected Step=%d, got Step=%d", i, expectedSteps[i], ev.Step)
		}
	}

	// Verify the gap is detectable by comparing step values.
	for i := 1; i < len(history); i++ {
		if history[i].Step-history[i-1].Step != 2 {
			t.Errorf("gap mismatch between step %d and %d: expected gap of 2", history[i-1].Step, history[i].Step)
		}
	}
}

// TestEventHistoryOrdering verifies events are returned in step order even when
// inserted out of order.
func TestEventHistoryOrdering(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := host.NewPostgresStore(db)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert events in reverse step order.
	events := []host.EventRecord{
		{Step: 4, EventType: host.EventTypeCall, Service: "svc", Op: "last", Request: `{}`, Response: `{}`},
		{Step: 2, EventType: host.EventTypeCall, Service: "svc", Op: "middle", Request: `{}`, Response: `{}`},
		{Step: 0, EventType: host.EventTypeCall, Service: "svc", Op: "first", Request: `{}`, Response: `{}`},
		{Step: 3, EventType: host.EventTypeCall, Service: "svc", Op: "third", Request: `{}`, Response: `{}`},
		{Step: 1, EventType: host.EventTypeCall, Service: "svc", Op: "second", Request: `{}`, Response: `{}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Load and verify order.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	if len(history) != 5 {
		t.Fatalf("expected 5 events, got %d", len(history))
	}

	expectedOps := []string{"first", "second", "middle", "third", "last"}
	for i, ev := range history {
		if ev.Step != i {
			t.Errorf("event %d: expected Step=%d, got Step=%d", i, i, ev.Step)
		}
		if ev.Op != expectedOps[i] {
			t.Errorf("event %d: expected Op=%q, got %q", i, expectedOps[i], ev.Op)
		}
	}
}

// TestEventHistoryLargePayload verifies events with large JSON payloads are
// stored and retrieved without truncation.
func TestEventHistoryLargePayload(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := host.NewPostgresStore(db)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Create a large JSON payload (~100KB).
	largeValue := strings.Repeat("x", 100*1024)
	largePayload := fmt.Sprintf(`{"data":"%s"}`, largeValue)

	events := []host.EventRecord{
		{Step: 0, EventType: host.EventTypeCall, Service: "svc", Op: "large-req", Request: largePayload, Response: `{}`},
		{Step: 1, EventType: host.EventTypeCall, Service: "svc", Op: "large-resp", Request: `{}`, Response: largePayload},
		{Step: 2, EventType: host.EventTypeCall, Service: "svc", Op: "both-large", Request: largePayload, Response: largePayload},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append large payload events: %v", err)
	}

	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	if len(history) != 3 {
		t.Fatalf("expected 3 events, got %d", len(history))
	}

	// Verify no truncation of large payloads.
	if history[0].Request != largePayload {
		t.Errorf("event 0 large request truncated: expected len=%d, got len=%d",
			len(largePayload), len(history[0].Request))
	}
	if history[1].Response != largePayload {
		t.Errorf("event 1 large response truncated: expected len=%d, got len=%d",
			len(largePayload), len(history[1].Response))
	}
	if history[2].Request != largePayload {
		t.Errorf("event 2 large request truncated: expected len=%d, got len=%d",
			len(largePayload), len(history[2].Request))
	}
	if history[2].Response != largePayload {
		t.Errorf("event 2 large response truncated: expected len=%d, got len=%d",
			len(largePayload), len(history[2].Response))
	}
}
