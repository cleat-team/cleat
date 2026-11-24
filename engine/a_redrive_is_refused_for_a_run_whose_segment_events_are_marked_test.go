//go:build cgo

package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// cleat#4079: with the in_defer_segment marker, RetryWorkflow and ReReplay refuse exactly the runs whose history holds a defer
// segment's events, and nothing else. Per dialect, with the shapes that must still redrive as controls.

// runThatRanASegment ends a run in outcome through the two-phase failure path: marked, claimed again, and finalized with the events
// a segment records, which carry the marker. It is the history shape a killed component run has after cleat#4079.
func runThatRanASegment(t *testing.T, ctx context.Context, store WorkflowStore, name, outcome string) string {
	t.Helper()
	dps := store.(DeferPhaseStore)
	marker := store.(DeferPhaseFailureMarker)
	id := startKeyedRun(t, ctx, store, name)
	c1 := claimByID(t, ctx, store, "worker-"+name+"-1", id)
	if err := marker.MarkFailureForDeferPhase(ctx, id, "worker-"+name+"-1", c1.Generation,
		DeferPhaseFailure{Status: outcome, ErrorMsg: "execution time limit exceeded", ErrorCode: "timeout"}, nil); err != nil {
		t.Fatalf("MarkFailureForDeferPhase: %v", err)
	}
	c2 := claimByID(t, ctx, store, "worker-"+name+"-2", id)
	seg := []EventRecord{{Step: 2, EventType: EventTypeCall, Service: "cleanup", Op: "refund", Response: `{}`, InDeferPhase: true, InDeferSegment: true}}
	if err := dps.FinalizeDeferPhase(ctx, id, "worker-"+name+"-2", c2.Generation, seg); err != nil {
		t.Fatalf("FinalizeDeferPhase: %v", err)
	}
	if got := mustGetWorkflow(t, ctx, store, id).Status; got != outcome {
		t.Fatalf("PRECONDITION FAILED: status %q after the segment, want %q", got, outcome)
	}
	return id
}

func TestRedriveRefusesARunWhoseSegmentEventsAreMarked(t *testing.T) {
	ctx := context.Background()
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		reReplay := func(id string) error {
			return ReReplay(ctx, store, id, mustGetWorkflow(t, ctx, store, id).Generation, "test")
		}
		status := func(id string) string { return mustGetWorkflow(t, ctx, store, id).Status }

		t.Run("ReReplay refuses a FAILED run that ran a segment", func(t *testing.T) {
			id := runThatRanASegment(t, ctx, store, "rr-failed", statusFailed)
			err := reReplay(id)
			if !errors.Is(err, ErrAdminStateConflict) || err == nil || !strings.Contains(err.Error(), "ran a defer segment") {
				t.Fatalf("want an ErrAdminStateConflict naming the segment, got %v", err)
			}
			if got := status(id); got != statusFailed {
				t.Fatalf("a refused re-replay changed the status to %q", got)
			}
		})

		t.Run("RetryWorkflow refuses a DEAD-LETTERED run that ran a segment", func(t *testing.T) {
			id := runThatRanASegment(t, ctx, store, "retry-dlq", statusDeadLettered)
			err := RetryWorkflow(ctx, store, id)
			if !errors.Is(err, ErrAdminStateConflict) || err == nil || !strings.Contains(err.Error(), "ran a defer segment") {
				t.Fatalf("want an ErrAdminStateConflict naming the segment, got %v", err)
			}
			if got := status(id); got != statusDeadLettered {
				t.Fatalf("a refused retry changed the status to %q", got)
			}
		})

		t.Run("CONTROL: an ordinary dead-lettered run with its own drain still retries", func(t *testing.T) {
			id := startKeyedRun(t, ctx, store, "retry-ordinary")
			// The guest's own cleanup after an exhausted call: InDeferPhase, and NOT a segment's, so unmarked.
			if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
				{Step: 2, EventType: EventTypeCall, Service: "cleanup", Op: "refund", Response: `{}`, InDeferPhase: true},
			}); err != nil {
				t.Fatalf("append: %v", err)
			}
			c := claimByID(t, ctx, store, "worker-retry-ordinary", id)
			if err := store.MoveToDeadLetterQueue(ctx, id, "worker-retry-ordinary", c.Generation, "retries exhausted", "retries_exhausted", "", nil); err != nil {
				t.Fatalf("MoveToDeadLetterQueue: %v", err)
			}
			if got := status(id); got != statusDeadLettered {
				t.Fatalf("PRECONDITION FAILED: status %q, want dead_lettered", got)
			}
			if err := RetryWorkflow(ctx, store, id); err != nil {
				t.Fatalf("CONTROL FAILED: an ordinary dead-lettered run with a drain was refused: %v", err)
			}
			if got := status(id); got != "ready" {
				t.Fatalf("status %q after the retry, want ready", got)
			}
		})

		t.Run("CONTROL: a dead-lettered run with no defers still retries", func(t *testing.T) {
			id := startTerminableWorkflow(t, ctx, store, "retry-plain", false)
			c := claimByID(t, ctx, store, "worker-retry-plain", id)
			if err := store.MoveToDeadLetterQueue(ctx, id, "worker-retry-plain", c.Generation, "boom", "unknown", "", nil); err != nil {
				t.Fatalf("MoveToDeadLetterQueue: %v", err)
			}
			if err := RetryWorkflow(ctx, store, id); err != nil {
				t.Fatalf("CONTROL FAILED: %v", err)
			}
			if got := status(id); got != "ready" {
				t.Fatalf("status %q after the retry, want ready", got)
			}
		})

		t.Run("CONTROL: a failed run with an unmarked drain still re-replays", func(t *testing.T) {
			id := startKeyedRun(t, ctx, store, "rr-ordinary")
			if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
				{Step: 2, EventType: EventTypeCall, Service: "cleanup", Op: "refund", Response: `{}`, InDeferPhase: true},
			}); err != nil {
				t.Fatalf("append: %v", err)
			}
			c := claimByID(t, ctx, store, "worker-rr-ordinary", id)
			if err := store.FailWorkflow(ctx, id, "worker-rr-ordinary", c.Generation, "boom", "", "", nil); err != nil {
				t.Fatalf("FailWorkflow: %v", err)
			}
			if err := reReplay(id); err != nil {
				t.Fatalf("CONTROL FAILED: a failed run whose only defer-phase events are its own drain was refused: %v", err)
			}
			if got := status(id); got != "ready" {
				t.Fatalf("status %q after the re-replay, want ready", got)
			}
		})

		t.Run("RetryWorkflow on a non-dead-lettered run stays the silent no-op it was", func(t *testing.T) {
			id := runThatRanASegment(t, ctx, store, "retry-failed", statusFailed)
			if err := RetryWorkflow(ctx, store, id); err != nil {
				t.Fatalf("a retry of a failed run must not start reporting segments: %v", err)
			}
			if got := status(id); got != statusFailed {
				t.Fatalf("status %q, want failed unchanged", got)
			}
		})
	})
}
