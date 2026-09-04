//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 3.35 phase 5 / 3.75 / 3.81.
//
// §3.75's design for the two-phase terminal transition rests on one claim,
// stated there as settled: "RequestCancellation is not in this set: it sets
// cancellation_requested, the guest observes it and exits through its own
// wrapper, so that path already runs defers." The tests here measure that
// claim. Half of it is true and half of it is false, and the false half
// decided the shape of the defer segment, so both are pinned rather than
// described -- followed by the mechanism that was built instead.
//
// The mockCaller cannot answer either question on its own. A defer body's own
// DurableCall is refused by the same cancellation check that refused the
// body's, so it never reaches the caller whether the body ran or not -- "no
// calls recorded" is exactly what a working drain and a broken one both look
// like. What separates them is the number of fresh-call ATTEMPTS, which
// keyedCancellationStore.queriedWith counts: one poll per attempt, before the
// refusal. Choosing the observable is the whole of these tests.

// deferPhaseProbeEngine builds a real Go SDK guest on wasmtime with a signal
// store that reports the workflow cancelled, which is the only mechanism in
// the tree today that refuses a fresh durable call.
func deferPhaseProbeEngine(t *testing.T, wfID string, cancelled bool) ([]byte, *Engine, *mockCaller, *keyedCancellationStore) {
	t.Helper()
	ctx := context.Background()

	wasmBytes, err := os.ReadFile(buildFixtureWasm(t, "deferfunc"))
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

	sig := &keyedCancellationStore{}
	if cancelled {
		sig.cancelledWorkflowID = wfID
		sig.reason = "terminated"
	}
	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID(wfID),
		WithSignalStore(sig))
	return wasmBytes, eng, caller, sig
}

// TestARefusedFreshCallMakesTheGuestDrainItsDefers is the half of §3.75's
// claim that holds.
//
// Refusing a fresh durable call is enough to make a Go SDK guest unwind out of
// its entry point and through the wrapper that drains its defer table. Nothing
// has to kill it and nothing has to ask it to. This is what makes a defer
// segment buildable at all: the host does not need a way to stop a replaying
// guest, only a way to refuse it.
//
// Measured 2026-09-02: 3 fresh-call attempts -- the body's, then one from each
// of the two defer bodies, in the LIFO order the drain runs them.
func TestARefusedFreshCallMakesTheGuestDrainItsDefers(t *testing.T) {
	wasmBytes, eng, _, sig := deferPhaseProbeEngine(t, "wf-refused-call", true)

	_, _, susp, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_order", json.RawMessage(`{}`))

	if susp != nil {
		t.Fatalf("the workflow suspended; it was supposed to be refused and unwind")
	}
	if err == nil {
		t.Fatalf("the workflow succeeded; a refused call must fail it")
	}

	// 3 = the body's call, plus one from each defer body. 1 would mean the
	// guest returned the error without draining, which is the state that would
	// make a defer segment impossible to build on a refusal alone.
	if got := len(sig.queriedWith); got != 3 {
		t.Fatalf("%d fresh-call attempts, want 3 (the body's, then both defer bodies').\n\n"+
			"1 means the guest did not drain its defer table when its call was refused, "+
			"and the whole two-phase terminal transition in IMPROVEMENT-PLAN 3.75 would "+
			"need a different way to reach the defers.", got)
	}
}

// TestRefusingEveryFreshCallDestroysTheCleanup is the half that does not hold,
// and it is the finding that changes phase 5's design.
//
// Cancellation refuses fresh calls unconditionally. A defer body's calls are
// fresh calls. So the drain runs -- the test above proves it -- and every
// cleanup call it makes is refused: the lock is not released, the charge is
// not refunded, and the defer table is emptied on the way through, because
// _cleatRunDeferred takes the table before running anything. The cleanup is
// not merely skipped. It is consumed.
//
// So cancellation is not the mechanism §3.75 took it for, and a defer segment
// cannot be built by reusing it. Refusing a call requires the host to
// distinguish one made by the workflow body from one made by a defer body,
// and it cannot: _cleatInDeferPhase (wasm/exports.go) is guest-side only and
// the guest never tells the host.
//
// WithDeferPhase does not solve that. It sidesteps it -- see
// TestADeferSegmentDrainsOnTheSuspension, which refuses nothing at all. This
// test therefore still passes, and pins why the refusal route was not taken.
func TestRefusingEveryFreshCallDestroysTheCleanup(t *testing.T) {
	wasmBytes, eng, caller, _ := deferPhaseProbeEngine(t, "wf-cleanup-refused", true)

	_, _, _, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_order", json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("the workflow succeeded; a refused call must fail it")
	}

	got := operationsCalled(caller)
	if len(got) != 0 {
		t.Fatalf("the defer bodies reached the ServiceCaller with %v.\n\n"+
			"If this is failing because a defer phase now exempts defer bodies from "+
			"the refusal, that is the fix -- rewrite this test to assert the "+
			"cleanup calls ARE made, and update IMPROVEMENT-PLAN 3.75, which "+
			"currently records cancellation as already running defers correctly.", got)
	}
}

