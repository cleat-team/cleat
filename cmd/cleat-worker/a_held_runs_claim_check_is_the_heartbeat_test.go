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

// cleat-review3 on #4276. The tick heartbeats and fences what w.inflight holds for a run ID, and a claim stores by ID alone, so a run this worker re-claims
// while it is still holding the previous execution REPLACES that execution there: the tick covers the successor, nothing ever fences the held one, and a check that
// read only the fence marker answered "still mine". The held execution's outbound call could then go out beside the successor's.
func TestAHeldExecutionThatASuccessorOnThisWorkerReplacedIsLostEvenThoughNothingFencedIt(t *testing.T) {
	var sent [][]engine.GenerationKey
	w, held, execCtx := claimCheckFixture(func(_ context.Context, _ string, runs []engine.GenerationKey) ([]string, error) {
		sent = append(sent, append([]engine.GenerationKey(nil), runs...))
		return nil, nil // the store reports nothing lost: it has the successor's lease
	})
	// The same worker claims the run again, as generation 4, while generation 3 is still held.
	succCtx := registerInflightExecution(w, "held-run", "test-def", 4)
	successor := &engine.WorkflowInstance{ID: "held-run", DefName: "test-def", Generation: 4}

	err := w.claimStillMine(context.Background(), held)

	if !errors.Is(err, errClaimLost) {
		t.Fatalf("the replaced generation-3 execution got %v, want errClaimLost", err)
	}
	// What makes this the hole and not an ordinary lost run: nothing fenced or cancelled generation 3 and the store never heard of it, so the old answer was nil.
	if w.runIsFenced("held-run", 3) || execCtx.Err() != nil {
		t.Errorf("PRECONDITION: generation 3 was fenced (%v) or cancelled (%v) by something else, so the case is not the one measured", w.runIsFenced("held-run", 3), execCtx.Err())
	}
	for _, batch := range sent {
		for _, k := range batch {
			if k.Generation == 3 {
				t.Errorf("a heartbeat was sent for generation 3 (%v): the superseded execution was still covered, so the case is not the one measured", batch)
			}
		}
	}

	// The control: the successor, which IS the registered execution, is still held, and the successor is not disturbed by the other's check.
	if err := w.claimStillMine(context.Background(), successor); err != nil {
		t.Errorf("the registered generation-4 execution got %v, want nil", err)
	}
	if succCtx.Err() != nil || w.runIsFenced("held-run", 4) {
		t.Errorf("the successor was cancelled (%v) or fenced (%v) by its predecessor's check", succCtx.Err(), w.runIsFenced("held-run", 4))
	}
}

func TestAnExecutionWithNoRegistrationAtAllIsLost(t *testing.T) {
	w, held, _ := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) { return nil, nil })
	w.inflight.Delete("held-run") // its own teardown ran, or it was never registered

	if err := w.claimStillMine(context.Background(), held); !errors.Is(err, errClaimLost) {
		t.Fatalf("an execution that is not registered got %v, want errClaimLost", err)
	}
}

// A replacement that lands WHILE the tick is in flight: the snapshot the tick took held generation 3 and the store answers "nothing lost", so only a second look
// after the tick can see that a successor registered in the meantime. Without it the answer is nil.
func TestAReplacementThatLandsWhileTheTickIsInFlightIsStillLost(t *testing.T) {
	var w *Worker
	w, held, _ := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) {
		registerInflightExecution(w, "held-run", "test-def", 4) // the claim lands mid-tick
		return nil, nil
	})

	if err := w.claimStillMine(context.Background(), held); !errors.Is(err, errClaimLost) {
		t.Fatalf("a successor registered during the tick left the held execution with %v, want errClaimLost", err)
	}
}

// An execution that is already known to be replaced is answered without a store round trip: the check costs a tick, and a lost answer needs none.
func TestAnExecutionAlreadyReplacedIsAnsweredWithoutAskingTheStore(t *testing.T) {
	calls := 0
	w, held, _ := claimCheckFixture(func(context.Context, string, []engine.GenerationKey) ([]string, error) {
		calls++
		return nil, nil
	})
	registerInflightExecution(w, "held-run", "test-def", 4)

	if err := w.claimStillMine(context.Background(), held); !errors.Is(err, errClaimLost) {
		t.Fatalf("got %v, want errClaimLost", err)
	}
	if calls != 0 {
		t.Errorf("the store was asked %d time(s) about an execution already known to be replaced, want 0", calls)
	}
}
