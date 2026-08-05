# Changelog

> **UPGRADE NOTES** — Breaking changes are called out at the top of each
> release section. Read them before upgrading between versions.

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### UPGRADE NOTES — breaking

- **Workers now refuse to start on a PostgreSQL connection that bypasses
  row-level security.** Every tenant-scoped table has RLS enabled and FORCEd,
  and for `GetWorkflowByID` and `ListWorkflows` those policies are the only
  tenant isolation there is — neither carries an application-level `tenant_id`
  filter. PostgreSQL never applies RLS to a superuser, so a superuser
  connection returned every tenant's data from those calls. Every
  configuration previously shipped connected as one.

  With `--require-auth` (default true), a worker whose `--db` role is a
  superuser, has `BYPASSRLS`, or owns the tables without `FORCE` will now log
  the reason and exit rather than serve traffic it cannot isolate.

  To upgrade:

  1. Apply `migrations/postgres/005_app_role.sql`, which creates the
     `cleat_app` role and grants it what the engine needs — no ownership, no
     DDL.
  2. Give it a password: `ALTER ROLE cleat_app LOGIN PASSWORD '...';` The
     migration deliberately does not, so no credential lives in the
     repository. (`docker-compose.cluster.yml` does this from
     `CLEAT_APP_PASSWORD` via `deploy/postgres/900-app-role.sh`, but
     `docker-entrypoint-initdb.d` only runs on a *first* initialisation, so an
     existing deployment must run it by hand.)
  3. Point `--db` at `cleat_app`, and pass the previous owner DSN as
     `--migrate-db` — migrations need DDL rights that `cleat_app` does not
     have. `--migrate-db` defaults to `--db`.

  `--rls-check=off` restores the old behaviour for a single-tenant deployment
  that does not want this. `--rls-check=require` refuses regardless of
  `--require-auth`.

- **The root `schema.sql` has been deleted.** `migrations/postgres/` is the
  only schema source. The deleted file was a second, hand-maintained copy that
  had drifted into a strict subset: no `finalize_workflow_status` (which the
  engine calls on every workflow completion, with no fallback), no RLS
  policies, and no `admin.tenants`. A database built from it could not complete
  a workflow. Apply every file in `migrations/postgres/` in lexical order.

- **`docker-compose.cluster.yml` mount layout changed.** `migrations/postgres`
  is now mounted read-only at `/opt/cleat/migrations` and applied by
  `deploy/postgres/100-apply-migrations.sh`; `deploy/postgres` is
  `/docker-entrypoint-initdb.d`. Deployments that copied the old volume block
  must update it.

- **`DURABLE_TEST_DB` is renamed to `CLEAT_TEST_DB`.** The old name still works
  and warns.

- **`engine/testutil.TestDB` now fails instead of skipping** when a DSN is
  configured for its dialect but cannot be reached. Asking for a database and
  not getting one is a broken configuration, not an absent one — this is how a
  CI job stayed green for months without ever connecting. With no DSN
  configured it still skips.

### Added
- Engine test coverage for WASM backends (wasmtime, wazero) exceeding 40%
- Integration tests for MySQL and MSSQL store backends
- Unit tests for SignalWorkflow, DurableScheduleInvoke, and RegisterUpdateHandler
- WasmDiskCache and in-memory WASM cache unit tests
- CGO dispatch layer unit tests for component_cgo.go
- Engine streaming, deferral, dispatch, and callbacks unit tests
- Mock-DB coverage for WorkflowLoader DB methods
- Multi-backend coverage for MySQL and MSSQL store methods
- Concurrency key and shard error-path tests for ShardedStore
- WASM lock, memory, and scan unit tests
- Plugin loader, migration, events, credentials, and encryption tests
- Engine/app.go comprehensive test suite (22 functions, 100% coverage)
- QueryBuilder and Dialect SQL helper unit tests
- Regression tests for critical-path PostgresStore methods

### Fixed
- Multi-database CI failures in MySQL and MSSQL integration tests
- Tenant isolation: restore tenant_id filter on GetWorkflowByID
- JSONB handling in PostgreSQL store
- All 54 MySQL integration tests now pass
- MSSQL test container configuration to avoid MCR pull block
- Error message quality improvements for CLI, worker, and engine
- Race condition fixes for concurrent workflow execution
- Wasmtime memory buffer and signal handling fixes
- Plugin test failures in manifest and eventtriggers
- Auth middleware and tenant store test repairs
- Project root detection in engine tests
- ABIVersion type comparison in mssql_store_test.go
- Expand fake SQL driver coverage for admin-prefixed queries
- Corrected CompleteWorkflow status assertion from "completed" to "done"
- MySQL test schema TEXT to VARCHAR for assigned_to column

