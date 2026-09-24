package main

import (
	"context"
	"errors"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// cleat#2006: a whole-fleet database stall silences every worker's
// heartbeat WRITES at once while reads keep working, so every running row
// ages past reclaimAfter() together and whichever reaper reaches the
// database first after recovery reclaims runs that are still alive,
// including its own. This file covers the detector (suspectedDBStall), the
// per-episode suppression state machine (stallSuppressionEpisode.evaluate),
// and reapOnce's wiring of both into the per-unit reclaim loop. See the
// doc comment above missedBeatThreshold in setup.go for the "ramp hole"
// this design exists to close.

// stallProbeMockStore adds a controllable DBStallDetector to mockStore.
// mockStore itself deliberately does not implement DBStallDetector, so a
// test that wants an ordinary (non-detecting) store -- the pre-#2006
// degrade path -- keeps using *mockStore directly; this type is only for
// tests that need reapOnce to see shape data.
type stallProbeMockStore struct {
	*mockStore
	staleSetShapeFn func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error)
}

func (s *stallProbeMockStore) StaleSetShape(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
	if s.staleSetShapeFn != nil {
		return s.staleSetShapeFn(ctx, timeout, missedBeatTimeout)
	}
	return engine.StaleSetShape{}, nil
}

// multiShardStallMockStore satisfies engine.MultiShard over a fixed set of
// named shard stores, each independently typed (so each can choose whether
// it implements DBStallDetector). It also embeds *mockStore purely so the
// top-level value itself satisfies engine.WorkflowStore -- reapUnits never
// calls through to it once a MultiShard type assertion succeeds.
type multiShardStallMockStore struct {
	*mockStore
	names  []string
	shards map[string]engine.WorkflowStore
}

func (m *multiShardStallMockStore) ShardNames() []string { return m.names }

func (m *multiShardStallMockStore) ShardStore(name string) (engine.WorkflowStore, bool) {
	s, ok := m.shards[name]
	return s, ok
}

// stallShapedShape is a StaleSetShape comfortably over every
// suspectedDBStall gate at the defaults this file uses (no recent
// heartbeat anywhere, 3 distinct holders): a clean positive control for
// "this looks like a stall".
func stallShapedShape() engine.StaleSetShape {
	now := time.Now()
	return engine.StaleSetShape{
		Running:                      10,
		MissedBeat:                   9,
		MissedBeatDistinctAssignedTo: 3,
		MissedBeatOldest:             now,
		MissedBeatNewest:             now,
		Stale:                        9,
		NoRecentHeartbeat:            true,
		DistinctAssignedTo:           3,
	}
}

// healthyShape is a StaleSetShape nowhere near any gate: a clean negative
// control for "this looks like ordinary dead-worker staleness, not a
// stall" (see TestReapOnceDoesNotSuppressAHealthyStaleSet's doc for why a
// negative control is load-bearing here, not decorative).
func healthyShape() engine.StaleSetShape {
	now := time.Now()
	return engine.StaleSetShape{
		Running:                      10,
		MissedBeat:                   1,
		MissedBeatDistinctAssignedTo: 1,
		MissedBeatOldest:             now,
		MissedBeatNewest:             now,
		Stale:                        1,
		NoRecentHeartbeat:            false,
		DistinctAssignedTo:           2,
	}
}

// --- suspectedDBStall: the pure detector function ---------------------
//
// cleat-review on cleat#2006 (2026-09-24) replaced the original
// fraction+spread criterion with NoRecentHeartbeat && DistinctAssignedTo>1
// -- see suspectedDBStall's own doc comment for why (a residual
// false-negative right at the 80% boundary, and an unbounded spread
// widening from one never-reclaimed row). These tests cover the new
// criterion directly.

