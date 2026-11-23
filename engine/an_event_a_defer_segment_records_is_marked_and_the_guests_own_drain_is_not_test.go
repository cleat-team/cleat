//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// cleat#4079: InDeferSegment is HOST-observed, set on every event a defer segment records and never on the guest's own
// ordinary drain. The boundary is tested in both directions, because a marker that is set too widely refuses redrives that
// would replay, and one set too narrowly lets a diverging redrive through.

func TestInDeferSegmentIsStoredCheckedAndOnlyWrittenWhenTrue(t *testing.T) {
	plain := EventRecord{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `{}`, TimestampMs: 1000}
	marked := plain
	marked.InDeferSegment = true

	t.Run("an unmarked event's payload carries no segment key, so no old checksum changes", func(t *testing.T) {
		b, err := eventRecordToPayload(plain)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "in_defer_segment") {
			t.Fatalf("an unmarked event's payload carries the key, which would change every old event's checksum: %s", b)
		}
	})
	t.Run("a marked event's payload does, and its checksum differs", func(t *testing.T) {
		b, err := eventRecordToPayload(marked)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"in_defer_segment":true`) {
			t.Fatalf("a marked event's payload does not carry the marker: %s", b)
		}
		if computeEventChecksum(plain, "") == computeEventChecksum(marked, "") {
			t.Fatal("the marker is not covered by the checksum, so it could be stripped from stored history undetected")
		}
	})

	// Round trip through a real store: the marker survives, an unmarked neighbour stays unmarked, and the chain verifies.
	forEachBackend(t, func(t *testing.T, store WorkflowStore) {
		ctx := context.Background()
		id := startTerminableWorkflow(t, ctx, store, "marker-roundtrip", false)
		hist := []EventRecord{
			{Step: 0, EventType: EventTypeDefer, DeferID: "defer-0", DeferDescription: "cleanup"},
			{Step: 1, EventType: EventTypeCall, Service: "svc", Op: "a", Response: `{}`, InDeferPhase: true}, // a guest's own drain
			{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "b", Response: `{}`, InDeferSegment: true},
		}
		if err := store.AppendEventHistoryBatch(ctx, id, hist); err != nil {
			t.Fatalf("append: %v", err)
		}
		got, err := store.LoadEventHistory(ctx, id)
		if err != nil {
			t.Fatalf("load (which also verifies the checksum chain): %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("PRECONDITION FAILED: %d events loaded, want 3", len(got))
		}
		if got[0].InDeferSegment || got[1].InDeferSegment || !got[2].InDeferSegment {
			t.Fatalf("marker after the round trip: [%v %v %v], want [false false true]", got[0].InDeferSegment, got[1].InDeferSegment, got[2].InDeferSegment)
		}
		if !got[1].InDeferPhase || got[2].InDeferPhase {
			t.Fatalf("InDeferPhase after the round trip: [%v %v], want the drain event true and the segment event false: the two flags are independent", got[1].InDeferPhase, got[2].InDeferPhase)
		}
	})
}

// A segment marks what it records; the guest's own ordinary drain does not. Both directions, through the real engine and store.
func TestASegmentMarksItsEventsAndAnOrdinaryDrainDoesNot(t *testing.T) {
	ctx := context.Background()

	// Ordinary drain: the entry registers a defer and then fails, so the guest's own wrapper runs the defer on the way out.
	// The history has InDeferPhase events and must have NO InDeferSegment event.
	t.Run("the guest's own drain after an ordinary failure is not marked", func(t *testing.T) {
		wasmBytes, eng, _, _ := deferPhaseProbeEngine(t, "wf-marker-drain", false)
		_, hist, _, _, _, err := eng.Execute(ctx, wasmBytes, "defer_on_error", json.RawMessage(`{}`))
		if err == nil {
			t.Fatal("PRECONDITION FAILED: defer_on_error did not fail")
		}
		drained := 0
		for i, ev := range hist {
			if ev.InDeferSegment {
				t.Fatalf("event %d (%v %s.%s) is marked as a segment's, but this was an ordinary run's own drain", i, ev.EventType, ev.Service, ev.Op)
			}
			if ev.InDeferPhase {
				drained++
			}
		}
		if drained == 0 {
			t.Fatal("PRECONDITION FAILED: no InDeferPhase event, so the drain this case is about never happened and the assertion above is vacuous")
		}
	})

	// A terminate's segment: segment 1 suspends, terminate, claim, segment 2 runs the defer through the host.
	t.Run("every event a terminate's defer segment records is marked, and segment 1's are not", func(t *testing.T) {
		wasmBytes, eng1, _, _ := deferPhaseProbeEngine(t, "wf-marker-seg", false)
		_, history, susp, _, _, err := eng1.Execute(ctx, wasmBytes, "defer_survives_suspension", json.RawMessage(`{}`))
		if err != nil || susp == nil {
			t.Fatalf("PRECONDITION FAILED: segment 1 err=%v suspended=%v", err, susp != nil)
		}
		for i, ev := range history {
			if ev.InDeferSegment {
				t.Fatalf("segment 1's event %d is marked: the body's own events must not be", i)
			}
		}
		forEachBackend(t, func(t *testing.T, store WorkflowStore) {
			id := startTerminableWorkflow(t, ctx, store, "marker-seg", false)
			if err := store.AppendEventHistoryBatch(ctx, id, history); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := store.TerminateWorkflow(ctx, id, "by test"); err != nil {
				t.Fatalf("terminate: %v", err)
			}
			claimed := claimByID(t, ctx, store, "worker-marker", id)
			loaded, _ := store.LoadEventHistory(ctx, id)
			_, rh, _, _, _, err := newDeferSegmentEngine(t, claimed.ID, &mockCaller{}).Replay(ctx, wasmBytes, "defer_survives_suspension", json.RawMessage(`{}`), loaded)
			if err != nil {
				t.Fatalf("segment: %v", err)
			}
			newEvents := rh[len(loaded):]
			if len(newEvents) == 0 {
				t.Fatal("PRECONDITION FAILED: the segment recorded nothing, so nothing could be marked")
			}
			for i, ev := range newEvents {
				if !ev.InDeferSegment {
					t.Fatalf("segment event %d (%v %s.%s) is not marked", i, ev.EventType, ev.Service, ev.Op)
				}
			}
			if err := store.(DeferPhaseStore).FinalizeDeferPhase(ctx, id, "worker-marker", claimed.Generation, newEvents); err != nil {
				t.Fatalf("finalize: %v", err)
			}
			after, err := store.LoadEventHistory(ctx, id)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			marked := 0
			for i, ev := range after {
				if ev.InDeferSegment {
					marked++
				} else if i >= len(loaded) {
					t.Fatalf("event %d was written by the segment and read back unmarked", i)
				}
			}
			if marked != len(newEvents) {
				t.Fatalf("%d marked events persisted, want %d (exactly the segment's)", marked, len(newEvents))
			}
		})
	})
}
