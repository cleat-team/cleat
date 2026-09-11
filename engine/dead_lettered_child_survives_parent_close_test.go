package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// A parent closing with TERMINATE must not overwrite a child that is already
// dead-lettered.
//
// cleat#1227. The close-policy UPDATE selected on `status NOT IN ('done',
// 'failed')`, and the engine writes two more terminal statuses than that --
// 'dead_lettered' and 'terminated'. So a dead-lettered child matched:
//
//	BEFORE  status=dead_lettered  error_msg="retries exhausted"           error_code=E_RETRY
//	AFTER   status=failed         error_msg="parent workflow terminated"  error_code=E_RETRY
//
// # Why error_code is the assertion that matters most
//
// Three things are lost in one UPDATE -- the run leaves the dead-letter queue,
// its failure reason is replaced, and error_code is NOT in the SET list. The
// third is what makes the result worse than a plain overwrite: the surviving
// row reports "parent workflow terminated" alongside the error code of the
// retry exhaustion that actually killed it. Two causes, neither marked, and
// nothing about the row looks wrong. A test that checked only `status` would
// pass against a fix that still replaced the message.
func TestADeadLetteredChildSurvivesItsParentsTerminateClosePolicy(t *testing.T) {
	for _, backend := range registeredBackends {
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			// setupTestData deploys "test-workflow"; workflow_instances carries
			// an FK on (tenant_id, def_name, def_version), so without it every
			// StartNewRun below fails on the foreign key rather than on
			// anything this test is about.
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			children, ok := store.(ChildWorkflowStore)
			if !ok {
				t.Fatalf("%T does not implement ChildWorkflowStore", store)
			}

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1,
				json.RawMessage(`{}`), "dlq-close-parent", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(parent): %v", err)
			}
			// TERMINATE is the policy under test; ABANDON would leave the child
			// alone for a reason that has nothing to do with this fix.
			childID, err := children.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			// Claimed first: MoveToDeadLetterQueue is fenced on the claim, so
			// dead-lettering an unclaimed row would exercise a path the worker
			// never takes.
			claimed, err := store.ClaimWorkflows(ctx, "w-dlq-close", 10)
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
			const code = "E_RETRY"
			if err := store.MoveToDeadLetterQueue(ctx, childID, "w-dlq-close", gen, reason, code, "run"); err != nil {
				t.Fatalf("MoveToDeadLetterQueue: %v", err)
			}

			// The premise, asserted rather than assumed: if the dead-letter
			// write silently did nothing, every assertion below would pass
			// against a child that was simply never terminal.
			before, err := store.GetWorkflowByID(ctx, childID)
			if err != nil || before == nil {
				t.Fatalf("GetWorkflowByID(child) before the close: wf=%v err=%v", before, err)
			}
			if before.Status != "dead_lettered" {
				t.Fatalf("child status before the parent closed = %q, want \"dead_lettered\" -- "+
					"the test did not reach the state it asserts about", before.Status)
			}

			// Close the parent. TerminateWorkflow is the unfenced terminal
			// write, and it is what runs enforceParentClosePolicy.
			if err := store.TerminateWorkflow(ctx, parentID, "parent done"); err != nil {
				t.Fatalf("TerminateWorkflow(parent): %v", err)
			}

			after, err := store.GetWorkflowByID(ctx, childID)
			if err != nil || after == nil {
				t.Fatalf("GetWorkflowByID(child) after the close: wf=%v err=%v", after, err)
			}

			if after.Status != "dead_lettered" {
				t.Errorf("child status = %q after its parent closed with TERMINATE, want "+
					"\"dead_lettered\".\n\nThis is cleat#1227: the run has been taken out of the "+
					"dead-letter queue by a close policy that treated it as still active.", after.Status)
			}
			if after.Error != reason {
				t.Errorf("child error_msg = %q, want %q.\n\nThe reason the run actually died has "+
					"been replaced by the reason its parent closed.", after.Error, reason)
			}
			if after.ErrorCode != code {
				t.Errorf("child error_code = %q, want %q", after.ErrorCode, code)
			}
			// The pair, stated as one assertion because the pair is the defect:
			// error_code is not in the close policy's SET list, so an overwrite
			// leaves the row self-contradictory rather than merely wrong.
			if after.Error != reason && after.ErrorCode == code {
				t.Errorf("the child now reports two different causes at once: error_msg=%q with "+
					"error_code=%q.\n\nNothing about that row looks wrong, which is why this is "+
					"worse than a plain overwrite.", after.Error, after.ErrorCode)
			}
		})
	}
}
