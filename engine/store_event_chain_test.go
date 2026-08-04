package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The checksum stored on an event chains from the previous event's checksum,
// and VerifyWorkflowEvents recomputes that chain over the whole history in step
// order. For the two to agree, the writer has to chain from whatever is already
// in the table -- not from the start of the slice it happens to have been
// handed.
//
// These tests use a real database on purpose. The existing
// VerifyWorkflowEvents tests in db_regression_test.go drive sqlmock, so they
// supply both the stored checksum and the row it is recomputed from, and can
// never disagree with each other. Nothing wrote a chain and then read it back.

// appendChainWorkflow inserts a workflow instance for the chain tests and
// returns its ID.
func appendChainWorkflow(t *testing.T, store *PostgresStore) string {
	t.Helper()
	deployFaultTestDef(t, store)
	runID := fmt.Sprintf("chain-%d", time.Now().UnixNano())
	db := store.db
	if _, err := db.Exec(`INSERT INTO workflow_instances (id, def_name, def_version, status, input)
		VALUES ($1, 'test', 1, 'ready', '{}') ON CONFLICT DO NOTHING`, runID); err != nil {
		t.Fatalf("insert test instance: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM event_history WHERE workflow_id = $1`, runID)
		db.Exec(`DELETE FROM workflow_instances WHERE id = $1`, runID)
	})
	return runID
}

func chainEvent(step int, op string) EventRecord {
	return EventRecord{
		Step:      step,
		EventType: EventTypeCall,
		Service:   "svc",
		Op:        op,
		Request:   `{}`,
		Response:  `{}`,
	}
}

// TestAppendEventHistory_ChainAcrossCalls is the production shape: a workflow
// that suspends and resumes writes its history in more than one call, because
// cmd/cleat-worker/setup.go finalizes each segment with
// `newEvents = resultHistory[len(history):]` -- only the events this segment
// produced. If the writer restarts the chain at each call, every workflow that
// ever sleeps or awaits a signal reports as corrupt.
func TestAppendEventHistory_ChainAcrossCalls(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	// Segment one.
	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{
		chainEvent(0, "first"),
	}); err != nil {
		t.Fatalf("append segment 1: %v", err)
	}
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("verify after segment 1: %v", err)
	}

	// Segment two, written by a separate call, as a resumed workflow does.
	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{
		chainEvent(1, "second"),
	}); err != nil {
		t.Fatalf("append segment 2: %v", err)
	}
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("verify after segment 2: %v\n"+
			"step 1 was written by a second call, so its checksum must chain "+
			"from step 0's stored checksum, not from an empty string", err)
	}

	// A third segment with several events at once: the first chains from the
	// table, the rest from each other.
	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{
		chainEvent(2, "third"),
		chainEvent(3, "fourth"),
	}); err != nil {
		t.Fatalf("append segment 3: %v", err)
	}
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("verify after segment 3: %v", err)
	}
}

// TestAppendEventHistory_ChainOutOfOrder covers a batch whose records are not
// in step order. VerifyWorkflowEvents reads ORDER BY step, so the writer has to
// chain in step order too -- slice order is not the chain order.
func TestAppendEventHistory_ChainOutOfOrder(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{
		chainEvent(0, "first"),
		chainEvent(2, "third"),
		chainEvent(1, "second"),
	}); err != nil {
		t.Fatalf("append out-of-order batch: %v", err)
	}
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("verify after out-of-order batch: %v", err)
	}
}

// TestAppendEventHistory_ChainDetectsTampering is the other half: the fix must
// not buy agreement by weakening what verification catches. Rewriting a
// persisted event still has to be detected, including through a chain that
// spans two calls.
func TestAppendEventHistory_ChainDetectsTampering(t *testing.T) {
	db := testDB(t)
	defer db.Close()

	store := NewPostgresStore(db)
	ctx := context.Background()
	runID := appendChainWorkflow(t, store)

	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{chainEvent(0, "first")}); err != nil {
		t.Fatalf("append step 0: %v", err)
	}
	if err := store.AppendEventHistoryBatch(ctx, runID, []EventRecord{chainEvent(1, "second")}); err != nil {
		t.Fatalf("append step 1: %v", err)
	}
	if err := store.VerifyWorkflowEvents(ctx, runID); err != nil {
		t.Fatalf("verify before tampering: %v", err)
	}

	// payload is what LoadEventHistory reads back (it overwrites the
	// per-column values), so it is what the checksum has to cover.
	if _, err := db.Exec(`UPDATE event_history
		SET payload = jsonb_set(payload::jsonb, '{operation}', '"tampered"')
		WHERE workflow_id = $1 AND step = 0`, runID); err != nil {
		t.Fatalf("tamper step 0: %v", err)
	}

	err := store.VerifyWorkflowEvents(ctx, runID)
	if err == nil {
		t.Fatal("tampering with a persisted event went undetected")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected a checksum mismatch, got: %v", err)
	}
}
