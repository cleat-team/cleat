# Multi-Database Implementation Plan

May 2026 — plan for production-quality MySQL/MariaDB and SQL Server support
alongside the existing PostgreSQL backend.

---

## Scope

This plan covers implementing two new `WorkflowStore` backends — **MySQL 8.0+**
(including MariaDB 10.6+) and **SQL Server 2017+** (including Azure SQL
Database) — at production quality. "Production quality" means: full 72-method
interface implementation, complete tenant isolation, migration files, CI testing
against real database instances, error code mapping with retry logic, and
documentation. It does NOT include a migration DSL — that is a separate project.

### What's already done (Phase 1 de-leaking)

The `WorkflowStore` interface is clean. The `StoreFactory` pattern is in place.
No `database/sql` or `lib/pq` types leak through the interface. Callers only see
`WorkflowStore` and `StoreFactory`. `ShardedStore` wraps `WorkflowStore` — any
backend can be sharded. `PluginDB` is backend-agnostic.

**Starting point**: 2,900 lines of `PostgresStore`, 10 PostgreSQL migration
files, 17 plugin migration files (also PostgreSQL-specific), test infrastructure
hardcoded to PostgreSQL.

---

## Stream Map

```
Stream A: Shared test suite          ────────┐
Stream B: MySQL/MariaDB backend      ────────┤  These three run in parallel
Stream C: SQL Server backend         ────────┘
Stream D: Migration infrastructure   ── after A+B+C stabilized
Stream E: Tenant isolation per DB    ── after B+C core methods pass
Stream F: CI/CD and operations       ── after A+B+C pass locally
Stream G: Documentation              ── throughout
```

Streams A, B, and C are independent and can run in parallel. Streams D–F depend
on A+B+C being stable. Stream G runs throughout.

---

## Stream A: Shared Test Suite (golden contract)

**Goal:** A single test suite that any `WorkflowStore` implementation must pass.
This is the deliverable that proves each backend is correct.

**File:** `internal/host/store_test.go` (new)
**Backend registration:** `internal/host/store_backends_test.go` (new)

### A.1 Backend Registration (0.5 day)

```go
// store_backends_test.go
type StoreBackend interface {
    Name() string
    Setup(t *testing.T) (WorkflowStore, func())
    Enabled() bool  // checks env var
}

var registeredBackends []StoreBackend

func RegisterBackend(b StoreBackend) { ... }

// Each backend registers itself via init() guarded by env var:
//   CLEAT_TEST_MYSQL=user:pass@tcp(localhost:3306)/cleat
//   CLEAT_TEST_MSSQL=sqlserver://user:pass@localhost:1433/cleat
```

### A.2 Test Cases (3 days)

The test suite covers every critical path. Tests run against ALL registered
backends via `t.Run(backend.Name(), ...)`.

**Group 1 — Claim and execute (critical path):**
- `TestClaimWorkflow` — single claim, verify returned data, verify row now "running"
- `TestClaimWorkflows_Batch` — claim N workflows, verify exactly N returned, no duplicates
- `TestClaimStickyWorkflows` — sticky claim with worker filter
- `TestClaimSkipLocked` — two concurrent claimers get disjoint sets (goroutine test)
- `TestNoWorkflowsToClaim` — returns nil, no error when queue is empty

**Group 2 — Exactly-once start:**
- `TestExactlyOnceStart` — two `StartNewRun` calls with same idempotency key return same runID
- `TestExactlyOnceStart_DifferentKeys` — different keys create different runs

**Group 3 — Event history:**
- `TestAppendEventHistory` — single event append, load back, verify fidelity
- `TestAppendEventHistoryBatch` — batch append, verify all events round-trip
- `TestAppendEventHistory_Idempotent` — same step twice, second is no-op
- `TestLoadEventHistoryPaginated` — pagination boundaries, offset/limit
- `TestBinaryDataRoundTrip` — base64-encoded binary payloads survive storage

