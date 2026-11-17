//go:build cgo

package engine

// cleat#4032. A retry policy the host refuses as too long for one segment must be refused on EVERY segment, not only the
// first. Before the fix, `DurableCallWithRetry` consulted history before the budget, so a replay of a workflow that had fallen
// back to the SDK's loop was answered by `replayCall` with the FIRST FAILED ATTEMPT -- an error the policy was supposed to
// retry.
//
// The shape below is the one that exposes it, and each part is needed:
//
//   - a tenant budget of 1ns, so a 1ms policy is refused before any call is made (`retryPolicyFitsBudget` is arithmetic and
//     does not consult a clock, so this half is exact);
//   - a caller that fails ONE call and then succeeds, because a caller that always fails makes "the second attempt ran"
//     unobservable;
//   - a replay LOOP rather than one call, because a 1ms backoff suspends or not depending on how long the machine takes
//     between recording the failed call and evaluating the deadline (`tenant_retry_budget_test.go` pins a clock to decide that
//     question deliberately; here it is not the question -- the question is what the replay ANSWERS, and the loop reaches it on
//     either side of the race).

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

// replayUntilFinished replays until the workflow neither suspends nor fails, and returns the result and any error. Bounded, so
// a workflow that never finishes fails the test rather than hanging it.
func replayUntilFinished(t *testing.T, eng *Engine, ctx context.Context, wasm []byte, entry string, history []EventRecord) (string, error) {
	t.Helper()
	var lastErr error
	for i := 0; i < 50; i++ {
		result, next, susp, _, _, err := eng.Replay(ctx, wasm, entry, json.RawMessage(`{}`), history)
		if err != nil {
			return "", err
		}
		if susp == nil {
			return string(result), nil
		}
		history = next
		lastErr = fmt.Errorf("still suspending: %s", susp.Reason)
		time.Sleep(20 * time.Millisecond)
	}
	return "", lastErr
}

// failFirstCallThenSucceed fails the first call to `failService` and answers every later one.
type failFirstCallThenSucceed struct {
	failService string
	err         error
	calls       int32
}

func (c *failFirstCallThenSucceed) Call(_ context.Context, service, operation, _ string) (string, error) {
	if service != c.failService {
		return `{"ok":true}`, nil
	}
	if atomic.AddInt32(&c.calls, 1) == 1 {
		return "", c.err
	}
	return `{"ok":true}`, nil
}

func (c *failFirstCallThenSucceed) attempts() int { return int(atomic.LoadInt32(&c.calls)) }

// TestARefusedRetryPolicyResumesWithItsNextAttempt is cleat#4032's regression test.
//
// The workflow's policy is refused on the first segment, so the Go SDK runs it itself: one attempt fails, and the 1ms backoff
// ends the segment. On the replay the guest asks the host the same question -- "will you run this policy?" -- and the answer
// must be the same REFUSAL. If history is consulted first, the answer becomes the recorded first failure, the SDK's loop is
// never entered, and the workflow dies on the error it was retrying.
func TestARefusedRetryPolicyResumesWithItsNextAttempt(t *testing.T) {
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
	caller := &failFirstCallThenSucceed{failService: "always-fails", err: errors.New("the service was briefly unavailable")}
	eng := NewEngine(rt, caller,
		WithBackends(WasmtimeLanguages, wt),
		WithWorkflowID("wf-4032-refused-replay"),
		WithWorkflowStore(&fakeSettingsStore{settings: TenantSettings{HostRetryBudget: time.Nanosecond}}))

	_, history, susp, _, _, err := eng.Execute(ctx, wasmBytes, "defer_on_retries_exhausted", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("the first segment failed rather than suspending: %v", err)
	}
	// The precondition, and the reason this test can measure anything: the host refused, the SDK loop ran ONE attempt, and the
	// backoff ended the segment. Without this the replay below is not a fallback replay at all.
	if susp == nil {
		t.Logf("the first segment did not suspend: the 1ms backoff was already past its deadline. The replay below is "+
			"still the measurement; the SDK loop ran either way, because the budget refused the policy (ops so far: %d)",
			caller.attempts())
	} else if !strings.Contains(susp.Reason, "cleat_sleep") {
		t.Fatalf("suspension reason is %q, want one naming cleat_sleep -- the SDK loop's backoff", susp.Reason)
	}
	if got := caller.attempts(); got > 1 {
		t.Fatalf("%d attempt(s) in the first segment, want at most 1: more would mean the HOST ran the policy", got)
	}

	result, err := replayUntilFinished(t, eng, ctx, wasmBytes, "defer_on_retries_exhausted", history)
	if err != nil {
		t.Fatalf("the replay FAILED with %v.\n\n"+
			"That is cleat#4032: the guest re-entered the host's retry entry point, the host answered from HISTORY "+
			"(replayCall returns the recorded first attempt) instead of refusing the policy again, so the SDK's loop was "+
			"never entered and the workflow died on the error it was retrying.", err)
	}
	if got := caller.attempts(); got != 2 {
		t.Errorf("%d attempt(s) in total, want 2 -- the policy is MaxAttempts 2 and the second must have run", got)
	}
	// The defer's own call is the workflow's last recorded action, so a completed run proves the body got past the retry.
	if !strings.Contains(string(result), "ok") && string(result) != "" {
		t.Logf("workflow result: %.200s", result)
	}
}
