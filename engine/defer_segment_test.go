//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"os"
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

// TestADeferSegmentPastTheFrontierDoesNewWork is a characterization test: it
// asserts behaviour that is WRONG, so that fixing it fails here and this
// comment is read.
//
// The segment above ends in a suspension, which is the common case -- a
// workflow worth terminating is usually one that is waiting. A workflow
// finalized `ready` is the other case: replay reaches the end of recorded
// history and the guest simply carries on, because nothing tells it to stop.
//
// Measured 2026-09-02 on a segment with no history at all, which is that case
// in its purest form:
//
//	operations reaching the ServiceCaller: [body second first]
//	result: {"status":"ok"}   suspended: false   err: nil
//
// Two defects, and the second is the worse one:
//
//  1. `body` ran. A defer segment performed the workflow's own side effect,
//     not its cleanup.
//  2. The segment returned a successful completion result. A workflow that was
//     terminated is reported as having finished normally, by the very machinery
//     meant to clean up after it.
//
// The fix is a host-to-guest "stop" on a fresh call, so the guest unwinds with
// __susSuspended set and lands on the drain path the test above exercises. It
// needs a sentinel bit in the cleat_call result word that the host can never
// produce by accident; see
// TestPackDurableCallResult_SentinelBitsTheHostCannotReach for which bits those
// are, and why the one IMPROVEMENT-PLAN named is not among them.
//
// When that lands, this test flips from asserting `body` is present to
// asserting it is absent.
func TestADeferSegmentPastTheFrontierDoesNewWork(t *testing.T) {
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
	if susp != nil {
		t.Fatalf("the segment suspended; this test measures the path that does " +
			"NOT suspend, so it no longer measures what it was written for")
	}

	got := operationsCalled(caller)
	sawBody := false
	for _, op := range got {
		if op == "body" {
			sawBody = true
		}
	}
	if !sawBody {
		t.Fatalf("the workflow body did NOT reach the ServiceCaller (%v).\n\n"+
			"If a stop-on-fresh-call was just implemented, that is the fix "+
			"landing: invert this test to assert `body` is ABSENT, and update "+
			"the plan section it names.", got)
	}

	// The more serious half. A terminated workflow reported as completed is a
	// status defect, not just a wasted side effect.
	if res == "" {
		t.Fatalf("the segment returned no result; this test's second assertion " +
			"no longer measures anything")
	}
	t.Logf("characterized (both WRONG, tracked in the plan): a defer segment past "+
		"the frontier ran %v and returned %q", got, res)
}
