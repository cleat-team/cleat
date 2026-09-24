package main

import (
	"context"
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
// suspectedDBStall gate at the defaults this file uses (90% > 80%, 3
// distinct holders, zero spread): a clean positive control for "this looks
// like a stall".
func stallShapedShape() engine.StaleSetShape {
	now := time.Now()
	return engine.StaleSetShape{
		Running:                      10,
		MissedBeat:                   9,
		MissedBeatDistinctAssignedTo: 3,
		MissedBeatOldest:             now,
		MissedBeatNewest:             now,
		Stale:                        9,
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
	}
}

// --- suspectedDBStall: the pure detector function ---------------------

func TestSuspectedDBStallRequiresFractionAboveEightyPercent(t *testing.T) {
	const heartbeat = 5 * time.Second
	base := stallShapedShape()
	base.MissedBeatDistinctAssignedTo = 2

	tests := []struct {
		name       string
		missedBeat int
		want       bool
	}{
		// 8/10 = 80% exactly -- suspectedDBStall requires STRICTLY above
		// the fraction (">") per its own source, not ">=".
		{"exactly at the threshold does not count", 8, false},
		{"one row over the threshold does", 9, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shape := base
			shape.MissedBeat = tt.missedBeat
			if got := suspectedDBStall(shape, heartbeat); got != tt.want {
				t.Fatalf("suspectedDBStall(missedBeat=%d of %d) = %v, want %v", tt.missedBeat, shape.Running, got, tt.want)
			}
		})
	}
}

// TestSuspectedDBStallRequiresMoreThanOneDistinctAssignedTo pins down the
// case setup.go's doc comment calls out explicitly: a single-worker fleet
// can never trip this, by design -- that worker's own probeBoundedCall
// failures already cover it via reapingIsSafe (#2166), and treating its
// own dead-worker cleanup as a "stall" would suppress reclaiming runs
// nobody else can ever pick up.
func TestSuspectedDBStallRequiresMoreThanOneDistinctAssignedTo(t *testing.T) {
	const heartbeat = 5 * time.Second
	shape := stallShapedShape() // 90% missed, well over the fraction gate
	shape.MissedBeatDistinctAssignedTo = 1

	if suspectedDBStall(shape, heartbeat) {
		t.Fatal("suspectedDBStall fired with only one distinct assigned_to -- a single-worker fleet's own dead-worker cleanup must never suppress as a stall")
	}

	shape.MissedBeatDistinctAssignedTo = 2
	if !suspectedDBStall(shape, heartbeat) {
		t.Fatal("suspectedDBStall did not fire once a second distinct assigned_to appeared, with every other gate already satisfied")
	}
}

// TestSuspectedDBStallRequiresSpreadWithinMissedBeatThreshold is the
// detector-level form of the "ramp hole" fix: a whole-fleet stall's missed
// beats cluster within one worker's own write-cycle width
// (missedBeatThreshold), not spread arbitrarily. A wide spread is ordinary
// staggered dead-worker attrition, not one synchronized event, even at an
// identical fraction and distinct-holder count.
func TestSuspectedDBStallRequiresSpreadWithinMissedBeatThreshold(t *testing.T) {
	const heartbeat = 5 * time.Second
	threshold := missedBeatThreshold(heartbeat)
	base := stallShapedShape()

	tests := []struct {
		name   string
		spread time.Duration
		want   bool
	}{
		{"spread exactly at the threshold still counts", threshold, true},
		{"spread one millisecond over the threshold does not", threshold + time.Millisecond, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shape := base
			shape.MissedBeatOldest = time.Unix(0, 0)
			shape.MissedBeatNewest = shape.MissedBeatOldest.Add(tt.spread)
			if got := suspectedDBStall(shape, heartbeat); got != tt.want {
				t.Fatalf("suspectedDBStall(spread=%v, threshold=%v) = %v, want %v", tt.spread, threshold, got, tt.want)
			}
		})
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
	const heartbeat = 5 * time.Second
	const reclaimAfter = 100 * time.Millisecond // stands in for R; only its relation to `now` matters here
	shape := stallShapedShape()
	t0 := time.Unix(1_700_000_000, 0)

	e := &stallSuppressionEpisode{}

	d := e.evaluate(shape, heartbeat, reclaimAfter, t0)
	if !d.Suppress || d.BoundHit {
		t.Fatalf("tick 1 (t0): got %+v, want Suppress=true BoundHit=false (episode just opened)", d)
	}

	d = e.evaluate(shape, heartbeat, reclaimAfter, t0.Add(reclaimAfter-time.Millisecond))
	if !d.Suppress || d.BoundHit {
		t.Fatalf("tick just under the bound: got %+v, want Suppress=true BoundHit=false", d)
	}

	d = e.evaluate(shape, heartbeat, reclaimAfter, t0.Add(reclaimAfter))
	if d.Suppress || !d.BoundHit {
		t.Fatalf("tick at the bound: got %+v, want Suppress=false BoundHit=true (the transition)", d)
	}

	d = e.evaluate(shape, heartbeat, reclaimAfter, t0.Add(reclaimAfter+time.Second))
	if d.Suppress || d.BoundHit {
		t.Fatalf("tick well past the bound: got %+v, want Suppress=false BoundHit=false (alert already fired once)", d)
	}
}

