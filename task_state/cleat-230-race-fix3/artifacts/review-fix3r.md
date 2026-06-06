# Review: cleat-230-race-fix3r

**Reviewer:** cleat-230-race-fix3r
**Target:** cleat-230-race-fix3 (plan review)
**Date:** 2026-06-06

## Summary

All three drain race fixes are correctly implemented and the five new tests verify the key invariants and all pass. One SHOULD_FIX item: the post-claim release uses `w.ctx` which can be cancelled in a narrow race window.

---

## Findings

### SHOULD_FIX — Post-claim release uses cancellable context

**File:** `cmd/cleat-worker/main.go:1370`
**Severity:** SHOULD_FIX

```go
if w.draining.Load() {
    for _, wf := range wfs {
        w.store.ReleaseWorkflow(w.ctx, wf.ID, w.id, wf.Generation, wf.NextWakeAt)
    }
```

The release call passes `w.ctx`, which `handleDrainStatus` may have cancelled via `drainOnce.Do` → `w.cancel()`. The race window: when a full batch is claimed, `time.Sleep(10 * time.Millisecond)` at line 1360 pauses execution between the claim and the post-claim recheck. During that 10ms:
1. `handleDrainStart` sets `draining = true`
2. `handleDrainStatus` sees `inflight == 0`, calls `drainOnce.Do` → `close(drainCh)` → `w.cancel()`
3. Post-claim recheck sees `draining.Load() == true`, calls `ReleaseWorkflow(w.ctx, ...)` with cancelled context → release fails silently

Three other release sites in the same file use `context.Background()` for exactly this reason:
- Line 1470: `w.store.ReleaseWorkflow(context.Background(), ...)`
- Line 1757: `w.store.ReleaseWorkflow(context.Background(), ...)`
- Line 2255: `w.store.ReleaseWorkflow(context.Background(), ...)`

**Fix:** Replace `w.ctx` with `context.Background()` at line 1370.

---

### NIT — Missing integration test for full drain lifecycle

**File:** `cmd/cleat-worker/worker_daemon_test.go`
**Severity:** NIT

Only unit tests were added. The unit tests individually verify each invariant (503 rejection, post-claim release, drainCh-before-cancel ordering, inflight blocking), so critical path coverage is adequate. An integration test tying all three fixes together in sequence (start drain → API rejects → dispatch releases claimed work → drainCh closes → context cancelled) would be stronger but is not a blocker.

---

### NIT — TestDispatchLoop_DrainAfterClaim only exercises the general claim path

**File:** `cmd/cleat-worker/worker_daemon_test.go:3394-3396`
**Severity:** NIT

```go
ms.claimStickyWorkflowsFn = func(ctx context.Context, workerID string, limit int) ([]*engine.WorkflowInstance, error) {
    return nil, nil
}
```

The mock returns nil for sticky claims, so only the general claim path triggers the post-claim recheck. Since both sticky and general claim results merge into the same `wfs` slice at line 1336 before the recheck at line 1368, code coverage for the release path is identical. Testing the sticky path explicitly is low value.

---

## Verified Correct

| Item | Status |
|------|--------|
| Fix 1: `handleStartWorkflow` returns 503 during drain (line 3313) | CORRECT |
| Fix 2: Post-claim drain recheck releases claimed workflows (lines 1366-1373) | CORRECT — except context issue flagged above |
| Fix 3a: `w.cancel()` removed from dispatch loop drain-complete path (lines 1236-1242) | CORRECT |
| Fix 3b: `handleDrainStatus` calls `w.cancel()` after `close(drainCh)` (lines 3182-3186) | CORRECT |
| `close(drainCh)` and `w.cancel()` are sequential within `drainOnce.Do` — ordering guaranteed | CORRECT |
| `DrainComplete()` channel closed before context cancellation | CORRECT |
| Discarding logger in `newTestWorker` / `newTestWorkerWithConcurrency` | CORRECT |
| Compaction build fix (`isCompactionDeadlockError`, imports) | CORRECT |

## Test Assessment

| Test | What it verifies | Status |
|------|-----------------|--------|
| `TestAPIStartWorkflow_Draining` | 503 during drain | PASS |
| `TestDispatchLoop_DrainAfterClaim` | Post-claim release of workflow after drain starts mid-claim | PASS |
| `TestDrainStatus_ClosesChannelBeforeCancel` | drainCh closed, then context cancelled, DrainComplete() non-blocking | PASS |
| `TestDrainStatus_DoesNotCloseChannelWhenInflight` | drainCh stays open while inflight > 0 | PASS |
| `TestDrainComplete_DoesNotBlock` | DrainComplete() unblocks after drain completes | PASS |

## Acceptance Criteria Assessment

| Criterion | Met? |
|-----------|------|
| 1. `handleStartWorkflow` returns 503 during drain | YES |
| 2. No workflow execution starts after drain is initiated | YES — with post-claim release, but the context issue above could cause a leak in the rare race window |
| 3. `drainCh` is always closed before root context cancellation | YES |
| 4. `DrainComplete()` never blocks indefinitely | YES |
| 5. Existing drain tests pass | YES — all 5 new tests pass, no regressions |
