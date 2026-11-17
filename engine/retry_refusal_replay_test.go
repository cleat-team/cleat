//go:build cgo

package engine

// cleat#4032. A retry policy the host refuses as too long for one segment must be refused on every segment in which the guest
// asks about it -- not only the first. Before the fix, `DurableCallWithRetry` consulted history before the budget, so a replay
// of a workflow that had fallen back to the SDK's own loop was answered by `replayCall` with the FIRST FAILED ATTEMPT: an error
// the policy existed to retry.
//
// The shape below is the one that exposes it, and each part is needed:
//
//   - a tenant budget of 1ns, so a 2-attempt 1ms policy is refused before any call is made (`retryPolicyFitsBudget` is
//     arithmetic and consults no clock, so this half is exact);
//   - a caller that fails ONE call and then succeeds, because a caller that always fails makes "the second attempt ran"
//     unobservable;
//   - a replay LOOP rather than one call, because a 1ms backoff suspends or not depending on how long the machine takes between
//     recording the failed call and evaluating the deadline (`tenant_retry_budget_test.go` pins a clock to decide that question
//     deliberately; here it is not the question -- the question is what the replay ANSWERS, and the loop reaches it on either
//     side of the race);
//   - and the replay run twice, once at the budget that refused and once at a budget under which the policy FITS. The second is
//     the case that decides the design: the tenant's budget is writable while a run is in flight, so a fix that re-asks the
//     budget instead of reading the recorded shape reproduces the original defect in the other direction. Measured on the
//     hoist-only fix: 1ns then 1h replays into "durable call always-fails.op: [2] the service was briefly unavailable".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mutableBudgetStore is an engine.WorkflowStore whose tenant-retry budget the test changes between segments, which is what
// TenantSettings.HostRetryBudget being writable through the worker API means for a run in flight.
type mutableBudgetStore struct {
	WorkflowStore
	budgetNS atomic.Int64
}

func (m *mutableBudgetStore) GetTenantSettings(context.Context) (TenantSettings, error) {
	return TenantSettings{HostRetryBudget: time.Duration(m.budgetNS.Load())}, nil
}

// failFirstCallThenSucceed fails the first call to `failService` and answers every later one.
type failFirstCallThenSucceed struct {
	failService string
	err         error
	calls       atomic.Int32
}

func (c *failFirstCallThenSucceed) Call(_ context.Context, service, _, _ string) (string, error) {
	if service != c.failService {
		return `{"ok":true}`, nil
	}
	if c.calls.Add(1) == 1 {
		return "", c.err
	}
	return `{"ok":true}`, nil
}

func (c *failFirstCallThenSucceed) attempts() int { return int(c.calls.Load()) }

// replayUntilFinished replays until the workflow neither suspends nor fails, and returns the result and any error. Bounded, so a
// workflow that never finishes fails the test rather than hanging it.
func replayUntilFinished(t *testing.T, eng *Engine, ctx context.Context, wasm []byte, entry string, history []EventRecord) (string, error) {
	t.Helper()
	var lastErr error
	for i := 0; i < 50; i++ {
		result, next, susp, _, _, err := eng.Replay(ctx, wasm, entry, json.RawMessage(`{}`), history)
		if err != nil {
			return "", err
		}
		if susp == nil {
			return result, nil
		}
		history = next
		lastErr = fmt.Errorf("still suspending after %d replays: %s", i+1, susp.Reason)
		time.Sleep(20 * time.Millisecond)
	}
	return "", lastErr
}

