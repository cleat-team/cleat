# cleat-230-race-fix2 — Wire execEngines for update dispatch

**Parent:** cleat-230-race (Race Condition Audit)
**Budget:** $15 (~1 engineer-day)
**Priority:** 2 (feature)
**Type:** Feature completion (dead code activation)

## Task

Wire the `execEngines` sync.Map so the update dispatch system actually works. Currently `execEngines.Store` is never called, making `dispatchPendingUpdates` dead code.

### Problem

The Worker struct declares `execEngines sync.Map` at `cmd/cleat-worker/main.go:1025`, but there is zero `execEngines.Store(...)` calls in the codebase. The `dispatchPendingUpdates` function at line 2125 calls `execEngines.Load(wfID)` which always returns `!ok`, so updates queued via `POST /api/workflows/:id/updates` are persisted to the DB but never dispatched to running workflow engines.

### Fix

1. Add `w.execEngines.Store(wf.ID, eng)` after engine creation in `executeWorkflow` (around line 1657)
2. Add `w.execEngines.Delete(wf.ID)` alongside `w.inflight.Delete` in the cleanup path (around line 1380)
3. Verify the full lifecycle: Store on create, Load in dispatch, Delete on cleanup

### Acceptance criteria

1. `execEngines.Store` and `execEngines.Delete` are called in the correct lifecycle order
2. Unit test verifies Store/Load/Delete lifecycle
3. Integration test submits an update via API and verifies it reaches the engine
4. No double-dispatch or lost updates under concurrent access
5. execEngines and inflight maps stay consistent (same set of workflow IDs)

### Out of scope

- Changes to the dispatch loop scheduling logic
- Changes to update payload format or routing
