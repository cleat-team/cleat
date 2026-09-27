package engine

import (
	"context"
	"strings"
	"testing"
)

// cleat#2039: RetryWorkflow (dead_lettered -> ready) had no guard against an
// unresolved pending call intent, unlike ReReplay's equivalent transition for
// 'failed' and 'terminated'. A dead-lettered workflow whose last call was left
// mid-flight by a crash retried straight back into [AMBIGUOUS], redispatching
// a call whose outcome was never recorded -- the same shape cleat#1999's TLA+
// model traced for re-replay, reached through the sibling path it did not
// cover.
//
// Falsified: removing the isPendingIntent loop in engine.RetryWorkflow
// (engine/admin_ops.go) turns TestRetryWorkflow_RefusesAnUnresolvedAmbiguity
// red -- the retry proceeds instead of refusing.

func TestRetryWorkflow_RetriesADeadLetteredWorkflow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "retry-ok")
		deadLetter(t, ctx, store, wfID)

		if err := RetryWorkflow(ctx, store, wfID); err != nil {
			t.Fatalf("RetryWorkflow: %v", err)
		}

		back, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if back.Status != "ready" {
			t.Errorf("status = %q after retry, want \"ready\" so the dispatcher picks it up", back.Status)
		}
		if back.Error != "" {
			t.Errorf("ErrorMsg = %q, want it cleared -- the run that dead-lettered is being retried", back.Error)
		}
	})
}

func TestRetryWorkflow_RefusesAnUnresolvedAmbiguity(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "retry-ambiguous")
		writePendingCall(t, ctx, store, wfID)
		deadLetter(t, ctx, store, wfID)

		err := RetryWorkflow(ctx, store, wfID)
		if err == nil {
			t.Fatal("retried a dead-lettered workflow with an unresolved ambiguous call; the " +
				"retry redispatches it and the operator has no way to tell the call was ever made")
		}
		if !strings.Contains(err.Error(), "/resolve") {
			t.Errorf("err = %v, want it to name the resolve-step endpoint", err)
		}

		// And once resolved, it goes through -- otherwise the guard would be a
		// dead end rather than a redirect, same as ReReplay's equivalent case.
		if err := ResolveStep(ctx, store, wfID, 0, `{"charged":true}`, "ops"); err != nil {
			t.Fatalf("ResolveStep: %v", err)
		}
		if err := RetryWorkflow(ctx, store, wfID); err != nil {
			t.Fatalf("RetryWorkflow after resolving the ambiguity: %v", err)
		}
	})
}

// deadLetter claims wfID and moves it to dead_lettered through the fenced
// path a real worker uses (MoveToDeadLetterQueue requires the caller to hold
// the claim), rather than an UPDATE that could put the row in a state
// production never produces.
func deadLetter(t *testing.T, ctx context.Context, store WorkflowStore, wfID string) {
	t.Helper()
	claimed, err := store.ClaimWorkflows(ctx, "worker-retry", 20)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	var gen int64
	var found bool
	for _, c := range claimed {
		if c.ID == wfID {
			gen, found = c.Generation, true
		}
	}
	if !found {
		t.Fatalf("workflow %s was not claimed; got %d", wfID, len(claimed))
	}
	if err := store.MoveToDeadLetterQueue(ctx, wfID, "worker-retry", gen,
		"retries exhausted", "E_RETRY", "call"); err != nil {
		t.Fatalf("MoveToDeadLetterQueue: %v", err)
	}
}
