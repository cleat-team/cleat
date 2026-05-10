# WorkflowStore Abstraction Hardening & MySQL Prototype

May 2026 — project plan to de-leak the `WorkflowStore` interface and validate it
with a prototype MySQL implementation. The goal is NOT to ship MySQL support.
The goal is to make the abstraction real by building a second backend and
discovering what breaks.

---

## 1. Motivation

The `WorkflowStore` interface (60+ methods in `internal/host/db.go:117-375`)
nominally abstracts the database layer. But it's leaky in practice:

- `ShardedStore` wraps concrete `*PostgresStore`, not the interface
- `*sql.DB` is threaded directly through callers (worker, CLI, auth, compaction,
  plugin system, versioned loader)
- `TraceWorkflow` returns `sql.Result` — a `database/sql` concrete type
- `search_path` manipulation, `pq.QuoteIdentifier`, and `CREATE SCHEMA IF NOT
  EXISTS` happen at call sites, not inside PostgresStore
- `lib/pq` is imported directly in `main.go`, `sharded_store.go`, `cleatctl`,
  `cleat-bench`
- The plugin `DB` interface at `internal/plugin/plugin.go:49-62` mirrors
  `*sql.DB`'s surface exactly, coupling plugins to `database/sql`

These leaks mean you can't actually drop in a new database backend — even
though the interface exists on paper. The callers assume PostgreSQL.

The project: fix the abstraction so it's genuine, then prove it by building a
prototype MySQL implementation that passes the same test suite. The prototype
validates the interface. MySQL support itself is a throwaway — it may never
ship.

---

## 2. Current Leak Inventory

### Leak 1: ShardedStore couples to concrete PostgresStore

```
internal/host/sharded_store.go:28:  Store  *PostgresStore   // should be WorkflowStore
internal/host/sharded_store.go:61:  sql.Open("postgres", dsn)  // hardcoded driver
internal/host/sharded_store.go:71:  pq.QuoteIdentifier(cfg.Schema)  // PostgreSQL-specific
```

78 lines in `sharded_store.go` — moderate.

### Leak 2: *sql.DB threaded through callers

| File | What | Why it leaks |
|------|------|-------------|
| `cmd/cleat-worker/main.go:161,1885` | Worker struct holds `*sql.DB` | Direct DB access for idempotency cleanup, schema creation |
| `cmd/cleat-worker/main.go:210` | `db.ExecContext("CREATE SCHEMA ...")` | PostgreSQL-specific DDL at call site |
| `cmd/cleatctl/deploy.go:15,48,132` | Functions take `*sql.DB` | Plugin deploys bypass the store interface |
| `cmd/cleatctl/main.go:66` | `host.NewPostgresStore(db)` | Caller constructs concrete type |
| `internal/host/versioned_loader.go:58` | `WorkflowLoader` holds `*sql.DB` | WASM cache loading bypasses the store |
| `internal/auth/middleware.go:32` | `TenantFromAPIKey` takes `*sql.DB` | Auth system coupled to database/sql |
| `internal/host/compaction.go:447` | `loadAllEventsForCompaction` takes `*sql.DB` | Compaction helpers bypass store |
| `internal/plugin/plugin.go:49-62` | Plugin DB interface mirrors `*sql.DB` | Plugins get raw DB handles |
| `cmd/cleat-bench/main.go:56,83` | Constructs `*PostgresStore` directly | Benchmarks tied to PostgreSQL |

~15 call sites across 8 files — moderate volume, mechanical fix.

### Leak 3: Interface returns sql.Result

```go
// db.go:262
TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error)
```

`sql.Result` is a `database/sql` type. Callers that use this method now import
`database/sql`. Fix: return `(int64, error)` (rows affected) instead — `sql.Result`
only exposes `RowsAffected()` and `LastInsertId()`.

### Leak 4: PostgreSQL-specific SQL at call sites

- `main.go:210`: `CREATE SCHEMA IF NOT EXISTS ` + `pq.QuoteIdentifier(...)`
- `main.go:2630-2643`: `dsnWithSchema()` parses PostgreSQL URL format
- `sharded_store.go:54-60`: Injects `search_path` into PostgreSQL DSN
- `sharded_store.go:71-72`: `CREATE SCHEMA IF NOT EXISTS` with `pq.QuoteIdentifier`

