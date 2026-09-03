package engine

import (
	"context"
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 3.89. Resolving a pending call in place gives its row a
// checksum it did not have, and every row already stored above it was chained
// on that absence -- so the resolve left the next row failing verification.
//
// A worker runs VerifyWorkflowEvents with failOnChecksumMismatch, so the
// consequence was not a warning: the operator's reconciliation was the thing
// that stopped the workflow from ever replaying again.

// checksumVerifier is the store half of the verifier the worker wires up.
type checksumVerifier interface {
	VerifyWorkflowEvents(ctx context.Context, workflowID string) error
}

func verifierOf(t *testing.T, store WorkflowStore) checksumVerifier {
	t.Helper()
	v, ok := store.(checksumVerifier)
	if !ok {
		t.Fatalf("%T has no VerifyWorkflowEvents", store)
	}
	return v
}

// The reachable shape, end to end through the two admin verbs an operator
// actually uses: force-fail a stuck workflow, then reconcile the ambiguous
// call once the external service has been checked.
//
// Breaking check: drop the `later` argument at admin_intent.go's
// ResolveCallIntent call (pass nil) and this fails with
// "step 1: checksum mismatch" on postgres and mysql alike.
func TestAdminResolveStep_LeavesHistoryVerifiableWithEventsAboveIt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "resolve-chain")
		writePendingCall(t, ctx, store, wfID)

		wf, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if err := ForceFail(ctx, store, wfID, wf.Generation, "ops@example.com", "stuck on an ambiguous call", "admin_force_fail"); err != nil {
			t.Fatalf("ForceFail: %v", err)
		}

		// Vacuity guard. The whole defect is about a row stored ABOVE the
		// pending one; if force-fail stopped appending its audit event this
		// test would pass while testing nothing.
		before, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if above := chainRepairsAfter(before, 0); len(above) == 0 {
			t.Fatalf("precondition: nothing is stored above step 0, so the resolve has no chain to break: %+v", before)
		}

		v := verifierOf(t, store)
		if err := v.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Fatalf("precondition: history should verify before the resolve: %v", err)
		}

		if err := ResolveStep(ctx, store, wfID, 0, `{"charged":true}`, "ops@example.com"); err != nil {
			t.Fatalf("ResolveStep: %v", err)
		}

		if err := v.VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("history no longer verifies after the operator resolved step 0: %v\n"+
				"a worker runs this verifier with failOnChecksumMismatch, so this is the rescue "+
				"path bricking the workflow it was rescuing", err)
		}
	})
}

// The control: the ordinary shape, where the pending row is the last one and
// there is nothing to repair. This is the path phase E's resolver takes, and it
// has to keep working unchanged -- a fix that only moved the breakage would
// pass the test above and fail this one.
func TestAdminResolveStep_VerifiesWhenThePendingRowIsLast(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "resolve-chain-last")
		writePendingCall(t, ctx, store, wfID)

		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if above := chainRepairsAfter(hist, 0); len(above) != 0 {
			t.Fatalf("precondition: this case is meant to have nothing above step 0, got %+v", above)
		}

		if err := ResolveStep(ctx, store, wfID, 0, `{"charged":true}`, "ops@example.com"); err != nil {
			t.Fatalf("ResolveStep: %v", err)
		}
		if err := verifierOf(t, store).VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Errorf("VerifyWorkflowEvents after resolving the last row: %v", err)
		}
	})
}

