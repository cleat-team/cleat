# cleat-235: Code Review

**Budget:** $20 (~2 days)
**Priority:** 2 (quality baseline)
**Status:** pending
**Depends on:** none

## Scope

Comprehensive review of the entire codebase for correctness and safety.

## Actions

1. Review all `engine/` hot paths for race conditions, error handling gaps, and resource leaks
2. Review WASM component model implementation for spec compliance
3. Verify encryption-at-rest paths (workflow payload encryption)
4. Review auth middleware for edge cases (rate limiting, API key rotation, tenant isolation)
5. Check all `defer` statements for correct cleanup ordering
6. Review SQL queries for injection risks (parameterized queries)
7. Any findings get fixed

## Key Files

- `engine/` — all hot paths
- `auth/` — middleware, tenant store
- `wasm/` — component model implementation
- `cmd/cleat-worker/` — worker daemon

## Additional Scope (from surveys)

- Review commits `1b7f8ed` (WASM input dispatch), `8d9b6f6` (jsonb validation), `98e32dd` (signal routing fix)
- `engine/backend_wasmtime.go:158` — `escaped, _ := json.Marshal(string(input))` silently discards error
- `engine/engine.go` — `DurableAwaitSignals` replay path signal store fallback (30 lines new code, commit 98e32dd)
- `engine/db.go:1398` — `json.Valid` guard (commit 8d9b6f6) + `::jsonb` cast (commit 98e32dd)
