package engine

import (
	"context"
	"testing"
)

// cleat#1152: force-complete and force-fail applied a terminal status directly,
// so a workflow with outstanding defers was resolved without running them.
//
// The repository owner decided on 2026-09-13 that both must route through
// MARK/FINALIZE, the path TerminateWorkflow already takes. This asserts the
// MARK half at the store: the workflow lands in 'terminating' carrying the
// operator's intended outcome, rather than in that outcome.
//
// BOTH ARMS ARE ASSERTED, and the second is the control. A workflow that owes
// NO defer phase must still resolve in one step -- routing unconditionally
// would turn every operator force-complete into an asynchronous operation, and
// a test that only checked the defer case would not notice.
func TestForceResolveRunsTheDefersItOwes(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()

			// --- owes a defer: must MARK, not finalize ---
			for _, tc := range []struct {
				name       string
				force      func(id string, gen int64) error
				wantending string
			}{
				{"force-complete", func(id string, gen int64) error {
					return store.AdminForceComplete(ctx, id, gen, `{"by":"operator"}`, "op")
				}, "done"},
				{"force-fail", func(id string, gen int64) error {
					return store.AdminForceFail(ctx, id, gen, "operator said so", "E_OP", "op")
				}, "failed"},
			} {
				t.Run(tc.name+" with a defer owed", func(t *testing.T) {
					id := newIntentWorkflow(t, ctx, store, "force-defer")
					wf, err := claimSpecific(t, ctx, store, id, "w-force")
					if err != nil {
						t.Fatalf("claim: %v", err)
					}
					// A registered defer is a 'defer' event in history --
					// the same fact deferPhaseOwed reads.
					if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
						{Step: 0, EventType: EventTypeDefer, Service: "cleanup", Op: "release"},
					}); err != nil {
						t.Fatalf("register defer: %v", err)
					}

					if err := tc.force(id, wf.Generation); err != nil {
						t.Fatalf("%s: %v", tc.name, err)
					}

					got := mustGetWorkflow(t, ctx, store, id)
					if got.Status != statusTerminating {
						t.Errorf("status is %q, want %q -- the outcome was applied directly and "+
							"the defers it owes will never run (cleat#1152)",
							got.Status, statusTerminating)
					}
					if got.PendingTerminalStatus != tc.wantending {
						t.Errorf("pending_terminal_status is %q, want %q -- the operator's "+
							"intended outcome was not recorded for the finalize to apply",
							got.PendingTerminalStatus, tc.wantending)
					}
				})
			}

			// --- THE CONTROL: owes nothing, must resolve in one step ---
			t.Run("no defer owed still resolves directly", func(t *testing.T) {
				id := newIntentWorkflow(t, ctx, store, "force-nodefer")
				wf, err := claimSpecific(t, ctx, store, id, "w-force2")
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				if err := store.AdminForceComplete(ctx, id, wf.Generation, `{"by":"operator"}`, "op"); err != nil {
					t.Fatalf("force-complete: %v", err)
				}
				got := mustGetWorkflow(t, ctx, store, id)
				if got.Status != "done" {
					t.Errorf("status is %q, want \"done\" -- a workflow owing no defer phase "+
						"must not be routed through the two-phase path; that would make every "+
						"operator force-resolve asynchronous", got.Status)
				}
				if got.PendingTerminalStatus != "" {
					t.Errorf("pending_terminal_status is %q, want empty -- a marker left on a "+
						"terminal row is applied later by ExpireDeferPhases over the operator's "+
						"outcome", got.PendingTerminalStatus)
				}
			})
		})
	}
}
