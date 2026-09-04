package main

// The worker's half of the two-phase terminal transition.
// IMPROVEMENT-PLAN 3.75 step 2.
//
// engine/defer_phase_vertical_test.go covers everything from the terminate to
// the finalize with a real guest and a real database. What it cannot see is the
// handful of lines in executeWorkflow that sit between them, and those lines
// are where the mechanism can quietly stop being used: a defer segment normally
// comes back SUSPENDED, and the ordinary reading of a suspension is "reschedule
// and run it again later", which would put the workflow back in the queue
// forever instead of terminating it.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// deferPhaseStore is a mockStore that can also finish a defer phase, which is
// the pairing every real store has.
type deferPhaseStore struct {
	*mockStore
	finalizeCalls  int
	finalizeEvents []engine.EventRecord
	finalizeGen    int64
	finalizeErr    error
	expireCalls    int
	expireN        int
	expireErr      error
}

func (m *deferPhaseStore) FinalizeDeferPhase(_ context.Context, _, _ string, generation int64, newEvents []engine.EventRecord) error {
	m.finalizeCalls++
	m.finalizeGen = generation
	m.finalizeEvents = newEvents
	return m.finalizeErr
}

func (m *deferPhaseStore) ExpireDeferPhases(context.Context) (int, error) {
	m.expireCalls++
	return m.expireN, m.expireErr
}

// newDeferPhaseWorker wires a worker whose store can finish a defer phase, and
// whose every OTHER terminal write fails the test. That second half is the
// point: the outcome was decided before this segment was claimed, so any write
// that chooses a status here is the defect.
func newDeferPhaseWorker(t *testing.T) (*Worker, *deferPhaseStore) {
	t.Helper()
	ms := &mockStore{}
	ms.finalizeWorkflowSegmentFn = func(_ context.Context, _, _ string, _ int64, _ []engine.EventRecord, finalStatus, _, _, _ string, _ map[string]string, _ time.Time) error {
		t.Errorf("a defer segment went through the ordinary finalize with status %q; "+
			"it must apply the outcome recorded at mark time instead", finalStatus)
		return nil
	}
	ms.failWorkflowFn = func(context.Context, string, string, int64, string, string, string, map[string]string) error {
		t.Error("a defer segment failed the workflow; the outcome was already decided")
		return nil
	}
	ms.completeWorkflowFn = func(context.Context, string, string, int64, string, map[string]string) error {
		t.Error("a defer segment completed the workflow; the outcome was already decided")
		return nil
	}
	w := newTestWorker(ms)
	return w, &deferPhaseStore{mockStore: ms}
}

// The events a defer segment produced go to the store with the terminal write.
// A defer body's host calls are durable calls; dropping them would terminate
// the workflow with a history that does not describe what actually ran.
func TestFinishDeferPhaseAppendsTheSegmentsOwnEvents(t *testing.T) {
	w, store := newDeferPhaseWorker(t)
	wf := &engine.WorkflowInstance{
		ID: "wf-1", DefName: "d", Generation: 7, PendingTerminalStatus: "terminated",
	}
	history := []engine.EventRecord{{Step: 0, EventType: engine.EventTypeDefer, DeferID: "defer-0"}}
	resultHistory := append(append([]engine.EventRecord{}, history...),
		engine.EventRecord{Step: 1, EventType: "call", Service: "lock", Op: "release"})

	w.finishDeferPhase(wf, store, history, resultHistory, time.Now())

	if store.finalizeCalls != 1 {
		t.Fatalf("FinalizeDeferPhase called %d times, want 1", store.finalizeCalls)
	}
	if store.finalizeGen != 7 {
		t.Fatalf("finalized at generation %d, want 7: the fence is the claim this "+
			"segment holds", store.finalizeGen)
	}
	if len(store.finalizeEvents) != 1 || store.finalizeEvents[0].Op != "release" {
		t.Fatalf("finalized with events %+v, want just the segment's own new event",
			store.finalizeEvents)
	}
}

