package engine

import (
	"context"
	"encoding/json"
	"testing"
)

// The whole transition, end to end, on a real guest: terminate a workflow that
// registered defers, claim what that produced, run it as a defer segment, and
// apply the recorded outcome. IMPROVEMENT-PLAN 3.75 step 2.
//
// The store tests next door prove each transition in isolation and
// TestADeferSegmentDrainsOnTheSuspension proves the segment mechanism in
// isolation. Neither can see the seam between them, which is where this
// mechanism spent a month: 3.81 built the segment, and until now
// `grep -rn WithDeferPhase --include=*.go` found only its own tests. What this
// asserts is that a TERMINATE is what starts one.
//
// It stops one layer short of cmd/cleat-worker: the ~30 lines of
// executeWorkflow that read PendingTerminalStatus, add the option and call
// finishDeferPhase are not exercised here. Everything they sit between is.
func TestTerminateRunsTheWorkflowsDefersBeforeItTerminates(t *testing.T) {
	ctx := context.Background()

	// Segment 1, with no store: the workflow registers its defers and sleeps.
	// This is the history a terminate arrives at.
	wasmBytes, eng1, caller1, _ := deferPhaseProbeEngine(t, "wf-terminate-vertical", false)
	_, history, susp, _, _, err := eng1.Execute(ctx, wasmBytes,
		"defer_survives_suspension", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if susp == nil {
		t.Fatal("segment 1 did not suspend; the fixture no longer produces the " +
			"state this test terminates from")
	}
	if got := operationsCalled(caller1); len(got) != 0 {
		t.Fatalf("segment 1 ran cleanup %v; a suspended workflow's defers are still pending", got)
	}
	if len(DeferralsFromHistory(history)) == 0 {
		t.Fatal("segment 1 registered no defers, so terminate would take the one-phase " +
			"path and this test would measure nothing")
	}

	for _, backend := range registeredBackends {
		backend := backend
		t.Run(backend.Name(), func(t *testing.T) {
			store, teardown := backend.Setup(t)
			defer teardown()

			// The workflow, with segment 1's history behind it -- including
			// the defer registrations, which is what makes terminate choose
			// the two-phase path.
			wfID := startTerminableWorkflow(t, ctx, store, "terminate-vertical", false)
			if err := store.AppendEventHistoryBatch(ctx, wfID, history); err != nil {
				t.Fatalf("AppendEventHistoryBatch: %v", err)
			}

			// Phase 1.
			if err := store.TerminateWorkflow(ctx, wfID, "terminated by test"); err != nil {
				t.Fatalf("TerminateWorkflow: %v", err)
			}
			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != statusTerminating {
				t.Fatalf("status = %q, want %q", wf.Status, statusTerminating)
			}

			// The dispatch loop's part: an ordinary claim, which is the whole
			// point of leaving the workflow schedulable.
			claimed := claimByID(t, ctx, store, "worker-vertical", wfID)
			if claimed.PendingTerminalStatus != "terminated" {
				t.Fatalf("claim carried PendingTerminalStatus %q, want %q",
					claimed.PendingTerminalStatus, "terminated")
			}

			// Phase 2: the defer segment. Everything below this line is what
			// cmd/cleat-worker does with a claim that carries a marker.
			loaded, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory: %v", err)
			}
			caller2 := &mockCaller{}
			eng2 := newDeferSegmentEngine(t, claimed.ID, caller2)
			_, resultHistory, susp2, _, _, err := eng2.Replay(ctx, wasmBytes,
				"defer_survives_suspension", json.RawMessage(`{}`), loaded)
			if err != nil {
				t.Fatalf("defer segment: %v", err)
			}
			if susp2 == nil {
				t.Fatal("the defer segment did not suspend; it no longer takes the path " +
					"the drain happens on")
			}

			// The cleanup ran, and it ran through the host rather than being
			// refused. This is the assertion 3.81 exists for: a drain whose
			// calls are all refused consumes the defer table without
			// performing anything, and looks identical from the outside.
			ops := operationsCalled(caller2)
			if !contains(ops, "after_sleep") {
				t.Fatalf("the defer segment recorded %v, want \"after_sleep\" among them: "+
					"the registered defer never reached the ServiceCaller, so terminate "+
					"destroyed this workflow's cleanup instead of running it", ops)
			}

			// Phase 2's write.
			dps, ok := store.(DeferPhaseStore)
			if !ok {
				t.Fatalf("%T cannot finalize a defer phase, but it marked one", store)
			}
			var newEvents []EventRecord
			if len(resultHistory) > len(loaded) {
				newEvents = resultHistory[len(loaded):]
			}
			if len(newEvents) == 0 {
				t.Fatal("the defer segment produced no new events, so the finalize below " +
					"would prove nothing about them being durable")
			}
			if err := dps.FinalizeDeferPhase(ctx, wfID, "worker-vertical", claimed.Generation, newEvents); err != nil {
				t.Fatalf("FinalizeDeferPhase: %v", err)
			}

			if wf := mustGetWorkflow(t, ctx, store, wfID); wf.Status != "terminated" {
				t.Fatalf("status = %q after the defer phase, want \"terminated\"", wf.Status)
			}
			// The defer bodies' calls are durable calls, and a segment that
			// ran them without recording them would replay differently.
			after, err := store.LoadEventHistory(ctx, wfID)
			if err != nil {
				t.Fatalf("LoadEventHistory (after): %v", err)
			}
			if len(after) <= len(loaded) {
				t.Fatalf("history is %d events after the defer phase and was %d before: "+
					"the segment's own events were not appended with the terminal write",
					len(after), len(loaded))
			}
		})
	}
}

// newDeferSegmentEngine builds the engine cmd/cleat-worker builds for a claim
// that carries a pending terminal outcome.
func newDeferSegmentEngine(t *testing.T, wfID string, caller ServiceCaller) *Engine {
	t.Helper()
	ctx := context.Background()
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	return NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID(wfID),
		WithDeferPhase())
}

func contains(ops []string, want string) bool {
	for _, op := range ops {
		if op == want {
			return true
		}
	}
	return false
}
