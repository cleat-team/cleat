# Changelog

> **UPGRADE NOTES** — Breaking changes are called out at the top of each
> release section. Read them before upgrading between versions.

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### UPGRADE NOTES — breaking

- **Terminating a workflow that has registered `defer` bodies is now asynchronous,
  and runs those bodies before the workflow becomes terminal.** Migrations
  `postgres/040`, `mssql/043` (MySQL needs none).

  Previously `TerminateWorkflow` wrote `status = 'terminated'` and then released
  the workflow's sticky assignment and concurrency keys. The registered defers
  never ran — and the resources a defer body would have released were dropped
  anyway, by the host, in the wrong order, with nothing recording that anything
  was owed.

  Now such a workflow goes to a new non-terminal status, **`terminating`**,
  carrying the outcome it will be given. A worker claims it like any other
  workflow, replays its history as a *defer segment* — the body does not run
  again; it is refused any new work — runs the outstanding defer bodies, and only
  then applies the recorded outcome and releases the resources.

  **Consequences.**
  - A caller that terminates and immediately reads `status` may see `terminating`
    rather than `terminated`, and must poll. This is `tiers.yaml`'s decision D6.
  - A workflow with **no** registered defers still terminates in one step, as
    before. Most deployments will see no change at all.
  - Terminating a workflow that is already in its defer phase terminates it
    immediately, cutting the cleanup short.
  - A defer phase never changes the outcome: if it traps, times out, or cannot
    start, the recorded outcome is applied anyway and the lost cleanup is logged.
    `defer_phase_deadline` (5 minutes) bounds it.
  - **Apply the migrations.** `postgres/040` widens `admin.claim_workflows` and
    the claim's partial indexes; `mssql/043` widens the filtered ones. A
    deployment running this code against the older schema keeps working — the
    cross-tenant claim falls back with a warning naming the migration — but its
    defer phases are never claimed, so every terminate waits out its deadline and
    skips the cleanup.

  **A closing parent's `TERMINATE` children work the same way**, and the change
  matters more there because it is a bulk operation: one closing parent used to
  drop every child's concurrency keys and sticky assignment at once, before any
  of their defers had run. A child that owes cleanup now goes to `terminating`
  carrying `pending_terminal_status = 'failed'` — the close policy's own
  outcome, not `terminated` — and is failed once its defers have run. A child
  that owes none is failed immediately, as before.

  The admin force-resolve verbs are unchanged: they still terminate in one step
  and still skip their defers.

  See IMPROVEMENT-PLAN §3.75, §3.112 and §3.114, and
  `docs/reference/workflow-lifecycle.md` for the whole state machine.

- **Workflow definition names are now per-tenant.** `workflow_defs`' primary key
  becomes `(tenant_id, name, version)`, and the three foreign keys that reference
  it — from `workflow_instances`, `workflow_tags` and `workflow_routing` — carry
  `tenant_id` too. Migrations `postgres/035`, `mysql/034`, `mssql/038`.

  Two tenants can now each hold their own `order-processor`. Previously the name
  was a shared namespace: the second tenant to deploy one was refused, and before
  that (pre-0.2.0) it silently overwrote the first.

  **Consequences.** A deploy no longer returns `409` for a name another tenant
  holds — there is no conflict to report. `ErrWorkflowDefOwnedByAnotherTenant` is
  removed, along with the default-tenant adoption window that let a definition
  deployed before per-tenant ownership stay reachable by every tenant; on
  PostgreSQL that also removes `OR tenant_id = '00000000-...'` from
  `tenant_isolation_defs`, bringing it into line with SQL Server, which never had
  it. A workflow started for a tenant that has not deployed the definition it
  names is refused by the foreign key.

  **MySQL only:** `workflow_defs.tenant_id` was nullable with no default, unlike
  the other two dialects. `mysql/034` backfills `NULL`s to the default tenant and
  makes the column `NOT NULL DEFAULT`, as a primary-key column must be.

  See IMPROVEMENT-PLAN §3.77 and D7 in `tiers.yaml`.

