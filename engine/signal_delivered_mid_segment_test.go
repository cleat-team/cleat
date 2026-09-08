package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestASignalDeliveredWhileTheWorkflowIsAwakeStillWakesIt is cleat#953.
//
// A signal delivered to a RUNNING workflow scheduled no wake. DeliverSignal
// pulls next_wake_at forward only for a workflow that is already suspended --
//
//	WHERE id = $1 AND status IN ('ready', 'suspended')
//
// and a claimed workflow is 'running', so that matched zero rows. The workflow
// then re-suspended and finalize wrote its own timeout deadline over
// next_wake_at, so the delivery sat in workflow_signals until the timeout
// expired: AwaitSignals reported a timeout with the signal it was waiting for
// already in the table.
//
// signal_seq is bumped by every delivery and signal_seq_at_claim is stamped by
// every claim, so a difference means a delivery landed mid-segment. finalize
// compares them IN ITS OWN TRANSACTION, which is why this shape was chosen
// over a post-commit poll: the poll closes the same window in the steady state
// -- measured, four interleavings, no gap -- but a worker dying between the
// commit and the poll loses that wake until the caller's own timeout.
//
// THE CONTROL IS THE HALF THAT MATTERS. The obvious version of this fix is
// "wake if workflow_signals has any row", and it SPINS: a workflow awaiting
// {a} with an unrelated {z} pending would wake, poll, find nothing it wants,
// re-suspend, and repeat forever. A counter only moves on a NEW delivery. The
// no-delivery case below is what tells those two implementations apart, and
// without it this test passes against the one that burns a core.
func TestASignalDeliveredWhileTheWorkflowIsAwakeStillWakesIt(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			d := dialectOf(t, store)
			db := rawDBOf(t, store)
			sig, ok := store.(SignalStore)
			if !ok {
				t.Fatalf("%T is not a SignalStore", store)
			}
			_ = newIntentWorkflow(t, ctx, store, "mid-segment-seed")

			// deadline is far enough out that "woke immediately" and "slept to
			// its deadline" cannot be confused by scheduling noise, and the
			// assertion is on which SIDE of the midpoint the wake lands rather
			// than on any particular duration -- no wall-clock tolerance to
			// tune, and nothing that gets flaky under load.
			const deadline = 60 * time.Second

			runSegment := func(label string, deliver bool) time.Duration {
				t.Helper()
				id := fmt.Sprintf("mid-segment-%s-%d", label, time.Now().UnixNano())
				if _, _, err := store.StartNewRun(ctx, id, "intent-workflow", 1,
					json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
					t.Fatalf("StartNewRun: %v", err)
				}
				wf, err := claimSpecific(t, ctx, store, id, "worker-mid-segment")
				if err != nil {
					t.Fatalf("claiming %s: %v", id, err)
				}
				// THE WINDOW: the workflow is claimed, so status is 'running'
				// and DeliverSignal's own wake update cannot match it.
				if deliver {
					if err := sig.DeliverSignal(ctx, id, "a", `{"p":1}`); err != nil {
						t.Fatalf("DeliverSignal: %v", err)
					}
				}
				if err := store.FinalizeWorkflowSegment(ctx, id, "worker-mid-segment",
					wf.Generation, nil, "ready", "", "", "",
					map[string]string{}, time.Now().Add(deadline)); err != nil {
					t.Fatalf("FinalizeWorkflowSegment: %v", err)
				}
				var wake time.Time
				q := "SELECT next_wake_at FROM workflow_instances WHERE id = " + d.placeholder(1)
				if err := db.QueryRow(q, id).Scan(&wake); err != nil {
					t.Fatalf("reading next_wake_at: %v", err)
				}
				return time.Until(wake)
			}

			if got := runSegment("delivered", true); got > deadline/2 {
				t.Errorf("a signal delivered while the workflow was RUNNING left it sleeping "+
					"for %v, its own %v timeout.\n\n"+
					"The delivery is durable in workflow_signals and the workflow is waiting "+
					"for exactly it, so AwaitSignals will report a timeout with the signal "+
					"already in the table. DeliverSignal's next_wake_at update cannot see a "+
					"'running' row, and finalize overwrites next_wake_at with the deadline -- "+
					"signal_seq differing from signal_seq_at_claim is what closes that.",
					got.Round(time.Second), deadline)
			}

			// CONTROL: no delivery, so nothing should have moved the deadline.
			if got := runSegment("CONTROL-no-delivery", false); got <= deadline/2 {
				t.Errorf("CONTROL FAILED: a workflow with NO delivery woke after %v instead of "+
					"sleeping to its %v deadline.\n\n"+
					"That is the spin the counter exists to avoid: an implementation that "+
					"wakes whenever workflow_signals is non-empty, or one that always wakes, "+
					"passes the assertion above and burns a core here.",
					got.Round(time.Second), deadline)
			}
		})
	}
}

