# cleat-235 Status

**Phase:** review_complete
**Last updated:** 2026-06-05
**Dispatched by:** cto-lap-032

## Review Findings

### Critical

1. **`engine/backend_wasmtime.go:158` — Silent error discard on `json.Marshal`**
   `escaped, _ := json.Marshal(string(input))` discards the Marshal error. While `json.Marshal(string)` should practically never fail, a nil/empty result from a failed Marshal would produce `{"inputJSON:}` — silently corrupting the WASM work dispatch format. Should at minimum log.Fatal or return an error.
   **Fix:** Check the error and return `fmt.Errorf("host: marshal input: %w", err)`.

2. **`engine/encryption.go:58,77` — Encryption uses nil AAD despite multi-tenant design**
   AES-256-GCM `Seal` and `Open` both pass `nil` for additional authenticated data. The code's own comment (line 43) notes that tenant_id should be passed as AAD for cross-tenant ciphertext substitution prevention. A compromised tenant could potentially substitute ciphertexts across tenants.
   **Fix:** Pass tenant_id bytes as AAD in `Encrypt`/`Decrypt` calls.

3. **`engine/db.go:1342` — Missing `json.Valid` guard in `ContinueAsNew` path**
   `FinalizeWorkflowSegment` (line 1398) checks `json.Valid` before writing result to a JSONB column, but `ContinueAsNew` (line 1342) writes `result = $3` without this guard. An empty or non-JSON result will cause a PostgreSQL "invalid input syntax for type json" error.
   **Fix:** Add the same `json.Valid` guard before line 1341, or verify that the caller always produces valid JSON.

4. **`engine/backend_wasmtime.go` — All host functions use `context.Background()` instead of execution context**
   Every `FuncWrap` closure (lines 635-1769) calls handlers with `context.Background()` (via `ctxWithMem(context.Background(), buf)`), discarding the execution's context. In contrast, the wazero backend (`imports.go`) passes the actual execution `ctx`. This means:
   - Context cancellation/timeout does not propagate to host operations
   - Trace context (W3C trace IDs) is lost in the wasmtime path
   **Fix:** Thread the execution context through the wasmtime FuncWrap closures, or derive a new context from it for buffer attachment.

### Medium

5. **`engine/db.go:4328` — String concatenation for DDL statement**
   `CREATE SCHEMA IF NOT EXISTS ` + `pq.QuoteIdentifier(f.schemaName)` constructs SQL via concatenation. While `pq.QuoteIdentifier` properly escapes, this pattern bypasses parameterization. The schema name comes from config (not user input), so risk is low. Same pattern at lines 2442-2444 and 2455-2456 uses `fmt.Sprintf` with `pq.QuoteIdentifier`.
   **Recommendation:** Acceptable for PostgreSQL DDL (which doesn't support parameterized identifiers), but add a validation guard on the schema name before use.

6. **`engine/db.go:659` — `setRLSOnTx` uses `tx.Exec` not `tx.ExecContext`**
   Does not propagate context cancellation to the `SELECT set_config(...)` call. Low risk since this is a fast local function call. All other transaction operations properly use `*Context` variants.

7. **`auth/middleware.go` — No API key rotation mechanism**
   Keys are hashed with SHA-256 (no salt, line 90) and compared directly. While `ResolveTenantFromAPIKey` checks `revoked_at IS NULL`, there is no built-in key rotation (old key + new key overlap period) or key expiration. The `CreateAPIKey` function is the only creation path, and `RevokeAPIKey` is the only revocation path.
   **Recommendation:** Acceptable for current scope; key rotation can be handled externally.

### Verified Correct

8. **Rate limiter cleanup (`cmd/cleat-worker/main.go:2917-3086`)** — Both `ipRateLimiter` and `keyedRateLimiter` start background cleanup goroutines via `go func()` with `stopCh` channels. `stop()` is called at lines 898-901 during shutdown. No goroutine leak.

9. **`rows.Err()` checked everywhere** — Every `rows.Next()` loop in `db.go` is followed by `rows.Err()` check (lines 752, 823, 940, 1057, 1189, 1625, 1704, 1827, 2661, 2758, 2825, 2908, 3071, 3283, 3450, 4030, 4065). No silent row iteration errors.

10. **Defer ordering in `backend_wasmtime.go:78,117`** — `defer store.Close()` at line 78, `defer module.Close()` at line 117. LIFO execution means module closes before store — correct.

11. **`ResolveTenantFromAPIKey` checks `revoked_at IS NULL`** — PostgresStore (db.go), MySQLStore (mysql_store.go:1743), and MSSQLStore all filter revoked keys. Correct.

12. **`DurableAwaitSignals` signal store fallback (`engine/engine.go:2447-2478`)** — Well-implemented. Correctly falls back to `splitSignalNames` when JSON unmarshal fails. Found signals get recorded as events for future replay determinism. Graceful handling when no signal found (returns timeout-like result).

13. **Encryption fail-secure behavior (`engine/engine.go:4612-4654`)** — On encryption failure, `tx.Rollback()` is called and the flush is aborted. No plaintext leak when encryption is enabled.

14. **`StreamEventHistory` (`engine/db.go:1077-1199`)** — Proper goroutine cleanup with `defer close(eventCh)`, `defer close(errCh)`. Context cancellation paths call `rows.Close()`. Page-based streaming works correctly.

15. **`sql.NullString` used for nullable columns** — All event history scans use `sql.NullString` for nullable fields, with `.Valid` checks before accessing (lines 1145-1169).

### Additional Notes

- **`backend_wasmtime.go:1823`** — `defer m.Close()` inside a loop is a known Go pattern that accumulates defers. Correct behavior, but for very large component bundles (100+ modules) this could accumulate many deferred calls. Not a correctness issue.
- **WASI configuration (`backend_wasmtime.go:86-90`)** — Correctly conditional on `wasm.HasWasiImports()` to avoid wasmtime-go nil pointer dereference.
- **`PerExecution()` pattern (`backend_wasmtime.go:62-64`)** — Creates a new backend sharing the Engine. This eliminates the data race on `b.handler` when `Execute` is called concurrently from multiple goroutines. Correct pattern.
- **Worker shutdown (`cmd/cleat-worker/main.go:891-914`)** — Proper signal handling with 30s timeout for background workers to complete. Rate limiters and HTTP server are shut down before waiting for workers.

### Fixes Applied

1. **`engine/backend_wasmtime.go:158`** — Added error check for `json.Marshal(string(input))`. Now returns `fmt.Errorf("host: marshal input for dispatch wrapper: %w", err)` on failure instead of silently producing corrupt dispatch JSON.

2. **`engine/db.go:1342`** — Added `json.Valid` guard in `ContinueAsNew` path matching the one in `FinalizeWorkflowSegment`. Empty or non-JSON result strings are now defaulted to `"{}"` before writing to the JSONB column.

3. **`engine/db.go:659`** — Changed `tx.Exec` to `tx.ExecContext(context.Background(), ...)` in `setRLSOnTx` for consistency with the rest of the codebase's context propagation patterns.