**Group 4 — Workflow lifecycle:**
- `TestCompleteWorkflow` — status → "completed", result stored
- `TestFailWorkflow` — status → "failed", error fields stored
- `TestReleaseWorkflow` — status → "ready", next_wake_at set
- `TestContinueAsNew_Atomic` — old run completed AND new run created in one transaction; failure rolls back both
- `TestFinalizeWorkflowSegment` — events appended AND status updated atomically
- `TestHeartbeat` — heartbeat_at updated, returns false if worker assignment changed
- `TestBatchHeartbeat` — multiple workflows updated in one call
- `TestMoveToDeadLetterQueue` — terminal dead_lettered status

**Group 5 — Cancellation:**
- `TestRequestCancellation` — cancel flag set
- `TestCheckCancellation` — returns true + reason after cancellation

**Group 6 — Signals:**
- `TestDeliverSignal` — signal stored
- `TestPollAndClaimSignal` — signal consumed atomically, exactly once
- `TestPollSignal_NonDestructive` — signal visible without consumption (if semantics differ from PollAndClaimSignal)

**Group 7 — Child workflows:**
- `TestStartChildWorkflow` — creates child with parent link
- `TestGetChildResult_Completed` — returns result when done
- `TestGetChildResult_NotCompleted` — returns completed=false when running

**Group 8 — Reaping:**
- `TestReapStaleInstances` — stale workflows reset to ready

**Group 9 — List and search:**
- `TestListWorkflows_ByStatus` — filter by status
- `TestListWorkflows_InputContains` — filter by JSON content
- `TestListWorkflows_Pagination` — offset/limit boundaries
- `TestListWorkflows_Search` — text search across fields
- `TestGetWorkflowByID` — direct lookup

**Group 10 — Schedules:**
- `TestCreateSchedule` — schedule stored
- `TestGetDueSchedules` — returns only enabled schedules with next_run_at <= now
- `TestUpdateScheduleNextRun` — next_run_at updated
- `TestSetScheduleEnabled` — toggle enabled flag
- `TestDeleteSchedule` — schedule removed

**Group 11 — Promises:**
- `TestCreatePromise` — promise stored
- `TestResolvePromise` — status → "resolved", result stored
- `TestRejectPromise` — status → "rejected", error stored
- `TestGetPromise` — returns current state
- `TestListPromises` — all promises for workflow ordered by creation

**Group 12 — Update requests:**
- `TestCreateUpdateRequest` — request stored as pending
- `TestGetPendingUpdateRequests` — returns pending only
- `TestCompleteUpdateRequest` — status updated, result/error stored

**Group 13 — Concurrency keys:**
- `TestAcquireConcurrencyKey` — first acquire succeeds, second fails
- `TestAcquireConcurrencyKey_Expired` — expired key can be re-acquired
- `TestReleaseConcurrencyKey` — key freed
- `TestReleaseWorkflowConcurrencyKeys` — all keys for workflow freed
- `TestReapExpiredConcurrencyKeys` — expired keys cleaned up

**Group 14 — Compaction:**
- `TestGetCompactionCandidates` — returns workflows exceeding threshold
- `TestLoadCompactionState` — returns saved state
- `TestCompactHistory` — old events deleted, checkpoint saved

**Group 15 — Version management:**
- `TestDeployWorkflowDef` — WASM bytes stored and retrievable
- `TestListWorkflowDefs` — all versions returned in order
- `TestResolveLatestVersion` — returns max version for name
- `TestValidateVersion` — valid version returns true, nonexistent returns false
- `TestMarkVersionDeprecated` — deprecated flag set
- `TestPurgeWorkflowDef` — definition permanently deleted
- `TestCountActiveInstances` — counts running/ready for version

**Group 16 — Query state:**
- `TestGetQueryState` — key-value state stored and retrieved

**Group 17 — Sticky sessions:**
- `TestUpdateStickyWorker` — sticky worker set
- `TestClearStickyWorker` — sticky worker cleared

**Group 18 — WASM loading:**
- `TestLoadWASM_RoundTrip` — bytes stored via DeployWorkflowDef, loaded via LoadWASM

**Group 19 — Memory stats:**
- `TestRecordWorkflowMemorySample` — sample stored
- `TestLoadMemoryEstimates` — EWMA values returned
- `TestCleanupMemorySamples` — old samples pruned

**Group 20 — Maintenance:**
- `TestQueueDepth` — returns count of ready workflows
- `TestDeleteExpiredEvents` — terminal workflows past cutoff cleaned up

**~50 test cases total.** Each runs against every registered backend.

