# DB Performance & Correctness — Implementation Report

Branch: `feature/db-performance-improvements` (off `develop`)
Date: 2026-05-11
Review fixes applied: 2026-05-11 (3 consistency/correctness fixes)

## Summary

12 files changed (543 insertions, 167 deletions), 3 new migration files (116 lines).

---

## Issue 1: namespace column inconsistency — FIXED

### Migration 013 (all 3 backends)
- **Postgres**: Re-adds `namespace TEXT NOT NULL DEFAULT 'default'` to `workflow_instances` and `workflow_defs`. Recreates `idx_instances_namespace_ready`. All `IF NOT EXISTS` guarded.
- **MySQL**: Skips namespace changes (column already exists from migration 002).
- **MSSQL**: Re-adds `namespace NVARCHAR(255) NOT NULL DEFAULT 'default'` to both tables. Recreates index. All guarded by `sys.columns`/`sys.indexes` existence checks.

### Migration 002 fix (MSSQL)
- Commented out `DROP COLUMN namespace` from both `workflow_defs` and `workflow_instances` in `mssql/002_tenant_foundation.sql`, matching the MySQL approach (MySQL already had these commented out). Added explanatory comment. Fixes the runtime error where SQL Server would fail to drop the column because `idx_instances_namespace_ready` depends on it.

**No Go code changes needed** — Go code still references `namespace` everywhere.

---

## Issue 2: tenant_id missing from MSSQL INSERTs — FIXED

Added `tenant_id` column and `s.tenantID` parameter to all 8 INSERTs in `mssql_store.go`:

| Method | Table | Param |
|--------|-------|-------|
| appendEventsInTx | event_history | @p32 |
| ContinueAsNew | workflow_instances | @p5 |
| StartNewRun (idempotency) | workflow_instances | @p5 |
| StartNewRun (no idempotency) | workflow_instances | @p5 |
| StartChildWorkflow | workflow_instances | @p7 |
| StartChildWorkflowAtomic | workflow_instances | @p7 |
| StartChildWorkflowAtomic | event_history | @p9 |
| CreateSchedule | workflow_schedules | @p8 |

---

## Issue 3: Postgres $3 parameter bug — FIXED

Three methods in `db.go` had `$3` in SQL with only 2 arguments. Changed to `$2` and added error logging:

| Method | Fixed SQL | Line (approx) |
|--------|-----------|---------------|
| CompleteWorkflow | `SET result = $2 WHERE workflow_id = $1` | ~1316 |
| FailWorkflow | `SET error_msg = $2 WHERE workflow_id = $1` | ~1363 |
| MoveToDeadLetterQueue | `SET error_msg = $2 WHERE workflow_id = $1` | ~1414 |

Added `"log"` import. Each ExecContext result is now captured and logged on error.

---

## Issue 4: Postgres RLS tenant isolation gaps — FIXED

Wrapped **38 methods** in `beginTxWithRLS` (transaction-scoped RLS session variable). Pattern:
```go
tx, err := s.beginTxWithRLS(ctx)
if err != nil { return ... }
defer tx.Rollback()
// use tx.QueryContext / tx.ExecContext instead of s.db
return ..., tx.Commit()
```

### Write methods (15):
TraceWorkflow, BatchHeartbeat, RetryWorkflow, UpdateStickyWorker, ClearStickyWorker, MoveToDeadLetterQueue, StartChildWorkflow, DeployWorkflowDef, MarkVersionDeprecated, PurgeWorkflowDef, CreateSchedule, DeleteSchedule, SetScheduleEnabled, UpdateScheduleNextRun, CreatePromise, ResolvePromise, RejectPromise, CreateUpdateRequest, CompleteUpdateRequest

### Read methods (20):
LoadEventHistory, LoadEventHistoryPaginated, CountEventHistory, CheckCancellation, GetQueryState, ListWorkflows, GetWorkflowByID, GetChildResult, ListWorkflowDefs, GetWorkflowDef, CountActiveInstances, ResolveLatestVersion, ValidateVersion, ListVersions, LoadWASM, LoadWorkflowConfig, LoadDAGSpec, GetPromise, ListPromises, GetPendingUpdateRequests, ListSchedules, QueueDepth, VerifyWorkflowEvents

### Double-filter fix:
- `GetCompactionCandidates`: Removed explicit `AND w.tenant_id = $2` (RLS handles it). Adjusted LIMIT param from `$3` to `$2`.
- `LoadCompactionState`: Removed explicit `AND tenant_id = $2`. Wrapped in `beginTxWithRLS`.
- `CompactHistory`: Removed redundant `AND tenant_id = $N` from DELETE and UPDATE queries. Converted from manual `s.db.BeginTx` + `setRLSOnTx` to `beginTxWithRLS`.

### Left without RLS (admin/cross-tenant):
ReapStaleInstances, DeleteExpiredEvents, GetActiveInstanceCountsByVersion, enforceParentClosePolicy, memory sample/stats methods, concurrency key methods, ResolveTenantFromAPIKey — with explanatory comments.

---

## Issue 5: MSSQL migration 002 will fail — FIXED

Commented out `DROP COLUMN namespace` in `mssql/002_tenant_foundation.sql` (see Issue 1).

---

## Issue 6: event_history index after PK change — NOT APPLICABLE

Migration 012 (event threading PK change) is on `feature/parallel-threads-prep`, not on this branch. The current schema retains `(workflow_id, step)` as the PK. The existing `idx_event_history_tenant_wf (tenant_id, workflow_id, step)` is valid as-is. The secondary indexes in migration 013 cover the hot query patterns regardless of the PK structure.

---

## Issue 7: Secondary indexes for hot queries — FIXED

