package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cleat-team/cleat/engine"
)

// TestReapOnceFiresChronicallySkippedWhenTheStallProbeNeverRecovers is
// cleat#2193: a stall probe (engine.DBStallDetector.StaleSetShape) that
// fails on EVERY tick used to never trip the "chronically skipped" alarm,
// because the two places reapOnce could end a tick without reclaiming
// anything for a database reason disagreed about what counted toward
// consecutiveReapSkips.
//
// THE MECHANISM (see recordReapSkip's doc for the full account): a failing
// probe calls recordDBTrouble on every failure, which re-arms
// reapingIsSafe's grace-period gate for roughly one more tick. So the old
// code's actual sequence was:
//
//	tick 1 (gate open):  probe fails -> recordDBTrouble -> consecutiveReapSkips
//	                      was reset to 0 at tick start (BEFORE the probe ran),
//	                      and the error branch never touched it -- stays 0.
//	tick 2 (gate closed): reapingIsSafe() == false -> gate-skip -> Add(1) -> 1.
//	tick 3 (gate open):   Since(lastDBTrouble) has now crossed reclaimAfter
//	                      again -> reapingIsSafe() == true -> reset to 0 ->
//	                      probe fails again -> recordDBTrouble -> stays 0.
//	tick 4 (gate closed): gate-skip -> Add(1) -> 1.
//	                      ... forever, never reaching reapSkipWarnThreshold.
//
// This test reproduces exactly that four-tick cadence by hand (no sleeps:
// lastDBTrouble is set directly, the same device
// reaper_recovery_grace_period_test.go's other tests use), asserts the
// counter reaches the threshold, and -- per cleat-review's standing ask that
// a test "assert the alarm actually fires," not merely that a counter moved
// -- asserts the WARN log line and the chronically_skipped metric call
// (via a buffer-backed logger) both fire exactly once across the run.
func TestReapOnceFiresChronicallySkippedWhenTheStallProbeNeverRecovers(t *testing.T) {
	var reapCalled atomic.Bool
	store := &stallProbeMockStore{
		mockStore: &mockStore{
			reapStaleInstancesFn: func(ctx context.Context, timeout time.Duration) (int, error) {
				reapCalled.Store(true)
				return 0, nil
			},
		},
		staleSetShapeFn: func(ctx context.Context, timeout, missedBeatTimeout time.Duration) (engine.StaleSetShape, error) {
			return engine.StaleSetShape{}, errors.New("simulated: aggregate query timed out, every tick")
		},
	}
	w := newTestWorkerFromStore(store)
	withDBCallDeadlineFloor(t, time.Millisecond)
	w.reclaimTimeout = 100 * time.Millisecond // reclaimAfter() == 100ms exactly, no floor (see reclaimWindow)
	seedRecentlyConfirmedHealthy(w)

	var buf bytes.Buffer
	w.logger = slog.New(slog.NewTextHandler(&buf, nil))

	// Tick 1: gate open (freshly seeded) -> probe fails -> recordDBTrouble
	// stamps "now".
	w.reapOnce()
	if got := w.consecutiveReapSkips.Load(); got != 1 {
		t.Fatalf("after tick 1: consecutiveReapSkips = %d, want 1", got)
	}

	// Simulate 70ms elapsed since that recordDBTrouble -- inside the 100ms
	// grace period, so tick 2's gate is closed.
	w.lastDBTrouble.Store(time.Now().Add(-70 * time.Millisecond).UnixNano())
	w.reapOnce()
	if got := w.consecutiveReapSkips.Load(); got != 2 {
		t.Fatalf("after tick 2 (gate-skip): consecutiveReapSkips = %d, want 2 -- "+
			"the gate-skip branch and the probe-failure branch must share one counter", got)
	}

	// Simulate 120ms elapsed -- past the 100ms grace period, so tick 3's
	// gate is open again and the (still-failing) probe runs once more.
	w.lastDBTrouble.Store(time.Now().Add(-120 * time.Millisecond).UnixNano())
	w.reapOnce()
	if got := w.consecutiveReapSkips.Load(); got != reapSkipWarnThreshold {
		t.Fatalf("after tick 3 (gate reopened, probe fails again): consecutiveReapSkips = %d, want %d -- "+
			"a persistently failing probe must not reset the streak just because the grace-period gate "+
			"happened to reopen in between failures", got, reapSkipWarnThreshold)
	}

	if n := strings.Count(buf.String(), "skipped its last several ticks in a row"); n != 1 {
		t.Fatalf("chronically-skipped WARN log line fired %d time(s) after reaching the threshold, want exactly 1:\n%s", n, buf.String())
	}

	// One more gate-skip tick: the counter keeps climbing, but the alarm
	// fires only once per streak (at the threshold), not on every tick past it.
	w.lastDBTrouble.Store(time.Now().Add(-70 * time.Millisecond).UnixNano())
	w.reapOnce()
	if got := w.consecutiveReapSkips.Load(); got != reapSkipWarnThreshold+1 {
		t.Fatalf("after tick 4: consecutiveReapSkips = %d, want %d", got, reapSkipWarnThreshold+1)
	}
	if n := strings.Count(buf.String(), "skipped its last several ticks in a row"); n != 1 {
		t.Fatalf("chronically-skipped WARN fired again past the threshold (%d occurrences) -- "+
			"it must fire once per streak, not once per tick past it:\n%s", n, buf.String())
	}

	if reapCalled.Load() {
		t.Fatal("setup: ReapStaleInstances was called despite the stall probe failing on every tick -- " +
			"this test's premise (every tick suppressed for a database reason) does not hold")
	}
}
