package engine

import (
	"context"
	"testing"
)

// Does a child closed by the DEFER arm of the close policy cascade to ITS
// children, when a child closed by the plain arm does not?
//
// TestTerminateCascadeDepthIsOneLevel (IMPROVEMENT-PLAN 3.410) measured the
// plain case: terminate a root, its TERMINATE child is failed by a direct
// UPDATE, and the grandchild is untouched. That UPDATE does not go through
// FailWorkflow, and FailWorkflow is one of the calls that fires the cascade --
// so the recursion point is bypassed by the mechanism doing the closing.
//
// enforceParentClosePolicy has TWO arms, though. A child that owes a defer
// phase is NOT failed outright: it goes to `terminating` with the outcome
// recorded, is claimed again, runs its defers, and is finalised by
// FinalizeDeferPhase -- which DOES call enforceParentClosePolicy
// (engine/store_defer_phase.go). ExpireDeferPhases does too.
//
// If that is right, the depth of a terminate depends on whether a workflow in
// the middle happened to have deferred work: a property of that workflow's own
// code, invisible to whoever pressed terminate. That is worse than a flat one
// level, because one level is at least predictable.
//
// 3.410 recorded it as a reading and said someone should build this. This is
// that, and it exists because a reading of exactly this shape -- correct
// citations, a chain with no branch in it -- was published in the port
// work-lists about blank Idempotency-Key values and was wrong.
func TestTerminateCascadeDepthDependsOnWhetherTheChildOwedDefers(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			deployConcurrencyTestWorkflows(t, store, "wf-root-dd", "wf-spare-dd")

			// root -> child -> grandchild, TERMINATE on every edge.
			child, err := store.StartChildWorkflow(ctx, "wf-root-dd",
				"concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow(child): %v", err)
			}
			grand, err := store.StartChildWorkflow(ctx, child,
				"concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow(grandchild): %v", err)
			}

			// The child, and ONLY the child, owes a defer phase. This is the
			// whole independent variable: same tree, same policies, same
			// terminate as 3.410's -- one `defer` row in the middle.
			if err := store.AppendEventHistoryBatch(ctx, child, []EventRecord{
				{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup"},
			}); err != nil {
				t.Fatalf("AppendEventHistoryBatch(child): %v", err)
			}

			// CONTROL: the tree is three levels and nothing is closed yet.
			for _, want := range []struct{ id, parent string }{
				{child, "wf-root-dd"}, {grand, child},
			} {
				wf := mustGetWorkflow(t, ctx, store, want.id)
				got := ""
				if wf.ParentWorkflowID != nil {
					got = *wf.ParentWorkflowID
				}
				if got != want.parent {
					t.Fatalf("%s has parent %q, want %q -- the fixture is not the shape "+
						"this test reasons about", want.id, got, want.parent)
				}
				if wf.Status == "failed" || wf.Status == "done" {
					t.Fatalf("%s is already %q before the terminate", want.id, wf.Status)
				}
			}

			if err := store.TerminateWorkflow(ctx, "wf-root-dd", "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow(root): %v", err)
			}

			// The defer arm was taken, not the plain one. If this is "failed"
			// the child never entered a defer phase and everything below is
			// measuring 3.410's case again under a different name.
			if wf := mustGetWorkflow(t, ctx, store, child); wf.Status != statusTerminating {
				t.Fatalf("the child is %q, want %q -- it owes a defer phase, so the close "+
					"policy's defer arm must have taken it. This test cannot say anything "+
					"about the defer path otherwise.", wf.Status, statusTerminating)
			}

			// BEFORE the phase completes, the grandchild must still be
			// untouched -- otherwise the cascade reached it directly and the
			// defer phase is not what carried it.
			beforeStatus := mustGetWorkflow(t, ctx, store, grand).Status
			if beforeStatus == "failed" || beforeStatus == statusTerminating {
				t.Fatalf("the grandchild is already %q before the child's defer phase ran, "+
					"so the cascade reached two levels directly and this test is "+
					"attributing it to the wrong mechanism", beforeStatus)
			}

			// Run the child's defer phase to completion, exactly as a worker
			// would: claim it back (the cascade cleared assigned_to), then
			// finalize.
			claimed := claimByID(t, ctx, store, "worker-defer-depth", child)
			dps, ok := store.(DeferPhaseStore)
			if !ok {
				t.Fatalf("%T cannot finalize a defer phase, but it marked one", store)
			}
			if err := dps.FinalizeDeferPhase(ctx, child, "worker-defer-depth", claimed.Generation, nil); err != nil {
				t.Fatalf("FinalizeDeferPhase(child): %v", err)
			}
			if wf := mustGetWorkflow(t, ctx, store, child); wf.Status != "failed" {
				t.Fatalf("child is %q after its defer phase, want \"failed\"", wf.Status)
			}

			// THE MEASUREMENT.
			after := mustGetWorkflow(t, ctx, store, grand)
			afterFlagged, _, err := store.PollCancellation(ctx, grand)
			if err != nil {
				t.Fatalf("PollCancellation(grandchild): %v", err)
			}
			reached := after.Status == "failed" || after.Status == statusTerminating || afterFlagged

			if !reached {
				t.Errorf("the grandchild is %q and unflagged after the child's defer phase "+
					"completed.\n\nThat makes the cascade one level deep on BOTH arms, which "+
					"is more consistent than this test expected -- FinalizeDeferPhase calls "+
					"enforceParentClosePolicy, so something else must be declining. Good "+
					"news for the contract; invert this test and record why.", after.Status)
				return
			}

			t.Logf("FINDING: the grandchild is %q (cancellation_requested=%v) after the "+
				"child's defer phase completed, where the identical tree WITHOUT a defer "+
				"event leaves it untouched (3.410). The depth of a terminate is therefore a "+
				"property of whether a workflow in the middle happened to owe deferred work, "+
				"not of the terminate. See cleat#1108.", after.Status, afterFlagged)
		})
	}
}
