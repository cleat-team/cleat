package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4311. A worker at its concurrency limit slept a whole --poll in the
// dispatch loop, which nothing could interrupt, so a slot freed (or a NOTIFY
// that would resume a run) a moment after the sleep began went unused until the
// tick: 340 to 403 ms at --poll 500ms against 69 to 99 ms with a free slot.
// These pin the wait that replaced the sleep. The end-to-end figure is a
// tests/chaos measurement; what a unit test can hold is that a freed slot ends
// the wait, that the timer is still the ceiling, and that cancellation ends it.

func newSlotWaitWorker(t *testing.T, poll time.Duration) (*Worker, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Worker{ctx: ctx, pollInterval: poll, slotFreedCh: newSlotFreedChannel()}, cancel
}

func TestAFreedSlotEndsTheAtCapacityWaitBeforeThePollTick(t *testing.T) {
	w, _ := newSlotWaitWorker(t, 30*time.Second)
	wf := &engine.WorkflowInstance{ID: "wf-slot", Generation: 1}
	reg := &execRegistration{generation: 1}
	w.inflight.Store(wf.ID, wf)

	go func() {
		time.Sleep(50 * time.Millisecond)
		w.deregisterExecution(wf, reg)
	}()
	start := time.Now()
	w.waitForSlot()
	// The ceiling is 30 s; a wait that only the timer ended would take all of it.
	if took := time.Since(start); took < 40*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the wait took %s: want it ended by the slot freed after ~50ms, not by the 30s tick", took)
	}
	if _, still := w.inflight.Load(wf.ID); still {
		t.Fatal("the execution is still in flight after deregisterExecution")
	}
}

func TestASlotFreedBeforeTheWaitBeginsIsNotLost(t *testing.T) {
	// The loop counts, then waits; a slot freed between the two must still end
	// the wait. One buffered token is what carries it.
	w, _ := newSlotWaitWorker(t, 30*time.Second)
	wf := &engine.WorkflowInstance{ID: "wf-early", Generation: 1}
	w.inflight.Store(wf.ID, wf)
	w.deregisterExecution(wf, &execRegistration{generation: 1})

	start := time.Now()
	w.waitForSlot()
	if time.Since(start) > 5*time.Second {
		t.Fatalf("a slot freed before the wait began was lost: waited %s", time.Since(start))
	}
}

func TestTheAtCapacityWaitStillEndsAtThePollTick(t *testing.T) {
	// A missed signal must cost what the sleep always cost, not more.
	w, _ := newSlotWaitWorker(t, 60*time.Millisecond)
	start := time.Now()
	w.waitForSlot()
	if took := time.Since(start); took < 50*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the wait took %s, want about the 60ms poll", took)
	}
}

func TestTheAtCapacityWaitEndsWhenTheWorkerStops(t *testing.T) {
	w, cancel := newSlotWaitWorker(t, 30*time.Second)
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	w.waitForSlot()
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a stopped worker waited %s", took)
	}
}

func TestDeregisterExecutionDoesNotBlockOrPanicWithoutASlotChannel(t *testing.T) {
	// Worker values built without the channel (most tests) must keep working: a
	// send on a nil channel in a select with a default is a no-op.
	w := &Worker{}
	wf := &engine.WorkflowInstance{ID: "wf-nil", Generation: 1}
	w.inflight.Store(wf.ID, wf)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.deregisterExecution(wf, &execRegistration{generation: 1})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deregisterExecution blocked with a nil slotFreedCh")
	}
}

// cleat#4476. The tests above hold waitForSlot and the send that ends it; nothing held that the dispatch loop's at-capacity branch USES them, so putting a plain time.Sleep back there
// (the cleat#4311 defect) kept every one of them green, and was caught only by the timing row in the scheduled chaos sweep. This drives dispatchLoop itself, on the same mock store
// TestDispatchLoop_AtCapacity uses: concurrency 1, one in-flight entry, and a --poll of an HOUR. The slot is freed through deregisterExecution, and a claim must follow within 10 s.
// A loop that waits for the tick, by whatever means, claims nothing for an hour; one that wakes on the freed slot claims in milliseconds. The margin (360x) runs one way: a slow runner
// cannot make a working loop miss it.
//
// WHAT IT NEEDS BEFORE THE SLOT IS FREED: the loop must already have counted the in-flight entry and be at the wait. A slot freed before that is not lost (the token is kept) but the
// loop would simply claim on its first pass, and a loop that ignores the freed slot would pass. The first QueueDepth call is the evidence the loop has gone round (Tick makes it, once
// per bound interval, at the top of every pass); the settle after it covers the few instructions between that call and the wait. Reading the loop's own state would be better; none
// of it is observable from here.
func TestTheDispatchLoopClaimsAgainWhenASlotIsFreedWithAnHourlyPoll(t *testing.T) {
	ms := &mockStore{}
	var claims, depthReads atomic.Int32
	ms.claimStickyWorkflowsFn = func(context.Context, string, int) ([]*engine.WorkflowInstance, error) {
		claims.Add(1)
		return nil, nil
	}
	ms.claimWorkflowsFn = func(context.Context, string, int) ([]*engine.WorkflowInstance, error) { return nil, nil }
	ms.queueDepthFn = func(context.Context) (int64, error) {
		depthReads.Add(1)
		return 0, nil
	}
	w := newTestWorkerWithConcurrency(ms, 1)
	w.pollInterval = time.Hour
	w.slotFreedCh = newSlotFreedChannel()
	busy := &engine.WorkflowInstance{ID: "wf-busy-1", DefName: "test", DefVersion: 1}
	w.inflight.Store(busy.ID, busy)

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.dispatchLoop()
		close(done)
	}()
	t.Cleanup(func() {
		w.cancel()
		w.inflight.Delete(busy.ID) // the shutdown drain waits for in-flight to reach zero
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("dispatchLoop did not stop within 5s of cancellation")
		}
	})

	for deadline := time.Now().Add(10 * time.Second); depthReads.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("UNMEASURED: the dispatch loop never made its first pass (no QueueDepth read in 10s), so it cannot be said to have been at capacity")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := claims.Load(); n != 0 {
		t.Fatalf("the loop claimed %d times while at capacity", n)
	}

	w.deregisterExecution(busy, nil)
	deadline := time.Now().Add(10 * time.Second)
	for claims.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no claim 10s after the slot was freed, with a 1h --poll: the at-capacity branch is not woken by a freed slot, so it is waiting for the poll tick (cleat#4311)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
