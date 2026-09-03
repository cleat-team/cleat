//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The two SDKs disagree about what a retry policy means, and the disagreement
// is invisible from the API. IMPROVEMENT-PLAN 3.88.
//
// Rust's HostCalls::cleat_call_with_retry calls the cleat_call_retry import
// directly, so attempts, backoff and exhaustion all happen on the HOST inside
// one host call: one segment, one history event, the worker held for the
// duration. Go's DurableCallWithOptions cannot reach that import -- wasm/usage.go
// wires it on the guest symbol DurableCallWithRetry and HostCallsImpl defines no
// such method -- so it falls back to an SDK-level loop that backs off with a
// durable sleep, making an N-attempt policy N segments
// (engine/retry_backoff_test.go).
//
// This file measures the Rust half. 3.88 had it as a reading of the call sites,
// which is what the section says it was: "A Go guest does not appear able to
// reach it... Rust's HostCalls::cleat_call_with_retry calls the import directly,
// so the substring is reachable there." Reading a call chain is how this repo
// has been wrong before, and the chain here crosses a language boundary and an
// ABI, so it is worth running.
//
// It also settles a prerequisite rather than just a curiosity. The product
// answer recorded in 3.88 is that a retry finishing within a few minutes should
// keep the worker instead of suspending -- which is the host loop's behaviour.
// Before adding the missing Go method, the question worth answering is whether
// the host path works end to end from a real guest at all. It does.

// alwaysFailsCounting fails one service and counts how many times it was asked.
// The count is the load-bearing observable: it separates "the host looped" from
// "the guest looped and each attempt was its own segment".
type alwaysFailsCounting struct {
	service string
	calls   int
}

func (c *alwaysFailsCounting) Call(_ context.Context, service, _, _ string) (string, error) {
	if service == c.service {
		c.calls++
		return "", errors.New("service is down")
	}
	return `{"ok":true}`, nil
}

// TestARustGuestReachesTheHostRetryLoop pins all three consequences at once,
// because separately none of them distinguishes the two designs.
//
// Two dispatched attempts with no suspension is the host loop; the SDK loop
// would show one attempt and a suspension. And the terminal error carrying
// "retries exhausted" is what makes this workflow dead-letterable where the
// identical Go policy is not.
func TestARustGuestReachesTheHostRetryLoop(t *testing.T) {
	ctx := context.Background()
	wasmBytes := buildRustProbeWasm(t)

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

	caller := &alwaysFailsCounting{service: "always-fails"}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-rust-host-retry"))

	_, _, susp, _, _, execErr := eng.Execute(ctx, wasmBytes, "retry_probe",
		json.RawMessage(`{"user_id":"u","cart":[]}`))

	// One segment. The host loop backs off in-process with time.After, so
	// nothing suspends and the worker is held for the whole policy -- which is
	// the behaviour 3.88 records as wanted for short retries.
	if susp != nil {
		t.Fatalf("the run suspended with reason %q.\n\n"+
			"The host-side retry loop backs off in-process and does not suspend. "+
			"A suspension here means this guest fell back to an SDK-level loop, "+
			"and the whole Go/Rust divergence in IMPROVEMENT-PLAN 3.88 needs "+
			"re-measuring.", susp.Reason)
	}
	if execErr == nil {
		t.Fatalf("the workflow succeeded; it was supposed to exhaust its policy")
	}

	// Two attempts, both inside that one segment. This is the assertion that
	// tells the two designs apart: the Go path dispatches ONE call and then
	// suspends for its backoff.
	if caller.calls != 2 {
		t.Fatalf("%d attempts dispatched, want 2.\n\n"+
			"Two attempts with no suspension is the host loop running the whole "+
			"policy inside one host call. One attempt would mean this guest is "+
			"on the SDK-level path after all.", caller.calls)
	}

	// And the consequence for operators. This substring is the worker's entire
	// dead-letter predicate (cmd/cleat-worker/setup.go), and the engine mints it
	// in exactly one place -- the host loop's exhaustion path in
	// engine/durablecalls.go. Reaching it from a guest is what makes the
	// dead-letter queue a real destination on this SDK.
	const workerDeadLetterPredicate = "retries exhausted"
	if !strings.Contains(execErr.Error(), workerDeadLetterPredicate) {
		t.Fatalf("the terminal error is %v, which does not contain %q.\n\n"+
			"The two assertions above say the host loop ran, so its exhaustion "+
			"message should be here. If the message changed, the worker's "+
			"dead-letter predicate no longer matches anything and "+
			"MoveToDeadLetterQueue is unreachable from EVERY SDK -- check "+
			"engine/durablecalls.go against cmd/cleat-worker/setup.go, and see "+
			"engine.TestAnExhaustedRetryRunsItsDefersAndIsNotDeadLetterable for "+
			"the Go side of the same assertion, written the other way round.",
			execErr, workerDeadLetterPredicate)
	}
}