// TestSuspectedDBStallRequiresNoRecentHeartbeatAnywhere is the direct
// replacement for the old fraction test: a single fresh survivor anywhere
// in scope -- NoRecentHeartbeat false -- must block suspicion outright,
// regardless of how stale every other row is.
func TestSuspectedDBStallRequiresNoRecentHeartbeatAnywhere(t *testing.T) {
	shape := stallShapedShape() // every other gate satisfied

	shape.NoRecentHeartbeat = false
	if suspectedDBStall(shape) {
		t.Fatal("suspectedDBStall fired with NoRecentHeartbeat=false -- a fresh survivor anywhere in scope must block suspicion")
	}

	shape.NoRecentHeartbeat = true
	if !suspectedDBStall(shape) {
		t.Fatal("suspectedDBStall did not fire with NoRecentHeartbeat=true and every other gate satisfied")
	}
}

// TestSuspectedDBStallRequiresMoreThanOneDistinctAssignedTo pins down the
// case setup.go's doc comment calls out explicitly: a single-worker fleet
// can never trip this, by design -- that worker's own probeBoundedCall
// failures already cover it via reapingIsSafe (#2166), and treating its
// own dead-worker cleanup as a "stall" would suppress reclaiming runs
// nobody else can ever pick up.
func TestSuspectedDBStallRequiresMoreThanOneDistinctAssignedTo(t *testing.T) {
	shape := stallShapedShape() // NoRecentHeartbeat true, every other gate satisfied
	shape.DistinctAssignedTo = 1

	if suspectedDBStall(shape) {
		t.Fatal("suspectedDBStall fired with only one distinct assigned_to -- a single-worker fleet's own dead-worker cleanup must never suppress as a stall")
	}

	shape.DistinctAssignedTo = 2
	if !suspectedDBStall(shape) {
		t.Fatal("suspectedDBStall did not fire once a second distinct assigned_to appeared, with every other gate already satisfied")
	}
}

// TestSuspectedDBStallIgnoresRunningZero is the Running==0 guard: an empty
// running set must never read as a stall, whatever NoRecentHeartbeat and
// DistinctAssignedTo happen to be (both are meaningless -- and, per the
// SQL, defaulted -- over zero rows).
func TestSuspectedDBStallIgnoresRunningZero(t *testing.T) {
	shape := stallShapedShape()
	shape.Running = 0
	if suspectedDBStall(shape) {
		t.Fatal("suspectedDBStall fired with Running=0")
	}
}

// TestMissedBeatThresholdArithmetic pins the worked example from
// missedBeatThreshold's own doc comment (8.5s at the 5s default) so a
// change to dbCallDeadlineFor or missedBeatSlack that silently moves this
// number gets caught here rather than only in prose.
func TestMissedBeatThresholdArithmetic(t *testing.T) {
	const heartbeat = 5 * time.Second
	want := heartbeat + dbCallDeadlineFor(heartbeat) + missedBeatSlack
	if got := missedBeatThreshold(heartbeat); got != want {
		t.Fatalf("missedBeatThreshold(5s) = %v, want %v", got, want)
	}
	if want != 8500*time.Millisecond {
		t.Fatalf("missedBeatThreshold(5s) = %v, want 8.5s (the documented default) -- the doc comment and the arithmetic have diverged", want)
	}
}

// --- stallSuppressionEpisode.evaluate: the state machine, driven by an
// explicit `now` so these need no real sleeps at all. ---------------------

func TestStallSuppressionEpisodeSuppressesUntilTheReclaimBoundElapses(t *testing.T) {
	const reclaimAfter = 100 * time.Millisecond // stands in for R; only its relation to `now` matters here
	shape := stallShapedShape()
	t0 := time.Unix(1_700_000_000, 0)

	e := &stallSuppressionEpisode{}

	d := e.evaluate(shape, reclaimAfter, t0)
	if !d.Suppress || d.BoundHit {
		t.Fatalf("tick 1 (t0): got %+v, want Suppress=true BoundHit=false (episode just opened)", d)
	}

	d = e.evaluate(shape, reclaimAfter, t0.Add(reclaimAfter-time.Millisecond))
	if !d.Suppress || d.BoundHit {
		t.Fatalf("tick just under the bound: got %+v, want Suppress=true BoundHit=false", d)
	}

	d = e.evaluate(shape, reclaimAfter, t0.Add(reclaimAfter))
	if d.Suppress || !d.BoundHit {
		t.Fatalf("tick at the bound: got %+v, want Suppress=false BoundHit=true (the transition)", d)
	}

	d = e.evaluate(shape, reclaimAfter, t0.Add(reclaimAfter+time.Second))
	if d.Suppress || d.BoundHit {
		t.Fatalf("tick well past the bound: got %+v, want Suppress=false BoundHit=false (alert already fired once)", d)
	}
}

