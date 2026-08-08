# PostgreSQL Schema

Cleat uses PostgreSQL as its sole infrastructure dependency, serving four
roles: blob store, state store, work queue, and timer service.

## Schema File

The canonical schema is `migrations/postgres/`. Apply every file, in
lexical order, against a PostgreSQL 16+ database before deploying workflows:

```bash
for f in migrations/postgres/*.sql; do psql -U postgres -d cleat -f "$f"; done
```

All files are idempotent, so re-running them is safe.

Apply **all** of them, not just `001_schema.sql`. `003_procedures.sql`
creates `finalize_workflow_status`, which the engine calls on every workflow
completion with no fallback — a database without it cannot finish a single
workflow.

> This document previously named a root `schema.sql` as canonical. That file
> was a second, hand-maintained copy that had drifted into a strict subset of
> the migrations: no stored procedures, no row-level security policies, and
> no `admin.tenants`. It has been deleted rather than repaired, so there is
> now exactly one source. See `engine/schema_bootstrap_test.go`, which builds
> a database from these files and asserts the engine's requirements hold.

## Core Tables

### workflow_defs

Stores compiled WASM blobs, versioned by workflow name. Each deploy creates a
new version.

```sql
CREATE TABLE workflow_defs (
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    wasm_bytes BYTEA NOT NULL,
    entry_points TEXT[] NOT NULL DEFAULT '{}',
    min_version INTEGER NOT NULL DEFAULT 0,
    max_history_length INTEGER NOT NULL DEFAULT 0,
    namespace TEXT NOT NULL DEFAULT 'default',
    dag_spec JSONB DEFAULT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version)
);
```

| Column | Type | Description |
|--------|------|-------------|
| `name` | TEXT | Workflow name (part of composite PK) |
| `version` | INTEGER | Monotonically increasing version number |
| `wasm_bytes` | BYTEA | Compiled WASM module binary |
| `entry_points` | TEXT[] | Exported entry point names (e.g., `{"place_order","cancel_order"}`) |
| `min_version` | INTEGER | Minimum compatible version for replay |
| `max_history_length` | INTEGER | Max events before compaction triggers (0 = default) |
| `namespace` | TEXT | Namespace for multi-tenant isolation |
| `dag_spec` | JSONB | DAG structure for visualization (optional) |
| `created_at` | TIMESTAMPTZ | Deployment timestamp |

**Indexes**:

- `idx_defs_active` on `(name, version DESC)` -- speeds up latest-version
  lookups for deployment.

### workflow_instances

Tracks individual workflow execution state. Serves as both state store and
work queue.

```sql
CREATE TABLE workflow_instances (
    id TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    def_version INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'ready',
    input JSONB NOT NULL DEFAULT '{}',
    assigned_to TEXT,
    heartbeat_at TIMESTAMPTZ,
    next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    result JSONB,
    error_msg TEXT,
    cancellation_requested BOOLEAN NOT NULL DEFAULT false,
    cancellation_reason TEXT,
    namespace TEXT NOT NULL DEFAULT 'default',
    parent_workflow_id TEXT,
    query_state JSONB DEFAULT '{}',
    sticky_worker_id TEXT,
    trace_id TEXT,
    FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
);
```

| Column | Type | Description |
|--------|------|-------------|
| `id` | TEXT | Unique workflow instance ID (UUID) |
| `def_name` | TEXT | References `workflow_defs.name` |
| `def_version` | INTEGER | References `workflow_defs.version` |
| `status` | TEXT | `ready`, `running`, `completed`, `failed`, `suspended` |
| `input` | JSONB | Workflow input arguments |
| `assigned_to` | TEXT | Worker ID currently claiming this instance |
| `heartbeat_at` | TIMESTAMPTZ | Last heartbeat from the claiming worker |
| `next_wake_at` | TIMESTAMPTZ | When to retry (sleep/suspend/resume deadline) |
| `result` | JSONB | Workflow result (if completed) |
| `error_msg` | TEXT | Error message (if failed) |
| `cancellation_requested` | BOOLEAN | Whether cancellation has been requested |
| `cancellation_reason` | TEXT | Reason for cancellation |
| `namespace` | TEXT | Namespace for multi-tenant routing |
| `parent_workflow_id` | TEXT | Parent workflow for child workflows |
| `query_state` | JSONB | Queryable workflow state |
| `sticky_worker_id` | TEXT | Preferred worker for cache locality |
| `trace_id` | TEXT | OpenTelemetry trace ID for observability |

