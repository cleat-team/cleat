# cleat-232 Status

**Phase:** in_progress
**Last updated:** 2026-06-06 (verified by explorer agent — 13th pass)
**HEAD:** 010c2ed
**Explored by:** explorer agent (13 passes)
**Implemented by:** cleat-232-tenant-storer agent
**Dispatched by:** cto-lap-032

## Implementation Summary

### P0: `auth/tenant_store.go` — FIXED (VERIFIED)

- `TenantStore` struct has `dialect plugin.Dialect` field (line 19)
- `NewTenantStore(db, dialect)` requires a dialect parameter (line 23)
- `tableName()` helper: returns `admin.<table>` for PostgreSQL, plain `<table>` for MySQL/MSSQL (lines 29-34)
- `CreateTenant` generates UUID in Go (`uuid.New()`) — no RETURNING dependency (line 38)
- `CreateAPIKey` and `RevokeAPIKey` use `plugin.Rebind()` for placeholder translation (lines 54, 64)
- **Callers verified:**
  - `cmd/cleat-worker/main.go:177` — `auth.NewTenantStore(gdb, driverToDialect(*driver))`
  - `cmd/cleat-worker/main.go:833` — `auth.NewTenantStore(db, driverToDialect(*driver))`
  - `driverToDialect()` at line 2527 maps driver strings to `plugin.Dialect`
  - All tests pass `plugin.DialectPostgres`

### P1: `auth/fake_driver_test.go` — FIXED (VERIFIED)

- Query matchers updated to use substring matching (lines 106, 108, 110): match on `"tenants"`, `"tenant_api_keys"` not full `"INSERT INTO tenants"`
- `execInsertTenant` in `ExecContext` (line 144), accepts 3 args (`tenant_id`, `name`, `display_name`) — no RETURNING
- `queryTenantLookup` handles `SELECT tenant_id FROM tenant_api_keys WHERE key_hash = ...` (line 124)

### P1: `plugin/migration.go:238-256` — FIXED (VERIFIED)

- `registerPluginTableQuery` var (line 238) has dialect-specific SQL:
  - Postgres: `INSERT INTO admin.plugin_tables ... ON CONFLICT DO NOTHING`
  - MySQL: `INSERT IGNORE INTO plugin_tables ...`
  - MSSQL: `IF NOT EXISTS (...) INSERT INTO plugin_tables ...`
- `RegisterPluginTables(ctx, db, dialect, pluginName, tableNames)` at line 247 accepts Dialect parameter

## Test Results (2026-06-06, verified live)

```
auth package: 33 PASS, 7 FAIL (all 7 failures from "/" as public path)
  TenantStore tests:      11/11 PASS
  NewTenantStore test:    1/1 PASS
  GenerateAPIKey tests:   3/3 PASS
  ExtractAPIKey tests:    7/7 PASS
  SHA256Hash tests:       2/2 PASS
  WithTenantID tests:     2/2 PASS
  TenantIDFromContext:    2/2 PASS
  TenantFromAPIKey tests: 3/3 PASS
  Middleware tests:       3/9 PASS (6 fail: 5 public path + 1 EndToEnd)
```

### Failure Breakdown

All 7 failures caused by `auth/middleware.go:47` which adds `"/"` as a public path:

| Test | Request Path | Expected | Actual |
|------|-------------|----------|--------|
| TestMiddleware_BearerToken_Valid_SetsTenantContext | `"/"` | tenant in context | bypassed (no tenant) |
| TestMiddleware_XCleatAPIKey_Valid_SetsTenantContext | `"/"` | tenant in context | bypassed (no tenant) |
| TestMiddleware_InvalidToken_ReturnsUnauthorized | `"/"` | 401 | 200 (passes through) |
| TestMiddleware_RevokedToken_ReturnsUnauthorized | `"/"` | 401 | 200 (passes through) |
| TestMiddleware_BearerTakesPriorityOverXCleatKey | `"/"` | 200 | 403 (no tenant) |
| TestMiddleware_TenantIDPropagation | `"/"` | 200 | 403 (no tenant) |
| TestStoreAndMiddleware_EndToEnd | `"/"` | tenant in context | bypassed (no tenant) |

**Fix:** Change test request paths from `"/"` to a non-public path like `"/api/test"`. Simple, one-line change per test in `middleware_test.go` and `tenant_store_test.go`.

Passing middleware tests that also use `"/"`:
- `TestMiddleware_NoAuth_PassesThrough` — passes because it tests no-auth behavior, correctly verified against the now-public `"/"`
- `TestMiddleware_MalformedAuthHeader_NotBearer` — passes because Basic auth is ignored, then `"/"` is public → 200
- `TestMiddleware_MalformedAuthHeader_OnlyBearerKeyword` — passes because empty extracted key + `"/"` public → 200

## Still Needed

| Priority | Item | Status |
|----------|------|--------|
| P1 | Fix middleware test paths (`"/"` → `"/api/test"`) | Not started |
| P1 | Add MySQL/MSSQL test variants for tenant store | Not started |
| P2 | `plugin/tenant_db.go:57` — inherently PG-only (tenant roles, per-tenant pools) | Documented, no fix needed for multi-db |
| P3 | MySQL missing migrations 008, 009 | Not started |
| P3 | MSSQL missing migration 009 | Not started |
| P3 | `::jsonb` cast audit | Verified PG-only — all safe |

## Migration Status

| Backend | Migrations Present | Missing |
|---------|-------------------|---------|
| PostgreSQL | 001-009 (9/9) | — |
| MySQL | 001-003, 005-007 (6/9) | 008_rls_fail_closed, 009_generation |
| MSSQL | 001-008 (8/9) | 009_generation |

## Architecture Verification

- Engine stores (`db.go`, `mysql_store.go`, `mssql_store.go`) already use dialect-specific SQL for tenant lookups — verified correct
- All `::jsonb` casts are in PostgreSQL-only code paths — verified safe
- `NewTenantStore` callers all pass dialect — verified (2 callers in `cmd/cleat-worker/main.go`, 14 callers in tests)
