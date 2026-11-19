//go:build cgo

package csharp

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#4079, through the real C# SDK: a run KILLED right after it registered a defer runs that defer's body in a defer segment.
//
// The registration is the killed run's last stoppable event, so the segment stops the guest AT it: the host records nothing new and
// answers the replayed durable-defer with err(suspended). The SDK has to have put the body on its table BEFORE asking the host, or
// the stop unwinds a guest whose table is empty and the cleanup the registration was for never runs. The body is the only thing here
// that makes the cleanup call, so its absence from the calls is the failure.
func TestCSharpDeferRegistrationIsTheStopPointAndItsBodyStillRuns(t *testing.T) {
	// A fresh run records: call(before), defer, call(after). The kill is simulated by cutting the history after the registration,
	// which is where a run killed there ends.
	fresh := runConformanceEntry(t, "defer_cleanup_after_kill", entryRun{WorkflowID: "wf-defer-kill"})
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

	seg := runConformanceEntry(t, "defer_cleanup_after_kill", entryRun{
		WorkflowID: "wf-defer-kill",
		Replay:     cut,
		Options:    []engine.EngineOption{engine.WithDeferPhase(), engine.WithStopAtLastStoppable()},
	})
	if seg.Err != nil {
		t.Fatalf("the defer segment failed: %v (a stop at a registration the SDK does not handle surfaces here as an uncaught exception)", seg.Err)
	}
	ops := opsOf(seg)
	cleanups := 0
	for _, op := range ops {
		switch op {
		case "svc/cleanup":
			cleanups++
		case "svc/after":
			t.Errorf("the guest ran on past the stop point and made svc/after: %v", ops)
		}
	}
	if cleanups != 1 {
		t.Fatalf("the defer body ran %d times in the segment, want exactly once; calls = %v", cleanups, ops)
	}
	var marked bool
	for _, ev := range seg.History {
		if ev.EventType == engine.EventTypeCall && ev.Op == "cleanup" {
			marked = ev.InDeferPhase
		}
	}
	if !marked {
		t.Errorf("the cleanup call is not marked as a defer body's (in_defer_phase)")
	}
}

// cleat#4194: a run that registered a defer and then SLEEPS is suspended, not finished. Its cleanup must not run at the suspension, and the
// resumed run must make the call after the sleep and then run the cleanup once. Before the fix the component path drained the defer table at every
// suspension, so the first run called svc/cleanup and the resume failed with a replay divergence at the step the cleanup had taken.
func TestCSharpDeferRegisteredBeforeASleepDoesNotRunAtTheSuspension(t *testing.T) {
	first := runConformanceEntry(t, "defer_then_sleep", entryRun{WorkflowID: "wf-defer-sleep"})
	if first.Err != nil || first.Suspended == nil {
		t.Fatalf("the first run should suspend in the sleep: err=%v suspended=%v result=%q", first.Err, first.Suspended != nil, first.Result)
	}
	if got := opsOf(first); len(got) != 1 || got[0] != "svc/before" {
		t.Fatalf("the first run made calls %v, want exactly [svc/before]: the cleanup ran while the workflow was only sleeping", got)
	}
	hist := moveRecordsBack(t, first.History, engine.EventTypeCall, 6_000)
	second := runConformanceEntry(t, "defer_then_sleep", entryRun{WorkflowID: "wf-defer-sleep", Replay: hist})
	if second.Err != nil || second.Suspended != nil {
		t.Fatalf("the resumed run did not finish: err=%v suspended=%v", second.Err, second.Suspended != nil)
	}
	if got := opsOf(second); len(got) != 2 || got[0] != "svc/after" || got[1] != "svc/cleanup" {
		t.Fatalf("the resumed run made calls %v, want [svc/after svc/cleanup]: before (replayed from history, not repeated), the cleanup last and once", got)
	}
}
