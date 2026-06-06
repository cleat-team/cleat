# cleat-232 Exploration Log — 10th Pass Verification

**Date:** 2026-06-05
**Agent:** explorer agent (10th pass — cleat-232-tenant-storee)
**Protocol:** No explorer-agent.md found; following STATUS.md as reference.

## Verification Results

All findings from STATUS.md (9th pass) re-verified against current working tree (HEAD: 010c2ed).

### Primary Issue: `auth/tenant_store.go` — CONFIRMED

| Line | Query | Verified |
|------|-------|----------|
| 29 | `INSERT INTO admin.tenants (name, display_name) VALUES ($1, $2) RETURNING tenant_id` | Match — `admin.` + `$N` + `RETURNING` all hardcoded |
| 39 | `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)` | Match |
| 47 | `UPDATE admin.tenant_api_keys SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL` | Match |

`NewTenantStore` still accepts only `*sql.DB` — no dialect parameter. Zero dialect awareness in `auth/` package (grep confirmed: `Dialect\|dialect` returns no matches). `git diff auth/tenant_store.go` is clean (no working tree changes).

### Secondary Issue: `plugin/migration.go:244` — CONFIRMED

`RegisterPluginTables` still hardcodes `admin.plugin_tables` + `$N` + `ON CONFLICT DO NOTHING`. No working tree changes. Comment at lines 37-38 still documents the known gap.

### Secondary Issue: `plugin/tenant_db.go:57` — CONFIRMED

`SELECT role_name, password FROM admin.tenant_roles WHERE tenant_id = $1` — `admin.` + `$N`. Line 69 still `sql.Open("postgres", ...)`. Entire `TenantPools` system is PG-only. No working tree changes.

### `::jsonb` Cast Audit — CONFIRMED SAFE

- `engine/db.go:1406` — Safe: `PostgresStore.FinalizeWorkflowSegment`, PostgreSQL-specific
- `cmd/cleatctl/restore.go:213` — PG-only tool
- `cmd/cleat/main.go:1032` — PG-only tool
- `plugins/eventstore/queries.go:12` — Already dialect-aware (has Default/MySQL/MSSQL variants)

### Engine Store Architecture — CONFIRMED CORRECT

- `PostgresStore` (db.go:1554): `admin.tenant_api_keys` + `$1`
- `MySQLStore` (mysql_store.go:1742): `tenant_api_keys` (no prefix) + `?`
- `MSSQLStore` (mssql_store.go:1626): `tenant_api_keys` (no prefix) + `@p1`

### Fake Driver Test Mismatch — CONFIRMED

All 4 query matchers in `auth/fake_driver_test.go` match WITHOUT `admin.` prefix:
- Line 94: `"SELECT tenant_id FROM tenant_api_keys"`
- Line 98: `"INSERT INTO tenants"`
- Line 113: `"INSERT INTO tenant_api_keys"`
- Line 115: `"UPDATE tenant_api_keys SET revoked_at"`

No working tree changes to this file.

### Migration Gaps — CONFIRMED

| Backend | Count | Files | Missing |
|---------|-------|-------|---------|
| PostgreSQL | 9 | 001-009 | None |
| MySQL | 7 | 001-007 | 008, 009 |
| MSSQL | 8 | 001-008 | 009 |

MSSQL 008 is a no-op placeholder (`SELECT 1;`) — MSSQL RLS is inherently fail-closed.

### Working Tree Diffs — UNCHANGED FROM 9TH PASS

Same files with relevant diffs:
1. **`auth/middleware.go`**: Extended public paths (`/`, `/index.html`, `/assets/*`, `/favicon.ico`)
2. **`engine/db.go`**: `tx.Exec` → `tx.ExecContext(context.Background(), ...)` + empty/invalid JSON guard in `ContinueAsNew`
3. **`engine/backend_wasmtime.go:158`**: Error check on `json.Marshal`

All other modified files are unrelated (web UI, task_state, AssemblyScript SDK, etc.).

### `admin.` Schema Reference Audit (Complete)

```
auth/tenant_store.go:29,39,47 — PG-only queries (P0)
engine/db.go:1554             — PostgresStore, safe (PG-only)
plugin/migration.go:244       — RegisterPluginTables (P1)
plugin/tenant_db.go:57        — TenantPools (P2, inherently PG-only)
```

7 total `admin.` references in Go source. Count matches 9th pass. No new references.

## Conclusion

**No changes since 9th pass. All 10 passes are consistent.** Exploration is complete and stable. The fix plan from STATUS.md remains correct:

1. P0: `auth/tenant_store.go` — add dialect awareness (accept `plugin.Dialect`, use per-dialect SQL)
2. P1: `auth/tenant_store_test.go` + `auth/fake_driver_test.go` — fix tests for `admin.` prefix
3. P1: `plugin/migration.go:244` — make `RegisterPluginTables` dialect-aware
4. P2: `plugin/tenant_db.go:57` — document as PG-only or add dialect awareness
5. P3: MySQL/MSSQL migrations 008, 009 — backfill or document gap
