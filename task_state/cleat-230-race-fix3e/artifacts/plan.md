# Plan — cleat-230-race-fix3e

## Summary

The three drain race fixes are already implemented and committed. This plan documents the design and verifies correctness against CONTRACT.md.

## Fix 1: API workflows bypass drain

**File:** `cmd/cleat-worker/main.go` — `handleStartWorkflow`
**Change:** Add `w.draining.Load()` check before memory controller check. Return HTTP 503 with descriptive message.
**Rationale:** Without this check, workflows started via API during drain are persisted as "ready" but never claimed (dispatch loop stops claiming during drain). In single-worker deployments, these workflows are abandoned.

## Fix 2: Dispatch loop claims work after drain starts

**File:** `cmd/cleat-worker/main.go` — `dispatchLoop`
**Change:** After combining sticky + general claims into `wfs` slice, re-check `w.draining.Load()`. If draining, release all claimed workflows via `ReleaseWorkflow` using `context.Background()` and continue to next iteration.
**Rationale:** The original check at line 1272 and the DB claim calls at lines 1284/1311 have a TOCTOU window. Drain can start between the check and the claim. Re-checking after the claim closes this window. Using `context.Background()` (not `w.ctx`) ensures release succeeds even if root context is cancelled.

## Fix 3: Drain-complete notification ordering

**File:** `cmd/cleat-worker/main.go` — `dispatchLoop` and `handleDrainStatus`
**Change 3a:** Remove `w.cancel()` from dispatch loop's inflight==0 drain path. The loop now just returns.
**Change 3b:** Add `s.worker.cancel()` to `handleDrainStatus` inside `drainOnce.Do`, immediately after `close(s.worker.drainCh)`.
**Rationale:** The original code had a race: `dispatchLoop` calls `w.cancel()` and `handleDrainStatus` closes `drainCh` via `drainOnce.Do`. If `w.cancel()` fires first, the HTTP server shuts down, `handleDrainStatus` is killed mid-response, `drainCh` is never closed, and `DrainComplete()` blocks forever. The fix consolidates both operations into `drainOnce.Do` with correct ordering: close channel first, then cancel context.

## Test plan

Five tests already implemented:
1. `TestAPIStartWorkflow_Draining` — 503 during drain
2. `TestDispatchLoop_DrainAfterClaim` — post-claim release
3. `TestDrainStatus_ClosesChannelBeforeCancel` — ordering invariant
4. `TestDrainStatus_DoesNotCloseChannelWhenInflight` — drainCh stays open
5. `TestDrainComplete_DoesNotBlock` — DrainComplete() unblocks

## Contract verification

| Invariant | Status |
|-----------|--------|
| No new workflow execution starts after drain | Fix 1 + Fix 2 |
| drainCh always closed before root context cancelled | Fix 3 |
| DrainComplete() never blocks | Fix 3 |
| drainOnce.Do fires exactly once | Fix 3 (atomic via sync.Once) |
| Dispatch loop inflight==0 doesn't cancel context | Fix 3a |
