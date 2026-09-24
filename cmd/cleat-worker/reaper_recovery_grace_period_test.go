package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
// reapOnce's grace-period gate directly (deterministic, no sleeps) and two
// end-to-end reproductions of the reported timeline (real short sleeps).
//
// cleat-review's follow-up review on the first version of this gate found a
// second hole: an IDLE worker made no database call at all, so its
// lastDBTrouble stayed clean through a stall it never observed, and its
// reaper reclaimed a run whose true holder was alive throughout. reapingIsSafe
// now requires BOTH a recent CONFIRMED contact (lastDBContactOK) and an
// old-enough trouble-free window (lastDBTrouble) -- see reapingIsSafe's doc.
// Every test below that wants the gate OPEN has to seed both; that is
// deliberate, not boilerplate -- a test that seeds only lastDBTrouble is
// exercising the OLD, insufficient rule, and several of these failed for
// exactly that reason when the gate was tightened (some outright, and two --
// TestReapOnceDoesNotRecordTroubleOnASuccessfulCall and
// TestReapOnceCutsOffACallThatOutlivesItsDeadline -- passed VACUOUSLY,
// because a worker that has never confirmed contact skips before ever
// reaching the call the test meant to exercise).

// seedRecentlyConfirmedHealthy seeds both atomics reapingIsSafe reads to
// "this worker just proved it can reach the database, cleanly": a recent
// lastDBContactOK and an old-enough (i.e. absent) lastDBTrouble. Tests that
// want the gate to start OPEN use this instead of seeding lastDBTrouble
// alone, which is no longer sufficient -- see the file doc above.
func seedRecentlyConfirmedHealthy(w *Worker) {
	w.lastDBContactOK.Store(time.Now().UnixNano())
	w.lastDBTrouble.Store(time.Now().Add(-time.Hour).UnixNano())
}

// withDBCallDeadlineFloor substitutes dbCallDeadlineFloor for the
// remainder of the calling test (restored via t.Cleanup), so a test at
// millisecond-scale heartbeat intervals gets the pre-GAP1 hb/2 arithmetic
// its timing was designed around instead of the real 2-second production
// floor. See dbCallDeadlineFloor's own doc for why this exists at all
// rather than every test simply using multi-second heartbeats -- these
// tests would otherwise take tens of seconds each. Never call this from a
// test that is actually about the floor itself.
func withDBCallDeadlineFloor(t *testing.T, floor time.Duration) {
	t.Helper()
	old := dbCallDeadlineFloor
	dbCallDeadlineFloor = floor
	t.Cleanup(func() { dbCallDeadlineFloor = old })
}

// pingingMockStore adds a controllable DBPinger to mockStore. mockStore
// itself deliberately does NOT implement DBPinger -- see DBPinger's doc for
// why that has to stay true for every other test in this package -- so this
// is its own type, used only by the tests in this file that need an idle
// worker's ping path to be real.
type pingingMockStore struct {
	*mockStore
	pingDBFn func(ctx context.Context) error
}

func (p *pingingMockStore) PingDB(ctx context.Context) error {
	if p.pingDBFn != nil {
		return p.pingDBFn(ctx)
	}
	return nil
}

