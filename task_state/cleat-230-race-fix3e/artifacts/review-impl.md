# Implementation Review — cleat-230-race-fix3e

## Review round 1

### Fix 1: API workflows bypass drain

`cmd/cleat-worker/main.go:3316-3318`
```go
if s.worker.draining.Load() {
    s.writeError(w, 503, "worker is draining; cannot accept new workflows")
    return
}
```
- Atomic load of draining flag before any work: CORRECT
- Returns 503 (Service Unavailable): CORRECT — appropriate status for drain
- Message is descriptive: CORRECT

### Fix 2: Post-claim drain re-check

`cmd/cleat-worker/main.go:1366-1373`
```go
if w.draining.Load() {
    for _, wf := range wfs {
        w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.Generation, wf.NextWakeAt)
    }
    continue
}
```
- Re-checks draining after sticky + general claims are combined: CORRECT
- Uses `context.Background()` not `w.ctx`: CORRECT — w.ctx may be cancelled during drain, and release must succeed
- Releases ALL claimed workflows: CORRECT — no partial release
- `continue` skips execution: CORRECT

### Fix 3: Drain-complete notification ordering

**3a: dispatch loop (line 1241 removed)**
```diff
-               w.cancel()
                return
```
- `w.cancel()` removed from dispatch loop: CORRECT
- Loop just returns when inflight==0 during drain: CORRECT

**3b: handleDrainStatus (line 3188 added)**
```go
s.worker.drainOnce.Do(func() {
    close(s.worker.drainCh)
    s.worker.cancel()
})
```
- Both operations inside `drainOnce.Do`: CORRECT — guarantees single execution
- `close(drainCh)` before `cancel()`: CORRECT — DrainComplete() waiters unblock before server shutdown
- HTTP response written after drainOnce.Do: CORRECT — in-flight request completes before server shutdown

### Test coverage

5 tests, all passing:
- `TestAPIStartWorkflow_Draining` — 503 during drain
- `TestDispatchLoop_DrainAfterClaim` — post-claim release verified via channel
- `TestDrainStatus_ClosesChannelBeforeCancel` — drainCh closed, context cancelled, DrainComplete() non-blocking
- `TestDrainStatus_DoesNotCloseChannelWhenInflight` — drainCh stays open, context stays alive
- `TestDrainComplete_DoesNotBlock` — DrainComplete() blocks before drain, unblocks after

### Findings

No issues found. All three fixes are correctly implemented with proper atomic operations, correct ordering of side effects, and appropriate context usage.

### Verdict: CONVERGED

0 BLOCKER, 0 SHOULD_FIX, 0 NIT. Implementation is correct and satisfies all contract invariants.