// TestStallSuppressionEpisodeDoesNotResetOnAMereRatioDip is the
// oscillating-near-80% case from the design: a mass-death event where the
// fraction wobbles above and below the gate tick to tick must not win a
// fresh grace period on every dip below it. Only true quiescence (Stale
// reaching zero) may reset the clock -- see evaluate's own doc comment.
func TestStallSuppressionEpisodeDoesNotResetOnAMereRatioDip(t *testing.T) {
	const heartbeat = 5 * time.Second
	const reclaimAfter = 100 * time.Millisecond
	t0 := time.Unix(1_700_000_000, 0)
	e := &stallSuppressionEpisode{}

	d := e.evaluate(stallShapedShape(), heartbeat, reclaimAfter, t0)
	if !d.Suppress {
		t.Fatalf("tick 1: got %+v, want Suppress=true (episode opened)", d)
	}

	// Tick 2: ratio dips under the fraction gate (not suspected), but the
	// stale set is not empty -- e.g. a worker or two came back briefly.
	// This must not reset the episode clock.
	dip := healthyShape()
	dip.Stale = 3 // still nonzero: not true quiescence
	d = e.evaluate(dip, heartbeat, reclaimAfter, t0.Add(10*time.Millisecond))
	if d.Suppress || d.BoundHit {
		t.Fatalf("dip tick: got %+v, want the zero decision (not suspected this tick)", d)
	}

	// Tick 3: suspected again, exactly at the ORIGINAL bound from t0. If
	// the dip had reset the clock, this would still be well inside a
	// fresh window and would wrongly suppress.
	d = e.evaluate(stallShapedShape(), heartbeat, reclaimAfter, t0.Add(reclaimAfter))
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
	const heartbeat = 5 * time.Second
	const reclaimAfter = 100 * time.Millisecond
	t0 := time.Unix(1_700_000_000, 0)
	e := &stallSuppressionEpisode{}

	d := e.evaluate(stallShapedShape(), heartbeat, reclaimAfter, t0)
	if !d.Suppress {
		t.Fatalf("tick 1: got %+v, want Suppress=true", d)
	}

	quiescent := healthyShape()
	quiescent.MissedBeat = 0
	quiescent.Stale = 0
	d = e.evaluate(quiescent, heartbeat, reclaimAfter, t0.Add(20*time.Millisecond))
	if d.Suppress || d.BoundHit {
		t.Fatalf("quiescent tick: got %+v, want the zero decision", d)
	}

	// A brand-new stall begins right at what would have been the OLD
	// bound. If the clock had not reset, this would report BoundHit; a
	// fresh episode must instead suppress from the start.
	d = e.evaluate(stallShapedShape(), heartbeat, reclaimAfter, t0.Add(reclaimAfter))
	if !d.Suppress || d.BoundHit {
		t.Fatalf("first tick of the new episode: got %+v, want Suppress=true BoundHit=false -- quiescence must have reset the episode clock", d)
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
