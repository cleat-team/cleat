package engine

import (
	"context"
	"testing"
)

// cleat#4159. A defer segment's body must not continue as new: it recorded a continue_as_new event in the history of a workflow being
// terminated, and a guest that carried on after the call lost its cleanup (cmd/cleat-worker measures that on a real worker). This is the
// engine half, for BOTH calls, at the two places a segment's body can arrive: past the end of the history, and at the replay frontier.
//
//	control          an ordinary session records the continuation and sets the suspension, so the probe can see a continuation at all.
//	known positive   a segment past the frontier (not replaying) is refused.
//	subject          a segment AT the replay frontier is refused, with nothing recorded and no suspension set.
//	recorded         a segment whose history already HOLDS the continuation replays it: reading history is not new work.
func TestADeferSegmentRefusesContinueAsNew(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(s *execSession) int64{
		"ContinueAsNew": func(s *execSession) int64 { return s.ContinueAsNew(ctx, nil, `{"round":2}`) },
		"ContinueAsNewWithVersion": func(s *execSession) int64 {
			return s.ContinueAsNewWithVersion(ctx, nil, `{"round":2}`, 2)
		},
	}
	prior := []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "charge", Request: `{}`, Response: `{}`}}
	build := func(deferPhase, replaying bool, history []EventRecord) *execSession {
		s := newTestExecSession()
		s.engine.deferPhase = deferPhase
		s.isReplay = replaying
		s.history = append([]EventRecord(nil), history...)
		return s
	}
	consumePrior := func(s *execSession) { s.DurableCall(ctx, nil, "svc", "charge", `{}`, 0, 0) }

	for name, call := range calls {
		t.Run(name+"/control: an ordinary session records the continuation", func(t *testing.T) {
			s := build(false, false, nil)
			if got := call(s); got == callSuspendSentinel {
				t.Fatalf("an ordinary session was refused")
			}
			if s.suspendErr == nil || s.suspendErr.Reason != "continue_as_new" || len(s.history) != 1 || s.history[0].EventType != EventTypeContinueAsNew {
				t.Fatalf("the control did not record a continuation: suspend=%v history=%v, so this probe cannot see one", s.suspendErr, s.history)
			}
		})
		t.Run(name+"/known positive: a segment past the frontier is refused", func(t *testing.T) {
			s := build(true, false, nil)
			if got := call(s); got != callSuspendSentinel || len(s.history) != 0 || s.suspendErr != nil {
				t.Fatalf("got %#x, history %v, suspend %v; want the stop sentinel with nothing recorded", got, s.history, s.suspendErr)
			}
		})
		t.Run(name+"/a segment AT the replay frontier is refused", func(t *testing.T) {
			s := build(true, true, prior)
			consumePrior(s)
			if got := call(s); got != callSuspendSentinel {
				t.Errorf("got %#x, want the stop sentinel %#x", got, callSuspendSentinel)
			}
			if len(s.history) != len(prior) {
				t.Errorf("the refused call left %d event(s) beyond the replayed one: %v", len(s.history)-len(prior), s.history[len(prior):])
			}
			if s.suspendErr != nil {
				t.Errorf("the refused call set a suspension %v; the segment's own refusal is the only thing that should end it", s.suspendErr)
			}
		})
		t.Run(name+"/a recorded continuation is replayed in a segment", func(t *testing.T) {
			s := build(true, true, []EventRecord{{Step: 0, EventType: EventTypeContinueAsNew, NewInput: `{"round":2}`}})
			if got := call(s); got == callSuspendSentinel {
				t.Fatalf("a RECORDED continuation was refused: reading history is not new work")
			}
			if s.suspendErr == nil || s.suspendErr.Reason != "continue_as_new" {
				t.Errorf("replaying the recorded continuation did not restore its suspension: %v", s.suspendErr)
			}
		})
	}
}