### A.3 Test Helpers (0.5 day)

- `setupTestData(store)` — creates standard test fixtures (workflow defs, instances in various states, events, promises, schedules)
- `truncateAll(store)` — cleanup between test cases (backend-agnostic, calls delete methods already on the interface)

**Stream A total: ~4 days**

---

## Stream B: MySQL/MariaDB Backend

**Goal:** `MySQLStore` implementing all 72 `WorkflowStore` methods, passing
Stream A's test suite, with production-quality connection management.

**Target:** MySQL 8.0+ and MariaDB 10.6+ (both have `SKIP LOCKED`, JSON type,
window functions, CTEs).

**Primary file:** `internal/host/mysql_store.go` (~2,500 lines estimated)

### B.1 Schema Translation (1.5 days)

Translate 10 core migration files to MySQL DDL. Key translations:

| PostgreSQL | MySQL 8.0+ |
|-----------|------------|
| `UUID` | `CHAR(36)` with `uuid()` default via trigger or Go-generated |
| `BIGSERIAL` | `BIGINT AUTO_INCREMENT` |
| `TEXT` | `TEXT` |
| `JSONB` | `JSON` |
| `BYTEA` | `LONGBLOB` |
| `TIMESTAMPTZ` | `TIMESTAMP(6)` (no timezone; store UTC) |
| `DOUBLE PRECISION` | `DOUBLE` |
| `BOOLEAN` | `TINYINT(1)` |
| `TEXT[]` | Separate junction table or JSON array |
| `gen_random_uuid()` | Go-side `uuid.New()` |
| `digest('sha256')` | Go-side `sha256.Sum256()` |
| `pgcrypto` extension | Not needed (hashing in Go) |
| PL/pgSQL functions | MySQL stored procedures (SQL/PSM) |
| `search_path` | `USE database` per tenant (separate databases) |
| `CREATE SCHEMA` | `CREATE DATABASE` |
| Partial indexes (WHERE clause) | MySQL 8.0.13+ supports functional indexes but not partial. Use full index or application-level filtering |
| `CREATE INDEX ... INCLUDE` | Not supported; use covering indexes differently |
| `RETURNING id` | `LAST_INSERT_ID()` or follow-up SELECT |

**Files created:** `migrations/mysql/001_initial_schema.sql` through
`migrations/mysql/010_workflow_memory_stats.sql`

### B.2 Driver and Connection Management (0.5 day)

- Use `github.com/go-sql-driver/mysql` driver
- `MySQLStoreFactory` implements `StoreFactory`
- Connection string format: `user:pass@tcp(host:port)/dbname?parseTime=true&multiStatements=true`
- Connection pool: `SetMaxOpenConns`, `SetMaxIdleConns`, `SetConnMaxLifetime`
- `time.Time` parsing: `parseTime=true` in DSN

### B.3 Core Claim/Execute Loop (1 day)

MySQL 8.0+ supports `SELECT ... FOR UPDATE SKIP LOCKED` natively. But lacks
`RETURNING`. The claim pattern is two statements in one transaction:

```sql
START TRANSACTION;

-- Select IDs with SKIP LOCKED
SELECT id FROM workflow_instances
WHERE status = 'ready' AND next_wake_at <= NOW(6)
ORDER BY created_at
LIMIT ? FOR UPDATE SKIP LOCKED;

-- Update claimed rows
UPDATE workflow_instances
SET assigned_to = ?, status = 'running', heartbeat_at = DATE_ADD(NOW(6), INTERVAL ? SECOND)
WHERE id IN (?);

-- Read back with condition check (row was actually updated by us)
SELECT * FROM workflow_instances WHERE id IN (?) AND assigned_to = ?;

COMMIT;
```

Methods: `ClaimWorkflow`, `ClaimWorkflows`, `ClaimStickyWorkflows`.

### B.4 Event History (0.5 day)

- `AppendEventHistory`: `INSERT IGNORE` for idempotency (replaces `ON CONFLICT DO NOTHING`)
- `AppendEventHistoryBatch`: batch `INSERT IGNORE`
- `LoadEventHistory`: `SELECT ... ORDER BY step`
- `VerifyWorkflowEvents`: Go-side SHA-256 checksum verification (no `pgcrypto`)

