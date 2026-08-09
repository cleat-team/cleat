# ARCHITECTURE — cleat Engine

## Invariants

- Workflow execution is deterministic — replay of the same event history
  must produce identical results regardless of language (Go, Python, JS,
  WASM)
- Tenant isolation is mandatory (RLS on all queries, tenant_id in every
  multi-tenant table, fail-closed)
- Checksum verification runs by default on all workflow state transitions
- Engine API is read-only during debug sessions (engine.db set to nil)
- No `log.Printf` in hot paths — use structured `slog` with
  workflow_id/tenant_id context

## Design Decisions

- **wasmtime for WASM execution** — single runtime, multi-language support
  via WASM compilation (Go→WASM via the standard Go toolchain targeting
  wasip1, Python/JS via their WASM toolchains)
- **Event sourcing for workflow state** — event_history is the system of
  record; workflow state is reconstructed via replay
- **PostgreSQL-first, MSSQL/MySQL compatibility** — same schema semantics
  across all three backends, tested via CI matrix
- **RLS fail-closed** — COALESCE fallback to default tenant replaced with
  `assert_tenant_set()` error function (cleat-224)
- **Single-step replay for debugging** — advanceReplayStep centralizes
  stepCount++; ReplayStepCallback lets callers pause and inspect

## Module Boundaries

> Corrected 2026-08-09. This table previously named `internal/host/`,
> `internal/wasm/`, `internal/wasmrw/`, and `internal/plugin/`, and listed a
> `cmd/clew-service/`. Commit `3eeb74e` (2026-06-01), "promote internal
> packages to public — engine as a library", moved the first four to
> `engine/`, `wasm/`, `wasmrw/`, and `plugin/` respectively (see CLAUDE.md's
> note on paths in older commits). `cmd/clew-service/` never existed in this
> repo — verified with `find . -iname '*clew*'`, which turns up only
> `testdata/clew-lifecycle` and the `make clew` Makefile target (a
> `cleat-worker` invocation against Neon, not a separate service binary).
> Layout re-verified against the actual tree with `ls -d */` and `ls cmd/`.

| Package | Owns | Depends On |
|---------|------|------------|
| `engine/` | Engine, workflow execution loop, WASM backends (wasmtime + wazero), replay, signals | wasm, migration, telemetry |
| `wasm/` | WASM module loading, codegen | — |
| `wasmrw/` | WASM read/write helpers | wasm |
| `migration/` | Schema DDL, migrations (PG/MSSQL/MySQL) | — |
| `auth/` | Tenant auth, RLS policy enforcement | migration |
| `internal/telemetry/` | OTel setup, span hierarchy | — |
| `plugin/` | Plugin interface, registry | — |
| `internal/plugingen/` | Plugin code generation | plugin |
| `internal/analyzer/` | Static analysis of workflow code | — |
| `internal/callgraph/` | Call graph construction | analyzer |
| `internal/closure/` | Transitive closure over dependencies | callgraph |
| `internal/transform/` | AST transformations | analyzer |
| `cmd/cleat-worker/` | Worker binary entry point | engine |
| `cmd/cleatctl/` | CLI admin/debug tool | engine (read-only) |

## Coupling Matrix

- `cmd/cleat-worker` → `engine`: MEDIUM (consumes Engine API)
- `cmd/cleatctl` → `engine`: MEDIUM (read-only Engine API for debug)
- `engine` → `wasm`: TIGHT (shared wasmtime/wazero types, execution)
- `engine` → `migration`: MEDIUM (schema contracts)
- `engine` → `internal/telemetry`: LOOSE (OTel is initialized separately)
- `wasmrw` → `wasm`: TIGHT (shared WASM primitives)
- `internal/plugingen` → `plugin`: TIGHT (generates plugin code)
- `internal/callgraph` → `internal/analyzer`: TIGHT (shared AST types)
- `internal/closure` → `internal/callgraph`: TIGHT (operates on call graphs)
- `internal/transform` → `internal/analyzer`: TIGHT (shared AST types)

## Data Model

> Corrected 2026-08-09: table names below did not match
> `migrations/postgres/001_schema.sql` (`workflow_runs` should be
> `workflow_instances`; `signals`, `promises`, `schedules` should be their
> `workflow_`-prefixed names — there is no bare `signals`/`promises`/
> `schedules` table). Verified with
> `grep -n '^CREATE TABLE' migrations/postgres/001_schema.sql`.

- `workflow_defs` — versioned WASM workflow definitions
- `workflow_instances` — workflow instances (id, tenant_id, state, checksum)
- `event_history` — event log (FK→workflow_instances)
- `concurrency_keys` — idempotency / concurrency control (tenant_id scoped)
- `workflow_signals` — cross-workflow signal delivery
- `workflow_promises` — async promise resolution
- `workflow_schedules` — scheduled workflow triggers (cron)
- `workflow_tags`, `workflow_routing` — tagging and task-queue routing
- All multi-tenant tables carry `tenant_id`; on PostgreSQL and SQL Server it
  is enforced by database-side RLS (FORCEd policy / FILTER PREDICATE), not
  just present as a column — see `docs/explanation/security-model.md`.

## Patterns

- **Structured logging**: Use `slog` with `workflow_id` and `tenant_id`
  context. Never `log.Printf` in hot paths.
- **Error enrichment**: Divergence errors include payload snapshots (4KB
  truncation + SHA-256). All errors include operation context.
- **Race safety**: Shared mutable state protected by `sync.Mutex` or
  `sync.RWMutex`. Goroutine lifecycles tied to context cancellation.
- **DB error handling**: Use `errors.Is(err, sql.ErrNoRows)`, never `==`.
- **Migrations**: Schema changes go in all three backends
  (postgres/mysql/mssql). Test schemas must match production schemas.

## Known Sharp Edges

- MSSQL `uniqueidentifier` type differs from PG `UUID` — scan carefully.
- MySQL test schema historically lagged production schema (cleat-215 fixed
  this, but the pattern could recur).
- The engine package is large (~174 files per CLAUDE.md's repo structure;
  `engine/engine.go` itself is 507 lines as of 2026-08-09, `wc -l
  engine/engine.go` — logic that used to live in one file is now spread
  across many) — surgery requires careful review of the whole package, not
  just one file.
- Replay determinism depends on all backends handling SideEffect results
  identically (cleat-207 verified cross-language).
