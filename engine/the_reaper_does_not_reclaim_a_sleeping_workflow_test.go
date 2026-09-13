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

			reaper, ok := store.(interface {
				ReapStaleInstances(ctx context.Context, timeout time.Duration, limit int) (int, error)
			})
			if !ok {
				t.Skipf("%T has no ReapStaleInstances", store)
			}
			releaser, ok := store.(interface {
				ReleaseWorkflow(ctx context.Context, workflowID, workerID string, generation int64, nextWakeAt time.Time) error
			})
			if !ok {
				t.Skipf("%T has no ReleaseWorkflow", store)
			}

			// SUBJECT: a workflow that went to sleep.
			asleep := newIntentWorkflow(t, ctx, store, "reaper-1429-asleep")
			wf, err := claimSpecific(t, ctx, store, asleep, "w-sleep")
			if err != nil {
				t.Fatalf("claim the sleeper: %v", err)
			}
			if err := releaser.ReleaseWorkflow(ctx, asleep, "w-sleep", wf.Generation,
				time.Now().Add(45*time.Second)); err != nil {
				t.Fatalf("ReleaseWorkflow (the DurableSleep park): %v", err)
			}

			// CONTROL: a workflow still held, whose worker died.
			running := newIntentWorkflow(t, ctx, store, "reaper-1429-running")
			if _, err := claimSpecific(t, ctx, store, running, "w-dead"); err != nil {
				t.Fatalf("claim the control: %v", err)
			}

			// timeout 0: every heartbeat is stale, so nothing is excluded by age
			// and the only thing deciding the outcome is the status predicate.
			n, err := reaper.ReapStaleInstances(ctx, 0, 100)
			if err != nil {
				t.Fatalf("ReapStaleInstances: %v", err)
			}
			t.Logf("reclaimed %d", n)
			t.Logf("  asleep  -> status=%q", mustGetWorkflow(t, ctx, store, asleep).Status)
			t.Logf("  running -> status=%q", mustGetWorkflow(t, ctx, store, running).Status)

			// The control first: without it, "the sleeper was not reclaimed" is
			// equally consistent with "the reaper reclaimed nothing at all".
			if n == 0 {
				t.Fatalf("the reaper reclaimed NOTHING, including the control that is " +
					"status='running' with a stale heartbeat. This test measured nothing.")
			}
			if n != 1 {
				t.Errorf("expected exactly the control to be reclaimed, got %d", n)
			}

			// WHICH row moved, not how many. See the header: status is blind here.
			sleptRC := mustGetWorkflow(t, ctx, store, asleep).ReclaimCount
			runRC := mustGetWorkflow(t, ctx, store, running).ReclaimCount
			t.Logf("  reclaim_count: asleep=%d running=%d", sleptRC, runRC)
			if sleptRC != 0 {
				t.Errorf("the SLEEPING workflow was reclaimed (reclaim_count=%d); the "+
					"reaper can see a parked row after all", sleptRC)
			}
			if runRC != 1 {
				t.Errorf("the running workflow's reclaim_count is %d, want 1 -- the "+
					"count of 1 above was not this row", runRC)
			}
		})
	}
}
