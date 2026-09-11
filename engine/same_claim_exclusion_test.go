package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// What follows pins the exclusion that stops a SECOND caller ON THE SAME CLAIM,
// and it is a different question from the one the fence predicate answers.
//
// Three lifecycle statements -- finalize, ContinueAsNew and ReleaseWorkflow --
// all end `WHERE id = $1 AND assigned_to = $2 AND generation = $N`, and none of
// them bumps `generation`. Claiming bumps it, so `generation` excludes a caller
// from an EARLIER claim, and `assigned_to = $2` excludes a DIFFERENT worker.
// Neither can tell two callers apart when they hold the same claim: same
// worker, same generation.
//
// What excludes the second one is `SET assigned_to = NULL` in the statement the
// first one ran. A clause in the SET list, not the WHERE clause that reads as
// the guard. It works, nothing names it, and CLAUDE.md 3.112 records the cost of
// that once already: a test named for the marker predicate stayed green with
// the predicate deleted, because what refused the repeated finalize was the
// finalize clearing assigned_to.
//
// This matters now rather than in the abstract. Decision 4 -- a durable record
// of which worker ran a workflow -- is approved, and its cheapest
// implementation is to stop discarding assigned_to. cleat#1175.
//
// # The two mutations these tests owe
//
// A test that passes under BOTH of the following is asserting neither:
//
//	delete `AND generation = $N` from the statement  -> must STILL PASS
//	preserve assigned_to instead of NULLing it       -> must FAIL
//
// The first is what proves the fence predicate is not what is doing the work
// here; the second is what proves the SET clause is.

func deployAndStart(t *testing.T, store WorkflowStore, name string) {
	t.Helper()
	ctx := context.Background()
	if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
		Name: name, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
		ABIVersion: 1, MinVersion: 1,
	}); err != nil {
		t.Fatalf("DeployWorkflowDef: %v", err)
	}
	if _, _, err := store.StartNewRun(ctx, "", name, 1,
		json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
		t.Fatalf("StartNewRun: %v", err)
	}
}

// successorsOf counts the rows that record `id` as the run they continued from.
// The count is the assertion that matters: "the second call returned an error"
// is satisfiable by an error from an unrelated path, and would pass against a
// version that forked the chain and then failed for some other reason.
func successorsOf(t *testing.T, store WorkflowStore, id string) int {
	t.Helper()
	ctx := context.Background()
	all, err := store.ListWorkflows(ctx, WorkflowFilter{Limit: 1000})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	// Re-fetched by id rather than read off the listing: workflowInstanceColumns
	// does not select continued_from, so every row ListWorkflows returns has an
	// empty ContinuedFrom. Counting off the listing looked right and reported
	// zero successors for a chain that had one -- a silently-empty field, which
	// is the failure this whole issue is about wearing different clothes.
	n := 0
	for _, listed := range all {
		wf, err := store.GetWorkflowByID(ctx, listed.ID)
		if err != nil {
			t.Fatalf("GetWorkflowByID(%s): %v", listed.ID, err)
		}
		if wf != nil && wf.ContinuedFrom == id {
			n++
		}
	}
	return n
}