// TestStallSuppressionEpisodeDoesNotResetOnAMereRatioDip is the
// oscillating-near-80% case from the design: a mass-death event where the
// fraction wobbles above and below the gate tick to tick must not win a
// fresh grace period on every dip below it. Only true quiescence (Stale
// reaching zero) may reset the clock -- see evaluate's own doc comment.
//
// CHANGED 2026-09-24 (cleat-review, cleat#2006 round 2): the dip tick used
// to assert the zero decision (Suppress=false) here, on the theory that
// "not suspected this tick" was itself enough reason to stop suppressing.
// It is not -- that is exactly the recovery-tail bug cleat-review found:
// dip.Stale stays 3, meaning three rows are still individually reclaim-
// eligible, and un-suppressing on a tick that merely stopped LOOKING
// suspected reclaims them on the strength of whatever made the ratio dip
// (e.g. one worker's heartbeat landing first). evaluate is now sticky --
// an open episode with Stale > 0 keeps suppressing on its existing clock
// regardless of whether this particular tick is itself suspected -- so the
// dip tick now asserts Suppress=true, matching the fix rather than the bug
// it used to pin.
func TestStallSuppressionEpisodeDoesNotResetOnAMereRatioDip(t *testing.T) {
	const reclaimAfter = 100 * time.Millisecond
	t0 := time.Unix(1_700_000_000, 0)
	e := &stallSuppressionEpisode{}

	d := e.evaluate(stallShapedShape(), reclaimAfter, t0)
	if !d.Suppress {
		t.Fatalf("tick 1: got %+v, want Suppress=true (episode opened)", d)
	}

	// Tick 2: ratio dips under the fraction gate (not suspected), but the
	// stale set is not empty -- e.g. a worker or two came back briefly.
	// This must not reset the episode clock, AND -- the recovery-tail fix --
	// must not un-suppress the still-stale rows either: sticky suppression
	// keeps this tick suppressed on the ORIGINAL clock, well inside its
	// reclaimAfter window.
	dip := healthyShape()
	dip.Stale = 3 // still nonzero: not true quiescence
	d = e.evaluate(dip, reclaimAfter, t0.Add(10*time.Millisecond))
	if !d.Suppress || d.BoundHit {
		t.Fatalf("dip tick: got %+v, want Suppress=true BoundHit=false -- sticky suppression must hold while Stale > 0, even on a tick that is not itself suspected", d)
	}

	// Tick 3: suspected again, exactly at the ORIGINAL bound from t0. If
	// the dip had reset the clock, this would still be well inside a
	// fresh window and would wrongly suppress.
	d = e.evaluate(stallShapedShape(), reclaimAfter, t0.Add(reclaimAfter))
	if d.Suppress || !d.BoundHit {
		t.Fatalf("tick at the original bound after an intervening dip: got %+v, want Suppress=false BoundHit=true -- the dip must not have reset the episode clock", d)
	}
}

