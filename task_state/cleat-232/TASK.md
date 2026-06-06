# cleat-232: Multi-DB Test Fixes

**Budget:** $20 (~2 days)
**Priority:** 1 (release blocker)
**Status:** pending
**Depends on:** none

## Scope

MySQL and MSSQL CI jobs are failing. Root cause identified: admin schema qualifier (`admin.tenant_api_keys`) in `auth/tenant_store.go` broke MySQL/MSSQL which don't have the `admin` schema.

## Actions

1. Run the full multi-db test suite locally for MySQL and MSSQL (Docker containers)
2. Fix schema references: `admin.` prefix should be conditional on dialect (Postgres only)
3. Fix any other dialect-specific SQL issues
4. Verify all migrations pass on all three backends
5. Target: green multi-db CI

## Key Files

- `auth/tenant_store.go` — admin schema prefix (lines 29, 39, 47)
- `engine/db.go` — `::jsonb` cast is PostgreSQL-only (from commit 98e32dd)
- `engine/mysql_store.go`
- `engine/mssql_store.go`
- Migrations

## Additional Scope (from surveys)

- Note `auth/` is at root, not `internal/auth/`
- Check `::jsonb` cast in `engine/db.go` — MySQL/MSSQL stores need different handling or no cast
- The `workflowID` fix in `engine/engine.go` (commit 98e32dd) is dialect-agnostic — good
