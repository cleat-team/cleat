package engine

import (
	"context"
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 3.20's third body. AdminReReplay was a stub on all three
// dialects, answering 501, because resetting a workflow to 'ready' means it
// replays its recorded history -- which needed 1.4 phases D-F to have defined
// semantics. They exist now.
//
// The distinction these tests pin: re-replay resumes from recorded history,
// where the dead-letter reprocess path starts a new run from the definition and
// input. A workflow that failed on its ninth call must not re-issue the first
// eight.

func TestAdminReReplay_ResetsAStoppedWorkflowAndKeepsItsHistory(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "rereplay-ok")

		// Give it history, then stop it the way a real failure would.
		if err := store.AppendEventHistory(ctx, wfID, EventRecord{
			Step: 0, EventType: EventTypeCall, Service: "billing", Op: "charge",
			Request: `{}`, Response: `{"ok":true}`,
		}); err != nil {
			t.Fatalf("AppendEventHistory: %v", err)
		}
		wf := claimAndFail(t, ctx, store, wfID)

		if err := ReReplay(ctx, store, wfID, wf.Generation, "ops@example.com"); err != nil {
			t.Fatalf("ReReplay: %v", err)
		}

		back, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if back.Status != "ready" {
			t.Errorf("status = %q after re-replay, want \"ready\" so the dispatcher picks it up", back.Status)
		}
		if back.Error != "" {
			t.Errorf("ErrorMsg = %q, want it cleared -- the run that failed is being retried", back.Error)
		}

		// The point of re-replay rather than reprocess: the recorded step is
		// still there, so replay will not re-issue the call it records.
		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		var calls, audits int
		for _, r := range hist {
			switch r.EventType {
			case EventTypeCall:
				calls++
			case EventTypeAdminAction:
				audits++
			}
		}
		if calls != 1 {
			t.Errorf("history has %d call events, want the 1 already recorded -- re-replay must not "+
				"discard history, or it is the reprocess path under another name", calls)
		}
		if audits != 1 {
			t.Errorf("history has %d admin_action events, want 1: the re-replay must be auditable", audits)
		}
	})
}

// The three refusals, and they need different words because handleAdminOpError
// maps them to different statuses: 404, 409, and a plain 500-level message.
func TestAdminReReplay_RefusesWhatItCannotResume(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()

		t.Run("unknown workflow is not found", func(t *testing.T) {
			err := ReReplay(ctx, store, "no-such-workflow", 0, "ops")
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("err = %v, want one containing \"not found\" so the API answers 404", err)
			}
		})

		t.Run("a stale generation is a conflict", func(t *testing.T) {
			wfID := newIntentWorkflow(t, ctx, store, "rereplay-stale")
			wf := claimAndFail(t, ctx, store, wfID)
			err := ReReplay(ctx, store, wfID, wf.Generation+99, "ops")
			if err == nil || !strings.Contains(err.Error(), "generation mismatch") {
				t.Fatalf("err = %v, want one containing \"generation mismatch\" so the API answers 409", err)
			}
		})

		// The case adminResolveMiss could not express: the row is there and the
		// generation matches, and it still cannot be re-replayed. Reporting that
		// as a generation mismatch would send an operator hunting a concurrent
		// writer that does not exist.
		t.Run("a workflow that never stopped is refused by status, not by generation", func(t *testing.T) {
			wfID := newIntentWorkflow(t, ctx, store, "rereplay-ready")
			wf, err := store.GetWorkflowByID(ctx, wfID)
			if err != nil {
				t.Fatalf("GetWorkflowByID: %v", err)
			}
			err = ReReplay(ctx, store, wfID, wf.Generation, "ops")
			if err == nil {
				t.Fatal("re-replaying a 'ready' workflow succeeded; it would bump the generation " +
					"out from under whichever worker is about to claim it")
			}
			if strings.Contains(err.Error(), "generation mismatch") {
				t.Errorf("err = %v, want the status reason rather than a generation mismatch", err)
			}
			if !strings.Contains(err.Error(), "can be re-replayed") {
				t.Errorf("err = %v, want it to name the statuses that can be re-replayed", err)
			}
		})
	})
}

// Re-replaying into an unresolved ambiguity reproduces the same failure in the
// same place. Phase F gave the operator somewhere to put the answer, so this
// points at it rather than burning a generation to learn nothing.
func TestAdminReReplay_RefusesAnUnresolvedAmbiguity(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		wfID := newIntentWorkflow(t, ctx, store, "rereplay-ambiguous")
		writePendingCall(t, ctx, store, wfID)
		wf := claimAndFail(t, ctx, store, wfID)

		err := ReReplay(ctx, store, wfID, wf.Generation, "ops")
		if err == nil {
			t.Fatal("re-replayed a workflow with an unresolved ambiguous call; replay reports " +
				"[AMBIGUOUS] again and the operator is no further forward")
		}
		if !strings.Contains(err.Error(), "/resolve") {
			t.Errorf("err = %v, want it to name the resolve-step endpoint", err)
		}

		// And once resolved, it goes through -- otherwise the guard would be a
		// dead end rather than a redirect.
		if err := ResolveStep(ctx, store, wfID, 0, `{"charged":true}`, "ops"); err != nil {
			t.Fatalf("ResolveStep: %v", err)
		}
		after, err := store.GetWorkflowByID(ctx, wfID)
		if err != nil {
			t.Fatalf("GetWorkflowByID: %v", err)
		}
		if err := ReReplay(ctx, store, wfID, after.Generation, "ops"); err != nil {
			t.Fatalf("ReReplay after resolving the ambiguity: %v", err)
		}
	})
}

// claimAndFail puts a workflow into the state re-replay is for: stopped with
// work left. Fails it through the fenced path a real worker uses rather than an
// UPDATE, so the generation the test then passes is the one production produces.
func claimAndFail(t *testing.T, ctx context.Context, store WorkflowStore, wfID string) *WorkflowInstance {
	t.Helper()
	claimed, err := store.ClaimWorkflows(ctx, "worker-rereplay", 20)
	if err != nil {
		t.Fatalf("ClaimWorkflows: %v", err)
	}
	var wf *WorkflowInstance
	for _, c := range claimed {
		if c.ID == wfID {
			wf = c
		}
	}
	if wf == nil {
		t.Fatalf("workflow %s was not claimed; got %d", wfID, len(claimed))
	}
	if err := store.FailWorkflow(ctx, wfID, "worker-rereplay", wf.Generation,
		"boom", "transient", "call", nil); err != nil {
		t.Fatalf("FailWorkflow: %v", err)
	}
	// Re-read rather than assuming what FailWorkflow did to the generation. It
	// does not bump it, and a test that hardcoded +1 passed its "stale
	// generation" case for the wrong reason while failing the happy path.
	after, err := store.GetWorkflowByID(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflowByID after FailWorkflow: %v", err)
	}
	return after
}
