package main

import (
	"testing"

	"github.com/cleat-team/cleat/engine"
)

// cleat#1155: a workflow that exhausted its retries and then ran a defer was
// classified `failed` rather than `dead_lettered`, because the defer's own
// durable call was the last event and dead-lettering asks what the last durable
// act was.
//
// The cases below are written as histories rather than driven through a guest
// on purpose. What is under test is the CLASSIFICATION RULE, and a test that
// booted a worker would exercise a dozen layers that can each independently
// decide the answer -- which is the "watch which layer is holding the test up"
// trap. The end-to-end proof that the flag actually arrives from a real guest
// is the port fixture, not this file.
func TestADeferDoesNotMoveAWorkflowOutOfTheDeadLetterQueue(t *testing.T) {
	exhausted := engine.EventRecord{
		Step: 0, EventType: engine.EventTypeCall,
		Service: "flaky", Op: "op", RetriesExhausted: true,
	}
	deferCall := engine.EventRecord{
		Step: 1, EventType: engine.EventTypeCall,
		Service: "bench-svc", Op: "send", InDeferPhase: true,
	}
	bodyCall := engine.EventRecord{
		Step: 2, EventType: engine.EventTypeCall,
		Service: "billing", Op: "charge",
	}

	cases := []struct {
		name    string
		history []engine.EventRecord
		want    bool
		why     string
	}{
		{
			name:    "the defect: exhausted, then a defer that called the host",
			history: []engine.EventRecord{exhausted, deferCall},
			want:    true,
			why: "the defer is the workflow cleaning up, not carrying on. Before " +
				"cleat#1155 this was false and the run was deleted by retention",
		},
		{
			name:    "more than one defer body ran",
			history: []engine.EventRecord{exhausted, deferCall, deferCall},
			want:    true,
			why:     "a defer table with two entries is still cleanup",
		},
		{
			name:    "no defer at all, which must be unchanged",
			history: []engine.EventRecord{exhausted},
			want:    true,
			why:     "the case that already worked",
		},
		{
			name:    "the workflow carried on AFTER its defer",
			history: []engine.EventRecord{exhausted, deferCall, bodyCall},
			want:    false,
			why: "only TRAILING defer events are skipped. Body work after a defer " +
				"means the run went on, which is exactly what the position rule " +
				"exists to detect -- and skipping non-trailing defers would break it",
		},
		{
			name:    "recovered and carried on, no defer involved",
			history: []engine.EventRecord{exhausted, bodyCall},
			want:    false,
			why:     "the rule's original purpose, unchanged",
		},
		{
			name:    "the last body act was not an exhaustion",
			history: []engine.EventRecord{bodyCall, deferCall},
			want:    false,
			why:     "a defer does not manufacture an exhaustion that never happened",
		},
		{
			name:    "every event was a defer",
			history: []engine.EventRecord{deferCall},
			want:    false,
			why:     "there is no body act to judge, and cleanup alone is not an exhaustion",
		},
		{
			name:    "empty history",
			history: nil,
			want:    false,
			why:     "unchanged",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := endedOnAnExhaustedCall(tc.history)
			if got != tc.want {
				t.Errorf("endedOnAnExhaustedCall = %v, want %v\n  %s", got, tc.want, tc.why)
			}
		})
	}
}

// A history written before cleat#1155 carries no InDeferPhase flags at all,
// because nothing set them. Such a workflow must classify exactly as it did
// before -- a workflow in flight across the upgrade does not change behaviour
// underneath itself.
//
// This is the same compatibility RetriesExhausted itself relies on, and it is
// asserted rather than assumed because "the new field defaults to false" is a
// claim about every reader of the history, not just this one.
func TestAHistoryWrittenBeforeTheFlagExistedClassifiesAsItAlwaysDid(t *testing.T) {
	old := []engine.EventRecord{
		{Step: 0, EventType: engine.EventTypeCall, Service: "flaky", Op: "op", RetriesExhausted: true},
		// The defer's call, as it would have been recorded before the flag
		// existed: indistinguishable from a body call, which is the defect.
		{Step: 1, EventType: engine.EventTypeCall, Service: "bench-svc", Op: "send"},
	}
	if endedOnAnExhaustedCall(old) {
		t.Error("an unflagged trailing event was treated as a defer: a history " +
			"written before cleat#1155 must classify exactly as it did then, " +
			"and this one classified as `failed`")
	}
}
