# Security Model

This document describes the current and planned security mechanisms in cleat.

## WASM Sandbox

The primary security boundary in cleat is the WebAssembly sandbox provided by
wazero.

### Current

- **No native code execution**: Workflow code runs inside wazero's WASM
  interpreter or compiler. It cannot execute native CPU instructions, make
  arbitrary syscalls, or access host memory outside its linear memory.
- **WASI limited**: WASI preview 1 is instantiated but does not provide
  filesystem or network access by default.
- **Host functions controlled**: The only way workflow code interacts with the
  outside world is through the 15+ host functions registered on the `env`
  module. Each host function is a controlled Go function that validates inputs
  before acting.
- **Linear memory isolation**: Each WASM module gets its own linear memory.
  Modules cannot read or write each other's memory.

### Limitations

- **WASM sandbox for versioning, not security**: The primary motivation for
  WASM compilation is lifecycle decoupling, not security. While the WASM
  sandbox provides defense-in-depth, it should not be the only security layer
  in multi-tenant deployments.
- **No resource limits**: wazero does not enforce WASM-level CPU or memory
  limits. A runaway workflow could consume excessive CPU (though it cannot
  escape the sandbox).
- **No WASI preview 2 support**: Current Go `wasip1` target only supports WASI
  preview 1, which has a broader (and less secure) syscall surface than
  preview 2's component model.

### Planned

- Per-module CPU and memory limits via wazero's `WithMemoryLimitConfig` and
  `WithCompiledModule` configuration.
- WASI preview 2 support for finer-grained capability-based security.

## PostgreSQL Row-Level Security (Future)

### Current

- **Namespace isolation**: Workflows can be scoped to a namespace via the
  `namespace` column. The worker filters by namespace in its claim queries.
- **No RLS**: PostgreSQL RLS (Row-Level Security) policies are not yet
  configured. All workflows in the database share the same worker connection.

### Planned

- **Tenant isolation via RLS**: Each tenant will have a dedicated connection
  with a `tenant_id` session variable. RLS policies on `workflow_instances`,
  `event_history`, and `workflow_defs` will filter by tenant.
- **Per-tenant connection pooling**: The sharded store will route each tenant
  to its own database shard or schema.

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
| WASM sandbox | wazero runtime, no native code, controlled host functions | Resource limits, WASI preview 2 |
| PostgreSQL RLS | Namespace filter only | Per-tenant RLS policies |
| API auth | Bearer token / header-based, SHA-256 hashed | Scoped keys, rotation, rate limiting |
| Secrets | Plaintext in DB, no built-in secrets manager | Secrets API on HostCalls, encryption |
| Input validation | Minimal (length, JSON parseability) | JSON Schema, stricter enforcement |
| Worker network | Optional API listener, DB connection only | Managed worker fleet with mTLS |
