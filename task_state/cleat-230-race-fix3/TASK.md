# cleat-230-race-fix3 — Fix drain races

**Parent:** cleat-230-race (Race Condition Audit)
**Budget:** $10 (~0.5 engineer-day)
**Priority:** 2 (feature)
**Type:** Bug fix

## Task

Fix two drain TOCTOU races and the drain-complete notification ordering bug.

### Problem 1: API workflows bypass drain

`handleStartWorkflow` (line 3301) checks `memoryController.CanAcceptAPIWorkflows()` but never checks `w.draining.Load()`. During drain, a workflow started via API is persisted as "ready" but never claimed (dispatch loop stops claiming). In a single-worker deployment, this workflow is abandoned.

### Problem 2: Dispatch loop claims work after drain starts

The dispatch loop checks `w.draining.Load()` at line 1273, then claims work at lines 1285/1312. Between the check and claim, drain can start. Result: one extra workflow claimed and executed after drain begins.

### Problem 3: Drain-complete notification ordering

`dispatchLoop` calls `w.cancel()` at line 1241 when drain is complete. `handleDrainStatus` closes `drainCh` via `drainOnce.Do` at line 3173. If `w.cancel()` fires first, the HTTP server shuts down, `handleDrainStatus` is killed mid-response, and `drainCh` is never closed — external callers waiting on `DrainComplete()` block forever.

### Fix

1. Add `w.draining.Load()` check in `handleStartWorkflow`, return 503
2. Re-check `w.draining.Load()` after DB claim in dispatch loop, abort execution if drain started
3. Remove `w.cancel()` from dispatch loop drain path; have `handleDrainStatus` call `w.cancel()` after closing `drainCh`

### Acceptance criteria

1. `handleStartWorkflow` returns 503 during drain
2. No workflow execution starts after drain is initiated
3. `drainCh` is always closed before root context cancellation
4. `DrainComplete()` never blocks indefinitely
5. Existing drain tests pass

### Out of scope

- Changes to drain timeout or strategy
- Multi-worker drain coordination
