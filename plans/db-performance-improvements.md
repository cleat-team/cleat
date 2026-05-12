# Database Performance & Correctness Improvements Plan

Branch: `feature/db-performance-improvements` (based on `develop`)

---

## Issue 1: `namespace` column inconsistency across backends (CRITICAL)

### Current State

| Backend | Has `namespace` column? | Migration action |
|---------|------------------------|------------------|
| Postgres | **No** | 002 drops it (line 85) |
| MySQL | **Yes** | 002 comments out the DROP (lines 100-104) |
| MSSQL | **No** | 002 drops it (line 141) |

The Go code (all 3 backends) still references `namespace` in:
- Claim/ClaimSticky WHERE clauses (`AND namespace = $2`)
- StartNewRun / ContinueAsNew / StartChildWorkflow INSERT column lists
- Subquery SELECTs on workflow_defs.namespace
- The `WorkflowStore` interface signature (`namespace string` parameter)
- The worker CLI (`-namespace` flag, `w.namespace` field)

**Postgres and MSSQL are broken at runtime** on any claim or workflow creation. MySQL works because it kept the column.

### Fix

Option A: Re-add `namespace` to Postgres and MSSQL via a new migration. Remove the DROP from 002 if not yet applied. Simplest fix — no Go code changes needed.

**Recommendation: Option A** — `namespace` is a live feature (worker CLI flag, logical partitioning within a tenant). Migration 002's DROP was a mistake. The MySQL migration intentionally kept it. Re-add it to Postgres/MSSQL.

**Migration 013**: `ALTER TABLE workflow_instances ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT 'default'; ALTER TABLE workflow_defs ADD COLUMN IF NOT EXISTS namespace TEXT NOT NULL DEFAULT 'default';` — plus recreate `idx_instances_namespace_ready`.

---

## Issue 2: `tenant_id` missing from MSSQL INSERTs (CRITICAL)

### Current State

Migration 002 makes `tenant_id` NOT NULL on `workflow_instances`, `event_history`, and `workflow_schedules`. The MSSQL store has 8 INSERTs that omit `tenant_id`:

| Method | Table | Line |
|--------|-------|------|
| appendEventsInTx | event_history | 491 |
| ContinueAsNew | workflow_instances | 780 |
| StartNewRun (with idempotency) | workflow_instances | 996 |
| StartNewRun (no idempotency) | workflow_instances | 1016 |
| StartChildWorkflow | workflow_instances | 1337 |
| StartChildWorkflowAtomic | workflow_instances | 1366 |
| StartChildWorkflowAtomic event | event_history | 1382 |
| CreateSchedule | workflow_schedules | 1552 |

### Fix

Add `tenant_id` column to every INSERT column list and `s.tenantID` to every VALUES/params list, matching the MySQL pattern.

---

## Issue 3: Postgres parameter numbering bug in idempotency key updates (CRITICAL)

### Current State

Three methods in `db.go` use `$3` in their SQL but only provide 2 arguments:

| Line | Method | SQL | Args | Bug |
|------|--------|-----|------|-----|
| 1316 | CompleteWorkflow | `SET result = $3 WHERE workflow_id = $1` | 2 args | Should be `$2` |
| 1363 | FailWorkflow | `SET error_msg = $3 WHERE workflow_id = $1` | 2 args | Should be `$2` |
| 1414 | MoveToDeadLetterQueue | `SET error_msg = $3 WHERE workflow_id = $1` | 2 args | Should be `$2` |

These produce `pq: got 2 parameters but the statement requires 3` at runtime. The error is silently discarded (`ExecContext` return is not checked). Idempotency key result/error is never stored.

### Fix

Change `$3` to `$2` in all three queries and add error logging if the ExecContext fails.

---

## Issue 4: Postgres methods bypass RLS tenant isolation (HIGH)

### Current State

~30 methods in `db.go` use `s.db` directly without wrapping in an RLS transaction (`beginTxWithRLS`). The RLS policy defaults to `tenant_id = '00000000-0000-0000-0000-000000000000'` when `cleat.tenant_id` is not set, so these methods only see/affect the default tenant.

