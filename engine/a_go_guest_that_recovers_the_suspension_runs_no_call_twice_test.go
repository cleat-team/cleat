package engine

// cleat#4285 with a REAL guest, in the one SDK that builds in-tree. The Go SDK stops a guest by panicking cleat.ErrSuspend out of the call that suspended;
// testdata/ignoredsuspend recovers it and makes a service call anyway. Before the host-side refusal that call was dispatched in the first segment, recorded
// after the pending await, and dispatched AGAIN when the workflow resumed: a service call that must happen once, twice. The test measures the thing the bug
// is about, the number of times the service saw the call across both segments, and not a stand-in for it.
//
// Run on the real wasmtime backend against a compiled guest, so the word the host answers is decoded by the SDK's own adapter. One SDK measured; the others
// are covered by the table in a_guest_that_ignores_a_suspension_starts_no_fresh_work_test.go at the host-call level, and Java by its own guest test.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAGoGuestThatRecoversTheChildSuspensionRunsNoCallTwice(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "ignoredsuspend"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })

	caller := &mockCaller{}
	eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-ignored-suspend-go"))
	var childDone atomic.Bool
	eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) {
		if childDone.Load() {
			return ChildOutcome{Completed: true, Result: `{"r":1}`}, nil
		}
		return ChildOutcome{}, nil
	}}
	dispatched := func() (n int) {
		for _, c := range caller.calls {
			if c.Service == "work" && c.Op == "after_suspend" {
				n++
			}
		}
		return n
	}
	typesOf := func(h []EventRecord) (out []string) {
		for _, r := range h {
			out = append(out, string(r.EventType))
		}
		return out
	}

	// SEGMENT 1: the child is running. The guest is told to stop, recovers, and calls the service.
	_, hist1, susp, _, _, err := eng.Execute(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if susp == nil || !strings.HasPrefix(susp.Reason, "await_child(") {
		t.Fatalf("segment 1 suspension = %+v, want await_child(...): the guest did not reach the await, so nothing below measured the bug", susp)
	}
	if n := dispatched(); n != 0 {
		t.Errorf("segment 1: the service saw the call %d time(s) from a guest that had already been told to stop; want 0 (dispatched=%v)", n, opsOf(caller.calls))
	}
	if got := typesOf(hist1); len(got) != 1 || got[0] != string(EventTypeAwaitChild) {
		t.Errorf("segment 1 history = %v, want just the pending await_child: a refused call records nothing", got)
	}

	// SEGMENT 2: the child has finished, and the workflow resumes by replaying that history. The call is made, once.
	childDone.Store(true)
	out, hist2, susp2, _, _, err := eng.Replay(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`), hist1)
	if err != nil {
		t.Fatalf("segment 2: %v", err)
	}
	if susp2 != nil || !strings.Contains(out, "ok") {
		t.Fatalf("segment 2 did not complete: result=%q suspended=%+v history=%v", out, susp2, typesOf(hist2))
	}
	if n := dispatched(); n != 1 {
		t.Errorf("the service saw the call %d times across both segments, want exactly 1 (dispatched=%v)", n, opsOf(caller.calls))
	}
	calls := 0
	for _, r := range hist2 {
		if r.EventType == EventTypeCall {
			calls++
		}
	}
	if calls != 1 {
		t.Errorf("final history holds %d call records, want 1: %v", calls, typesOf(hist2))
	}
}

// TestAnOldHistoryWithACallAfterAPendingAwaitIsReplayedWithoutRunningItAgain is cleat#4285 step 3, measured on a real guest. A history written BEFORE the host
// refused fresh work after a suspension holds [pending await_child, a service call the guest made after ignoring the suspension], and that call ALREADY ran.
// Replaying it must not dispatch the call again, and must say why. Measured on develop 8da56c0b, before this change, the two resumes went differently:
//   - the child STILL running: the guest was told to stop again, ignored it, and the host refused its call (0 dispatches, a quiet suspension);
//   - the child FINISHED: nothing suspends, so there was no suspension to refuse on: the await was answered over the pending record and the guest carried on to a call
//     whose record sat unread behind it, so the service saw the call a SECOND time (1 dispatch). That is the one that mattered: it is how a parent wakes.
//
// Both are now a replay divergence at the pending await (the check runs before the child's state is read), so the workflow fails loudly, the call is dispatched
// zero times, and nothing waits for the child only to fail later.
func TestAnOldHistoryWithACallAfterAPendingAwaitIsReplayedWithoutRunningItAgain(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "ignoredsuspend"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	engineFor := func(childDone bool) (*Engine, *mockCaller) {
		caller := &mockCaller{}
		eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-old-pending-history"))
		eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) {
			if childDone {
				return ChildOutcome{Completed: true, Result: `{"r":1}`}, nil
			}
			return ChildOutcome{}, nil
		}}
		return eng, caller
	}
	dispatched := func(c *mockCaller) (n int) {
		for _, call := range c.calls {
			if call.Service == "work" && call.Op == "after_suspend" {
				n++
			}
		}
		return n
	}

	// The call record exactly as the host writes it: run the guest once with the child already finished, and take the call's record from that history.
	engDone, callerDone := engineFor(true)
	if _, hist, _, _, _, err := engDone.Execute(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`)); err != nil || len(hist) != 2 || hist[1].EventType != EventTypeCall {
		t.Fatalf("setup: could not derive a call record: err=%v history=%v", err, hist)
	} else {
		oldHistory := []EventRecord{
			{Step: 0, EventType: EventTypeAwaitChild, RunID: "child-1"}, // the pending await: EMPTY, as the host records it while the child runs
			hist[1], // the call the guest made after ignoring the suspension: it ALREADY ran
		}
		oldHistory[1].Step = 1
		if n := dispatched(callerDone); n != 1 {
			t.Fatalf("setup: the derivation run dispatched the call %d times, want 1", n)
		}

		for _, tc := range []struct {
			name      string
			childDone bool
		}{
			{"the child is STILL running at the resume (was a quiet suspension: the host refused the call)", false},
			{"the child has FINISHED at the resume (was a second dispatch of the call)", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				eng, caller := engineFor(tc.childDone)
				_, _, _, _, _, err := eng.Replay(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`), append([]EventRecord(nil), oldHistory...))
				if n := dispatched(caller); n != 0 {
					t.Errorf("replaying a history in which the call ALREADY RAN dispatched it %d more time(s); want 0, because the record of the first run is right there", n)
				}
				if err == nil || !strings.Contains(err.Error(), "replay divergence") {
					t.Errorf("replay of a pending await followed by a call the body recorded returned err=%v; want a replay divergence, so the operator is told and nothing runs", err)
				}
			})
		}
	}
}

// TestAStaleCallThatCompactionFoldedAwayIsNotADivergence is the other half of the rule, through the REAL executor: a state compacted before cleat#4310 did not carry the drain stamps
// (InDeferPhase, InDeferSegment), so a record expanded from it has an UNKNOWN stamp, and failing a workflow on an unknown would fail a healthy drain whose records were compacted.
// The state is built with no marker (CompactionState.StampedFrom nil), which is what every state written before cleat#4323 decodes to; a state WITH the marker judges its records by
// their stamps, and TestTheRefusalOfACallAfterAPendingAwaitFollowsTheStampsThatAreKnown covers that side and the boundary between the two. The history here is the same shape as the one above, with the pending await and the call BOTH inside the compacted prefix and one live
// record after them; the engine must not diverge. (The call does run again here: that is the quiet, reported path this rule leaves for what it cannot tell apart.)
// A mutant that never sets execSession.unstampedRecords in the executor fails this and nothing in the unit rows, which set the field by hand.
func TestAStaleCallThatCompactionFoldedAwayIsNotADivergence(t *testing.T) {
	ctx := context.Background()
	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "ignoredsuspend"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	rt, err := NewRuntime(ctx, 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(ctx) })
	wt, err := NewWasmtimeBackend(ctx)
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(ctx) })
	newEngine := func(opts ...EngineOption) (*Engine, *mockCaller) {
		caller := &mockCaller{}
		eng := NewEngine(rt, caller, append([]EngineOption{WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-compacted-pending-history")}, opts...)...)
		eng.childWfStore = &mockChildStore{getChildResultFn: func(context.Context, string) (ChildOutcome, error) {
			return ChildOutcome{Completed: true, Result: `{"r":1}`}, nil
		}}
		return eng, caller
	}
	engDone, _ := newEngine()
	_, hist, _, _, _, err := engDone.Execute(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`))
	if err != nil || len(hist) != 2 || hist[1].EventType != EventTypeCall {
		t.Fatalf("setup: could not derive a call record: err=%v history=%v", err, hist)
	}
	folded := []EventRecord{{Step: 0, EventType: EventTypeAwaitChild, RunID: "child-1"}, hist[1]}
	folded[1].Step = 1
	cs, err := extractCompactionStateFrom(nil, folded)
	if err != nil {
		t.Fatalf("setup: compaction: %v", err)
	}
	cs.StampedFrom = nil // a state written before the marker existed: nothing in it is known to be stamped
	tail := []EventRecord{{Step: 2, EventType: EventTypeSignalReceived, Service: SignalEventService, Op: "deliver"}}
	eng, _ := newEngine(WithCompactionState(cs))
	_, _, _, _, _, err = eng.Replay(ctx, wasmBytes, "ignore_child_suspend", json.RawMessage(`{}`), tail)
	// The text to look for is the generic one: this guest swallows the await's own divergence message and the error that reaches the caller is the NEXT call's, which
	// is the same "replay divergence" (the first version of this test looked for the await's wording, passed with the executor wiring removed, and so measured nothing).
	if err != nil && strings.Contains(err.Error(), "replay divergence") {
		t.Fatalf("a pending await followed by a call that compaction folded into the prefix was a divergence (%v): the stamps are unknown there (a state with no marker, from before cleat#4310), so it must not be", err)
	}
}
