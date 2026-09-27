package engine

import (
	"context"
	"strings"
	"testing"
)

// A history with a record missing is not a history that ended. cleat#1507.
//
// WHAT THIS PINS, and why it is a property of the whole package rather than of
// one guard. Every replay guard in this package reads
//
//	if s.stepCount < len(s.history) { rec := s.history[s.stepCount]; ... }
//
// Thirty-one of them then compare rec.EventType and report a divergence when it
// is wrong. NONE of them can see a record that is ABSENT: a missing row does not
// produce a wrong type, it shifts every later record one index earlier and makes
// the history one shorter, so the guard eventually finds `stepCount <
// len(history)` false and takes exactly the path a workflow that legitimately
// reached the end of its history takes -- exitReplay(), and on into fresh
// execution.
//
// NOT cleat#1429, though an earlier version of this comment said so. That
// repro deletes a SLEEPING parent's only row -- DurableSleep records no event,
// so the history is the lone child_workflow spawn -- which empties the history
// rather than perforating it, and an empty history is the excluded tail case
// below. The distinction is worth carrying because the symptom is identical.
//
// So the fix cannot live in the guards, and adding a branch to thirty-four call
// sites would not have found it either. It is one property of the history,
// checked once.
func TestAMissingHistoryRecordIsNotAnExhaustedHistory(t *testing.T) {
	// A complete history: index i carries Step i. This is what fifty
	// `Step: s.stepCount` record sites produce.
	dense := []EventRecord{
		{Step: 0, EventType: EventTypeChildWorkflow, ChildName: "charge", RunID: "run-a"},
		{Step: 1, EventType: EventTypeAwaitChild, RunID: "run-a", Response: `{"ok":true}`},
		{Step: 2, EventType: EventTypeCall, Service: "svc", Op: "op", Response: `{}`},
	}

	t.Run("a dense history passes", func(t *testing.T) {
		if err := validateReplayStepDensity(dense); err != nil {
			t.Fatalf("a complete history was rejected: %v", err)
		}
	})

	// An empty history passes, and this is the SECOND known limit rather than a
	// degenerate case to wave through. A workflow on its first execution and a
	// workflow whose every row was deleted present identically here: no records,
	// nothing to compare, and `isReplay` false either way, so the run proceeds
	// fresh and redoes whatever the lost rows described.
	//
	// That is cleat#1429's actual shape, measured by a peer after this check was
	// built against the wrong example: a sleeping parent records only its
	// child_workflow spawn (DurableSleep writes no event), so deleting that one
	// row leaves event_count=1 and rows=0, and the resumed run spawns a second
	// child. Asserted as a PASS so the limit is visible in the test file rather
	// than only in prose.
	t.Run("an empty history passes, which is the second known limit", func(t *testing.T) {
		if err := validateReplayStepDensity(nil); err != nil {
			t.Fatalf("an empty history was rejected: %v\n\n"+
				"That would fail every first execution, which is the overwhelmingly common "+
				"case. An emptied history is indistinguishable from a fresh one by this "+
				"means; separating them needs something written before the loss and read "+
				"after it, such as workflow_instances.event_count.", err)
		}
	})

	// THE CASE THIS EXISTS FOR, reproduced as data: a history of two or more
	// records that loses one. The step-0 record is deleted; the rest are
	// untouched and still carry their original step numbers, which is what a
	// row disappearing from event_history looks like on the next read.
	//
	// Two records minimum is load-bearing. A one-record history that loses its
	// record is EMPTY, not perforated, and lands in the excluded case at the
	// bottom of this test -- which is what cleat#1429's repro actually
	// produces.
	t.Run("the step-0 record is missing", func(t *testing.T) {
		gapped := dense[1:]

		err := validateReplayStepDensity(gapped)
		if err == nil {
			t.Fatal("a history whose first record carries step 1 was accepted.\n\n" +
				"Replay would hand the guest's first durable call the record for step 1, and " +
				"where the types happen to agree it would consume it and return another " +
				"step's result. Nothing downstream can detect this: the type checks see a " +
				"plausible record and the exhaustion branch sees a short history.")
		}
		// The message has to name the position, because the operator's next
		// question is which record to look for.
		for _, want := range []string{"step 1", "missing"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not contain %q, so it does not say which record is "+
					"absent: %v", want, err)
			}
		}
	})

	t.Run("a record missing from the middle", func(t *testing.T) {
		// Steps 0 and 2 present, 1 gone. The first record is fine, so a
		// check that looked only at the head would pass this.
		middle := []EventRecord{dense[0], dense[2]}
		if err := validateReplayStepDensity(middle); err == nil {
			t.Fatal("a history of steps 0 and 2 was accepted; a validator that only " +
				"inspects the first record would also accept it")
		}
	})

	// The limit, asserted so nobody reads the check as stronger than it is.
	//
	// A TRUNCATED TAIL is indistinguishable from a completed history by this
	// means, because the records that would disagree are precisely the ones
	// that are gone. This test asserts the FALSE NEGATIVE deliberately: if
	// someone later makes tail truncation detectable, this case should start
	// failing and be rewritten, rather than the limit being rediscovered.
	t.Run("a truncated tail is NOT detected, and that is the known limit", func(t *testing.T) {
		truncated := dense[:2]
		if err := validateReplayStepDensity(truncated); err != nil {
			t.Fatalf("the density check reported a truncated tail: %v\n\n"+
				"That is a stronger result than this check can give, so either the check "+
				"changed or this fixture is wrong. It cannot distinguish a history that "+
				"lost its last record from one whose workflow suspended there.", err)
		}
	})
}

