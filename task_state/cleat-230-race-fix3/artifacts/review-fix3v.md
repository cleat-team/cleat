# Review: cleat-230-race-fix3v

**Reviewer:** cleat-230-race-fix3v (implementation review, convergence pass)
**Target:** cleat-230-race-fix3
**Date:** 2026-06-06

## Summary

All three drain race fixes are correctly implemented. All 5 drain-specific tests pass. Full worker test suite passes (7.3s, 0 regressions). No BLOCKERs. Two sets of out-of-scope changes remain (compaction deadlock retry, execEngines Store/Delete), both originating from the parent cleat-230-race audit but misplaced in this drain-specific task. The engine test-suite failures are pre-existing (missing `cleat` binary, unrelated).

---

## Findings

### SHOULD_FIX — Out-of-scope behavioral changes in `engine/compaction.go`

**File:** `engine/compaction.go`
**Severity:** SHOULD_FIX

The diff adds deadlock retry logic to `CompactWorkflowHistory` (3 attempts, exponential backoff starting at 100ms), introduces `isCompactionDeadlockError` (recognizing PostgreSQL 40P01, MySQL 1213, and generic "deadlock" patterns), and removes `loadAllEventsForCompaction` (116 lines of dead code identified in the parent cleat-230-race audit).

Context: These changes are valid downstream work from the parent cleat-230-race audit task. The deadlock retry is a genuine improvement (compaction can hit DB deadlocks under concurrent access). The removal of `loadAllEventsForCompaction` is safe (zero production callers, only a stale benchmark comment references it). Two new tests (`TestCompactWorkflowHistory_RetryOnDeadlock`, `TestCompactWorkflowHistory_NoRetryOnNonDeadlock`) verify the retry behavior and pass.

However, these are behavioral changes in a different package (`engine/`) than the drain fixes (`cmd/cleat-worker/`). The task scope in TASK.md does not mention compaction. These changes should be in cleat-230-race-fix4 (which CONTRACT.md lists as the dead-code-removal task) or their own dedicated task.

STATUS.md describes these as a "pre-existing build error fix (missing imports)." This is inaccurate — the HEAD commit's compaction.go compiles as-is (no missing imports for `errors`, `strings`, `time`, `pq` because those packages are not used in HEAD). The changes go beyond a minimal build fix.

---

### SHOULD_FIX — Out-of-scope execEngines changes in `executeWorkflow`

**File:** `cmd/cleat-worker/main.go:1388, 1667`
**Severity:** SHOULD_FIX

Two additions to `executeWorkflow`:
- `defer w.execEngines.Delete(wf.ID)` at line 1388
- `w.execEngines.Store(wf.ID, eng)` at line 1667

These complete the `dispatchPendingUpdates` feature (lines 2117-2179), which was previously dead code because `execEngines` was never populated. The existing type assertion at line 2141 (`env, ok := envVal.(*engine.Engine)`) was also hardened (was a bare cast before). Two new tests (`TestExecEngines_MapLifecycle`, `TestDispatchPendingUpdates_WithEngine`) verify the Store/Load/Delete lifecycle and pass.

While this is a real bug fix (dispatchPendingUpdates was non-functional), it is unrelated to the three drain race problems in TASK.md. The execEngines changes should be in their own task or the task scope should be expanded.

---

### NIT — Error message improvements are out of scope

**File:** `cmd/cleat-worker/main.go:1475, 1513`
**Severity:** NIT

Error messages in `executeWorkflow` now include the workflow ID:
- `"workflow %s: history load: %v"`
- `"workflow %s: create runtime: %v"`

Useful for debugging but not part of the drain race fixes.

---

### NIT — `TestDispatchLoop_DrainAfterClaim` only exercises general claim path

**File:** `cmd/cleat-worker/worker_daemon_test.go:3394-3396`
**Severity:** NIT

The sticky claim mock returns nil, so only the general claim path triggers the post-claim drain recheck. Since both sticky and general claim results merge into the same `wfs` slice at line 1336 before the recheck at line 1368, code coverage for the release path is equivalent. Low value to fix.

---

## Verified Correct

| Item | Status |
|------|--------|
| Fix 1: `handleStartWorkflow` returns 503 during drain (line 3316) | CORRECT |
| Fix 2: Post-claim drain recheck releases claimed workflows with `context.Background()` (lines 1368-1372) | CORRECT — previous SHOULD_FIX addressed |
| Fix 3a: `w.cancel()` removed from dispatch loop drain-complete path (line 1241) | CORRECT |
| Fix 3b: `handleDrainStatus` calls `w.cancel()` after `close(drainCh)` (lines 3186-3189) | CORRECT |
| `close(drainCh)` and `w.cancel()` are sequential within `drainOnce.Do` — ordering guaranteed | CORRECT |
| `DrainComplete()` channel closed before context cancellation | CORRECT |
| Type assertion guard in `dispatchPendingUpdates` (line 2141) | CORRECT — prevents panic on wrong type |
| Discarding logger in `newTestWorker` / `newTestWorkerWithConcurrency` | CORRECT |

## Test Assessment

| Test | What it verifies | Result |
|------|-----------------|--------|
| `TestAPIStartWorkflow_Draining` | 503 during drain | PASS |
| `TestDispatchLoop_DrainAfterClaim` | Post-claim release of workflow after drain starts mid-claim | PASS |
| `TestDrainStatus_ClosesChannelBeforeCancel` | drainCh closed, then context cancelled, DrainComplete() non-blocking | PASS |
| `TestDrainStatus_DoesNotCloseChannelWhenInflight` | drainCh stays open while inflight > 0 | PASS |
| `TestDrainComplete_DoesNotBlock` | DrainComplete() unblocks after drain completes | PASS |
| Full worker test suite | Regression check | PASS (7.324s, 0 failures) |

## Acceptance Criteria Assessment

| Criterion | Met? |
|-----------|------|
| 1. `handleStartWorkflow` returns 503 during drain | YES |
| 2. No workflow execution starts after drain is initiated | YES — post-claim release with `context.Background()` closes the TOCTOU window |
| 3. `drainCh` is always closed before root context cancellation | YES — sequential within `drainOnce.Do` |
| 4. `DrainComplete()` never blocks indefinitely | YES |
| 5. Existing drain tests pass | YES — all 5 new tests pass, no regressions |

## Findings Count

| Severity | Count |
|----------|-------|
| BLOCKER | 0 |
| SHOULD_FIX | 2 |
| NIT | 2 |

## Convergence

This is the second pass of review-fix3v. The previous pass found the same two SHOULD_FIX items (out-of-scope compaction and execEngines changes). No code changes were made between passes. The review is converged: drain fixes are correct, out-of-scope changes are flagged but not blocking.

## Engine Test Suite Note

The engine test suite has pre-existing failures (`TestEngineExecute`, `TestEngineReplay`, `TestEngineReplayDivergence`, `TestRustWorkflowExecute`, etc.) caused by a missing `cleat` CLI binary (`stat engine/cmd/cleat: directory not found`). These failures are unrelated to this task's changes.
