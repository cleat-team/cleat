# STATUS — cleat-230-race-fix4

**Phase:** done
**Created:** 2026-06-06T09:00:00Z
**Completed:** 2026-06-06
**Verified:** 2026-06-06 (cleat-230-race-fix4i)
**Budget:** $10
**Spent:** $0

## Summary

All four items were already implemented and verified by iteration cleat-230-race-fix4i:

1. **ShardedStore.Close() lock** — `s.mu.RLock()`/`s.mu.RUnlock()` already present at `sharded_store.go:75-76`.
2. **Compaction retry** — 3-attempt exponential backoff retry loop already present at `compaction.go:219-240`, with `isCompactionDeadlockError()` helper recognizing Postgres 40P01, MySQL 1213, and MSSQL deadlock codes.
3. **Compaction generation guard** — All three backends already include `generation = ?` in their UPDATE statements:
   - PostgresStore: `db.go:2875` — `WHERE id = $1 AND generation = $2`
   - MSSQLStore: `mssql_store.go:2185` — `WHERE id = @p1 AND generation = @p4`
   - MySQLStore: `mysql_ops.go:451` — `WHERE id = ? AND tenant_id = ? AND generation = ?`
4. **Dead code removal** — All six items confirmed absent:
   - `completeMu`/`completeResult`/`completeErr` — removed from runtime.go
   - `signals` map — removed from engine.go
   - `QueryHandlers()` — removed from engine.go
   - `executeViaDispatcher` — removed from backend_wasmtime.go
   - `loadAllEventsForCompaction` — removed from compaction.go
   - `workEntryPoint`/`workInput` — removed from runtime.go

Compilation succeeds. Compaction tests pass (all 28 including fuzz seeds). No changes needed.