// TestStallSuppressionEpisodeResetsOnTrueQuiescence is the
// real-stall-full-recovery case: once writes genuinely resume and nothing
// reclaim-eligible remains (Stale==0), the NEXT suspected episode is a
// fresh one -- it gets its own full grace period rather than inheriting
// the elapsed time from a stall that already ended.
func TestStallSuppressionEpisodeResetsOnTrueQuiescence(t *testing.T) {
	const reclaimAfter = 100 * time.Millisecond
	t0 := time.Unix(1_700_000_000, 0)
	e := &stallSuppressionEpisode{}

	d := e.evaluate(stallShapedShape(), reclaimAfter, t0)
	if !d.Suppress {
		t.Fatalf("tick 1: got %+v, want Suppress=true", d)
	}

	quiescent := healthyShape()
	quiescent.MissedBeat = 0
	quiescent.Stale = 0
	d = e.evaluate(quiescent, reclaimAfter, t0.Add(20*time.Millisecond))
	if d.Suppress || d.BoundHit {
		t.Fatalf("quiescent tick: got %+v, want the zero decision", d)
	}

	// A brand-new stall begins right at what would have been the OLD
	// bound. If the clock had not reset, this would report BoundHit; a
	// fresh episode must instead suppress from the start.
	d = e.evaluate(stallShapedShape(), reclaimAfter, t0.Add(reclaimAfter))
	if !d.Suppress || d.BoundHit {
		t.Fatalf("first tick of the new episode: got %+v, want Suppress=true BoundHit=false -- quiescence must have reset the episode clock", d)
	}
}

// TestStallSuppressionEpisodeStaysStickyThroughTheRecoveryTail is
// cleat-review's exact round-2 finding on cleat#2006 (2026-09-24), the "S3"
// case: a stall does not end for every worker at once, and the FIRST
// heartbeat to land un-suspects the whole shape before the laggards'
// heartbeats catch up.
//
//	tick 1 (t0):      3 workers, all stale, suspected -- episode opens.
//	tick 2 (t0+1s):   worker a's heartbeat lands. NoRecentHeartbeat flips
//	                  false (suspectedDBStall stops firing) but b and c
//	                  are still individually stale (Stale=2). Pre-fix,
//	                  this was the zero decision -- unsuppressed -- and
//	                  reapOnce would reclaim b and c on a's heartbeat
//	                  alone, at 1s into a reclaimAfter window this test
//	                  sets to 10s. Fixed: sticky suppression holds.
//	tick 3 (t0+10s):  the original bound. Genuinely suspected again (b
//	                  and c's laggard heartbeats have not landed either)
//	                  is not required -- Stale is still nonzero and the
//	                  clock from tick 1 has now elapsed, so this reaches
//	                  the bound via the SAME episode, not a fresh one.
//
// This is the sweep test's own documented blind spot, named directly in
// cleat-review's finding: simulateStaleSetShape only ticks while
// tickOffset < D, so it never observes a tick where the FLEET has recovered
// (NoRecentHeartbeat false) while individual ROWS have not (Stale > 0).
// That gap is why the sweep read "0 wrongful reclaims at every offset"
// while this exact case still reclaimed two live rows.
func TestStallSuppressionEpisodeStaysStickyThroughTheRecoveryTail(t *testing.T) {
	const reclaimAfter = 10 * time.Second
	t0 := time.Unix(1_700_000_000, 0)
	e := &stallSuppressionEpisode{}

	allStale := engine.StaleSetShape{
		Running: 3, MissedBeat: 3, MissedBeatDistinctAssignedTo: 3,
		Stale: 3, NoRecentHeartbeat: true, DistinctAssignedTo: 3,
	}
	d := e.evaluate(allStale, reclaimAfter, t0)
	if !d.Suppress || d.BoundHit {
		t.Fatalf("tick 1: got %+v, want Suppress=true BoundHit=false (episode opens)", d)
	}

	// a's heartbeat landed; b and c have not caught up yet.
	aRecovered := engine.StaleSetShape{
		Running: 3, MissedBeat: 2, MissedBeatDistinctAssignedTo: 2,
		Stale: 2, NoRecentHeartbeat: false, DistinctAssignedTo: 3,
	}
	d = e.evaluate(aRecovered, reclaimAfter, t0.Add(1*time.Second))
	if !d.Suppress || d.BoundHit {
		t.Fatalf("recovery-tail tick (a back, b/c still stale): got %+v, want Suppress=true BoundHit=false -- "+
			"one survivor's heartbeat must not un-suppress the laggards still inside their reclaimAfter window", d)
	}

	// b and c still have not caught up; the bound from tick 1 arrives.
	d = e.evaluate(aRecovered, reclaimAfter, t0.Add(reclaimAfter))
	if d.Suppress || !d.BoundHit {
		t.Fatalf("tick at the original bound, still mid-recovery: got %+v, want Suppress=false BoundHit=true -- "+
			"the episode clock from tick 1 must govern, not a fresh one started by the recovery-tail tick", d)
	}
}

