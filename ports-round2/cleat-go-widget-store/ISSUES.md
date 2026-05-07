# Issues Found During Porting

## Issue 1: TestEnv does not set WorkflowID in HostCallsOptions

- **Severity:** High
- **File:** `durable/durabletest/durabletest.go` line 233-263
- **Problem:** `NewTestEnv()` creates a `HostCalls` via `NewHostCalls(HostCallsOptions{...})` but does not set the `WorkflowID` field. As a result, `h.WorkflowID()` always returns an empty string in tests.
- **Expected:** The test environment should allow setting a workflow ID, either via a `SetWorkflowID` method or by passing an option to `NewTestEnv`.
- **Workaround used:** Wrapped the `HostCalls` with a struct that overrides `WorkflowID()` to return a test-specific value:
  ```go
  type workflowIDWrapper struct {
      durable.HostCalls
      wfID string
  }
  func (w *workflowIDWrapper) WorkflowID() string {
      return w.wfID
  }
  ```
- **Recommendation:** Add `WorkflowID` to the `HostCallsOptions` passed by `NewTestEnv`, and add a `SetWorkflowID(wfID string)` method on `TestEnv`.

---

## Issue 2: DurableSleep does not return an error

- **Severity:** Medium
- **File:** `durable/runtime.go` line 1031-1033 vs DBOS `Sleep(ctx, duration) (time.Duration, error)`
- **Problem:** DBOS's `Sleep` returns `(time.Duration, error)`, allowing callers to handle cancellation/timeout errors. Cleat's `DurableSleep` takes a duration and returns nothing (`func(d time.Duration)`). If sleep is interrupted (e.g., cancellation), the workflow has no way to detect it.
- **Expected:** `DurableSleep` should return an error so workflows can respond to cancellation during sleep.
- **Workaround used:** N/A — calls must tolerate non-returning sleep.
- **Recommendation:** Change the signature to `DurableSleep(d time.Duration) error`, or provide a `SleepCtx` variant that returns context cancellation errors.

---

## Issue 3: Saga only compensates on TerminalError, not on any error

- **Severity:** Medium
- **File:** `durable/runtime.go` lines 1638-1673
- **Problem:** The Saga's `Run()` method checks `IsTerminalError(err)` before compensating. For non-terminal errors, the saga returns the error immediately without compensating completed steps. This differs from Temporal's default behavior where any step failure triggers compensation of all prior steps. If a step fails with a regular error (e.g., a downstream service returns an invalid response), previously completed steps are NOT compensated.
- **Expected:** By default, Saga should compensate on any step failure. `TerminalError` should be an optimization for non-retryable failures.
- **Workaround used:** Wrapped errors in `durable.NewTerminalError()` to force compensation. This is error-prone — callers must remember to wrap errors.
- **Recommendation:** Change the default Saga behavior to compensate on all errors. Add an option (e.g., `SagaWithOnlyTerminalTriggersCompensation()`) for the current behavior.

---

## Issue 4: No SetEvent/GetEvent for external communication

- **Severity:** Medium
- **Context:** DBOS provides `SetEvent`/`GetEvent` for publishing events that can be read by external HTTP handlers. Cleat has no direct equivalent for publishing events bound to a workflow that external code can wait on.
- **Expected:** A mechanism for external HTTP handlers to wait for workflow state changes (e.g., a `WaitForQueryState(key, timeout)` runtime API).
- **Workaround used:** Used `SetQueryState` to publish key-value pairs, with the understanding that external retrieval requires the Cleat runtime's query API.
- **Recommendation:** Consider adding a `WorkflowEvent` primitive that externally-listenable and timeout-aware, similar to DBOS's `GetEvent`.

---

## Issue 5: ChildWorkflow in TestEnv does not execute the child function

- **Severity:** Medium
- **File:** `durable/durabletest/durabletest.go` lines 617-638
- **Problem:** When a workflow calls `h.ChildWorkflow(name, inputJSON)` in the test environment, the child workflow is not actually executed. Instead, a stubbed result is returned immediately. The `RegisterChildWorkflow` method allows registering a handler, but the handler runs synchronously within the childWorkflowImpl, not as an independent workflow execution.
- **Expected:** The test environment should support executing real child workflow functions, not just stubbed results. This is important for testing parent-child coordination.
- **Workaround used:** Used `OnChildWorkflow(name).Return(result, err)` for stubbing. For children that need "real" execution, `RegisterChildWorkflow` runs the handler synchronously.
- **Recommendation:** Add a mechanism to register real child workflow functions and execute them as independent (but simulated) workflow runs.

---

## Issue 6: DurableDefer takes string, DurableDeferFunc exists but needs separate registration