// newTestWorkerFromStore is newTestWorker with the store typed as the
// engine.WorkflowStore interface rather than the concrete *mockStore, so a
// test can pass a *pingingMockStore (which mockStore itself deliberately
// cannot satisfy DBPinger's own type assertion for -- see that type's doc).
func newTestWorkerFromStore(store engine.WorkflowStore) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	monitor := NewMemoryMonitor(5 * time.Second)
	mc := NewMemoryController(monitor, store, "test-worker", 5, 1<<40, 1<<40)
	w := &Worker{
		Metrics:             newTestPrometheus(),
		id:                  "test-worker",
		store:               store,
		concurrency:         5,
		memoryController:    mc,
		heartbeatInterval:   10 * time.Millisecond,
		pollInterval:        1 * time.Millisecond,
		compactionThreshold: engine.DefaultCompactionThreshold,
		compactionInterval:  10 * time.Millisecond,
		ctx:                 ctx,
		cancel:              cancel,
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		wasmCache:           newWasmLRUCache(100, 500),
		healthTracker:       newHealthTracker(),
		loopCtxMap:          make(map[string]*loopContext),
	}
	w.lastHeartbeatOK.Store(time.Now().UnixNano())
	w.lastDBTrouble.Store(time.Now().UnixNano())
	return w
}

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
	// Trouble recorded a moment ago -- well inside the grace period. (A
	// recent lastDBContactOK would not help here: condition 2, the
	// trouble-free window, is what this test exercises, and it must gate
	// on its own.)
	seedRecentlyConfirmedHealthy(w)
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
	seedRecentlyConfirmedHealthy(w)

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
	seedRecentlyConfirmedHealthy(w)

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
	var reapCalled atomic.Bool
	ms := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			reapCalled.Store(true)
			return 0, nil
		},
	}
	w := newTestWorker(ms)
	w.reclaimTimeout = 50 * time.Millisecond
	seedRecentlyConfirmedHealthy(w)
	staleTrouble := time.Now().Add(-time.Hour)
	w.lastDBTrouble.Store(staleTrouble.UnixNano())

	w.reapOnce()

	if !reapCalled.Load() {
		t.Fatal("setup: ReapStaleInstances was never called -- this test proves nothing about a successful call if the gate skipped it")
	}
	if got := time.Unix(0, w.lastDBTrouble.Load()); !got.Equal(staleTrouble) {
		t.Fatalf("lastDBTrouble moved from %v to %v after a SUCCESSFUL call -- only a failure should advance it", staleTrouble, got)
	}
}

func TestReapOnceCutsOffACallThatOutlivesItsDeadline(t *testing.T) {
	withDBCallDeadlineFloor(t, time.Millisecond)
	// A store that just hangs -- like the docker-paused database in
	// cleat#2005's own reproduction -- rather than erroring. Honors ctx
	// cancellation the way a real driver under a real (non-frozen-container)
	// stall does, which is the shape dbCallDeadline exists to bound.
	release := make(chan struct{})
	var reapAttempted atomic.Bool
	ms := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			reapAttempted.Store(true)
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
	seedRecentlyConfirmedHealthy(w)

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

	if !reapAttempted.Load() {
		t.Fatal("setup: ReapStaleInstances was never attempted -- this test proves nothing about a hung call if the gate skipped it before ever calling the store")
	}
	if w.reapingIsSafe() {
		t.Fatal("reapingIsSafe() = true right after a call that was cut off by its deadline -- recordDBTrouble should have fired")
	}
}

// TestReapingIsSafeRequiresAConfirmedRecentContactNotJustAnAbsenceOfTrouble is
// the direct, deterministic test of cleat-review's finding: lastDBTrouble
// being old enough is NOT sufficient on its own. A worker that has never
// made a single successful round trip -- lastDBContactOK still at its
// atomic.Int64 zero value -- must not reap, no matter how long ago
// lastDBTrouble was (including "never", which reads as infinitely long ago).
func TestReapingIsSafeRequiresAConfirmedRecentContactNotJustAnAbsenceOfTrouble(t *testing.T) {
	ms := &mockStore{}
	w := newTestWorker(ms)
	w.reclaimTimeout = 50 * time.Millisecond
	// lastDBTrouble: never recorded -- the OLD rule's "clean" state.
	// lastDBContactOK: also never recorded -- deliberately left this way.
	if w.reapingIsSafe() {
		t.Fatal("reapingIsSafe() = true with no confirmed contact ever recorded -- an absence of trouble is not evidence of a checked, clean connection")
	}
}

