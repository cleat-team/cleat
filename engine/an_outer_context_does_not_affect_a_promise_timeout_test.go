package engine

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestAnOuterContextDoesNotAffectAPromiseTimeout is cleat#2020's survey of
// AwaitPromise's timeout path: does whether Replay's ctx reaches (or fails to
// reach) a host call matter to it, the way it matters to the three confirmed
// dead ctx.Done() branches in engine/durablecalls.go?
//
// READING engine/promises.go answers no before any guest runs: AwaitPromise
// suspends via s.suspendErr and a wall-clock SuspendUntil computed from
// time.Now() (or the recorded await's anchor, on resume) rather than
// blocking on a select over ctx.Done() the way durablecalls.go's backoff
// wait does. There is no ctx.Done() case here for the CGO gap to sever,
// because nothing here ever waited on one.
//
// #2008's own history is the reason that reading is not where this stops.
// An execCtx-cancellation claim there survived a code reading AND a unit
// test calling the wasmtime hostfunc closure directly
// (engine/backend_wasmtime_test.go's TestClosure_AwaitPromise is exactly
// that shape for THIS host call), and was disproved only by an end-to-end
// acceptance test making a real second call through a real compiled module
// (cmd/cleat-worker/fenced_execution_acceptance_test.go). This test applies
// the same discipline here: a real wasmtime guest (testdata/promisetimeout),
// and BOTH a live and an ALREADY-CANCELLED ctx passed to the resuming Replay
// call, to see whether either changes the answer -- rather than trusting
// that an unexercised code path behaves the way it reads.
func TestAnOuterContextDoesNotAffectAPromiseTimeout(t *testing.T) {
	run := func(t *testing.T, resumeCtx context.Context) (string, *mockCaller) {
		t.Helper()

		wasmPath := buildFixtureWasm(t, "promisetimeout")
		wasmBytes, err := os.ReadFile(wasmPath)
		if err != nil {
			t.Fatalf("read WASM: %v", err)
		}

		bgCtx := context.Background()
		rt, err := NewRuntime(bgCtx, 0, 0)
		if err != nil {
			t.Fatalf("NewRuntime: %v", err)
		}
		defer rt.Close(bgCtx)

		backend, err := NewWasmtimeBackend(bgCtx)
		if err != nil {
			// Deliberately not a t.Skip -- see TestCancellationEndToEnd's
			// identical comment. wasmtime is the backend of record; skipping
			// here would let a CGO_ENABLED=0 run report this survey green
			// without ever exercising the backend it is about.
			t.Fatalf("wasmtime backend unavailable: %v (if this build disabled CGO, "+
				"that is the defect: it removes the primary backend entirely)", err)
		}
		defer backend.Close(bgCtx)

		caller := &mockCaller{}
		// status "" (the zero value) is GetPromise's pending case: never
		// resolved, never rejected. That is deliberate -- the fixture's
		// promise must still be pending when AwaitPromise's 200ms timeout
		// elapses, or the resolved branch fires instead of the timeout one
		// this test exists to exercise.
		promises := &mockPromiseStore{}
		eng := NewEngine(rt, caller, WithBackend("go", backend), WithPromiseStore(promises))

		// The fixture's Entry takes one bare string parameter, which cleat
		// build's own analysis warns binds to the ENTIRE input JSON rather
		// than a named field -- confirmed by building testdata/promisetimeout
		// directly. The fixture never reads it, so an empty JSON string
		// suffices.
		_, history, suspended, _, _, err := eng.Execute(bgCtx, wasmBytes, "entry", json.RawMessage(`""`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if suspended == nil {
			t.Fatalf("expected the workflow to suspend awaiting a promise the store never resolves")
		}
		// The fresh pass makes no call: its AwaitPromise suspends the run with
		// the stop sentinel rather than a timeout-shaped word, so the fixture
		// never reaches its "timed_out" branch until the deadline has really
		// passed (cleat#3383; TestASuspendingAwaitPromiseStopsTheGuest...).
		// Before that fix the fresh pass took the branch and issued a real
		// DurableCall, and this comment recorded that as "a property of the
		// suspend protocol".
		if len(caller.calls) != 0 {
			t.Fatalf("the fresh pass made %d call(s), want 0 -- the guest took its timeout branch "+
				"on a wait that had not timed out: %+v", len(caller.calls), caller.calls)
		}

		// Past the fixture's 200ms timeout, measured against the real clock
		// AwaitPromise's suspend/resume deadline check uses (time.Now(), per
		// engine/promises.go) -- not a mock or a fake clock, because whether
		// that comparison depends on ctx is exactly this test's question.
		time.Sleep(250 * time.Millisecond)

		result, _, suspended2, _, _, err := eng.Replay(resumeCtx, wasmBytes, "entry", json.RawMessage(`""`), history)
		if err != nil {
			t.Fatalf("Replay: %v", err)
		}
		if suspended2 != nil {
			t.Fatalf("expected the timeout to fire on resume, got a second suspend: %+v", suspended2)
		}
		return result, caller
	}

	// The control: a live ctx, establishing the fixture and the harness
	// report the timeout at all before the cancelled case below is read as
	// meaning anything. Without this, a harness bug that always reports
	// "timed_out" regardless of ctx would pass the cancelled case too, for a
	// reason that has nothing to do with AwaitPromise.
	var liveCallCount int
	t.Run("live ctx reaches the timeout branch", func(t *testing.T) {
		result, caller := run(t, context.Background())
		if result != "timed_out" {
			t.Fatalf("result = %q, want the timed_out branch", result)
		}
		assertAllReportTimedOut(t, caller.calls)
		liveCallCount = len(caller.calls)
	})

	// The question this test exists to answer: an ALREADY-CANCELLED ctx
	// passed to the resuming Replay call. If AwaitPromise's timeout depended
	// on ctx reaching the host call the way durablecalls.go's backoff wait
	// does, this ctx being pre-cancelled could plausibly make the resume
	// error out immediately, or report timed-out for the wrong reason
	// (ctx.Err() rather than the wall clock), or behave unpredictably. None
	// of that is possible if the reading above is right: the deadline check
	// is time.Now() against a value already in history, and ctx is not
	// consulted at all.
	t.Run("already-cancelled ctx reaches the same result", func(t *testing.T) {
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()

		result, caller := run(t, cancelledCtx)
		if result != "timed_out" {
			t.Fatalf("result = %q, want the timed_out branch even with an already-cancelled resume ctx -- "+
				"AwaitPromise's timeout is a wall-clock deadline check (engine/promises.go), "+
				"not a select on ctx.Done()", result)
		}
		assertAllReportTimedOut(t, caller.calls)
		if got := len(caller.calls); got != liveCallCount {
			t.Errorf("cancelled-ctx run made %d calls, live-ctx run made %d -- "+
				"a cancelled resume ctx changed how many times the guest reached the "+
				"timed_out branch, which is exactly the kind of divergence this test "+
				"exists to catch", got, liveCallCount)
		}
	})
}

// assertAllReportTimedOut checks the calls the resuming Replay pass made: only
// the genuine timeout reaches the guest's "if timedOut { DurableCall(report,
// timed_out) }" branch, so there is one, and it is report.timed_out in both the
// live-ctx and cancelled-ctx runs alike. The fresh pass makes none (cleat#3383).
func assertAllReportTimedOut(t *testing.T, calls []CallRecord) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatalf("expected at least one report.timed_out call, got none")
	}
	for _, c := range calls {
		if c.Service != "report" || c.Op != "timed_out" {
			t.Errorf("call = %s.%s, want report.timed_out", c.Service, c.Op)
		}
	}
}
