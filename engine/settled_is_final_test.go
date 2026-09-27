package engine

// cleat#1975 (D3, "settled is final"). Terminate, pre-emptive cancel,
// force-complete and force-fail on a run that is already in one of the five
// settled statuses (done, failed, dead_lettered, terminated, cancelled) must
// refuse with ErrAdminStateConflict and leave the row untouched. The sole
// exception is TerminateWorkflow on a dead_lettered row -- the transition
// POST /api/dead-letters/:id/terminate exists to perform.
//
// These run against every configured backend for the same reason
// admin_force_resolve_test.go's do: the interesting content is the SQL, and
// the three dialects disagree about placeholders, the clock, and FOR
// UPDATE/UPDLOCK spelling. All three share the exact same Go-level
// isSettledStatus check, so a defect in one dialect's insertion point (wrong
// side of the FOR UPDATE read, wrong side of the deferPhaseOwed branch) would
// not show up in the other two -- which is why this is not "run it once on
// Postgres and trust the others".

import (
	"context"
	"errors"
	"testing"
)

func TestIsSettledStatus(t *testing.T) {
	settled := []string{statusDone, statusFailed, statusDeadLettered, statusTerminated, statusCancelled}
	for _, s := range settled {
		if !isSettledStatus(s) {
			t.Errorf("isSettledStatus(%q) = false, want true", s)
		}
	}
	notSettled := []string{"ready", "running", "suspended", statusTerminating, "", "bogus"}
	for _, s := range notSettled {
		if isSettledStatus(s) {
			t.Errorf("isSettledStatus(%q) = true, want false", s)
		}
	}
}

// settledFixture creates a workflow, claims it (so it has a real generation
// and an assigned worker, like every row an operator acts on), and forces it
// into the given status via raw SQL -- the same helper
// store_test_groups_6_10_test.go uses to reach a status no store method
// writes on its own.
func settledFixture(t *testing.T, ctx context.Context, store WorkflowStore, tag, status string) *WorkflowInstance {
	t.Helper()
	wfID, _ := startClaimedWorkflow(t, ctx, store, tag)
	setWorkflowStatus(t, store, wfID, status)
	before := mustGetWorkflow(t, ctx, store, wfID)
	if before.Status != status {
		t.Fatalf("fixture: setWorkflowStatus did not take: status = %q, want %q", before.Status, status)
	}
	return before
}

var allSettledStatuses = []string{statusDone, statusFailed, statusDeadLettered, statusTerminated, statusCancelled}

func TestSettledIsFinal_Terminate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		for _, status := range allSettledStatuses {
			status := status
			t.Run(status, func(t *testing.T) {
				ctx := context.Background()
				before := settledFixture(t, ctx, store, "term-settled-"+status, status)

				err := store.TerminateWorkflow(ctx, before.ID, "operator terminate")

				if status == statusDeadLettered {
					// The one documented exception: dead-lettered -> terminated
					// is what the DLQ terminate route exists to do.
					if err != nil {
						t.Fatalf("TerminateWorkflow(dead_lettered): %v, want nil", err)
					}
					after := mustGetWorkflow(t, ctx, store, before.ID)
					if after.Status != statusTerminated {
						t.Errorf("status = %q, want %q", after.Status, statusTerminated)
					}
					return
				}

				if !errors.Is(err, ErrAdminStateConflict) {
					t.Fatalf("TerminateWorkflow(%s): err = %v, want ErrAdminStateConflict", status, err)
				}
				after := mustGetWorkflow(t, ctx, store, before.ID)
				if after.Status != status {
					t.Errorf("a refused terminate changed status: %q -> %q", status, after.Status)
				}
			})
		}
	})
}