// TestARefusedRetryPolicyResumesWithItsNextAttempt is cleat#4032's regression test.
//
// The workflow's policy is refused on the first segment, so the Go SDK runs it itself: one attempt fails, and the 1ms backoff
// ends the segment. On the replay the guest asks the host the same question -- "will you run this policy?" -- and must get the
// same REFUSAL, whatever the budget says now, because the guest's own recorded attempt is sitting where the host's retry
// records would be.
func TestARefusedRetryPolicyResumesWithItsNextAttempt(t *testing.T) {
	cases := []struct {
		name     string
		replay   time.Duration // what the replay's worker resolves
		expectNS int64         // asserted, so a case cannot silently measure the other one
	}{
		// The budget that refused, unchanged. This is the case the defect was found with.
		{"replayed at the same budget", time.Nanosecond, int64(time.Nanosecond)},
		// The budget WIDENED between segments, which is allowed. The ENGINE clamps a tenant's value to the operator's
		// ceiling (DefaultHostRetryBudget, 60s), so the resolved budget is 1m, not 1h -- the point is only that the policy
		// now FITS. A fix that asks the budget again instead of reading the recorded shape then sends the replay down the
		// host path and returns the recorded first failure.
		{"replayed at a budget the policy fits", time.Hour, int64(DefaultHostRetryBudget)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			// TWO ENGINES, one per segment, because an engine KEEPS the tenant settings it read on its first
			// successful lookup (`Engine.tenantSettings`, "kept after the first success"). A run whose worker is
			// replaced between segments is therefore the only way a replay can observe a changed budget -- which is
			// also what a real replay is, and why the budget is tenant state rather than a property of the run.
			caller := &failFirstCallThenSucceed{failService: "always-fails", err: errors.New("the service was briefly unavailable")}
			newEngine := func(budget time.Duration) *Engine {
				settings := &mutableBudgetStore{}
				settings.budgetNS.Store(int64(budget))
				return NewEngine(rt, caller,
					WithBackends(WasmtimeLanguages, wt),
					WithWorkflowID("wf-4032-refused-replay"),
					WithWorkflowStore(settings))
			}
			eng := newEngine(time.Nanosecond)

			_, history, susp, _, _, err := eng.Execute(ctx, wasmBytes, "defer_on_retries_exhausted", json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("the first segment failed rather than suspending: %v", err)
			}
			// The precondition, and the reason the replay measures anything: the host refused, so the SDK's loop ran and
			// recorded an ordinary call. Without this the replay below is not a fallback replay at all.
			if susp == nil {
				t.Logf("the first segment did not suspend: the 1ms backoff was already past its deadline. The replay below is "+
					"still the measurement; the SDK loop ran either way, because the budget refused the policy (attempts: %d)",
					caller.attempts())
			} else if !strings.Contains(susp.Reason, "cleat_sleep") {
				t.Fatalf("suspension reason is %q, want one naming cleat_sleep -- the SDK loop's backoff", susp.Reason)
			}
			if got := caller.attempts(); got > 1 {
				t.Fatalf("%d attempt(s) in the first segment, want at most 1: more would mean the HOST ran the policy", got)
			}

			// The replay happens on a worker that resolves a different budget, measured rather than assumed:
			resumed := newEngine(tc.replay)
			if got := resumed.hostRetryBudget(ctx); int64(got) != tc.expectNS {
				t.Fatalf("the replay engine resolved a budget of %s, want %s; this test would then measure the wrong thing",
					got, time.Duration(tc.expectNS))
			}

			_, err = replayUntilFinished(t, resumed, ctx, wasmBytes, "defer_on_retries_exhausted", history)
			if err != nil {
				t.Fatalf("the replay FAILED with %v.\n\n"+
					"That is cleat#4032: the guest re-entered the host's retry entry point, the host answered from HISTORY "+
					"(replayCall returns the recorded first attempt) instead of refusing the policy again, so the SDK's loop "+
					"was never entered and the workflow died on the error it was retrying.", err)
			}
			if got := caller.attempts(); got != 2 {
				t.Errorf("%d attempt(s) in total, want 2 -- the policy is MaxAttempts 2 and the second must have run", got)
			}
		})
	}
}

// TestAReplayOfASuccessfulHostRetryDoesNotRefuseIt never gets a refusal to replay, because the policy fits -- and that is
// the point: a replay must answer the way the recording segment did, for the commonest case there is, a host-run policy whose
// FIRST attempt succeeded.
//
// It goes through the LOW-LEVEL entry (`DurableCallWithRetry`), which the Go SDK documents as having no fallback, and which is
// what the AssemblyScript, Java and Python SDKs have: for them a refusal is a hard failure. So this is the test that a
// shape-only rule breaks -- a lone plain `call` record is exactly what this success leaves behind, and refusing it turns a
// successful call into a failed workflow on replay.
func TestAReplayOfASuccessfulHostRetryDoesNotRefuseIt(t *testing.T) {
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
	caller := &alwaysSucceedsCaller{}
	eng := NewEngine(rt, caller, WithBackends(WasmtimeLanguages, wt), WithWorkflowID("wf-4032-success-replay"))

	result, history, susp, _, _, err := eng.Execute(ctx, wasmBytes, "retry_on_host_first_attempt_succeeds", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("the first segment failed: %v", err)
	}
	if susp != nil {
		t.Fatalf("the first segment suspended (%s); the policy fits the default budget and the first attempt succeeds", susp.Reason)
	}
	if len(history) != 1 || history[0].EventType != EventTypeCall {
		t.Fatalf("history is %v, want exactly one call record: a host-run policy whose first attempt succeeded writes nothing else", history)
	}
	if got := caller.calls.Load(); got != 1 {
		t.Fatalf("%d attempt(s) on the first segment, want 1", got)
	}

	replayed, _, susp2, _, _, err := eng.Replay(ctx, wasmBytes, "retry_on_host_first_attempt_succeeds", json.RawMessage(`{}`), history)
	if err != nil {
		t.Fatalf("the replay FAILED with %v.\n\n"+
			"A plain call record is what this success leaves behind. Refusing it on replay turns a successful call into a "+
			"hard failure for every guest without a code-6 fallback -- AssemblyScript, Java and Python have none.", err)
	}
	if susp2 != nil {
		t.Fatalf("the replay suspended (%s); the recorded call answers it", susp2.Reason)
	}
	if replayed != result {
		t.Errorf("the replay answered %q, want the first execution's %q", replayed, result)
	}
	if got := caller.calls.Load(); got != 1 {
		t.Errorf("the service was called %d time(s) across both segments, want 1: the recorded call must be replayed", got)
	}
}

// alwaysSucceedsCaller answers every call.
type alwaysSucceedsCaller struct{ calls atomic.Int32 }

func (c *alwaysSucceedsCaller) Call(context.Context, string, string, string) (string, error) {
	c.calls.Add(1)
	return `{"ok":true}`, nil
}
