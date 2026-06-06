# ARCHITECTURE — cleat Engine

## Invariants

- Workflow execution is deterministic — replay of the same event history
  must produce identical results regardless of language (Go, Python, JS,
  TinyGo/WASM)
- Tenant isolation is mandatory (RLS on all queries, tenant_id in every
  multi-tenant table, fail-closed)
- Checksum verification runs by default on all workflow state transitions
- Engine API is read-only during debug sessions (engine.db set to nil)
- No `log.Printf` in hot paths — use structured `slog` with
  workflow_id/tenant_id context

## Design Decisions

- **wasmtime for WASM execution** — single runtime, multi-language support
  via WASM compilation (Go→WASM via TinyGo, Python/JS via their WASM
  toolchains)
- **Event sourcing for workflow state** — event_history is the system of
  record; workflow state is reconstructed via replay
- **PostgreSQL-first, MSSQL/MySQL compatibility** — same schema semantics
  across all three backends, tested via CI matrix
- **RLS fail-closed** — COALESCE fallback to default tenant replaced with
  `assert_tenant_set()` error function (cleat-224)
- **Single-step replay for debugging** — advanceReplayStep centralizes
  stepCount++; ReplayStepCallback lets callers pause and inspect

## Module Boundaries

| Package | Owns | Depends On |
|---------|------|------------|
| `internal/host/` | Engine, workflow execution loop, replay, signals | wasm, migration, telemetry |
| `internal/wasm/` | WASM module loading, wasmtime bindings | — |
| `internal/wasmrw/` | WASM read/write helpers | wasm |
| `internal/migration/` | Schema DDL, migrations (PG/MSSQL/MySQL) | — |
| `internal/auth/` | Tenant auth, RLS policy enforcement | migration |
| `internal/telemetry/` | OTel setup, span hierarchy | — |
| `internal/plugin/` | Plugin interface, registry | — |
| `internal/plugingen/` | Plugin code generation | plugin |
| `internal/analyzer/` | Static analysis of workflow code | — |
| `internal/callgraph/` | Call graph construction | analyzer |
| `internal/closure/` | Transitive closure over dependencies | callgraph |
| `internal/transform/` | AST transformations | analyzer |
| `cmd/cleat-worker/` | Worker binary entry point | host |
| `cmd/cleatctl/` | CLI admin/debug tool | host (read-only) |
| `cmd/clew-service/` | Standalone clew HTTP service | host |

## Coupling Matrix

- `cmd/cleat-worker` → `internal/host`: MEDIUM (consumes Engine API)
- `cmd/cleatctl` → `internal/host`: MEDIUM (read-only Engine API for debug)
- `internal/host` → `internal/wasm`: TIGHT (shared wasmtime types, execution)
- `internal/host` → `internal/migration`: MEDIUM (schema contracts)
- `internal/host` → `internal/telemetry`: LOOSE (OTel is initialized
  separately)
- `internal/wasmrw` → `internal/wasm`: TIGHT (shared WASM primitives)
- `internal/plugingen` → `internal/plugin`: TIGHT (generates plugin code)
- `internal/callgraph` → `internal/analyzer`: TIGHT (shared AST types)
- `internal/closure` → `internal/callgraph`: TIGHT (operates on call graphs)
- `internal/transform` → `internal/analyzer`: TIGHT (shared AST types)

## Data Model

- `workflow_runs` — workflow instances (id, tenant_id, state, checksum)
- `event_history` — event log (FK→workflow_runs, ON DELETE CASCADE from
  cleat-222)
- `concurrency_keys` — idempotency / concurrency control (tenant_id scoped)
- `signals` — cross-workflow signal delivery
- `promises` — async promise resolution
- `schedules` — scheduled workflow triggers
- All multi-tenant tables include `tenant_id` with RLS policy

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

- TinyGo compilation is deprecated (#36) — standard Go compilation is the
  default. TinyGo toolchain has WASM compatibility issues.
- MSSQL `uniqueidentifier` type differs from PG `UUID` — scan carefully.
- MySQL test schema historically lagged production schema (cleat-215 fixed
  this, but the pattern could recur).
- Engine.go is large (~3000+ lines) — surgery requires careful review.
- Replay determinism depends on all backends handling SideEffect results
  identically (cleat-207 verified cross-language).
