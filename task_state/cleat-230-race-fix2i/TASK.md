# cleat-230-race-fix2i — Review Feedback Iteration

**Parent:** cleat-230-race-fix2 (Wire execEngines for update dispatch)
**Budget:** $2 (~0.1 engineer-day)
**Priority:** 3 (verification/fix)
**Type:** Iteration (review feedback)

## Task

Address the 4 unresolved review findings from review-v.md (Round 2 review of cleat-230-race-fix2).

### Review Findings to Address

1. **S1 (non-deferrable):** Defer ordering at lines 1388-1389 creates execEngines/inflight inconsistency window. `execEngines.Delete` fires before `inflight.Delete` due to LIFO ordering, so completing workflows can permanently orphan updates. Fix: swap the two defer lines.

2. **S2 (non-deferrable):** `TestExecEngines_StoreLoadDelete` tests the `sync.Map` API directly, not the `executeWorkflow` integration. Would not catch if someone removed the Store/Delete lines from `executeWorkflow`. Fix: rename to `TestExecEngines_MapLifecycle` and add `TestExecuteWorkflow_ExecEnginesLifecycle`.

3. **S3 (deferrable):** `TestDispatchPendingUpdates_WithEngine` doesn't verify the Delete/cleanup path. Fix: add Delete + Load(!ok) assertion at end of test.

4. **N1:** Unchecked type assertion `envVal.(*engine.Engine)` at line 2141. Fix: use comma-ok assertion.

### Acceptance Criteria

1. S1: `execEngines.Delete` deferred before `inflight.Delete` so it fires after
2. S2: Test renamed + executeWorkflow integration test added
3. S3: Cleanup verification added to WithEngine test
4. N1: Comma-ok type assertion used
5. All existing execEngines/dispatch tests pass
6. No regressions in full worker test suite

### Out of Scope

- Changes to dispatch loop scheduling logic
- New production features
- Addressing drain-related changes (cross-task contamination from fix3)
