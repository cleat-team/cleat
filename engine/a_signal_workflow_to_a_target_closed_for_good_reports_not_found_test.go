package engine

import (
	"context"
	"testing"
)

// cleat#4340, the guest's side. A guest that signals a run that can never be reopened (done, cancelled) used to be told it succeeded. It now gets the existing not-found outcome, and not a new class,
// because the outcome classes are part of the guest ABI every SDK decodes. A run that is failed or terminated CAN be reopened under the same id, so a signal to it is still accepted (cleat#4345 is the
// owner's question), and an open run is the control: the same call succeeds.
func TestASignalWorkflowCallToATargetClosedForGoodReportsNotFound(t *testing.T) {
	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()
			ctx := context.Background()
			sig, ok := store.(SignalStore)
			if !ok {
				t.Fatalf("%T is not a SignalStore", store)
			}
			parent := newIntentWorkflow(t, ctx, store, "guest-closed-parent")
			send := func(target string) int64 {
				s := newTestExecSession()
				s.engine.signalStore = sig
				return s.SignalWorkflow(ctx, nil, target, "approve", `{"n":1}`)
			}
			if code := send(settleRunOpen(t, ctx, store, parent)); code != 0 {
				t.Fatalf("CONTROL: a send to an OPEN run returned %d, want 0", code)
			}
			for _, how := range []string{"done", "cancelled"} {
				if code := send(settleRun(t, ctx, store, parent, how)); code != errSignalTargetNotFoundInt {
					t.Errorf("a send to a %s run returned %d, want errSignalTargetNotFoundInt (%d): the guest was told it succeeded", how, code, errSignalTargetNotFoundInt)
				}
			}
			for _, how := range []string{"failed", "terminated"} {
				if code := send(settleRun(t, ctx, store, parent, how)); code != 0 {
					t.Errorf("a send to a %s run returned %d, want 0: that run can be redriven under the same id, so the signal is still accepted", how, code)
				}
			}
		})
	}
}
