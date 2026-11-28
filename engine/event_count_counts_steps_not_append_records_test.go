package engine

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/cleat-team/cleat/engine/testutil"
)

// cleat#3779: workflow_instances.event_count is the highest step recorded plus
// one, not the number of append RECORDS.
//
// It used to be "event_count + len(recs)". An await re-records the same step on
// every wake at a still-pending await_child / await_promise / await_all_children
// (an upsert, on purpose), and a repeated finalize re-appends the same records;
// each added to the count and nothing to the rows. Measured on PostgreSQL 16 with
// a parent waiting 300 s on a child: the count passed the quota at the fourth
// 30 s wake, on a run whose history was four rows, and replay's short-history
// check read it as a truncated tail on every healthy run.
//
// These tests drive AppendEventHistoryBatch, which is appendEventsInTx on every
// dialect, and read the column through a privileged handle.

func eventCountReader(t *testing.T, db *sql.DB, d testutil.Dialect, wfID string) func() int64 {
	t.Helper()
	q := `SELECT event_count FROM workflow_instances WHERE id = $1`
	switch d {
	case testutil.DialectMySQL:
		q = `SELECT event_count FROM workflow_instances WHERE id = ?`
	case testutil.DialectMSSQL:
		q = `SELECT event_count FROM workflow_instances WHERE id = @p1`
	}
	admin := testutil.AdminDB(t, db, d)
	return func() int64 {
		t.Helper()
		var n int64
		if err := admin.QueryRow(q, wfID).Scan(&n); err != nil {
			t.Fatalf("reading event_count on %s: %v", d, err)
		}
		return n
	}
}

func eventRowCounter(t *testing.T, db *sql.DB, d testutil.Dialect, wfID string) func() int {
	t.Helper()
	admin := testutil.AdminDB(t, db, d)
	return func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow(countEventsSQL(d), wfID).Scan(&n); err != nil {
			t.Fatalf("counting event_history rows on %s: %v", d, err)
		}
		return n
	}
}

func stepRecords(from, to int) []EventRecord {
	var recs []EventRecord
	for i := from; i < to; i++ {
		recs = append(recs, EventRecord{
			Step: i, EventType: EventTypeCall, Service: "svc", Op: "op",
			Request: `{}`, Response: fmt.Sprintf(`"r%d"`, i),
		})
	}
	return recs
}

func pendingAwaitChild(step int) EventRecord {
	return EventRecord{Step: step, EventType: EventTypeAwaitChild, Request: `{"child_id":"c1"}`, Response: ""}
}

func eachDialectWithCountFixture(t *testing.T, run func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore, wfID string)) {
	t.Helper()
	for _, d := range []testutil.Dialect{testutil.DialectPostgres, testutil.DialectMySQL, testutil.DialectMSSQL} {
		d := d
		t.Run(string(d), func(t *testing.T) {
			db := testutil.TestDB(t, d)
			defer db.Close()
			switch d {
			case testutil.DialectMySQL:
				testutil.SetupMySQLFullSchema(t, db)
			case testutil.DialectMSSQL:
				testutil.SetupMSSQLFullSchema(t, db)
			default:
				testutil.SetupFullSchema(t, db, d)
			}
			wfID := "event-count-steps-" + string(d)
			seedWorkflowInstance(t, db, d, wfID)
			run(t, d, db, storeFor(t, d, db), wfID)
		})
	}
}

// A wake at a pending await records the same step again. The count must not move,
// and a genuinely new step must still move it (the positive control: without it,
// "unchanged" is also what a read of the wrong row returns).
func TestEventCountDoesNotGrowWhenAnAwaitIsReRecorded(t *testing.T) {
	eachDialectWithCountFixture(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore, wfID string) {
		ctx := context.Background()
		count := eventCountReader(t, db, d, wfID)
		rows := eventRowCounter(t, db, d, wfID)

		first := append(stepRecords(0, 3), pendingAwaitChild(3))
		if err := store.AppendEventHistoryBatch(ctx, wfID, first); err != nil {
			t.Fatalf("first append: %v", err)
		}
		if got := count(); got != 4 {
			t.Fatalf("event_count = %d after four records, want 4", got)
		}

		for wake := 1; wake <= 5; wake++ {
			if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{pendingAwaitChild(3)}); err != nil {
				t.Fatalf("wake %d: %v", wake, err)
			}
		}
		if got, n := count(), rows(); got != int64(n) || got != 4 {
			t.Errorf("event_count = %d, event_history rows = %d after five wakes at the same pending await, want 4 and 4 "+
				"(\"+ len(recs)\" gives 9)", got, n)
		}

		// Positive control: the next step really recorded moves the count.
		if err := store.AppendEventHistoryBatch(ctx, wfID, stepRecords(4, 5)); err != nil {
			t.Fatalf("next step: %v", err)
		}
		if got, n := count(), rows(); got != 5 || n != 5 {
			t.Errorf("event_count = %d, rows = %d after a genuinely new step, want 5 and 5", got, n)
		}
	})
}