**Indexes**:

- `idx_instances_ready` on `(status, next_wake_at)` WHERE `status = 'ready'` --
  accelerates the worker poll loop. This is the most critical index for worker
  throughput.
- `idx_instances_heartbeat` on `(assigned_to, heartbeat_at)` WHERE
  `status = 'running'` -- enables monitoring and stale-assignment detection.
- `idx_instances_stale` on `(status, heartbeat_at)` WHERE `status = 'running'` --
  used by the reaper to reclaim instances with stale heartbeats.
- `idx_instances_namespace_ready` on `(namespace, status, next_wake_at)` WHERE
  `status = 'ready'` -- namespace-filtered claim lookups.
- `idx_instances_sticky` on `(sticky_worker_id)` WHERE `sticky_worker_id IS NOT
  NULL` -- sticky worker fast path.

### event_history

Ordered list of every cleat call, sleep, signal, defer, and child workflow
event. This is the core of the replay mechanism.

```sql
CREATE TABLE event_history (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    step INTEGER NOT NULL,
    event_type TEXT NOT NULL DEFAULT 'call',
    service TEXT,
    operation TEXT,
    request JSONB,
    response JSONB,
    error TEXT,
    duration_ms BIGINT,
    signal_names TEXT,
    timeout_ms BIGINT,
    signal_name TEXT,
    signal_payload JSONB,
    defer_description TEXT,
    defer_id TEXT,
    child_name TEXT,
    child_input JSONB,
    run_id TEXT,
    new_input JSONB,
    plugin_name TEXT,
    plugin_func TEXT,
    plugin_input JSONB,
    plugin_output JSONB,
    plugin_error TEXT,
    promise_name TEXT,
    promise_id TEXT,
    promise_result TEXT,
    promise_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, step)
);
```

The `event_type` column classifies each event. Supported types include:
`call`, `sleep`, `await_signals`, `signal_received`, `defer`,
`child_workflow`, `await_child`, `continue_as_new`, `heartbeat`,
`plugin_call`, `create_promise`, `await_promise`, `plugin_call_stream_chunk`,
`run_detached`, and others.

Events are appended in sequential order. During replay, the engine walks
events by `step` number, returning cached responses for completed calls and
executing real calls for the first uncompleted step.

### workflow_signals

External signals delivered to running workflows.

```sql
CREATE TABLE workflow_signals (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    signal_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, signal_name)
);
```

The engine checks for signals during `AwaitSignals` and `PollSignal`. Signal
delivery is recorded in `event_history` as `signal_received` events for
deterministic replay.

### Additional Tables

#### workflow_schedules

Cron-based recurring workflow execution. Created via `cleat schedule add` or
the REST API.

