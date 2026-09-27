package engine

import (
	"context"
	"testing"
)

// cleat#1153, on the repository owner's decision of 2026-09-10: add pre-emptive
// cancellation, "with the explicit constraint that a cancelled workflow which
// has registered defers must still run them".
//
// WHAT WAS WRONG. Cancellation was cooperative AND unobservable. RequestCancellation
// set a flag; stopping and reporting were left entirely to the workflow, and
// 'cancelled' was an ErrorCode rather than a status the engine ever wrote. So a
// workflow that honoured a cancellation and one that simply ran to completion
// were both 'done', and an operator could not answer "did this stop because I
// asked it to?".
//
// THREE ARMS, AND EACH IS LOAD-BEARING:
//
//	defers owed      MARK, do not finalize -- the owner's hard requirement
//	no defers owed   settle in one step -- the control
//	end to end       the recorded outcome is actually APPLIED as 'cancelled'
//
// The second arm is the control and matters as much as the first: routing
// unconditionally through MARK/FINALIZE would turn every cancel into an
// asynchronous operation, and a test that only checked the defer case would not
// notice. That is the shape cleat#1152 established for force-resolve, and this
// follows it deliberately rather than inventing a second one.
//
// The third arm is what the first two cannot say. A workflow parked in
// 'terminating' with 'cancelled' recorded is a promise, not an outcome; if
// FinalizeDeferPhase refused the value -- it does not, it copies
// pending_terminal_status verbatim on all three dialects -- the first arm would
// still pass and no run would ever reach 'cancelled'.
func TestCancellationIsAnOutcomeAndItRunsItsDefers(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			t.Run("a defer owed is marked, not finalized", func(t *testing.T) {
				id := newIntentWorkflow(t, ctx, store, "cancel-defer")
				if _, err := claimSpecific(t, ctx, store, id, "w-cancel"); err != nil {
					t.Fatalf("claim: %v", err)
				}
				// A registered defer is a 'defer' event in history -- the same
				// fact deferPhaseOwed reads.
				if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
					{Step: 0, EventType: EventTypeDefer, Service: "cleanup", Op: "release"},
				}); err != nil {
					t.Fatalf("register defer: %v", err)
				}

				if err := store.CancelWorkflow(ctx, id, "operator cancelled"); err != nil {
					t.Fatalf("CancelWorkflow: %v", err)
				}

				got := mustGetWorkflow(t, ctx, store, id)
				if got.Status != statusTerminating {
					t.Errorf("status is %q, want %q -- the outcome was applied directly and the "+
						"defers this workflow owes will never run, which is the one constraint "+
						"the owner attached to this feature", got.Status, statusTerminating)
				}
				if got.PendingTerminalStatus != statusCancelled {
					t.Errorf("pending_terminal_status is %q, want %q -- the intended outcome was "+
						"not recorded, so the finalize has nothing to apply and this run cannot "+
						"end as cancelled", got.PendingTerminalStatus, statusCancelled)
				}
			})

			t.Run("no defer owed settles in one step", func(t *testing.T) {
				id := newIntentWorkflow(t, ctx, store, "cancel-nodefer")
				if _, err := claimSpecific(t, ctx, store, id, "w-cancel2"); err != nil {
					t.Fatalf("claim: %v", err)
				}

				if err := store.CancelWorkflow(ctx, id, "operator cancelled"); err != nil {
					t.Fatalf("CancelWorkflow: %v", err)
				}

				got := mustGetWorkflow(t, ctx, store, id)
				if got.Status != statusCancelled {
					t.Errorf("status is %q, want %q -- a workflow owing no defer phase must "+
						"settle in ONE step; routing it through MARK/FINALIZE makes every "+
						"cancel asynchronous for no reason", got.Status, statusCancelled)
				}
				// THE POINT OF THE ISSUE, asserted rather than implied: this is
				// the comparison an operator could not make before, because a
				// cancelled run and a completed one were both 'done'.
				if got.Status == statusDone {
					t.Errorf("a cancelled run still reports %q -- indistinguishable from one that "+
						"simply finished, which is the whole of cleat#1153", statusDone)
				}
				if got.CompletedAt == nil {
					t.Error("completed_at is nil on a settled run: every retention sweep is gated " +
						"on it, so the row and its event history are never collected (cleat#867)")
				}
			})

			t.Run("end to end: the defer phase applies the recorded outcome", func(t *testing.T) {
				id := newIntentWorkflow(t, ctx, store, "cancel-e2e")
				if _, err := claimSpecific(t, ctx, store, id, "w-e2e"); err != nil {
					t.Fatalf("claim: %v", err)
				}
				if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
					{Step: 0, EventType: EventTypeDefer, Service: "cleanup", Op: "release"},
				}); err != nil {
					t.Fatalf("register defer: %v", err)
				}
				if err := store.CancelWorkflow(ctx, id, "operator cancelled"); err != nil {
					t.Fatalf("CancelWorkflow: %v", err)
				}

				// The MARK bumped generation and cleared assigned_to, so the
				// defer phase is claimed like any other work. That re-claim is
				// exactly why pre-emption takes effect at the next claim rather
				// than interrupting a call in flight.
				wf, err := claimSpecific(t, ctx, store, id, "w-defer-runner")
				if err != nil {
					t.Fatalf("claim defer phase: %v", err)
				}
				// THE DISCRIMINATOR IS pending_terminal_status, NOT status, and
				// that is measured rather than assumed -- the first version of
				// this assertion checked for 'terminating' and failed on all
				// three dialects reporting "running". Claiming a workflow sets
				// status = 'running' whatever it was before, so by the time a
				// worker holds a defer phase the marker is the only thing that
				// says so. cmd/cleat-worker/setup.go agrees: `deferPhase :=
				// wf.PendingTerminalStatus != ""`.
				if wf.PendingTerminalStatus != statusCancelled {
					t.Fatalf("claimed a run whose pending_terminal_status is %q, want %q -- this "+
						"arm is not measuring the defer phase it thinks it is",
						wf.PendingTerminalStatus, statusCancelled)
				}

				// FinalizeDeferPhase is on DeferPhaseStore, not WorkflowStore.
				// Asserted rather than assumed: if a backend did not implement
				// it, a silent skip here would leave this arm measuring nothing
				// while reporting a pass.
				dp, ok := store.(DeferPhaseStore)
				if !ok {
					t.Fatalf("%T does not implement DeferPhaseStore, so this arm cannot run -- "+
						"UNMEASURED, not a pass", store)
				}
				if err := dp.FinalizeDeferPhase(ctx, id, "w-defer-runner", wf.Generation, nil); err != nil {
					t.Fatalf("FinalizeDeferPhase: %v", err)
				}

				got := mustGetWorkflow(t, ctx, store, id)
				if got.Status != statusCancelled {
					t.Errorf("after its defer phase the run is %q, want %q -- the outcome was "+
						"recorded but never applied, so a cancelled workflow with defers never "+
						"reaches a terminal status at all", got.Status, statusCancelled)
				}
				if got.PendingTerminalStatus != "" {
					t.Errorf("pending_terminal_status is still %q after finalize; a leftover "+
						"marker makes the run look mid-shutdown forever",
						got.PendingTerminalStatus)
				}
			})
		})
	}
}