- **Severity:** Low
- **File:** `durable/runtime.go` lines 1091-1103
- **Context:** `DurableDefer(description string)` takes only a description string, not a closure. `DurableDeferFunc(fn func())` exists separately. Both need to be registered in `HostCallsOptions` for the test env.
- **Expected:** `DurableDefer` should accept a closure directly (like Go's `defer`), with a `DurableDeferDescription` for logging.
- **Workaround used:** Used `DurableDeferFunc` where a closure was needed. The test case uses `DurableDefer` with a string for the simpler case.
- **Recommendation:** Make `DurableDefer` accept `func()` and keep a separate `DurableDeferDescription` for informational logging, or rename `DurableDeferFunc` to `DurableDefer` and deprecate the string variant.

---

## Issue 7: DurableDefer not set up in TestEnv

- **Severity:** Low
- **File:** `durable/durabletest/durabletest.go` line 233-263
- **Problem:** `NewTestEnv` sets up `DurableDefer` but NOT `DurableDeferFunc`. If a workflow uses `h.DurableDeferFunc(fn)`, it fails with "not initialized" in tests.
- **Expected:** Both `DurableDefer` and `DurableDeferFunc` should be wired up in the test environment.
- **Workaround used:** Used `DurableDefer` (string variant) which works in tests.
- **Recommendation:** Add `DurableDeferFunc` to the `HostCallsOptions` in `NewTestEnv`.

---

## Issue 8: DurableCallTyped in TestEnv requires nil matcher for JSON-encoded requests

- **Severity:** Low
- **File:** `durable/durabletest/durabletest.go` lines 467-511
- **Problem:** When a workflow calls `h.DurableCallTyped("store", "createOrder", &struct{}{}, &orderID)`, the request gets JSON-marshaled to `"{}"` before being dispatched to the `DurableCall` implementation. The test env matches stubs against this JSON string. If the matcher is an exact string, it must match the JSON encoding exactly (e.g., `"{}"` for an empty struct, `"1"` for the integer 1).
- **Expected:** `DurableCallTyped` should be wired directly in `TestEnv` rather than relying on the JSON fallback through `DurableCall`. This would allow typed stubs (e.g., `env.OnCallTyped("store", "createOrder").Return(1, nil)`).
- **Workaround used:** Used `nil` matchers throughout, which match any request payload.
- **Recommendation:** Add `DurableCallTyped` to the `TestEnv` `HostCallsOptions`, and provide `OnCallTyped` methods that accept typed request/response values.

---

## Issue 9: No direct equivalent to DBOS's `RunAsStep` for idempotent operations

- **Severity:** Low
- **Context:** DBOS's `RunAsStep` wraps arbitrary closures as durable, replay-safe steps. Cleat requires operations to be modeled as `DurableCall` to a service. There is no mechanism to wrap a plain Go function as a replay-safe step without going through the service abstraction.
- **Expected:** A utility like `h.RunStep(description string, fn func() (string, error))` that records the function call and result in the journal, replaying it from cache on re-execution.
- **Workaround used:** Called `DurableCallTyped` with service/operation names for each operation. For simple lookups this adds JSON marshaling overhead.
- **Recommendation:** Consider adding a `RunStep` helper that records arbitrary function calls in the journal, with replay-safe execution.

---

## Issue 10: ChildWorkflow does not support typed input/output in the base API

- **Severity:** Low
- **File:** `durable/runtime.go` lines 1168-1182
- **Context:** `h.ChildWorkflow(name, inputJSON)` takes a JSON string for input and returns `(string, error)` for output. The typed variants `ChildWorkflowTyped` and `AwaitChildTyped` exist but require explicit registration in `HostCallsOptions`. In the test env, they use marshaling fallbacks but are not directly wired.
- **Expected:** The base `ChildWorkflow` method should have a typed variant (using generics) that handles marshaling automatically, similar to `DurableCallTyped`.
- **Workaround used:** Used `h.ChildWorkflow(name, inputJSON)` with `strconv.Itoa(orderID)` for the order ID.
- **Recommendation:** Add generic `ChildWorkflow[TReq, TResp]` method, or make `ChildWorkflowTyped` available without needing an explicit host function.

---

## Summary

| # | Severity | Area | Summary |
|---|----------|------|---------|
| 1 | **High** | TestEnv | WorkflowID not configurable in tests |
| 2 | **Medium** | SDK | DurableSleep doesn't return error |
| 3 | **Medium** | SDK | Saga only compensates on TerminalError |
| 4 | **Medium** | SDK | No SetEvent/GetEvent for external communication |
| 5 | **Medium** | TestEnv | ChildWorkflow doesn't execute child function |
| 6 | **Low** | SDK | DurableDefer takes string, not closure |
| 7 | **Low** | TestEnv | DurableDeferFunc not wired up |
| 8 | **Low** | TestEnv | DurableCallTyped matchers use JSON strings |
| 9 | **Low** | SDK | No RunStep for plain function wrapping |
| 10 | **Low** | SDK | ChildWorkflow lacks typed generics variant |
