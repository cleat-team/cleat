package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// cleat#1024: the retention sweep did two things and reported one number.
//
// DeleteExpiredEvents deleted event_history rows and, in a second loop, cleared
// compaction bookkeeping on workflow_instances -- discarding that loop's
// RowsAffected. The first loop can never match (finalize_workflow_status purges
// those events first, cleat#1016), so the sweep reported zero on runs where it
// had cleared real rows.
//
// The obvious fix was to sum them. This is the test that refuses it: the two
// numbers count different tables and different operations, and the counter the
// sum would land in is documented as "expired event history rows deleted".
//
// Against real databases on every dialect, because the claim is about what two
// SQL statements do. A mock returns whatever it is told and can only restate
// the assertion.
func TestTheCompactionClearIsCountedApartFromTheEventDelete(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			id, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "counted-apart", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: %v %v", wf, err)
			}
			if err := store.FinalizeWorkflowSegment(ctx, id, "worker-1", wf.Generation,
				nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("finalize: %v", err)
			}

			// Give it compaction bookkeeping and an old completed_at, which is
			// the state the sweep's second half exists to clean up.
			setCompactionStateForTest(t, store, id)

			cutoff := time.Now().Add(24 * time.Hour) // everything is "expired"

			deleted, err := store.DeleteExpiredEvents(ctx, cutoff)
			if err != nil {
				t.Fatalf("DeleteExpiredEvents: %v", err)
			}
			cleared, err := store.ClearExpiredCompactionState(ctx, cutoff)
			if err != nil {
				t.Fatalf("ClearExpiredCompactionState: %v", err)
			}

			if cleared < 1 {
				t.Errorf("ClearExpiredCompactionState cleared %d rows, want at least 1.\n\n"+
					"This half of the sweep does the real work -- the event half "+
					"cannot match, because finalize already purged those rows. If "+
					"it reports zero, the sweep is silent about everything it did.", cleared)
			}
			if deleted != 0 {
				t.Errorf("DeleteExpiredEvents returned %d, want 0, with %d compaction "+
					"rows cleared.\n\nThe two must not be summed. They count different "+
					"tables and different operations, and this return feeds a counter "+
					"documented as \"expired event history rows deleted\". Fixing "+
					"\"reports zero\" by making the other number mean two things is "+
					"worse than the zero, which is at least honest about its arm.",
					deleted, cleared)
			}
		})
	}
}

// setCompactionStateForTest puts a finalized workflow into the state the
// sweep's compaction half looks for: bookkeeping present, completed_at set.
// Written per dialect because the placeholder syntax differs and there is no
// store method for it -- compaction writes these columns, and this test needs
// the state without running a compaction.
func setCompactionStateForTest(t *testing.T, store WorkflowStore, id string) {
	t.Helper()
	switch s := store.(type) {
	case *PostgresStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET compaction_state = $1, compaction_step = 1,
			 compacted_at = now(), completed_at = now() WHERE id = $2`, `{}`, id); err != nil {
			t.Fatalf("seed compaction state (postgres): %v", err)
		}
	case *MySQLStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET compaction_state = ?, compaction_step = 1,
			 compacted_at = NOW(6), completed_at = NOW(6) WHERE id = ?`, `{}`, id); err != nil {
			t.Fatalf("seed compaction state (mysql): %v", err)
		}
	case *MSSQLStore:
		if _, err := s.db.Exec(
			`UPDATE workflow_instances SET compaction_state = @p1, compaction_step = 1,
			 compacted_at = SYSUTCDATETIME(), completed_at = SYSUTCDATETIME() WHERE id = @p2`,
			`{}`, id); err != nil {
			t.Fatalf("seed compaction state (mssql): %v", err)
		}
	default:
		t.Fatalf("setCompactionStateForTest: unknown store %T", store)
	}
}
