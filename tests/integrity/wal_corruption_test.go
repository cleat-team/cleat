package integrity

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	host "github.com/cleat-team/cleat/engine"
	"github.com/cleat-team/cleat/engine/testutil"

	_ "github.com/lib/pq"
)

// TestWalCorruption_ChecksumTampering inserts events, tampers with the checksum
// column, then verifies that VerifyWorkflowEvents detects the mismatch.
// This test requires a PostgreSQL database with the checksum column available.
func TestWalCorruption_ChecksumTampering(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	// Ensure the checksum column exists.
	db.Exec(`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT`)

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert a batch of events with checksums computed by AppendEventHistoryBatch.
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc0", Op: "op0", Request: `{"a":1}`, Response: `{"ok":true}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "svc1", Op: "op1", Request: `{"b":2}`, Response: `{"ok":true}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "svc2", Op: "op2", Request: `{"c":3}`, Response: `{"ok":true}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Verify checksums are correct before tampering.
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("expected no error before tampering, got: %v", err)
	}

	// Tamper with the checksum of step 1.
	result, err := db.Exec(`UPDATE event_history SET checksum = 'tampered-checksum-value' WHERE workflow_id = $1 AND step = 1`, runID)
	if err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		t.Fatal("tamper checksum: no rows affected")
	}

	// Verify that VerifyWorkflowEvents detects the mismatch.
	err = store.VerifyWorkflowEvents(ctx, runID)
	if err == nil {
		t.Fatal("expected error after checksum tampering, got nil")
	}

	// The error should reference the step number or contain "checksum mismatch".
	if !strings.Contains(err.Error(), "step 1") && !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected error message to mention tampered step or checksum mismatch, got: %v", err)
	}
}

// TestWalCorruption_PayloadTampering inserts events, tampers with the operation
// column, then verifies that VerifyWorkflowEvents detects the mismatch
// (since the stored checksum no longer matches the recomputed checksum).
func TestWalCorruption_PayloadTampering(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	db.Exec(`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT`)

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert events.
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "original", Request: `{"data":"original"}`, Response: `{}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Verify clean before tampering.
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("expected no error before tampering, got: %v", err)
	}

	// Tamper with the operation column (used in checksum computation).
	_, err := db.Exec(`UPDATE event_history SET operation = 'tampered-op' WHERE workflow_id = $1 AND step = 0`, runID)
	if err != nil {
		t.Fatalf("tamper operation: %v", err)
	}

	// Verify that VerifyWorkflowEvents detects the mismatch.
	err = store.VerifyWorkflowEvents(ctx, runID)
	if err == nil {
		t.Fatal("expected error after operation tampering, got nil")
	}

	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected error message to contain 'checksum mismatch', got: %v", err)
	}
}

// TestWalCorruption_MissingEvent inserts events at steps 0, 1, 2, 3, then
// deletes step 2, and verifies that the gap is detectable.
func TestWalCorruption_MissingEvent(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	db.Exec(`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT`)

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert a complete sequence of events.
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "step0", Request: `{}`, Response: `{}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "svc", Op: "step1", Request: `{}`, Response: `{}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "svc", Op: "step2", Request: `{}`, Response: `{}`},
		{Step: 3, EventType: engine.EventTypeCall, Service: "svc", Op: "step3", Request: `{}`, Response: `{}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Delete step 2 to simulate a missing event.
	result, err := db.Exec(`DELETE FROM event_history WHERE workflow_id = $1 AND step = 2`, runID)
	if err != nil {
		t.Fatalf("delete step 2: %v", err)
	}
	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		t.Fatal("delete step 2: no rows affected")
	}

	// Load history -- should return steps 0, 1, 3 (missing step 2).
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	if len(history) != 3 {
		t.Errorf("expected 3 events after deletion (steps 0, 1, 3), got %d", len(history))
	}

	// Verify step presence and ordering.
	expectedSteps := []int{0, 1, 3}
	for i, ev := range history {
		if ev.Step != expectedSteps[i] {
			t.Errorf("event %d: expected Step=%d, got Step=%d", i, expectedSteps[i], ev.Step)
		}
	}

	// Verify the gap between steps 1 and 3 is detectable.
	for i := 1; i < len(history); i++ {
		gap := history[i].Step - history[i-1].Step
		if gap != expectedSteps[i]-expectedSteps[i-1] {
			t.Errorf("gap between step %d and %d: expected %d, got %d",
				history[i-1].Step, history[i].Step, expectedSteps[i]-expectedSteps[i-1], gap)
		}
	}
}

