# cleat-232 Exploration Log — 7th Pass Verification

**Date:** 2026-06-05
**Agent:** explorer agent (7th pass — cleat-232m)
**Protocol:** No explorer-agent.md found; following STATUS.md as reference.

## Verification Results

All findings from STATUS.md (6th pass) re-verified against current working tree (HEAD: 010c2ed).

### Primary Issue: `auth/tenant_store.go` — CONFIRMED

| Line | Query | Verified |
|------|-------|----------|
| 29 | `INSERT INTO admin.tenants (name, display_name) VALUES ($1, $2) RETURNING tenant_id` | Match — `admin.` + `$N` + `RETURNING` all hardcoded |
| 39 | `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)` | Match |
| 47 | `UPDATE admin.tenant_api_keys SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL` | Match |

`NewTenantStore` still accepts only `*sql.DB` — no dialect parameter. Zero dialect awareness in `auth/` package (grep confirmed).

### Secondary Issue: `plugin/migration.go:244` — CONFIRMED

`RegisterPluginTables` still hardcodes `admin.` + `$N` + `ON CONFLICT DO NOTHING`.

### Secondary Issue: `plugin/tenant_db.go:57` — CONFIRMED

`admin.` + `$N`. Line 69 still `sql.Open("postgres", ...)`. Entire `TenantPools` system is PG-only.

### `::jsonb` Cast Audit — CONFIRMED SAFE

| File | Line | Verdict |
|------|------|---------|
| `engine/db.go` | 1406 | Safe — `PostgresStore.FinalizeWorkflowSegment` |
| `engine/db.go` | 1554 | Safe — `PostgresStore.ResolveTenantFromAPIKey` |
| `cmd/cleatctl/restore.go` | 213-215 | PG-only tool — `$4::jsonb`, `$7::TEXT`, `$7::TIMESTAMPTZ`, `ON CONFLICT` |
| `cmd/cleat/main.go` | 1032 | PG-only — `$5::jsonb` |

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

### Migration Gaps — CONFIRMED

| Backend | Count | Missing |
|---------|-------|---------|
| PostgreSQL | 9 (001-009) | None |
| MySQL | 7 (001-007) | 008, 009 |
| MSSQL | 8 (001-008) | 009 |

MSSQL 008 is a no-op placeholder (`SELECT 1;`) — MSSQL RLS is inherently fail-closed.

### Working Tree Diffs — UNCHANGED FROM 6TH PASS

1. **`auth/middleware.go`**: Extended public paths (`/`, `/index.html`, `/assets/*`, `/favicon.ico`) — makes `TestStoreAndMiddleware_EndToEnd` auth bypass worse
2. **`engine/db.go`**: Two changes:
   - Line 659: `tx.Exec` → `tx.ExecContext(context.Background(), ...)` — API hygiene
   - Lines 1341-1343: Empty/invalid JSON guard in `ContinueAsNew` — dialect-agnostic, safe

### `admin.` Schema Reference Audit (Complete)

```
auth/tenant_store.go:29,39,47 — PG-only queries (P0)
engine/db.go:1554             — PostgresStore, safe (PG-only)
plugin/migration.go:244       — RegisterPluginTables (P1)
plugin/tenant_db.go:57        — TenantPools (P2, inherently PG-only)
```

No new references. Count matches STATUS.md.

## Conclusion

No changes since 6th pass. All 6 passes are consistent. Exploration is complete and stable. The fix plan from STATUS.md remains correct:

1. P0: `auth/tenant_store.go` — add dialect awareness (accept `plugin.Dialect`, use `plugin.Query` or per-dialect methods)
2. P1: `auth/tenant_store_test.go` + `auth/fake_driver_test.go` — fix tests for `admin.` prefix
3. P1: `plugin/migration.go:244` — make `RegisterPluginTables` dialect-aware
4. P2: `plugin/tenant_db.go:57` — document as PG-only or add dialect awareness
5. P3: MySQL/MSSQL migrations 008, 009 — backfill or document gap
