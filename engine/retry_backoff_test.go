//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// IMPROVEMENT-PLAN 3.88 step 1, settled.
//
// That section recorded a measurement it could not explain: a Go guest with a
// 2-attempt retry policy against an always-failing service made ONE attempt and
// suspended, and pinning WithWorkflowStartTime and WithClock did not change it.
// It said so rather than asserting a state whose mechanism was unknown.
//
// The mechanism is here, and it is not a defect -- it is a property nothing had
// written down. cleat/runtime.go's SDK-level retry loop backs off with
// `h.DurableSleep(backoff)`, a DURABLE sleep. DurableSleep suspends whenever its
// deadline is ahead of the engine's wall clock. So an SDK-level retry is not a
// loop inside one segment: every backoff suspends the workflow, and the retry
// resumes in the next segment after replay. A 3-attempt policy with 1s backoffs
// is three segments.
//
// SINCE 3.88's DECISION LANDED, that is the LONG-POLICY path only. A policy
// whose worst-case total backoff fits in cleat.hostRetryBudget now runs on the
// host instead: one segment, the worker held for the duration, the way
// non-durable code would do it. Which path a policy takes is decided before the
// first attempt, from the policy alone, and the two tests below pin both sides
// of that threshold. Either alone would pass against a build that always chose
// one path.
//
// Why the earlier clock pinning failed is the part worth keeping. DurableSleep's
// anchor is `max(session nowMs, Now())`, and Now() reads the LAST RECORDED
// EVENT's timestamp -- which the first attempt's call event has just written at
// real wall time (recordEvent stamps time.Now() directly, lifecycle.go:153, and
// assigns s.nowMs from it). That overrides any WithWorkflowStartTime seed in the
// past, so a clock pinned relative to the seed is behind the anchor and the
// sleep suspends anyway. To make a backoff complete, the clock has to be ahead
// of REAL time, not ahead of the seed. Both directions are asserted below,
// because the failing one is the one that cost a session.
//
// BOTH TESTS PIN THE CLOCK, and the first one did not when it was written. That
// cost a red CI run on #610 and is the more useful half of this file.
//
// The fixture's backoff is 1ms. The suspend decision is
// `s.nowMs <= realNowMs()` with s.nowMs = anchor + 1ms, so on the real clock
// the assertion below reduces to: did LESS than one millisecond of real time
// pass between recording the failed call event and evaluating the deadline?
// That is a race with a 1ms window, not a property. It suspended on the author's
// machine and on its own PR's CI, then completed under load on a later run,
// exhausted the policy in-segment, and failed with the message the OTHER test
// expects. Reproduced deterministically both ways before this fix:
//
//	clock real now +5ms   -> susp=false, "retry exhausted after 2 attempts", ops=[op op after_exhaustion]
//	clock pinned to t0    -> susp=true,  reason "cleat_sleep(1ms)",          ops=[op]
//
// So the pinned clock is not decoration. It replaces a 1ms margin of real time
// with a margin of years, and the pair below now differ only in which side of
// the deadline the engine's clock sits -- which is the mechanism, stated as two
// opposite assertions with no timing left in either. CLAUDE.md: if an assertion
// depends on wall-clock time, remove the timing rather than widening it.

// retryBackoffEngine builds a Go SDK guest on wasmtime whose only service fails.
// clock nil means the real wall clock.
func retryBackoffEngine(t *testing.T, wfID string, clock func() int64) ([]byte, *Engine, *failOnceRecordingCaller) {
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

	caller := &failOnceRecordingCaller{failService: "always-fails", err: errors.New("service is down")}
	opts := []EngineOption{
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID(wfID),
	}
	if clock != nil {
		opts = append(opts, WithClock(clock))
	}
	return wasmBytes, NewEngine(rt, caller, opts...), caller
}

// failOnceRecordingCaller fails one service and records every operation asked
// for, in order. The defer's own call must succeed, because "the cleanup ran" is
// what the test below reads off it and a recorded failure is weaker evidence.
type failOnceRecordingCaller struct {
	failService string
	err         error
	ops         []string
}

func (c *failOnceRecordingCaller) Call(_ context.Context, service, operation, _ string) (string, error) {
	c.ops = append(c.ops, operation)
	if service == c.failService {
		return "", c.err
	}
	return `{"ok":true}`, nil
}

