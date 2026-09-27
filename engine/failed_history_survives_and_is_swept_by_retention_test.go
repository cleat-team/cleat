package engine

import (
	"context"
	"testing"
	"time"
)

// TestFailedWorkflowHistorySurvivesFinalizeAndIsSweptByRetention is cleat#1973.
//
// finalize_workflow_status's 'failed' branch does delete event_history --
// but nothing in production calls it with finalStatus='failed'.
// FinalizeWorkflowSegment's one production call site only ever passes
// 'done' or 'ready' (cmd/cleat-worker/setup.go); the real failure path,
// FailWorkflow, is a plain UPDATE on all three dialects that never touches
// event_history. So a done workflow's history is purged immediately at
// finalize, and a failed workflow's history survives until --retention-days
// removes it. This test pins both halves of that contrast in one place, plus
// the sweep that eventually does remove the failed workflow's rows.
func TestFailedWorkflowHistorySurvivesFinalizeAndIsSweptByRetention(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()

		// The 'done' half of the contrast: finalize purges its history
		// immediately, same as before this issue.
		doneID := newIntentWorkflow(t, ctx, store, "done-history-finalize")
		if err := store.AppendEventHistory(ctx, doneID, EventRecord{
			Step: 0, EventType: EventTypeCall, Service: "billing", Op: "charge",
			Request: `{}`, Response: `{"ok":true}`,
		}); err != nil {
			t.Fatalf("AppendEventHistory (done): %v", err)
		}
		doneClaimed, err := store.ClaimWorkflows(ctx, "worker-retention-done", 20)
		if err != nil {
			t.Fatalf("ClaimWorkflows (done): %v", err)
		}
		var doneWF *WorkflowInstance
		for _, c := range doneClaimed {
			if c.ID == doneID {
				doneWF = c
			}
		}
		if doneWF == nil {
			t.Fatalf("workflow %s was not claimed; got %d", doneID, len(doneClaimed))
		}
		if err := store.FinalizeWorkflowSegment(ctx, doneID, "worker-retention-done",
			doneWF.Generation, nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != nil {
			t.Fatalf("FinalizeWorkflowSegment (done): %v", err)
		}
		doneHist, err := store.LoadEventHistory(ctx, doneID)
		if err != nil {
			t.Fatalf("LoadEventHistory (done): %v", err)
		}
		if len(doneHist) != 0 {
			t.Errorf("a done workflow's event_history must be purged at finalize, still has %d rows", len(doneHist))
		}

		// The 'failed' half: FailWorkflow must not purge it.
		failedID := newIntentWorkflow(t, ctx, store, "failed-history-retention")
		if err := store.AppendEventHistory(ctx, failedID, EventRecord{
			Step: 0, EventType: EventTypeCall, Service: "billing", Op: "charge",
			Request: `{}`, Response: `{"ok":true}`,
		}); err != nil {
			t.Fatalf("AppendEventHistory (failed): %v", err)
		}
		claimAndFail(t, ctx, store, failedID)

		failedHist, err := store.LoadEventHistory(ctx, failedID)
		if err != nil {
			t.Fatalf("LoadEventHistory after FailWorkflow: %v", err)
		}
		if len(failedHist) == 0 {
			t.Fatal("a failed workflow's event_history must survive FailWorkflow -- only " +
				"the retention sweep should remove it, not finalize")
		}

		// A cutoff in the future rather than ageing the rows: FailWorkflow set
		// completed_at = now(), so anything after that matches without
		// per-dialect date arithmetic (same technique
		// TestARetentionPreviewAgreesWithTheSweepAndDeletesNothing uses).
		cutoff := time.Now().Add(time.Hour)

		previewed, err := store.CountExpiredEvents(ctx, cutoff)
		if err != nil {
			t.Fatalf("CountExpiredEvents: %v", err)
		}
		if previewed == 0 {
			t.Fatal("the events-retention preview reports 0 with a failed workflow's " +
				"history seeded past the cutoff -- the events arm must match failed " +
				"runs, not only done ones")
		}

		deleted, err := store.DeleteExpiredEvents(ctx, cutoff)
		if err != nil {
			t.Fatalf("DeleteExpiredEvents: %v", err)
		}
		if deleted != previewed {
			t.Errorf("preview reported %d, sweep deleted %d -- they share one predicate "+
				"and must agree (cleat#1457)", previewed, deleted)
		}

		afterSweep, err := store.LoadEventHistory(ctx, failedID)
		if err != nil {
			t.Fatalf("LoadEventHistory after sweep: %v", err)
		}
		if len(afterSweep) != 0 {
			t.Errorf("failed workflow still has %d event rows after the retention sweep, want 0", len(afterSweep))
		}
	})
}