These are store initialization concerns that should live inside the store
implementation, not at the call site.

### Leak 5: lib/pq imported broadly

```
cmd/cleat-worker/main.go:43:  "github.com/lib/pq"
cmd/cleat/main.go:23:        "github.com/lib/pq"
cmd/cleatctl/main.go:29:     "github.com/lib/pq"
cmd/cleat-bench/main.go:25:  "github.com/lib/pq"
internal/host/sharded_store.go:10: "github.com/lib/pq"
```

After cleanup, `lib/pq` should only be imported by `PostgresStore` and its
tests.

---

## 3. The Target Abstraction

### 3.1 StoreFactory — Database-Neutral Initialization

```go
// internal/host/store.go (new file)

// StoreFactory creates WorkflowStore instances. Each database backend
// implements one. The factory encapsulates connection management, schema
// migration, and tenant isolation — callers never touch *sql.DB directly.
type StoreFactory interface {
    // OpenStore creates or connects to a named store (shard, tenant, etc.).
    // The config map carries backend-specific parameters (schema name, DSN
    // overrides, etc.). Callers use only the returned WorkflowStore and a
    // Close function.
    OpenStore(ctx context.Context, config map[string]string) (WorkflowStore, io.Closer, error)

    // Migrate runs pending schema migrations for this backend. Called once
    // at startup before any stores are opened.
    Migrate(ctx context.Context, db *sql.DB) error

    // DriverName returns the database/sql driver name for health checks.
    DriverName() string
}
```

The `*sql.DB` leak is contained: `Migrate` takes one for the migration runner,
but regular operation goes through `WorkflowStore` only.

### 3.2 Cleaned-Up WorkflowStore Interface

Fix the one interface-level leak:

```go
// Before:
TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error)

// After:
TraceWorkflow(ctx context.Context, workflowID, traceID string) error
```

No `database/sql` types in the interface. Callers don't need `RowsAffected()`
from this call — it's fire-and-forget tracing.

### 3.3 ShardedStore Uses the Interface

```go
// Before:
type Shard struct {
    Config ShardConfig
    DB     *sql.DB
    Store  *PostgresStore
}

// After:
type Shard struct {
    Config ShardConfig
    Store  WorkflowStore
    Close  func() error
}
```

`ShardedStore` no longer imports `database/sql` or `lib/pq`. It delegates
everything to `WorkflowStore` implementations.

### 3.4 Plugin DB Interface Abstracts database/sql

Replace the `*sql.DB`-mirroring interface with a purpose-built one:

```go
// internal/plugin/plugin.go

// PluginDB is the database handle available to plugins. It intentionally
// does NOT expose *sql.DB — plugins get a scoped interface appropriate to
// their declared DatabaseAccess level.
type PluginDB interface {
    Exec(ctx context.Context, query string, args ...interface{}) error
    QueryRow(ctx context.Context, query string, args ...interface{}) RowScanner
    Begin(ctx context.Context) (PluginTx, error)
}

type PluginTx interface {
    Exec(ctx context.Context, query string, args ...interface{}) error
    QueryRow(ctx context.Context, query string, args ...interface{}) RowScanner
    Commit() error
    Rollback() error
}
```

This is a subset of `database/sql` that can be backed by any driver. Plugin SQL
is still raw (plugins need DDL for their own tables), but the handle is
abstract.

### 3.5 Callers Only See the Interface

After cleanup, the call graph looks like:

```
main.go → StoreFactory.OpenStore() → WorkflowStore
                                   → io.Closer

Worker  → WorkflowStore (never sees *sql.DB)
Auth    → WorkflowStore (tenant lookup moves to store method)
CLI     → WorkflowStore (deploy/list/rollback go through interface)
Plugins → PluginDB     (scoped, not *sql.DB)
```

