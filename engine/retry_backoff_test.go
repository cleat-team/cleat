//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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

// TestAnSDKRetryBackoffSuspendsTheWorkflow pins the property.
//
// One attempt, then a suspension whose reason names the sleep. This is what an
// operator's 3-attempt policy actually costs: not one segment with two waits in
// it, but three segments, each replaying the history so far.
//
// The clock is pinned BEHIND the anchor rather than left real -- see the header.
// The anchor is the failed call's own event timestamp, stamped from real time
// regardless of this clock, so a deadline of "anchor + 1ms" sits about three
// years ahead of t0 and the sleep cannot complete for timing reasons. The
// sibling test pins the clock a day AHEAD of real time so every backoff
// completes. Same fixture, same policy, opposite sides of the deadline.
func TestAnSDKRetryBackoffSuspendsTheWorkflow(t *testing.T) {
	const t0 int64 = 1_700_000_000_000
	wasmBytes, eng, caller := retryBackoffEngine(t, "wf-retry-backoff",
		func() int64 { return t0 })

	res, _, susp, _, _, err := eng.Execute(context.Background(), wasmBytes,
		"defer_on_retries_exhausted", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if susp == nil {
		t.Fatalf("the run did not suspend; it returned %q.\n\n"+
			"An SDK-level retry backs off with a DURABLE sleep, so the first "+
			"backoff must end the segment. The clock here is pinned about three "+
			"years BEHIND the deadline, so this is not the 1ms timing race the "+
			"header describes -- do not reach for a longer backoff or a retry. "+
			"If the retry loop stopped using DurableSleep, that is an "+
			"improvement worth recording in IMPROVEMENT-PLAN 3.88, not a silent "+
			"change.", res)
	}
	if !strings.Contains(susp.Reason, "cleat_sleep") {
		t.Fatalf("suspension reason is %q, want one naming cleat_sleep.\n\n"+
			"Some other suspension would mean the backoff is not what ends the "+
			"segment, and the whole explanation in this file's header is wrong.",
			susp.Reason)
	}
	if len(caller.ops) != 1 {
		t.Fatalf("%d calls dispatched, want 1: %v.\n\n"+
			"More than one means the retry loop ran further than the first "+
			"backoff, which contradicts the suspension above.", len(caller.ops), caller.ops)
	}
}

// TestAnExhaustedRetryRunsItsDefersAndIsNotDeadLetterable is 3.88's step 2
// measurement, which step 1 was blocking.
//
// The clock is ahead of REAL time, not of a seeded start time -- see the header.
// That lets every backoff complete, so the policy exhausts inside one segment
// and the terminal path is observable in a single run.
//
// Two assertions, and they answer different questions. The cleanup call proves
// the defers ran on the terminal-failure path, which is what 3.75 asks about
// MoveToDeadLetterQueue. The absent substring proves this workflow would not
// have been dead-lettered at all.
func TestAnExhaustedRetryRunsItsDefersAndIsNotDeadLetterable(t *testing.T) {
	ahead := func() int64 { return time.Now().Add(24 * time.Hour).UnixMilli() }
	wasmBytes, eng, caller := retryBackoffEngine(t, "wf-retry-exhausted", ahead)

	_, _, susp, _, _, execErr := eng.Execute(context.Background(), wasmBytes,
		"defer_on_retries_exhausted", json.RawMessage(`{}`))

	if susp != nil {
		t.Fatalf("the run suspended with reason %q; a clock a day ahead of real "+
			"time must let every 1ms backoff complete.", susp.Reason)
	}
	if execErr == nil {
		t.Fatalf("the workflow succeeded; it was supposed to exhaust its policy")
	}

	// The cleanup ran, inside the segment, on the way out. Whatever the worker
	// does with the error afterwards, the defers are already done.
	found := false
	for _, op := range caller.ops {
		if op == "after_exhaustion" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the defer body's call was not made: %v.\n\n"+
			"A workflow reached a terminal failure without its cleanup running. "+
			"That would make IMPROVEMENT-PLAN 3.75's host-driven terminal sites "+
			"the smaller half of the problem and the guest wrapper's own drain "+
			"(3.70) the larger one.", caller.ops)
	}

	// And the gap 3.88 records. The worker dead-letters on
	// strings.Contains(errMsg, "retries exhausted") (cmd/cleat-worker/setup.go).
	// The engine mints that prefix only in its HOST-side retry loop, behind the
	// cleat_call_retry import -- which wasm/usage.go wires on the guest symbol
	// DurableCallWithRetry, and HostCallsImpl has no such method. So a Go
	// workflow exhausting a retry policy produces the SDK's own message and is
	// never dead-lettered.
	//
	// Asserted as the broken state deliberately: the day a Go guest can reach
	// the host retry loop, this fails and points at the section rather than the
	// change landing silently.
	const workerDeadLetterPredicate = "retries exhausted"
	if strings.Contains(execErr.Error(), workerDeadLetterPredicate) {
		t.Fatalf("the terminal error now contains %q: %v\n\n"+
			"That is the BEHAVIOUR WE WANT -- a Go workflow that exhausts its "+
			"retries becoming dead-letterable. Rewrite this to assert the "+
			"dead-letter path positively, and revisit IMPROVEMENT-PLAN 3.75's "+
			"MoveToDeadLetterQueue question, currently answered \"not a fourth "+
			"marker site, because nothing on this SDK reaches it\".",
			workerDeadLetterPredicate, execErr)
	}
	if !strings.Contains(execErr.Error(), "retry exhausted after 2 attempts") {
		t.Fatalf("the terminal error is %v.\n\n"+
			"Expected the SDK-level retry loop's own message. If the message "+
			"changed, check whether it now matches the worker's dead-letter "+
			"predicate above -- the two are joined by a string literal and "+
			"nothing else.", execErr)
	}
}
