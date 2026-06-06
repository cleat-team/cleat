# STATUS — cleat-230-race

**Phase:** decomposed
**Started:** 2026-06-06T07:45:00Z
**Exploration completed:** 2026-06-06T08:30:00Z
**Planning completed:** 2026-06-06T09:15:00Z
**Budget:** $10 (exploration) + planning
**Spent:** $10 (exploration $5 + planning $5)

## Summary

Exploration and planning complete. Decomposition artifacts at `artifacts/dag.json`.

### Exploration Findings (verified)

All 7 findings from the exploration report independently verified against the codebase:

- **Finding 1 (REAL RACE)**: `bytes.Buffer` stdout/stderr on shared Runtime — `PerExecution()` shares `*Runtime`, `InstantiateModuleNamed` calls `Reset()` while wazero writes concurrently. Confirmed at `engine/runtime.go:37-38,190-195`, `engine/backend_wazero.go:46-48`.
- **Finding 2 (DEAD CODE)**: `execEngines` sync.Map — zero `Store` calls, only `Load` at `cmd/cleat-worker/main.go:2125`. Confirmed via grep.
- **Finding 3 (DESIGN)**: Watchdog restart with 5s timeout can double-launch loops. Confirmed at `cmd/cleat-worker/main.go:2848-2868`.
- **Finding 4 (DESIGN)**: Drain TOCTOU — API bypass at `cmd/cleat-worker/main.go:3301`, claim race at line 1273. Confirmed.
- **Finding 5 (DESIGN)**: Drain notification ordering — `w.cancel()` at line 1241 races with `drainCh` close at line 3173. Confirmed.
- **Finding 6 (LATENT)**: `ShardedStore.Close()` without `mu.RLock()`. Confirmed at `engine/sharded_store.go:74-80`.
- **Finding 7 (LATENT)**: Compaction lock ordering vs execution. Confirmed at `engine/db.go:2848-2872`.

### Dead Code Verified

- `completeMu`/`completeResult`/`completeErr`: zero `completeMu.` accesses in runtime.go
- `signals` map: zero `.signals` accesses in engine.go
- `QueryHandlers()`: zero callers in entire codebase
- `executeViaDispatcher`: zero callers
- `loadAllEventsForCompaction`: zero callers
- `workEntryPoint`/`workInput`: never read (confirmed by exploration)

### Decomposition

4 child tasks created, all independent (can run in parallel):

| Task | Budget | Priority | Files |
|------|--------|----------|-------|
| cleat-230-race-fix1 | $5 | 2 | engine/runtime.go, engine/backend_wazero.go |
| cleat-230-race-fix2 | $15 | 2 | cmd/cleat-worker/main.go |
| cleat-230-race-fix3 | $10 | 2 | cmd/cleat-worker/main.go |
| cleat-230-race-fix4 | $10 | 3 | engine/sharded_store.go, engine/compaction.go, engine/db.go, engine/runtime.go, engine/engine.go |

### Coupling

- MEDIUM: fix2 ↔ fix3 (both touch cmd/cleat-worker/main.go — executeWorkflow/dispatch loop area, different functions)
- LOOSE: fix1 ↔ fix4 (both touch engine/runtime.go — different lines, different concerns)
- NONE: all other pairs

### Next Steps

The `artifacts/dag.json` is ready for the CTO agent to dispatch via `clew-decompose` workflow.