// TestAnIdleWorkerDoesNotReclaimABusyWorkersLiveRunAcrossAStall is
// cleat-review's two-worker acceptance case: one worker (L) holds a live
// run and heartbeats it; a second worker (I) is idle -- nothing in its own
// inflight set -- and shares the same (fake) database. Before this fix, I's
// heartbeatAndFenceInFlight made no database call at all while idle, so I's
// lastDBTrouble stayed clean through the whole stall and I's reaper
// reclaimed L's live run the moment the stall cleared, because "L died" and
// "I never checked" were indistinguishable to I. Repeated at several stall
// lengths relative to the heartbeat interval, per cleat-review's ask.
//
// I's reaper is driven exactly ONCE, at the very end -- deliberately, not
// for test speed. In production reaperLoop's own ticker never fires more
// often than max(heartbeatInterval, 10s), so a stall of a few seconds can
// start and end entirely BETWEEN two real reaper ticks: cleat-review's own
// wording was "if no [reaper] tick lands in the stall." Driving I's reaper
// on the same fast cadence as its heartbeat -- an earlier version of this
// test did exactly that -- lets I's OWN reap attempts stumble into
// detecting the stall as a side effect, which is not the mechanism this
// test exists to check and made the result depend on which of two
// independently-scheduled goroutines happened to run first at the stall's
// boundary. Driving I's HEARTBEAT frequently but its REAPER only once,
// after recovery, is what actually isolates the claim: an idle worker's
// heartbeat tick alone -- via DBPinger -- has to be what keeps its gate
// closed, because its reaper never gets a chance to notice anything
// itself.
func TestAnIdleWorkerDoesNotReclaimABusyWorkersLiveRunAcrossAStall(t *testing.T) {
	for _, stall := range []time.Duration{
		150 * time.Millisecond, // under reclaimTimeout (200ms): the row never even looks stale
		250 * time.Millisecond, // past reclaimTimeout, but still under the OTHER new mechanism's
		// own recency bound (2*(heartbeat+deadline) = 300ms) -- this is the
		// narrow window where a seed-only "recently confirmed" reading
		// (from before the stall) would still look recent enough on its
		// own, so this specifically isolates whether the idle heartbeat's
		// OWN ping during the stall is doing the work, not just the fact
		// that an unrefreshed timestamp eventually ages out.
		600 * time.Millisecond, // well past both, sustained
	} {
		t.Run(stall.String(), func(t *testing.T) {
			testIdleWorkerDoesNotReclaimAcrossStall(t, stall)
		})
	}
}