// TestABurstOfSignalsIsDrainedRatherThanSleptThrough is the case cleat#953's
// first fix did not handle, and the reason it did not is the point.
//
// signal_seq_at_claim detects "a delivery arrived DURING my segment". A burst
// arrives BEFORE the claim, so the stamp is taken at the already-moved value,
// nothing changes during the segment, and finalize schedules the full deadline
// over deliveries still sitting in workflow_signals. Measured, before the
// second counter existed:
//
//	seg2: claimed     seq=3 at_claim=3
//	seg2: finalized   seq=3 at_claim=3  wake_in=1m0s   <-- 2 signals pending
//
// THE AXIS, NOT THE MECHANISM, IS WHAT WAS MISSING. The first regression test
// delivered ONE signal in ONE segment, and a mutation that made finalize always
// wake was correctly caught by its control. Neither asks whether more than one
// signal in flight is handled: every mutation is evaluated at the same inputs
// the test already uses, so a suite with every axis at N=1 gets a clean
// mutation report AND a clean review, both honest, both silent about this.
//
// The fix is PROGRESS, not examination: a segment that consumed something can
// consume again, so waking it is productive and bounded by the queue depth; a
// segment that consumed nothing has already shown it wants nothing that is
// there, which is why the unrelated-pending-signal case cannot spin.
func TestABurstOfSignalsIsDrainedRatherThanSleptThrough(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			d := dialectOf(t, store)
			db := rawDBOf(t, store)
			sig := store.(SignalStore)
			consumer, ok := store.(interface {
				ConsumeSignal(context.Context, string, int64) error
			})
			if !ok {
				t.Fatalf("%T cannot consume signals", store)
			}
			_ = newIntentWorkflow(t, ctx, store, "burst-seed")

			const deadline = 60 * time.Second
			const burst = 3

			id := fmt.Sprintf("burst-%s-%d", backend.Name(), time.Now().UnixNano())
			if _, _, err := store.StartNewRun(ctx, id, "intent-workflow", 1,
				json.RawMessage(`{}`), "", DefaultTenantUUID, 0); err != nil {
				t.Fatalf("StartNewRun: %v", err)
			}
			wakeIn := func() time.Duration {
				var w time.Time
				q := "SELECT next_wake_at FROM workflow_instances WHERE id = " + d.placeholder(1)
				if err := db.QueryRow(q, id).Scan(&w); err != nil {
					t.Fatalf("reading next_wake_at: %v", err)
				}
				return time.Until(w)
			}
			pending := func() int {
				var n int
				q := "SELECT count(*) FROM workflow_signals WHERE workflow_id = " + d.placeholder(1)
				if err := db.QueryRow(q, id).Scan(&n); err != nil {
					t.Fatalf("counting signals: %v", err)
				}
				return n
			}
			// One segment: claim, optionally consume one, finalize.
			segment := func(consume bool) {
				t.Helper()
				wf, err := claimSpecific(t, ctx, store, id, "worker-burst")
				if err != nil {
					t.Fatalf("claiming %s: %v", id, err)
				}
				if consume {
					if dlv, found, err := sig.PollSignal(ctx, id, "tick"); err == nil && found {
						if err := consumer.ConsumeSignal(ctx, id, dlv.ID); err != nil {
							t.Fatalf("ConsumeSignal: %v", err)
						}
					}
				}
				if err := store.FinalizeWorkflowSegment(ctx, id, "worker-burst", wf.Generation,
					nil, "ready", "", "", "", map[string]string{},
					time.Now().Add(deadline)); err != nil {
					t.Fatalf("FinalizeWorkflowSegment: %v", err)
				}
			}

			// The burst lands while the workflow is running, then it suspends.
			wf, err := claimSpecific(t, ctx, store, id, "worker-burst")
			if err != nil {
				t.Fatalf("claiming %s: %v", id, err)
			}
			for i := 0; i < burst; i++ {
				if err := sig.DeliverSignal(ctx, id, "tick", `{}`); err != nil {
					t.Fatalf("DeliverSignal %d: %v", i, err)
				}
			}
			if err := store.FinalizeWorkflowSegment(ctx, id, "worker-burst", wf.Generation, nil,
				"ready", "", "", "", map[string]string{}, time.Now().Add(deadline)); err != nil {
				t.Fatalf("FinalizeWorkflowSegment: %v", err)
			}

			// Each subsequent segment consumes ONE and must be woken again
			// while any remain -- the workflow drains the burst rather than
			// sleeping on it.
			for i := 1; i < burst; i++ {
				segment(true)
				if left := pending(); left == 0 {
					t.Fatalf("segment %d drained the whole queue; the fixture is not testing a burst", i)
				}
				if got := wakeIn(); got > deadline/2 {
					t.Fatalf("after consuming %d of %d, %d signal(s) still pending and the "+
						"workflow is asleep for %v.\n\n"+
						"A burst arrives BEFORE the claim, so signal_seq_at_claim is stamped "+
						"at the already-moved value and no delivery lands DURING the segment. "+
						"What must wake it is that this segment consumed something and more "+
						"remains: a segment that consumed once can consume again.",
						i, burst, pending(), got.Round(time.Second))
				}
			}

			// Drain the last one, then TERMINATION: the wasted wake must not
			// repeat. The whole design rests on this, and the mechanism it
			// replaced also terminated correctly for a single signal -- so it
			// is asserted rather than reasoned about.
			segment(true)
			if left := pending(); left != 0 {
				t.Fatalf("queue not drained after %d segments: %d left", burst, left)
			}
			segment(false) // the one wasted wake: consumes nothing
			if got := wakeIn(); got <= deadline/2 {
				t.Errorf("a segment that consumed NOTHING with an empty queue was woken again "+
					"after %v instead of sleeping to its %v deadline.\n\n"+
					"That is the spin this design exists to avoid: waking on progress "+
					"terminates because each wake consumes one, but only if a segment that "+
					"made no progress stops the chain.", got.Round(time.Second), deadline)
			}
		})
	}
}
