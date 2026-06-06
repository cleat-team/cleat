# cleat-232 Exploration Log — 4th Pass Verification

**Date:** 2026-06-05
**Agent:** explorer agent (4th pass — re-verification)
**Protocol:** No explorer-agent.md protocol file exists; following STATUS.md as reference.
**Task ID note:** Invoked as `cleat-232i`, but task directory is `cleat-232`.

## Protocol Issue

The instructions referenced `/localssd/rcownie/cleat/prompts/explorer-agent.md` which does not exist. No explorer agent protocol file was found in the repo or in `cleat-internal/prompts/`. Followed the CTO agent prompt (`prompts/cto-agent.md`) and STATUS.md as guidance instead.

## Verification Results

Every finding in STATUS.md was re-verified against the current working tree.

### Primary Issue: `auth/tenant_store.go` — CONFIRMED

| Line | STATUS.md Claim | Verified |
|------|-----------------|----------|
| 29 | `INSERT INTO admin.tenants (name, display_name) VALUES ($1, $2) RETURNING tenant_id` | Match |
| 39 | `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)` | Match |
| 47 | `UPDATE admin.tenant_api_keys SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL` | Match |

Struct still has only `*sql.DB` — no dialect parameter. `grep -rn "Dialect\|dialect" auth/` returns no matches.

### Secondary Issue: `plugin/migration.go:244` — CONFIRMED

```go
`INSERT INTO admin.plugin_tables (plugin_name, table_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`
```

Still PG-specific. Comment at lines 37-38 still says "RegisterPluginTables remains PostgreSQL-specific."

### Secondary Issue: `plugin/tenant_db.go:57` — CONFIRMED

Line 57: `SELECT role_name, password FROM admin.tenant_roles WHERE tenant_id = $1`
Line 69: `sql.Open("postgres", tenantDSN)` still hardcoded.

### `::jsonb` Cast Audit — CONFIRMED SAFE

| File | Line | Claim | Verified |
|------|------|-------|----------|
| `engine/db.go` | 1405 | Safe — in `PostgresStore.FinalizeWorkflowSegment` | Match |
| `engine/db.go` | 1554 | Safe — in `PostgresStore.ResolveTenantFromAPIKey` | Match |
| `engine/mysql_store.go` | 1742 | MySQL has own impl without `::jsonb`, uses `tenant_api_keys` (no prefix) | Match |
| `engine/mssql_store.go` | 1626 | MSSQL has own impl without `::jsonb`, uses `tenant_api_keys` (no prefix) | Match |

### Engine Store Architecture — CONFIRMED CORRECT

All three stores properly separated with dialect-specific SQL:
- `PostgresStore`: `$N` placeholders, `::jsonb`, `admin.` prefix
- `MySQLStore`: `?` placeholders, `NOW(6)`, no `admin.` prefix
- `MSSQLStore`: `@pN` placeholders, `SYSUTCDATETIME()`, no `admin.` prefix

### Fake Driver Test Mismatch — CONFIRMED

`auth/fake_driver_test.go` matches queries without `admin.` prefix:
- Line 94: `"SELECT tenant_id FROM tenant_api_keys"` — won't match `admin.tenant_api_keys`
- Line 98: `"INSERT INTO tenants"` — won't match `admin.tenants`
- Line 113: `"INSERT INTO tenant_api_keys"` — won't match `admin.tenant_api_keys`
- Line 115: `"UPDATE tenant_api_keys SET revoked_at"` — won't match `admin.tenant_api_keys`

### Middleware Public Path — CONFIRMED

Line 47 in `auth/middleware.go` adds `"/"` as a public path (working tree change). This would bypass auth for `TestStoreAndMiddleware_EndToEnd` which sends requests to `"/"`.

### Migration Gaps — CONFIRMED

| Backend | Migrations Present | Missing |
|---------|-------------------|---------|
| PostgreSQL | 001-009 (9 migrations) | None |
| MySQL | 001-003, 005-007 (7 migrations) | 008_rls_fail_closed, 009_generation |
| MSSQL | 001-003, 005-008 (8 migrations) | 009_generation |

## Discrepancies from STATUS.md

None. All findings in STATUS.md dated 2026-06-05 (3rd pass) are still accurate.

## Conclusion

The exploration is complete and verified. No new issues discovered. The task is ready for implementation. Priority order from STATUS.md remains correct:
1. P0: `auth/tenant_store.go` — dialect-aware queries
2. P1: `auth/tenant_store_test.go` — MySQL/MSSQL variants; `plugin/migration.go` — dialect-aware RegisterPluginTables
3. P2: `plugin/tenant_db.go` — document as PG-only or make dialect-aware
4. P3: `cmd/cleatctl/restore.go`, `cmd/cleat/main.go` — verify PG-only status
