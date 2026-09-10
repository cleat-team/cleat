package engine

import (
	"context"
	"testing"
)

// The two arms of the close policy must reach the same depth.
//
// enforceParentClosePolicy closes a TERMINATE child two different ways. A child
// that owes no defers is failed by a bulk UPDATE. A child that owes a defer
// phase goes to `terminating`, is claimed again, runs its defers, and is
// finalised by FinalizeDeferPhase.
//
// Measured 2026-09-09 before the fix (IMPROVEMENT-PLAN 3.411): they disagreed.
// The defer arm reached the grandchild, the plain arm did not, so the depth of a
// terminate was a property of whether a workflow in the middle happened to have
// deferred work -- its own code, invisible to whoever pressed terminate.
//
// WHY THIS IS ONE TEST AND NOT TWO. The disagreement survived because the two
// behaviours live in different arms, each looks correct from inside its own, and
// nothing compared them. Two separate tests -- one per arm, in different files --
// is the arrangement that let it happen: both would have passed. This builds both
// trees in one run and asserts the SAME outcome, so a future change that fixes
// one arm and not the other cannot be green.
func TestBothCloseArmsReachTheSameDepth(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			deployConcurrencyTestWorkflows(t, store, "wf-arms-root", "wf-arms-spare")

			// Two subtrees under one root. Identical but for a single `defer`
			// row on one middle child, which is what selects the arm.
			mid := map[string]string{}
			for _, arm := range []string{"plain", "defer"} {
				child, err := store.StartChildWorkflow(ctx, "wf-arms-root",
					"concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
				if err != nil {
					t.Fatalf("StartChildWorkflow(%s): %v", arm, err)
				}
				mid[arm] = child
			}
			grand := map[string]string{}
			for arm, child := range mid {
				g, err := store.StartChildWorkflow(ctx, child,
					"concurrency-test-workflow", `{}`, 1, "TERMINATE", 0)
				if err != nil {
					t.Fatalf("StartChildWorkflow(grandchild of %s): %v", arm, err)
				}
				grand[arm] = g
			}
			if err := store.AppendEventHistoryBatch(ctx, mid["defer"], []EventRecord{
				{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup"},
			}); err != nil {
				t.Fatalf("AppendEventHistoryBatch: %v", err)
			}

			if err := store.TerminateWorkflow(ctx, "wf-arms-root", "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow(root): %v", err)
			}

			// CONTROL: the two arms really were taken, one each. Without this,
			// two subtrees that both took the plain arm would agree trivially
			// and the test would assert nothing about the defer arm.
			if wf := mustGetWorkflow(t, ctx, store, mid["plain"]); wf.Status != "failed" {
				t.Fatalf("the plain middle child is %q, want \"failed\" -- it owes no defers, "+
					"so the plain arm must have taken it", wf.Status)
			}
			if wf := mustGetWorkflow(t, ctx, store, mid["defer"]); wf.Status != statusTerminating {
				t.Fatalf("the defer middle child is %q, want %q -- it owes a defer phase, so "+
					"the defer arm must have taken it. Both subtrees took the same arm and "+
					"this test is not comparing anything.", wf.Status, statusTerminating)
			}

			// Drive the defer arm to completion, as a worker would.
			claimed := claimByID(t, ctx, store, "worker-arms", mid["defer"])
			dps, ok := store.(DeferPhaseStore)
			if !ok {
				t.Fatalf("%T cannot finalize a defer phase, but it marked one", store)
			}
			if err := dps.FinalizeDeferPhase(ctx, mid["defer"], "worker-arms", claimed.Generation, nil); err != nil {
				t.Fatalf("FinalizeDeferPhase: %v", err)
			}

			// THE COMPARISON.
			reached := map[string]bool{}
			status := map[string]string{}
			for arm, g := range grand {
				wf := mustGetWorkflow(t, ctx, store, g)
				flagged, _, err := store.PollCancellation(ctx, g)
				if err != nil {
					t.Fatalf("PollCancellation(%s grandchild): %v", arm, err)
				}
				status[arm] = wf.Status
				reached[arm] = wf.Status == "failed" || wf.Status == statusTerminating || flagged
			}

			if reached["plain"] != reached["defer"] {
				t.Errorf("the two arms disagree on depth: plain-arm grandchild is %q "+
					"(reached=%v), defer-arm grandchild is %q (reached=%v).\n\n"+
					"How much of a tree a terminate closes must not depend on whether a "+
					"workflow in the middle happened to owe deferred work. That is its own "+
					"code, invisible to whoever pressed terminate, and it changes the day "+
					"someone adds or removes a defer. See cleat#1108 and IMPROVEMENT-PLAN "+
					"3.411 for the measurement that found this.",
					status["plain"], reached["plain"], status["defer"], reached["defer"])
			}
			if !reached["plain"] {
				t.Errorf("neither arm reached its grandchild (plain=%q defer=%q). The arms "+
					"agree, which this test asks for, but they agree on leaving the subtree "+
					"running -- see TestTerminateCascadeReachesEveryDescendant.",
					status["plain"], status["defer"])
			}
		})
	}
}
