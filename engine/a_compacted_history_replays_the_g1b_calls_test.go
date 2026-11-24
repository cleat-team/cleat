package engine

// A history that has been COMPACTED and expanded again must replay the calls that read an identity from their record (cleat#4152, G1b).
//
// This test exists because the first version of signal_workflow's identity compared the record's target (RunID) with the call's, and
// compaction does not keep a signal send's RunID: extractCompactionStateFrom copies SignalName, SignalPayload, Service, Op and Response for
// a signal_received event and no RunID, so an expanded history carries it as "". The old replay never read the target, so it did not matter;
// the identity check did, and every replay of a compacted signal send would have reported a divergence. Nothing in the unit table could see
// it, because that table feeds the call a record in memory and never sends it through compaction (cleat#4177's shape: a test that hand-writes
// the producer's output cannot see the producer's transformations).
//
// Each case compacts a record exactly as the call writes it, expands it, and replays the same call against the result. The call must consume
// the record, answer what it answered, and reach the outside world not at all.

import (
	"context"
	"testing"
)

func TestACompactedHistoryReplaysTheCallsThatCheckAnIdentity(t *testing.T) {
	for _, gc := range g1bCalls {
		gc := gc
		t.Run(gc.name, func(t *testing.T) {
			own := g1bOwn(gc)
			cs, err := extractCompactionStateFrom(nil, own)
			if err != nil {
				t.Fatalf("compacting the record the call writes: %v", err)
			}
			expanded := buildFullHistoryFromCompaction(nil, cs)
			if len(expanded) != len(own) {
				t.Fatalf("PRECONDITION FAILED: %d events went into compaction and %d came out, so the replay below is not about the same history", len(own), len(expanded))
			}

			// The control: the uncompacted record replays. A failure here is not about compaction.
			w := newG1bWorld()
			s := g1bSession(w, own, true)
			if got := g1bObserve(gc, w, s); got != g1bGolden[gc.name]["replay"] {
				t.Fatalf("PRECONDITION FAILED: the UNcompacted record does not replay as the characterization says:\n  got:  %s\n  want: %s", got, g1bGolden[gc.name]["replay"])
			}

			// The measurement: the same record, after a round trip through compaction.
			w = newG1bWorld()
			s = g1bSession(w, expanded, true)
			ctx := context.Background()
			_ = ctx
			got := g1bObserve(gc, w, s)
			want := g1bGolden[gc.name]["replay"]
			if got != want {
				t.Errorf("a COMPACTED history did not replay the way the uncompacted one does:\n  uncompacted: %s\n  compacted:   %s", want, got)
			}
		})
	}
}

// TestASignalSendIdentityComparesTheNameAlwaysAndTheTargetWhenTheRecordHasOne pins each half of the identity separately. The golden row
// signal_workflow/replay_other_target_and_name changes BOTH, so it stays green if either comparison alone is lost; these cases change one.
func TestASignalSendIdentityComparesTheNameAlwaysAndTheTargetWhenTheRecordHasOne(t *testing.T) {
	signalCall := g1bCalls[0]
	if signalCall.name != "signal_workflow" {
		t.Fatalf("PRECONDITION FAILED: g1bCalls[0] is %q, not signal_workflow", signalCall.name)
	}
	for _, c := range []struct {
		name       string
		runID      string
		signalName string
		diverges   bool
	}{
		{"the same send", "target-run", "go", false},
		{"another target, the same name", "another-run", "go", true},
		{"the same target, another name", "target-run", "stop", true},
		{"a record with no target (compacted), the same name", "", "go", false},
		{"a record with no target (compacted), another name", "", "stop", true},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rec := g1bOwn(signalCall)
			rec[0].RunID, rec[0].SignalName = c.runID, c.signalName
			w := newG1bWorld()
			s := g1bSession(w, rec, true)
			got := g1bObserve(signalCall, w, s)
			diverged := s.stepCount == 0
			if diverged != c.diverges {
				t.Errorf("diverged=%v, want %v:\n  %s", diverged, c.diverges, got)
			}
		})
	}
}
