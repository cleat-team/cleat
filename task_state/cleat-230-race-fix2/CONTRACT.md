# CONTRACT — cleat-230-race-fix2

## Deliverables

1. `w.execEngines.Store(wfID, eng)` after engine creation in `executeWorkflow`
2. `w.execEngines.Delete(wfID)` in cleanup path alongside `w.inflight.Delete`
3. Unit test for Store/Load/Delete lifecycle correctness
4. Integration test for end-to-end update dispatch

## Invariants

- `execEngines` always tracks the exact same set of workflow IDs as `inflight`
- `Store` happens before the engine goroutine is launched (happens-before relationship)
- `Delete` happens after the engine goroutine has exited
- No update is dispatched to a finished workflow (Load returns !ok for completed workflows)
- `dispatchPendingUpdates` correctly handles the race where an update arrives between workflow completion and Delete

## API Surface

No API changes. Behavior change: `POST /api/workflows/:id/updates` now successfully dispatches updates to running engines instead of silently persisting them.

## Integration Points

- `cmd/cleat-worker/main.go` — `executeWorkflow` (line ~1657): add `execEngines.Store`
- `cmd/cleat-worker/main.go` — inflight cleanup (line ~1380): add `execEngines.Delete`
- `cmd/cleat-worker/main.go` — `dispatchPendingUpdates` (line ~2125): already calls `Load`, no change needed
- `cmd/cleat-worker/main.go` — Worker struct (line ~1025): `execEngines` field, no change needed

## Test Requirements

- Unit test: create mock engine, Store it, Load it, verify it's found; Delete it, Load it, verify it's gone
- Integration test: start workflow via API, POST an update, verify engine receives it via DispatchUpdate
- Existing tests must pass (`go test ./cmd/cleat-worker/...`)

## Coupling

- MEDIUM with `cleat-230-race-fix3` (both touch `cmd/cleat-worker/main.go` — executeWorkflow/dispatch loop area)
- NONE with `cleat-230-race-fix1`, `cleat-230-race-fix4`
