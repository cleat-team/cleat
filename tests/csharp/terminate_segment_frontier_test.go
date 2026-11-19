//go:build cgo

package csharp

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4143, through the real C# SDK, in the configuration a TERMINATE or CANCEL segment runs in.
//
// cmd/cleat-worker/setup.go arms WithStopAtLastStoppable only for a failed or dead-lettered segment, so a terminate's or cancel's is
// built from WithDeferPhase() alone. The existing test over this entry (TestCSharpDeferRegistrationIsTheStopPointAndItsBodyStillRuns)
// arms both and so cannot see the frontier: it stops the guest AT the registration. Here nothing stops it there, the segment replays
// [call, defer], and the guest's next call (svc/after) arrives at the REPLAY FRONTIER with the session still in replay.
//
// The run is the same killed one: call(before), defer, call(after), with the history cut after the registration. Two readings of that
// history, so the second is only meaningful beside the first:
//
//	resumed     an ordinary claim. svc/after must be made: this is the control, and shows the entry reaches that call.
//	terminated  a terminate's segment. svc/after must NOT be made, and the registered cleanup must run exactly once.
func TestACSharpTerminateSegmentDoesNotMakeTheNextCallAtTheReplayFrontier(t *testing.T) {
	fresh := runConformanceEntry(t, "defer_cleanup_after_kill", entryRun{WorkflowID: "wf-terminate-frontier"})
	mustFinish(t, fresh)
	var cut []engine.EventRecord
	for _, ev := range fresh.History {
		cut = append(cut, ev)
		if ev.EventType == engine.EventTypeDefer {
			break
		}
	}
	if len(cut) != 2 || cut[0].EventType != engine.EventTypeCall || cut[1].EventType != engine.EventTypeDefer {
		t.Fatalf("PRECONDITION FAILED: expected a history of [call, defer] to cut, got %d event(s): %v", len(cut), cut)
	}
	count := func(ops []string, want string) int {
		n := 0
		for _, op := range ops {
			if op == want {
				n++
			}
		}
		return n
	}

	t.Run("control: an ordinary resume makes svc/after", func(t *testing.T) {
		resumed := runConformanceEntry(t, "defer_cleanup_after_kill", entryRun{WorkflowID: "wf-terminate-frontier", Replay: cut})
		if resumed.Err != nil {
			t.Fatalf("the resumed run failed: %v", resumed.Err)
		}
		if ops := opsOf(resumed); count(ops, "svc/after") != 1 {
			t.Fatalf("the control did not make svc/after (calls = %v), so this entry cannot show the call being refused", ops)
		}
	})

	t.Run("a terminate's segment runs the cleanup and does not make svc/after", func(t *testing.T) {
		seg := runConformanceEntry(t, "defer_cleanup_after_kill", entryRun{
			WorkflowID: "wf-terminate-frontier",
			Replay:     cut,
			Options:    []engine.EngineOption{engine.WithDeferPhase()}, // NOT WithStopAtLastStoppable: that is failed/dead_lettered only
		})
		if seg.Err != nil {
			t.Fatalf("the defer segment failed: %v", seg.Err)
		}
		ops := opsOf(seg)
		if n := count(ops, "svc/after"); n != 0 {
			t.Errorf("a terminated workflow's defer segment MADE THE BODY'S NEXT CALL svc/after %d time(s): calls = %v.\n"+
				"It arrived at the replay frontier, where nothing refused it.", n, ops)
		}
		if n := count(ops, "svc/cleanup"); n != 1 {
			t.Errorf("the registered cleanup ran %d times, want exactly once: calls = %v", n, ops)
		}
	})
}
