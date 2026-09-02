package engine

import (
	"context"
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 1.4 phase F. A call left pending by a crash could be
// resolved only by a configured AmbiguityResolver, and resolveAmbiguity returns
// immediately when there is none -- so in most deployments the workflow
// reported [AMBIGUOUS] on every replay forever, while telling the operator to
// go and check the external service and giving them nowhere to put the answer.
//
// These run against every registered dialect, because the resolution goes
// through ResolveCallIntent's per-dialect SQL even though ResolveStep itself
// is dialect-agnostic.

func TestAdminResolveStep_RecordsTheOutcomeAndWhoAssertedIt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()

		wfID := newIntentWorkflow(t, ctx, store, "resolve-step-ok")
		writePendingCall(t, ctx, store, wfID)

		if err := ResolveStep(ctx, store, wfID, 0, `{"charged":true}`, "ops@example.com"); err != nil {
			t.Fatalf("ResolveStep: %v", err)
		}

		hist, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if len(hist) != 2 {
			t.Fatalf("history has %d events, want 2 (the resolved call and the audit record): %+v", len(hist), hist)
		}

		resolved := hist[0]
		if resolved.Pending {
			t.Error("the step is still pending after being resolved")
		}
		if resolved.Response != `{"charged":true}` {
			t.Errorf("Response = %q, want the operator's outcome", resolved.Response)
		}
		if resolved.Err != "" {
			t.Errorf("Err = %q, want it cleared", resolved.Err)
		}
		// The load-bearing one: replay will treat this as the call's real
		// outcome forever, so the row has to say it was asserted rather
		// than observed.
		if resolved.ResolvedBy != "ops@example.com" {
			t.Errorf("ResolvedBy = %q, want the operator -- without it the history "+
				"claims the service returned this", resolved.ResolvedBy)
		}

		audit := hist[1]
		if audit.EventType != EventTypeAdminAction {
			t.Errorf("audit event type = %q, want %q", audit.EventType, EventTypeAdminAction)
		}
	})
}

// The point of the whole feature: after an operator resolves it, replay stops
// reporting [AMBIGUOUS] and hands the workflow the recorded outcome.
//
// Breaking check: skip the ResolveStep call and this fails with the ambiguous
// response, which is the state being fixed.
func TestAdminResolveStep_ReplayThenReadsTheResolvedOutcome(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()

		wfID := newIntentWorkflow(t, ctx, store, "resolve-step-replay")
		writePendingCall(t, ctx, store, wfID)

		before, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if !before[0].isPendingIntent() {
			t.Fatalf("precondition: step 0 should read as a pending intent, got %+v", before[0])
		}

		if err := ResolveStep(ctx, store, wfID, 0, `{"charged":true}`, "ops"); err != nil {
			t.Fatalf("ResolveStep: %v", err)
		}

		after, err := store.LoadEventHistory(ctx, wfID)
		if err != nil {
			t.Fatalf("LoadEventHistory: %v", err)
		}
		if after[0].isPendingIntent() {
			t.Fatal("step 0 still reads as a pending intent, so replay would report [AMBIGUOUS] again")
		}
		if after[0].Response != `{"charged":true}` {
			t.Errorf("replay would read Response %q, want the resolved outcome", after[0].Response)
		}
	})
}

func TestAdminResolveStep_RefusesWhatItCannotResolve(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()

		wfID := newIntentWorkflow(t, ctx, store, "resolve-step-refuse")
		writePendingCall(t, ctx, store, wfID)

		// The wording is load-bearing: handleAdminOpError maps these to
		// 404 and 409 by substring, so a reworded error silently changes
		// the HTTP contract.
		t.Run("unknown workflow is not found", func(t *testing.T) {
			err := ResolveStep(ctx, store, "no-such-workflow", 0, `{}`, "ops")
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("err = %v, want one containing \"not found\" so the API answers 404", err)
			}
		})

		t.Run("unknown step is not found", func(t *testing.T) {
			err := ResolveStep(ctx, store, wfID, 99, `{}`, "ops")
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("err = %v, want one containing \"not found\" so the API answers 404", err)
			}
		})

		t.Run("resolving twice is a conflict, not a silent overwrite", func(t *testing.T) {
			if err := ResolveStep(ctx, store, wfID, 0, `{"charged":true}`, "ops"); err != nil {
				t.Fatalf("first ResolveStep: %v", err)
			}
			err := ResolveStep(ctx, store, wfID, 0, `{"charged":false}`, "someone-else")
			if err == nil {
				t.Fatal("resolving an already-resolved step succeeded; a second operator " +
					"just overwrote a recorded outcome replay had already used")
			}
			if !strings.Contains(err.Error(), "generation mismatch") {
				t.Errorf("err = %v, want one containing \"generation mismatch\" so the API answers 409", err)
			}

			hist, herr := store.LoadEventHistory(ctx, wfID)
			if herr != nil {
				t.Fatalf("LoadEventHistory: %v", herr)
			}
			if hist[0].Response != `{"charged":true}` {
				t.Errorf("the first resolution was overwritten: Response = %q", hist[0].Response)
			}
		})
	})
}

// ResolvedBy has to survive the payload round trip and compaction for the same
// reason ErrCode does: it is the only record that a human asserted this
// outcome, and compaction rewrites exactly the oldest history.
func TestResolvedBySurvivesPayloadAndCompaction(t *testing.T) {
	rec := EventRecord{
		Step: 0, EventType: EventTypeCall, Service: "billing", Op: "charge",
		Response: `{"charged":true}`, ResolvedBy: "ops@example.com",
	}

	payload, err := eventRecordToPayload(rec)
	if err != nil {
		t.Fatalf("eventRecordToPayload: %v", err)
	}
	back := EventRecord{Step: rec.Step, EventType: rec.EventType}
	populateFromPayload(&back, payload)
	if back.ResolvedBy != "ops@example.com" {
		t.Errorf("ResolvedBy = %q after the payload round trip, want the operator", back.ResolvedBy)
	}

	restored := buildFullHistoryFromCompaction(nil, extractCompactionState([]EventRecord{rec}))
	if len(restored) != 1 {
		t.Fatalf("restored %d events, want 1", len(restored))
	}
	if restored[0].ResolvedBy != "ops@example.com" {
		t.Errorf("ResolvedBy = %q after compaction, want the operator", restored[0].ResolvedBy)
	}
}