The only place `*sql.DB` appears is:
- `main.go` startup: created, passed to `StoreFactory.Migrate()`, then never touched again
- Migration runner: `internal/migration/runner.go` runs SQL files at startup
- Test helpers: `testutil.SetupSchema()` creates test databases

---

## 4. The Proof: Prototype MySQLStore

### 4.1 Scope — What Gets Built

A `MySQLStore` struct in `internal/host/mysql_store.go` that implements
`WorkflowStore` and passes the same integration test suite as `PostgresStore`.
Target MySQL 8.0+ (which has `SKIP LOCKED` — same claim semantics).

**Minimum viable prototype:**
- ClaimWorkflow / ClaimWorkflows / ClaimStickyWorkflows (SKIP LOCKED works)
- AppendEventHistory / AppendEventHistoryBatch (upsert via `ON DUPLICATE KEY` or `INSERT IGNORE`)
- StartNewRun / StartChildWorkflow (with idempotency)
- CompleteWorkflow / FailWorkflow / ReleaseWorkflow / FinalizeWorkflowSegment
- LoadEventHistory / LoadWASM / ListVersions
- Heartbeat / BatchHeartbeat
- ContinueAsNew (single transaction — MySQL supports this)
- ReapStaleInstances
- ListWorkflows (with search/filter)
- Promise and Update methods
- Concurrency keys
- Schedules
- Compaction

**Explicitly out of scope:**
- Tenant isolation (stub it — MySQL doesn't have RLS; this is a known
  design difference to document)
- DAG composition methods (delegate to caller)
- Memory stats (stub them)
- Migration runner for MySQL (write SQL by hand for the prototype, don't
  build the migration DSL)
- Performance tuning (the prototype proves correctness, not speed)
- Production hardening (connection pooling, retry logic, error code mapping)
- Plugin database access (stub the plugin DB interface)

### 4.2 Key Translation Points

| PostgreSQL | MySQL 8.0+ |
|------------|------------|
| `FOR UPDATE SKIP LOCKED` | `FOR UPDATE SKIP LOCKED` (same) |
| `RETURNING id, ...` | Follow-up `SELECT` in same transaction, or `LAST_INSERT_ID()` for auto-increment |
| `ON CONFLICT (...) DO NOTHING` | `INSERT IGNORE ...` or `ON DUPLICATE KEY UPDATE` |
| `ON CONFLICT (...) DO UPDATE SET` | `ON DUPLICATE KEY UPDATE` |
| `JSONB` | `JSON` |
| `gen_random_uuid()` | UUID generated in Go |
| `::type` casts | `CAST(... AS type)` or Go-side conversion |
| `ILIKE` | `LIKE` with `LOWER()` or `COLLATE utf8mb4_general_ci` |
| `now()` | `NOW()` (same) — but prefer parameterized timestamps |
| `EXTRACT(EPOCH FROM ...)` | `UNIX_TIMESTAMP(...)` |
| `search_path` schema routing | Separate databases (`dbname`) per tenant |
| RLS + `set_config()` | Application-level `WHERE tenant_id = ?` filtering |
| `digest('sha256')` | Go-side SHA-256 |
| `percentile_cont()` | Go-side computation |
| `pq.Array()` / `ANY($1)` | `IN (?, ?, ?)` with dynamic placeholders |

### 4.3 Test Strategy

The existing integration test at `internal/host/integration_test.go` (589 lines,
4 tests) is guarded by `CLEAT_TEST_DB` env var. Extend this:

```go
// internal/host/store_test.go (new file)

// TestWorkflowStore runs the full store test suite against any implementation.
// Backends register themselves via init() and are controlled by env vars:
//   CLEAT_TEST_DB=postgres://...   → PostgresStore
//   CLEAT_TEST_DB=mysql://...      → MySQLStore
func TestWorkflowStore(t *testing.T) {
    backend := detectBackend(t)
    store, cleanup := backend.Setup(t)
    defer cleanup()

    t.Run("ClaimAndExecute", func(t *testing.T) { testClaimAndExecute(t, store) })
    t.Run("ExactlyOnceStart", func(t *testing.T) { testExactlyOnceStart(t, store) })
    t.Run("ContinueAsNewAtomic", func(t *testing.T) { testContinueAsNewAtomic(t, store) })
    t.Run("EventHistoryRoundTrip", func(t *testing.T) { testEventHistoryRoundTrip(t, store) })
    t.Run("BinaryDataRoundTrip", func(t *testing.T) { testBinaryDataRoundTrip(t, store) })
    t.Run("ConcurrencyKeys", func(t *testing.T) { testConcurrencyKeys(t, store) })
    t.Run("Promises", func(t *testing.T) { testPromises(t, store) })
    t.Run("UpdateRequests", func(t *testing.T) { testUpdateRequests(t, store) })
    t.Run("ReapStaleInstances", func(t *testing.T) { testReapStaleInstances(t, store) })
    t.Run("ListAndFilter", func(t *testing.T) { testListAndFilter(t, store) })
    t.Run("Schedules", func(t *testing.T) { testSchedules(t, store) })
    t.Run("Compaction", func(t *testing.T) { testCompaction(t, store) })
}
```

The same test suite runs against both backends. If MySQLStore passes,
the abstraction is correct. This is a golden test suite — it becomes the
contract that any future database backend must satisfy.

### 4.4 MySQLStore Implementation Plan

**Phase 1: Schema translation (1 day)**
- Translate the 11 PostgreSQL migration files to MySQL SQL
- Create a `mysql_migrations/` directory
- Key differences: `JSONB` → `JSON`, `TIMESTAMPTZ` → `TIMESTAMP(6)`, `TEXT[]` →
  separate table or JSON array, `BYTEA` → `LONGBLOB`, partial indexes → full
  indexes with WHERE (MySQL 8.0.13+), `BIGSERIAL` → `BIGINT AUTO_INCREMENT`,
  `DOUBLE PRECISION` → `DOUBLE`, `pgcrypto` → Go-side

**Phase 2: Core claim/execute loop (1 day)**
- ClaimWorkflow: `SELECT ... FOR UPDATE SKIP LOCKED LIMIT 1` (MySQL supports this)
- ClaimWorkflows: same with LIMIT N
- The claim-and-return-data pattern: since MySQL lacks RETURNING, use a
  transaction: `SELECT ... FOR UPDATE SKIP LOCKED` → read data → `UPDATE ... SET
  assigned_to = ?, status = 'running'` → return data. This is two queries in one
  transaction instead of one, but correctness is identical.
- ClaimStickyWorkflows: same pattern with sticky worker filter

**Phase 3: Event history (1 day)**
- AppendEventHistoryBatch: use `INSERT IGNORE` instead of `ON CONFLICT DO NOTHING`
  for idempotency
- LoadEventHistory: straightforward SELECT with ORDER BY step
- Base64 encoding/decoding for binary data (same as PostgreSQL path)

**Phase 4: Workflow lifecycle (1 day)**
- StartNewRun: INSERT with Go-generated UUID, handle idempotency key
- CompleteWorkflow / FailWorkflow / ReleaseWorkflow: UPDATE with version check
  (optimistic locking via `WHERE status = 'running' AND assigned_to = ?`)
- FinalizeWorkflowSegment: multi-statement transaction (append events + update
  status)
- ContinueAsNew: START TRANSACTION → INSERT new run → UPDATE old run → COMMIT
- Heartbeat / BatchHeartbeat: straightforward UPDATE