Added in migration 013 for all 3 backends:

| Index | Tables | Purpose |
|-------|--------|---------|
| `event_count` column | workflow_instances | Replaces expensive `SELECT workflow_id, COUNT(*) FROM event_history GROUP BY workflow_id` |
| `idx_instances_created_at` | workflow_instances(tenant_id, created_at DESC) | ListWorkflows ORDER BY |
| `idx_instances_terminal_completed` | workflow_instances(tenant_id, status, completed_at) | DeleteExpiredEvents inner query (partial on PG/MSSQL) |
| `idx_concurrency_keys_expires` | concurrency_keys(expires_at) | ReapExpiredConcurrencyKeys reaper |
| `idx_instances_parent_policy` | workflow_instances(parent_workflow_id, parent_close_policy, status) | enforceParentClosePolicy |

Backend-specific notes:
- PostgreSQL: DESC in index, partial index on terminal_completed
- MySQL: No DESC (ignored), no partial index (full index instead)
- MSSQL: DESC in index, partial index on terminal_completed, all guarded by IF NOT EXISTS

### Go code changes for event_count (not yet implemented):
The plan calls for incrementing `event_count` in `appendEventsInTx`. This was deferred — the `event_count` column exists and defaults to 0, but the app does not yet increment it. The old `COUNT(*) FROM event_history` query will still work (backward compatibility) until the `appendEventsInTx` increment is added.

---

## Issue 8: Backend-specific cleanup — FIXED

### MySQL (6 items)
1. `concurrency_keys.key_hash`: `VARBINARY(64)` → `VARBINARY(32)` (SHA-256 = 32 bytes) — migration 001
2. `idempotency_keys.key_hash`: `VARBINARY(64)` → `VARBINARY(32)` — migration 005
3. `tenant_api_keys.key_hash`: `LONGBLOB` → `VARBINARY(32)` — migrations 002 and 009. Removed prefix length (255) from `idx_api_keys_hash`.
4. `DeleteExpiredEvents`: Added `ORDER BY completed_at` before `LIMIT 10000` in both subqueries — `mysql_ops.go`
5. SKIP LOCKED comment: `MariaDB 10.5+` → `MariaDB 10.6+` — `mysql_store.go`
6. `idx_instances_namespace_ready`: Added comment confirming it's intentionally kept — migration 002

### Postgres (3 items)
1. `QueueDepth`: Wrapped in `beginTxWithRLS`
2. `FinalizeWorkflowSegment`: Added comment documenting `result` parameter repurposed as `error_msg` when failed
3. `ClaimWorkflows`: Added comment noting explicit `tx.Rollback()` is intentional (releases locks immediately)

### MSSQL (4 items)
1. `workflow_instances.id`: `NVARCHAR(64)` → `NVARCHAR(255)` — migration 013
2. Wide columns: Narrowed 22 `NVARCHAR(MAX)` columns to `NVARCHAR(32)`/`NVARCHAR(255)`/`NVARCHAR(512)` in workflow_instances, event_history, workflow_defs — migration 013
3. CHECK constraints: Added `ISJSON` checks on `workflow_instances.result` and `workflow_instances.query_state` — migration 013
4. `AcquireConcurrencyKey` race condition: Wrapped 3 separate implicit transactions (DELETE expired, INSERT, SELECT verify) into one explicit `tx.BeginTx`/`tx.Commit` transaction
5. `GetQueryState` JSON path: Added security comment documenting safe-from-injection nature of `'$.' + @p2` concatenation

### Test schema (bonus)
- `internal/host/testutil/mysql_schema.go`: Updated `VARBINARY(64)` → `VARBINARY(32)` for concurrency_keys and idempotency_keys to match production migrations

---

## What was NOT done (deferred)

1. **Go-side event_count increment**: The `event_count` column exists with default 0, but `appendEventsInTx` does not yet issue `UPDATE workflow_instances SET event_count = event_count + 1`. The existing COUNT(*) subquery still works. This is backward-compatible and can be added in a follow-up PR.

2. **MySQL LoadEventHistory rows.Scan for thread columns**: Migration 012 (threading) is on a different branch. The thread columns don't exist yet on this branch, so no scan changes needed.

3. **MSSQL LoadEventHistory rows.Scan for thread columns**: Same — no threading migration on this branch.

---

## Files changed

| File | Insertions | Deletions | Purpose |
|------|-----------|-----------|---------|
| internal/host/db.go | 471 | 124 | Issues 3, 4, 8-Postgres |
| internal/host/mssql_store.go | 50 | 28 | Issues 2, 8-MSSQL |
| internal/host/mysql_ops.go | 2 | 0 | Issue 8-MySQL (ORDER BY) |
| internal/host/mysql_store.go | 1 | 1 | Issue 8-MySQL (comment) |
| internal/host/testutil/mysql_schema.go | 2 | 2 | Issue 8-MySQL (test schema) |
| migrations/mssql/002_tenant_foundation.sql | 8 | 6 | Issue 5 |
| migrations/mysql/001_initial_schema.sql | 1 | 1 | Issue 8-MySQL |
| migrations/mysql/002_tenant_foundation.sql | 5 | 2 | Issue 8-MySQL |
| migrations/mysql/005_exactly_once.sql | 2 | 2 | Issue 8-MySQL |
| migrations/mysql/009_tenant_roles.sql | 1 | 1 | Issue 8-MySQL |
| migrations/postgres/013_db_performance.sql | +34 | new | Issues 1, 7 |
| migrations/mysql/013_db_performance.sql | +38 | new | Issue 7 |
| migrations/mssql/013_db_performance.sql | +44 | new | Issues 1, 7, 8-MSSQL |
