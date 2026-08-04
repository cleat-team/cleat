package integrity

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"

	_ "github.com/lib/pq"
)

// suiteQueue keeps this suite's workflows off the queues the other tests/
// suites use.
//
// Without it every DB-backed suite inserted onto "default" and constructed its
// store with no queue list, which also polls "default". Go runs distinct
// packages in parallel and they all point at CLEAT_TEST_DB, so
// `go test ./tests/integrity/... ./tests/upgrade/... ./tests/scale/...`
// had tests/scale claiming tests/integrity's workflows out from under it:
// 17 failures, and every one of them passes when the suites are run one at a
// time. ClaimWorkflows filters on `task_queue = ANY($2)`, so giving each suite
// its own queue is the whole fix. IMPROVEMENT-PLAN 2.39.
const suiteQueue = "queue-integrity-tests"

// testDB returns a database connection for integrity tests.
//
// The schema comes from engine/testutil, which builds it from
// migrations/postgres/. It is deliberately not built here: the previous version
// of this helper created every table itself with CREATE TABLE IF NOT EXISTS,
// and the workflow_instances it invented had no foreign key to workflow_defs.
// Against a real migrated database that difference failed 22 of the 30 tests in
// this package on workflow_instances_def_name_def_version_fkey. Nobody found
// out, because the suite was in UNWIRED_SUITES (see
// scripts/check-ci-package-coverage.sh) and no job ran it.
//
// testutil.TestDB also fails, rather than skips, when CLEAT_TEST_DB is set but
// unreachable -- so a database that stops arriving empties this job loudly
// instead of quietly.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.TestDB(t, testutil.DialectPostgres)

	// Almost every test in this package inserts a workflow_instances row with
	// def_name='test', def_version=1, and the foreign key requires the
	// definition to exist first. Seeded once here rather than at each of the
	// sixteen insert sites.
	if _, err := db.Exec(`INSERT INTO workflow_defs (name, version, wasm_bytes, entry_points)
		VALUES ('test', 1, '\x00', '{}') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("seed workflow_defs(test, 1): %v", err)
	}

	// Clean up any leftover test data from previous runs. Children first: the
	// same foreign keys apply to deletes.
	db.Exec(`DELETE FROM event_history WHERE workflow_id LIKE 'int-%'`)
	db.Exec(`DELETE FROM workflow_instances WHERE id LIKE 'int-%'`)

	return db
}

// createTestWorkflow creates a test workflow instance and returns its ID.
func createTestWorkflow(t *testing.T, db *sql.DB, store *engine.PostgresStore, ctx context.Context) string {
	t.Helper()
	runID := fmt.Sprintf("int-eh-%d", time.Now().UnixNano())
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', '`+suiteQueue+`') ON CONFLICT DO NOTHING`, runID)
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

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert a batch of events.
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc1", Op: "op1", Request: `{"a":1}`, Response: `{"ok":true}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "svc1", Op: "op2", Request: `{"b":2}`, Response: `{"ok":true}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "svc1", Op: "op3", Request: `{"c":3}`, Response: `{"ok":true}`},
	}

	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Simulate a fault: duplicate insert of the same batch.
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("duplicate append: %v", err)
	}

	// Simulate a partial write: insert a subset with overlapping steps.
	partial := []engine.EventRecord{
		{Step: 1, EventType: engine.EventTypeCall, Service: "svc1", Op: "op2", Request: `{"b":2}`, Response: `{"ok":true}`},
		{Step: 3, EventType: engine.EventTypeCall, Service: "svc2", Op: "op3", Request: `{}`, Response: `{}`},
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
	if history[2].Service != "svc1" || history[2].Op != "op3" {
		t.Errorf("event 2: expected svc1/op3, got %s/%s", history[2].Service, history[2].Op)
	}
}

// TestEventHistoryGaps verifies that gap detection works — if events are
// missing, LoadEventHistory returns exactly the events that exist.
func TestEventHistoryGaps(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert events with gaps (steps 0, 2, 4 — missing 1 and 3).
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "step0", Request: `{}`, Response: `{}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "svc", Op: "step2", Request: `{}`, Response: `{}`},
		{Step: 4, EventType: engine.EventTypeCall, Service: "svc", Op: "step4", Request: `{}`, Response: `{}`},
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

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert events in reverse step order.
	events := []engine.EventRecord{
		{Step: 4, EventType: engine.EventTypeCall, Service: "svc", Op: "last", Request: `{}`, Response: `{}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "svc", Op: "middle", Request: `{}`, Response: `{}`},
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "first", Request: `{}`, Response: `{}`},
		{Step: 3, EventType: engine.EventTypeCall, Service: "svc", Op: "third", Request: `{}`, Response: `{}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "svc", Op: "second", Request: `{}`, Response: `{}`},
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

	store := engine.NewPostgresStore(db, suiteQueue)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Create a large JSON payload (~100KB).
	largeValue := strings.Repeat("x", 100*1024)
	largePayload := fmt.Sprintf(`{"data":"%s"}`, largeValue)

	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "large-req", Request: largePayload, Response: `{}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "svc", Op: "large-resp", Request: `{}`, Response: largePayload},
		{Step: 2, EventType: engine.EventTypeCall, Service: "svc", Op: "both-large", Request: largePayload, Response: largePayload},
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