// TestAShortRetryPolicyRunsOnTheHostInOneSegment is 3.88's decision, measured
// on the Go SDK.
//
// A retry finishing quickly should keep its worker rather than suspending --
// frequent, ordinary behaviour that should not be surprising. Three assertions,
// and they are the same three the Rust SDK has always satisfied
// (TestARustGuestReachesTheHostRetryLoop): no suspension, both attempts inside
// the one segment, and a terminal error the worker would dead-letter.
//
// The clock is NOT pinned, and that is not an oversight -- see this file's
// header and #612. The host loop backs off in-process with time.After and never
// consults the engine clock, so there is no deadline to race. That the pinning
// this test used to need is now unnecessary is itself evidence of which path
// ran.
func TestAShortRetryPolicyRunsOnTheHostInOneSegment(t *testing.T) {
	wasmBytes, eng, caller := retryBackoffEngine(t, "wf-retry-short", nil)

	_, _, susp, _, _, execErr := eng.Execute(context.Background(), wasmBytes,
		"defer_on_retries_exhausted", json.RawMessage(`{}`))

	if susp != nil {
		t.Fatalf("the run suspended with reason %q.\n\n"+
			"A 1ms-backoff policy is far inside cleat.hostRetryBudget, so it "+
			"must run on the host in one segment. A suspension means the "+
			"cleat_call_retry import is not wired -- check wasm/usage.go still "+
			"maps it on DurableCallWithOptions, since without that entry "+
			"cleat/runtime.go's host branch is unreachable.", susp.Reason)
	}
	if execErr == nil {
		t.Fatalf("the workflow succeeded; it was supposed to exhaust its policy")
	}

	// Both attempts, in this one segment. The SDK-level path dispatches ONE
	// call and then suspends for its backoff, which is what the long-policy
	// test below asserts -- so this count is what tells the two paths apart.
	if len(caller.ops) != 3 {
		t.Fatalf("%d operations dispatched, want 3 (two attempts + the defer's "+
			"cleanup): %v.\n\n"+
			"Two attempts in one segment is the host loop. One would mean the "+
			"SDK-level loop ran and suspended for its backoff.",
			len(caller.ops), caller.ops)
	}

	// And the consequence for operators, asserted positively now. This
	// substring is the worker's entire dead-letter predicate
	// (cmd/cleat-worker/setup.go), minted only by the host loop's exhaustion
	// path in engine/durablecalls.go. A Go workflow reaching it is what makes
	// the dead-letter queue a real destination on the tier-1 SDK -- it was not
	// one before 3.88, and docs/operations/workflow-retention.md said so.
	const workerDeadLetterPredicate = "retries exhausted"
	if !strings.Contains(execErr.Error(), workerDeadLetterPredicate) {
		t.Fatalf("the terminal error is %v, which does not contain %q.\n\n"+
			"The assertions above say the host loop ran, so its exhaustion "+
			"message should be here. If it is not, the dead-letter queue is "+
			"unreachable from this SDK again and "+
			"docs/operations/workflow-retention.md needs to go back to saying "+
			"so.", execErr, workerDeadLetterPredicate)
	}

	// The cleanup still ran on the way out. 3.75 asks whether a workflow can
	// reach a terminal state with defers outstanding; on this path it cannot.
	found := false
	for _, op := range caller.ops {
		if op == "after_exhaustion" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the defer body's call was not made: %v", caller.ops)
	}
}

// TestALongRetryPolicySuspendsInsteadOfHoldingTheWorker is the other side of
// the threshold, and the reason it exists.
//
// Three attempts two minutes apart is four minutes of waiting. Holding a worker
// for that is the opposite of what durable execution is for -- and it would not
// merely waste the worker, it would exceed --wasm-wall-clock-ceiling (5m by
// default, IMPROVEMENT-PLAN 3.90) and get the invocation killed, where
// suspending completes it. So this must take the SDK path.
//
// The clock IS pinned here, behind the deadline, for the reason #612 records:
// DurableSleep's anchor is the last recorded event's real timestamp, so a
// real-clock assertion about whether a backoff suspends is a race.
func TestALongRetryPolicySuspendsInsteadOfHoldingTheWorker(t *testing.T) {
	const t0 int64 = 1_700_000_000_000
	wasmBytes, eng, caller := retryBackoffEngine(t, "wf-retry-long",
		func() int64 { return t0 })

	_, _, susp, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_on_long_retry_policy", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if susp == nil {
		t.Fatalf("the run did not suspend.\n\n" +
			"Four minutes of backoff must not be run on the host: it holds a " +
			"worker for the duration and exceeds the wall-clock ceiling, which " +
			"kills the invocation. If cleat.hostRetryBudget was raised, raise " +
			"--wasm-wall-clock-ceiling with it and revisit both -- they are not " +
			"independent. IMPROVEMENT-PLAN 3.88 and 3.90.")
	}
	if !strings.Contains(susp.Reason, "cleat_sleep") {
		t.Fatalf("suspension reason is %q, want one naming cleat_sleep", susp.Reason)
	}
	if len(caller.ops) != 1 {
		t.Fatalf("%d calls dispatched, want 1: %v.\n\n"+
			"The SDK-level loop makes one attempt and suspends for its backoff. "+
			"More than one would mean the host loop took this policy after all.",
			len(caller.ops), caller.ops)
	}
}
