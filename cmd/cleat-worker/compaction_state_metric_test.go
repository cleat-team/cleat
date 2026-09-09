package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// cleat#1024: the retention sweep did two things and reported one number.
//
// DeleteExpiredEvents deleted event_history rows and, in a second loop,
// cleared compaction bookkeeping on workflow_instances -- discarding that
// loop's RowsAffected. The first loop can never match (finalize already purged
// those events, cleat#1016), so the sweep logged "deleted 0" on runs where it
// had cleared thousands of rows.
//
// The obvious fix was to sum them. These tests exist to stop that: the two
// counts are of different tables and different operations, and one number
// reporting both is a number that means two things.

func TestTheSweepReportsTheCompactionClearSeparately(t *testing.T) {
	var eventsCutoff, clearCutoff time.Time
	ms := &mockStore{}
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		eventsCutoff = olderThan
		return 0, nil // structurally zero -- see cleat#1016
	}
	ms.clearExpiredCompactionStateFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		clearCutoff = olderThan
		return 4200, nil
	}
	w := newTestWorker(ms)

	w.runRetentionSweep(30, 0, 0)

	if clearCutoff.IsZero() {
		t.Fatal("ClearExpiredCompactionState was not called by the retention sweep.\n\n" +
			"Before cleat#1024 this work happened inside DeleteExpiredEvents and " +
			"its row count was discarded, so a sweep that cleared thousands of " +
			"rows reported zero.")
	}
	if !eventsCutoff.Equal(clearCutoff) {
		t.Errorf("the two halves used different cutoffs (%v vs %v); they are one "+
			"sweep of one retention window and must agree", eventsCutoff, clearCutoff)
	}
}

// The "counts are not summed" claim is asserted in engine/, against real
// databases -- see TestTheCompactionClearIsCountedApartFromTheEventDelete.
// A version of it lived here and was VACUOUS: it drove a mockStore whose
// deleteExpiredEventsFn returned 0 and then asserted the result was 0, so it
// could not fail. It passed unchanged when the two counts were summed at the
// call site, which is the mutation it existed to catch.

// TestAFailedCompactionClearDoesNotStopTheSweep: the arms are independent, and
// a failure in one must not suppress the other's work or its reporting.
func TestAFailedCompactionClearDoesNotStopTheSweep(t *testing.T) {
	deletedCalled := false
	ms := &mockStore{}
	ms.deleteExpiredEventsFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		deletedCalled = true
		return 0, nil
	}
	ms.clearExpiredCompactionStateFn = func(ctx context.Context, olderThan time.Time) (int64, error) {
		return 0, errors.New("deadlock detected")
	}
	w := newTestWorker(ms)

	w.runRetentionSweep(30, 0, 0) // must not panic

	if !deletedCalled {
		t.Error("the event-deletion arm did not run; the two halves are independent")
	}
}
