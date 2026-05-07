# Issues and Gaps Found During Porting

## 1. Missing `ParentWorkflowID` on HostCalls

**Severity**: Medium

**Description**: In Temporal, a child workflow can retrieve its parent's workflow ID via `workflow.GetInfo(ctx).ParentWorkflowExecution`. Cleat's `HostCalls` interface has `WorkflowID()` and `RunID()` but no equivalent for getting the parent workflow ID.

**Impact**: The child must receive the parent's workflow ID through its input payload. The port works around this by injecting `SingleRecord.ParentWorkflowID` into each child's input. This is workable but adds coupling between workflow stages and means every child workflow input type must include a parent ID field.

**Suggested fix**: Add `ParentWorkflowID() string` to the `HostCalls` interface.

## 2. `SignalWorkflow` Parameter Semantics Unclear (Workflow ID vs Run ID)

**Severity**: High

**Description**: The `HostCalls.SignalWorkflow(targetRunID, signalName, payload)` method's first parameter is named `targetRunID`, suggesting it requires a run ID. In Temporal, `SignalExternalWorkflow` accepts a workflow ID (and optionally a run ID), allowing signals to reach the current run even after ContinueAsNew changes the run ID. If Cleat's `SignalWorkflow` requires a run ID, signals will not survive `ContinueAsNew` boundaries.

**Impact**: If `SignalWorkflow` is run-ID-scoped, the signal-based child-to-parent coordination that makes the sliding window work across ContinueAsNew boundaries cannot function. Children from the old run cannot notify the new run of completion, breaking the cross-run sliding window.

**Workaround in the port**: The current implementation uses `SignalWorkflow` with the parent's `WorkflowID()`. If the runtime accepts a workflow ID (despite the parameter name), this works. The test environment ignores the target ID and delivers locally, so tests pass regardless.

**Suggested fix**: Clarify in documentation whether `SignalWorkflow` accepts workflow IDs, run IDs, or both. If it only accepts run IDs, add a `SignalWorkflowByID(workflowID, signalName, payload)` variant.

## 3. No `AwaitCondition` / `Await` Predicate Mechanism

**Severity**: Medium

**Description**: Temporal has `workflow.Await(ctx, func() bool)` which blocks until a predicate becomes true. This is used in the original sliding window to wait for `len(currentRecords) < SlidingWindowSize`. Cleat has no equivalent.

**Impact**: Workflows that need to wait for a state-based condition must use a signal-based approach instead. The port works around this by combining `PollSignal` (non-blocking) and `AwaitSignals` (blocking with timeout) in a loop. This is more complex and verbose than a predicate-based `Await`.

**Suggested fix**: Add `AwaitCondition(predicate func() bool, timeout time.Duration) bool` to `HostCalls`.

## 4. ContinueAsNew is a No-Op in Test Environment

**Severity**: Medium

**Description**: `durabletest.TestEnv.continueAsNewImpl` returns `nil` (no-op). This means workflows that depend on ContinueAsNew to create a new run cannot be tested end-to-end in the test environment. Only the first run executes; subsequent runs (which would happen in production) do not occur.

**Impact**: Tests for workflows that use ContinueAsNew can only verify the logic within a single run. Multi-run behaviors (e.g., processing records across run boundaries, state transitions between runs, signal delivery across runs) cannot be tested.

**Workaround in the port**: Tests use `PageSize` large enough to cover all records in one run when they need to verify full-range processing, or test only the first-page behavior when testing the ContinueAsNew boundary.

**Suggested fix**: Consider adding test-env support for ContinueAsNew that re-invokes the workflow function with the new input.

## 5. No SideEffect / Nondeterministic Workflow Warning

**Severity**: Low

**Description**: Temporal provides `workflow.SideEffect` for non-deterministic operations (like random number generation) within workflows, with a linter (`workflowcheck:ignore`) to mark safe non-determinism. Cleat has `HostCalls.Random()` which is deterministic-by-design.

**Impact**: No issue for this pattern -- `Random()` is the right API. But workflows that need true non-determinism (e.g., reading the wall clock) lack a `SideEffect` escape hatch. Not a blocker for this port but worth noting.

## 6. No Workflow Registration API in SDK

**Severity**: Low

**Description**: In Temporal, workflow functions are registered with the worker via `worker.RegisterWorkflow(MyWorkflow)`. In Cleat's Go SDK, workflow registration is implicit (workflow functions are exported Go functions referenced by name in `ChildWorkflow` calls). The test environment expects child workflow names to match those passed to `ChildWorkflow`.

**Impact**: The port works correctly, but the mapping between workflow function names and child workflow names is a string-based convention (e.g., `"RecordProcessorWorkflow"` must match between the parent's `ChildWorkflow` call and the test's `OnChildWorkflow` registration). Typos are not caught at compile time.

## Summary

| # | Issue | Severity | Status |
|---|-------|----------|--------|
| 1 | Missing `ParentWorkflowID()` | Medium | Workaround: pass via input |
| 2 | `SignalWorkflow` target semantics | High | Needs runtime clarification |
| 3 | No `AwaitCondition` predicate | Medium | Workaround: PollSignal + AwaitSignals loop |
| 4 | ContinueAsNew no-op in tests | Medium | Limits test coverage of multi-run flows |
| 5 | No SideEffect escape hatch | Low | `Random()` covers most cases |
| 6 | String-based workflow name mapping | Low | No compile-time safety |