// --- reapOnce: the full wiring through the mock store -------------------

func TestReapOnceSuppressesReclaimWhenStaleSetLooksLikeADatabaseStall(t *testing.T) {
	var reapCalled atomic.Bool
	store := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				reapCalled.Store(true)
				return 5, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return stallShapedShape(), nil
		},
	}
	w := newTestWorkerFromStore(store)
	withDBCallDeadlineFloor(t, time.Millisecond)
	w.reclaimTimeout = time.Second
	seedRecentlyConfirmedHealthy(w)

	w.reapOnce()

	if reapCalled.Load() {
		t.Fatal("reapOnce called ReapStaleInstances on a stall-shaped stale set -- it must suppress and wait a tick instead")
	}
}

// TestReapOnceDoesNotSuppressAHealthyStaleSet is the negative control for
// the test above: a stale set that looks like ordinary dead-worker
// staleness (low fraction, one holder) must reclaim exactly as it did
// before cleat#2006. Without this, a detector that always returns
// Suppress=true would still pass every suppression test in this file.
func TestReapOnceDoesNotSuppressAHealthyStaleSet(t *testing.T) {
	var reapCalled atomic.Bool
	store := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				reapCalled.Store(true)
				return 1, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return healthyShape(), nil
		},
	}
	w := newTestWorkerFromStore(store)
	withDBCallDeadlineFloor(t, time.Millisecond)
	w.reclaimTimeout = time.Second
	seedRecentlyConfirmedHealthy(w)

	w.reapOnce()

	if !reapCalled.Load() {
		t.Fatal("reapOnce suppressed reclaim on an ordinary (non-stall-shaped) stale set -- suspectedDBStall must have false-positived")
	}
}

// TestReapOnceDegradesTransparentlyWhenStoreDoesNotImplementDBStallDetector
// is the pre-#2006 path: a store with no StaleSetShape method (every store
// before this feature) must reclaim exactly as before -- reapOnce's type
// assertion has to fail closed to "no suppression available", never panic
// or silently suppress forever.
func TestReapOnceDegradesTransparentlyWhenStoreDoesNotImplementDBStallDetector(t *testing.T) {
	var reapCalled atomic.Bool
	store := &mockStore{
		reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
			reapCalled.Store(true)
			return 1, nil
		},
	}
	w := newTestWorker(store)
	w.reclaimTimeout = 50 * time.Millisecond
	seedRecentlyConfirmedHealthy(w)

	w.reapOnce()

	if !reapCalled.Load() {
		t.Fatal("reapOnce did not reclaim against a store with no DBStallDetector -- the new detection path must degrade transparently, not swallow reclaims")
	}
}

// TestReapOnceSkipsReclaimWhenTheStallProbeItselfFails is cleat-review's
// GAP 1 (cleat#2006, 2026-09-24): a StaleSetShape call that errors used to
// fall through to an UNSUPPRESSED ReapStaleInstances -- exactly backwards,
// because a slow or failing aggregate over the same table
// ReapStaleInstances is about to UPDATE is itself the shape a genuine
// stall takes. This is the known-positive: a store whose StaleSetShape
// always errors must never reclaim, on a unit that implements
// DBStallDetector at all -- "I could not check" must fail closed, not
// open.
func TestReapOnceSkipsReclaimWhenTheStallProbeItselfFails(t *testing.T) {
	var reapCalled atomic.Bool
	store := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				reapCalled.Store(true)
				return 5, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return engine.StaleSetShape{}, errors.New("simulated: aggregate query timed out")
		},
	}
	w := newTestWorkerFromStore(store)
	withDBCallDeadlineFloor(t, time.Millisecond)
	w.reclaimTimeout = time.Second
	seedRecentlyConfirmedHealthy(w)

	w.reapOnce()

	if reapCalled.Load() {
		t.Fatal("reapOnce called ReapStaleInstances despite the stall probe itself failing -- a failed probe must fail CLOSED (skip this tick's reclaim), not open")
	}
}

