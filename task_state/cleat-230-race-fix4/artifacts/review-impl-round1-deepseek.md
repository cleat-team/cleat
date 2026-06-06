# Implementation Review — cleat-230-race-fix4 (Round 1, deepseek)

**Reviewer task:** cleat-230-race-fix4v
**Date:** 2026-06-06
**Model:** deepseek-v4-pro
**Type:** Implementation review
**Budget:** $10

## Independent verification

All five deliverables verified by reading current source code (not relying on STATUS.md claims):

### Deliverable 1 — ShardedStore.Close() lock

CONFIRMED. `engine/sharded_store.go:75-76`: `s.mu.RLock()` / `defer s.mu.RUnlock()` around the shards iteration. Pattern matches existing `Shards()` (L86-87). Test `TestShardedStore_Close_HoldsLock` at `sharded_store_test.go:44` exercises 5 concurrent `Close()` + 5 concurrent `Shards()` calls. Passes under `-race` (1.051s).

### Deliverable 2 — Compaction retry

CONFIRMED. `engine/compaction.go:219-240`: 3-attempt retry loop with exponential backoff (100ms, 200ms between attempts). Context cancellation checked before each retry. `engine/compaction.go:248-259`: `isCompactionDeadlockError()` recognizes:
- Postgres 40P01 (via `pq.Error` code check)
- MySQL 1213 (via error string substring)
- MSSQL deadlock (via "deadlock"/"Deadlock" substring)

Tests: `TestCompactWorkflowHistory_RetryOnDeadlock` (2 mock failures then success → 3 calls expected) and `TestCompactWorkflowHistory_NoRetryOnNonDeadlock` (non-deadlock error → 1 call expected) at `compaction_test.go:1550-1580`.

### Deliverable 3 — Compaction generation guard

CONFIRMED in all three backends. Each reads current generation, then includes `AND generation = ?` in the UPDATE WHERE clause:

| Backend | File:Line | Generation read | UPDATE WHERE |
|---------|-----------|----------------|--------------|
| PostgresStore | `db.go:2856-2876` | `SELECT generation ... WHERE id = $1` (L2857) | `WHERE id = $3 AND generation = $4` (L2875) |
| MSSQLStore | `mssql_store.go:2163-2186` | `SELECT generation ... WHERE id = @p1` (L2164) | `WHERE id = @p1 AND generation = @p4` (L2185) |
| MySQLStore | `mysql_ops.go:432-452` | `SELECT generation ... WHERE id = ? AND tenant_id = ?` (L433) | `WHERE id = ? AND tenant_id = ? AND generation = ?` (L451) |

All use optimistic locking: if generation changed concurrently, UPDATE affects 0 rows.

### Deliverable 4 — Dead code removal

CONFIRMED. All six items absent from current engine source:

| Item | Specified location | Status |
|------|-------------------|--------|
| `completeMu`/`completeResult`/`completeErr` fields | `runtime.go` | Not found |
| `signals` map field | `engine.go` execSession | Not found in struct (L1662-1710) |
| `QueryHandlers()` method | `engine.go` | Not found |
| `executeViaDispatcher` function | `backend_wasmtime.go` | Not found |
| `loadAllEventsForCompaction` function | `compaction.go` | Not found |
| `workEntryPoint`/`workInput` fields | `runtime.go` | Not found |

Note: `cleatComplete` type correctly retained in `runtime.go:23` — it is actively used via context-based completion mechanism (`runtime.go:480-481`, `imports.go:868-870`). The CONTRACT.md condition "remove if only used by these fields" is satisfied: the fields are gone, the type is still independently useful.

The `signals` comment reference at `engine.go:1700` was fixed (prior review NIT). Current comment reads: "mu protects maps (queryState, stateStore, deferrals)" — no mention of removed `signals` field.

### Deliverable 5 — Compilation and tests

CONFIRMED. `go build ./engine/...` succeeds (no output). `go test ./engine/ -run "Compact|ShardedStore" -count=1` passes (0.346s, 28+ tests). `go test ./engine/ -run "TestShardedStore_Close_HoldsLock" -race -count=1` passes (1.051s).

## Findings

### NIT 1 — CONTRACT.md backoff imprecision (deferrable)

**File:** `task_state/cleat-230-race-fix4/CONTRACT.md:6`
**Issue:** CONTRACT.md says "exponential backoff (100ms, 200ms, 400ms)" but with 3 total attempts there are only 2 retry waits (100ms, 200ms). The 400ms value is never reached.
**Impact:** Minimal. Implementation correctly follows TASK.md's "3 attempts, exponential backoff." The CONTRACT has a documentation imprecision.
**Recommendation:** Either update CONTRACT to say "100ms, 200ms" or expand the loop to 4 attempts.

### NIT 2 — Stale comment references removed function (deferrable)

**File:** `benchmarks/db_bench_test.go:398-400`
**Issue:** Comment on `BenchmarkCompaction` says "This exercises the cursor-based pagination in loadAllEventsForCompaction." The function `loadAllEventsForCompaction` was removed; the benchmark now uses inline SQL queries.
**Impact:** Minimal. A stale comment that could confuse future readers tracing the codebase.

### NIT 3 — Stale planning document references (deferrable)

**File:** `plans/workflowstore-abstraction-project.md:58,415`
**Issue:** Planning document references `loadAllEventsForCompaction` at old file paths.
**Impact:** Minimal. This is a historical planning document, not code.

## Go-specific checks

| Check | Result |
|-------|--------|
| Unhandled errors | PASS — all error returns checked; generation guard propagates errors; retry loop handles both success and failure |
| Deferred cleanup | PASS — `defer tx.Rollback()` in all 3 CompactHistory backends; `defer s.mu.RUnlock()` in Close() |
| Goroutine lifetime | PASS — only test goroutines, all managed with sync.WaitGroup |
| Nil receiver safety | PASS — no new pointer-receiver methods |
| `time.After` usage | ACCEPTABLE — short delays (100-200ms), not a hot path; timer leak on context cancellation is negligible |

## Convergence summary

- [BLOCKER]: 0
- [SHOULD_FIX]: 0
- [NIT]: 3 (CONTRACT backoff imprecision; stale benchmark comment; stale planning doc references)

All three NITs are deferrable — they don't affect correctness, security, data integrity, or user-visible behavior. The implementation faithfully delivers all five CONTRACT requirements.

[OUTCOME:PASS]
