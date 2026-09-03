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
	t.Run("a tenant budget below the policy pushes it off the host", func(t *testing.T) {
		wasmBytes, eng, caller := retryBackoffEngine(t, "wf-tenant-retry-tight", nil,
			WithWorkflowStore(&fakeSettingsStore{
				settings: TenantSettings{HostRetryBudget: time.Nanosecond},
			}))
		_, _, susp, _, _, execErr := eng.Execute(context.Background(), wasmBytes,
			"defer_on_retries_exhausted", json.RawMessage(`{}`))

		if susp == nil {
			t.Fatalf("the run did not suspend (err=%v).\n\n"+
				"This tenant's host-retry budget is 1ns, below this policy's "+
				"worst-case backoff, so the host must refuse it with "+
				"callErrorCode 6 and the guest must run the policy itself, "+
				"suspending on its first backoff. Not suspending means the "+
				"refusal did not happen: either Engine.hostRetryBudget is not "+
				"reading the tenant's row, or DurableCallWithRetry is not "+
				"consulting it. That is 3.94's 'value reaches the executor but "+
				"not the backend' failure, in the retry path.", execErr)
		}
		if !strings.Contains(susp.Reason, "cleat_sleep") {
			t.Fatalf("suspension reason is %q, want one naming cleat_sleep -- "+
				"the SDK-level loop backs off with a durable sleep", susp.Reason)
		}
		// One attempt, then the segment ended. The host loop would have
		// dispatched both, which is what the control asserts.
		if len(caller.ops) != 1 {
			t.Fatalf("%d ops dispatched, want 1: %v.\n\n"+
				"One attempt then a suspension is the SDK-level loop; two would "+
				"mean the host ran the policy despite the tenant's budget.",
				len(caller.ops), caller.ops)
		}
	})
}

// TestATenantCannotRaiseItsRetryBudgetPastTheOperatorsCeiling is the clamp, at
// the resolver rather than through a workflow.
//
// The clamp is what makes a tenant-writable settings table safe at all: a
// tenant that could RAISE this value could hold a shared worker slot for as
// long as it liked, and the operator's flag would be advisory. ClampToCeiling
// has its own unit tests; this asserts that the retry path is wired to it, in
// the direction that matters.
func TestATenantCannotRaiseItsRetryBudgetPastTheOperatorsCeiling(t *testing.T) {
	const ceiling = 30 * time.Second

	cases := []struct {
		name   string
		tenant time.Duration
		want   time.Duration
	}{
		{"tenant lowers it", 5 * time.Second, 5 * time.Second},
		{"tenant tries to raise it", 10 * time.Minute, ceiling},
		{"tenant sets nothing", 0, ceiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(nil, nil,
				WithHostRetryBudget(ceiling),
				WithWorkflowStore(&fakeSettingsStore{
					settings: TenantSettings{HostRetryBudget: tc.tenant},
				}),
			)
			if got := e.hostRetryBudget(context.Background()); got != tc.want {
				t.Fatalf("hostRetryBudget = %v, want %v (operator ceiling %v, tenant %v)",
					got, tc.want, ceiling, tc.tenant)
			}
		})
	}
}

// TestTheDefaultRetryBudgetAppliesWhenNoOperatorCeilingIsSet pins the
// compatibility guarantee that lets step 4 claim it changed no behaviour.
//
// Every engine built without WithHostRetryBudget -- every test in this
// package, cleattest, wasmtest, embedded -- must keep the 60s threshold the two
// SDKs used to compile in. If this returned 0 it would read as "unbounded" and
// every policy would run on the host, which is the pre-3.88 behaviour that made
// an hour-long backoff hold a worker for an hour.
func TestTheDefaultRetryBudgetAppliesWhenNoOperatorCeilingIsSet(t *testing.T) {
	e := NewEngine(nil, nil)
	if got := e.hostRetryBudget(context.Background()); got != DefaultHostRetryBudget {
		t.Fatalf("hostRetryBudget = %v with no ceiling configured, want %v",
			got, DefaultHostRetryBudget)
	}
}