// TestReapOnceTreatsEachShardsProbeFailureIndependently is the fail-closed
// fix combined with the per-shard-independence guarantee: one shard whose
// probe fails must not stop a healthy sibling, whose own probe succeeded
// and found nothing stall-shaped, from reclaiming.
func TestReapOnceTreatsEachShardsProbeFailureIndependently(t *testing.T) {
	var failedShardReaped, healthyShardReaped atomic.Bool
	probeFails := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				failedShardReaped.Store(true)
				return 5, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return engine.StaleSetShape{}, errors.New("simulated: aggregate query timed out")
		},
	}
	healthy := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				healthyShardReaped.Store(true)
				return 1, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return healthyShape(), nil
		},
	}
	top := &multiShardStallMockStore{
		mockStore: &mockStore{},
		names:     []string{"probe-fails-shard", "healthy-shard"},
		shards: map[string]engine.WorkflowStore{
			"probe-fails-shard": probeFails,
			"healthy-shard":     healthy,
		},
	}
	w := newTestWorkerFromStore(top)
	withDBCallDeadlineFloor(t, time.Millisecond)
	w.reclaimTimeout = time.Second
	seedRecentlyConfirmedHealthy(w)

	w.reapOnce()

	if failedShardReaped.Load() {
		t.Fatal("the shard whose stall probe failed still reclaimed -- a failed probe on one shard must not bypass its own fail-closed suppression")
	}
	if !healthyShardReaped.Load() {
		t.Fatal("the healthy shard never reclaimed -- one shard's failed probe must not suppress a healthy sibling")
	}
}

// TestReapOnceReclaimsOnceTheSuppressionBoundIsReached drives real reaper
// ticks (short real sleeps, ms-scale reclaimTimeout) until the suppression
// bound elapses, and checks both halves: nothing is reclaimed before the
// bound, and reclaiming resumes once it is hit -- the mass-death-with-bound
// case, minus the metric assertion (this package has no OTel test reader
// wired in; ReapStaleInstances being called is the load-bearing behavior).
func TestReapOnceReclaimsOnceTheSuppressionBoundIsReached(t *testing.T) {
	var reapCount atomic.Int64
	store := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				reapCount.Add(1)
				return 5, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return stallShapedShape(), nil
		},
	}
	w := newTestWorkerFromStore(store)
	withDBCallDeadlineFloor(t, time.Millisecond)
	const bound = 40 * time.Millisecond
	w.reclaimTimeout = bound
	seedRecentlyConfirmedHealthy(w)

	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		w.reapOnce()
		if reapCount.Load() != 0 {
			t.Fatalf("ReapStaleInstances was called before the suppression bound (%v) elapsed", bound)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Poll past the bound; a persistent stall must eventually resume
	// reclaiming rather than suppress forever.
	ok := false
	for i := 0; i < 50; i++ {
		w.reapOnce()
		if reapCount.Load() != 0 {
			ok = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !ok {
		t.Fatal("ReapStaleInstances was never called even well past the suppression bound -- a persistent stall-shaped stale set must not suppress forever")
	}
}

// TestReapOnceTreatsEachShardIndependently is the per-shard-independence
// case: one stalled shard must not pause reclaiming on a healthy sibling
// processed in the same tick, and a healthy majority must not mask a
// genuinely stalled minority.
func TestReapOnceTreatsEachShardIndependently(t *testing.T) {
	var stalledReaped, healthyReaped atomic.Bool
	stalled := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				stalledReaped.Store(true)
				return 5, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return stallShapedShape(), nil
		},
	}
	healthy := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				healthyReaped.Store(true)
				return 1, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return healthyShape(), nil
		},
	}
	top := &multiShardStallMockStore{
		mockStore: &mockStore{},
		names:     []string{"stalled-shard", "healthy-shard"},
		shards: map[string]engine.WorkflowStore{
			"stalled-shard": stalled,
			"healthy-shard": healthy,
		},
	}
	w := newTestWorkerFromStore(top)
	withDBCallDeadlineFloor(t, time.Millisecond)
	w.reclaimTimeout = time.Second
	seedRecentlyConfirmedHealthy(w)

	w.reapOnce()

	if stalledReaped.Load() {
		t.Fatal("the stalled shard's ReapStaleInstances was called -- its own suppression must not have been bypassed by the healthy sibling")
	}
	if !healthyReaped.Load() {
		t.Fatal("the healthy shard's ReapStaleInstances was never called -- one shard's suppression must not mask reclaiming on a healthy sibling")
	}
}

