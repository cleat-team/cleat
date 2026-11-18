//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// cleat#4094: ReReplay of a terminated run whose defer segment ran fails with a replay divergence, so it is refused up front.
//
// The known-positive is the shape measured on all three dialects (testdata/deferfunc: a registered defer, a sleep, then a call
// after it; terminated while sleeping, so a segment runs the defer and appends its events after the last recorded one). Three
// controls, because a refusal that fires on everything would pass the positive on its own:
//   - a FAILED run whose history holds defer-phase events (an ordinary failure's own drain) still redrives;
//   - a TERMINATED run with no defer-phase events still redrives;
//   - a run that never had a segment is not refused by the history scan at all (the same as the line above, from the failed side).
func TestReReplayRefusesATerminatedRunWhoseDeferSegmentAlreadyRan(t *testing.T) {
	ctx := context.Background()
	wasmBytes, eng1, _, _ := deferPhaseProbeEngine(t, "wf-4094", false)
	_, history, susp, _, _, err := eng1.Execute(ctx, wasmBytes, "defer_survives_suspension", json.RawMessage(`{}`))
	if err != nil || susp == nil {
		t.Fatalf("PRECONDITION FAILED: segment 1 err=%v suspended=%v", err, susp != nil)
	}

	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		reReplay := func(t *testing.T, id string) error {
			t.Helper()
			wf := mustGetWorkflow(t, ctx, store, id)
			return ReReplay(ctx, store, id, wf.Generation, "test")
		}
		status := func(t *testing.T, id string) string {
			t.Helper()
			return mustGetWorkflow(t, ctx, store, id).Status
		}

		t.Run("terminated with a segment is refused", func(t *testing.T) {
			id := startTerminableWorkflow(t, ctx, store, "rr-seg", false)
			if err := store.AppendEventHistoryBatch(ctx, id, history); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := store.TerminateWorkflow(ctx, id, "by test"); err != nil {
				t.Fatalf("terminate: %v", err)
			}
			claimed := claimByID(t, ctx, store, "worker-4094", id)
			loaded, _ := store.LoadEventHistory(ctx, id)
			_, rh, _, _, _, err := newDeferSegmentEngine(t, claimed.ID, &mockCaller{}).Replay(ctx, wasmBytes, "defer_survives_suspension", json.RawMessage(`{}`), loaded)
			if err != nil {
				t.Fatalf("segment: %v", err)
			}
			if err := store.(DeferPhaseStore).FinalizeDeferPhase(ctx, id, "worker-4094", claimed.Generation, rh[len(loaded):]); err != nil {
				t.Fatalf("finalize: %v", err)
			}
			if got := status(t, id); got != statusTerminated {
				t.Fatalf("PRECONDITION FAILED: status %q, want %q", got, statusTerminated)
			}
			err = reReplay(t, id)
			if !errors.Is(err, ErrAdminStateConflict) || err == nil || !strings.Contains(err.Error(), "defer segment has already run") {
				t.Fatalf("want an ErrAdminStateConflict naming the segment, got %v", err)
			}
			if got := status(t, id); got != statusTerminated {
				t.Fatalf("a refused re-replay changed the status to %q", got)
			}
		})

		t.Run("terminated with no defer-phase events still redrives", func(t *testing.T) {
			id := startTerminableWorkflow(t, ctx, store, "rr-plain", false)
			if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `{}`}}); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := store.TerminateWorkflow(ctx, id, "by test"); err != nil {
				t.Fatalf("terminate: %v", err)
			}
			if got := status(t, id); got != statusTerminated {
				t.Fatalf("PRECONDITION FAILED: status %q, want %q", got, statusTerminated)
			}
			if err := reReplay(t, id); err != nil {
				t.Fatalf("CONTROL FAILED: a terminated run with no segment was refused: %v", err)
			}
			if got := status(t, id); got != "ready" {
				t.Fatalf("status %q after the re-replay, want ready", got)
			}
		})

		t.Run("failed with a drain's defer-phase events still redrives", func(t *testing.T) {
			id := startTerminableWorkflow(t, ctx, store, "rr-failed", false)
			if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
				{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup"},
				{Step: 1, EventType: EventTypeCall, Service: "cleanup", Op: "after", Response: `{}`, InDeferPhase: true},
			}); err != nil {
				t.Fatalf("append: %v", err)
			}
			claimed := claimByID(t, ctx, store, "worker-4094f", id)
			if err := store.FailWorkflow(ctx, id, "worker-4094f", claimed.Generation, "boom", "", "", nil); err != nil {
				t.Fatalf("FailWorkflow: %v", err)
			}
			if got := status(t, id); got != "failed" {
				t.Fatalf("PRECONDITION FAILED: status %q, want failed", got)
			}
			if err := reReplay(t, id); err != nil {
				t.Fatalf("CONTROL FAILED: a failed run with a drain's defer-phase events was refused: %v", err)
			}
			if got := status(t, id); got != "ready" {
				t.Fatalf("status %q after the re-replay, want ready", got)
			}
		})

		// KNOWN OVER-REFUSAL, pinned so that a later fix (a marker that tells a segment from an ordinary drain) changes this on purpose.
		// A dead-lettered run terminated off the queue goes straight to terminated with no segment, and its history may hold the
		// ordinary failure's own drain, whose events carry InDeferPhase. The scan cannot tell it from a segment, so it refuses a
		// redrive that would in fact replay. Measured on all three dialects while writing cleat#4094.
		t.Run("dead-lettered then terminated with a drain's events is refused too (known over-refusal)", func(t *testing.T) {
			id := startTerminableWorkflow(t, ctx, store, "rr-dlq", false)
			if err := store.AppendEventHistoryBatch(ctx, id, []EventRecord{
				{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup"},
				{Step: 1, EventType: EventTypeCall, Service: "cleanup", Op: "after", Response: `{}`, InDeferPhase: true},
			}); err != nil {
				t.Fatalf("append: %v", err)
			}
			claimed := claimByID(t, ctx, store, "worker-4094d", id)
			if err := store.MoveToDeadLetterQueue(ctx, id, "worker-4094d", claimed.Generation, "boom", "", "", nil); err != nil {
				t.Fatalf("MoveToDeadLetterQueue: %v", err)
			}
			if err := store.TerminateWorkflow(ctx, id, "off the queue"); err != nil {
				t.Fatalf("terminate: %v", err)
			}
			if got := status(t, id); got != statusTerminated {
				t.Fatalf("PRECONDITION FAILED: status %q, want %q", got, statusTerminated)
			}
			if err := reReplay(t, id); !errors.Is(err, ErrAdminStateConflict) {
				t.Fatalf("the known over-refusal changed: want ErrAdminStateConflict, got %v. If a marker now tells a segment from a drain, "+
					"this case should redrive; update this test and the comment on refuseRedriveAfterADeferSegment together", err)
			}
		})
	})
}