// TestASleepingWorkflowNeverReachesAFreshCall is the second obstacle, and it
// is the common case rather than an edge one.
//
// A workflow worth terminating is usually one that is waiting -- sleeping, or
// awaiting a signal. Replay it and it re-suspends on that same wait: it never
// reaches the end of history, so it never makes a fresh call, so the refusal
// that would have started its defer phase never fires. Measured here as zero
// fresh-call attempts on the second segment.
//
// A defer segment therefore cannot rely on "replay until the first fresh
// call". It has to handle a replay that ends in a suspension, which is the
// shape §3.75's "replay history to reconstruct the instance, run the
// registered defers in it" does not describe -- and which turns out to be the
// mechanism rather than the obstacle. This test is the control for the one
// below it: same fixture, same history, no defer phase, no cleanup.
func TestASleepingWorkflowNeverReachesAFreshCall(t *testing.T) {
	ctx := context.Background()

	// Segment 1: registers a defer, then sleeps. The defer must not run --
	// a suspension is not workflow exit.
	wasmBytes, eng1, caller1, _ := deferPhaseProbeEngine(t, "wf-sleeping", false)
	_, history, susp, _, _, err := eng1.Execute(ctx, wasmBytes,
		"defer_survives_suspension", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if susp == nil {
		t.Fatalf("segment 1 did not suspend; the fixture is supposed to sleep")
	}
	if got := operationsCalled(caller1); len(got) != 0 {
		t.Fatalf("segment 1 made calls %v; a defer must not run on a suspension", got)
	}

	// Segment 2: the workflow has been cancelled, and replays. The recorded
	// sleep has not elapsed, so it suspends again.
	_, eng2, caller2, sig2 := deferPhaseProbeEngine(t, "wf-sleeping", true)
	_, _, susp2, _, _, _ := eng2.Replay(ctx, wasmBytes,
		"defer_survives_suspension", json.RawMessage(`{}`), history)

	if susp2 == nil {
		t.Fatalf("segment 2 did not suspend. If the sleep now completes on replay, "+
			"this test no longer measures what it was written for -- ops=%v",
			operationsCalled(caller2))
	}
	if got := len(sig2.queriedWith); got != 0 {
		t.Fatalf("%d fresh-call attempts on the replay, want 0. The workflow was "+
			"supposed to re-suspend on its recorded sleep without reaching the end "+
			"of history.", got)
	}
	if got := operationsCalled(caller2); len(got) != 0 {
		t.Fatalf("segment 2 ran cleanup %v. A re-suspended workflow has not "+
			"terminated, so its defers must still not run.", got)
	}
}

// TestADeferSegmentDrainsOnTheSuspension is the mechanism §3.81 names, measured.
//
// Same setup as TestASleepingWorkflowNeverReachesAFreshCall -- a workflow that
// registered a defer and then slept -- but the replay runs as a defer segment.
// The recorded sleep still has not elapsed, so the guest still re-suspends and
// still never reaches a fresh call. The difference is that the host now drains
// the defer table itself, on the instance that is still live, and the defer
// body's own host call goes through with ordinary semantics.
//
// That last part is the whole point and is what separates this from
// cancellation: TestRefusingEveryFreshCallDestroysTheCleanup measures 0
// recorded calls because the cleanup was refused. Here it is performed.
func TestADeferSegmentDrainsOnTheSuspension(t *testing.T) {
	ctx := context.Background()

	wasmBytes, eng1, caller1, _ := deferPhaseProbeEngine(t, "wf-defer-segment", false)
	_, history, susp, _, _, err := eng1.Execute(ctx, wasmBytes,
		"defer_survives_suspension", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if susp == nil {
		t.Fatalf("segment 1 did not suspend")
	}
	if got := operationsCalled(caller1); len(got) != 0 {
		t.Fatalf("segment 1 made calls %v; a defer must not run on a suspension", got)
	}

	// The defer segment. No cancellation: nothing is refused, because nothing
	// needs to be -- the guest re-suspends on its own recorded sleep.
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

	caller2 := &mockCaller{}
	eng2 := NewEngine(rt, caller2,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-defer-segment"),
		WithDeferPhase())

	_, _, susp2, _, _, _ := eng2.Replay(ctx, wasmBytes,
		"defer_survives_suspension", json.RawMessage(`{}`), history)
	if susp2 == nil {
		t.Fatalf("the defer segment did not suspend; this test no longer measures " +
			"the suspension path it was written for")
	}

	got := operationsCalled(caller2)
	want := "after_sleep"
	found := false
	for _, op := range got {
		if op == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("the defer segment recorded %v, want %q among them.\n\n"+
			"The registered defer did not reach the ServiceCaller. Either the host "+
			"did not call %s on the suspension, or the guest's table was already "+
			"empty -- and an empty table is the 3.81 failure, where the drain ran "+
			"with its calls refused and consumed the cleanup.", got, want, deferRunnerExport)
	}
}

// TestADeferSegmentPastTheFrontierRunsOnlyTheDefers is the regression test for
// IMPROVEMENT-PLAN 3.83.
//
// The segment above ends in a suspension the guest reached on its own, which is
// the common case -- a workflow worth terminating is usually one that is
// waiting. A workflow finalized `ready` is the other case: replay reaches the
// end of recorded history and the guest simply carries on, because nothing
// tells it to stop.
//
// Before the fix, measured on a segment with no history at all -- that case in
// its purest form:
//
//	operations reaching the ServiceCaller: [body second first]
//	result: {"status":"ok"}   suspended: false   err: nil
//
// Two defects, and the second is the worse one. `body` ran, so the segment
// performed the workflow's own side effect rather than its cleanup. And the
// segment returned a successful completion result, so a terminated workflow is
// reported as having finished normally by the machinery meant to clean up
// after it.
//
// The host now returns callSuspendSentinel for a durable call the workflow
// BODY makes past the frontier. The guest unwinds with its suspend flag set,
// skips its own drain, and the host runs the defer table itself.
//
// This test asserts BOTH halves, which is the point. "The body was stopped" is
// satisfied by a segment that stops everything, including the cleanup -- which
// is precisely the destructive outcome 3.81 measured, where refusing every
// fresh call consumes the defer table instead of running it. So the absence of
// `body` and the presence of the cleanup calls have to be asserted together;
// either one alone passes for a broken implementation.
func TestADeferSegmentPastTheFrontierRunsOnlyTheDefers(t *testing.T) {
	wasmBytes, _, _, _ := deferPhaseProbeEngine(t, "wf-past-frontier", false)

	rt, err := NewRuntime(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	t.Cleanup(func() { rt.Close(context.Background()) })
	wt, err := NewWasmtimeBackend(context.Background())
	if err != nil {
		t.Fatalf("NewWasmtimeBackend: %v", err)
	}
	t.Cleanup(func() { wt.Close(context.Background()) })

	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-past-frontier"),
		WithDeferPhase())

	res, _, susp, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_order", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The segment must not report an outcome for a workflow whose outcome was
	// already decided. This is the half that was a status defect rather than a
	// wasted side effect.
	if susp == nil {
		t.Fatalf("the segment did not suspend; it returned result %q. A defer "+
			"segment that runs to completion reports a terminated workflow as "+
			"having finished normally.", res)
	}

	got := operationsCalled(caller)
	for _, op := range got {
		if op == "body" {
			t.Fatalf("the workflow body reached the ServiceCaller: %v.\n\n"+
				"A defer segment performed the workflow's own side effect. The "+
				"host must return callSuspendSentinel for a body call past the "+
				"frontier.", got)
		}
	}

	// And the other half: stopping the body must not stop the cleanup. Both
	// defers, in LIFO order, with their calls reaching the caller -- which only
	// happens if the host bracketed its own drain with inDeferDrain.
	want := []string{"second", "first"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("the defer bodies recorded %v, want exactly %v.\n\n"+
			"An empty list is the 3.81 failure: the drain ran with its calls "+
			"stopped too, which CONSUMES the cleanup rather than skipping it, "+
			"because _cleatRunDeferred takes the whole table before running "+
			"anything.", got, want)
	}
}

// TestDeferSegmentLanguagesIsExactlyWhatHasBeenVerified pins the list itself.
//
// This exists because the test below could not catch its own mutation. It was
// written with a t.Skipf for "this language now decodes the sentinel, move it
// to the positive test", which is the polite thing to write and made the test
// vacuous: widening deferSegmentLanguages to all five made every subtest SKIP,
// and the suite printed ok. A guard that cannot fail when the thing it guards
// is removed is not a guard.
//
// So the list is asserted exactly. Growing it is a deliberate act that fails
// here first, and the failure says what has to accompany it.
func TestDeferSegmentLanguagesIsExactlyWhatHasBeenVerified(t *testing.T) {
	want := map[string]bool{"go": true, "java": true, "rust": true, "assemblyscript": true}
	if len(deferSegmentLanguages) != len(want) {
		t.Fatalf("deferSegmentLanguages = %v, want %v.\n\n"+
			"Adding a language here means its SDK decodes callSuspendSentinel "+
			"and something crosses the boundary end to end -- a host that emits "+
			"and a guest that never decodes are two green half-tests and no "+
			"working feature (IMPROVEMENT-PLAN 3.73). Update this test in the "+
			"same change.", deferSegmentLanguages, want)
	}
	for lang := range want {
		if !deferSegmentLanguages[lang] {
			t.Fatalf("deferSegmentLanguages is missing %q", lang)
		}
	}
}

// TestADeferSegmentRefusesAGuestThatCannotHearTheStop pins the fail-closed half
// of IMPROVEMENT-PLAN 3.83.
//
// callSuspendSentinel only stops a guest whose SDK decodes it. ONE of the five
// still does not: python has no decode at all, and cannot get one without an
// ABI change -- it is a Component Model guest whose WIT declares
// `durable-call: func(...) -> string`, so there is no result word to carry bit
// 31 at all. The closing paragraphs of 3.106 and 3.107 both record it, with
// the measurement (extractStringFromPacked(0x80000000, buf) == "", an empty
// SUCCESSFUL response). An SDK that does not decode
// reads the word through the ordinary durable-call layout -- responseLen = 0,
// errCode = 0 -- and gets an EMPTY SUCCESSFUL RESPONSE: it carries on past the
// stop, does the new work the segment exists to prevent, and reports the
// terminated workflow as completed, with nothing anywhere to see.
//
// So the engine refuses the segment for a language not in
// deferSegmentLanguages, rather than running one it cannot stop. This asserts
// the refusal AND that it names the language, because "some error occurred" is
// satisfied by any of the several other ways Execute can fail on a synthetic
// module -- which is the trap this file's other tests keep hitting.
func TestADeferSegmentRefusesAGuestThatCannotHearTheStop(t *testing.T) {
	// Down to one. assemblyscript left with 3.106 and rust with 3.107, both on
	// 2026-09-03; each departure made this test fail on that language first,
	// which is the pin above working rather than a test needing a nudge.
	for _, lang := range []string{"python"} {
		t.Run(lang, func(t *testing.T) {
			ctx := context.Background()
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

			eng := NewEngine(rt, &mockCaller{},
				WithBackends(WasmtimeLanguages, wt),
				WithWorkflowID("wf-"+lang),
				WithDeferPhase())

			_, _, _, _, _, err = eng.Execute(ctx, wasmWithLanguage(lang),
				"anything", json.RawMessage(`{}`))
			if err == nil {
				t.Fatalf("a defer segment ran on a %s guest, whose SDK cannot "+
					"decode the stop; it would have done new work silently", lang)
			}
			if !strings.Contains(err.Error(), "no defer-segment support") ||
				!strings.Contains(err.Error(), lang) {
				t.Fatalf("Execute failed with %q.\n\nThat is not the refusal this "+
					"test is for -- it must name the language and say the segment "+
					"was refused, or a module that failed to load for an unrelated "+
					"reason would pass this test.", err)
			}
		})
	}
}

// countingChildStore records every child start the engine attempts.
//
// The observable has to be the store, not the caller: a child workflow does not
// go through the ServiceCaller at all, so operationsCalled cannot see one
// either started or refused. Choosing the observable is the same problem this
// file's header describes for the cancellation probe, one path over.
type countingChildStore struct {
	stubWorkflowStore
	started []string
}

func (c *countingChildStore) StartChildWorkflowAtomic(ctx context.Context, childID, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, event EventRecord, priority int) (string, error) {
	c.started = append(c.started, defName)
	return "run-" + defName, nil
}

func (c *countingChildStore) StartChildWorkflow(ctx context.Context, parentID, defName, inputJSON string, defVersion int, parentClosePolicy string, priority int) (string, error) {
	c.started = append(c.started, defName)
	return "run-" + defName, nil
}

// TestADeferSegmentDoesNotStartAChildWorkflow is 3.84's test, and it has to
// exercise a path that is NOT cleat_call or it re-measures 3.83.
//
// A child workflow is the sharpest of the four remaining paths. A durable
// call's side effect belongs to some other service; a child workflow's is a row
// in this system's own workflow_instances, scheduled and claimable, which
// outlives the segment that created it. A defer segment for a TERMINATED
// workflow that starts one has manufactured live work on behalf of a workflow
// that is over.
//
// It also cannot reuse 3.83's sentinel bit. cleat_child_workflow returns a
// 32-bit run-ID length at bits 32-63, where bit 39 means a 128-byte run ID --
// an ordinary value, not a sentinel. That is why 3.84 replaced the bit rather
// than adding a second guard, and why this test is not a duplicate of the one
// above.
//
// Note which layer holds which half up, because this test does NOT check the
// bit. Measured 2026-09-02: setting callSuspendSentinel back to 1<<39 and
// updating the decoder to match leaves this test GREEN, because both sides
// still agree -- the defect bit 39 carries is that a legitimate 128-byte run ID
// collides with it, and no end-to-end run produces one. The bit choice is held
// up by TestStopSentinelBitsAcrossEveryLayout, which fails on that same
// mutation with "has bits outside the region free in all six layouts".
//
// What this test does hold up, measured the same way: removing the host guard
// in children.go starts the child; removing withSuspendCheck from the
// ChildWorkflow decoder makes the run fail instead of suspend.
func TestADeferSegmentDoesNotStartAChildWorkflow(t *testing.T) {
	ctx := context.Background()
	wasmBytes, _, _, _ := deferPhaseProbeEngine(t, "wf-child-frontier", false)

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

	children := &countingChildStore{}
	caller := &mockCaller{}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-child-frontier"),
		WithChildWorkflowStore(children),
		WithDeferPhase())

	res, _, susp, _, _, err := eng.Execute(ctx, wasmBytes,
		"defer_child_workflow", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if susp == nil {
		t.Fatalf("the segment did not suspend; it returned %q, reporting a "+
			"terminated workflow as having finished normally.", res)
	}

	if len(children.started) != 0 {
		t.Fatalf("the segment started child workflows %v.\n\n"+
			"A terminated workflow's cleanup pass created live, claimable work. "+
			"cleat_child_workflow needs the same stop cleat_call has -- and not "+
			"3.83's bit 39, which is an ordinary run-ID length in this layout. "+
			"See IMPROVEMENT-PLAN 3.84.", children.started)
	}

	// The other half, as always: stopping the body must not stop the cleanup.
	// Without this, a segment that refuses everything passes the assertion
	// above while consuming the defer table it exists to run.
	got := operationsCalled(caller)
	if len(got) != 1 || got[0] != "after_child" {
		t.Fatalf("defer calls were %v, want [after_child].\n\n"+
			"Empty means the guest did not drain its defer table -- the stop "+
			"reached the child-workflow call and then swallowed the cleanup too.", got)
	}
}