// A repeated finalize appends the same records again (cleat#1507's hypothesis):
// 3 -> 6 over two appends of the same three records before this fix.
func TestEventCountDoesNotGrowWhenTheSameRecordsAreAppendedTwice(t *testing.T) {
	eachDialectWithCountFixture(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore, wfID string) {
		ctx := context.Background()
		count := eventCountReader(t, db, d, wfID)
		recs := stepRecords(0, 3)
		for i := 1; i <= 2; i++ {
			if err := store.AppendEventHistoryBatch(ctx, wfID, recs); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}
		if got := count(); got != 3 {
			t.Errorf("event_count = %d after the same three records appended twice, want 3", got)
		}
	})
}

// A run whose count an older worker inflated is set back by its next append: no
// migration, no recount.
func TestAnInflatedEventCountHealsAtTheNextAppend(t *testing.T) {
	eachDialectWithCountFixture(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore, wfID string) {
		ctx := context.Background()
		count := eventCountReader(t, db, d, wfID)
		if err := store.AppendEventHistoryBatch(ctx, wfID, stepRecords(0, 4)); err != nil {
			t.Fatalf("append: %v", err)
		}
		set := `UPDATE workflow_instances SET event_count = 400 WHERE id = $1`
		switch d {
		case testutil.DialectMySQL:
			set = `UPDATE workflow_instances SET event_count = 400 WHERE id = ?`
		case testutil.DialectMSSQL:
			set = `UPDATE workflow_instances SET event_count = 400 WHERE id = @p1`
		}
		if _, err := testutil.AdminDB(t, db, d).Exec(set, wfID); err != nil {
			t.Fatalf("inflating the count: %v", err)
		}
		if got := count(); got != 400 {
			t.Fatalf("setup: event_count = %d, want 400", got)
		}
		if err := store.AppendEventHistoryBatch(ctx, wfID, stepRecords(4, 5)); err != nil {
			t.Fatalf("append after inflation: %v", err)
		}
		if got := count(); got != 5 {
			t.Errorf("event_count = %d after the next append, want 5 (the highest step is 4)", got)
		}
	})
}

// After compaction the rows below the compaction step are gone and the count must
// still be the number of steps, so that the quota, the auto-ContinueAsNew
// threshold and the replay short-history check all read the same true number --
// the last against the merged history (restored prefix + surviving rows), which
// is what warnIfReplayHistoryIsShort measures, not against the surviving rows.
func TestEventCountOfACompactedRunIsStillTheNumberOfSteps(t *testing.T) {
	eachDialectWithCountFixture(t, func(t *testing.T, d testutil.Dialect, db *sql.DB, store WorkflowStore, wfID string) {
		ctx := context.Background()
		count := eventCountReader(t, db, d, wfID)
		rows := eventRowCounter(t, db, d, wfID)

		const steps, compactedBelow = 10, 6
		if err := store.AppendEventHistoryBatch(ctx, wfID, stepRecords(0, steps)); err != nil {
			t.Fatalf("append: %v", err)
		}
		state := `{"version":1,"compacted_step":6,"events":[`
		for i := 0; i < compactedBelow; i++ {
			if i > 0 {
				state += ","
			}
			state += fmt.Sprintf(`{"s":%d}`, i)
		}
		state += `]}`
		if err := store.CompactHistory(ctx, wfID, []byte(state), compactedBelow, compactedBelow); err != nil {
			t.Fatalf("CompactHistory: %v", err)
		}
		if n := rows(); n != steps-compactedBelow {
			t.Fatalf("setup: %d rows survive compaction, want %d", n, steps-compactedBelow)
		}
		// Still the number of steps, though only four rows are left.
		if got := count(); got != steps {
			t.Fatalf("event_count = %d after compaction, want %d", got, steps)
		}

		// A wake at a pending await above the compaction step, five times over.
		if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{pendingAwaitChild(steps)}); err != nil {
			t.Fatalf("first await: %v", err)
		}
		for i := 0; i < 5; i++ {
			if err := store.AppendEventHistoryBatch(ctx, wfID, []EventRecord{pendingAwaitChild(steps)}); err != nil {
				t.Fatalf("wake: %v", err)
			}
		}
		recorded := int(count())
		merged := compactedBelow + rows() // what len(replayHistory) is after the merge
		if recorded != steps+1 || merged != steps+1 {
			t.Errorf("recorded = %d, merged history = %d, want both %d", recorded, merged, steps+1)
		}
		// The quota reads event_count (flush.go: currentCount >= maxQuotaEvents), so it
		// agrees with the true step count and a cap one above it is not yet hit.
		if recorded >= steps+2 {
			t.Errorf("a cap of %d would be hit by a run of %d steps", steps+2, steps+1)
		}
		// And the short-history check, against the merged history, stays quiet.
		if short, missing := reportShortReplayHistory(recorded, merged, 0); short {
			t.Errorf("replay short-history warning fires on a healthy compacted run: recorded=%d merged=%d missing=%d",
				recorded, merged, missing)
		}
		// Known-positive for that reader: a genuinely missing tail still fires.
		if short, _ := reportShortReplayHistory(recorded, merged-1, 0); !short {
			t.Error("the short-history check no longer reports a real missing tail, so the quiet reading above proves nothing")
		}
	})
}
