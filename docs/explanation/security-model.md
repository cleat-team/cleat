# Security Model

This document describes the current and planned security mechanisms in cleat.

## WASM Sandbox

> Corrected 2026-08-09 (see CLAUDE.md's "Two WASM backends" section, and
> `engine/backend_wasmtime.go`, `engine/wasmtime_options.go`). This section
> previously said the primary security boundary was wazero and never
> mentioned wasmtime. That was backwards: **wasmtime is the backend of
> record** (preferred automatically whenever CGO is available — the default —
> per `cmd/cleat-worker/main.go`), and wazero is the CGO-less fallback only.
> The two are not equivalent for this purpose: per CLAUDE.md, wazero cannot be
> fenced for a compute-bound guest (`WithCloseOnContextDone` breaks all
> execution, fuel only decrements on function entry, and closing the module
> has no effect on a tight loop — all measured, all failing). Treat a
> wazero-only deployment as running without CPU/wall-clock enforcement.

The primary security boundary in cleat is the WebAssembly sandbox. On the
wasmtime backend it is enforced with real resource limits (below); on the
wazero fallback it is enforced only at the memory-limit and syscall level, not
at the CPU/wall-clock level.

### Current

- **No native code execution**: Workflow code runs inside the WASM sandbox
  (wasmtime by default, wazero as the CGO-less fallback). It cannot execute
  native CPU instructions, make arbitrary syscalls, or access host memory
  outside its linear memory.
- **WASI limited**: WASI preview 1 is instantiated but does not provide
  filesystem or network access by default.
- **Host functions controlled**: The only way workflow code interacts with the
  outside world is through the host functions registered on the `env` module
  (59 as of 2026-08-09 — `grep -c '\.Export("' engine/imports.go` plus the
  three non-`cleat_`-prefixed exports; see `ABI.md` §2). Each host function is
  a controlled Go function that validates inputs before acting.
- **Linear memory isolation**: Each WASM module gets its own linear memory.
  Modules cannot read or write each other's memory.
- **Resource limits are wired on the backend of record**: the wasmtime
  backend enforces a **guest-execution** timeout via epoch interruption
  (default 30s, `DefaultWasmtimeExecutionTimeout` in
  `engine/wasmtime_options.go`, configurable via `--wasm-instance-timeout`),
  an optional instruction/fuel budget (`--wasm-instruction-limit`), and a
  linear-memory ceiling (default 32 MiB per module,
  `DefaultWasmtimeMemoryLimitBytes`, configurable via `--wasm-memory-max-mb`).
  These bound even a WASM module stuck in a tight loop that never calls back
  into the host — see CLAUDE.md's note that this differs across wasmtime's
  three execution paths (core module, native component, decomposition).
- **Guest execution and wall clock are bounded separately.**
  `--wasm-instance-timeout` measures only time the guest is actually running:
  time it spends blocked in a host call — a service call, a plugin call, a
  retry backoff — is not charged against it, so a workflow waiting on slow
  dependencies is not killed as though it were a runaway guest.
  `--wasm-wall-clock-ceiling` (default 5m) is the bound that covers waiting,
  and is what stops an invocation blocked on an unresponsive service from
  holding a worker slot indefinitely. Set the ceiling at or above the instance
  timeout; below it, the epoch deadline is clamped to whatever the context has
  left and the ceiling silently becomes the guest's execution bound too (the
  worker warns at startup). See IMPROVEMENT-PLAN §3.90 for the measurements —
  before that item the two were one number, and a workflow making three 12s
  service calls tripped a 30s "runaway" fence.
- **A tenant can tighten the wall-clock ceiling for itself, and cannot loosen
  it.** `tenant_settings.wasm_wall_clock_ceiling_ms` (all three dialects) is
  an optional per-tenant override so that several microservices or
  organisations sharing one deployment can manage their own limits. The flag
  is a **ceiling**: the tenant's value is clamped to it, never substituted for
  it, so a tenant on a shared deployment cannot grant itself more than the
  operator allowed. `NULL` means "no override" and is distinct from zero —
  zero means *unbounded* at the point of use, so a `CHECK` constraint on each
  dialect refuses non-positive values, which is a privilege boundary rather
  than input validation. The table is tenant-scoped like any other: an RLS
  policy on PostgreSQL, a `FILTER PREDICATE` security policy on SQL Server,
  and on MySQL neither is needed because D1 makes it single-tenant. See
  IMPROVEMENT-PLAN §3.94 step 3. The instance timeout and the retry budget
  have columns but are **not yet consulted** — §3.94 steps 5b and 4.