### B.5 Workflow Lifecycle (1 day)

- `StartNewRun`: INSERT with Go-generated UUID, handle idempotency key
- `CompleteWorkflow` / `FailWorkflow` / `ReleaseWorkflow`: UPDATE with optimistic locking (`WHERE status = 'running' AND assigned_to = ?`)
- `ContinueAsNew`: Multi-statement transaction — INSERT new run + UPDATE old run
- `FinalizeWorkflowSegment`: Transaction — INSERT events + UPDATE status
- `Heartbeat` / `BatchHeartbeat`: UPDATE heartbeat_at
- `MoveToDeadLetterQueue`: UPDATE status

### B.6 Signals, Promises, Updates (0.5 day)

- `PollAndClaimSignal`: `SELECT ... FOR UPDATE` + `DELETE` in transaction
- `DeliverSignal`: INSERT
- Promise CRUD: Straightforward INSERT/UPDATE/SELECT
- Update requests: Same pattern

### B.7 Schedules and Background (0.5 day)

- `GetDueSchedules`: SELECT with `NOW(6)` comparison
- `ReapStaleInstances`: UPDATE with subquery join
- `Compaction`: DELETE + INSERT in transaction

### B.8 List/Search (0.5 day)

- `ListWorkflows`: Dynamic query building with `LIKE` (case-insensitive via `LOWER()` or `COLLATE utf8mb4_general_ci`)
- `ILIKE` → `LOWER(col) LIKE LOWER(?)`
- JSON path queries: `JSON_EXTRACT(col, '$.field')` instead of `col->>'field'`

### B.9 Memory Stats, Queue Depth, Maintenance (0.5 day)

- Percentile computation in Go (no native `PERCENTILE_CONT` in MySQL)
- `QueueDepth`: `SELECT COUNT(*)`
- `DeleteExpiredEvents`: DELETE with JOIN

### B.10 Integration Testing and Bug Fixing (2 days)

- Run Stream A test suite against MySQLStore
- Fix edge cases: deadlock retry, connection loss, driver-specific behavior
- Error code mapping: MySQL error codes → standard sentinel errors
- `Error 1213` (deadlock) → retryable
- `Error 1062` (duplicate key) → idempotent/conflict
- `Error 1205` (lock wait timeout) → retryable

**Stream B total: ~9 days**

---

## Stream C: SQL Server Backend

**Goal:** `MSSQLStore` implementing all 72 `WorkflowStore` methods, passing
Stream A's test suite, with production-quality connection management.

**Target:** SQL Server 2017+ and Azure SQL Database (both have `READPAST`,
`OUTPUT`, `MERGE`, JSON functions, window functions).

**Primary file:** `internal/host/mssql_store.go` (~2,400 lines estimated)

### C.1 Schema Translation (1.5 days)

Translate 10 core migration files to T-SQL. Key translations:

| PostgreSQL | SQL Server 2017+ |
|-----------|-----------------|
| `UUID` | `UNIQUEIDENTIFIER` |
| `BIGSERIAL` | `BIGINT IDENTITY(1,1)` |
| `TEXT` | `NVARCHAR(MAX)` |
| `JSONB` | `NVARCHAR(MAX)` (with `ISJSON` check constraint) |
| `BYTEA` | `VARBINARY(MAX)` |
| `TIMESTAMPTZ` | `DATETIMEOFFSET` |
| `DOUBLE PRECISION` | `FLOAT(53)` |
| `BOOLEAN` | `BIT` |
| `TEXT[]` | Separate table or `NVARCHAR(MAX)` with JSON array |
| `gen_random_uuid()` | `NEWID()` or Go-side |
| `digest('sha256')` | `HASHBYTES('SHA2_256', ...)` (native!) |
| `pgcrypto` extension | Not needed (`HASHBYTES` built in) |
| PL/pgSQL functions | T-SQL stored procedures |
| `search_path` | `EXECUTE AS USER` or `sp_set_session_context` |
| Partial indexes | Filtered indexes (`CREATE INDEX ... WHERE ...`) — SQL Server supports these natively |
| `CREATE INDEX ... INCLUDE` | Same syntax — SQL Server originated this |
| `RETURNING` | `OUTPUT INSERTED.*` / `OUTPUT DELETED.*` |
| `FOR UPDATE SKIP LOCKED` | `WITH (READPAST, UPDLOCK, ROWLOCK)` table hint |
| `ON CONFLICT DO NOTHING` | `MERGE ... WHEN NOT MATCHED THEN INSERT` or `IF NOT EXISTS (SELECT ...) INSERT` |

