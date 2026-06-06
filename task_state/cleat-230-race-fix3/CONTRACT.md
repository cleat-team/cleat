# CONTRACT — cleat-230-race-fix3

## Deliverables

1. `w.draining.Load()` check in `handleStartWorkflow` — return 503 if draining
2. Re-check `w.draining.Load()` after `ClaimWorkflows` / `ClaimStickyWorkflows` in dispatch loop — abort execution if drain started
3. Consolidate drain-complete signaling: remove `w.cancel()` from dispatch loop, have `handleDrainStatus` call `w.cancel()` after closing `drainCh`

## Invariants

- No new workflow execution starts after `handleDrainStart` sets `draining = true`
- `drainCh` is always closed before the root context is cancelled
- `DrainComplete()` always returns (never blocks forever)
- `drainOnce.Do` fires exactly once per drain cycle
- The dispatch loop's inflight==0 check in drain mode does NOT cancel the root context (that's handleDrainStatus's job)

## API Surface

- `POST /api/workflows/:name/start` now returns 503 during drain (previously accepted workflows that were abandoned)
- No other API changes

## Integration Points

- `cmd/cleat-worker/main.go` — `handleStartWorkflow` (line ~3301): add draining check
- `cmd/cleat-worker/main.go` — `dispatchLoop` drain check (line ~1273): re-check after claim
- `cmd/cleat-worker/main.go` — `dispatchLoop` inflight==0 path (line ~1238-1243): remove `w.cancel()`
- `cmd/cleat-worker/main.go` — `handleDrainStatus` (line ~3157-3180): add `w.cancel()` after closing drainCh
- `cmd/cleat-worker/main.go` — `handleDrainStart` (line ~3117): no change needed, sets draining before returning

## Test Requirements

- Unit test: drain started between ClaimWorkflows and executeWorkflow → workflow not executed
- Unit test: handleStartWorkflow during drain → 503
- Unit test: handleDrainStatus closes drainCh then cancels context → DrainComplete() unblocks
- Integration test: full drain lifecycle (start drain, API rejects, dispatch stops, drain completes)
- Existing drain tests must pass

## Coupling

- MEDIUM with `cleat-230-race-fix2` (both touch `cmd/cleat-worker/main.go` — dispatch loop area)
- NONE with `cleat-230-race-fix1`, `cleat-230-race-fix4`