Affected methods include: LoadEventHistory, CountEventHistory, BatchHeartbeat, ReapStaleInstances, QueueDepth, ListWorkflows, GetWorkflowByID, CheckCancellation, GetQueryState, LoadWASM, DeployWorkflowDef, ListWorkflowDefs, ResolveLatestVersion, VerifyWorkflowEvents, GetCompactionCandidates, LoadCompactionState, DeleteExpiredEvents, StartChildWorkflow, GetChildResult, RetryWorkflow, and all their variants.

Additionally, `GetCompactionCandidates` and `LoadCompactionState` have a double-filter contradiction: they explicitly pass `tenant_id = $2` AND RLS adds `tenant_id = default_tenant`, making the effective WHERE `tenant_id = X AND tenant_id = default` — a contradiction for non-default tenants.

### Fix

Audit every method that accesses RLS-protected tables (`workflow_instances`, `event_history`, `workflow_defs`, `workflow_signals`, `workflow_schedules`). For each method, either:
- Wrap in `beginTxWithRLS` if it does writes or needs consistent reads
- Set the session variable via a separate `SET SESSION` / `set_config` call if it's read-only and doesn't need a transaction

Remove the explicit `tenant_id = $N` filter from GetCompactionCandidates and LoadCompactionState (RLS handles it).

---

## Issue 5: MSSQL migration 002 will fail (HIGH)

### Current State

Migration `mssql/002_tenant_foundation.sql` line 141 drops `namespace` from `workflow_instances`, but the index `idx_instances_namespace_ready` (created in 001) is never dropped first. SQL Server raises: `"Cannot drop the column 'namespace' because the index 'idx_instances_namespace_ready' requires it."`

### Fix

Add `DROP INDEX IF EXISTS idx_instances_namespace_ready ON dbo.workflow_instances;` before the DROP COLUMN. Or, if Issue 1 is resolved by keeping namespace, remove the DROP COLUMN entirely.

---

## Issue 6: Missing `idx_event_history_tenant_wf` equivalent on MySQL/MSSQL after PK change (HIGH)

### Current State

Migration 012 (event threading) changes the event_history PK from `(workflow_id, step)` to `(workflow_id, thread_id, local_step)`. After this change, `LoadEventHistory`'s `ORDER BY step` no longer matches the PK order.

The secondary index `idx_event_history_tenant_wf (tenant_id, workflow_id, step)` bridges this gap — but only if:
1. It exists on all 3 backends (currently: yes)
2. The query includes `tenant_id` in the WHERE clause so the index can be used

**MySQL**: LoadEventHistory includes `AND tenant_id = ?` — index works.
**MSSQL**: LoadEventHistory uses only `WHERE workflow_id = @p1` without tenant_id. The index `(tenant_id, workflow_id, step)` cannot be used efficiently without a tenant_id filter. If the MSSQL RLS-equivalent (`sp_set_session_context`) adds tenant_id implicitly, this works. Otherwise, the query loses index ordering on `step`.

### Fix

Verify MSSQL RLS/session-context actually filters by tenant_id. If not, either add `AND tenant_id = @pN` to MSSQL's LoadEventHistory WHERE, or add an index on `(workflow_id, step)` (without tenant_id).

---

## Issue 7: Secondary indexes for hot queries (MEDIUM)

### 7a: `GetCompactionCandidates` full event_history GROUP BY

All 3 backends do `SELECT workflow_id, COUNT(*) FROM event_history GROUP BY workflow_id`, which scans the entire events table. For 10M+ events, this is minutes-long.

**Fix**: Add `event_count BIGINT NOT NULL DEFAULT 0` column to `workflow_instances`. Increment it in `appendEventsInTx` (UPDATE workflow_instances SET event_count = event_count + 1 WHERE id = $1). Use it instead of the GROUP BY subquery:

```sql
SELECT id FROM workflow_instances
WHERE event_count > $1
  AND (compaction_step IS NULL OR compaction_step < event_count - $1)
  AND tenant_id = $2
ORDER BY event_count DESC
LIMIT $3
```

