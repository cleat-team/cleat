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
| `engine/` | Engine, workflow execution loop, replay, signals, WASM runtime | wasm, plugin |
| `wasm/` | WASM module loading, code generation, adapter | — |
| `wasmrw/` | WASM read/write helpers | — |
| `migration/` | Schema DDL runner (PG/MSSQL/MySQL) | — |
| `auth/` | Tenant auth, RLS middleware | — |
| `plugin/` | Plugin interface, registry, manifest, host helpers | wasmrw |
| `internal/telemetry/` | OTel setup, span hierarchy | — |
| `internal/plugingen/` | Plugin code generation | plugin |
| `internal/analyzer/` | Static analysis of workflow code | — |
| `internal/callgraph/` | Call graph construction | analyzer |
| `internal/closure/` | Transitive closure over dependencies | callgraph |
| `internal/transform/` | AST transformations | analyzer |
| `cmd/cleat-worker/` | Worker daemon entry point | engine, migration |
| `cmd/cleatctl/` | CLI admin/debug tool | engine |
| `cmd/clew-service/` | Standalone clew HTTP service | engine |

## Coupling Matrix

- `cmd/cleat-worker` → `engine`: MEDIUM (consumes Engine API)
- `cmd/cleat-worker` → `migration`: LOOSE (runs migrations at startup)
- `cmd/cleatctl` → `engine`: MEDIUM (read-only Engine API for debug)
- `engine` → `wasm`: TIGHT (shared wasmtime types, WASM execution)
- `engine` → `plugin`: MEDIUM (plugin loading, call dispatch)
- `plugin` → `wasmrw`: LOOSE (uses wasmrw.OK / wasmrw.Error helpers)
- `internal/plugingen` → `plugin`: TIGHT (generates plugin code)
- `internal/callgraph` → `internal/analyzer`: TIGHT (shared AST types)
- `internal/closure` → `internal/callgraph`: TIGHT (operates on call graphs)
- `internal/transform` → `internal/analyzer`: TIGHT (shared AST types)

## Child Workflow API

The HostCalls interface exposes three child workflow APIs. They are not redundant — each serves a distinct purpose:

| API | Signature | WASM Import | Use Case |
|-----|-----------|-------------|----------|
| `ChildWorkflow` | `(name, inputJSON string) (runID, error)` | `cleat_child_workflow` | Default: start a child with no special options |
| `ChildWorkflowWithOptions` | `(name, inputJSON string, opts ChildWorkflowOptions) (runID, error)` | `cleat_child_workflow_with_options` | Start a child with version pinning, parent close policy, or priority |
| `ChildWorkflowTyped` | `(name string, request interface{}) (runID, error)` | `cleat_child_workflow` (via delegation) | Go-level typed convenience: marshals `request` to JSON, calls `ChildWorkflow` |

**Runtime delegation:**

- `ChildWorkflowTyped` marshals the typed request to JSON and calls `ChildWorkflow`.
- `ChildWorkflowWithOptions` uses the options handler when available; falls back to `ChildWorkflow` otherwise (backward compat for runtimes without options support).
- `ChildWorkflow` is the base primitive — lightweight at the WASM boundary (2 params) and sufficient for all calls that don't need version/policy/priority config.

**Guidance:**

- Prefer `ChildWorkflow` for simple child starts — it's the canonical form when no options are needed.
- Use `ChildWorkflowWithOptions` when you need version pinning, `ParentClosePolicy`, or `Priority`.
- Use `ChildWorkflowTyped` for type-safe Go ergonomics; it's a thin wrapper around `ChildWorkflow`.

All three APIs remain fully functional. None is deprecated.

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
