package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestARetentionPreviewAgreesWithTheSweepAndDeletesNothing is cleat#1457.
//
// Retention was the only one of cleat's destructive operations without a
// preview -- drop-tenant, revokeapikey, the version GC and --uninstall-dry-run
// all have one -- and it is the least reversible, since none of the others
// deletes event history.
//
// TWO ASSERTIONS, AND NEITHER IS SUFFICIENT ALONE:
//
//	the preview equals what the sweep then deletes   -- it is not a model
//	the preview changed nothing                      -- it is a preview
//
// A preview that reported the right number by deleting the rows would pass the
// first. One that returned a constant 0 and touched nothing would pass the
// second. Only together do they say "reported the truth without acting".
//
// THE POSITIVE CONTROL IS THE THIRD REQUIREMENT and it is why this seeds rows
// rather than asserting against whatever the database happens to hold. Every
// arm of this preview reports 0 on an empty table, and so does a preview whose
// predicate is wrong, whose tenant scoping is wrong, or which is not wired up at
// all. A zero is only evidence once something was deliberately made to match.
func TestARetentionPreviewAgreesWithTheSweepAndDeletesNothing(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			setupTestData(t, store)
			truncateAll(t, store)

			const defName = "retention-preview"
			if err := store.DeployWorkflowDef(ctx, &WorkflowDef{
				Name: defName, Version: 1, WASMBytes: []byte{0x00, 0x61, 0x73, 0x6d},
				ABIVersion: 1, MinVersion: 1,
			}); err != nil {
				t.Fatalf("DeployWorkflowDef: %v", err)
			}

			// Seed terminal runs and age them past the cutoff. completed_at is
			// set through the store's own finalize rather than by hand, so the
			// rows look exactly like rows retention would find in production.
			const seeded = 3
			for i := 0; i < seeded; i++ {
				id := fmt.Sprintf("prev-%d-%d", i, time.Now().UnixNano())
				if _, _, err := store.StartNewRun(ctx, id, defName, 1,
					json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun: %v", err)
				}
				claimed, err := store.ClaimWorkflow(ctx, "preview-worker")
				if err != nil || claimed == nil {
					t.Fatalf("ClaimWorkflow: %v %v", claimed, err)
				}
				if err := store.FinalizeWorkflowSegment(ctx, claimed.ID, "preview-worker",
					claimed.Generation, nil, "done", `{"ok":true}`, "", "", nil, time.Time{}); err != nil {
					t.Fatalf("FinalizeWorkflowSegment: %v", err)
				}
			}

			// DECOYS THE COMPLETED ARM MUST EXCLUDE, and without them this test
			// cannot detect the failure it exists to detect.
			//
			// Seeding only rows that match means a LOOSER predicate counts them
			// too, so a preview that dropped the status filter entirely would
			// agree with the sweep and pass. Measured: replacing the shared
			// predicate with a separately-written `completed_at < $1` -- exactly
			// the divergence cleat#1457 forbids -- passed this test before these
			// rows existed.
			//
			// A dead-lettered run is the decoy that discriminates: it is
			// terminal, it is old, it has its own retention arm, and the
			// completed arm must not touch it.
			deadID := fmt.Sprintf("prev-dead-%d", time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, deadID, defName, 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun (decoy): %v", err)
			}
			dclaimed, err := store.ClaimWorkflow(ctx, "preview-worker")
			if err != nil || dclaimed == nil {
				t.Fatalf("ClaimWorkflow (decoy): %v %v", dclaimed, err)
			}
			if err := store.MoveToDeadLetterQueue(ctx, dclaimed.ID, "preview-worker",
				dclaimed.Generation, "decoy", "", ""); err != nil {
				t.Fatalf("MoveToDeadLetterQueue (decoy): %v", err)
			}

			// A cutoff in the FUTURE rather than ageing the rows, so this needs
			// no per-dialect date arithmetic and no UPDATE of its own fixture.
			cutoff := time.Now().Add(time.Hour)

			// The decoy must be visible to ITS OWN arm, or "the completed arm
			// excluded it" is satisfied by a row that does not exist.
			if n, err := store.CountDeadLetteredWorkflows(ctx, cutoff); err != nil {
				t.Fatalf("CountDeadLetteredWorkflows: %v", err)
			} else if n == 0 {
				t.Fatal("the dead-lettered decoy is not visible to the dead-lettered " +
					"preview arm, so its absence from the completed arm proves nothing")
			}

			before, err := store.CountCompletedWorkflows(ctx, cutoff)
			if err != nil {
				t.Fatalf("CountCompletedWorkflows: %v", err)
			}
			if before != seeded {
				t.Errorf("the completed preview reports %d with %d completed runs and one "+
					"dead-lettered decoy seeded.\n\nCounting the decoy means the preview's "+
					"predicate is looser than the sweep's -- it would tell an operator "+
					"that retention is about to delete a row it will not touch.",
					before, seeded)
			}
			if before == 0 {
				t.Fatalf("the preview counted 0 completed workflows with %d seeded to "+
					"match.\n\nEvery arm reports 0 on an empty table, and so does a "+
					"broken predicate, wrong tenant scoping, or a preview that is not "+
					"wired up -- so this test would pass without measuring anything. "+
					"Fix the count before trusting any zero it reports.", seeded)
			}

			// A preview must not change what a second preview sees.
			again, err := store.CountCompletedWorkflows(ctx, cutoff)
			if err != nil {
				t.Fatalf("CountCompletedWorkflows (repeat): %v", err)
			}
			if again != before {
				t.Errorf("two consecutive previews disagree: %d then %d. The preview is "+
					"not read-only.", before, again)
			}

			deleted, err := store.DeleteCompletedWorkflows(ctx, cutoff)
			if err != nil {
				t.Fatalf("DeleteCompletedWorkflows: %v", err)
			}
			if deleted != before {
				t.Errorf("the preview said %d and the sweep deleted %d.\n\n"+
					"cleat#1457 requires the preview to share the sweep's predicate "+
					"rather than re-implement it; a disagreement here means the two "+
					"have drifted, which is the failure that decision exists to "+
					"prevent -- and it fails silently, in the reassuring direction.",
					before, deleted)
			}

			after, err := store.CountCompletedWorkflows(ctx, cutoff)
			if err != nil {
				t.Fatalf("CountCompletedWorkflows (after): %v", err)
			}
			if after != 0 {
				t.Errorf("after the sweep the preview still reports %d, want 0", after)
			}

			// The other three arms run for real rather than being assumed. They
			// report 0 here -- nothing seeded matches them -- and what is being
			// checked is that each EXECUTES: a preview arm whose SQL does not
			// parse on this dialect returns an error, and an error is what this
			// would otherwise never surface, since 0 is also the right answer.
			for _, arm := range []struct {
				name string
				fn   func(context.Context, time.Time) (int64, error)
			}{
				{"expired events", store.CountExpiredEvents},
				{"expired compaction state", store.CountExpiredCompactionState},
				{"dead-lettered workflows", store.CountDeadLetteredWorkflows},
			} {
				if _, err := arm.fn(ctx, cutoff); err != nil {
					t.Errorf("%s preview failed on %s: %v", arm.name, backend.Name(), err)
				}
			}
		})
	}
}
