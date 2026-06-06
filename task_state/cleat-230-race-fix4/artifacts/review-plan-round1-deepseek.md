# Review — cleat-230-race-fix4 (Plan Review, Round 1, deepseek)

**Reviewer task:** cleat-230-race-fix4r
**Date:** 2026-06-06
**Model:** deepseek-v4-pro
**Type:** Plan review (all items already implemented per STATUS.md)

## Independent verification

All four items verified by reading current source code (not relying on STATUS.md claims):

### Item 1 — ShardedStore.Close() lock (CONTRACT deliv 1)

CONFIRMED. `engine/sharded_store.go:75-76`: `s.mu.RLock()` / `defer s.mu.RUnlock()` around the shards iteration in `Close()`. Pattern matches existing `Shards()` method (L86-87). Addresses CONTRACT invariant: "ShardedStore.Shards() already uses mu.RLock() — Close() pattern must match."

### Item 2 — Compaction retry (CONTRACT deliv 2)

CONFIRMED. `engine/compaction.go:219-240`: 3-attempt retry loop with exponential backoff. Delay formula: `100 * (1<<(attempt-1))` ms, yielding 100ms then 200ms between retries. Context cancellation checked before each retry. `engine/compaction.go:248-259`: `isCompactionDeadlockError()` recognizes:
- Postgres 40P01 (via `pq.Error` code check)
- MySQL 1213 (via error string substring)
- MSSQL deadlock (via "deadlock"/"Deadlock" substring)

### Item 3 — Compaction generation guard (CONTRACT deliv 3)

CONFIRMED in all three backends:

| Backend | File:Line | Generation read | UPDATE WHERE |
|---------|-----------|----------------|--------------|
| PostgresStore | `db.go:2856-2875` | `SELECT generation ... WHERE id = $1` | `WHERE id = $3 AND generation = $4` |
| MSSQLStore | `mssql_store.go:2163-2185` | `SELECT generation ... WHERE id = @p1` | `WHERE id = @p1 AND generation = @p4` |
| MySQLStore | `mysql_ops.go:432-451` | `SELECT generation ... WHERE id = ? AND tenant_id = ?` | `WHERE id = ? AND tenant_id = ? AND generation = ?` |

All use optimistic locking: read generation, include it in UPDATE WHERE clause. If generation changed concurrently, UPDATE affects 0 rows.

### Item 4 — Dead code removal (CONTRACT deliv 4)

CONFIRMED. All six items absent from current engine source:

| Item | Specified Location | Status |
|------|-------------------|--------|
| `completeMu`/`completeResult`/`completeErr` fields | `runtime.go` | Not found |
| `signals` map field | `engine.go` execSession | Not found |
| `QueryHandlers()` method | `engine.go` | Not found |
| `executeViaDispatcher` function | `backend_wasmtime.go` | Not found |
| `loadAllEventsForCompaction` function | `compaction.go` | Not found |
| `workEntryPoint`/`workInput` fields | `runtime.go` Runtime struct | Not found |

Note: `cleatComplete` type still exists in `runtime.go:23` and `imports.go:868-870` — it is actively used via context-based completion mechanism, not just by the dead fields. Correctly retained.

### Item 5 — Compilation and tests (CONTRACT deliv 5)

CONFIRMED. `go build ./engine/...` succeeds with no output. `go test ./engine/ -run "Compact" -count=1` passes (0.344s, all 28 tests including fuzz seeds).

## Findings

### NIT — CONTRACT.md over-specifies backoff values

**File:** `task_state/cleat-230-race-fix4/CONTRACT.md:6`
**Issue:** CONTRACT.md says "100ms, 200ms, 400ms" for exponential backoff, but with 3 total attempts (as specified by both TASK.md and CONTRACT.md), there are only 2 retry waits. The implementation correctly applies 100ms, 200ms (2 waits for 3 attempts). The "400ms" in CONTRACT.md is internally inconsistent with "3 attempts."
**Why it matters:** Minimal. The implementation correctly follows the TASK.md specification. The CONTRACT has a documentation imprecision.
**Recommendation:** Either update CONTRACT to say "100ms, 200ms" or update the loop to 4 attempts with 100ms, 200ms, 400ms backoff.

### NIT — Stale doc references to removed functions

**File:** `benchmarks/db_bench_test.go:400`
**Issue:** Comment references `loadAllEventsForCompaction` which was removed per Item 4.
**Why it matters:** Minimal. A comment referencing a removed function could confuse future readers.

**File:** `plans/workflowstore-abstraction-project.md:58,415`
**Issue:** Planning document references `loadAllEventsForCompaction` at old file paths.
**Why it matters:** Minimal. This is a historical planning document, not code.

## Plan review checklist

| Criterion | Status |
|-----------|--------|
| Satisfies every CONTRACT.md requirement | PASS (all 5 deliverables verified) |
| Missing edge cases | PASS (retry handles context cancellation; generation guard is optimistic-lock safe) |
| Interaction with other system parts | PASS (CONTRACT: LOOSE coupling with fix1 at different runtime.go lines) |
| Test cases sufficient | PASS (compaction retry tested via retryMockStore; Close() has existing coverage) |
| Unnecessary complexity | PASS (straightforward changes; no over-engineering) |
| Security | PASS (no new attack surface; lock and generation guard reduce existing risk) |

## Convergence summary

- [BLOCKER]: 0
- [SHOULD_FIX]: 0
- [NIT]: 2 (CONTRACT backoff imprecision; stale doc comments)

Both NITs are deferrable — they don't affect correctness, security, data integrity, or user-visible behavior.

[OUTCOME:PASS]