func TestSettledIsFinal_Cancel(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		for _, status := range allSettledStatuses {
			status := status
			t.Run(status, func(t *testing.T) {
				ctx := context.Background()
				// Cancel has no dead-letter exception: finalStatus is always
				// statusCancelled, so the terminate-only carve-out never
				// applies. Every settled status, dead_lettered included, is
				// refused.
				before := settledFixture(t, ctx, store, "cancel-settled-"+status, status)

				err := store.CancelWorkflow(ctx, before.ID, "operator cancel")
				if !errors.Is(err, ErrAdminStateConflict) {
					t.Fatalf("CancelWorkflow(%s): err = %v, want ErrAdminStateConflict", status, err)
				}
				after := mustGetWorkflow(t, ctx, store, before.ID)
				if after.Status != status {
					t.Errorf("a refused cancel changed status: %q -> %q", status, after.Status)
				}
			})
		}
	})
}

func TestSettledIsFinal_ForceFail(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		for _, status := range allSettledStatuses {
			status := status
			t.Run(status, func(t *testing.T) {
				ctx := context.Background()
				// force-resolve has no dead-letter exception at all: unlike
				// terminate, the settled check is unconditional.
				before := settledFixture(t, ctx, store, "ff-settled-"+status, status)

				err := store.AdminForceFail(ctx, before.ID, before.Generation,
					"operator says so", "OPERATOR", adminTestOperator)
				if !errors.Is(err, ErrAdminStateConflict) {
					t.Fatalf("AdminForceFail(%s): err = %v, want ErrAdminStateConflict", status, err)
				}
				after := mustGetWorkflow(t, ctx, store, before.ID)
				if after.Status != status {
					t.Errorf("a refused force-fail changed status: %q -> %q", status, after.Status)
				}
				if after.Generation != before.Generation {
					t.Errorf("a refused force-fail bumped generation: %d -> %d", before.Generation, after.Generation)
				}
				if n := countAdminEvents(t, ctx, store, before.ID); n != 0 {
					t.Errorf("a refused force-fail appended %d admin_action event(s), want 0", n)
				}
			})
		}
	})
}

func TestSettledIsFinal_ForceComplete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		for _, status := range allSettledStatuses {
			status := status
			t.Run(status, func(t *testing.T) {
				ctx := context.Background()
				before := settledFixture(t, ctx, store, "fc-settled-"+status, status)

				err := store.AdminForceComplete(ctx, before.ID, before.Generation,
					`{"ok":true}`, adminTestOperator)
				if !errors.Is(err, ErrAdminStateConflict) {
					t.Fatalf("AdminForceComplete(%s): err = %v, want ErrAdminStateConflict", status, err)
				}
				after := mustGetWorkflow(t, ctx, store, before.ID)
				if after.Status != status {
					t.Errorf("a refused force-complete changed status: %q -> %q", status, after.Status)
				}
				if n := countAdminEvents(t, ctx, store, before.ID); n != 0 {
					t.Errorf("a refused force-complete appended %d admin_action event(s), want 0", n)
				}
			})
		}
	})
}

// TestSettledIsFinal_TerminatingStaysReachable pins the acceptance criterion
// that 'terminating' -- the two-phase defer-phase window -- is NOT settled and
// keeps today's behaviour: a second terminate while a defer phase is still
// running cuts it short rather than being refused. isSettledStatus(status) is
// false for "terminating" (TestIsSettledStatus already covers that in
// isolation); this is the behavioural twin, through the real claim/terminate
// path rather than a raw status patch.
func TestSettledIsFinal_TerminatingStaysReachable(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := startTerminableWorkflow(t, ctx, store, "settled-final-terminating", true)

		if err := store.TerminateWorkflow(ctx, wfID, "first"); err != nil {
			t.Fatalf("TerminateWorkflow (first): %v", err)
		}
		if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != statusTerminating {
			t.Fatalf("status = %q after the first terminate, want %q", wf.Status, statusTerminating)
		}

		// Not settled, so this must still succeed -- refusing it would be a
		// regression in the OTHER direction: a genuinely in-flight cleanup
		// window becoming unreachable.
		if err := store.TerminateWorkflow(ctx, wfID, "second"); err != nil {
			t.Fatalf("TerminateWorkflow (second, while terminating): %v, want nil", err)
		}
		if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != statusTerminated {
			t.Fatalf("status = %q after the second terminate, want %q", wf.Status, statusTerminated)
		}
	})
}
