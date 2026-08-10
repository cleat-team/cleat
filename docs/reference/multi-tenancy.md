# Multi-Tenancy in Cleat

## Tenant Resolution Modes

Cleat supports three tenant resolution modes via the `--tenant-resolver` flag:

1. **`single-tenant`** (default): All requests use the zero UUID. No per-tenant isolation.

2. **`header:<name>`**: The tenant UUID is extracted from the named HTTP header.
   Example: `--tenant-resolver=header:X-Tenant-ID`

3. **`api-key`**: The tenant is derived from the API key via `Authorization: Bearer <key>`.
   The key is SHA-256 hashed and looked up in `tenant_api_keys`.

## Tenant Propagation

1. HTTP request arrives → auth middleware resolves tenant → stored in request context
2. Workflow started → tenant stored in `workflow_instances.tenant_id`
3. Workflow executes → engine injects tenant into `plugin.CallContext`
4. Plugin host functions → extract tenant via `plugin.CallContextFromContext(ctx)`

## Row-Level Security

> Corrected 2026-08-09: this previously said RLS was PostgreSQL-only and that
> MySQL and SQL Server fell back to application-level filtering. That was
> wrong for SQL Server. `tiers.yaml`'s D1 decision (2026-08-06) grants
> `multi_tenant: [postgres, mssql]`. Verified via
> `migrations/mssql/012_admin_role.sql:110-121`, which binds
> `dbo.fn_tenant_filter` as a real `SECURITY POLICY` / `FILTER PREDICATE`,
> checked against `SESSION_CONTEXT('tenant_id')`, on the same seven
> multi-tenant tables PostgreSQL forces RLS on.

PostgreSQL and SQL Server deployments use database-enforced row-level
security: PostgreSQL via `CREATE POLICY` + `FORCE ROW LEVEL SECURITY` scoped
by a `tenant_id` session variable, SQL Server via a native `SECURITY POLICY`
bound to a `FILTER PREDICATE` function checked against
`SESSION_CONTEXT('tenant_id')`. MySQL has no row-level security feature at
all (`CREATE POLICY` is a syntax error on 8.4); it is documented
single-tenant-only rather than emulating isolation at 6.1x the cost of a full
scan with no database backstop behind a missed filter -- see
`IMPROVEMENT-PLAN.md` §1.7 for the measurement.