// A lost fence is a normal outcome here rather than a failure, and it has one
// more cause than elsewhere: ExpireDeferPhases takes a phase away from a worker
// still grinding on it by bumping the generation. Either way the outcome has
// already been applied, so nothing further may be written.
func TestFinishDeferPhaseTreatsALostFenceAsSomebodyElsesSuccess(t *testing.T) {
	w, store := newDeferPhaseWorker(t)
	store.finalizeErr = engine.ErrFenceLost
	wf := &engine.WorkflowInstance{ID: "wf-2", DefName: "d", Generation: 3, PendingTerminalStatus: "terminated"}

	w.finishDeferPhase(wf, store, nil, nil, time.Now())

	if store.finalizeCalls != 1 {
		t.Fatalf("FinalizeDeferPhase called %d times, want 1", store.finalizeCalls)
	}
	// Nothing else was written -- the hooks installed by newDeferPhaseWorker
	// fail the test if it was. The dangerous one is a terminal failure: it
	// would replace an outcome the owner that won the fence has already
	// applied.
}

// A store that cannot finalize a defer phase must not be silently skipped: the
// workflow it marked would sit in 'terminating' with nothing able to move it
// until its deadline. Unreachable through any real store -- engine asserts the
// pairing at compile time -- and it must not panic.
func TestFinishDeferPhaseOnAStoreThatCannotFinalizeDoesNotPanic(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	if _, ok := any(ms).(engine.DeferPhaseStore); ok {
		t.Fatal("mockStore implements DeferPhaseStore, so this test measures nothing")
	}
	wf := &engine.WorkflowInstance{ID: "wf-3", DefName: "d", PendingTerminalStatus: "terminated"}
	w.finishDeferPhase(wf, ms, nil, nil, time.Now())
}

// The deadline sweep rides on the reaper's tick, and a store that cannot expire
// is silent rather than an error -- it is also a store that could not have
// marked a phase, so it has nothing to sweep.
func TestExpireDeferPhasesSweepsAndTolerates(t *testing.T) {
	w, store := newDeferPhaseWorker(t)
	w.store = store
	store.expireN = 2
	w.expireDeferPhases()
	if store.expireCalls != 1 {
		t.Fatalf("ExpireDeferPhases called %d times, want 1", store.expireCalls)
	}

	store.expireErr = errors.New("database is down")
	w.expireDeferPhases() // must not panic or propagate
	if store.expireCalls != 2 {
		t.Fatalf("ExpireDeferPhases called %d times, want 2", store.expireCalls)
	}

	ms := &mockStore{}
	w2 := newTestWorker(ms)
	w2.expireDeferPhases() // a store with no capability: a no-op, not a crash
}

// executeWorkflow's own handling, through the path a defer phase is most
// likely to take when something is wrong before the segment ever runs.
//
// The history load fails here, which is one of five pre-segment failures --
// tenant store, history, WASM, version check, panic recovery -- that all reach
// a terminal write through writeTerminalFailure. For an ordinary workflow that
// write is correct. For a defer phase it would turn a terminate into a failure
// because the CLEANUP could not start, replacing an outcome the database had
// already recorded.
func TestExecuteWorkflowOnADeferPhaseNeverWritesAFailure(t *testing.T) {
	w, store := newDeferPhaseWorker(t)
	defer w.cancel()
	w.store = store
	store.loadEventHistoryFn = func(context.Context, string) ([]engine.EventRecord, error) {
		return nil, errors.New("history is unreadable")
	}
	store.moveToDeadLetterQueueFn = func(context.Context, string, string, int64, string, string, string) error {
		t.Error("a defer phase was dead-lettered; its outcome was already decided")
		return nil
	}

	runExecuteWorkflow(w, &engine.WorkflowInstance{
		ID: "wf-early-failure", DefName: "d", DefVersion: 1, Generation: 4,
		PendingTerminalStatus: "terminated",
	})

	if store.finalizeCalls != 1 {
		t.Fatalf("FinalizeDeferPhase called %d times, want 1: a defer phase that could "+
			"not start still has to apply the outcome it was claimed to apply, or the "+
			"workflow sits in 'terminating' until its deadline", store.finalizeCalls)
	}
}