// The validator above is only worth having if the executor calls it, and a test
// of the function alone cannot tell the difference between "wired" and
// "written". This drives Engine.Replay -- the entry point a worker uses -- and
// asserts on the mock backend's execute count that the guest NEVER RAN.
//
// That last assertion is the one that matters and it is not decoration. The
// point of failing here rather than inside a guard is that a workflow whose
// history is incomplete must not execute at all: a guard-level report happens
// after the guest has already re-run some prefix of its steps, and for a
// workflow whose steps have external effects that is too late. So "returned an
// error" is not enough; "returned an error and did not start the guest" is the
// property.
func TestTheExecutorRefusesAnIncompleteHistoryBeforeRunningTheGuest(t *testing.T) {
	ctx := context.Background()

	dense := []EventRecord{
		{Step: 0, EventType: EventTypeChildWorkflow, ChildName: "charge", RunID: "run-a"},
		{Step: 1, EventType: EventTypeAwaitChild, RunID: "run-a", Response: `{"ok":true}`},
	}

	t.Run("a gapped history is refused and the backend is never entered", func(t *testing.T) {
		backend := &mockBackend{name: "test-backend"}
		e := NewEngine(nil, &mockCaller{}, WithBackend("go", backend))

		_, _, _, _, _, err := e.Replay(ctx, wasmWithoutLanguage(), "Handle", nil, dense[1:])
		if err == nil {
			t.Fatal("Replay accepted a history whose first record carries step 1")
		}
		if !strings.Contains(err.Error(), "incomplete") {
			t.Errorf("the error does not identify itself as an incomplete history: %v", err)
		}
		if backend.executeCalled != 0 {
			t.Errorf("the backend ran %d time(s). The guest must not execute against a "+
				"history known to be missing records -- refusing after it has re-run a "+
				"prefix of its steps is too late for any step with an external effect.",
				backend.executeCalled)
		}
	})

	// THE CONTROL. Without it the case above passes against an engine that
	// refuses every replay, or one whose backend wiring is broken so nothing
	// ever executes. The same call with a COMPLETE history must reach the
	// backend.
	t.Run("control: a dense history does reach the backend", func(t *testing.T) {
		backend := &mockBackend{name: "test-backend"}
		e := NewEngine(nil, &mockCaller{}, WithBackend("go", backend))

		_, _, _, _, _, err := e.Replay(ctx, wasmWithoutLanguage(), "Handle", nil, dense)
		if err != nil {
			t.Fatalf("a complete history was refused: %v", err)
		}
		if backend.executeCalled != 1 {
			t.Fatalf("the backend ran %d times on a complete history, want 1. The negative "+
				"case above proves nothing if the backend is unreachable in both.",
				backend.executeCalled)
		}
	})
}