**Files created:** `migrations/mssql/001_initial_schema.sql` through
`migrations/mssql/010_workflow_memory_stats.sql`

### C.2 Driver and Connection Management (0.5 day)

- Use `github.com/microsoft/go-mssqldb` driver
- `MSSQLStoreFactory` implements `StoreFactory`
- Connection string format: `sqlserver://user:pass@host:1433?database=cleat`
- Azure SQL: `Authentication=ActiveDirectoryDefault` for managed identity
- Connection pool: `SetMaxOpenConns`, `SetMaxIdleConns`, `SetConnMaxLifetime`
- Transient fault handling: Azure SQL disconnect resilience with retry policy

### C.3 Core Claim/Execute Loop (1 day)

SQL Server can do the claim in a SINGLE atomic statement using `UPDATE ... OUTPUT`:

```sql
UPDATE TOP(@limit) w
SET assigned_to = @workerID,
    status = 'running',
    heartbeat_at = DATEADD(SECOND, @timeout, SYSUTCDATETIME())
OUTPUT INSERTED.id, INSERTED.def_name, INSERTED.def_version,
       INSERTED.input, INSERTED.min_version, INSERTED.tenant_id,
       INSERTED.next_wake_at, INSERTED.created_at
FROM workflow_instances w WITH (READPAST, UPDLOCK, ROWLOCK)
WHERE w.status = 'ready'
  AND w.next_wake_at <= SYSUTCDATETIME()
ORDER BY w.created_at;
```

This is architecturally cleaner than both PostgreSQL (SELECT FOR UPDATE) and
MySQL (two-statement transaction). One statement. Atomic. No race between
SELECT and UPDATE.

Methods: `ClaimWorkflow`, `ClaimWorkflows`, `ClaimStickyWorkflows` (add sticky worker filter).

### C.4 Event History (0.5 day)

- `AppendEventHistory`: `MERGE` statement for upsert semantics
- `AppendEventHistoryBatch`: `MERGE` with table-valued parameter or multiple VALUES
- `LoadEventHistory`: `SELECT ... ORDER BY step OFFSET ... FETCH NEXT ...`
- `VerifyWorkflowEvents`: `HASHBYTES('SHA2_256', ...)` — native hashing, no Go-side workaround needed

### C.5 Workflow Lifecycle (0.5 day)

- `StartNewRun`: INSERT with Go-generated UUID or `NEWID()`
- `CompleteWorkflow` / `FailWorkflow` / `ReleaseWorkflow`: UPDATE with OUTPUT for optimistic locking verification
- `ContinueAsNew`: `BEGIN TRANSACTION` + INSERT + UPDATE + `COMMIT`
- `FinalizeWorkflowSegment`: Same transaction pattern
- `Heartbeat` / `BatchHeartbeat`: UPDATE
- `MoveToDeadLetterQueue`: UPDATE

### C.6 Signals, Promises, Updates (0.5 day)

