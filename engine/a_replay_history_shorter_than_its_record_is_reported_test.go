package engine

import "testing"

// The tail case cleat#1507 left unreported, and the three ways a correct
// implementation must stay quiet.
//
// validateReplayStepDensity walks the history asserting history[i].Step == i,
// so it catches a hole and cannot catch a missing tail: [0,1,2] with step 3
// gone satisfies the predicate at every index, because there is no index at
// which to disagree. These cases pin the comparison that can.
func TestAReplayHistoryShorterThanItsRecordIsReported(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		recorded, loaded, truncated int
		wantShort                   bool
		wantMissing                 int
		why                         string
	}{
		{
			name:     "a missing tail is reported",
			recorded: 4, loaded: 3, truncated: 0,
			wantShort: true, wantMissing: 1,
			why: "the case the issue is about: step 3 deleted after commit",
		},
		{
			name:     "a crashed segment is not reported",
			recorded: 0, loaded: 5, truncated: 0,
			wantShort: false,
			why: "the per-step flush persists rows and does not count them; finalize " +
				"counts the segment when it ends. A worker that dies mid-segment leaves " +
				"rows event_count never counted -- measured at 5 rows, event_count 0. " +
				"This is the ordinary state of the case replay exists to recover, so an " +
				"equality check would fire on every recovery it is meant to protect",
		},
		{
			name:     "a truncated compaction is not reported",
			recorded: 100, loaded: 40, truncated: 60,
			wantShort: false,
			why: "extractCompactionState drops the oldest compacted events to bound the " +
				"JSONB and records the count; the reconstructed history is shorter by " +
				"design. Without the correction term this fires on every workflow long " +
				"enough to be truncated -- the ones most likely to be replayed",
		},
		{
			name:     "a truncated compaction that is ALSO short is reported",
			recorded: 100, loaded: 39, truncated: 60,
			wantShort: true, wantMissing: 1,
			why: "the correction term must not become a blanket exemption for compacted " +
				"workflows: subtracting exactly what compaction dropped still leaves the " +
				"tail case visible",
		},
		{
			name:     "an unknown count says nothing",
			recorded: 0, loaded: 0, truncated: 0,
			wantShort: false,
			why: "0 is what a failed or unattempted GetEventCount leaves behind, and a " +
				"check that cannot tell 'no events' from 'I did not manage to ask' must " +
				"not speak",
		},
		{
			name:     "a longer history is not reported",
			recorded: 3, loaded: 5, truncated: 0,
			wantShort: false,
			why:       "recorded < loaded is the finalize-not-yet-run direction, not loss",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			short, missing := reportShortReplayHistory(tc.recorded, tc.loaded, tc.truncated)
			if short != tc.wantShort {
				t.Errorf("reportShortReplayHistory(%d, %d, %d) short = %v, want %v\n%s",
					tc.recorded, tc.loaded, tc.truncated, short, tc.wantShort, tc.why)
			}
			if short && missing != tc.wantMissing {
				t.Errorf("reportShortReplayHistory(%d, %d, %d) missing = %d, want %d",
					tc.recorded, tc.loaded, tc.truncated, missing, tc.wantMissing)
			}
		})
	}
}

// truncatedCompactedEvents must survive both nils, because the common case --
// a workflow that has never been compacted -- reaches it with a nil state, and
// a compacted-but-not-truncated one reaches it with a nil summary.
func TestTruncatedCompactedEventsHandlesBothAbsences(t *testing.T) {
	if got := truncatedCompactedEvents(nil); got != 0 {
		t.Errorf("nil compaction state: got %d, want 0", got)
	}
	if got := truncatedCompactedEvents(&CompactionState{}); got != 0 {
		t.Errorf("nil summary: got %d, want 0", got)
	}
	if got := truncatedCompactedEvents(&CompactionState{
		Summary: &TruncationSummary{TruncatedCount: 7},
	}); got != 7 {
		t.Errorf("summary with 7 truncated: got %d, want 7", got)
	}
}
