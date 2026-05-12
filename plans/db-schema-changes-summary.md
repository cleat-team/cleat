## DB Schema Changes — `feature/db-performance-improvements`

---

### Migration 002 fixes

**MSSQL** (`mssql/002_tenant_foundation.sql`):
- Added `DROP INDEX idx_instances_namespace_ready ON dbo.workflow_instances` before `DROP COLUMN namespace` (fixes runtime error: SQL Server won't drop a column that an index depends on)

**MySQL** (`mysql/002_tenant_foundation.sql`):
- Uncommented `DROP INDEX idx_instances_namespace_ready` + `DROP COLUMN namespace` on both `workflow_defs` and `workflow_instances` (was mistakenly commented out — MySQL was the only backend that kept the column)

**Postgres** (`postgres/002_tenant_foundation.sql`):
- No change needed — already had correct `DROP COLUMN IF EXISTS namespace`

---

### Migration 013 (new — all 3 backends)

#### 1. New column: `event_count`

| Backend | Table | Column | Type |
|---------|-------|--------|------|
| Postgres | `workflow_instances` | `event_count` | `BIGINT NOT NULL DEFAULT 0` (`IF NOT EXISTS`) |
| MySQL | `workflow_instances` | `event_count` | `BIGINT NOT NULL DEFAULT 0` |
| MSSQL | `dbo.workflow_instances` | `event_count` | `BIGINT NOT NULL DEFAULT 0` (guarded by `sys.columns` check) |

Purpose: replaces the expensive `SELECT workflow_id, COUNT(*) FROM event_history GROUP BY workflow_id` in `GetCompactionCandidates`. The Go-side increment in `appendEventsInTx` is not yet wired up — the old COUNT(*) query still works as a fallback.

#### 2. New secondary indexes

| Index name | Table | Columns | Backends | Purpose |
|------------|-------|---------|----------|---------|
| `idx_instances_created_at` | `workflow_instances` | `(tenant_id, created_at DESC)` | All 3 | `ListWorkflows ORDER BY created_at` |
| `idx_instances_terminal_completed` | `workflow_instances` | `(tenant_id, status, completed_at)` | All 3 (partial on PG/MSSQL) | `DeleteExpiredEvents` inner query |
| `idx_concurrency_keys_expires` | `concurrency_keys` | `(expires_at)` | All 3 | `ReapExpiredConcurrencyKeys` reaper |
| `idx_instances_parent_policy` | `workflow_instances` | `(parent_workflow_id, parent_close_policy, status)` | All 3 | `enforceParentClosePolicy` |

Backend notes:
- **Postgres**: `IF NOT EXISTS`, `DESC` on `created_at`, partial index `WHERE status IN ('done','failed')` on terminal_completed
- **MySQL**: No `IF NOT EXISTS`, no `DESC`, no partial index (full index on terminal_completed)
- **MSSQL**: All guarded by `sys.indexes` existence checks, `DESC`, partial index `WHERE status IN ('done','failed')`

#### 3. MSSQL column width fixes (MSSQL only)

Narrow 22 `NVARCHAR(MAX)` columns to appropriate sizes:

| Table | Column | Old | New |
|-------|--------|-----|-----|
| `workflow_instances` | `status` | `NVARCHAR(MAX)` | `NVARCHAR(32)` |
| `workflow_instances` | `assigned_to`, `error_code`, `error_op`, `parent_workflow_id`, `sticky_worker_id` | `NVARCHAR(MAX)` | `NVARCHAR(255)` |
| `workflow_instances` | `parent_close_policy` | `NVARCHAR(MAX)` | `NVARCHAR(32)` |
| `workflow_instances` | `id` | `NVARCHAR(64)` | `NVARCHAR(255)` |
| `event_history` | `service`, `operation`, `signal_name`, `defer_id`, `child_name`, `run_id`, `plugin_name`, `plugin_func`, `promise_name`, `promise_id` | `NVARCHAR(MAX)` | `NVARCHAR(255)` |
| `event_history` | `signal_names`, `defer_description` | `NVARCHAR(MAX)` | `NVARCHAR(512)` |

All guarded by `sys.columns` checks — only alters if currently `NVARCHAR(MAX)`.

#### 4. MSSQL CHECK constraints (MSSQL only)

| Constraint | Table | Rule |
|------------|-------|------|
| `ck_workflow_instances_result` | `workflow_instances` | `result IS NULL OR ISJSON(result) = 1` |
| `ck_workflow_instances_query_state` | `workflow_instances` | `query_state IS NULL OR ISJSON(query_state) = 1` |

---

### Migration 001/005/009 fixes (MySQL only)

| File | Table | Column | Old type | New type |
|------|-------|--------|----------|----------|
| `001_initial_schema.sql` | `concurrency_keys` | `key_hash` | `VARBINARY(64)` | `VARBINARY(32)` |
| `005_exactly_once.sql` | `idempotency_keys` | `key_hash` | `VARBINARY(64)` | `VARBINARY(32)` |
| `002_tenant_foundation.sql` | `tenant_api_keys` | `key_hash` | `LONGBLOB` | `VARBINARY(32)` |
| `009_tenant_roles.sql` | `tenant_api_keys` | `key_hash` | `LONGBLOB` | `VARBINARY(32)` |
| `002_tenant_foundation.sql` | `tenant_api_keys` | index | `idx_api_keys_hash(key_hash(255))` | `idx_api_keys_hash(key_hash)` |

SHA-256 produces 32 bytes — the old `VARBINARY(64)` and `LONGBLOB` were oversized. The prefix index `(255)` on a 32-byte column is unnecessary.

---

### Namespace column — fully removed

- **Migration 001** (all 3): Adds `namespace` column (unchanged — needed for clean installs)
- **Migration 002** (all 3): Drops `namespace` column + `idx_instances_namespace_ready` index
- **Migration 013**: Does NOT reference `namespace` at all
- **Go code**: Zero references to `namespace` in production code — removed from interface, claim queries, INSERTs, test schemas

---

### What's NOT in these migrations (deferred to `feature/parallel-threads-prep`)

- `thread_id`, `local_step`, `global_seq` columns on `event_history` — migration 012 on the other branch
- Event history PK change from `(workflow_id, step)` to `(workflow_id, thread_id, local_step)`
- Go-side `event_count` increment in `appendEventsInTx`
