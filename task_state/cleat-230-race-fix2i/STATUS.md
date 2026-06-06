# STATUS — cleat-230-race-fix2i

**Phase:** complete
**Created:** 2026-06-06T10:00:00Z
**Completed:** 2026-06-06T10:15:00Z
**Budget:** $2
**Spent:** $2

## Summary

Addressed all 4 review findings from review-v.md (Round 2 review of cleat-230-race-fix2).

## Changes

### Production code (`cmd/cleat-worker/main.go`)

| Finding | Change | Line |
|---------|--------|------|
| **S1** | Swapped defer order: `execEngines.Delete` deferred before `inflight.Delete` so it fires AFTER (LIFO) | 1387-1389 |
| **N1** | Changed `envVal.(*engine.Engine)` to comma-ok assertion with `return true` on type mismatch | 2141-2144 |

**S1 rationale:** Original ordering had `execEngines.Delete` firing before `inflight.Delete`. Since `dispatchPendingUpdates` gates on `inflight.Range`, a completing workflow could be found in inflight but not in execEngines, permanently orphaning updates. With the new ordering, `inflight.Delete` fires first → `dispatchPendingUpdates` skips the wfID entirely → no orphan window.

**N1 rationale:** Direct type assertion panics if a non-`*engine.Engine` value is stored. Comma-ok assertion skips safely.

### Test code (`cmd/cleat-worker/worker_daemon_test.go`)

| Finding | Change |
|---------|--------|
| **S2** | Renamed `TestExecEngines_StoreLoadDelete` → `TestExecEngines_MapLifecycle` to clarify scope (tests sync.Map API, not executeWorkflow) |
| **S3** | Added `execEngines.Delete` + `Load(!ok)` cleanup verification at end of `TestDispatchPendingUpdates_WithEngine` |

## Test results

All 4 fix2-related tests pass:

| Test | Result |
|------|--------|
| `TestDispatchPendingUpdates_EmptyInflight` | PASS |
| `TestDispatchPendingUpdates_NoEngine` | PASS |
| `TestExecEngines_MapLifecycle` | PASS |
| `TestDispatchPendingUpdates_WithEngine` | PASS |

Full worker test suite: PASS (0 regressions, 7.3s).

## Acceptance criteria

| Criterion | Status |
|-----------|--------|
| 1. `execEngines.Delete` deferred before `inflight.Delete` | Fixed — line 1388 |
| 2. Test renamed to `TestExecEngines_MapLifecycle` | Done |
| 3. Cleanup verification added to `TestDispatchPendingUpdates_WithEngine` | Done — lines 2247-2250 |
| 4. Comma-ok type assertion used | Done — lines 2141-2144 |
| 5. All existing execEngines/dispatch tests pass | PASS |
| 6. No regressions in full worker test suite | PASS |

## Decision

**PASS** — All 4 review findings addressed. Tests pass with no regressions.
