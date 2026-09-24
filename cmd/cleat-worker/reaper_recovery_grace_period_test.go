package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2005: after a database stall longer than the reclaim window, the
// reaper used to reclaim a LIVE worker's own run within milliseconds of the
// database coming back, because ReapStaleInstances' staleness predicate is
// satisfied by the stall's own duration and nothing asked whether THIS
// worker's own observation of that staleness could be trusted. These cover
// reapOnce's grace-period gate directly (deterministic, no sleeps) and one
// end-to-end reproduction of the reported timeline (real short sleeps).

func TestReapOnceSkipsWhenRecentDBTroubleHasNotClearedTheGracePeriod(t *testing.T) {
	var reapCalled atomic.Bool
	ms := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			reapCalled.Store(true)
			return 1, nil
		},
	}
	w := newTestWorker(ms)
	w.reclaimTimeout = 200 * time.Millisecond
	// Trouble recorded a moment ago -- well inside the grace period.
	w.lastDBTrouble.Store(time.Now().UnixNano())

	w.reapOnce()

	if reapCalled.Load() {
		t.Fatal("reapOnce called ReapStaleInstances while inside the recovery grace period -- it must skip until reapingIsSafe()")
	}
}

func TestReapOnceReclaimsOnceTheGracePeriodHasCleared(t *testing.T) {
	var reapCalled atomic.Bool
	ms := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			reapCalled.Store(true)
			return 1, nil
		},
	}
	w := newTestWorker(ms)
	w.reclaimTimeout = 50 * time.Millisecond
	// Trouble recorded well outside the grace period.
	w.lastDBTrouble.Store(time.Now().Add(-time.Second).UnixNano())

	w.reapOnce()

	if !reapCalled.Load() {
		t.Fatal("reapOnce did not call ReapStaleInstances once the grace period had cleared")
	}
}

func TestReapOnceRecordsDBTroubleOnAFailedCall(t *testing.T) {
	ms := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			return 0, errors.New("connection refused")
		},
	}
	w := newTestWorker(ms)
	w.reclaimTimeout = 50 * time.Millisecond
	// Healthy going in, so the call is actually attempted.
	w.lastDBTrouble.Store(time.Now().Add(-time.Second).UnixNano())

	before := time.Now()
	w.reapOnce()

	if w.reapingIsSafe() {
		t.Fatal("reapingIsSafe() = true right after a failed ReapStaleInstances call -- recordDBTrouble should have fired")
	}
	last := time.Unix(0, w.lastDBTrouble.Load())
	if last.Before(before) {
		t.Fatalf("lastDBTrouble = %v, which is before the failed call at %v -- it was not advanced", last, before)
	}
}

func TestReapOnceDoesNotRecordTroubleOnASuccessfulCall(t *testing.T) {
	ms := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			return 0, nil
		},
	}
	w := newTestWorker(ms)
	w.reclaimTimeout = 50 * time.Millisecond
	staleTrouble := time.Now().Add(-time.Hour)
	w.lastDBTrouble.Store(staleTrouble.UnixNano())

	w.reapOnce()

	if got := time.Unix(0, w.lastDBTrouble.Load()); !got.Equal(staleTrouble) {
		t.Fatalf("lastDBTrouble moved from %v to %v after a SUCCESSFUL call -- only a failure should advance it", staleTrouble, got)
	}
}

func TestReapOnceCutsOffACallThatOutlivesItsDeadline(t *testing.T) {
	// A store that just hangs -- like the docker-paused database in
	// cleat#2005's own reproduction -- rather than erroring. Honors ctx
	// cancellation the way a real driver under a real (non-frozen-container)
	// stall does, which is the shape dbCallDeadline exists to bound.
	release := make(chan struct{})
	ms := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			select {
			case <-release:
				return 1, nil
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		},
	}
	defer close(release)

	w := newTestWorker(ms)
	w.heartbeatInterval = 20 * time.Millisecond // dbCallDeadline() = 10ms
	w.reclaimTimeout = time.Second
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())

	done := make(chan struct{})
	go func() {
		w.reapOnce()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reapOnce did not return within 2s -- the bound context did not cut off the hung call")
	}

	if w.reapingIsSafe() {
		t.Fatal("reapingIsSafe() = true right after a call that was cut off by its deadline -- recordDBTrouble should have fired")
	}
}

