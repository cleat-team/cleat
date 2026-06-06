# cleat-232-tenant-storei — Exploration 13th Pass (Independent Verification)

**Date:** 2026-06-06
**Agent:** explorer agent (13th pass — cleat-232-tenant-storei)
**HEAD:** 010c2ed

## Summary

STATUS.md is **accurate**. The 12th pass log was wrong when it claimed source files were "UNCHANGED, BROKEN" — the implementation described in STATUS.md is actually present in the source. All P0/P1 fixes are in place and verified working. 31 tests pass, 7 fail (all public-path issue, unrelated to dialect work).

## Source Verification vs STATUS.md

### `auth/tenant_store.go` — MATCHES STATUS.md

- `TenantStore` struct has `dialect plugin.Dialect` field (line 19)
- `NewTenantStore(db, dialect)` takes dialect (line 23)
- `tableName()` helper: returns `admin.` prefix for PG, plain name otherwise (lines 29-33)
- `CreateTenant`: generates UUID in Go (`uuid.New()`, line 38), uses `plugin.Rebind()` (line 39)
- `CreateAPIKey`: uses `plugin.Rebind()` (line 54)
- `RevokeAPIKey`: uses `plugin.Rebind()` (line 64)

### `auth/fake_driver_test.go` — MATCHES STATUS.md

- Query matchers use substring matching: `strings.Contains(query, "tenants")`, not `"INSERT INTO tenants"` (lines 106, 108, 110)
- `execInsertTenant` accepts 3 args (tenant_id, name, display_name) and returns `driver.Result` (lines 144-171)
- Tenant lookup uses `"SELECT"` + `"tenant_api_keys"` substrings (line 91)

### `plugin/migration.go:238-242` — MATCHES STATUS.md

- `registerPluginTableQuery` has Default (PG), MySQL, MSSQL variants (lines 238-242)
- `RegisterPluginTables` uses `query.For(dialect)` (line 248)

### `auth/middleware.go` — WORKING TREE DIFF, STATUS.md CORRECT

- Lines 46-49: `"/"`, `"/index.html"`, `"/assets/"`, `"/favicon.ico"` are public paths
- This is uncommitted — `git diff` shows it as working tree change
- Breaks 7 middleware tests that use `"/"` as their request path

## Test Results

Ran `go test ./auth/... -v -count=1`:

```
31 PASS, 7 FAIL
```

### All P0/P1 fixes VERIFIED (24 pass, 0 fail in tenant store + context + hash + extract + generate):

- 12/12 TenantStore tests PASS (CreateTenant, CreateTenant_ReturnsUUID, DuplicateName, MultipleTenants, CreateAPIKey, CreateAPIKey_DifferentKeys, RevokeAPIKey, NotFoundIsNotError, DoubleRevokeIsIdempotent, RevokedKeyCannotAuthenticate, NewTenantStore, GenerateAPIKey_UsesCryptoRand)
- 3/3 TenantFromAPIKey tests PASS
- 2/2 SHA256Hash tests PASS
- 7/7 ExtractAPIKey tests PASS
- 2/2 WithTenantID tests PASS
- 2/2 TenantIDFromContext tests PASS
- 2/2 GenerateAPIKey tests PASS

### 7 failures — ALL public path (`"/"` is now public, bypasses auth):

| Test | Expected | Actual | Root Cause |
|------|----------|--------|------------|
| TestMiddleware_BearerToken_Valid_SetsTenantContext | tenant in context | no tenant | `"/"` bypasses auth |
| TestMiddleware_XCleatAPIKey_Valid_SetsTenantContext | tenant in context | no tenant | same |
| TestMiddleware_InvalidToken_ReturnsUnauthorized | 401 | 200 | same |
| TestMiddleware_RevokedToken_ReturnsUnauthorized | 401 | 200 | same |
| TestMiddleware_BearerTakesPriorityOverXCleatKey | 200 | 403 | same (no tenant → inner MW rejects) |
| TestMiddleware_TenantIDPropagation | 200 | 403 | same |
| TestStoreAndMiddleware_EndToEnd | tenant in context | no tenant | same |

### 3 middleware tests that happen to PASS (but should also be path-changed):

- `TestMiddleware_NoAuth_PassesThrough` — `"/"` with no auth → 200. Passes, would still pass with non-public path (requireAuth=false).
- `TestMiddleware_MalformedAuthHeader_NotBearer` — `"/"` with Basic auth → 200. Passes, but path should also change.
- `TestMiddleware_MalformedAuthHeader_OnlyBearerKeyword` — `"/"` with "Bearer" only → 200. Passes, but path should also change.

### Fix for middleware tests

Change all test request paths from `"/"` to a non-public path like `"/api/test"`. This is a one-line change per test — 10 total in `middleware_test.go` and `tenant_store_test.go`. The 3 currently-passing tests will still pass with a non-public path because they use `requireAuth=false` (the middleware proceeds without tenant when no auth is present).

## Remaining Items

| Priority | Item | Status | Verified |
|----------|------|--------|----------|
| P0 | `auth/tenant_store.go` dialect awareness | DONE | 12/12 tests pass |
| P1 | `auth/fake_driver_test.go` matchers | DONE | Verified working |
| P1 | `plugin/migration.go` dialect INSERT | DONE | Verified in source |
| P1 | Fix middleware test paths (`"/"` → non-public) | **NOT STARTED** | 7 tests fail, fix is 10 one-line path changes |
| P1 | Add MySQL/MSSQL test variants for tenant store | **NOT STARTED** | Tests only use `plugin.DialectPostgres` |
| P2 | `plugin/tenant_db.go:57` PG-only hardcoding | **NOT ADDRESSED** | `admin.tenant_roles`, `$1`, `sql.Open("postgres",...)` — by design (tenant roles are PG-only) |
| P3 | MySQL missing migrations 008, 009 | **NOT STARTED** | MySQL: 7/9 migrations present |
| P3 | MSSQL missing migration 009 | **NOT STARTED** | MSSQL: 8/9 migrations present |
| P3 | `::jsonb` cast audit | DONE | All safe (PG-only code paths) |

## Callers Verified

- `cmd/cleat-worker/main.go:177`: `auth.NewTenantStore(gdb, driverToDialect(*driver))` — OK
- `cmd/cleat-worker/main.go:833`: `auth.NewTenantStore(db, driverToDialect(*driver))` — OK
- All 14 test callers use `plugin.DialectPostgres` — OK

## Assessment

STATUS.md is the authoritative source of truth. The 12th pass exploration log is stale — it was written without verifying the implementation was already applied. The P0/P1 core work (dialect awareness in tenant_store.go, fake_driver_test.go, migration.go) is complete and all tenant store tests pass.

The 7 remaining test failures are trivial to fix: change test paths from `"/"` to a non-public path. This is strictly a test-only change.

The P2/P3 items are genuine gaps but are either by-design (tenant_db.go tenant roles are PG-only) or low-priority (migration backfills for MySQL/MSSQL).
