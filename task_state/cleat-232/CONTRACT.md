# CONTRACT: cleat-232 — Multi-DB Test Fixes

## Deliverables

1. **MySQL CI green**: All MySQL tests pass locally and in CI
2. **MSSQL CI green**: All MSSQL tests pass locally and in CI
3. **Dialect-conditional admin schema**: `admin.` prefix only applied for PostgreSQL
4. **Migration verification**: All migrations verified on all three backends
5. **`::jsonb` cast audit**: Verify MySQL/MSSQL handle the result column correctly without PostgreSQL-specific casts

## Invariants

- PostgreSQL continues to work correctly — no regressions
- No schema changes that break existing Postgres deployments
- All three backends can run the same migrations (dialect-conditional DDL only)

## API Surface

| File | Change |
|------|--------|
| `auth/tenant_store.go:29` | `admin.tenant_api_keys` → dialect-conditional |
| `auth/tenant_store.go:39` | `admin.tenant_api_keys` → dialect-conditional |
| `auth/tenant_store.go:47` | `admin.tenant_api_keys` → dialect-conditional |
| `engine/db.go` | Review `::jsonb` cast for MySQL/MSSQL compatibility |

## Test Requirements

- `go test ./auth/...` passes on Postgres, MySQL, MSSQL
- `go test ./engine/...` passes on Postgres, MySQL, MSSQL
- Migration tests pass on all three backends
- No new test failures introduced on any backend

## Integration Points

- PostgreSQL remains the primary backend — no changes to Postgres-specific features
- MySQL and MSSQL stores share the same interface as Postgres store
- CI workflow configs may need updating to run multi-db tests

## Coupling

- MEDIUM with `cleat-234` (cleat-234 consumes this task's green multi-db CI result)
- LOOSE with `cleat-235` (same engine/db.go file, different concerns)
- NONE with other leaf tasks