// TestReaperDoesNotReclaimALiveWorkersOwnRunAcrossADatabaseStallItObservedItself
// reproduces cleat#2005's timeline without docker: a store that stalls (blocks
// every call, honoring context cancellation) for longer than reclaimAfter(),
// then recovers. heartbeatAndFenceInFlight and reapOnce are driven directly,
// standing in for their loops' tickers, so the test needs no multi-second
// waits for the reaper's own 10s-floor interval (see TestReaperLoop_CallsReap
// for why the existing tests avoid that ticker too).
//
// Before the fix (reapOnce with no grace-period gate and an unbounded
// context) this fails: a reap issued while stalled returns SUCCESS the
// instant the store unblocks, with ReapStaleInstances' predicate satisfied by
// the stall's own duration -- reclaiming a run whose holder, this very
// worker, was alive throughout.
func TestReaperDoesNotReclaimALiveWorkersOwnRunAcrossADatabaseStallItObservedItself(t *testing.T) {
	var stalled atomic.Bool
	blockUntilUnstalledOrDone := func(ctx context.Context, unblock <-chan struct{}) error {
		if !stalled.Load() {
			return nil
		}
		select {
		case <-unblock:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	unblock := make(chan struct{})
	var reapSuccesses atomic.Int64
	ms := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			if err := blockUntilUnstalledOrDone(ctx, unblock); err != nil {
				return nil, err
			}
			return nil, nil
		},
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			if err := blockUntilUnstalledOrDone(ctx, unblock); err != nil {
				return 0, err
			}
			reapSuccesses.Add(1)
			// This worker's own run would look exactly this stale to
			// ReapStaleInstances' real SQL predicate once the stall's
			// duration exceeds staleTimeout -- that predicate is not what
			// is under test here; reapOnce's gate in front of it is.
			return 1, nil
		},
	}

	w := newTestWorker(ms)
	w.heartbeatInterval = 10 * time.Millisecond // dbCallDeadline() = 5ms
	w.reclaimTimeout = 150 * time.Millisecond   // reclaimAfter() = 150ms

	// Establish a healthy baseline: this worker has ALREADY gone a full
	// reclaimAfter() window with no recorded trouble, same as a worker that
	// has been running uneventfully for a while (newTestWorker itself seeds
	// lastDBTrouble pessimistically -- see lastDBTrouble's doc -- which a
	// fresh worker must earn, not something this test is exercising).
	w.lastDBTrouble.Store(time.Now().Add(-time.Second).UnixNano())
	if !w.reapingIsSafe() {
		t.Fatal("setup: worker is not healthy before the stall even begins")
	}

	stalled.Store(true)

	// Simulate both loops' real ticker cadence during the stall: repeated
	// attempts, each cut off by dbCallDeadline, each refreshing
	// lastDBTrouble -- the same reason heartbeatLoop retries fast rather
	// than waiting out a full interval after trouble. A single cut-off
	// attempt would let lastDBTrouble age past reclaimAfter() on its own,
	// which is not the shape a real stall produces.
	stopStallLoop := make(chan struct{})
	stallLoopDone := make(chan struct{})
	go func() {
		defer close(stallLoopDone)
		for {
			select {
			case <-stopStallLoop:
				return
			default:
			}
			w.heartbeatAndFenceInFlight()
			w.reapOnce()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// The stall continues past reclaimAfter() -- exactly the reported
	// shape: everything would look stale by now if anything asked.
	time.Sleep(w.reclaimTimeout + 30*time.Millisecond)
	if n := reapSuccesses.Load(); n != 0 {
		t.Fatalf("ReapStaleInstances succeeded %d time(s) while stalled -- every call issued during the stall must be cut off, not complete once the store later unblocks", n)
	}
	if w.reapingIsSafe() {
		t.Fatal("reapingIsSafe() = true while still stalled -- the repeated cut-off attempts above should keep recording trouble")
	}

	// Stop the background stall-loop deterministically before touching
	// unblock/stalled, so the recovery phase below is driven by single,
	// unambiguous calls on the main goroutine rather than racing a
	// concurrent one that might sneak in an extra attempt right at the
	// unblock boundary.
	close(stopStallLoop)
	<-stallLoopDone

	// A reap tick right as the stall ends must still be refused: the LAST
	// recorded trouble is recent (from the cut-off attempts above), so the
	// grace period has not cleared yet even though the store is about to
	// respond again.
	w.reapOnce()
	if n := reapSuccesses.Load(); n != 0 {
		t.Fatalf("ReapStaleInstances succeeded %d time(s) immediately after the stall -- the grace period must still be running", n)
	}

	close(unblock)
	stalled.Store(false)

	// One heartbeat lands immediately (this is the "heartbeat immediately
	// on reconnect" half of the fix), but a single success does not clear
	// the grace period by itself -- only the elapsed time since the LAST
	// trouble does.
	w.heartbeatAndFenceInFlight()
	if w.reapingIsSafe() {
		t.Fatal("reapingIsSafe() = true immediately after recovery -- one success must not itself clear the grace period")
	}

	// Once reclaimAfter() has genuinely elapsed since the last recorded
	// trouble, the reaper trusts itself again.
	time.Sleep(w.reclaimTimeout + 20*time.Millisecond)
	if !w.reapingIsSafe() {
		t.Fatal("reapingIsSafe() = false well after recovery and a full reclaimAfter() window -- the grace period should have cleared")
	}
	w.reapOnce()
	if n := reapSuccesses.Load(); n == 0 {
		t.Fatal("reaper never reclaimed anything once the grace period genuinely cleared")
	}
}
