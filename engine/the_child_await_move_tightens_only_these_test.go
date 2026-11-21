package engine

// W3a of cleat#4152: the rows that moving AwaitChild and AwaitAllChildren onto the step dispatcher deliberately CHANGES, each stating what the old code did.
// Everything else the move touched is in the_child_awaits_behave_as_recorded_before_the_dispatcher_test.go, whose rows pass on the old code and the moved code
// alike. These do not pass on the old code: each was run against it first (the old code is the develop that table was recorded from), and the failing
// message is what the "OLD" comment on the row says.
//
// THE RULE these follow: a migration never loosens a check the old code had, and identity rows only TIGHTEN. All of them are the same one change, applied
// to the places the old code did not look: a recorded record that names ANOTHER call is a divergence, and a divergence consumes NOTHING (cleat#4157).
// They use the harness of the table (runChildAwaitRow), so the result word, the recorded events, the step count and the store reads are all asserted.
//
// Two rows are here that hold on BOTH codes, and are kept by name because they are the other side of the tightening: a record that names NO child
// cannot name another, so it is still this call's.

import "testing"

func TestTheChildAwaitMoveTightensOnlyThese(t *testing.T) {
	const (
		two = `["c1","c2"]`
	)
	done := func(res string) ChildOutcome { return ChildOutcome{Completed: true, Result: res} }
	anotherChild := func(resp string) EventRecord {
		return EventRecord{EventType: EventTypeAwaitChild, RunID: "c2", Response: resp}
	}
	foreign := EventRecord{EventType: EventTypeSideEffect, Response: "x"}
	rows := []childAwaitRow{
		// ---- await_child ----
		{name: "OLD consumed it and answered with another child's result: an await_child ANSWER naming another child is a divergence, and consumes nothing",
			call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{anotherChild("R of c2")},
			want: childAwaitWant{errCode: 1, resultHas: `child "c1", recorded "c2"`, step: 0, replaying: true}},
		{name: "OLD ignored the run id of a pending record, re-checked THIS call's child and recorded its answer over another call's pending record: a pending await_child naming another child is a divergence",
			call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{anotherChild("")}, outcomes: map[string]ChildOutcome{"c1": done("R")},
			want: childAwaitWant{errCode: 1, resultHas: `child "c1", recorded "c2"`, step: 0, replaying: true}},
		{name: "UNCHANGED (holds on both): an await_child answer that names NO child is still this call's",
			call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{{EventType: EventTypeAwaitChild, Response: "R"}},
			want: childAwaitWant{result: "R", step: 1, replaying: true}},
		{name: "UNCHANGED (holds on both): a pending await_child that names NO child is still this call's, and is answered over",
			call: callOfAwaitChild, ids: []string{"c1"}, hist: []EventRecord{{EventType: EventTypeAwaitChild}}, outcomes: map[string]ChildOutcome{"c1": done("R")},
			want: childAwaitWant{result: "R", recs: []childAwaitRec{{typ: EventTypeAwaitChild, resp: "R", runID: "c1"}}, step: 1, gets: 1}},

		// ---- await_all_children ----
		{name: "OLD consumed the record first (step 1) and then diverged: an await_all answer for other run ids is a divergence that consumes NOTHING",
			call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, hist: []EventRecord{allRecForTightening(`["c9"]`, `[{"run_id":"c9","result":"x"}]`)},
			want: childAwaitWant{errCode: 1, resultHas: `run IDs ["c1","c2"], recorded ["c9"]`, step: 0, replaying: true}},
		{name: "OLD consumed the record first (step 1) and then diverged: the same ids in another ORDER are another request, and the divergence consumes NOTHING",
			call: callOfAwaitAllChildren, ids: []string{"c2", "c1"}, hist: []EventRecord{allRecForTightening(two, `[{"run_id":"c1","result":"a"},{"run_id":"c2","result":"b"}]`)},
			want: childAwaitWant{errCode: 1, resultHas: `run IDs ["c2","c1"], recorded ["c1","c2"]`, step: 0, replaying: true}},
		{name: "OLD consumed the record first (step 1) and then diverged: a record of another type is a divergence that consumes NOTHING",
			call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, hist: []EventRecord{foreign},
			want: childAwaitWant{errCode: 1, resultHas: "replay divergence at step 0: expected await_all_children, got side_effect", step: 0, replaying: true}},
		{name: "OLD ignored the request of a pending record and recorded THIS call's answer over it: a pending await_all naming other run ids is a divergence",
			call: callOfAwaitAllChildren, ids: []string{"c1", "c2"}, hist: []EventRecord{allRecForTightening(`["c9"]`, "")},
			outcomes: map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			want:     childAwaitWant{errCode: 1, resultHas: `run IDs ["c1","c2"], recorded ["c9"]`, step: 0, replaying: true}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { runChildAwaitRow(t, r) })
	}
}

func allRecForTightening(req, resp string) EventRecord {
	return EventRecord{EventType: EventTypeAwaitAllChildren, Request: req, Response: resp}
}
