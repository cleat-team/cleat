//go:build cgo

package csharp

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4079 part 3, through the real C# SDK: a promise SETTLEMENT is a stoppable event, and in a defer segment that stops at it the host
// answers resolve_promise / reject_promise with the stop on bit 31 of the packed result they already return (no WIT change; the event
// carries Service "promise" and Op "resolve" or "reject"). The SDK has to read that bit BEFORE the status byte and unwind the guest, so
// the defer body still runs and nothing after the settlement does. python-sdk's resolve_then_spin / reject_then_spin are the models.
//
// Separate top-level functions on purpose, as in the other groups: a t.Run loop skips as subtests while its parent reports PASS.

// settleStopSegment runs b_settle_then_call fresh, cuts its history right after the settlement, and replays the cut as a defer segment
// that stops at its last stoppable event. It returns the segment and the store the segment used.
func settleStopSegment(t *testing.T, how string, typ engine.EventType, op string) (conformanceResult, *fakePromiseStore) {
	t.Helper()
	input := `{"how":"` + how + `"}`
	id := "wf-settle-" + how
	fresh := runConformanceEntry(t, "b_settle_then_call", entryRun{WorkflowID: id, Input: input,
		Options: []engine.EngineOption{engine.WithPromiseStore(&fakePromiseStore{})}})
	mustFinish(t, fresh)

	var cut []engine.EventRecord
	found := false
	for _, ev := range fresh.History {
		cut = append(cut, ev)
		if ev.EventType == typ && ev.Service == engine.PromiseEventService && ev.Op == op {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("PRECONDITION FAILED: no %s event with Service %q and Op %q in the fresh history: %v", typ, engine.PromiseEventService, op, fresh.History)
	}
	if len(cut) >= len(fresh.History) {
		t.Fatalf("PRECONDITION FAILED: nothing was recorded after the settlement, so the cut kills nothing (%d of %d events)", len(cut), len(fresh.History))
	}

	store := &fakePromiseStore{}
	seg := runConformanceEntry(t, "b_settle_then_call", entryRun{
		WorkflowID: id,
		Input:      input,
		Replay:     cut,
		Options:    []engine.EngineOption{engine.WithPromiseStore(store), engine.WithDeferPhase(), engine.WithStopAtLastStoppable()},
	})
	return seg, store
}

func checkSettleStopSegment(t *testing.T, seg conformanceResult, store *fakePromiseStore) {
	t.Helper()
	if seg.Err != nil {
		t.Fatalf("the defer segment failed: %v (a stop on the settlement that the SDK does not unwind surfaces here)", seg.Err)
	}
	// The defer body names what the guest had done in the operation of its call. "saw-charged" means the guest was unwound AT the
	// settlement; "saw-charged-settled" means it ran past it and only a later call stopped it, which is the stop being ignored.
	var saw []string
	for _, op := range opsOf(seg) {
		switch op {
		case "svc/after":
			t.Errorf("the guest ran on past the settlement and made svc/after: %v", opsOf(seg))
		default:
			if len(op) >= 8 && op[:8] == "svc/saw-" {
				saw = append(saw, op)
			}
		}
	}
	if len(saw) != 1 {
		t.Fatalf("the defer body ran %d times in the segment, want exactly once; calls = %v", len(saw), opsOf(seg))
	}
	if saw[0] != "svc/saw-charged" {
		t.Errorf("the defer body saw %q, want svc/saw-charged: the guest ran past the settlement before it was unwound", saw[0])
	}
	if len(store.resolved) != 0 || len(store.rejected) != 0 {
		t.Errorf("the segment settled the promise again (resolved=%v rejected=%v): a stop must refuse the settlement, not repeat it", store.resolved, store.rejected)
	}
	marked := false
	for _, ev := range seg.History {
		if ev.EventType == engine.EventTypeCall && len(ev.Op) >= 4 && ev.Op[:4] == "saw-" {
			marked = ev.InDeferPhase
		}
	}
	if !marked {
		t.Errorf("the cleanup call is not marked as a defer body's (in_defer_phase)")
	}
}

func TestCSharpResolvePromiseStopInADeferSegmentUnwindsAndTheDeferBodyStillRuns(t *testing.T) {
	seg, store := settleStopSegment(t, "resolve", engine.EventTypePromiseResolved, engine.PromiseOpResolve)
	checkSettleStopSegment(t, seg, store)
}

func TestCSharpRejectPromiseStopInADeferSegmentUnwindsAndTheDeferBodyStillRuns(t *testing.T) {
	seg, store := settleStopSegment(t, "reject", engine.EventTypePromiseRejected, engine.PromiseOpReject)
	checkSettleStopSegment(t, seg, store)
}