func TestASecondContinueAsNewOnTheSameClaimIsRefused_MultiBackend(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			truncateAll(t, store)
			ctx := context.Background()

			deployAndStart(t, store, "same-claim-can")
			wf, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || wf == nil {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", wf, err)
			}

			first, err := store.ContinueAsNew(ctx, wf.ID, "worker-1", wf.Generation,
				"same-claim-can", 1, json.RawMessage(`{}`), nil, "", nil, 0)
			if err != nil {
				t.Fatalf("the first ContinueAsNew failed: %v", err)
			}

			// The same worker, the same generation -- the same claim. Nothing in
			// the WHERE clause distinguishes this call from the one above.
			second, err := store.ContinueAsNew(ctx, wf.ID, "worker-1", wf.Generation,
				"same-claim-can", 1, json.RawMessage(`{}`), nil, "", nil, 0)
			if !errors.Is(err, ErrFenceLost) {
				t.Errorf("a second ContinueAsNew on the SAME claim returned (%q, %v), "+
					"want ErrFenceLost.\n\nThe first call's `SET assigned_to = NULL` is "+
					"what refuses this -- `generation` is not bumped here, so the fence "+
					"predicate matches identically for both callers.", second, err)
			}

			if n := successorsOf(t, store, wf.ID); n != 1 {
				t.Errorf("%s has %d successors, want exactly 1.\n\nThis is the "+
					"assertion that matters: a forked chain with an unrelated error "+
					"on the second call would satisfy the ErrFenceLost check above "+
					"and still be wrong.", wf.ID, n)
			}
			if first == "" {
				t.Error("the first ContinueAsNew returned an empty successor id")
			}
		})
	}
}

// TestReleasingAClaimTwiceDoesNotResurrectIt_MultiBackend.
//
// ReleaseWorkflow is the third site, and it is the one with no observable
// refusal: it does not read RowsAffected, so a second release on a claim
// already given up returns nil either way. What CAN be asserted is that the
// second call does not undo the first -- and that is worth pinning, because
// the release sets `status`, `assigned_to` and `next_wake_at` together, so a
// version that matched a row it no longer owned could move a workflow another
// worker had since claimed.
func TestReleasingAClaimTwiceDoesNotResurrectIt_MultiBackend(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			truncateAll(t, store)
			ctx := context.Background()

			deployAndStart(t, store, "same-claim-release")
			first, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil || first == nil {
				t.Fatalf("ClaimWorkflow: wf=%v err=%v", first, err)
			}
			if err := store.ReleaseWorkflow(ctx, first.ID, "worker-1", first.Generation, time.Now().UTC()); err != nil {
				t.Fatalf("the first ReleaseWorkflow failed: %v", err)
			}

			// A different worker picks it up.
			second, err := store.ClaimWorkflow(ctx, "worker-2")
			if err != nil || second == nil {
				t.Fatalf("the released workflow was not re-claimable: wf=%v err=%v", second, err)
			}
			if second.ID != first.ID {
				t.Fatalf("claimed %s, expected the released %s", second.ID, first.ID)
			}

			// worker-1 releases again, with the credentials it used to hold.
			// It must not take the workflow away from worker-2.
			//
			// THE DIALECTS DISAGREE ABOUT HOW A STALE RELEASE REPORTS ITSELF,
			// and that difference is deliberately not asserted here.
			// PostgreSQL and MySQL no-op silently; SQL Server returns "no rows
			// affected for <id>". Both wrote nothing, which is the outcome this
			// test is about -- and an earlier version of it called the error a
			// failure, which made SQL Server red for having the same effect by
			// a different route.
			//
			// It is still a divergence worth someone deciding on: a caller
			// cannot tell "you no longer hold this" from "it worked" on two of
			// three dialects, and cannot write portable code that distinguishes
			// them. Filed as cleat#1223 rather than settled here, because picking the
			// winner is an API decision and this test's subject is the exclusion.
			staleErr := store.ReleaseWorkflow(ctx, first.ID, "worker-1", first.Generation, time.Now().UTC())
			if staleErr != nil {
				t.Logf("stale release reported an error on this dialect (not a failure, see above): %v", staleErr)
			}

			after, err := store.GetWorkflowByID(ctx, first.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			if after.AssignedTo != "worker-2" {
				t.Errorf("after a stale release the workflow is assigned to %q, want "+
					"\"worker-2\".\n\nworker-1 released a claim it no longer held and "+
					"took the workflow from the worker that did. `assigned_to = NULL` "+
					"in the first release is what stops the second matching -- "+
					"`generation` is not bumped here and cannot tell them apart.",
					after.AssignedTo)
			}
		})
	}
}