- `PollAndClaimSignal`: `DELETE ... OUTPUT DELETED.payload` — single atomic statement (cleaner than PostgreSQL's SELECT + DELETE)
- `DeliverSignal`: INSERT
- Promise and Update CRUD: Straightforward

### C.7 Schedules and Background (0.5 day)

- `GetDueSchedules`: SELECT with `SYSUTCDATETIME()`
- `ReapStaleInstances`: UPDATE with correlated subquery
- `Compaction`: DELETE + INSERT in transaction

### C.8 List/Search (0.5 day)

- `ListWorkflows`: Dynamic query building
- `ILIKE` → `LOWER(col) LIKE LOWER(?)` (no case-insensitive LIKE collation)
- JSON path queries: `JSON_VALUE(col, '$.field')` instead of `col->>'field'`
- Pagination: `OFFSET ... FETCH NEXT ... ROWS ONLY`

### C.9 Memory Stats, Queue Depth, Maintenance (0.5 day)

- `PERCENTILE_CONT()` available in SQL Server 2022+ / Azure SQL — use native
- `QueueDepth`: `SELECT COUNT(*)`
- `DeleteExpiredEvents`: DELETE with EXISTS subquery

### C.10 Integration Testing and Bug Fixing (2 days)

- Run Stream A test suite against MSSQLStore
- Fix edge cases: deadlock retry (error 1205), connection resiliency
- Error code mapping: SQL Server error codes → standard sentinel errors
- Error 1205 (deadlock victim) → retryable
- Error 2627 (unique constraint violation) → idempotent/conflict
- Error 3960 (snapshot isolation) → retryable

**Stream C total: ~8.5 days**

---

## Stream D: Migration Infrastructure

**Goal:** A migration runner that can apply the correct dialect-specific
migration files based on the active backend. Does NOT build a full migration
DSL — that's a separate project. This is the minimum viable multi-dialect
migration support.

**Files:** `internal/host/migrate.go` (refactored), `migrations/postgres/`,
`migrations/mysql/`, `migrations/mssql/`

### D.1 Reorganize Migration Files (0.5 day)

Move existing migration files into dialect directories:

```
migrations/
├── postgres/
│   ├── 001_initial_schema.sql
│   ├── 002_tenant_foundation.sql
│   ├── ...
│   └── 011_add_event_checksum.sql
├── mysql/
│   ├── 001_initial_schema.sql
│   ├── ...
│   └── 010_workflow_memory_stats.sql
└── mssql/
    ├── 001_initial_schema.sql
    ├── ...
    └── 010_workflow_memory_stats.sql
```

Update `internal/plugin/migration.go` to look up dialect-specific directories.
The migration tracking table (`cleat_migrations`) already tracks which
migrations have been applied.

### D.2 Dialect Selection (0.5 day)

Add a `Dialect` type to `StoreFactory`:

```go
type Dialect string

const (
    DialectPostgres Dialect = "postgres"
    DialectMySQL    Dialect = "mysql"
    DialectMSSQL    Dialect = "mssql"
)

type StoreFactory interface {
    OpenStore(ctx context.Context, taskQueues ...string) (WorkflowStore, io.Closer, error)
    DriverName() string
    Dialect() Dialect    // NEW
}
```

Update `PostgresStoreFactory` to return `DialectPostgres`. Callers pass dialect
to `RunMigrations()`.

### D.3 Plugin Migration Handling (0.5 day)

Plugin migrations are currently PostgreSQL-specific SQL embedded in Go files.
Option: each plugin migration file checks the dialect and returns the right SQL.
Or: plugins export separate SQL files per dialect in their directory. The
latter is cleaner but more boilerplate.

For now, accept that plugin migrations are PostgreSQL-only and document this as
a known limitation. Plugins that need cross-DB support add dialect-specific
migration files later. This avoids blocking the core store work on plugin
changes.

### D.4 Test Schema Setup (1 day)

Refactor `testutil/schema.go` to be dialect-aware:
- `SetupFullSchema(t, dialect)` — applies correct migration set
- `TestDB(t, dialect)` — returns `*sql.DB` connected to the right test database
- Environment variables: `CLEAT_TEST_POSTGRES`, `CLEAT_TEST_MYSQL`, `CLEAT_TEST_MSSQL`

**Stream D total: ~2.5 days**

---

## Stream E: Tenant Isolation per Backend

**Goal:** Each backend enforces tenant isolation at a level appropriate to its
capabilities.

### E.1 PostgreSQL (done, no changes)

RLS + `search_path` schema routing. The gold standard.

### E.2 MySQL — Application-Level Isolation (2 days)

MySQL has no RLS. Tenant isolation is enforced in the application layer.

**Approach:** Separate database per tenant (`cleat_<tenant_id>`).

- `MySQLStoreFactory` holds a map of tenant → `*sql.DB` (connection pool per tenant database)
- `OpenStore` takes a `tenantID` parameter (add to `StoreFactory` interface or pass via context)
- Every query operates against the tenant-specific connection, which is scoped to the tenant's database
- Cross-tenant operations (e.g., the admin API) use a master connection and query across databases

**Security model:** Isolation is at the database level (separate databases).
A missing tenant filter can't leak data because the connection itself is scoped
to one database. This is stronger than per-query `WHERE tenant_id = ?` filtering
but weaker than PostgreSQL RLS (which blocks writes to the wrong tenant even
within the same schema).

**Implementation:**
- `MySQLStore` holds a reference to its tenant database
- `MySQLStoreFactory.OpenStore` creates a new connection pool for the tenant database
- `CreateTenantDatabase()` method on the factory for provisioning

### E.3 SQL Server — RLS (1 day)

SQL Server has RLS comparable to PostgreSQL.

**Approach:** `sp_set_session_context 'tenant_id', @tenantID` at connection open,
`SESSION_CONTEXT('tenant_id')` in RLS policies.

```sql
-- Set at connection open
EXEC sp_set_session_context N'tenant_id', @tenantID;

-- RLS predicate function
CREATE FUNCTION dbo.fn_tenant_filter()
RETURNS TABLE WITH SCHEMABINDING
AS RETURN
  SELECT 1 AS fn_tenant_filter_result
  WHERE tenant_id = CAST(SESSION_CONTEXT(N'tenant_id') AS UNIQUEIDENTIFIER)
   OR IS_MEMBER('db_owner') = 1;  -- admin bypass

-- Security policy (one per table)
CREATE SECURITY POLICY dbo.TenantFilter_Instances
  ADD FILTER PREDICATE dbo.fn_tenant_filter() ON dbo.workflow_instances,
  ADD BLOCK PREDICATE dbo.fn_tenant_filter() ON dbo.workflow_instances;
```

**Implementation:**
- `MSSQLStoreFactory.OpenStore` calls `sp_set_session_context` after opening connection
- Migration 002 (`tenant_foundation`) translates RLS policies to T-SQL
- `MSSQLStore` is otherwise identical — RLS is transparent to queries

**Stream E total: ~3 days** (2 MySQL, 1 SQL Server)

---

## Stream F: CI/CD and Operations

**Goal:** CI runs the shared test suite against all three databases on every
PR. Real database instances, not mocks.

### F.1 CI Workflow (1 day)

New workflow: `.github/workflows/multi-db-ci.yml`

```yaml
jobs:
  test-postgres:  # existing, unchanged
  test-mysql:
    services:
      mysql:
        image: mysql:8.4
        env: MYSQL_ROOT_PASSWORD=cleat, MYSQL_DATABASE=cleat
    steps:
      - run: CLEAT_TEST_MYSQL=root:cleat@tcp(127.0.0.1:3306)/cleat go test ./internal/host/...
  test-mssql:
    services:
      mssql:
        image: mcr.microsoft.com/mssql/server:2022-latest
        env: ACCEPT_EULA=Y, MSSQL_SA_PASSWORD=CleatTest123!
    steps:
      - run: CLEAT_TEST_MSSQL=sqlserver://sa:CleatTest123!@127.0.0.1:1433?database=master go test ./internal/host/...
```

### F.2 Local Development Setup (0.5 day)

Update `docker-compose.cluster.yml` (or create `docker-compose.dev.yml`) with
MySQL and SQL Server services for local testing.

Add `make test-all-dbs` target that runs the test suite against all three.

### F.3 Benchmarking Infrastructure (0.5 day)

Extend `cmd/cleat-bench/main.go` to accept a `--driver` flag (`postgres`,
`mysql`, `mssql`). Run the same benchmark workload against all three backends.
Capture results for the launch blog post.

**Stream F total: ~2 days**

---

## Stream G: Documentation

**Goal:** Users can find, configure, and trust each database backend.

### G.1 Multi-Database Guide (1 day)

New file: `docs/database-backends.md`

- Per-backend configuration (connection strings, environment variables)
- Supported versions matrix
- Feature comparison table (RLS, SKIP LOCKED, JSON querying, etc.)
- Migration guide (PostgreSQL → MySQL, PostgreSQL → SQL Server)
- Known limitations per backend
- Performance characteristics and tuning guidance

### G.2 Update Existing Docs (0.5 day)

- `README.md`: "Works with PostgreSQL, MySQL, or SQL Server" (not "PostgreSQL-native")
- `CONTRIBUTING.md`: add MySQL and SQL Server prerequisites
- `docs/release-process.md`: add multi-database testing to release checklist

### G.3 Schema Documentation (0.5 day)

Update `docs/architecture/postgresql-schema.md` to include MySQL and SQL Server
schema translations. Add a cross-reference table of data type mappings across
the three databases.

**Stream G total: ~2 days**

---

## Effort Summary

| Stream | Description | Can Start | Duration |
|--------|-------------|-----------|----------|
| A | Shared test suite (golden contract) | Immediately | 4 days |
| B | MySQL/MariaDB backend | After A.1 (backend registration) | 9 days |
| C | SQL Server backend | After A.1 (backend registration) | 8.5 days |
| D | Migration infrastructure | After B+C schemas stabilize | 2.5 days |
| E | Tenant isolation per DB | After B+C core methods pass | 3 days |
| F | CI/CD and operations | After A+B+C pass locally | 2 days |
| G | Documentation | Throughout | 2 days |

### Parallel Schedule

```
Week 1:  [A: test suite ................................] (4 days)
         [B: MySQL schema + driver setup ...............] (2 days)
         [C: MSSQL schema + driver setup ...............] (2 days)

Week 2:  [B: MySQL core methods ........................] (5 days)
         [C: MSSQL core methods ........................] (4.5 days)

Week 3:  [B: MySQL testing/bugfixing ...................] (2 days)
         [C: MSSQL testing/bugfixing ..................] (2 days)
         [E: Tenant isolation ..........................] (3 days)

Week 4:  [D: Migration infrastructure .................] (2.5 days)
         [F: CI/CD ....................................] (2 days)
         [G: Documentation ............................] (2 days)
```

**Total calendar time: ~4 weeks** for one developer working full-time.
With parallel AI-assisted development, potentially 2-3 weeks.

### Line Count Estimates

| Artifact | Estimated Lines |
|----------|----------------|
| `internal/host/store_test.go` | ~1,500 |
| `internal/host/mysql_store.go` | ~2,500 |
| `internal/host/mssql_store.go` | ~2,400 |
| `migrations/mysql/*.sql` (10 files) | ~500 |
| `migrations/mssql/*.sql` (10 files) | ~550 |
| `internal/host/migrate.go` (refactored) | +100 |
| `.github/workflows/multi-db-ci.yml` | ~80 |
| `docs/database-backends.md` | ~300 |
| Other docs updates | ~100 |
| **Total new code** | **~8,000 lines** |

### What This Plan Does NOT Cover

- **Migration DSL** — still write SQL by hand per dialect (the 10 migration
  files per backend). A DSL (e.g., atlasgo.io-style declarative schema) is a
  separate project gated on having paying users.
- **Plugin migration per dialect** — plugins remain PostgreSQL-only for their
  own tables initially. Documented limitation.
- **SQLite backend** — may be added later for embedded/edge deployments.
- **Performance tuning to parity** — each backend gets functional correctness
  first. Performance is tuned post-launch based on user workloads.
- **Cross-database replication or migration tooling** — users who want to
  switch databases use their own tools (`pg_dump` → MySQL, etc.).

---

## Risk Register

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| MySQL SKIP LOCKED semantics differ subtly from PostgreSQL | Medium | High | Test concurrent claim extensively in Stream A; add goroutine-based claim isolation test |
| MySQL has no RETURNING — two-statement claim is racy | Low | High | Both statements in one transaction with FOR UPDATE; verified by exactly-once tests |
| SQL Server READPAST behaves differently under snapshot isolation | Low | Medium | Test with both READ COMMITTED and SNAPSHOT isolation levels |
| Driver bugs or missing features in go-mssqldb | Medium | Medium | Pin to known-good version; fall back to ODBC driver if needed |
| JSON query semantics differ across databases | Medium | Medium | Test JSON path queries exhaustively in Stream A; document differences |
| go-sql-driver/mysql connection pool exhaustion under load | Low | High | Conservative pool defaults; document tuning parameters |
| Plugin ecosystem breaks with non-PostgreSQL backends | High | Low | Plugins use PluginDB interface which is already abstract; their own SQL may be PostgreSQL-specific — documented limitation |
| Migration numbering gets out of sync across dialects | Medium | Medium | Same migration numbers across all three; CI enforces that all three directories have the same set of numbered files |