// --- D-sweep: the shape of the whole design, driven the way cleat-review
// drove its own comparison for cleat#2006 (2026-09-24). ------------------

// simulateStaleSetShape computes the StaleSetShape a real store would
// report at simulated time `now`, given each worker's pre-stall heartbeat
// phase (how long before the stall began it last wrote) and a stall that
// has been running since t0 for at least `now.Sub(t0)` (the caller does
// not drive this past the stall's own end -- see the sweep below).
func simulateStaleSetShape(now, t0 time.Time, phases []time.Duration, heartbeat, reclaimAfter time.Duration) engine.StaleSetShape {
	missedBeat := missedBeatThreshold(heartbeat)
	var shape engine.StaleSetShape
	shape.Running = len(phases)
	shape.DistinctAssignedTo = len(phases)
	shape.MissedBeatDistinctAssignedTo = len(phases)
	var newest time.Time
	for _, phase := range phases {
		lastWrite := t0.Add(-phase)
		if lastWrite.After(newest) {
			newest = lastWrite
		}
		age := now.Sub(lastWrite)
		if age >= missedBeat {
			shape.MissedBeat++
		}
		if age >= reclaimAfter {
			shape.Stale++
		}
	}
	shape.NoRecentHeartbeat = now.Sub(newest) >= missedBeat
	return shape
}

