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
// W3b (await_any_child) adds its own rows below, each marked the same way: OLD = what the old code did, and the row fails on it.
//
// Two rows are here that hold on BOTH codes, and are kept by name because they are the other side of the tightening: a record that names NO child
// cannot name another, so it is still this call's.

import (
	"context"
	"testing"
)

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

		// ---- await_any_child (W3b) ----
		{name: "OLD consumed it and answered with another set's child: an await_any ANSWER recorded for ANOTHER set of children is a divergence, and consumes nothing (D1)",
			call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, hist: []EventRecord{anyRecForTightening(`["c9"]`, `{"run_id":"c9","result":"R9"}`)},
			want: childAwaitWant{errCode: 1, resultHas: `run IDs ["c1","c2"], recorded ["c9"]`, step: 0, replaying: true}},
		{name: "OLD consumed another set's empty await and its answer and delivered that answer: an EMPTY await recorded for another set is a divergence too (D1)",
			call: callOfAwaitAnyChild, ids: []string{"c1", "c2"},
			hist: []EventRecord{anyRecForTightening(`["c9"]`, ""), anyRecForTightening(`["c9"]`, `{"run_id":"c9","result":"R9"}`)},
			want: childAwaitWant{errCode: 1, resultHas: `run IDs ["c1","c2"], recorded ["c9"]`, step: 0, replaying: true}},
		{name: "OLD consumed another set's pending await and answered THIS call's children after it: a lone empty await for another set is a divergence (D1)",
			call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, hist: []EventRecord{anyRecForTightening(`["c9"]`, "")}, outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want: childAwaitWant{errCode: 1, resultHas: `run IDs ["c1","c2"], recorded ["c9"]`, step: 0, replaying: true}},
		{name: "OLD refused it (a guest drain's await in a session that had suspended): a drain the guest BEGAN BEFORE the suspension goes through, as for await_child and await_all (D2)",
			call: callOfAwaitAnyChild, ids: []string{"c1"}, outcomes: map[string]ChildOutcome{"c1": done("R1")},
			mod: func(s *execSession) {
				s.SetDeferPhase(context.Background(), true)
				s.suspendErr = &SuspendError{Reason: "earlier"}
			},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{{typ: EventTypeAwaitAnyChild, resp: `{"run_id":"c1","result":"R1"}`, req: `["c1"]`}}, reason: "earlier", step: 1, gets: 1, getOrder: []string{"c1"}}},
		{name: "UNCHANGED (holds on both): an await_any answer that names NO set is still this call's",
			call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, hist: []EventRecord{anyRecForTightening("", `{"run_id":"c1","result":"R1"}`)},
			want: childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, step: 1, replaying: true}},
		{name: "UNCHANGED (holds on both, D4): the empties of two DIFFERENT sets side by side are two calls' pending awaits: the first call's run ends at the second set's record, which is left unread, and the call re-checks",
			call: callOfAwaitAnyChild, ids: []string{"c1", "c2"}, hist: []EventRecord{anyRecForTightening(`["c1","c2"]`, ""), anyRecForTightening(`["c9"]`, "")},
			outcomes: map[string]ChildOutcome{"c1": done("R1")},
			want:     childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{{typ: EventTypeAwaitAnyChild, resp: `{"run_id":"c1","result":"R1"}`, req: `["c1","c2"]`}}, step: 2, gets: 1, getOrder: []string{"c1"}}},

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
	// cleat#4285 step 3: a pending child await followed by a call the guest's BODY recorded after it is a divergence that consumes nothing. Each of the three child
	// awaits has the same pair: the OLD row (the old code left that record unread, re-checked the child and went on, so the recorded call would run again; fails on
	// it) and the exemptions, which hold on BOTH codes because they are the records that are legitimately left unread.
	for _, c := range []struct {
		label   string
		call    childAwaitCall
		ids     []string
		pending EventRecord
		out     map[string]ChildOutcome
		then    childAwaitWant // the answer when the later record is left unread
	}{
		{"await_child", callOfAwaitChild, []string{"c1"}, EventRecord{EventType: EventTypeAwaitChild, RunID: "c1"}, map[string]ChildOutcome{"c1": done("R")},
			childAwaitWant{result: "R", recs: []childAwaitRec{{typ: EventTypeAwaitChild, resp: "R", runID: "c1"}}, step: 1, gets: 1}},
		{"await_all", callOfAwaitAllChildren, []string{"c1", "c2"}, allRecForTightening(two, ""), map[string]ChildOutcome{"c1": done("R1"), "c2": done("R2")},
			childAwaitWant{result: `[{"run_id":"c1","result":"R1"},{"run_id":"c2","result":"R2"}]`, recs: []childAwaitRec{{typ: EventTypeAwaitAllChildren, resp: `[{"run_id":"c1","result":"R1"},{"run_id":"c2","result":"R2"}]`, req: two}}, step: 1, gets: 2, getOrder: []string{"c1", "c2"}}},
		{"await_any", callOfAwaitAnyChild, []string{"c1", "c2"}, anyRecForTightening(two, ""), map[string]ChildOutcome{"c1": done("R1")},
			childAwaitWant{result: `{"run_id":"c1","result":"R1"}`, recs: []childAwaitRec{{typ: EventTypeAwaitAnyChild, resp: `{"run_id":"c1","result":"R1"}`, req: two}}, step: 2, gets: 1, getOrder: []string{"c1"}}},
	} {
		later := foreign // a recorded side_effect: the effect of a call the guest's body made
		rows = append(rows,
			childAwaitRow{name: "OLD left the record unread, re-checked the child and went on, so the recorded call would run again: " + c.label + " replay of its pending record FOLLOWED by a call the body recorded is a divergence that consumes NOTHING (cleat#4285)",
				call: c.call, ids: c.ids, hist: []EventRecord{c.pending, later}, outcomes: c.out,
				want: childAwaitWant{errCode: 1, resultHas: "ignored a child-await suspension before 0.6.0 (cleat#4285)", step: 0, replaying: true}},
			childAwaitRow{name: "UNCHANGED (holds on both): " + c.label + " replay of its pending record followed by a call a defer DRAIN recorded (InDeferPhase) leaves that record unread: the drain's own defers follow its pending await by design (cleat#3429)",
				call: c.call, ids: c.ids, hist: []EventRecord{c.pending, stamped(later, true, false)}, outcomes: c.out, want: c.then},
			childAwaitRow{name: "UNCHANGED (holds on both): " + c.label + " replay of its pending record followed by a call a defer SEGMENT recorded (InDeferSegment) leaves that record unread",
				call: c.call, ids: c.ids, hist: []EventRecord{c.pending, stamped(later, false, true)}, outcomes: c.out, want: c.then},
			childAwaitRow{name: "UNCHANGED (holds on both): " + c.label + " replay of a pending record followed by a call that was EXPANDED FROM A COMPACTION leaves that record unread: a state compacted before cleat#4310 did not carry the stamps, so an unstamped record there is unknown and not stale",
				call: c.call, ids: c.ids, hist: []EventRecord{c.pending, later}, outcomes: c.out, mod: func(s *execSession) { s.unstampedRecords = 2 }, want: c.then},
			childAwaitRow{name: "UNCHANGED (holds on both): " + c.label + " replay of its pending record followed by a record that is NOT a call's effect (an admin audit row, a delivery) leaves that record unread: re-reading it runs nothing twice",
				call: c.call, ids: c.ids, hist: []EventRecord{c.pending, {EventType: EventTypeSignalReceived, Service: SignalEventService, Op: "deliver"}}, outcomes: c.out, want: c.then},
		)
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { runChildAwaitRow(t, r) })
	}
}

// stamped returns rec marked as a defer drain's (inPhase) or a defer segment's (inSegment) record.
func stamped(rec EventRecord, inPhase, inSegment bool) EventRecord {
	rec.InDeferPhase, rec.InDeferSegment = inPhase, inSegment
	return rec
}

func allRecForTightening(req, resp string) EventRecord {
	return EventRecord{EventType: EventTypeAwaitAllChildren, Request: req, Response: resp}
}

func anyRecForTightening(req, resp string) EventRecord {
	return EventRecord{EventType: EventTypeAwaitAnyChild, Request: req, Response: resp}
}
