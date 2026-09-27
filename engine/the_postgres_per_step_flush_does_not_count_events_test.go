package engine

import (
	"context"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// TestThePostgresPerStepFlushDoesNotCountEvents is the PostgreSQL arm that
// TestPerStepFlushDoesNotDoubleCountEvents (flush_dialect_test.go) does not
// have. cleat#1594.
//
// That test states the rule -- the per-step flush must not increment
// workflow_instances.event_count, because FinalizeWorkflowSegment re-appends
// the whole segment and increments unconditionally, so counting in both places
// doubles every event and halves the effective cap -- and then asserts it on
// MySQL and SQL Server only. For PostgreSQL it says, in prose:
//
//	PostgreSQL's raw per-step insert has never touched event_count either,
//	which is what makes leaving it alone the consistent choice rather than a
//	special case.
//
// Measured 2026-09-14 on PostgreSQL 16: true. This is that sentence as a test.
//
// IT CANNOT BE A SUBTEST OF THE EXISTING TABLE, for two reasons that are worth
// knowing before anyone tries:
//
//  1. The existing test drives store.(perStepEventFlusher).flushEventForStep.
//     The PostgreSQL store does not implement that interface -- its per-step
//     path is Engine.flushEvent (engine/flush.go), which builds insertEventSQL
//     directly -- and the existing test t.Fatalf's on the failed assertion.
//  2. eventCountOf emits `?` placeholders for every dialect but MSSQL, so a
//     pg subtest reading through it fails with a syntax error rather than a
//     behavioural difference. A wrong answer that looks like a broken test is
//     the better of the two outcomes here; a broken test that looks like a
//     wrong answer is what this file exists to avoid.
//
// THE FINALIZE APPEND IS A POSITIVE CONTROL, NOT A SECOND ASSERTION. Without
// it, `event_count == 0` after the per-step flush is equally what a query
// against the wrong row, the wrong column or an unmigrated database returns.
// The control has to move the number this test claims does not move, in the
// same run and through the same read.
func TestThePostgresPerStepFlushDoesNotCountEvents(t *testing.T) {
	d := testutil.DialectPostgres
	db := testutil.TestDB(t, d)
	defer db.Close()
	testutil.SetupFullSchema(t, db, d)

	const wfID = "pg-per-step-flush-counting"
	seedWorkflowInstance(t, db, d, wfID)
	store := storeFor(t, d, db)
	ctx := context.Background()
	admin := testutil.AdminDB(t, db, d)

	eventCount := func() int64 {
		var n int64
		if err := admin.QueryRow(
			`SELECT event_count FROM workflow_instances WHERE id = $1`, wfID).Scan(&n); err != nil {
			t.Fatalf("reading event_count: %v", err)
		}
		return n
	}
	rowCount := func() int {
		var n int
		if err := admin.QueryRow(countEventsSQL(d), wfID).Scan(&n); err != nil {
			t.Fatalf("counting event_history rows: %v", err)
		}
		return n
	}

	recs := make([]EventRecord, 3)
	for i := range recs {
		recs[i] = EventRecord{
			Step: i, EventType: EventTypeCall,
			Service: "payments", Op: "Charge",
			Request: `{"amount":100}`, Response: `{"charge_id":"chg-1"}`,
		}
	}

	eng := NewEngine(nil, nil,
		WithDB(db), WithWorkflowStore(store),
		WithWorkflowID(wfID), WithTenantID(DefaultTenantUUID))
	for _, rec := range recs {
		if err := eng.flushEvent(ctx, wfID, rec, ""); err != nil {
			t.Fatalf("per-step flush of step %d: %v", rec.Step, err)
		}
	}

	if got := eventCount(); got != 0 {
		t.Errorf("event_count is %d after three per-step flushes, want 0: the finalize "+
			"append counts the segment, so counting here too doubles every event and "+
			"halves the effective quota", got)
	}
	if got := rowCount(); got != 3 {
		t.Fatalf("event_history has %d rows after three per-step flushes, want 3 -- the "+
			"flushes did not land, so the event_count assertion above proves nothing", got)
	}

	// The segment ends, exactly as FinalizeWorkflowSegment does it.
	if err := store.AppendEventHistoryBatch(ctx, wfID, recs); err != nil {
		t.Fatalf("finalize append: %v", err)
	}
	if got := eventCount(); got != 3 {
		t.Errorf("POSITIVE CONTROL: event_count is %d after the finalize append, want 3. "+
			"This read can move, or the zero measured above means nothing.", got)
	}
	if got := rowCount(); got != 3 {
		t.Errorf("event_history has %d rows after the finalize append, want 3: the append "+
			"re-writes the whole segment and must stay idempotent", got)
	}
}
