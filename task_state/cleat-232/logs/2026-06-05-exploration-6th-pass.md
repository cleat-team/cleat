# cleat-232 Exploration Log — 6th Pass Verification

**Date:** 2026-06-05
**Agent:** explorer agent (6th pass — cleat-232k)
**Protocol:** No explorer-agent.md found; following STATUS.md as reference.

## Verification Results

All findings from STATUS.md (5th pass) re-verified against current working tree.

### Primary Issue: `auth/tenant_store.go` — CONFIRMED

| Line | Query | Verified |
|------|-------|----------|
| 29 | `INSERT INTO admin.tenants (name, display_name) VALUES ($1, $2) RETURNING tenant_id` | Match — `admin.` + `$N` + `RETURNING` all hardcoded |
| 39 | `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)` | Match |
| 47 | `UPDATE admin.tenant_api_keys SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL` | Match |

Struct still has only `*sql.DB` — no dialect parameter. `NewTenantStore` signature unchanged.

### Secondary Issue: `plugin/migration.go:244` — CONFIRMED

```go
`INSERT INTO admin.plugin_tables (plugin_name, table_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`
```

`admin.` + `$N` + `ON CONFLICT` — all PG-only.

### Secondary Issue: `plugin/tenant_db.go:57` — CONFIRMED

```go
`SELECT role_name, password FROM admin.tenant_roles WHERE tenant_id = $1`
```

`admin.` + `$N`. Line 69 still `sql.Open("postgres", tenantDSN)` — entire system is PG-only.

### `::jsonb` Cast Audit — CONFIRMED SAFE

| File | Line | Verdict |
|------|------|---------|
| `engine/db.go` | 1406 | Safe — in `PostgresStore.FinalizeWorkflowSegment` |
| `engine/db.go` | 1554 | Safe — in `PostgresStore.ResolveTenantFromAPIKey` |
| `cmd/cleatctl/restore.go` | 213-215 | PG-only tool — `$4::jsonb`, `$7::TEXT`, `$7::TIMESTAMPTZ`, `ON CONFLICT DO NOTHING` |
| `cmd/cleat/main.go` | 1032 | PG-only — `$5::jsonb` |

### Engine Store Architecture — CONFIRMED CORRECT

Each store implementation uses the correct table references and placeholder syntax for its backend:
- `PostgresStore` line 1554: `admin.tenant_api_keys` + `$1`
- `MySQLStore` line 1742: `tenant_api_keys` (no prefix) + `?`
- `MSSQLStore` line 1626: `tenant_api_keys` (no prefix) + `@p1`

Neither `mysql_store.go` nor `mssql_store.go` contain any `admin.` references.

### Fake Driver Test Mismatch — CONFIRMED

All 4 query matchers in `auth/fake_driver_test.go` match WITHOUT the `admin.` prefix:
- Line 94: `"SELECT tenant_id FROM tenant_api_keys"`
- Line 98: `"INSERT INTO tenants"`
- Line 113: `"INSERT INTO tenant_api_keys"`
- Line 115: `"UPDATE tenant_api_keys SET revoked_at"`

These will fail against the `admin.`-qualified queries produced by `TenantStore`.

### Migration Gaps — CONFIRMED

| Backend | Migrations | Missing |
|---------|-----------|---------|
| PostgreSQL | 001, 002, 003, 005, 006, 007(fk+event_history), 008, 009 (9) | None |
| MySQL | 001, 002, 003, 005, 006, 007(fk+event_history) (7) | 008, 009 |
| MSSQL | 001, 002, 003, 005, 006, 007(fk+event_history), 008 (8) | 009 |

MSSQL 008 is a no-op placeholder (`SELECT 1;`) as documented — MSSQL RLS is inherently fail-closed.

## Working Tree Changes Since 5th Pass

### `auth/middleware.go` — Extended Public Paths
The 5th pass noted only `"/"` was added. The current working tree extends this further:
```go
if path == "/healthz" || path == "/metrics" ||
    path == "/" || path == "/index.html" ||
    strings.HasPrefix(path, "/assets/") ||
    (path == "/favicon.ico") {
```
This makes the fake driver test mismatch worse: `TestStoreAndMiddleware_EndToEnd` sends a request to `"/"`, which now bypasses auth entirely. The test may pass for the wrong reason (auth is skipped) rather than testing the auth flow.

### `engine/db.go` — Two Changes
1. Line 659: `tx.Exec` → `tx.ExecContext(context.Background(), ...)` — API hygiene, no multi-DB impact.
2. Lines 1341-1343: Empty/invalid JSON handling in `ContinueAsNew` — `if result == "" || !json.Valid(...)` → `result = "{}"`. Dialect-agnostic, safe.

## `admin.` Schema Reference Audit (Complete)

All `admin.` references in Go files:
```
auth/tenant_store.go:29,39,47 — PG-only queries (P0)
engine/db.go:1554             — PostgresStore, safe (PG-only)
plugin/migration.go:244       — RegisterPluginTables (P1)
plugin/tenant_db.go:57        — TenantPools (P2, inherently PG-only)
```

No new references found. Complete count matches STATUS.md.

## Conclusion

Exploration is complete and verified across 6 independent passes. All findings are stable. No regressions or new issues found in the working tree. The two working-tree changes (middleware public paths, engine.go JSON handling) do not affect the core multi-DB fix scope but do make the fake driver test issue more acute.

**Priority-ordered fix plan remains:**
1. P0: `auth/tenant_store.go` — add dialect awareness
2. P1: `auth/tenant_store_test.go` + `auth/fake_driver_test.go` — fix tests for `admin.` prefix
3. P1: `plugin/migration.go:244` — make dialect-aware
4. P2: `plugin/tenant_db.go:57` — document as PG-only or add dialect awareness
5. P3: MySQL/MSSQL migrations 008, 009 — backfill or document gap