```sql
CREATE TABLE workflow_schedules (
    name TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    entry_point TEXT NOT NULL DEFAULT '',
    cron_expression TEXT NOT NULL,
    input JSONB NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT true,
    next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_run_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### workflow_promises

Inter-workflow promise coordination (for cross-workflow data passing).

```sql
CREATE TABLE workflow_promises (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    promise_id TEXT NOT NULL,
    promise_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, promise_id)
);
```

#### concurrency_keys

Per-key concurrency control -- ensures only one workflow holds a given key at
a time.

```sql
CREATE TABLE concurrency_keys (
    key_hash BYTEA PRIMARY KEY,
    key_text TEXT NOT NULL,
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
```

#### workflow_update_requests

Update handler requests (in-flight workflow mutations).

```sql
CREATE TABLE workflow_update_requests (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    update_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    promise_id TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, update_name)
);
```

## Key Indexes Summary

| Index | Table | Purpose | Uniqueness |
|-------|-------|---------|------------|
| `idx_instances_ready` | `workflow_instances` | Worker poll loop: find runnable instances | Non-unique, partial |
| `idx_instances_namespace_ready` | `workflow_instances` | Namespace-scoped poll loop | Non-unique, partial |
| `idx_instances_heartbeat` | `workflow_instances` | Heartbeat monitoring | Non-unique, partial |
| `idx_instances_stale` | `workflow_instances` | Reaper: stale heartbeat detection | Non-unique, partial |
| `idx_instances_sticky` | `workflow_instances` | Sticky worker fast path | Non-unique, partial |
| `idx_defs_active` | `workflow_defs` | Latest-version lookup | Non-unique |
| `idx_promises_status` | `workflow_promises` | Promise resolution lookup | Non-unique |
| `idx_concurrency_keys_workflow` | `concurrency_keys` | Key-to-workflow lookup | Non-unique |
| `idx_update_requests_pending` | `workflow_update_requests` | Pending update lookup | Non-unique |

## Migration Strategy

### Current State

Schema migrations are currently **manual**. There is no automated migration
tool. Changes are applied by running `migrations/postgres/*.sql` (which use
`CREATE TABLE IF NOT EXISTS` and `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`
for idempotent application).

### Future Plans

- **Auto-migration** at worker startup: the worker will check the schema
  version and apply pending migrations before entering the dispatch loop.
- **Versioned migrations**: each migration will be a numbered SQL file in a
  `migrations/` directory with an up/down pair.
- **Plugin migrations**: plugins implementing `plugin.HasMigrations` can
  register their own migrations, which are applied during plugin initialization
  in dependency order.

## Connection Management

The worker uses Go's `database/sql` connection pool:

```go
db.SetMaxOpenConns(concurrency + 5)   // Allow headroom for heartbeats, etc.
db.SetMaxIdleConns(5)
db.SetConnMaxLifetime(5 * time.Minute)
```

### Sharded Deployments

For multi-tenant deployments, the worker supports sharded databases. Each
shard has its own connection string and tenant assignment. The sharded store
dispatches workflow operations to the correct shard by tenant ID. See
`engine/sharded_store.go` and `cmd/cleat-worker/main.go` for
implementation details.

---

## Cross-Database Type Mappings

The project now supports three database backends: PostgreSQL 16+, MySQL 8.0+, and
SQL Server 2017+. This section documents how types and SQL patterns map between
them.

### Data Type Mapping

| PostgreSQL | MySQL 8.0+ | SQL Server 2017+ | Notes |
|---|---|---|---|
| `UUID` | `CHAR(36)` | `UNIQUEIDENTIFIER` | UUIDs generated in Go via `uuid.New()`. PostgreSQL can also use `gen_random_uuid()`; MSSQL can use `NEWID()`. |
| `TEXT` (PK or small column) | `VARCHAR(255)` | `NVARCHAR(64)` / `NVARCHAR(255)` | MySQL requires `VARCHAR` (not `TEXT`) for primary keys. MSSQL uses `NVARCHAR(64)` for UUID IDs, `NVARCHAR(255)` for names. |
| `TEXT` (data column) | `TEXT` | `NVARCHAR(MAX)` | |
| `JSONB` | `JSON` | `NVARCHAR(MAX)` + `ISJSON` check | MySQL `JSON` is not binary-optimized like PostgreSQL `JSONB`. MSSQL enforces JSON via `CHECK (ISJSON(col) = 1)` constraints. |
| `BYTEA` | `LONGBLOB` | `VARBINARY(MAX)` | |
| `BYTEA` (hash/fixed-size) | `VARBINARY(64)` | `VARBINARY(32)` | SHA-256 hash keys use `BYTEA` in PG, `VARBINARY(64)` in MySQL, `VARBINARY(32)` in MSSQL. |
| `TIMESTAMPTZ` | `TIMESTAMP(6)` | `DATETIMEOFFSET` | MySQL stores UTC without timezone awareness. PostgreSQL and MSSQL preserve timezone offset. |
| `BOOLEAN` | `TINYINT(1)` | `BIT` | MySQL uses `0`/`1` integers; MSSQL uses `0`/`1` bits. |
| `TEXT[]` | `JSON` (array) | `NVARCHAR(MAX)` (JSON array) | No native array type in MySQL or MSSQL. Stored as JSON arrays (e.g., `'["place_order","cancel_order"]'`). |
| `INTEGER` | `INTEGER` | `INT` | Identical semantics across all three. |
| `BIGINT` | `BIGINT` | `BIGINT` | Identical semantics across all three. |
| `DOUBLE PRECISION` | `DOUBLE` | `FLOAT(53)` | Not currently used in migration files, but listed for completeness. |
| `SERIAL` | `INT AUTO_INCREMENT` | `INT IDENTITY(1,1)` | Not currently used — all primary keys are application-generated UUIDs or composite keys. |
| `BIGSERIAL` | `BIGINT AUTO_INCREMENT` | `BIGINT IDENTITY(1,1)` | Not currently used — all primary keys are application-generated UUIDs or composite keys. |

### Default Value Differences

| PostgreSQL | MySQL 8.0+ | SQL Server 2017+ |
|---|---|---|
| `now()` | `NOW(6)` | `SYSUTCDATETIME()` |
| `gen_random_uuid()` | Go-side `uuid.New().String()` | `NEWID()` or Go-side |
| `now() + INTERVAL '7 days'` | `NOW(6) + INTERVAL 7 DAY` | `DATEADD(DAY, 7, SYSUTCDATETIME())` |
| `'{}'::jsonb` | `('{}')` | `'{}'` with `ISJSON` constraints |
| `'{}'::text[]` | `('[]')` | `'[]'` |

### Key SQL Translation Table

| Operation | PostgreSQL | MySQL 8.0+ | SQL Server 2017+ |
|---|---|---|---|
| Claim (skip locked) | `SELECT ... FOR UPDATE SKIP LOCKED` | `SELECT ... FOR UPDATE SKIP LOCKED` | `UPDATE ... SET ... OUTPUT INSERTED.* WHERE id IN (SELECT id FROM ... WITH (READPAST, UPDLOCK, ROWLOCK) ... OFFSET 0 ROWS FETCH NEXT N ROWS ONLY)` |
| Upsert (no-op on conflict) | `INSERT ... ON CONFLICT DO NOTHING` | `INSERT IGNORE` | `INSERT ... SELECT ... WHERE NOT EXISTS (SELECT 1 FROM ...)` |
| Upsert (update on conflict) | `INSERT ... ON CONFLICT DO UPDATE SET ...` | `INSERT ... ON DUPLICATE KEY UPDATE ...` | `MERGE target AS t USING source AS s ON ... WHEN MATCHED THEN UPDATE ... WHEN NOT MATCHED THEN INSERT ...` |
| Return inserted/updated ID | `RETURNING id` | App-generated UUID (two-step: SELECT...FOR UPDATE then UPDATE) [1] | `OUTPUT INSERTED.id` |
| Case-insensitive match | `ILIKE` | `LOWER(col) LIKE LOWER(?)` | `LOWER(col) LIKE LOWER(?)` |
| JSON field access | `col->>'field'` | `JSON_EXTRACT(col, '$.field')` | `JSON_VALUE(col, '$.field')` |
| Hash function | `digest('sha256', ...)` (pgcrypto extension) | Go-side `sha256.Sum256()` | `HASHBYTES('SHA2_256', ...)` (native) or Go-side |
| UUID generation | `gen_random_uuid()` | Go-side `uuid.New()` | `NEWID()` or Go-side |
| Pagination | `LIMIT ? OFFSET ?` | `LIMIT ? OFFSET ?` | `OFFSET ? ROWS FETCH NEXT ? ROWS ONLY` |
| Create if not exists | `CREATE TABLE IF NOT EXISTS` | `CREATE TABLE IF NOT EXISTS` | `IF NOT EXISTS (SELECT 1 FROM sys.objects WHERE ...) CREATE TABLE` |
| Add column if not exists | `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` | `ALTER TABLE ... ADD COLUMN` (with manual check) | `IF NOT EXISTS (SELECT 1 FROM sys.columns WHERE ...) ALTER TABLE ... ADD` |
| Drop index if exists | `DROP INDEX IF EXISTS` | `DROP INDEX ... ON ...` (no IF EXISTS) | `IF EXISTS (SELECT 1 FROM sys.indexes WHERE ...) DROP INDEX` |
| Drop column if exists | `ALTER TABLE ... DROP COLUMN IF EXISTS` | Manual existence check | `IF EXISTS (SELECT 1 FROM sys.columns WHERE ...) ALTER TABLE ... DROP COLUMN` |
| Make column NOT NULL | `ALTER TABLE ... ALTER COLUMN ... SET NOT NULL` | `ALTER TABLE ... MODIFY COLUMN ... NOT NULL` | `ALTER TABLE ... ALTER COLUMN ... NOT NULL` |
| Duplicate key detection | `ON CONFLICT` (no separate check) | Error code 1062 check in Go (`isDuplicateKeyError`) | Error code 2627 check in Go |

**Footnotes**:

1. MySQL does not support `UPDATE ... RETURNING`. The claim pattern uses a two-step
   approach: (a) `SELECT id ... FOR UPDATE SKIP LOCKED`, then (b) `UPDATE ... WHERE id IN (...)`.
   See `engine/mysql_store.go`.

### Index Differences

| Feature | PostgreSQL 16+ | MySQL 8.0+ | SQL Server 2017+ |
|---|---|---|---|
| Partial indexes (WHERE clause) | Yes | **No** — partial indexes are omitted; application code adds the filter to queries | Yes — filtered indexes with deterministic predicates only |
| Covering indexes (INCLUDE) | Yes (11+) | **No** — columns must be in the index key | Yes |
| JSON indexes | GIN index on `JSONB` column | Generated column + index (can't index `JSON` directly) | Computed column + index (can't index `NVARCHAR(MAX)` JSON directly) |
| DESC in index | Supported | Ignored (direction does not affect B-tree in InnoDB) | Supported |
| Unique constraints on nullable columns | Multiple NULLs allowed | Multiple NULLs allowed (InnoDB) | Only one NULL allowed (historically; `CREATE UNIQUE INDEX ... WHERE col IS NOT NULL` can work around) |

**Index Migration Notes**:

- PostgreSQL partial indexes (e.g., `WHERE status = 'ready'`) are **omitted** in
  MySQL. The application layer (`mysql_store.go`) adds the filter condition
  directly in queries.
- Indexes on `UUID`/`CHAR(36)` columns use the full column width in MySQL
  (no prefix length needed for `CHAR`).
- MSSQL filtered indexes cannot use non-deterministic functions like
  `SYSUTCDATETIME()` in the predicate. Expiration-cleanup queries use a regular
  index and filter at query time instead.

### Row-Level Security Comparison

| Aspect | PostgreSQL | MySQL 8.0+ | SQL Server 2017+ |
|---|---|---|---|
| Mechanism | `CREATE POLICY ... FOR ALL USING (tenant_id = current_setting('cleat.tenant_id')::uuid)` | Not available — application-layer `WHERE tenant_id = ?` on every query | `CREATE SECURITY POLICY ... ADD FILTER PREDICATE dbo.fn_tenant_filter() ON dbo.<table>` |
| Session context | `current_setting('cleat.tenant_id', true)` | N/A | `SESSION_CONTEXT(N'tenant_id')` |
| Predicate function | Inline policy expression | N/A | Inline TVF returning `1` when `SESSION_CONTEXT` matches |
| Bypass | Superuser | N/A | `IS_MEMBER('db_owner') = 1` |
| Fail-closed | Yes (NULL context returns no rows) | Yes (queries without tenant filter return no rows for other tenants) | Yes (unset context returns no rows) |
| Block predicates | Not implemented (filter only) | N/A | Yes — `ADD BLOCK PRECATE` prevents INSERT/UPDATE of wrong-tenant rows |

### Checking Type Equivalents in Migrations

Each backend maintains a parallel set of migration files in:

```
migrations/postgres/     -- Canonical PostgreSQL DDL
migrations/mysql/        -- MySQL 8.0+ port (with MySQL differences documented in comments)
migrations/mssql/        -- SQL Server 2017+ / Azure SQL port (T-SQL)
```

Each MySQL migration file includes a comment block at the top documenting all
MySQL-specific deviations from the PostgreSQL original. For example, from
`migrations/mysql/001_initial_schema.sql`:

```sql
-- MySQL differences from PostgreSQL:
--   - TEXT columns used as primary keys are VARCHAR(255)
--   - JSONB becomes JSON
--   - BYTEA becomes LONGBLOB
--   - TIMESTAMPTZ becomes TIMESTAMP(6) (stored as UTC, no timezone)
--   - BOOLEAN becomes TINYINT(1)
--   - Partial indexes (WHERE clause) are omitted
--   - TEXT[] becomes JSON (stored as JSON array)
```

The Go store implementations that translate queries for each backend are at:

- `engine/store.go` — common interface and shared logic
- `engine/mysql_store.go` — MySQL query translations
- `engine/mssql_store.go` — MSSQL query translations


## Which role to connect as

**Do not point `--db` at a superuser.** Tenant isolation depends on it.

Every tenant-scoped table has row-level security enabled and `FORCE`d
(`001_schema.sql`), and for `GetWorkflowByID` and `ListWorkflows` those policies
are the *only* isolation there is — neither carries an application-level
`tenant_id` filter. PostgreSQL never applies row-level security to a superuser,
and there is no setting that changes that. A superuser connection therefore
returns every tenant's data from those calls, however the policies are written.

`005_app_role.sql` creates the role to use instead: `cleat_app`, which owns
nothing, has no DDL rights, and is `NOSUPERUSER NOBYPASSRLS`. Ownership matters
as much as superuser here — an owner is exempt from its own policies unless
`FORCE` is set, so a role that owns nothing is subject to them unconditionally,
rather than depending on a flag a later change could clear.

The role is created `NOLOGIN` and without a password: a credential does not
belong in a file that is committed, mounted into containers, and re-applied by
every worker at boot. Give it one at deploy time:

```sql
ALTER ROLE cleat_app LOGIN PASSWORD '...';
```

`deploy/postgres/900-app-role.sh` does this for `docker-compose.cluster.yml`
from `CLEAT_APP_PASSWORD`, and refuses to proceed if that variable is unset —
so a missing password fails the deployment rather than quietly leaving the
cluster on a superuser connection.

Migrations still need DDL rights, so workers take two DSNs:

```
--db=postgres://cleat_app:...@host:5432/cleat        # runtime, unprivileged
--migrate-db=postgres://owner:...@host:5432/cleat    # schema only
```

`--migrate-db` defaults to `--db`, so a deployment that has not been split
keeps working as before.

### The startup check

On PostgreSQL the worker verifies at boot that its runtime connection really is
subject to the policies, and reports every reason it is not — superuser,
`BYPASSRLS`, row-level security switched off on a table that has policies, or
ownership without `FORCE`. It also reports a database with no policies at all,
which would otherwise pass every other check while isolating nothing.

`--rls-check` controls what happens next:

| Value | Behaviour |
|---|---|
| `auto` (default) | Refuse to start when `--require-auth` is set; warn otherwise |
| `require` | Always refuse to start |
| `off` | Skip the check |
