package engine

// cleat#4325. awaitAllChildrenStep compares the record's Request (the run IDs awaited) with the call's, and compaction did not carry Request for await_all_children, so a record
// that went through compaction came back with Request "" and the comparison failed every replay of it. Measured on develop 8e992d22 through the real step: the uncompacted record
// replays (result 0x0, the step consumed) and the same record after compaction and expansion diverges (result 0x1, nothing consumed). The check came with the move of the child
// awaits onto the dispatcher; v0.5.0 compared no request, so it was never released.
//
// The fix is two things and each has a row that fails without it: compaction carries the request, and the identity compares it only when the record HAS one (the tolerance
// signal_workflow and await_any_child already have), because a state compacted before this fix has lost it and cannot get it back.

import (
	"context"
	"testing"
)

func TestACompactedAwaitAllChildrenRecordReplays(t *testing.T) {
	const ids = `["child-a","child-b"]`
	const results = `[{"run_id":"child-a","result":"ok"},{"run_id":"child-b","result":"ok"}]`
	rec := EventRecord{Step: 0, EventType: EventTypeAwaitAllChildren, Request: ids, Response: results}

	// replay runs the call against a one-record history and says whether the record was consumed and what the guest was told.
	replay := func(callIDs string, hist []EventRecord) (consumed bool, result int64) {
		s := stepSession(nil, hist)
		result = s.step(context.Background(), nil, &awaitAllChildrenStep, stepArgs{s1: callIDs})
		return s.stepCount == 1, result
	}
	compactAndExpand := func(t *testing.T, mutate func(cs *CompactionState)) []EventRecord {
		t.Helper()
		cs, err := extractCompactionStateFrom(nil, []EventRecord{rec})
		if err != nil {
			t.Fatalf("compacting the record the call writes: %v", err)
		}
		if mutate != nil {
			mutate(cs)
		}
		out := buildFullHistoryFromCompaction(nil, cs)
		if len(out) != 1 {
			t.Fatalf("PRECONDITION FAILED: %d events went into compaction and %d came out", 1, len(out))
		}
		return out
	}

	// CONTROL: the uncompacted record replays. A failure here is not about compaction.
	if consumed, res := replay(ids, []EventRecord{rec}); !consumed || res != 0 {
		t.Fatalf("PRECONDITION FAILED: the UNcompacted record does not replay (consumed=%v result=%#x)", consumed, res)
	}
	// CONTROL: the identity check exists: an uncompacted record with another request is still a divergence.
	if consumed, res := replay(`["child-x"]`, []EventRecord{rec}); consumed || res&0xFF == 0 {
		t.Errorf("CONTROL: an uncompacted record replayed against another request (consumed=%v result=%#x), so a pass below could mean the check is gone", consumed, res)
	}

	t.Run("the request survives the round trip", func(t *testing.T) {
		out := compactAndExpand(t, nil)
		if out[0].Request != ids {
			t.Fatalf("the request came back as %q, want %q", out[0].Request, ids)
		}
		if d := diffEventFields(rec, out[0]); len(d) != 0 {
			t.Errorf("fields that did not survive: %v", d)
		}
	})
	t.Run("a compacted record replays, as the uncompacted one does", func(t *testing.T) {
		if consumed, res := replay(ids, compactAndExpand(t, nil)); !consumed || res != 0 {
			t.Errorf("a COMPACTED await_all_children record did not replay: consumed=%v result=%#x (the uncompacted record replays 0x0 and consumes it)", consumed, res)
		}
	})
	t.Run("a compacted record with ANOTHER request is still a divergence", func(t *testing.T) {
		if consumed, res := replay(`["child-x"]`, compactAndExpand(t, nil)); consumed || res&0xFF == 0 {
			t.Errorf("a compacted record replayed against another request (consumed=%v result=%#x): carrying the request must not have removed the check", consumed, res)
		}
	})
	t.Run("a record compacted BEFORE this fix has no request and still replays", func(t *testing.T) {
		// A state written by a build that did not carry it: the request is gone and cannot be recovered, so the identity must not fail on its absence.
		old := compactAndExpand(t, func(cs *CompactionState) {
			for i := range cs.Events {
				cs.Events[i].Request = ""
			}
		})
		if old[0].Request != "" {
			t.Fatalf("PRECONDITION FAILED: the simulated old state still carries a request (%q)", old[0].Request)
		}
		if consumed, res := replay(ids, old); !consumed || res != 0 {
			t.Errorf("an await_all_children record with no request (a state compacted before the fix) did not replay: consumed=%v result=%#x", consumed, res)
		}
	})
}