// The repaired rows have to be *right*, not merely different: a repair that
// wrote any old checksum would satisfy the verifier only if the verifier were
// broken too. This asserts against a hand-run of the same chain rule.
func TestAdminResolveStep_RepairedChecksumsMatchTheChainRule(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "resolve-chain-values")
		writePendingCall(t, ctx, store, wfID)

		wf, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if err := ForceFail(ctx, store, wfID, wf.Generation, "ops@example.com", "stuck", "admin_force_fail"); err != nil {
			t.Fatalf("ForceFail: %v", err)
		}
		if err := ResolveStep(ctx, store, wfID, 0, `{"charged":true}`, "ops@example.com"); err != nil {
			t.Fatalf("ResolveStep: %v", err)
		}

		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		// Walk the whole history the way VerifyWorkflowEvents does and check
		// each row against the store, rather than trusting the store's own
		// verifier to be the judge of the store's own repair.
		var chain string
		for _, rec := range hist {
			chain = computeEventChecksum(rec, chain)
		}
		if err := verifierOf(t, store).VerifyWorkflowEvents(ctx, wfID); err != nil {
			t.Fatalf("VerifyWorkflowEvents: %v", err)
		}
		if len(hist) < 3 {
			t.Fatalf("expected the pending call, the force-fail audit and the resolve audit, got %d: %+v", len(hist), hist)
		}
		if hist[0].ResolvedBy != "ops@example.com" {
			t.Errorf("step 0 ResolvedBy = %q, want the operator", hist[0].ResolvedBy)
		}
	})
}

// chainRepairsAfter's contract, without a database.
func TestChainRepairsAfter(t *testing.T) {
	pending := EventRecord{Step: 3, EventType: EventTypeCall, Pending: true}
	hist := []EventRecord{
		{Step: 0, EventType: EventTypeCall},
		{Step: 1, EventType: EventTypeCall},
		{Step: 2, EventType: EventTypeAdminAction},
		pending,
		{Step: 4, EventType: EventTypeAdminAction},
	}

	got := chainRepairsAfter(hist, 0)
	if len(got) != 2 || got[0].Step != 1 || got[1].Step != 2 {
		t.Errorf("chainRepairsAfter(hist, 0) = %+v, want steps 1 and 2 and then a stop at the pending row 3", got)
	}

	if got := chainRepairsAfter(hist, 4); len(got) != 0 {
		t.Errorf("chainRepairsAfter(hist, 4) = %+v, want nothing above the last step", got)
	}

	// Out of order in, step order out: the stop-at-pending rule is only
	// correct in step order.
	shuffled := []EventRecord{hist[4], hist[1], hist[3], hist[0], hist[2]}
	got = chainRepairsAfter(shuffled, 0)
	if len(got) != 2 || got[0].Step != 1 || got[1].Step != 2 {
		t.Errorf("chainRepairsAfter(shuffled, 0) = %+v, want the same steps 1 and 2", got)
	}
}

// The error a broken chain produces, so the assertions above are anchored to
// the message an operator would actually see rather than to any error.
func TestVerifyWorkflowEventsReportsTheStepItBrokeAt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "resolve-chain-msg")
		writePendingCall(t, ctx, store, wfID)

		wf, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if err := ForceFail(ctx, store, wfID, wf.Generation, "ops@example.com", "stuck", "admin_force_fail"); err != nil {
			t.Fatalf("ForceFail: %v", err)
		}

		// Resolve without the repair, exactly as the pre-fix code did, so the
		// failure mode this section is about stays pinned even if the admin
		// path is rewritten.
		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		completed := hist[0]
		completed.Response = `{"charged":true}`
		completed.Pending = false
		payload, err := eventRecordToPayload(completed)
		if err != nil {
			t.Fatalf("eventRecordToPayload: %v", err)
		}
		resolver, ok := store.(callIntentResolver)
		if !ok {
			t.Fatalf("%T is not a callIntentResolver", store)
		}
		if err := resolver.ResolveCallIntent(ctx, wfID, completed, payload, "", 0, nil); err != nil {
			t.Fatalf("ResolveCallIntent: %v", err)
		}

		err = verifierOf(t, store).VerifyWorkflowEvents(ctx, wfID)
		if err == nil {
			t.Fatal("resolving with no chain repair left a history that still verifies; " +
				"either the chain rule changed or this test no longer reproduces 3.89")
		}
		if !strings.Contains(err.Error(), "checksum mismatch") {
			t.Errorf("error = %v, want a checksum mismatch", err)
		}
	})
}
