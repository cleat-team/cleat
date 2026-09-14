package engine

import (
	"context"
	"testing"
)

// TestAFireAndForgetReplayChecksWhatItConsumes covers the worst of the three
// failure modes cleat#1507 enumerates.
//
// DurableSend and DurableScheduleInvoke did not check the event type at all.
// They advanced past whatever record sat at the current step and returned
// SUCCESS. So a divergence was not merely unreported -- the record belonging to
// some OTHER operation was consumed, every subsequent step then read the wrong
// history entry, and the run reported success throughout.
//
// That is worse than the silent re-execution cleat#1506 fixed for the child
// spawn. There the damage was one duplicated child; here it propagates to
// every step after it.
//
// THE LOAD-BEARING ASSERTION IS stepCount, NOT THE ERROR CODE. A version that
// returned an error while still advancing the step would satisfy "it reported a
// divergence" and leave the alignment damage in place -- which is the half that
// makes this failure mode distinctive. Both are checked, and the step is the
// one that would be missed.
func TestAFireAndForgetReplayChecksWhatItConsumes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		call  func(s *execSession) int64
		right EventType
	}{
		{
			name:  "DurableSend",
			right: EventTypeDurableSend,
			call: func(s *execSession) int64 {
				return s.DurableSend(context.Background(), nil, "svc", "op", `{}`)
			},
		},
		{
			name:  "DurableScheduleInvoke",
			right: EventTypeDurableScheduleInvoke,
			call: func(s *execSession) int64 {
				return s.DurableScheduleInvoke(context.Background(), nil, "svc", "op", `{}`, 1000)
			},
		},
	} {
		t.Run(tc.name+" refuses a foreign record", func(t *testing.T) {
			s := newTestExecSession()
			s.isReplay = true
			s.history = []EventRecord{{
				Step:      0,
				EventType: EventTypeCall, // belongs to a durable call, not to this
			}}

			result := tc.call(s)

			if errCode := uint32(result & 0xFFFFFFFF); errCode == 0 {
				t.Errorf("reported SUCCESS on a foreign record; a caller cannot tell "+
					"this run from one that replayed correctly (errCode=%d)", errCode)
			}
			// THE POINT. Consuming the record is what misaligns every later
			// step, and it is invisible to any assertion about this call alone.
			if s.stepCount != 0 {
				t.Errorf("stepCount = %d, want 0: the foreign record was CONSUMED, so every "+
					"subsequent step now reads the wrong history entry. Reporting the "+
					"divergence without leaving the step alone fixes the message and "+
					"keeps the damage.", s.stepCount)
			}
			if !s.isReplay {
				t.Error("isReplay was cleared, so the fresh-execution path below the guard " +
					"would dispatch this send for real, on top of the divergence")
			}
		})

		t.Run(tc.name+" still consumes its own record", func(t *testing.T) {
			// NEGATIVE CONTROL. Without this, the assertions above are equally
			// satisfied by a guard that refuses everything -- which would break
			// every correct replay while passing the test.
			s := newTestExecSession()
			s.isReplay = true
			s.history = []EventRecord{{Step: 0, EventType: tc.right}}

			if result := tc.call(s); uint32(result&0xFFFFFFFF) != 0 {
				t.Errorf("refused its OWN event type (errCode=%d); the guard is rejecting "+
					"correct replays", uint32(result&0xFFFFFFFF))
			}
			if s.stepCount != 1 {
				t.Errorf("stepCount = %d, want 1: a matching record must be consumed", s.stepCount)
			}
		})
	}
}
