# Review — cleat-230-race-fix4

**Reviewer task:** cleat-230-race-fix4r
**Date:** 2026-06-06
**Verdict:** PASS (1 NIT)

## Verification summary

All four items from TASK.md are confirmed implemented against current source.

### Item 1 — ShardedStore.Close() lock

CONFIRMED. `sharded_store.go:75-76` holds `s.mu.RLock()` / `s.mu.RUnlock()` around the shard iteration.

### Item 2 — Compaction retry

CONFIRMED. `compaction.go:219-240` has a 3-attempt retry loop with exponential backoff (100ms, 200ms). `compaction.go:248-259` defines `isCompactionDeadlockError()` recognizing Postgres 40P01 (via pq.Error code), MySQL 1213 (via error string), and MSSQL deadlock (via "deadlock"/"Deadlock" substring).

### Item 3 — Compaction generation guard

CONFIRMED. All three backends read the current generation and include it in the UPDATE WHERE clause:

| Backend | File:Line | Generation read | UPDATE WHERE |
|---------|-----------|----------------|--------------|
| PostgresStore | `db.go:2848-2882` | L2857 | L2875: `WHERE id = $3 AND generation = $4` |
| MSSQLStore | `mssql_store.go:2155-2192` | L2164 | L2185: `WHERE id = @p1 AND generation = @p4` |
| MySQLStore | `mysql_ops.go:424-458` | L433 | L451: `WHERE id = ? AND tenant_id = ? AND generation = ?` |

### Item 4 — Dead code removal

CONFIRMED. All six items are absent from current source:

| Item | Location | Status |
|------|----------|--------|
| `completeMu`/`completeResult`/`completeErr` | `runtime.go` | Not found |
| `signals` map field | `engine.go` struct | Not found |
| `QueryHandlers()` | `engine.go` | Not found |
| `executeViaDispatcher` | `backend_wasmtime.go` | Not found |
| `loadAllEventsForCompaction` | `compaction.go` | Not found |
| `workEntryPoint`/`workInput` | `runtime.go` | Not found |

## Findings

### NIT — Stale comment references removed `signals` field — FIXED

**File:** `engine/engine.go:1700`
**Issue:** The comment referenced `signals` in the list of maps protected by `mu`, but the `signals` field had been removed.
**Fix:** Removed `signals,` from the comment. Applied.
