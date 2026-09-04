//go:build cgo

package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestATenantsOwnRetryBudgetDecidesWhereThePolicyRuns is the per-tenant half of
// §3.94 step 4, and it is the assertion the section's "what to be suspicious
// of" list asks for by name: "a per-tenant limit that is never exercised ...
// written, read, and then ignored because the value reaches the executor but
// not the backend".
//
// Everything else about step 4 exercises the DEFAULT budget, where a green run
// is equally consistent with the tenant's value being read and discarded. This
// runs one identical workflow twice -- same fixture, same export, same 1ms
// policy -- changing nothing but the tenant's settings row, and requires the
// two runs to take different paths.
//
// The policy must stay SHORT. A long one is refused for both tenants, so the
// test would pass without the tenant's value ever being read.
//
// # What the two paths look like
//
// Host loop: no suspension, both attempts in this segment, and a terminal error
// carrying "retries exhausted" -- the whole of the worker's dead-letter
// predicate, minted only by the host loop's exhaustion path.
//
// SDK loop: one attempt, then DurableSleep ends the segment. Measured: the run
// suspends with reason cleat_sleep(1ms) and one dispatched op. A 1ms backoff
// does suspend -- an earlier draft of this test assumed it would not, on the
// theory that the deadline was already past, and asserted on the terminal
// message instead. That draft failed against a correct implementation, because
// there is no terminal error at all on this path: the workflow has not finished.
func TestATenantsOwnRetryBudgetDecidesWhereThePolicyRuns(t *testing.T) {
	const hostLoopPrefix = "retries exhausted"

	// The control. No settings store, so the engine falls back to
	// DefaultHostRetryBudget and this policy is far inside it.
	//
	// Without this half the assertion below would pass against a build that
	// refused EVERY policy -- which is the failure that would make the tenant's
	// value look load-bearing when it was not.
	t.Run("default budget runs the policy on the host", func(t *testing.T) {
		wasmBytes, eng, caller := retryBackoffEngine(t, "wf-tenant-retry-default", nil)
		_, _, susp, _, _, execErr := eng.Execute(context.Background(), wasmBytes,
			"defer_on_retries_exhausted", json.RawMessage(`{}`))

		if susp != nil {
			t.Fatalf("the run suspended (%s); with no tenant settings a 1ms "+
				"policy must run on the host in one segment", susp.Reason)
		}
		if execErr == nil || !strings.Contains(execErr.Error(), hostLoopPrefix) {
			t.Fatalf("terminal error is %v, want one containing %q -- the host "+
				"loop's exhaustion path. If this is gone the host is refusing "+
				"policies it should accept, and the other half of this test "+
				"proves nothing.", execErr, hostLoopPrefix)
		}
		if len(caller.ops) != 3 {
			t.Fatalf("%d ops dispatched, want 3 (two attempts + the defer): %v",
				len(caller.ops), caller.ops)
		}
	})

	// The same policy, for a tenant whose budget is below its worst-case
	// backoff. 1ns is not expressible through the ms-granularity settings
	// column; the fake store returns a TenantSettings directly, and the point
	// is the COMPARISON rather than the value.
	// The same policy, for a tenant whose budget is below its worst-case
	// backoff. 1ns is not expressible through the ms-granularity settings
	// column; the fake store returns a TenantSettings directly, and the point
	// is the COMPARISON rather than the value.
	//
	// The clock is PINNED, and that is the fix for the race this test shipped
	// with. DurableSleep anchors on the last recorded event's timestamp, so
	// under a real clock whether a 1ms backoff suspends depends on how much
	// wall time has passed since it -- the shape #612 records and CLAUDE.md
	// warns about. The first version demanded a suspension: it suspended on the
	// machine it was written on, did not in CI, and so merged green and then
	// failed. Measuring one machine is not measuring a property.
	//
	// Widening a window would not fix that; removing the dependence does. With
	// the clock frozen the deadline is always ahead of now, so the SDK loop
	// always suspends and the assertion is exact.
	//
	// Note what does NOT depend on the clock: the refusal itself. A 1ns budget
	// against 1ms of worst-case backoff is decided by arithmetic in
	// retryPolicyFitsBudget before any call is made. So this one subtest is
	// sufficient evidence that the tenant's value was read and applied; the
	// in-segment shape the real clock sometimes produced is the same refusal
	// wearing different clothes, not a second thing to prove.
	tight := []EngineOption{
		WithWorkflowStore(&fakeSettingsStore{
			settings: TenantSettings{HostRetryBudget: time.Nanosecond},
		}),
	}

	t.Run("refused, and the backoff suspends", func(t *testing.T) {
		// Frozen: the 1ms deadline is always ahead of now, so the sleep
		// suspends. This is the same device TestALongRetryPolicySuspends uses.
		const t0 int64 = 1_700_000_000_000
		wasmBytes, eng, caller := retryBackoffEngine(t, "wf-tenant-retry-tight-frozen",
			func() int64 { return t0 }, tight...)
		_, _, susp, _, _, execErr := eng.Execute(context.Background(), wasmBytes,
			"defer_on_retries_exhausted", json.RawMessage(`{}`))

		if susp == nil {
			t.Fatalf("the run did not suspend (err=%v).\n\n"+
				"With the clock frozen the SDK loop's backoff deadline is always "+
				"ahead of now, so it must suspend. Not suspending means the host "+
				"ran the policy despite this tenant's 1ns budget: either "+
				"Engine.hostRetryBudget is not reading the tenant's row, or "+
				"DurableCallWithRetry is not consulting it -- 3.94's 'value "+
				"reaches the executor but not the backend', in the retry path.",
				execErr)
		}
		if !strings.Contains(susp.Reason, "cleat_sleep") {
			t.Fatalf("suspension reason is %q, want one naming cleat_sleep -- "+
				"the SDK-level loop backs off with a durable sleep", susp.Reason)
		}
		if len(caller.ops) != 1 {
			t.Fatalf("%d ops dispatched before suspending, want 1: %v.\n\n"+
				"One attempt then a suspension is the SDK loop; two would mean "+
				"the host ran the policy.", len(caller.ops), caller.ops)
		}
	})

}
