package engine

import (
	"context"
	"testing"
	"time"
)

// Can the reaper see a workflow that is asleep? It cannot, and that is the
// design rather than an oversight. cleat#1429.
//
// ReleaseWorkflow parks a sleeping workflow at status='ready',
// assigned_to=NULL, next_wake_at=<when>; ReapStaleInstances matches
// status='running'. A parked row is unowned, so there is no claim to reclaim
// and nothing stale about it -- it resumes through the ordinary claim
// predicate when its time comes, whichever worker gets there first.
//
// The consequence is easy to get backwards, and a ports test currently does:
// killing the worker of a workflow that is mid-DurableSleep reclaims nothing,
// because the workflow gave up its claim before the kill. Such a test is
// exercising resumption-from-sleep, not crash recovery, and will pass with the
// reaper switched off entirely.
//
// Two assertions carry this, and NEITHER is the obvious one.
//
// STATUS CANNOT TELL THE TWO ROWS APART, and reaching for it is the natural
// mistake -- it is what this test did first. Reclaiming sets status back to
// 'ready', which is exactly where a sleeping workflow already sits, so after
// the sweep both rows read "ready". A status-based assertion is therefore
// satisfied by the reaper taking the SLEEPER and leaving the control: the
// precise inversion of the property under test. reclaim_count is the
// discriminator, 0 against 1.
//
// THE CONTROL IS THE OTHER HALF. Without a row the reaper should take, "the
// sleeper was not reclaimed" and "the reaper did nothing" are one observation.
// The Fatal on n == 0 is what separates them.
//
// Probe written and measured on all three dialects by the WS-3 session.
func TestTheReaperDoesNotReclaimASleepingWorkflow(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// Called straight off WorkflowStore: both ReapStaleInstances and
			// ReleaseWorkflow are on that interface, so the defensive type
			// assertions this started with could never fail. Two skips that
			// cannot fire are two passes wearing a skip's clothes, and
			// scripts/check-skips.sh is right to refuse them -- its case (c),
			// "the precondition is always satisfiable in this repo".

			// ONE ROW, BOTH STATES. An earlier version used two rows -- a
			// sleeper and a separate running control -- and it failed on CI's
			// SQL Server with "reclaimed 0" while passing locally on all three
			// dialects. That shape cannot say why: the two rows differ in
			// tenant, in heartbeat provenance, and in their position under the
			// sweep's ORDER BY heartbeat_at / TOP(n), so any per-row difference
			// in the environment breaks the control ambiguously.
			//
			// Using the SAME row in both states removes every cross-row
			// variable. If it is reclaimable while running and not while
			// parked, the status arm is what did it, and nothing else can be
			// blamed. And a first sweep that fails to reclaim now says exactly
			// one thing -- this environment cannot reap at all -- rather than
			// leaving a difference between two rows to be guessed at.
			wfID := newIntentWorkflow(t, ctx, store, "reaper-1429")

			// STATE 1: running, owned, heartbeat stale (timeout 0).
			if _, err := claimSpecific(t, ctx, store, wfID, "w-dead"); err != nil {
				t.Fatalf("claim while running: %v", err)
			}
			n1, err := store.ReapStaleInstances(ctx, 0, 100)
			if err != nil {
				t.Fatalf("ReapStaleInstances (running): %v", err)
			}
			rc1 := mustGetWorkflow(t, ctx, store, wfID).ReclaimCount
			t.Logf("running: swept %d, this row reclaim_count=%d", n1, rc1)

			// The control, and it is now about THIS row rather than a sibling.
			if rc1 != 1 {
				t.Fatalf("the reaper did not reclaim this workflow while it was "+
					"status='running' with a stale heartbeat (reclaim_count=%d, "+
					"swept %d). Nothing below measures anything until this holds.",
					rc1, n1)
			}

			// STATE 2: the same row, parked by ReleaseWorkflow the way a
			// DurableSleep parks it.
			wf, err := claimSpecific(t, ctx, store, wfID, "w-sleep")
			if err != nil {
				t.Fatalf("re-claim before parking: %v", err)
			}
			if err := store.ReleaseWorkflow(ctx, wfID, "w-sleep", wf.Generation,
				time.Now().Add(45*time.Second)); err != nil {
				t.Fatalf("ReleaseWorkflow (the DurableSleep park): %v", err)
			}
			before := mustGetWorkflow(t, ctx, store, wfID).ReclaimCount

			n2, err := store.ReapStaleInstances(ctx, 0, 100)
			if err != nil {
				t.Fatalf("ReapStaleInstances (parked): %v", err)
			}
			after := mustGetWorkflow(t, ctx, store, wfID)
			t.Logf("parked:  swept %d, this row reclaim_count %d -> %d, status=%q",
				n2, before, after.ReclaimCount, after.Status)

			// Status cannot carry this: reclaiming sets status to 'ready', which
			// is exactly where parking already put it, so the row reads "ready"
			// either way. Only reclaim_count distinguishes them.
			if after.ReclaimCount != before {
				t.Errorf("the PARKED workflow was reclaimed (reclaim_count %d -> %d); "+
					"the reaper can see a parked row after all", before, after.ReclaimCount)
			}
		})
	}
}
