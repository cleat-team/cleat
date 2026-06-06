# cleat-232 Exploration Log — 12th Pass (Independent Verification)

**Date:** 2026-06-06
**Agent:** explorer agent (12th pass — cleat-232-tenant-storep)
**HEAD:** 010c2ed
**Working tree:** 38 modified files (unchanged from 11th pass)

## Test Execution Results

Ran `go test ./auth/... -v -count=1` — **16 FAIL, 13 PASS**. This confirms the STATUS.md findings are still live.

### Failure Categories

**Category A: Fake driver `admin.` prefix mismatch (12 failures)**

Every `TenantStore` method test fails because the fake driver's `QueryContext`/`ExecContext` matchers use unprefixed table names but the real queries now use `admin.` prefix:

| Test | Error Query |
|------|-------------|
| TestTenantStore_CreateTenant | `INSERT INTO admin.tenants (...) VALUES ($1, $2) RETURNING tenant_id` |
| TestTenantStore_CreateTenant_ReturnsUUID | (same) |
| TestTenantStore_CreateTenant_DuplicateName | (same) |
| TestTenantStore_CreateTenant_MultipleTenants | (same) |
| TestTenantStore_CreateAPIKey | (same) |
| TestTenantStore_CreateAPIKey_DifferentKeys | (same) |
| TestTenantStore_RevokeAPIKey | (same) |
| TestTenantStore_RevokeAPIKey_DoubleRevokeIsIdempotent | (same) |
| TestTenantStore_RevokeAPIKey_NotFoundIsNotError | `UPDATE admin.tenant_api_keys SET revoked_at = now() WHERE key_id = $1` |
| TestTenantStore_RevokeAPIKey_RevokedKeyCannotAuthenticate | (same) |
| TestNewTenantStore | (same as CreateTenant) |
| TestStoreAndMiddleware_EndToEnd | (same as CreateTenant) |

**Category B: Public path bypass (4 failures)**

`auth/middleware.go` working tree diff adds `"/"` as a public path. Tests that send requests to `"/"` and expect auth behavior now get the request passed through without auth:

| Test | Expected | Actual |
|------|----------|--------|
| TestMiddleware_BearerToken_Valid_SetsTenantContext | tenant in context | no tenant |
| TestMiddleware_XCleatAPIKey_Valid_SetsTenantContext | tenant in context | no tenant |
| TestMiddleware_InvalidToken_ReturnsUnauthorized | 401 | 200 (passes through) |
| TestMiddleware_RevokedToken_ReturnsUnauthorized | 401 | 200 (passes through) |
| TestMiddleware_BearerTakesPriorityOverXCleatKey | 200 | 403 (no tenant → inner MW rejects) |
| TestMiddleware_TenantIDPropagation | 200 | 403 (no tenant → inner MW rejects) |

Note: `TestMiddleware_NoAuth_PassesThrough` **passes** because `"/"` is now a public path — it was likely *intended* to verify no-auth behavior, but now succeeds for the wrong reason.

### Passes

13 tests pass: all `TestExtractAPIKey_*` (6), `TestSHA256Hash_*` (2), `TestWithTenantID*` (2), `TestTenantIDFromContext_*` (2), `TestGenerateAPIKey_*` (2), `TestMiddleware_MalformedAuthHeader_*` (2), `TestTenantFromAPIKey_NotFound`, `TestTenantFromAPIKey_NilDB`. These either don't touch the DB or expect an error path that the fake driver happens to satisfy.

## Source File Verification (re-confirmed)

### `auth/tenant_store.go` — UNCHANGED, BROKEN

```
Line 29: INSERT INTO admin.tenants (name, display_name) VALUES ($1, $2) RETURNING tenant_id
Line 39: INSERT INTO admin.tenant_api_keys (tenant_id, key_hash, description) VALUES ($1, $2, $3)
Line 47: UPDATE admin.tenant_api_keys SET revoked_at = now() WHERE key_id = $1 AND revoked_at IS NULL
```

All three use `admin.` schema + `$N` placeholders. `NewTenantStore` takes only `*sql.DB` — no dialect parameter. Zero dialect awareness.

### `auth/fake_driver_test.go` — UNCHANGED, BROKEN

All 4 matchers match WITHOUT `admin.` prefix:
```
Line 94:  strings.Contains(query, "SELECT tenant_id FROM tenant_api_keys")
Line 98:  strings.Contains(query, "INSERT INTO tenants")
Line 113: strings.Contains(query, "INSERT INTO tenant_api_keys")
Line 115: strings.Contains(query, "UPDATE tenant_api_keys SET revoked_at")
```

### `auth/middleware.go` — WORKING TREE DIFF (not yet committed)

Adds `"/"`, `"/index.html"`, `"/assets/*"`, `"/favicon.ico"` as public paths. This breaks middleware tests that use `"/"` as the test path.

### `plugin/migration.go:244` — UNCHANGED, PG-ONLY

```go
INSERT INTO admin.plugin_tables (plugin_name, table_name) VALUES ($1, $2) ON CONFLICT DO NOTHING
```

### `plugin/tenant_db.go:57,69` — UNCHANGED, PG-ONLY

```go
SELECT role_name, password FROM admin.tenant_roles WHERE tenant_id = $1
sql.Open("postgres", tenantDSN)
```

### Engine Store Architecture — CORRECT, VERIFIED

- PostgresStore (`db.go:1554`): `SELECT tenant_id FROM admin.tenant_api_keys WHERE key_hash = $1`
- MySQLStore (`mysql_store.go:1742`): `SELECT tenant_id FROM tenant_api_keys WHERE key_hash = ?`
- MSSQLStore (`mssql_store.go:1626`): `SELECT tenant_id FROM tenant_api_keys WHERE key_hash = @p1`

### `::jsonb` Cast Audit — ALL SAFE

All `::jsonb` uses are in PG-only code paths (`PostgresStore`, `cleatctl` restore, `cleat` deploy). Verified.

### Migration Gaps — CONFIRMED

| Backend | Present | Missing |
|---------|---------|---------|
| PostgreSQL | 001-009 (9/9) | — |
| MySQL | 001-003, 005-007 (7/9) | 008_rls_fail_closed, 009_generation |
| MSSQL | 001-008 (8/9) | 009_generation |

## Conclusion

All findings from passes 1-11 are confirmed with live test execution. No changes have been applied. The fix plan from STATUS.md remains correct:

- **P0:** `auth/tenant_store.go` — add dialect awareness (accept Dialect param, generate correct SQL per backend)
- **P1:** `auth/fake_driver_test.go` — fix matchers to include `admin.` prefix (or make dialect-aware)
- **P1:** `auth/middleware.go` — resolve public path vs test path conflict (tests use `"/"` which is now public)
- **P1:** `plugin/migration.go:244` — make `RegisterPluginTables` dialect-aware
- **P2:** `plugin/tenant_db.go:57` — document or fix (tenant roles are PG-specific feature)
- **P3:** MySQL/MSSQL migrations 008, 009 — backfill or document gap