### 7b: `ListWorkflows` ORDER BY created_at — no index

All 3 backends have `ORDER BY created_at DESC` with no supporting index. Causes a full sort on every list query.

**Fix**: Add `CREATE INDEX idx_instances_created_at ON workflow_instances(tenant_id, created_at DESC)`.

### 7c: `DeleteExpiredEvents` inner query — no index on terminal workflows

The inner subquery `WHERE status IN ('done','failed') AND completed_at < $1` has no index support.

**Fix**: Add `CREATE INDEX idx_instances_terminal_completed ON workflow_instances(tenant_id, status, completed_at) WHERE status IN ('done','failed')` — partial on Postgres/MSSQL, full index on MySQL.

### 7d: `ReapExpiredConcurrencyKeys` — no index on expires_at

MySQL/MSSQL lack index on `concurrency_keys(expires_at)`. Postgres has no partial index for it either. The periodic reaper does a full scan.

**Fix**: Add `CREATE INDEX idx_concurrency_keys_expires ON concurrency_keys(expires_at)` on all 3 backends.

### 7e: `enforceParentClosePolicy` — no index

The query `WHERE parent_workflow_id = ? AND parent_close_policy = 'TERMINATE' AND status NOT IN ('done','failed')` has no supporting index.

**Fix**: Add `CREATE INDEX idx_instances_parent_policy ON workflow_instances(parent_workflow_id, parent_close_policy, status)`.

---

## Issue 8: Backend-specific cleanup (LOW)

### MySQL
- **Stale index `idx_instances_namespace_ready`**: Keep if namespace is kept, otherwise drop it.
- **`SKIP LOCKED` compatibility comment**: MariaDB 10.6+, not 10.5. Update comments.
- **`concurrency_keys.key_hash` size**: `VARBINARY(64)` should be `VARBINARY(32)` (SHA-256 is 32 bytes).
- **`idempotency_keys.key_hash` size**: Same — `VARBINARY(64)` should be `VARBINARY(32)`.
- **`tenant_api_keys.key_hash` type**: `LONGBLOB` is excessive. Use `VARBINARY(32)`.
- **Missing ORDER BY in `DeleteExpiredEvents`**: Non-deterministic batch selection. Add `ORDER BY completed_at`.

### MSSQL
- **`workflow_instances.id` type**: `NVARCHAR(64)` is too short. UUIDs fit, but custom IDs may not.
- **Wide columns**: Many event fields use `NVARCHAR(MAX)` where `NVARCHAR(255)` or `NVARCHAR(512)` would be better for indexing and storage.
- **Missing CHECK constraints**: `workflow_instances.result` and `query_state` lack `ISJSON` checks.
- **Race condition in `AcquireConcurrencyKey`**: Three separate implicit transactions. Should be one explicit transaction.
- **`GetQueryState` dynamic JSON path**: Concatenation of user-controlled `key` into `JSON_VALUE(query_state, '$.' + @p2)` — safe from injection (no DML possible) but should use `JSON_QUERY` or parameterize the path.

### Postgres
- **`QueueDepth`**: No RLS wrapping, counts across all tenants (or only default tenant via RLS default).
- **`FinalizeWorkflowSegment` passes `result` as idempotency error_msg when failed**: Confusing parameter repurposing. Document or rename parameter.
- **Double-rollback in ClaimWorkflows**: `defer tx.Rollback()` + explicit `tx.Rollback()` when empty. Non-idiomatic but harmless.

---

## Implementation Order

1. **Migration 013**: Re-add `namespace`, add `event_count`, add secondary indexes (7a-7e)
2. **Fix CRITICAL bugs**: Issue 2 (MSSQL tenant_id INSERTs), Issue 3 (Postgres $3 param bug)
3. **Fix HIGH bugs**: Issue 4 (Postgres RLS gaps), Issue 5 (MSSQL migration), Issue 6 (event_history index)
4. **Fix LOW cleanup items**: Issue 8 backend-specific items
5. **Test**: Run full multi-DB CI suite, verify claim/start-run on all backends
