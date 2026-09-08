package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestFinalizeWorkflowSegment_ParentWake(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			parentID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "pw-parent", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun (parent): %v", err)
			}

			// Claim parent immediately before creating the child so the child is
			// the only ready workflow for the second claim.
			parentWF, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow (parent): %v", err)
			}
			if parentWF == nil || parentWF.ID != parentID {
				t.Fatalf("ClaimWorkflow expected parent %s, got %v", parentID, parentWF)
			}

			childID, err := store.StartChildWorkflow(ctx, parentID, "test-workflow", `{}`, 1, "abandon", 0)
			if err != nil {
				t.Fatalf("StartChildWorkflow: %v", err)
			}

			// Suspend parent with far-future next_wake_at (simulates AwaitChild).
			farFuture := time.Now().Add(1 * time.Hour)
			if err := store.FinalizeWorkflowSegment(ctx, parentID, "worker-1", parentWF.Generation, nil, "ready", "", "", "", nil, farFuture); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (parent suspend): %v", err)
			}

			// Now the child is the only claimable workflow.
			childWF, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow (child): %v", err)
			}
			if childWF == nil || childWF.ID != childID {
				t.Fatalf("ClaimWorkflow expected child %s, got %v", childID, childWF)
			}

			// Complete child. Must atomically wake the parent.
			if err := store.FinalizeWorkflowSegment(ctx, childID, "worker-1", childWF.Generation, nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("FinalizeWorkflowSegment (child done): %v", err)
			}

			// The parent must now be CLAIMABLE. That is what "woken" means, and
			// asserting it directly is what keeps this test off the clock.
			//
			// This used to read next_wake_at back and compare it against
			// time.Now() with a 2s `clockSkewTolerance`, because next_wake_at is
			// written by now() on the database server and the comparison ran on
			// the host -- two clocks, ~50-100ms apart in this sandbox. A
			// tolerance does not fix that, it only sets a threshold for how much
			// disagreement is unremarkable. The mssql leg was observed failing in a
			// full-suite run; which of the two clock assertions tripped was not
			// captured, and the honest statement is that both depend on how long the
			// host takes to get from the finalize to the read. Widening the tolerance
			// again would only move that dependency further out, so it is removed.
			//
			// Every dialect's claim predicate is `next_wake_at <= <server now>`
			// (mssql_lifecycle.go SYSUTCDATETIME, mysql_lifecycle.go NOW(6),
			// postgres now()), so both sides of THAT comparison are the server's
			// own clock and the skew is gone by construction rather than absorbed.
			// It is also the stronger claim: next_wake_at <= now was only ever a
			// proxy for "a worker can pick this up", and the child is finished, so
			// the parent is the only workflow left to claim.
			parentAfter, err := store.GetWorkflowByID(ctx, parentID)
			if err != nil {
				t.Fatalf("GetWorkflowByID (parent): %v", err)
			}
			// Checked before the claim, which would itself move it to running.
			if parentAfter.Status != "ready" {
				t.Errorf("parent status = %q, want ready", parentAfter.Status)
			}

			woken, err := store.ClaimWorkflow(ctx, "worker-2")
			if err != nil {
				t.Fatalf("ClaimWorkflow (parent after child completed): %v", err)
			}
			if woken == nil {
				// The pre-wake next_wake_at is an hour out, so this is exactly
				// the symptom of finalize_workflow_status's parent-wake UPDATE
				// not running or not matching -- the bug this test guards.
				t.Fatalf("no workflow claimable after child completed: parent %s was not woken", parentID)
			}
			if woken.ID != parentID {
				t.Fatalf("ClaimWorkflow returned %s, want woken parent %s", woken.ID, parentID)
			}
		})
	}
}

func TestFinalizeWorkflowSegment_ParentWake_NoParent(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			setupTestData(t, store)
			truncateAll(t, store)
			ctx := context.Background()

			childID, _, err := store.StartNewRun(ctx, "", "test-workflow", 1, json.RawMessage(`{}`), "no-parent-test", DefaultTenantUUID, 0)
			if err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}

			childWF, err := store.ClaimWorkflow(ctx, "worker-1")
			if err != nil {
				t.Fatalf("ClaimWorkflow: %v", err)
			}

			if err := store.FinalizeWorkflowSegment(ctx, childID, "worker-1", childWF.Generation, nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != nil {
				t.Fatalf("FinalizeWorkflowSegment should succeed: %v", err)
			}
		})
	}
}