### Changed
- Project renamed from "durable" to "cleat" across the codebase

### Fixed
- **No `cleat-worker` could start against MySQL either.** The migration runner
  split each file on every `;`, including semicolons inside comments and inside
  stored-procedure bodies, so neither shipped MySQL file could be applied:
  `001_schema.sql` failed with `Error 1064` on a semicolon in a comment, and
  `003_procedures.sql` — which creates `finalize_workflow_status`, the
  procedure the engine calls on every workflow completion with no fallback —
  was cut into fragments and its `DELIMITER` directive sent to the server. A
  worker pointed at a MySQL database whose schema had not been built by hand
  logged the error and exited. Statement splitting is now comment-, string- and
  `DELIMITER`-aware, and `multi-db-ci.yml` runs the migration tests against
  live MySQL and SQL Server.
- No `cleat-worker` could start against PostgreSQL: a session-scoped
  `SET search_path` in the migration files broke the migration runner's own
  bookkeeping, and concurrent workers raced each other's DDL at boot. Both
  migration runners now hold an advisory lock, and the core runner
  schema-qualifies its tracking table.
- The shipped schema created its objects in a schema named after the
  connecting role rather than `public`, because `POSTGRES_USER=cleat` collides
  with a schema `001_schema.sql` creates.
- `ContinueAsNew` had never worked on PostgreSQL (an INSERT listed nine columns
  and supplied eight values).
- `AssignedTo` was overwritten with an empty string on every claim.
- `tenant_id` was not written by `CreatePromise`, `DeliverSignal` or
  `CreateSchedule`, so those rows were invisible to the tenant that created
  them.
- `PollSignal` deleted the signals it read, contradicting its own contract and
  both other backends. It is on the live signaller path.
- The auto-generated startup API key was never created on any PostgreSQL
  deployment: the count query named `tenant_api_keys` unqualified while the
  table is `admin.tenant_api_keys`. With `--require-auth` defaulting to true, a
  fresh cluster had no key and no way in.
- `cleat build` chose its output `.wasm` filename at random, because entry
  points were read from a map in iteration order.
- The `kvstore` and `feature-flags` plugins did not work on MySQL or SQL
  Server: unquoted `key` (a reserved word in both), no `LIMIT` on SQL Server,
  and JSON columns that could be neither read nor written there.

### Removed
- TinyGo support for compiling Go workflows to WASM. TinyGo is an
  embedded-systems toolchain and lacked the standard library coverage this
  project needs. The standard Go toolchain targeting `wasip1` (`--target go`)
  is now the only supported way to compile Go workflows to WASM.

## [0.1.0] - 2026-05-13

### Added
- Durable execution engine with deterministic replay model
- Multi-database support: PostgreSQL, MySQL, SQL Server (MSSQL)
- WASM compilation pipeline (Go to wasip1, TinyGo support)
- 22 built-in plugins (blobstore, event-triggers, feature-flags, kafka-connect,
  llm, notifications, pagerduty-alert, slack-notify, webhook-ingest, and more)
- 5 language SDKs: Go, Rust, Python, Java, AssemblyScript
- Svelte 5 admin dashboard with embedded web UI
- CLI toolchain: build, vet, deploy, versions, schedule
- Prometheus metrics endpoint and OpenTelemetry tracing support
- wazero WASM runtime (pure Go, no CGo required)
- PostgreSQL-backed work queue, timer service, and blob store
- Deterministic replay from event history (Temporal-style model)
- Saga pattern with compensating transactions
- Durable promises for cross-workflow coordination
- Virtual Object (entity workflow) pattern for stateful actors
- Continue-as-new for workflow history compaction
- Heartbeat support for long-running operations
- Server-side retry with configurable backoff policy
- Signal patterns (fire-and-forget, request-response, polling)
- Update handler pattern (bi-directional RPC with validation)
- Query handlers for read-only workflow state inspection
- Sharded store for horizontal scalability
- Tenant isolation with schema-per-tenant support
- Workflow versioning with minimum version support

[Unreleased]: https://github.com/cleat-team/cleat/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/cleat-team/cleat/releases/tag/v0.1.0
