# Exploration — cleat-230-race-fix3e

**Date:** 2026-06-06
**Scope:** Drain race fixes for cleat-230

## Current state

All three fixes from the parent TASK.md are already implemented and committed (9d42352). The fixes address:

1. **Fix 1** — API workflows bypass drain: `handleStartWorkflow` checks `w.draining.Load()` at line 3316, returns 503
2. **Fix 2** — Dispatch loop claims work after drain starts: Post-claim re-check at lines 1366-1373 releases claimed workflows via `ReleaseWorkflow` using `context.Background()`
3. **Fix 3** — Drain-complete notification ordering: `w.cancel()` removed from dispatch loop drain path (line 1241 just returns); `handleDrainStatus` calls `close(drainCh)` then `w.cancel()` inside `drainOnce.Do` at lines 3186-3189

## Files touched

- `cmd/cleat-worker/main.go` — the three drain fixes (lines 1236-1245, 1366-1373, 3186-3189, 3316-3318)
- `cmd/cleat-worker/worker_daemon_test.go` — 5 drain tests (lines 3410-3597)

## Code verification

### Fix 1: handleStartWorkflow drain check (line 3316)
```go
if s.worker.draining.Load() {
    s.writeError(w, 503, "worker is draining; cannot accept new workflows")
    return
}
```
Correct: atomic read of draining flag before any work is done.

### Fix 2: Post-claim drain re-check (lines 1366-1373)
```go
if w.draining.Load() {
    for _, wf := range wfs {
        w.store.ReleaseWorkflow(context.Background(), wf.ID, w.id, wf.Generation, wf.NextWakeAt)
    }
    continue
}
```
Correct: uses `context.Background()` for ReleaseWorkflow (not w.ctx, which might be cancelled). Continues to next iteration instead of executing workflows.

### Fix 3: Drain-complete notification ordering
dispatchLoop (lines 1236-1245):
```go
if w.draining.Load() {
    inflight := 0
    w.inflight.Range(func(_, _ interface{}) bool { inflight++; return true })
    if inflight == 0 {
        w.logger.InfoContext(w.ctx, "drain complete", "worker_id", w.id)
        return  // No w.cancel() here
    }
    ...
}
```

handleDrainStatus (lines 3186-3189):
```go
s.worker.drainOnce.Do(func() {
    close(s.worker.drainCh)
    s.worker.cancel()
})
```
Correct: drainCh is closed before cancel(), ensuring DrainComplete() always unblocks.

## Test coverage

5 tests, all verifying the acceptance criteria:
- `TestAPIStartWorkflow_Draining` — 503 during drain
- `TestDispatchLoop_DrainAfterClaim` — post-claim release
- `TestDrainStatus_ClosesChannelBeforeCancel` — ordering invariant
- `TestDrainStatus_DoesNotCloseChannelWhenInflight` — drainCh stays open
- `TestDrainComplete_DoesNotBlock` — DrainComplete() unblocks

## Conclusion

All three fixes are correctly implemented and tested. No code changes needed for this variant.
