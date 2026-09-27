package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// A dead-lettered child is terminal, and a parent awaiting one has to be told.
// cleat#1213.
//
// GetChildResult tested `status == "done" || status == "failed"`. GetChildCount,
// in the same file, treats the terminal set as ('done', 'failed',
// 'dead_lettered') -- two definitions of "terminal" differing by one value, and
// only one of them decides whether a parent can stop waiting.
//
// WHAT THE PARENT ACTUALLY DID, because the issue's phrasing ("never woken")
// turns out to be the less accurate of the two readings and the difference
// decides the fix:
//
// A suspension is not a parked row. The executor defaults SuspendError.Until to
// now+30s or now+10m when the caller sets none (executor.go), and the worker
// writes `status = 'ready'` with `next_wake_at = SuspendUntil`
// (cmd/cleat-worker/setup.go). So the parent is re-claimed, replays its whole
// history, calls AwaitChild again, gets the same non-answer, and suspends
// again. Forever. It is a livelock, not a deadlock -- which is worse than being
// parked, because every dead-lettered child leaves a permanently `ready` row
// re-entering the claim set and replaying at a fixed interval for the life of
// the deployment.
//
// AND THERE IS NO PUSH MECHANISM TO REPAIR. executor.go's comment named
// "child completion via wakeParent" as one of the events that wake a parent
// early. No such function is defined -- check with
// `grep -rn '^func wakeParent' --include='*.go' .`. The ^ is what makes that
// check survive being written down: a bare name search matches this paragraph,
// and so does the un-anchored declaration form, because the command quoting it
// is itself prose containing the string. The timeout that comment called a fallback for "edge cases
// where the wake mechanism fails" is the only mechanism there is.
//
// That settles the issue's open question. "Keep the parent suspended, but make
// it wake" is already what happens, so the retry story is not the thing at
// stake; the choice is between an actionable error now and an unbounded replay
// loop for a child that exhausted its retries and will not be retried without
// an operator. So: a dead-lettered child is reported as completed-and-failed,
// carrying the error_msg MoveToDeadLetterQueue already writes.
func TestADeadLetteredChildIsReportedToItsParent(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			children, ok := store.(ChildWorkflowStore)
			if !ok {
				t.Fatalf("%T does not implement ChildWorkflowStore", store)
			}

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "dlq-parent", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(parent): %v", err)
			}
			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "ABANDON", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			// Claimed first, because MoveToDeadLetterQueue is fenced on the
			// claim: it is called from writeTerminalFailure on a run this
			// worker owns. Dead-lettering an unclaimed row would exercise a
			// path the worker never takes.
			claimed, err := store.ClaimWorkflows(ctx, "w-dlq", 10)
			if err != nil {
				t.Fatalf("ClaimWorkflows: %v", err)
			}
			var gen int64
			var found bool
			for _, wf := range claimed {
				if wf.ID == childID {
					gen, found = wf.Generation, true
				}
			}
			if !found {
				t.Fatalf("the child was not among the %d claimed workflows", len(claimed))
			}

			const reason = "retries exhausted"
			if err := store.MoveToDeadLetterQueue(ctx, childID, "w-dlq", gen, reason, "", ""); err != nil {
				t.Fatalf("MoveToDeadLetterQueue: %v", err)
			}

			// The premise. Without it a store that silently failed to
			// dead-letter the row would make every assertion below pass for
			// the wrong reason -- the child would just be `running`, and
			// "not completed" would be correct.
			child, err := store.GetWorkflowByID(ctx, childID)
			if err != nil || child == nil {
				t.Fatalf("GetWorkflowByID(child): %v (nil=%v)", err, child == nil)
			}
			if child.Status != "dead_lettered" {
				t.Fatalf("premise failed: the child is %q, not dead_lettered -- nothing "+
					"below is a test of cleat#1213", child.Status)
			}

			out, err := children.GetChildResult(ctx, childID)
			if err != nil {
				t.Fatalf("GetChildResult: %v", err)
			}

			if !out.Completed {
				t.Errorf("a dead-lettered child reports Completed=false.\n\n"+
					"The child is terminal and stable, so AwaitChild suspends the parent. "+
					"That is not a parked row: the worker writes status='ready' with a "+
					"next_wake_at, so the parent is re-claimed, replays its whole history, "+
					"gets this same non-answer and suspends again -- for the life of the "+
					"deployment, once per dead-lettered child. status=%q", child.Status)
			}
			if !out.Failed {
				t.Errorf("a dead-lettered child reports Failed=false.\n\n"+
					"Completed without Failed is how a SUCCESS is spelled, so reporting "+
					"terminality alone would hand the parent an empty result as a result -- "+
					"which is cleat#1115, the defect #1212 had just fixed. status=%q",
					child.Status)
			}
			if out.Error != reason {
				t.Errorf("a dead-lettered child reports Error=%q, want %q.\n\n"+
					"MoveToDeadLetterQueue writes the reason to error_msg, and it is the "+
					"only account of why the child stopped. Dropping it leaves the parent "+
					"an error it cannot describe.", out.Error, reason)
			}
			if out.Result != "" {
				t.Errorf("a dead-lettered child carries Result=%q.\n\n"+
					"A failed run's result column is never written, so a non-empty value "+
					"here is the COALESCE default being passed off as a result.", out.Result)
			}
		})
	}
}
