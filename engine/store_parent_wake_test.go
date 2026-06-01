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

			// Parent should have next_wake_at <= now (woken atomically).
			parentNextWake := queryWorkflowNextWakeAt(t, store, parentID)
			if parentNextWake.After(time.Now()) {
				t.Errorf("parent next_wake_at was not updated: got %v, want <= now", parentNextWake)
			}
			if time.Since(parentNextWake) > 5*time.Second {
				t.Errorf("parent next_wake_at too old: %v ago", time.Since(parentNextWake))
			}

			// Parent should still be "ready" (not accidentally finalized).
			parentAfter, err := store.GetWorkflowByID(ctx, parentID)
			if err != nil {
				t.Fatalf("GetWorkflowByID (parent): %v", err)
			}
			if parentAfter.Status != "ready" {
				t.Errorf("parent status = %q, want ready", parentAfter.Status)
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