**Phase 5: Signals, promises, updates (1 day)**
- PollAndClaimSignal: SELECT + DELETE in transaction
- Promise and Update request CRUD: straightforward
- No RETURNING workarounds needed (these don't use it)

**Phase 6: Schedules and background tasks (0.5 day)**
- GetDueSchedules: straightforward SELECT
- ReapStaleInstances: UPDATE with subquery
- Compaction: two-statement transaction (DELETE old events + UPSERT compaction state)

**Phase 7: List/search and memory stats (0.5 day)**
- ListWorkflows: dynamic query building with `LIKE` instead of `ILIKE`
- Memory stats: stub or compute percentiles in Go

**Phase 8: Integration testing and abstraction fixes (2 days)**
- Run the shared test suite against MySQLStore
- Fix whatever breaks in the abstraction (methods that need splitting, types
  that don't map, error semantics that differ)
- Document every abstraction change

**Total: ~8 days for the prototype.** This is a focused spike, not production
work.

---

## 5. Phase 1: De-Leak the Abstraction (Before MySQLStore)

This work happens first and is kept independent of MySQL. The goal is to
refactor the existing code so a second backend CAN be plugged in. PostgresStore
behavior does not change.

### Step 1: Fix the sql.Result leak (15 minutes)

```go
// db.go:262
- TraceWorkflow(ctx context.Context, workflowID, traceID string) (sql.Result, error)
+ TraceWorkflow(ctx context.Context, workflowID, traceID string) error
```

Change `PostgresStore.TraceWorkflow` to return `err` only (discard
`sql.Result`). It's only called in one place (`engine.go`) and the result is
never checked.

### Step 2: Make ShardedStore interface-based (2 hours)

- Change `Shard.Store` from `*PostgresStore` to `WorkflowStore`
- Change `Shard.DB` to `Close func() error` (the store owns its connection)
- `NewShardedStore` takes a `StoreFactory` instead of opening connections itself
- Remove `lib/pq` import from `sharded_store.go`
- Move `search_path` injection and `CREATE SCHEMA IF NOT EXISTS` into
  PostgresStore's factory function

Files: `internal/host/sharded_store.go`, `internal/host/db.go` (add factory
function to PostgresStore)

### Step 3: Add StoreFactory interface (1 hour)

```go
// internal/host/store.go (new file)
type StoreFactory interface { ... }
```

Add `PostgresStoreFactory` implementing it. Update `main.go` to use the factory.
The factory encapsulates DSN parsing, connection pool setup, schema creation,
and search_path injection — all PostgreSQL concerns that currently leak into
callers.

### Step 4: Route callers through the interface (3 hours)

| Call site | Change |
|-----------|--------|
| `cmd/cleat-worker/main.go:161,1885` | Worker struct drops `*sql.DB`, holds `WorkflowStore` + `StoreFactory` |
| `cmd/cleat-worker/main.go:210` | `CREATE SCHEMA` moves into `PostgresStoreFactory.Migrate()` |
| `cmd/cleat-worker/main.go:2594` | Idempotency cleanup: add `CleanupIdempotencyKeys()` to `WorkflowStore` |
| `cmd/cleatctl/deploy.go` | Plugin deploy: add `DeployPlugin()` to `WorkflowStore` or accept the leak as temporary (plugins are in-process and will always need raw SQL for their own tables) |
| `internal/auth/middleware.go` | Add `ResolveTenantFromAPIKey()` to `WorkflowStore` |
| `internal/host/versioned_loader.go` | `WorkflowLoader` takes `WorkflowStore` instead of `*sql.DB` — LoadWASM is already on the interface |
| `internal/host/compaction.go:447` | `loadAllEventsForCompaction` uses `WorkflowStore.LoadEventHistory` (already on the interface) |
| `cmd/cleat-bench/main.go` | Benchmarks use `StoreFactory` |

### Step 5: Abstract the Plugin DB interface (2 hours)

Define `PluginDB` / `PluginTx` interfaces in `internal/plugin/plugin.go`.
Create adapters that wrap `*sql.DB` and `*sql.Tx` (for PostgreSQL) and
equivalent types for future backends. This is mechanical.

### Step 6: Verify no regressions (1 hour)

Run the full test suite. All existing tests must pass — PostgresStore behavior
is unchanged.

**Phase 1 total: ~1.5 days.** At the end, the codebase compiles without `lib/pq`
imported anywhere except `PostgresStore` and its tests. `*sql.DB` only appears
in factory implementations and the migration runner.

---

## 6. Phase 2: Prototype MySQLStore (~8 days)

Build `MySQLStore` as described in Section 4. The test suite from Section 4.3
is the deliverable — it validates that a second backend can implement
`WorkflowStore` without special-casing.

### What we learn from the prototype:

1. **Interface completeness**: Are there operations that PostgresStore does in
   one query that require two in MySQL? Does the interface handle multi-
   statement operations correctly?

2. **Error semantics**: PostgreSQL and MySQL have different error codes, different
   deadlock behavior, different constraint violation messages. Does the
   interface abstract these adequately, or do callers need to understand
   database-specific errors?

3. **Transaction boundaries**: Does `ContinueAsNew`'s atomicity requirement
   expose database-specific transaction behavior?

4. **JSON handling**: PostgreSQL's JSONB vs MySQL's JSON — do the query patterns
   in ListWorkflows (filtering by input content, search) translate cleanly?

5. **Tenant isolation gap**: This is the big one. The prototype will reveal
   whether tenant isolation can be adequately enforced at the application level
   (MySQL) compared to the database level (PostgreSQL RLS). This is the hardest
   part of multi-DB and the prototype will tell us whether it's tractable.

---

## 7. What We Do NOT Do

- **Do NOT ship MySQL support.** The prototype is throwaway code. If it reveals
  that the abstraction is solid, the MySQLStore code serves as a reference for
  a future production implementation — but it is not that implementation.
- **Do NOT write a second migration tree.** The prototype uses hand-written
  MySQL DDL. A production implementation would need a migration DSL or parallel
  migration files — that's a separate project.
- **Do NOT build MySQL tenant isolation.** Stub it. The prototype acknowledges
  this gap and documents what a real implementation would need.
- **Do NOT tune for performance.** The prototype proves the interface works.
  Production MySQL tuning is a different problem.
- **Do NOT touch the transformer, WASM runtime, or SDKs.** This is purely a
  store-layer project.

---

## 8. Deliverables

| Deliverable | Description |
|-------------|-------------|
| `internal/host/store.go` | `StoreFactory` interface + `PostgresStoreFactory` implementation |
| `internal/host/sharded_store.go` (updated) | `ShardedStore` uses `WorkflowStore` interface, not `*PostgresStore` |
| `internal/host/db.go` (updated) | `TraceWorkflow` returns `error` not `sql.Result`; `PostgresStore` gains factory method |
| `internal/host/store_test.go` | Shared test suite runnable against any `WorkflowStore` backend |
| `internal/host/mysql_store.go` | Prototype `MySQLStore` — passes shared test suite, never ships |
| `internal/plugin/plugin.go` (updated) | `PluginDB` / `PluginTx` interfaces replacing `*sql.DB` mirror |
| `cmd/cleat-worker/main.go` (updated) | Worker uses `StoreFactory` + `WorkflowStore`, drops direct `*sql.DB` |
| `internal/auth/middleware.go` (updated) | Auth uses `WorkflowStore.ResolveTenantFromAPIKey()` |
| `docs/multi-database-feasibility.md` (updated) | Updated with prototype findings and refined interface design |

---

## 9. Effort and Timeline

| Phase | Work | Duration |
|-------|------|----------|
| Phase 1 | De-leak the abstraction | 1.5 days |
| Phase 2 | Prototype MySQLStore | 8 days |
| **Total** | | **~2 weeks** |

A 2-week investment to validate the architectural boundary. If it works, the
project has a proven abstraction and can confidently say "multi-DB is
architecturally supported" without having to ship it. If it doesn't work, we
learn what's wrong with the abstraction and fix it before it calcifies.

---

## 10. Decision Framework

**Do this project if:**
- You want the architectural credibility of having proven the abstraction works
- You suspect there are hidden coupling points the hostile review didn't find
- You want to be able to say "we prototyped MySQL support and the interface is
  clean" in design partner conversations
- The 2-week investment is acceptable relative to adoption work

**Skip this project if:**
- The next 2 weeks are better spent on the OSS launch, demo video, and blog
  posts (the adoption work that actually gets users)
- You're confident the `WorkflowStore` interface is good enough and would rather
  fix it reactively when the first non-PostgreSQL need arises

**Recommended:** Do Phase 1 (de-leak, 1.5 days) immediately — it's pure
code quality improvement with no downside. Defer Phase 2 (MySQL prototype) until
after the OSS launch, unless a design partner specifically raises multi-DB as a
blocker.
