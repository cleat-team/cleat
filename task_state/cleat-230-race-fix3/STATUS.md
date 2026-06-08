# STATUS -- cleat-230-race-fix3

**Phase:** complete
**Created:** 2026-06-06T09:00:00Z
**Completed:** 2026-06-06T07:50:00Z
**Budget:** $10
**Spent:** $10

## Summary

All three drain race fixes implemented and tested.

### Changes

1. **Fix 1** (`cmd/cleat-worker/main.go:3313`): Added `w.draining.Load()` check in `handleStartWorkflow`. Returns 503 "worker is draining" before the memory controller check.

2. **Fix 2** (`cmd/cleat-worker/main.go:1367-1374`): Added post-claim drain re-check in `dispatchLoop`. After combining sticky + general claims, re-checks `w.draining.Load()`. If draining started during the claim, releases all claimed workflows via `ReleaseWorkflow` and continues to the next iteration.

3. **Fix 3a** (`cmd/cleat-worker/main.go:1240-1241`): Removed `w.cancel()` from the dispatch loop's drain-complete path. The loop now just returns when drain is complete, leaving context cancellation to `handleDrainStatus`.

4. **Fix 3b** (`cmd/cleat-worker/main.go:3186`): Added `s.worker.cancel()` to `handleDrainStatus`, immediately after `close(s.worker.drainCh)` inside `drainOnce.Do`. Ensures drainCh is always closed before the root context is cancelled.

### Test helper fix

Added discarding logger (`slog.New(slog.NewTextHandler(io.Discard, nil))`) to `newTestWorker` and `newTestWorkerWithConcurrency` to prevent nil pointer panics in dispatch tests.

### Test results

5 new tests, all passing:
- `TestAPIStartWorkflow_Draining` -- 503 during drain
- `TestDispatchLoop_DrainAfterClaim` -- post-claim release
- `TestDrainStatus_ClosesChannelBeforeCancel` -- drainCh closed before cancel
- `TestDrainStatus_DoesNotCloseChannelWhenInflight` -- drainCh stays open while inflight
- `TestDrainComplete_DoesNotBlock` -- DrainComplete() unblocks after drain

Full worker test suite: all passing (0 regressions).

### Engine build fix

Fixed pre-existing build error in `engine/compaction.go` (missing imports for `time`, `errors`, `strings`, `pq`; renamed duplicate `isDeadlockError` to `isCompactionDeadlockError`).

### Review verification (cleat-230-race-fix3i)

Review fix3r's SHOULD_FIX (`ReleaseWorkflow` should use `context.Background()` not `w.ctx`) confirmed already applied. All 5 drain tests and full worker suite pass (0 regressions). Details: `task_state/cleat-230-race-fix3i/STATUS.md`.