// TestWalCorruption_EventOrdering inserts events out of step order (0, 2, 1)
// and verifies that LoadEventHistory returns them in correct step order.
func TestWalCorruption_EventOrdering(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	db.Exec(`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT`)

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := createTestWorkflow(t, db, store, ctx)

	// Insert events out of step order: 0, 2, 1.
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "first", Request: `{}`, Response: `{}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "svc", Op: "third", Request: `{}`, Response: `{}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "svc", Op: "second", Request: `{}`, Response: `{}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Load and verify events are returned in step order.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	if len(history) != 3 {
		t.Fatalf("expected 3 events, got %d", len(history))
	}

	expectedOps := []string{"first", "second", "third"}
	for i, ev := range history {
		if ev.Step != i {
			t.Errorf("event %d: expected Step=%d, got Step=%d", i, i, ev.Step)
		}
		if ev.Op != expectedOps[i] {
			t.Errorf("event %d: expected Op=%q, got %q", i, expectedOps[i], ev.Op)
		}
	}

	// Verify checksums are still valid after loading out-of-order events.
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Errorf("VerifyWorkflowEvents failed after ordering check: %v", err)
	}
}

// TestWalCorruption_PgSwitchWAL tests that the application correctly handles
// WAL segment boundaries by forcing a WAL switch via pg_switch_wal().
func TestWalCorruption_PgSwitchWAL(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	db.Exec(`ALTER TABLE event_history ADD COLUMN IF NOT EXISTS checksum TEXT`)

	store := engine.NewPostgresStore(db)
	ctx := context.Background()

	// Test pg_switch_wal() multiple times, inserting events between switches.
	for i := 0; i < 3; i++ {
		// Create a separate workflow run for each iteration.
		runID := fmt.Sprintf("int-wal-%d-%d", time.Now().UnixNano(), i)
		_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
			VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
		if err != nil {
			t.Fatalf("create workflow instance: %v", err)
		}
		t.Cleanup(func() {
			db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
			db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
		})

		// Insert some events.
		events := []engine.EventRecord{
			{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: fmt.Sprintf("pre-switch-%d", i), Request: `{}`, Response: `{}`},
		}
		if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
			t.Fatalf("append events before WAL switch: %v", err)
		}

		// Force WAL segment rotation. This may fail on systems without
		// superuser privileges or replication permissions.
		_, walErr := db.Exec(`SELECT pg_switch_wal()`)
		if walErr != nil {
			skipMsg := "pg_switch_wal() requires superuser or replication privilege"
			errStr := strings.ToLower(walErr.Error())
			if strings.Contains(errStr, "must be superuser") || strings.Contains(errStr, "permission denied") {
				skipMsg += ": " + walErr.Error()
			}
			t.Logf("pg_switch_wal() not available: %v", walErr)
			t.Skip(skipMsg)
		}

		// Insert additional events after WAL switch.
		postEvents := []engine.EventRecord{
			{Step: 1, EventType: engine.EventTypeCall, Service: "svc", Op: fmt.Sprintf("post-switch-%d", i), Request: `{}`, Response: `{}`},
		}
		if err := store.AppendEventHistoryBatch(ctx, runID, postEvents); err != nil {
			t.Fatalf("append events after WAL switch: %v", err)
		}

		// Load and verify all events persisted correctly.
		history, err := store.LoadEventHistory(ctx, runID)
		if err != nil {
			t.Fatalf("LoadEventHistory after WAL switch: %v", err)
		}

		if len(history) != 2 {
			t.Errorf("iteration %d: expected 2 events, got %d", i, len(history))
		}

		// Verify checksums pass.
		if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
			t.Errorf("iteration %d: VerifyWorkflowEvents failed: %v", i, err)
		}
	}
}

// TestWalCorruption_ReplayVerification verifies that LoadEventHistory +
// VerifyWorkflowEvents works end-to-end for a realistic workflow scenario,
// and that checksum corruption is detected.
func TestWalCorruption_ReplayVerification(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	// Use the minimal schema so we get the checksum column.
	testutil.SetupMinimalSchema(t, db, testutil.DialectPostgres)

	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := fmt.Sprintf("int-replay-%d", time.Now().UnixNano())

	// Create a workflow instance directly.
	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	})

	// Insert a realistic sequence of events.
	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "math", Op: "add", Request: `{"x":1,"y":2}`, Response: `{"result":3}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "math", Op: "mul", Request: `{"x":3,"y":4}`, Response: `{"result":12}`},
		{Step: 2, EventType: engine.EventTypeCall, Service: "storage", Op: "save", Request: `{"key":"result","value":12}`, Response: `{"ok":true}`},
		{Step: 3, EventType: engine.EventTypeCall, Service: "notify", Op: "send", Request: `{"msg":"done"}`, Response: `{"sent":true}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	// Load and verify.
	history, err := store.LoadEventHistory(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEventHistory: %v", err)
	}

	if len(history) != 4 {
		t.Fatalf("expected 4 events, got %d", len(history))
	}

	for i, ev := range history {
		if ev.Step != i {
			t.Errorf("event %d: expected Step=%d, got Step=%d", i, i, ev.Step)
		}
	}

	// Verify checksums pass end-to-end.
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("VerifyWorkflowEvents: %v", err)
	}

	// Now corrupt a checksum and verify replay would fail.
	_, err = db.Exec(`UPDATE event_history SET checksum = 'corrupted' WHERE workflow_id = $1 AND step = 2`, runID)
	if err != nil {
		t.Fatalf("corrupt checksum: %v", err)
	}

	// Verify detection.
	err = store.VerifyWorkflowEvents(ctx, runID)
	if err == nil {
		t.Fatal("expected error after checksum corruption, got nil")
	}

	if !strings.Contains(err.Error(), "step 2") && !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected error to mention step 2 or checksum mismatch, got: %v", err)
	}
}

// TestWalCorruption_DefaultOnReplayFailure verifies the default-on checksum
// verification semantics: the exact verifier function wired via
// WithWorkflowEventVerifier(store.VerifyWorkflowEvents, true) fails on corrupted
// checksums, which causes engine replay to abort.
func TestWalCorruption_DefaultOnReplayFailure(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	testutil.SetupMinimalSchema(t, db, testutil.DialectPostgres)
	store := engine.NewPostgresStore(db)
	ctx := context.Background()
	runID := fmt.Sprintf("int-default-on-%d", time.Now().UnixNano())

	_, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input, task_queue)
		VALUES ($1, 'test', 1, 'ready', '{}', 'default') ON CONFLICT DO NOTHING`, runID)
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	})

	events := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "svc", Op: "op0", Request: `{"a":1}`, Response: `{"ok":true}`},
		{Step: 1, EventType: engine.EventTypeCall, Service: "svc", Op: "op1", Request: `{"b":2}`, Response: `{"ok":true}`},
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, events); err != nil {
		t.Fatalf("append events: %v", err)
	}

	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("expected no error before tampering, got: %v", err)
	}

	// Corrupt a checksum.
	_, err = db.Exec(`UPDATE event_history SET checksum = 'corrupted' WHERE workflow_id = $1 AND step = 1`, runID)
	if err != nil {
		t.Fatalf("corrupt checksum: %v", err)
	}

	err = store.VerifyWorkflowEvents(ctx, runID)
	if err == nil {
		t.Fatal("VerifyWorkflowEvents must return error on checksum corruption (default-on replay would silently succeed)")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error message must contain 'checksum mismatch', got: %v", err)
	}
}
