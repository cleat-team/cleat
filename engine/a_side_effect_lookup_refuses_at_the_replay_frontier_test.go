package engine

// cleat#4143, the half the first fix did not cover. A defer segment runs the defers and nothing else, so the body's new work is refused. A
// side effect has TWO host calls: the lookup (SideEffectRecorded: "does the history hold the value for this step?") and the record
// (SideEffect). An SDK that asks first runs the USER'S FUNCTION between them, and that function may call an API.
//
// At the replay frontier the lookup answered "not recorded" with no stop check, because the record after it makes the switch to fresh and
// refuses. So the sequence in a terminated workflow's segment was: lookup says not recorded, THE FUNCTION RUNS, the record is refused. The
// refusal sat after the thing it was meant to prevent. cleat-review2 measured it through the real C# SDK: the defer body saw `saw-ran-1`
// (the function ran once) in a segment over a history cut at the frontier, and `saw-ran-0` only with WithStopAtLastStoppable.
//
// FIVE SESSIONS, because each one rules out a way for this to pass without testing anything:
//
//	control          an ordinary session at the frontier. The lookup answers "not recorded" and the function runs once: shows the probe sees
//	                 the function run.
//	known positive   a segment PAST the frontier (the non-replay half). Refused already (#4119): shows the probe sees a refusal.
//	subject          a segment AT the frontier. Refused, the function runs 0 times, nothing recorded.
//	defer body       the same segment draining the guest's own defers. NOT refused: a cleanup's side effect must be able to run.
//	recorded         a segment whose history HOLDS the value. The lookup still answers with it: the refusal is for new work only.

import (
	"context"
	"testing"
)

// askFirst is what an SDK that asks first does: look up, and compute and record only when nothing is recorded. It returns how many times the
// user's function ran and the lookup's answer.
func askFirst(ctx context.Context, s *execSession, buf []byte) (ran int, lookup int64) {
	lookup = s.SideEffectRecorded(ctx, nil, 0, uint32(len(buf)))
	if lookup == callSuspendSentinel {
		return 0, lookup
	}
	if recordedStatus(lookup) == sideEffectNotRecorded {
		ran++ // the user's function, which may call an API
		s.SideEffect(ctx, nil, `"computed"`, 0, uint32(len(buf)))
	}
	return ran, lookup
}

func TestASideEffectLookupRefusesAtTheReplayFrontierOfADeferSegment(t *testing.T) {
	buf := make([]byte, 256)
	ctx := ctxWithMem(context.Background(), buf)
	// One recorded call ahead of the lookup, so a session over it starts in replay and the lookup arrives at the frontier.
	prior := []EventRecord{{Step: 0, EventType: EventTypeCall, Service: "svc", Op: "charge", Request: `{}`, Response: `{}`}}
	consumePrior := func(s *execSession) {
		s.DurableCall(ctx, nil, "svc", "charge", `{}`, 0, uint32(len(buf)))
	}
	build := func(deferPhase, replaying, draining bool, history []EventRecord) *execSession {
		s := newTestExecSession()
		s.engine.deferPhase = deferPhase
		s.isReplay = replaying
		s.inDeferDrain = draining
		s.history = append([]EventRecord(nil), history...)
		return s
	}

	t.Run("control: an ordinary session at the frontier runs the function once", func(t *testing.T) {
		s := build(false, true, false, prior)
		consumePrior(s)
		ran, lookup := askFirst(ctx, s, buf)
		if lookup == callSuspendSentinel || ran != 1 {
			t.Fatalf("the control ran the function %d time(s) and was refused=%v; want 1 and not refused, so this probe cannot see the function run", ran, lookup == callSuspendSentinel)
		}
	})

	t.Run("known positive: a segment past the frontier is refused", func(t *testing.T) {
		s := build(true, false, false, nil)
		ran, lookup := askFirst(ctx, s, buf)
		if lookup != callSuspendSentinel || ran != 0 {
			t.Fatalf("a segment past the frontier ran the function %d time(s), lookup %#x; want 0 and the stop sentinel %#x: the probe cannot see the refusal it exists to find", ran, lookup, callSuspendSentinel)
		}
	})

	t.Run("a segment AT the replay frontier is refused, the function runs 0 times, nothing is recorded", func(t *testing.T) {
		s := build(true, true, false, prior)
		consumePrior(s)
		ran, lookup := askFirst(ctx, s, buf)
		if lookup != callSuspendSentinel || ran != 0 {
			t.Errorf("a defer segment AT the replay frontier ran the user's function %d time(s) (want 0); the lookup answered %#x (want the stop sentinel %#x).\n\n"+
				"The lookup answers \"not recorded\" at the frontier with no stop check, so an SDK that asks first runs its function before the record "+
				"after it is refused.", ran, lookup, callSuspendSentinel)
		}
		if len(s.history) != len(prior) {
			t.Errorf("the refused lookup left %d event(s) beyond the replayed one: %v", len(s.history)-len(prior), s.history[len(prior):])
		}
	})

	t.Run("a defer body at the frontier is not refused", func(t *testing.T) {
		s := build(true, true, true, prior)
		consumePrior(s)
		ran, lookup := askFirst(ctx, s, buf)
		if lookup == callSuspendSentinel || ran != 1 {
			t.Fatalf("a defer body's side effect (inDeferDrain) was refused (%#x) or did not run (ran %d): a cleanup has to be able to compute a value", lookup, ran)
		}
	})

	t.Run("a recorded value is still answered in a segment", func(t *testing.T) {
		recorded := []EventRecord{{Step: 0, EventType: EventTypeSideEffect, SideEffectResult: `"frozen"`}}
		s := build(true, true, false, recorded)
		ran, lookup := askFirst(ctx, s, buf)
		if lookup == callSuspendSentinel || recordedStatus(lookup) != 0 || ran != 0 {
			t.Fatalf("a recorded value was not answered in a segment: lookup %#x status %d, function ran %d time(s); the refusal is for NEW work, and reading a recorded value is not", lookup, recordedStatus(lookup), ran)
		}
		if got := extractStringFromSimplePacked(lookup, buf); got != `"frozen"` {
			t.Errorf("the lookup answered %q, want the recorded value", got)
		}
	})
}
