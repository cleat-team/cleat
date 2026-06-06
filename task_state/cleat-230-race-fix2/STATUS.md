# STATUS — cleat-230-race-fix2

**Phase:** verified
**Created:** 2026-06-06T09:00:00Z
**Completed:** 2026-06-06
**Verified:** 2026-06-06 (cleat-230-race-fix2p)
**Budget:** $15
**Spent:** $0

## Summary

Wired `execEngines` sync.Map so update dispatch system works. Added `Store` at engine creation and `Delete` at workflow cleanup.

## Changes

### Production code (`cmd/cleat-worker/main.go`)

1. **Line 1389** — `defer w.execEngines.Delete(wf.ID)` in `executeWorkflow` cleanup, alongside `w.inflight.Delete`, ensuring engines are removed when workflows finish
2. **Line 1667** — `w.execEngines.Store(wf.ID, eng)` after `engine.NewEngine(...)`, so `dispatchPendingUpdates` can find running engines  
3. **Line 2135** — `w.execEngines.Load(wfID)` already existed, now returns the engine instead of `!ok`

### Tests (`cmd/cleat-worker/worker_daemon_test.go`)

1. **`TestExecEngines_StoreLoadDelete`** — unit test verifying Store/Load (found) and Delete/Load (not found) lifecycle, plus DispatchUpdate through the stored engine
2. **`TestDispatchPendingUpdates_WithEngine`** — integration test: stores engine with update handler in execEngines, calls dispatchPendingUpdates, verifies the update name/payload reach the engine and CompleteUpdateRequest is called

### Test status

Engine package has pre-existing build errors from concurrent task work (cleat-230-race-fix4 dead code removal left runtime.go, compaction.go, backend_wazero.go in transitional state). These are unrelated to this task's changes. The cmd/cleat-worker package code itself is syntactically correct.

## Verification (cleat-230-race-fix2p)

All 4 tests pass:
- `TestDispatchPendingUpdates_EmptyInflight` — PASS
- `TestDispatchPendingUpdates_NoEngine` — PASS
- `TestExecEngines_StoreLoadDelete` — PASS
- `TestDispatchPendingUpdates_WithEngine` — PASS

Acceptance criteria:
1. ✅ Store (line 1667) and Delete (line 1389) in correct lifecycle order
2. ✅ Unit test verifies Store/Load/Delete lifecycle
3. ✅ Integration test verifies update reaches engine via dispatchPendingUpdates
4. ✅ sync.Map ensures concurrent safety; Load !ok skip prevents lost updates and double-dispatch
5. ✅ execEngines and inflight kept consistent (brief divergence windows handled by dispatch skip)
