package main

import (
	"context"
	"testing"
	"time"
)

// cleat#1002: retentionLoop was tick-first on a hardcoded 24-hour ticker, so a
// worker restarting inside a day never swept once -- while --retention-days
// defaulted to 30 and reported the feature as on.
//
// These assert the sweep happens BEFORE the first tick. They must not wait on
// a ticker to do it: a test that passes only because it waited out an interval
// proves the interval elapsed, not that a startup sweep exists. Both use an
// interval far longer than the test could ever run, so anything observed can
// only have come from the pre-tick sweep.

func TestRetentionSweepsOnceAtStartupBeforeAnyTick(t *testing.T) {
	swept := make(chan time.Time, 4)
	ms := &mockStore{}
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		swept <- time.Now()
		return 0, nil
	}
	w := newTestWorker(ms)
	// An hour, so the ticker cannot possibly fire during this test. Whatever
	// arrives on the channel came from the startup sweep or from nothing.
	w.retentionInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx

	w.wg.Add(1)
	go w.retentionLoop(30, 0, 0)

	select {
	case <-swept:
		// The sweep ran without a tick, which is the whole claim.
	case <-time.After(3 * time.Second):
		t.Fatal("retention did not sweep at startup.\n\n" +
			"The ticker is an hour here, so this is not a slow-test problem: " +
			"with retention on and no sweep before the first tick, a worker " +
			"that restarts inside its interval never runs retention at all, " +
			"while --retention-days reports it as enabled and the health " +
			"tracker reports the loop as running.")
	}
	cancel()
}

// TestRetentionStaysOffWhenDisabled is the control. A startup sweep must not
// turn an opt-out into an unconditional one -- completedWorkflowRetentionDays
// defaults to 0 deliberately, because deleting user-visible workflow records
// is a different class of act from trimming events, and retentionLoop's own
// comment says so.
//
// Without this, "sweep at startup" would be satisfied by sweeping always.
func TestRetentionStaysOffWhenDisabled(t *testing.T) {
	ms := &mockStore{}
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		t.Error("DeleteExpiredEvents ran with retention disabled; the startup " +
			"sweep must respect the same guard the loop does")
		return 0, nil
	}
	ms.deleteCompletedWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		t.Error("DeleteCompletedWorkflows ran with retention disabled")
		return 0, nil
	}
	w := newTestWorker(ms)
	w.retentionInterval = time.Hour

	done := make(chan struct{})
	w.wg.Add(1)
	go func() {
		w.retentionLoop(0, 0, 0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("retentionLoop(0, 0) did not return immediately")
	}
}

// TestOnlyTheEnabledHalfSweeps pins the halves apart. --retention-days is on by
// default and --completed-workflow-retention-days is not, so a startup sweep
// that ran both would begin deleting completed workflows on every worker that
// had never opted in -- which the loop's comment calls a materially more
// destructive default.
func TestOnlyTheEnabledHalfSweeps(t *testing.T) {
	events := make(chan struct{}, 4)
	ms := &mockStore{}
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		events <- struct{}{}
		return 0, nil
	}
	ms.deleteCompletedWorkflowsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		t.Error("the startup sweep deleted completed workflows with " +
			"--completed-workflow-retention-days at its default of 0")
		return 0, nil
	}
	w := newTestWorker(ms)
	w.retentionInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.ctx = ctx

	w.wg.Add(1)
	go w.retentionLoop(30, 0, 0)

	select {
	case <-events:
	case <-time.After(3 * time.Second):
		t.Fatal("the events half did not sweep at startup")
	}
	cancel()
}
