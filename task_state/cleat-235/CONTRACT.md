# CONTRACT: cleat-235 — Code Review

## Deliverables

1. **Engine hot-path review**: Race conditions, error handling, resource leaks — report with findings and fixes
2. **WASM compliance review**: Component model spec compliance, ABI correctness
3. **Security review**: Encryption-at-rest, auth edge cases, SQL injection audit
4. **Error handling audit**: All discarded errors reviewed, `defer` cleanup ordering verified
5. **Recent commit review**: Commits `1b7f8ed`, `8d9b6f6`, `98e32dd` reviewed post-merge
6. **All findings fixed** — not just reported

## Invariants

- No new features introduced during review fixes
- ABI frozen for 0.5 — no host call signature changes
- Review fixes must not regress any existing tests

## Review Checklist

| Area | Files | Key Focus |
|------|-------|-----------|
| Signal routing | `engine/engine.go` (~2447-2576) | Replay determinism, signal store fallback correctness |
| WASM dispatch | `engine/backend_wasmtime.go` | Error handling (line 158 discarded error), input validation |
| DB layer | `engine/db.go` | `json.Valid` guard, `::jsonb` cast, SQL injection |
| Auth | `auth/middleware.go`, `auth/tenant_store.go` | Rate limiting, key rotation, tenant isolation |
| WASM ABI | `engine/imports.go` | 53 `cleat_*` exports — verify signatures match SDKs |
| Worker daemon | `cmd/cleat-worker/` | Graceful shutdown, error recovery |

## Test Requirements

- All existing tests continue to pass
- If review finds a bug, add a regression test
- No reduction in coverage

## Integration Points

- Review findings may create follow-up tasks (document in findings report)
- Auth review overlaps with cleat-232 (tenant_store.go) — coordinate if both active
- WASM review overlaps with cleat-233 (SDK tests) — coordinate if both active

## Coupling

- LOOSE with `cleat-232` (same auth/tenant_store.go, engine/db.go files)
- LOOSE with `cleat-233` (same wasm/ files)
- LOOSE with `cleat-236` (review findings may require doc updates)
- NONE with other leaf tasks
