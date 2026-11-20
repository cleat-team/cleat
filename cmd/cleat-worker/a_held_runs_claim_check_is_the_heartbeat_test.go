package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4162, PR 2c. The claim check of a held run IS the heartbeat (claimStillMine runs heartbeatAndFenceInFlight and reads what it did), so these use the same fake store
// the heartbeat's own tests use and need no database. The Postgres scenarios in a_terminated_held_run_does_not_act_test.go are what show it end to end.

func claimCheckFixture(fn func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error)) (*Worker, *engine.WorkflowInstance, context.Context) {
	w := newTestWorker(&mockStore{heartbeatBatchFencedFn: fn})
	execCtx := registerInflightExecution(w, "held-run", "test-def", 3)
	return w, &engine.WorkflowInstance{ID: "held-run", DefName: "test-def", Generation: 3}, execCtx
}

func TestAClaimTheStoreStillHasIsHeld(t *testing.T) {
	calls := 0
	w, wf, execCtx := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) {
		calls++
		return nil, nil
	})

	if err := w.claimStillMine(context.Background(), wf); err != nil {
		t.Fatalf("claimStillMine = %v, want nil for a run the store did not report lost", err)
	}
	if calls != 1 {
		t.Errorf("the store was asked %d times, want once: the check is one heartbeat", calls)
	}
	if w.runIsFenced("held-run", 3) || execCtx.Err() != nil {
		t.Errorf("a held run was fenced (%v) or cancelled (%v)", w.runIsFenced("held-run", 3), execCtx.Err())
	}
}

func TestAClaimTheStoreHasTakenBackIsLostAndTheRunIsFencedAndCancelled(t *testing.T) {
	w, wf, execCtx := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) {
		return []string{"held-run"}, nil
	})

	err := w.claimStillMine(context.Background(), wf)

	if !errors.Is(err, errClaimLost) {
		t.Fatalf("claimStillMine = %v, want errClaimLost", err)
	}
	if !w.runIsFenced("held-run", 3) {
		t.Errorf("the run was not marked fenced, so its next durable call would not be refused")
	}
	if execCtx.Err() == nil {
		t.Errorf("the run's execution was not cancelled")
	}
}

// Unknown is neither: the heartbeat does not cancel on an error (one blip would stop every execution), so a held run whose check could not be answered is suspended but NOT
// fenced and NOT cancelled. It is replayed if the suspension stands.
func TestAClaimThatCannotBeConfirmedIsUnknownNotLostAndTheRunIsLeftAlone(t *testing.T) {
	w, wf, execCtx := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) {
		return nil, errors.New("connection refused")
	})

	err := w.claimStillMine(context.Background(), wf)

	if !errors.Is(err, errClaimUnknown) || errors.Is(err, errClaimLost) {
		t.Fatalf("claimStillMine = %v, want errClaimUnknown and not errClaimLost", err)
	}
	if w.runIsFenced("held-run", 3) {
		t.Errorf("an unanswerable check marked the run fenced")
	}
	if execCtx.Err() != nil {
		t.Errorf("an unanswerable check cancelled the execution: %v", execCtx.Err())
	}
}

// A run the heartbeat's own ticker has already fenced is lost without asking again: the answer is known, and the store round trip would only repeat it.
func TestARunTheHeartbeatAlreadyFencedIsLostWithoutAskingTheStore(t *testing.T) {
	calls := 0
	w, wf, _ := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) {
		calls++
		return nil, nil
	})
	w.fencedRuns.Store("held-run", int64(3))

	if err := w.claimStillMine(context.Background(), wf); !errors.Is(err, errClaimLost) {
		t.Fatalf("claimStillMine = %v, want errClaimLost", err)
	}
	if calls != 0 {
		t.Errorf("the store was asked %d times for a run already known to be fenced", calls)
	}
}

// A fence marker for ANOTHER generation of the same run is a stale one and says nothing about this execution (cleat#2942).
func TestAFenceMarkerForAnotherGenerationDoesNotMakeThisExecutionLost(t *testing.T) {
	w, wf, _ := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) { return nil, nil })
	w.fencedRuns.Store("held-run", int64(2)) // the execution is generation 3

	if err := w.claimStillMine(context.Background(), wf); err != nil {
		t.Fatalf("claimStillMine = %v, want nil: the marker is for generation 2", err)
	}
}

// Two ticks must never overlap. A held run's check is a second caller of heartbeatAndFenceInFlight, which used to have exactly one (the loop) and so never ran against itself;
// run together, two ticks could store lastHeartbeatOK out of order and move the worker's own reading of its health backwards. The store is made slow so that an overlap, if there
// is one, is certain and not a race that usually does not happen.
func TestTwoHeartbeatTicksNeverRunAtTheSameTime(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	w, _, _ := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		inFlight.Add(-1)
		return nil, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.heartbeatAndFenceInFlight() }()
	}
	wg.Wait()

	if got := maxInFlight.Load(); got != 1 {
		t.Errorf("up to %d HeartbeatBatchFenced calls ran at once, want 1: the ticks overlapped", got)
	}
}
