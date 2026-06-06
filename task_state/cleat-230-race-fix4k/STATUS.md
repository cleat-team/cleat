# STATUS — cleat-230-race-fix4k

**Phase:** verified
**Created:** 2026-06-06T14:00:00Z
**Completed:** 2026-06-06
**Budget:** $10 (parent task)
**Spent:** $0 (verification only — no changes needed)

## Summary

Independent verification of cleat-230-race-fix4. All four items confirmed already implemented (by prior iteration cleat-230-race-fix4i). No code changes required.

## Verification results

### 1. ShardedStore.Close() lock
- **Status:** PRESENT
- **Location:** `engine/sharded_store.go:75-76`
- `s.mu.RLock()` at line 75, `defer s.mu.RUnlock()` at line 76

### 2. Compaction retry
- **Status:** PRESENT
- **Location:** `engine/compaction.go:219-240`
- 3-attempt exponential backoff (100ms, 200ms, 400ms)
- `isCompactionDeadlockError()` at line 248-259 recognizes: Postgres 40P01, MySQL 1213, MSSQL deadlock

### 3. Compaction generation guard
- **Status:** PRESENT in all three backends
- Postgres: `engine/db.go:2875` — `WHERE id = $3 AND generation = $4`
- MSSQL: `engine/mssql_store.go:2185` — `WHERE id = @p1 AND generation = @p4`
- MySQL: `engine/mysql_ops.go:451` — `WHERE id = ? AND tenant_id = ? AND generation = ?`

### 4. Dead code removal
| Item | File | Status |
|------|------|--------|
| `completeMu`/`completeResult`/`completeErr` fields | `engine/runtime.go` | REMOVED |
| `workEntryPoint`/`workInput` fields | `engine/runtime.go` | REMOVED |
| `signals` map | `engine/engine.go` | REMOVED |
| `QueryHandlers()` method | `engine/engine.go` | REMOVED |
| `executeViaDispatcher` function | `engine/backend_wasmtime.go` | REMOVED |
| `loadAllEventsForCompaction` function | `engine/compaction.go` | REMOVED |

Note: `workEntryPoint`/`workInput` fields in `engine/backend_wasmtime.go` are distinct from the removed `runtime.go` fields — they are actively used by the wasmtime backend and are NOT dead code.
