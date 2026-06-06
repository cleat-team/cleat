# cleat-232 Exploration Log — 5th Pass Verification

**Date:** 2026-06-05
**Agent:** explorer agent (5th pass — cleat-232c)
**Protocol:** explorer-agent.md does not exist; following STATUS.md as reference.

## Verification Results

Every finding in STATUS.md was re-verified against the current working tree.

### Primary Issue: `auth/tenant_store.go` — CONFIRMED

| Line | STATUS.md Claim | Verified |
|------|-----------------|----------|
| 29 | `INSERT INTO admin.tenants (name, display_name) VALUES ($1, $2) RETURNING tenant_id` | Match |
| 39 | `INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)` | Match |
| 47 | `UPDATE admin.tenant_api_keys SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL` | Match |

Struct still has only `*sql.DB` — no dialect parameter.

### Secondary Issue: `plugin/migration.go:244` — CONFIRMED

```go
`INSERT INTO admin.plugin_tables (plugin_name, table_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`
```

### Secondary Issue: `plugin/tenant_db.go:57` — CONFIRMED

```go
`SELECT role_name, password FROM admin.tenant_roles WHERE tenant_id = $1`
```

### `::jsonb` Cast Audit — CONFIRMED SAFE

| File | Line | Verdict |
|------|------|---------|
| `engine/db.go` | 1405 | Safe — in PostgresStore.FinalizeWorkflowSegment |
| `engine/db.go` | 1554 | Safe — in PostgresStore.ResolveTenantFromAPIKey |

### Engine Store Architecture — CONFIRMED CORRECT

- `PostgresStore`: `admin.tenant_api_keys`, `$N` placeholders, `::jsonb`
- `MySQLStore` (line 1742): `tenant_api_keys` (no prefix), `?` placeholders, no `::jsonb`
- `MSSQLStore` (line 1626): `tenant_api_keys` (no prefix), `@pN` placeholders, no `::jsonb`

Neither `mysql_store.go` nor `mssql_store.go` contain `admin.` references.

### Fake Driver Test Mismatch — CONFIRMED

All 4 query matchers in `auth/fake_driver_test.go` use table names without `admin.` prefix:
- Line 94: `"SELECT tenant_id FROM tenant_api_keys"`
- Line 98: `"INSERT INTO tenants"`
- Line 113: `"INSERT INTO tenant_api_keys"`
- Line 115: `"UPDATE tenant_api_keys SET revoked_at"`

### Middleware Public Path — CONFIRMED

Line 47 in `auth/middleware.go` adds `"/"` as a public path.

### Migration Gaps — CONFIRMED

| Backend | Migrations Present | Missing |
|---------|-------------------|---------|
| PostgreSQL | 001-003, 005-009 (9) | None |
| MySQL | 001-003, 005-007 (7) | 008, 009 |
| MSSQL | 001-003, 005-008 (8) | 009 |

## Discrepancies from STATUS.md

None. All findings in STATUS.md dated 2026-06-05 are still accurate.

## Conclusion

Exploration is complete and verified. No new issues discovered. The task is ready for implementation and dispatch.