func testIdleWorkerDoesNotReclaimAcrossStall(t *testing.T, stallDuration time.Duration) {
	withDBCallDeadlineFloor(t, time.Millisecond)
	const (
		heartbeatInterval = 100 * time.Millisecond // dbCallDeadline() = 50ms
		reclaimTimeout    = 200 * time.Millisecond // the invariant's exact floor: heartbeat + 2*deadline
	)

	// unblock, not a bare ctx timeout, is what actually gates a call during
	// the stall: a call issued while stalled=true cannot succeed until
	// unblock closes, no matter how many times its own bounded context
	// expires and it is retried. Without this, a call made just as
	// stalled flips back to false can race a DIFFERENT call (the holder's
	// next heartbeat, refreshing the row, vs. this worker's own recheck)
	// purely on goroutine scheduling, which would make this test's outcome
	// depend on timing luck rather than on the gate. Mirrors
	// TestReaperDoesNotReclaimALiveWorkersOwnRunAcrossADatabaseStallItObservedItself's
	// pattern, which already relies on the same determinism.
	var stalled atomic.Bool
	unblock := make(chan struct{})
	blockWhileStalled := func(ctx context.Context) error {
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

	// The shared fake row: assignedTo/heartbeatAt is what a real
	// workflow_instances row would carry. Guarded by the stall flag rather
	// than a mutex-protected clock, since both worker's calls are cut off by
	// their own bounded context the same way a real driver would be.
	var rowHeartbeatAt atomic.Int64
	var reclaimed atomic.Bool
	rowHeartbeatAt.Store(time.Now().UnixNano())

	// L: holds the run, heartbeats it.
	holderStore := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			if err := blockWhileStalled(ctx); err != nil {
				return nil, err
			}
			rowHeartbeatAt.Store(time.Now().UnixNano())
			return nil, nil
		},
	}
	holder := newTestWorker(holderStore)
	holder.id = "worker-L"
	holder.heartbeatInterval = heartbeatInterval
	holder.reclaimTimeout = reclaimTimeout
	seedRecentlyConfirmedHealthy(holder)
	holder.inflight.Store("live-run", &engine.WorkflowInstance{ID: "live-run", Generation: 1})

	// I: idle. Its store implements DBPinger, so its own heartbeat tick
	// still makes a real round trip with nothing in flight -- that is
	// exactly what this test is checking actually happens.
	idleBase := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			if err := blockWhileStalled(ctx); err != nil {
				return 0, err
			}
			hb := time.Unix(0, rowHeartbeatAt.Load())
			if time.Since(hb) >= timeout {
				reclaimed.Store(true)
				return 1, nil
			}
			return 0, nil
		},
	}
	idleStore := &pingingMockStore{mockStore: idleBase, pingDBFn: blockWhileStalled}
	idle := newTestWorkerFromStore(idleStore)
	idle.id = "worker-I"
	idle.heartbeatInterval = heartbeatInterval
	idle.reclaimTimeout = reclaimTimeout
	seedRecentlyConfirmedHealthy(idle)
	// idle.inflight is deliberately left empty.

	// L: heartbeat only, driven fast -- there is no L reaper in this test.
	stopHolder := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		for {
			select {
			case <-stopHolder:
				return
			default:
			}
			holder.heartbeatAndFenceInFlight()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	// I: heartbeat driven fast (this is the mechanism under test -- an idle
	// worker's own ping). Its reaper is NOT driven here at all; see below.
	stopIdleHeartbeat := make(chan struct{})
	idleHeartbeatDone := make(chan struct{})
	go func() {
		defer close(idleHeartbeatDone)
		for {
			select {
			case <-stopIdleHeartbeat:
				return
			default:
			}
			idle.heartbeatAndFenceInFlight()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	stalled.Store(true)
	time.Sleep(stallDuration)

	// Stop BOTH heartbeat loops BEFORE clearing the stall, and only then
	// unblock. Nothing is left running that could refresh the row or
	// re-touch I's gate between "the stall ends" and "I's one reap check,"
	// so the row's age at that check is exactly stallDuration and the
	// check is deterministic rather than a race between L's next heartbeat
	// (refreshing the row) and I's reap (reading it) -- both of which would
	// otherwise fire the instant unblock closes.
	close(stopHolder)
	close(stopIdleHeartbeat)
	<-holderDone
	<-idleHeartbeatDone
	close(unblock)
	stalled.Store(false)

	// NOW I's reaper gets its one and only tick -- the "no tick landed
	// during the stall" case cleat-review described. If I's heartbeat-only
	// probing during the stall did its job, I's gate is still closed here
	// (recent trouble recorded during the stall, grace period not yet
	// elapsed) and this call skips. If it did not, this is the FIRST
	// database contact I's reaper has ever had, the gate reads clean by
	// default, and it reclaims L's live, stale-looking row on the spot.
	idle.reapOnce()

	if reclaimed.Load() {
		t.Fatalf("idle worker I reclaimed L's live run after a %v stall -- I never observed the stall directly (nothing in its own inflight, and its reaper never ticked during it), so its own heartbeat-driven gate is what should have refused this, and it did not", stallDuration)
	}
}

// TestASlowHeartbeatCycleDoesNotOpenAGapWiderThanReclaimAfterAllows is
// cleat-review's "sliver": a busy worker's heartbeat_at is only refreshed
// once per successful call, and a call can legitimately take up to
// dbCallDeadline to return -- so the true gap between two successful writes
// can be heartbeatInterval + dbCallDeadline, not heartbeatInterval alone.
//
// THIS WAS VACUOUS (cleat-review, third round). The previous version drove
// heartbeatAndFenceInFlight back to back with no wait between calls, so the
// only gap it could ever observe was the call's own ~90ms duration -- it
// stayed green even at reclaimTimeout == heartbeatInterval, which is
// provably too tight for a call that can legitimately take up to
// dbCallDeadline. It never exercised heartbeatLoop's real cadence at all:
// a Timer re-armed for a fresh heartbeatInterval only AFTER each call
// returns (see heartbeatLoop), which is what actually produces the
// heartbeatInterval + dbCallDeadline gap this test is supposed to be
// about.
//
// This version runs the REAL heartbeatLoop (not a hand-rolled substitute)
// and includes a KNOWN-POSITIVE: at reclaimTimeout == heartbeatInterval
// alone (deliberately below the true worst case of
// heartbeatInterval + dbCallDeadline for a call that is merely slow, never
// failing), this must observe staleness -- proving the fixed harness can
// see the gap it exists to catch. At minimumReclaimAfter, the actual
// invariant, it must not.
func TestASlowHeartbeatCycleDoesNotOpenAGapWiderThanReclaimAfterAllows(t *testing.T) {
	withDBCallDeadlineFloor(t, time.Millisecond)
	const heartbeatInterval = 200 * time.Millisecond // dbCallDeadline() = 100ms

	run := func(reclaimTimeout time.Duration) (sawStale bool, maxGap time.Duration) {
		var rowHeartbeatAt atomic.Int64
		rowHeartbeatAt.Store(time.Now().UnixNano())

		holderStore := &mockStore{
			heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
				// Just under the deadline every time: the worst legitimate
				// case, not a failure.
				select {
				case <-time.After(90 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				rowHeartbeatAt.Store(time.Now().UnixNano())
				return nil, nil
			},
		}
		holder := newTestWorker(holderStore)
		holder.heartbeatInterval = heartbeatInterval
		holder.reclaimTimeout = reclaimTimeout
		seedRecentlyConfirmedHealthy(holder)
		holder.inflight.Store("live-run", &engine.WorkflowInstance{ID: "live-run", Generation: 1})

		var maxGapNs atomic.Int64
		lastSeen := rowHeartbeatAt.Load()
		holder.wg.Add(1)
		go holder.heartbeatLoop()

		deadline := time.Now().Add(2 * time.Second)
		var stale bool
		for time.Now().Before(deadline) {
			hb := rowHeartbeatAt.Load()
			if hb != lastSeen {
				if gap := hb - lastSeen; gap > maxGapNs.Load() {
					maxGapNs.Store(gap)
				}
				lastSeen = hb
			}
			if age := time.Since(time.Unix(0, hb)); age >= reclaimTimeout {
				stale = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		holder.cancel()
		holder.wg.Wait()
		return stale, time.Duration(maxGapNs.Load())
	}

	t.Run("known-positive: reclaimTimeout == heartbeatInterval alone is too tight", func(t *testing.T) {
		sawStale, maxGap := run(heartbeatInterval)
		if !sawStale {
			t.Fatalf("expected staleness at reclaimTimeout == heartbeatInterval (%v) for a call that can legitimately "+
				"take up to dbCallDeadline -- max observed gap between successful writes was %v; if this doesn't go "+
				"stale, the harness itself is not exercising the real heartbeat cadence and this whole test is vacuous again",
				heartbeatInterval, maxGap)
		}
	})

	t.Run("at the real invariant, it does not", func(t *testing.T) {
		reclaimTimeout := minimumReclaimAfter(heartbeatInterval)
		sawStale, maxGap := run(reclaimTimeout)
		if sawStale {
			t.Fatalf("the row looked stale (age >= reclaimTimeout %v) even though the holder's heartbeat calls never failed -- "+
				"max observed gap between successful writes was %v; minimumReclaimAfter should prevent this",
				reclaimTimeout, maxGap)
		}
	})
}

// TestASingleFailedHeartbeatRetryCanOutlastTheOldReclaimInvariantButNotTheNewOne
// is cleat-review's third-round finding on cleat#2005, reproduced with two
// real Workers running their REAL heartbeatLoop (not a hand-rolled
// substitute): a stall SHORTER than one heartbeat interval can still cost
// the holder a full failed-then-retried cycle, and the resulting gap
// between successful writes exceeds the old heartbeat + 2*deadline floor
// while a differently-phased idle worker's own gate -- which never
// observed the stall itself -- reads clean throughout.
//
// The mechanics, at heartbeatInterval=200ms (dbCallDeadline=100ms):
//   - The holder's heartbeat call ordinarily takes 95ms -- close to, but
//     under, its own 100ms deadline.
//   - A single 190ms stall (under one heartbeat interval) overlaps one
//     call. Since 95ms of the call's own latency is spent before it even
//     checks whether the database is reachable, the remaining budget under
//     its 100ms deadline is only 5ms -- nowhere near enough to outlast a
//     190ms stall, so this call fails.
//   - heartbeatRetryInterval is min(heartbeatInterval, 1s), which AT THIS
//     HEARTBEAT provides no speedup at all (200ms either way) -- so the
//     retry does not start until a full heartbeatInterval after the
//     failure, and itself can take up to another 95ms to succeed.
//   - Total: heartbeatInterval (to the failing call) + dbCallDeadline (to
//     fail) + heartbeatInterval (retry wait) + ~dbCallDeadline (retry's own
//     latency) -- measured at ~595ms here, comfortably past the OLD 400ms
//     floor and the corrected 500ms one alike, eventually. The WINDOW this
//     test targets is the part in between: at approximately 400-490ms
//     after the last real write, the row already looks stale under the old
//     floor and does not yet under the corrected one.
//   - The idle worker's own ping never overlaps this particular stall (a
//     different phase on the same cadence -- realistic for two
//     independently-started workers), so its own lastDBTrouble is never
//     touched and its gate reads safe the entire time, exactly as it
//     should once real time has passed since it last confirmed health.
//
// This is RED against the pre-round-3 formula (heartbeat + 2*deadline,
// 400ms here) and GREEN against minimumReclaimAfter (heartbeat +
// 3*deadline, 500ms here) -- see the falsification note at the end of this
// function.
func TestASingleFailedHeartbeatRetryCanOutlastTheOldReclaimInvariantButNotTheNewOne(t *testing.T) {
	withDBCallDeadlineFloor(t, time.Millisecond)
	const heartbeatInterval = 200 * time.Millisecond
	reclaimTimeout := minimumReclaimAfter(heartbeatInterval) // 500ms today

	var stalled atomic.Bool
	unblock := make(chan struct{})
	blockWhileStalled := func(ctx context.Context) error {
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

	var rowHeartbeatAt atomic.Int64
	t0 := time.Now()
	rowHeartbeatAt.Store(t0.UnixNano())

	holderStore := &mockStore{
		heartbeatBatchFencedFn: func(ctx context.Context, workerID string, runs []engine.GenerationKey) ([]string, error) {
			select {
			case <-time.After(95 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if err := blockWhileStalled(ctx); err != nil {
				return nil, err
			}
			rowHeartbeatAt.Store(time.Now().UnixNano())
			return nil, nil
		},
	}
	holder := newTestWorker(holderStore)
	holder.id = "holder"
	holder.heartbeatInterval = heartbeatInterval
	holder.reclaimTimeout = reclaimTimeout
	seedRecentlyConfirmedHealthy(holder)
	holder.inflight.Store("live-run", &engine.WorkflowInstance{ID: "live-run", Generation: 1})

	// idle's own ping is never subject to the stall: a differently-phased
	// worker's probe simply does not land during this particular 190ms
	// window, which is realistic (two independently-started workers are not
	// phase-locked) and is the specific case cleat-review described as "a
	// stall D < heartbeat can fall between probes."
	idleStore := &pingingMockStore{mockStore: &mockStore{}, pingDBFn: func(ctx context.Context) error { return nil }}
	idle := newTestWorkerFromStore(idleStore)
	idle.id = "idle"
	idle.heartbeatInterval = heartbeatInterval
	idle.reclaimTimeout = reclaimTimeout
	seedRecentlyConfirmedHealthy(idle)

	holder.wg.Add(1)
	go holder.heartbeatLoop()
	idle.wg.Add(1)
	go idle.heartbeatLoop()

	time.Sleep(150 * time.Millisecond)
	stalled.Store(true)
	time.Sleep(190 * time.Millisecond)
	close(unblock)
	stalled.Store(false)

	// The discriminating instant: past 340ms (150+190) but still short of
	// the holder's actual recovery (~595ms) and short of the corrected
	// 500ms invariant. 450ms was chosen empirically as comfortably inside
	// the window the old 400ms floor gets wrong and the new 500ms one gets
	// right (measured window: roughly 400-490ms).
	const checkAt = 450 * time.Millisecond
	if remaining := checkAt - 150*time.Millisecond - 190*time.Millisecond; remaining > 0 {
		time.Sleep(remaining)
	}

	safe := idle.reapingIsSafe()
	age := time.Since(time.Unix(0, rowHeartbeatAt.Load()))
	stale := age >= reclaimTimeout

	holder.cancel()
	idle.cancel()
	holder.wg.Wait()
	idle.wg.Wait()

	if safe && stale {
		t.Fatalf("idle worker reclaimed the holder's live run %v after its last write, "+
			"under reclaimTimeout=%v -- a single failed-then-retried heartbeat cycle across a %v stall "+
			"(shorter than the %v heartbeat interval) outlasted the invariant", age, reclaimTimeout, 190*time.Millisecond, heartbeatInterval)
	}
	if !safe {
		t.Fatalf("test setup: idle's own gate was unexpectedly unsafe (its ping never stalls in this test) -- age=%v", age)
	}
}

// Falsification (applied by hand, verified, and reverted -- never
// committed): replace minimumReclaimAfter's body with
// `return heartbeat + 2*dbCallDeadlineFor(heartbeat)`, the pre-round-3
// formula. TestASingleFailedHeartbeatRetryCanOutlastTheOldReclaimInvariantButNotTheNewOne
// must then fail, because reclaimTimeout becomes 400ms and the row is
// already stale (age >= 400ms) at the 450ms check while idle's gate still
// reads safe.

// TestReclaimWindowDefaultMatchesTheStatedInvariant is the deterministic
// counterpart to the two timing-based tests above: it asserts the actual
// arithmetic, with no sleeping, against minimumReclaimAfter -- THE stated
// invariant, in one place, per cleat-review's third-round ask. This fails
// if anyone reintroduces a formula computed separately here rather than
// calling that function, which is exactly how the very first version of
// this bound (a bare `2 * heartbeat`) drifted from what reclaimWindow
// actually needs without anyone deciding it should.
func TestReclaimWindowDefaultMatchesTheStatedInvariant(t *testing.T) {
	for _, hb := range []time.Duration{
		time.Second, // below dbCallDeadlineFor's 2s floor: hb/2 = 500ms
		4 * time.Second,
		5 * time.Second,
		200 * time.Millisecond,
		30 * time.Second,
	} {
		want := max(minimumReclaimAfter(hb), 10*time.Second)
		got := reclaimWindow(0, hb)
		if got != want {
			t.Errorf("reclaimWindow(0, %v) = %v, want %v (minimumReclaimAfter(heartbeat), floored at 10s)", hb, got, want)
		}
	}
}

// TestValidateReclaimTimeoutRefusesBelowTheStatedInvariant is
// TestReclaimWindowDefaultMatchesTheStatedInvariant's counterpart for the
// explicit --reclaim-timeout path: the refusal floor must track
// minimumReclaimAfter exactly, not a separately maintained formula or
// literal.
func TestValidateReclaimTimeoutRefusesBelowTheStatedInvariant(t *testing.T) {
	for _, hb := range []time.Duration{4 * time.Second, time.Second} {
		floor := minimumReclaimAfter(hb)

		if err := validateReclaimTimeout(floor-time.Millisecond, hb); err == nil {
			t.Fatalf("validateReclaimTimeout(%v, %v) = nil, want a refusal: %v is below the invariant floor %v", floor-time.Millisecond, hb, floor-time.Millisecond, floor)
		}
		if err := validateReclaimTimeout(floor, hb); err != nil {
			t.Fatalf("validateReclaimTimeout(%v, %v) = %v, want nil: %v meets the invariant floor exactly", floor, hb, err, floor)
		}
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
	withDBCallDeadlineFloor(t, time.Millisecond)
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
	// This worker's OWN live run, so heartbeatAndFenceInFlight takes the
	// busy path (through heartbeatBatchFencedFn, exercised below) rather
	// than the empty-inflight path -- which, on a plain mockStore with no
	// DBPinger, has no way to confirm contact at all. See DBPinger's doc:
	// a store that cannot prove liveness while idle must not let its
	// reaper trust staleness while idle either, which is deliberately NOT
	// the case this test is about.
	w.inflight.Store("live-run", &engine.WorkflowInstance{ID: "live-run", Generation: 1})

	// Establish a healthy baseline: this worker has ALREADY gone a full
	// reclaimAfter() window with no recorded trouble, AND has confirmed
	// contact recently -- same as a worker that has been running
	// uneventfully for a while (newTestWorker itself seeds lastDBTrouble
	// pessimistically -- see lastDBTrouble's doc -- which a fresh worker
	// must earn, not something this test is exercising; lastDBContactOK
	// needs the same earning, which is what seedRecentlyConfirmedHealthy
	// establishes here).
	seedRecentlyConfirmedHealthy(w)
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
	// trouble, the reaper trusts itself again -- but reapingIsSafe ALSO
	// requires a RECENTLY confirmed contact (cleat-review's follow-up
	// review), so this worker has to keep proving itself the same way a
	// real heartbeatLoop would, not just wait silently. A single call
	// followed by a bare sleep would let lastDBContactOK itself age past
	// its own recency bound and fail the gate for an unrelated reason.
	recoveryDone := make(chan struct{})
	go func() {
		defer close(recoveryDone)
		deadline := time.Now().Add(w.reclaimTimeout + 20*time.Millisecond)
		for time.Now().Before(deadline) {
			w.heartbeatAndFenceInFlight()
			time.Sleep(2 * time.Millisecond)
		}
	}()
	<-recoveryDone
	if !w.reapingIsSafe() {
		t.Fatal("reapingIsSafe() = false well after recovery and a full reclaimAfter() window -- the grace period should have cleared")
	}
	w.reapOnce()
	if n := reapSuccesses.Load(); n == 0 {
		t.Fatal("reaper never reclaimed anything once the grace period genuinely cleared")
	}
}
