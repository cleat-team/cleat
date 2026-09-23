package engine

// cleat#1978. cleat#1974 taught GetChildResult/AwaitChild to answer a
// TERMINATED or CANCELLED child instead of leaving its parent stuck
// suspended forever -- but every case it covers reaches "terminated" through
// a DIRECT TerminateWorkflow call. A TERMINATE close-policy child reaches
// the exact same status through a different path: enforceParentClosePolicy,
// run out of FinalizeWorkflowSegment/CompleteWorkflow/FailWorkflow/... when
// the PARENT closes, not when anyone calls TerminateWorkflow on the child
// itself. Before this PR that arm wrote status='failed', so it was already
// covered by the ordinary failure path and #1974's fix never had to touch
// it. Now it writes status='terminated', and this test is the one place
// that confirms the two paths actually compose: that a grandparent awaiting
// a policy-closed child sees "[TERMINATED] ..." and not a livelock, and that
// the message names the parent's REAL outcome rather than a fixed string.
//
// THE CONTROL IS THE MESSAGE, not just the status. childOutcomeForSettledStatus
// already mapped 'terminated' to "[TERMINATED] "+errMsg before this PR shipped
// (cleat#1974/#1993) -- so a test that only checks the status/Failed shape
// would pass against a build that still hardcoded "parent workflow terminated"
// regardless of why the parent closed. Closing the parent with CompleteWorkflow
// (finalStatus="done") rather than TerminateWorkflow is what makes that
// distinguishable: a hardcoded message and the real one disagree here.
import (
	"context"
	"encoding/json"
	"testing"
)

func TestAGrandparentIsToldWhenAPolicyClosedChildIsTerminated(t *testing.T) {
	const parentResult = `{"parent":"done"}`

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
				json.RawMessage(`{}`), "grandparent-sees-terminated-parent", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun(parent): %v", err)
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow (TERMINATE policy): %v", err)
			}

			// Close the parent normally -- CompleteWorkflow's fenced path, not
			// a direct TerminateWorkflow on anything -- so the child's status
			// change comes ONLY from enforceParentClosePolicy's cascade.
			gen, err := claimGeneration(t, store, parentID)
			if err != nil {
				t.Fatalf("claim the parent: %v", err)
			}
			if err := store.FinalizeWorkflowSegment(ctx, parentID, "w-1974", gen, nil,
				"done", parentResult, "", "", map[string]string{}, time0()); err != nil {
				t.Fatalf("finalize the parent as done: %v", err)
			}

			// The premise: without it, the assertions below would be vacuous.
			assertStatus(t, store, childID, "terminated")

			// --- store level ---
			out, err := children.GetChildResult(ctx, childID)
			if err != nil {
				t.Fatalf("GetChildResult(child): %v", err)
			}
			assertTerminalFailure(t, "policy-terminated", out, "[TERMINATED] parent workflow completed")

			// --- session level: the real AwaitChild path a grandparent takes ---
			gp := newTestExecSession()
			gp.engine.childWfStore = children
			code := awaitCode(gp.AwaitChild(ctx, nil, childID, 0, 0))
			if code != 1 {
				t.Errorf("a grandparent awaiting a TERMINATE-policy-closed child got error code %d, "+
					"want 1 -- AwaitChild reads a non-completed ChildOutcome as \"not yet\", so this "+
					"grandparent would suspend and replay into the same non-answer forever.", code)
			}
			if len(gp.history) != 1 {
				t.Fatalf("expected one recorded event, got %d", len(gp.history))
			}
			if got := gp.history[0].Err; got != "[TERMINATED] parent workflow completed" {
				t.Errorf("the recorded await_child event carries Err=%q, want %q", got,
					"[TERMINATED] parent workflow completed")
			}
			if gp.history[0].Response != "" {
				t.Errorf("the recorded await_child event carries Response=%q; a failure must not "+
					"also present as a result", gp.history[0].Response)
			}
		})
	}
}