### Limitations

- **WASM sandbox for versioning, not solely security**: The primary
  motivation for WASM compilation is lifecycle decoupling, not security.
  While the WASM sandbox provides defense-in-depth, it should not be the only
  security layer in multi-tenant deployments.
- **wazero has no CPU/wall-clock enforcement**: unlike wasmtime, wazero
  cannot be fenced for a compute-bound guest (see the callout above). A
  deployment that falls back to wazero (`CGO_ENABLED=0`, no CGO toolchain)
  loses this specific protection, not just performance.
- **No WASI preview 2 support**: Current Go `wasip1` target only supports WASI
  preview 1, which has a broader (and less secure) syscall surface than
  preview 2's component model.

### Planned

- WASI preview 2 support for finer-grained capability-based security.
- Per-instruction resource accounting that is uniform across wasmtime's three
  execution paths (core module, native component, decomposition), which
  today have had three different answers about what a limit bounds.

## PostgreSQL and SQL Server Row-Level Security

> Corrected 2026-08-09. This section previously said RLS was not yet
> configured ("Future"). That was wrong: RLS ships and is enforced today on
> both PostgreSQL and SQL Server. Verified via
> `grep -n "FORCE ROW LEVEL SECURITY" migrations/postgres/001_schema.sql`
> (8 tables: `workflow_defs`, `workflow_instances`, `event_history`,
> `workflow_signals`, `workflow_schedules`, `workflow_tags`,
> `workflow_routing`, `workflow_promises`) and
> `migrations/mssql/012_admin_role.sql:110-121`, which binds
> `dbo.fn_tenant_filter` as a `FILTER PREDICATE` security policy on the same
> seven multi-tenant tables. `tiers.yaml`'s D1 decision (2026-08-06) grants
> `multi_tenant: [postgres, mssql]`; MySQL has no row-level security feature
> at all (`CREATE POLICY`/`CREATE SECURITY POLICY` is not available), so it
> remains single-tenant only — see `docs/reference/multi-tenancy.md`.

### Current

- **Database-enforced tenant isolation on PostgreSQL and SQL Server**:
  PostgreSQL uses `CREATE POLICY` plus `FORCE ROW LEVEL SECURITY` on 8
  multi-tenant tables, scoped by a `tenant_id` session variable set per
  connection. SQL Server uses a native `SECURITY POLICY` with a
  `FILTER PREDICATE` function (`dbo.fn_tenant_filter`) bound to the same set
  of tables, checked against `SESSION_CONTEXT('tenant_id')`.
  `engine/rls_check.go` verifies the policies are present at startup and
  fails closed (`assert_tenant_set()`) rather than falling back to a default
  tenant.
- **MySQL is single-tenant only, by design**: MySQL has no row-level security
  feature (`CREATE POLICY` is a syntax error on 8.4). Application-layer
  tenant filtering was prototyped as an emulation and measured at 6.1x the
  cost of a full scan versus SQL Server's native +20% — see
  `IMPROVEMENT-PLAN.md` §1.7. Emulating a missing engine feature at 6x is a
  worse product than stating plainly which engines have it, so MySQL is
  documented single-tenant-only rather than pretending to isolate tenants it
  cannot actually fence.
- **Namespace isolation**: Independent of tenancy, workflows can also be
  scoped to a namespace via the `namespace` column; the worker filters by
  namespace in its claim queries.

### Planned

- Per-tenant connection pooling: the sharded store routing each tenant to its
  own database shard or schema, beyond the current session-variable model.

## API Key Authentication

### Current

The `internal/auth` package provides API key authentication for the worker's
HTTP API. Keys are prefixed with `cleat_sk_` and stored as SHA-256 hashes in a
`tenant_api_keys` database table.

**Auth mechanisms**:

```http
Authorization: Bearer cleat_sk_abc123...
# or
X-Cleat-API-Key: cleat_sk_abc123...
```

**Open endpoints** (no auth required):

- `/healthz` -- health check
- `/metrics` -- Prometheus metrics
- Internal worker-to-database operations (not HTTP)

### Implementation

The `auth.Middleware` function wraps the HTTP handler chain:

```go
func Middleware(db *sql.DB) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            key := extractAPIKey(r)
            if key == "" {
                // No key present -- pass through. Handler decides
                // whether to require auth for specific routes.
                next.ServeHTTP(w, r)
                return
            }
            keyHash := sha256Hash(key)
            tenantID, err := TenantFromAPIKey(r.Context(), db, keyHash)
            if err != nil {
                http.Error(w, `{"error":"invalid or revoked API key"}`, http.StatusUnauthorized)
                return
            }
            ctx := context.WithValue(r.Context(), tenantIDKey{}, tenantID)
            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}
```

### Limitations

- API key validation is a database round-trip per request.
- No key rotation or expiration mechanism in the middleware.
- No scoped keys (all keys have the same access level).

### Planned

- Scoped API keys with per-resource permissions.
- API key rotation and expiration.
- Rate limiting per API key.

## Secrets Handling

### Current

- **Secrets in workflow state**: Workflow input and event history are stored as
  `JSONB` columns in PostgreSQL. If secrets are passed as workflow input, they
  are stored in plaintext in the database.
- **No built-in secrets manager**: There is no integration with external secrets
  managers (HashiCorp Vault, AWS Secrets Manager, etc.).
- **Plugin-level secrets**: Plugins can read secrets from the environment or
  from the plugin configuration file (`--plugin-config`). Configuration is
  loaded once at startup and passed to `plugin.Environment.Config`.

### Recommendations for Production

1. Do not pass secrets as workflow input or output.
2. Use plugin-level secrets management (e.g., the plugin calls an external
   secrets manager at runtime).
3. Encrypt the `wasm_bytes` column in `workflow_defs` at the application layer
   if the WASM binary contains sensitive logic.
4. Use PostgreSQL's `pgcrypto` extension for column-level encryption of
   sensitive event history fields.

### Planned

- Secrets API (`h.Secret(key string) string`) on the `HostCalls` interface
  that reads from a configurable secrets backend.
- Transparent encryption of sensitive event history records.

## Input Validation

### Current

- **WASM boundary**: All string data crossing the WASM boundary is validated
  by the host functions before being acted upon. The validation is minimal --
  length checks and JSON parseability.
- **No schema validation**: Workflow inputs are accepted as arbitrary JSON.
  There is no schema enforcement at the API or database level.

### Boundary Validation Points

| Point | What is validated | Method |
|-------|-------------------|--------|
| HTTP API receive | JSON parseability, required fields | Go `json.Decoder` |
| Worker claim | `status`, `namespace`, `next_wake_at` | SQL WHERE clause |
| WASM host call | Service/operation names are non-empty, length limits | Host function prologue |
| Event persistence | Event type is known, step is sequential | Engine append logic |
| Signal delivery | `workflow_id` exists, `signal_name` is non-empty | SQL foreign key + check |

### Planned

- JSON Schema validation for workflow inputs (defined in `workflow_defs`).
- SQL injection prevention through parameterized queries (already standard).

## Worker Security

### Current

- Workers require database credentials to claim and execute workflows.
- Workers do not expose a management interface (no SSH, no admin endpoints).
- HTTP API is optional (`--api-addr` flag). When disabled, the worker has no
  network listener.

### Recommendations for Production

1. Run workers in a private network with no public exposure.
2. Use a dedicated PostgreSQL user with minimal permissions (SELECT/INSERT on
   workflow tables, no DDL).
3. Encrypt worker-to-database connections (TLS/SSL).
4. Set `--api-addr` only when the HTTP API or web UI is needed, and place a
   reverse proxy with authentication in front.

## Summary

| Area | Current State | Planned |
|------|---------------|---------|
| WASM sandbox | wasmtime (backend of record: epoch/fuel/memory limits) or wazero (CGO-less fallback, no CPU/wall-clock enforcement); no native code, controlled host functions | WASI preview 2, uniform limits across all three wasmtime execution paths |
| PostgreSQL / SQL Server RLS | Database-enforced, FORCEd/FILTER PREDICATE on 8 tables, fail-closed | Per-tenant connection pooling / sharding |
| MySQL tenancy | Single-tenant only (no RLS feature; documented product boundary, not a gap) | — |
| API auth | Bearer token / header-based, SHA-256 hashed | Scoped keys, rotation, rate limiting |
| Secrets | Plaintext in DB, no built-in secrets manager | Secrets API on HostCalls, encryption |
| Input validation | Minimal (length, JSON parseability) | JSON Schema, stricter enforcement |
| Worker network | Optional API listener, DB connection only | Managed worker fleet with mTLS |