// TestSuspectedStallProtectionHoldsBelowTheDocumentedBound is a D-sweep
// simulation modeled on cleat-review's own methodology: 5 workers, random
// heartbeat phases, one worker pinned to have written its last heartbeat
// the INSTANT the stall began (phase 0 -- the widest possible spread
// against the other four), stall durations D from 4s to 36s, 20 random
// trials per D. It asserts exactly what stallProtectionLower documents:
// no wrongful reclaim -- Suppress=false while a row is Stale>0 and the
// stall has not yet ended -- for any D strictly below the protection
// bound, whatever the phases happen to be.
//
// This is a hand-computed StaleSetShape per simulated tick against
// stallSuppressionEpisode.evaluate directly, the same style as the
// evaluate tests above, so the whole sweep runs with no real sleeps and no
// per-D flakiness from wall-clock timing.
func TestSuspectedStallProtectionHoldsBelowTheDocumentedBound(t *testing.T) {
	const heartbeat = 5 * time.Second
	const numWorkers = 5
	// Pinned rather than computed from minimumReclaimAfter, deliberately:
	// this test's whole point is to notice if the protection bound moves,
	// and computing R from the same model that produces the bound would
	// let both drift together silently.
	const reclaimAfter = 14500 * time.Millisecond
	interval := max(heartbeat, 10*time.Second)
	lower := stallProtectionLower(heartbeat, reclaimAfter)
	if lower != 23*time.Second {
		t.Fatalf("stallProtectionLower(5s, 14.5s) = %v, want 23s -- the documented default has drifted; update this test and the doc comment together", lower)
	}

	rng := rand.New(rand.NewSource(1))
	const trialsPerD = 20
	totalBelowBound := 0
	wrongfulReclaims := 0

	for dSeconds := 4; dSeconds <= 36; dSeconds += 2 {
		D := time.Duration(dSeconds) * time.Second
		belowBound := D < lower
		for trial := 0; trial < trialsPerD; trial++ {
			phases := make([]time.Duration, numWorkers)
			phases[0] = 0 // pinned: this worker just wrote, right as the stall began
			for i := 1; i < numWorkers; i++ {
				phases[i] = time.Duration(rng.Int63n(int64(heartbeat)))
			}
			// Reaper ticks are not synchronized to the stall's start.
			firstTickOffset := time.Duration(rng.Int63n(int64(interval)))

			episode := &stallSuppressionEpisode{}
			t0 := time.Unix(1_700_000_000, 0)
			wrongful := false

			for tickOffset := firstTickOffset; tickOffset < D; tickOffset += interval {
				now := t0.Add(tickOffset)
				shape := simulateStaleSetShape(now, t0, phases, heartbeat, reclaimAfter)
				decision := episode.evaluate(shape, reclaimAfter, now)
				if !decision.Suppress && shape.Stale > 0 {
					// The stall has NOT ended (tickOffset < D) and at
					// least one row would actually be reclaimed this
					// tick -- that row is still genuinely alive.
					wrongful = true
				}
			}

			if belowBound {
				totalBelowBound++
				if wrongful {
					wrongfulReclaims++
					t.Errorf("D=%v (below the %v protection bound): wrongful reclaim with phases=%v firstTickOffset=%v", D, lower, phases, firstTickOffset)
				}
			}
		}
	}

	if totalBelowBound == 0 {
		t.Fatal("no trials ran below the protection bound -- the sweep range or bound computation is wrong")
	}
	t.Logf("%d trials below the %v protection bound; %d wrongful reclaims", totalBelowBound, lower, wrongfulReclaims)
}

// TestSuspectedStallProtectionSweepDetectsAWrongfulReclaimWhenOneOccurs is
// the known-positive control for the sweep above: a stall long enough that
// suppression MUST eventually give up (BoundHit) and let a still-technically-
// alive row reclaim. Without this, a sweep harness with an inverted
// condition -- or one that never actually drives episode.evaluate into
// the unsuppressed state -- would report zero wrongful reclaims for every
// D, including one deliberately chosen to demonstrate the bound's own
// failure mode, and the test above would be vacuous.
func TestSuspectedStallProtectionSweepDetectsAWrongfulReclaimWhenOneOccurs(t *testing.T) {
	const heartbeat = 5 * time.Second
	const reclaimAfter = 14500 * time.Millisecond
	interval := max(heartbeat, 10*time.Second)
	upper := stallProtectionUpper(heartbeat, reclaimAfter)

	// Comfortably past the upper bound -- every phase must observe a
	// wrongful reclaim, not just an unlucky one.
	D := upper + 30*time.Second
	phases := []time.Duration{0, 0, 0, 0, 0}
	episode := &stallSuppressionEpisode{}
	t0 := time.Unix(1_700_000_000, 0)
	wrongful := false

	for tickOffset := time.Duration(0); tickOffset < D; tickOffset += interval {
		now := t0.Add(tickOffset)
		shape := simulateStaleSetShape(now, t0, phases, heartbeat, reclaimAfter)
		decision := episode.evaluate(shape, reclaimAfter, now)
		if !decision.Suppress && shape.Stale > 0 {
			wrongful = true
			break
		}
	}

	if !wrongful {
		t.Fatalf("a %v stall (comfortably past the %v protection upper bound) never produced a reclaim while Stale>0 -- the sweep's own wrongful-reclaim detection cannot fire, which makes the test above vacuous", D, upper)
	}
}