- **Cross-schema child workflows are removed.** The `cleat_child_workflow_in_schema`
  host call, the `cleat:host-calls/durable-extended-children` component interface,
  the `--peer-schemas` worker flag and the corresponding surface in the Go, Rust,
  Java, Python and AssemblyScript SDKs are all gone. A worker started with
  `--peer-schemas` now fails on an unknown flag.

  It let a workflow start a child by writing a row directly into another
  PostgreSQL schema. That makes the other deployment's schema part of your API and
  its migrations part of your compatibility surface, and it had no settled answer
  for whose tenant the child belonged to — the definition lookup in the peer schema
  carried no tenant predicate, and where the target tenant could not be recovered
  from the schema name the insert ran with no tenant context at all.

  **Use the other pool's API instead**, the same way any two services talk. Nothing
  in `tiers.yaml` claimed this feature at any tier and no end-to-end test exercised
  it. See IMPROVEMENT-PLAN §3.78.

  ABI host-function count goes 59 → 58 on both backends. `CurrentABIVersion` is
  unchanged: nothing that remains changed shape.

- **Terminating a parent now closes its children.** `TerminateWorkflow` never
  called `enforceParentClosePolicy`, on any dialect, so terminating a parent
  left its `TERMINATE`-policy children running and its `REQUEST_CANCEL`
  children unflagged — while force-completing or force-failing the *same*
  parent closed them. Measured 2026-09-02: a child of a parent closed with
  `TerminateWorkflow` stayed `ready`; the same child under `AdminForceComplete`
  went to `failed`.

  Who this breaks: a deployment that relied on `terminate` being the narrow
  "stop this one workflow" verb. Its children now close with it —
  `TERMINATE` children are failed with `parent workflow terminated`, and
  `REQUEST_CANCEL` children have cancellation requested. `ABANDON` children are
  unaffected, as they always were.

  The design document says this is what should happen (*"`enforceParentClosePolicy`
  runs on parent terminal transition"*, *"children are cancelled with their
  parent, preventing orphan workflows"*), and the deciding argument is internal
  consistency: `adminForceResolve` is an operator verb on an unclaimed workflow
  setting a terminal status with a direct `UPDATE` — the same shape as
  `TerminateWorkflow` in every respect — and it enforced the policy. See
  IMPROVEMENT-PLAN §3.79.

### Added

- **`allowed_signals` can be set.** `GET` and `PUT
  /api/workflows/{id}/allowed-signals` read and replace the list
  `--require-signal-auth` checks a caller against, backed by
  `WorkflowStore.SetAllowedSignalCallers` on all three dialects. Until now
  nothing in cleat could write that column, so 0.2.0's note below — that the
  flag denied every signal with no supported remedy — described a gap that is
  now closed.

  `PUT` replaces the whole list; send it without a caller to revoke, or `[]` to
  clear. Both verbs are scoped to the calling tenant, and a workflow belonging
  to another tenant answers `404` rather than `403`, so the endpoint cannot be
  used to find out which ids exist.

  **`--require-signal-auth` still defaults to `false`.** Every workflow starts
  with an empty list and nothing sets one at start time, so enabling the flag
  denies every signal until callers are granted per workflow. Grant first, then
  enable. See `docs/reference/worker-config.md`.

- **An operator can resolve a call left ambiguous by a crash.** `POST
  /api/admin/instances/{id}/steps/{step}/resolve`, with `X-Confirm:
  resolve-step` and `{"response": "..."}`, records an outcome for a durable
  call that was in flight when the process died.

  Such a call leaves a pending row, and replay reports it as `[AMBIGUOUS]` and
  says to check the external service before retrying — with nowhere to put the
  answer. An `AmbiguityResolver` could supply one, but the resolve path returns
  immediately when none is configured, which is most deployments, so the
  workflow reported the same ambiguity on every replay forever.

  The response is written to the event as though the call had returned it,
  because that is what replay has to see. What keeps it honest is the new
  `resolved_by` field, written to the same row in the same statement: the
  outcome is usable by replay and permanently marked as **asserted by an
  operator rather than observed**. The row must still be pending, so an
  operator racing a worker cannot overwrite a real result — whoever writes
  first wins. IMPROVEMENT-PLAN §1.4 phase F.

- **An operator can re-replay a stopped workflow.** `POST
  /api/admin/instances/{id}/re-replay`, with `X-Confirm: re-replay` and
  `{"generation": N}`, returns a `failed`, `terminated` or `dead_lettered`
  workflow to `ready`: the claim, heartbeat and error fields are cleared and
  the generation is bumped, so a stale worker's late write cannot land. History
  is **kept** — the workflow resumes from what it recorded rather than starting
  over. The non-terminal statuses are refused because the dispatcher owns them.

  Re-replay refuses a workflow whose history holds an unresolved ambiguous
  call, and names the step: replaying one stops again in the same place for the
  same reason, so it points at the resolve endpoint above instead. This was the
  last of the three admin operations that was a stub returning
  `not implemented` on all three dialects. IMPROVEMENT-PLAN §3.20.

- **`GET /api/workflows/{id}/history` reports `err_code` and `resolved_by` per
  event.** `err_code` is how the caller classified a failed call — the
  `ErrorCode` a `ServiceCaller` supplied through `CleatError` — recorded at the
  time of the failure, where history previously collapsed the whole
  classification to a single retryable-or-not bit. Its vocabulary is the one
  `workflow_instances.error_code` already stores, so one operator query matches
  in both tables.

  It is for reading, not for deciding: `err_non_retryable` stays authoritative
  for retry behaviour, deliberately. The two can legitimately disagree, because
  the guest's own retry policy travels across the ABI — and deriving
  retryability from the class instead would let an upgrade change the retry
  behaviour of workflows already in flight, which is the determinism bug §2.35
  exists to prevent. Both fields survive history compaction.
  IMPROVEMENT-PLAN §2.35.

### Fixed

- **Closing a workflow left concurrency slots and sticky-worker assignments
  held until their TTL.** `releaseWorkflowResources` runs the two best-effort
  cleanups that follow every commit taking a workflow out of the runnable set.
  Two paths committed such a transition without it:

  - `MySQLStore.TerminateWorkflow` execed its `UPDATE` and returned, where
    PostgreSQL and SQL Server both released. One slot per terminated workflow,
    on a tier-1 dialect (IMPROVEMENT-PLAN §3.76).
  - `enforceParentClosePolicy`'s TERMINATE arm failed every child of a closing
    parent and released nothing, **on all three dialects** — so one closing
    parent stranded a slot per child (IMPROVEMENT-PLAN §3.80).

  Bounded rather than leaked: `concurrency_keys.expires_at` is `NOT NULL` and
  the reaper deletes expired rows, so the slots freed themselves at the key's
  TTL. They freed themselves silently, with every workflow queued on those keys
  waiting out the window for nothing.

## [0.2.0] - 2026-08-10

### UPGRADE NOTES — breaking

- **SQL Server 2022 is now the minimum.** `migrations/mssql/011` uses
  `ISJSON(payload, VALUE)`, whose second argument requires 2022, so that the
  payload columns accept the JSON scalars PostgreSQL and MySQL have always
  accepted — without it, `DeliverSignal` and `CreateUpdateRequest` failed on
  any SQL Server built from the shipped schema. `README.md` and
  `docs/reference/database-backends.md` previously said 2017+; nothing has ever
  tested below 2022.
- **`--require-signal-auth` now defaults to `false`.** It gates a check that
  reads `workflow_instances.allowed_signals`, and nothing in cleat can write
  that column — no store method, no API endpoint, no CLI verb, no SDK call. The
  check denies when the list is empty, so with the flag on by default every
  cross-workflow signal, every plugin-originated signal and every external HTTP
  signal was denied, and the documented remedy (add `"*"` to `allowed_signals`)
  could not be carried out. A deployment that wants the old behaviour can pass
  `--require-signal-auth=true`, but should know that it denies every signal.
  The default goes back to `true` when there is a way to populate the list.

- **A deploy no longer overwrites a workflow definition owned by another
  tenant; it fails instead.** `workflow_defs` is keyed by `(name, version)`
  with no tenant in the key, and all three backends upserted on that key — so
  the second tenant to deploy a given name silently replaced the first
  tenant's WASM bytes, and the first tenant's workflows then executed the
  second tenant's code. `DeployWorkflowDef` now records the deploying tenant
  and refuses to write over a definition that belongs to someone else,
  returning an error wrapping `engine.ErrWorkflowDefOwnedByAnotherTenant`.

  Who this breaks: a multi-tenant deployment in which two tenants deploy the
  same definition name. That previously "worked" in the sense that one row
  survived and served both; it now fails for whichever tenant does not own the
  name. If you were relying on one shared definition across tenants, deploy it
  as the default tenant (`00000000-0000-0000-0000-000000000000`) and do not
  redeploy it as a specific tenant — a definition owned by the default tenant
  stays readable by every tenant, which is what this table's PostgreSQL RLS
  policy has always allowed.

  What upgrades cleanly: every definition in an existing database is owned by
  the default tenant, because `PostgresStore` hardcoded that value and
  `MSSQLStore`'s `MERGE` omitted the column. Such a definition is *adopted* by
  the first tenant that redeploys it, so ordinary redeploys keep working and
  ownership takes effect from then on. Until a definition has been redeployed
  once, a tenant other than its creator can still take it over.

  This does not make two tenants able to hold the same name — that needs the
  tenant in the primary key, and with it three foreign keys per dialect. The
  name remains a global namespace; squatting one is now loud instead of
  silent. IMPROVEMENT-PLAN §3.12.

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

### Changed
- Project renamed from "durable" to "cleat" across the codebase

### Removed
- TinyGo support for compiling Go workflows to WASM. TinyGo is an
  embedded-systems toolchain and lacked the standard library coverage this
  project needs. The standard Go toolchain targeting `wasip1` (`--target go`)
  is now the only supported way to compile Go workflows to WASM.

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
- **`CreateUpdateRequest` rejected ordinary payloads.** A non-JSON update
  payload failed outright on MySQL and SQL Server, and one containing a quote
  or a backslash failed on all three — `workflow_update_requests.payload` never
  received the JSON encoding signals got in the same fix. Both the encode and
  the decode are now shared, so every dialect stores and returns what the
  caller passed in.
- **Workflows could not complete on SQL Server.** `json.Marshal` of a nil map
  returns `null`, not nil, so a workflow with no query handlers wrote the JSON
  value `null` into `query_state` — which PostgreSQL and MySQL accept and the
  shipped SQL Server schema rejects with
  `CHECK (ISJSON(query_state) = 1)`. `CompleteWorkflow`, `FailWorkflow` and
  `ContinueAsNew` all failed there. On the other two dialects the query state
  was silently stored as `null` rather than `{}`.
- **`CreateSchedule` could not create a schedule on SQL Server.**
  `json.RawMessage` binds as `VARBINARY`, so `workflow_schedules.input` was
  written as the binary rendering of the JSON, and the shipped schema's
  `CHECK (ISJSON(input) = 1)` rejected the row. Every scheduled workflow on a
  SQL Server built from `migrations/mssql/001_schema.sql` failed to be
  created. The test schema declared no such constraint, which is why the suite
  never showed it.
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

[Unreleased]: https://github.com/cleat-team/cleat/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/cleat-team/cleat/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/cleat-team/cleat/releases/tag/v0.1.0
