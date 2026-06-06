# cleat-232 Exploration Log — 11th Pass Verification

**Date:** 2026-06-05
**Agent:** explorer agent (11th pass — cleat-232-tenant-storep)
**Protocol:** No explorer-agent.md found; following STATUS.md as reference.

## Verification Results

All findings from STATUS.md (10 passes) re-verified against current working tree (HEAD: 010c2ed). 38 files modified in working tree, unchanged from 10th pass.

### Primary Issue: `auth/tenant_store.go` — CONFIRMED

Lines 29, 39, 47 — all hardcode `admin.` schema prefix + `$N` placeholders + `RETURNING`. `NewTenantStore` accepts only `*sql.DB`. Zero dialect awareness in `auth/`. No working tree changes.

### Secondary: `plugin/migration.go:244` — CONFIRMED

`admin.plugin_tables` + `$N` + `ON CONFLICT DO NOTHING`. Unchanged.

### Secondary: `plugin/tenant_db.go:57` — CONFIRMED

`admin.tenant_roles` + `$N` + `sql.Open("postgres", ...)`. Entire TenantPools system is PG-only. Unchanged.

### `::jsonb` Cast Audit — SAFE

- `engine/db.go:1406` — `PostgresStore.FinalizeWorkflowSegment` (PG-only)
- `plugins/eventstore/queries.go:12` — already dialect-aware
- `cmd/cleat/main.go:1032` — PG-only deploy
- `cmd/cleatctl/restore.go:213` — PG-only restore

### Engine Store Architecture — CORRECT

- PostgresStore: `admin.tenant_api_keys` + `$1` (db.go:1554)
- MySQLStore: `tenant_api_keys` + `?` (mysql_store.go:1742)
- MSSQLStore: `tenant_api_keys` + `@p1` (mssql_store.go:1626)

### Fake Driver Mismatch — CONFIRMED

All 4 matchers in `auth/fake_driver_test.go` (lines 94, 98, 113, 115) match WITHOUT `admin.` prefix. Tests likely broken since commit a019068.

### Migration Gaps — CONFIRMED

PostgreSQL: 9/9 | MySQL: 7/9 (missing 008, 009) | MSSQL: 8/9 (missing 009)

### Relevant Working Tree Diffs — UNCHANGED

1. `auth/middleware.go` — extended public paths
2. `engine/db.go` — ExecContext + empty JSON guard (dialect-agnostic)
3. `engine/backend_wasmtime.go:158` — json.Marshal error check

## Conclusion

No changes since 10th pass. All 11 passes consistent. Fix plan unchanged:

- P0: `auth/tenant_store.go` — add dialect awareness
- P1: tests + `plugin/migration.go:244` — fix tests and RegisterPluginTables
- P2: `plugin/tenant_db.go:57` — doc or fix
- P3: MySQL/MSSQL migrations 008, 009 — backfill or doc gap
