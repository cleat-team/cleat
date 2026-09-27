package engine

import (
	"context"
	"testing"
)

// TestADetachedRunDivergenceDoesNotConsume covers the last of the three
// distinct failure modes cleat#1507 enumerates: a guard that DETECTS a
// divergence and reports it uselessly.
//
// runDetached compared the record's type and name -- but only AFTER calling
// advanceReplayStep, so by the time it decided the record was foreign it had
// already consumed it. Every later step then read the wrong history entry, in
// a run that had also reported an error. Same corruption as cleat#1532's, from
// the other direction: there the type was never checked, here it was checked
// too late.
//
// It also returned a bare `1` with no metric and no message, so an operator saw
// a failed detached run with nothing distinguishing a replay divergence from a
// refused start.
//
// THE STEP IS THE LOAD-BEARING ASSERTION, as in cleat#1532. A version that
// reports loudly and still advances satisfies every claim about this call's own
// result and leaves the alignment damage in place.
func TestADetachedRunDivergenceDoesNotConsume(t *testing.T) {
	t.Run("a foreign record is refused without being consumed", func(t *testing.T) {
		s := newTestExecSession()
		s.isReplay = true
		s.history = []EventRecord{{
			Step:      0,
			EventType: EventTypeCall, // belongs to a durable call
		}}

		_, code := s.runDetached(context.Background(), nil, "worker", `{}`, 0, 0, false)

		if code == 0 {
			t.Error("reported success on a foreign record")
		}
		if s.stepCount != 0 {
			t.Errorf("stepCount = %d, want 0: the foreign record was CONSUMED before the "+
				"type was compared, so every later step reads the wrong entry. Reporting "+
				"the divergence does not undo that.", s.stepCount)
		}
		if !s.isReplay {
			t.Error("isReplay was cleared, so the fresh path below would start a second detached run")
		}
	})

	t.Run("a run_detached record with the WRONG NAME is also refused", func(t *testing.T) {
		// A detached run is identified by type AND name. Checking only the type
		// would pass this case while replaying the wrong workflow's id back to
		// the guest.
		s := newTestExecSession()
		s.isReplay = true
		s.history = []EventRecord{{
			Step:         0,
			EventType:    EventTypeRunDetached,
			DetachedName: "some-other-worker",
		}}

		_, code := s.runDetached(context.Background(), nil, "worker", `{}`, 0, 0, false)

		if code == 0 {
			t.Error("accepted a run_detached record for a DIFFERENT workflow name")
		}
		if s.stepCount != 0 {
			t.Errorf("stepCount = %d, want 0", s.stepCount)
		}
	})

	t.Run("its OWN record is still consumed and answered", func(t *testing.T) {
		// NEGATIVE CONTROL. Without it, the assertions above are equally
		// satisfied by a guard that refuses everything -- which would break
		// every correct replay of a detached run.
		s := newTestExecSession()
		s.isReplay = true
		s.history = []EventRecord{{
			Step:          0,
			EventType:     EventTypeRunDetached,
			DetachedName:  "worker",
			DetachedRunID: "run-42",
		}}

		_, code := s.runDetached(context.Background(), nil, "worker", `{}`, 0, 0, false)

		if code != 0 {
			t.Errorf("refused its OWN record (code=%d): the guard is rejecting correct replays", code)
		}
		if s.stepCount != 1 {
			t.Errorf("stepCount = %d, want 1: a matching record must be consumed", s.stepCount)
		}
	})

}
