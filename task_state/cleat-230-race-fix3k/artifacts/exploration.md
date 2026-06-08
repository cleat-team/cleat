# Exploration — cleat-230-race-fix3k

**Date:** 2026-06-06
**Scope:** Verification pass for cleat-230-race-fix3 drain race fixes

## Current state

`cleat-230-race-fix3` is marked complete with all three drain race fixes implemented. The review fix3v found 0 BLOCKERs, 2 deferrable SHOULD_FIX items (both out-of-scope changes), and 2 NITs. All acceptance criteria are met.

## Files touched by fix3

- `cmd/cleat-worker/main.go` — the three drain fixes + out-of-scope execEngines Store/Delete + error message improvements
- `cmd/cleat-worker/worker_daemon_test.go` — 5 drain tests + 2 execEngines tests + discarding logger fix
- `engine/compaction.go` — deadlock retry + dead code removal (out-of-scope, belongs to fix4)
- `engine/compaction_test.go` — new compaction retry tests (out-of-scope, belongs to fix4)

## Verified

### Fix 1: API workflows bypass drain
- `handleStartWorkflow` returns 503 when `w.draining.Load()` is true at line 3313
- Test: `TestAPIStartWorkflow_Draining` — PASS

### Fix 2: Dispatch loop claims work after drain starts
- Post-claim recheck at lines 1368-1372 releases claimed workflows using `context.Background()` when draining
- Test: `TestDispatchLoop_DrainAfterClaim` — PASS

### Fix 3: Drain-complete notification ordering
- `w.cancel()` removed from dispatch loop drain-complete path (line 1241 was `w.cancel()`, now removed)
- `handleDrainStatus` calls `s.worker.cancel()` after `close(s.worker.drainCh)` at line 3186, both inside `drainOnce.Do`
- Tests: `TestDrainStatus_ClosesChannelBeforeCancel` — PASS, `TestDrainStatus_DoesNotCloseChannelWhenInflight` — PASS, `TestDrainComplete_DoesNotBlock` — PASS

### Test suite
- Full worker test suite: PASS (7.3s, 0 regressions)
- Compaction tests: all 20 PASS (including `TestCompactWorkflowHistory_RetryOnDeadlock` and `TestCompactWorkflowHistory_NoRetryOnNonDeadlock`)

## Deferrable SHOULD_FIX items (from review fix3v)

Both are out-of-scope changes mixed into the drain task:

1. **`engine/compaction.go`** — Deadlock retry + `loadAllEventsForCompaction` removal. These belong to `cleat-230-race-fix4` (already complete, already contains these changes). The changes are valid; they're just in the wrong task.

2. **`cmd/cleat-worker/main.go`** — `execEngines.Store`/`Delete` in `executeWorkflow`. This completes the previously-non-functional `dispatchPendingUpdates` feature. Real bug fix, wrong task.

## Decision

No code changes needed. All acceptance criteria are met. All tests pass. The two deferrable SHOULD_FIX items are out-of-scope for this task and do not affect drain correctness. They are already tracked in their respective tasks (fix4, and the execEngines changes are implicitly included in the combined commit).
