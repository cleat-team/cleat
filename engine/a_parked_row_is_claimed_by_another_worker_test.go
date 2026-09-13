package engine

import (
	"context"
	"testing"
	"time"
)

// TestAParkedRowIsClaimedByAnotherWorker is the engine half of cleat#1429's
// first outstanding run.
//
// #1436 established that the reaper CANNOT see a sleeping workflow: it parks
// at status='ready', assigned_to=NULL, and ReapStaleInstances matches
// status='running'. That answers "nothing is reclaimed". It does not answer
// the question the ports recovery test actually turns on, which is how the
// workflow resumes at all once its worker is gone.
//
// The reading is that ANY worker takes it at next_wake_at through the ordinary
// claim predicate -- status IN ('ready','terminating') AND next_wake_at <=
// now(). If so, worker.restart() is not load-bearing in that test, and what it
// exercises is resumption-from-sleep rather than crash recovery.
//
// This measures that. It is the ENGINE half only: it does not run the ports
// harness, so it cannot settle what that suite is pinning -- see #1429.
func TestAParkedRowIsClaimedByAnotherWorker(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			id := newIntentWorkflow(t, ctx, store, "parked-then-claimed")

			// Worker A claims it, then parks it with a wake time ALREADY PAST --
			// the sleep having elapsed, which is the state the ports test reaches
			// by waiting. A future wake time would test the scheduler's patience
			// rather than who may claim.
			first, err := claimSpecific(t, ctx, store, id, "worker-A")
			if err != nil {
				t.Fatalf("worker-A claim: %v", err)
			}
			if err := store.ReleaseWorkflow(ctx, id, "worker-A", first.Generation,
				time.Now().Add(-time.Second)); err != nil {
				t.Fatalf("park (the DurableSleep release): %v", err)
			}

			// Worker A is now "dead": it never comes back. Worker B is a
			// different worker id and has never touched this row.
			second, err := claimSpecific(t, ctx, store, id, "worker-B")
			if err != nil {
				t.Fatalf("worker-B could not claim the parked row: %v -- if a parked "+
					"row needs its original worker, the ports recovery test really "+
					"does depend on the restart (cleat#1429)", err)
			}
			if second == nil {
				t.Fatal("worker-B claimed nothing")
			}
			t.Logf("worker-B claimed it: assigned_to now %q, generation %d -> %d",
				second.AssignedTo, first.Generation, second.Generation)

			if second.AssignedTo != "worker-B" {
				t.Errorf("the row is assigned to %q, want worker-B", second.AssignedTo)
			}

			// The claim must be a real handover, not a no-op read: worker-A's
			// generation must no longer be current, or a stale A coming back
			// could still write.
			if second.Generation == first.Generation {
				t.Errorf("generation did not move (%d): worker-A's fence is still "+
					"valid, so this was not a handover", first.Generation)
			}

			// CONTROL, and without it the assertions above are satisfied by an
			// engine that hands any row to anyone who asks. Park a SECOND row
			// with a wake time in the FUTURE: worker-B must NOT get it, or the
			// test above is measuring "ClaimWorkflow returns something" rather
			// than the next_wake_at predicate the whole argument rests on.
			asleep := newIntentWorkflow(t, ctx, store, "parked-in-the-future")
			sleeper, err := claimSpecific(t, ctx, store, asleep, "worker-A")
			if err != nil {
				t.Fatalf("worker-A claim (control): %v", err)
			}
			if err := store.ReleaseWorkflow(ctx, asleep, "worker-A", sleeper.Generation,
				time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("park the control an hour out: %v", err)
			}
			// DRAIN the queue, and fail if the bound is hit rather than stopping
			// quietly. The first version looped a fixed 20 times and treated
			// running out of iterations the same as running out of rows -- so in
			// a database holding more claimable work than the bound, it would
			// exit without ever reaching this row and the assertion below would
			// hold trivially. The control would pass having tested nothing.
			//
			// That is the shape cleat#1447 is chasing next door: a sweep bounded
			// at TOP (100) over a row that sorts last. Setup truncates, so on a
			// clean database two iterations suffice -- but "it is clean today" is
			// the assumption, not the guarantee, and the bound must say which
			// case it is in.
			const maxDrain = 200
			drained := 0
			for ; drained < maxDrain; drained++ {
				got, err := store.ClaimWorkflow(ctx, "worker-B")
				if err != nil {
					t.Fatalf("control claim: %v", err)
				}
				if got == nil {
					break
				}
				if got.ID == asleep {
					t.Fatalf("worker-B claimed a row parked an HOUR in the future; "+
						"next_wake_at is not gating the claim, so the handover above "+
						"proves nothing about resumption timing (workflow %s)", asleep)
				}
			}
			if drained == maxDrain {
				t.Fatalf("drained %d claimable rows without emptying the queue, so this "+
					"control never established that %s was unreachable -- it ran out of "+
					"iterations, which is not the same as running out of rows",
					maxDrain, asleep)
			}
			t.Logf("control: queue drained after %d claim(s); the future-parked row was "+
				"never offered", drained)
		})
	}
}
