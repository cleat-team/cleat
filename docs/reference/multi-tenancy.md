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

## Which dialects are multi-tenant at all

`tiers.yaml` (D1, cleared 2026-08-06 by #333) grants:

```yaml
tenancy:
  multi_tenant:  [postgres, mssql]
  single_tenant: [postgres, mysql, mssql]
```

**MySQL is single-tenant only.** That is a decision, not an omission, and it is
why `engine/plugindb_tenant.go`'s `beginTenantTx` returns `(nil, nil)` for any
dialect that is not PostgreSQL or SQL Server: there is no per-tenant plugin-DB
routing on MySQL because MySQL is not offered for multiple tenants. Running
untrusted tenants against a MySQL deployment is outside what this project
claims, and nothing in the code will stop you.

## What actually enforces the tenant boundary

**On every dialect, the load-bearing layer is the same: statement-level tenant
predicates in the Go SQL.** Every statement against a tenant-scoped table must
say which tenant is asking. This is not a convention — it is gated at authoring
time, per dialect:

| gate | dialect |
|---|---|
| `engine/mssql_tenant_predicate_test.go` | SQL Server |
| `engine/mysql_tenant_predicate_test.go` | MySQL |
| `engine/postgres_rls_reachability_test.go` | PostgreSQL |

Row-level security is a **backstop underneath that layer**, not the mechanism.
`engine/mssql_tenant_predicate_test.go` says why, in its own words: on SQL
Server `dbo.fn_tenant_filter` is off for any `dbo.cleat_admin` connection, "so
the per-query predicate is the whole of the isolation there."

## Row-Level Security: the real coverage, per dialect

Measured 2026-09-17 against live databases built by running `migrations/` to
head, read from the catalogs (`pg_class`, `pg_policies`,
`sys.security_predicates`) rather than from the SQL files. All three dialects
carry **21 tenant-bearing tables**.

| | PostgreSQL | SQL Server | MySQL |
|---|---|---|---|
| tenant-bearing tables | 21 | 21 | 21 |
| RLS / filter predicates | **17** (`ENABLE` + `FORCE`) | **14** | **0** |
| policies | 18 | 14 | — |
| **write-blocking predicates** | n/a (`FORCE` covers writes) | **0** | 0 |
| backstop active for an admin connection | no (`FORCE` applies to the owner) | **no** | — |

Three things that table is meant to make impossible to miss:

**SQL Server has no `BLOCK` predicates — none.** A `FILTER` predicate removes
other tenants' rows from reads. It does **not** stop a write from placing a row
outside the caller's tenant, or from moving one out of it. So on SQL Server the
backstop is read-side only. This is consistent with the threat model in
`SECURITY.md` — the database is a trusted component, and cleat's own SQL never
issues such a statement because the statement-level gate above forbids it — but
it means the backstop does not catch a write that the gate missed.

**PostgreSQL's 16 of 20 is deliberate, not partial.** The four without RLS are
the tenant registry itself — `admin.tenants`, `admin.tenant_api_keys`,
`admin.tenant_roles`, `admin.tenant_egress_allow` — which live in the `admin`
schema and describe tenants rather than belonging to one.

**MySQL's zero is a property of MySQL.** There is no row-level security feature
to use: `CREATE POLICY` is a syntax error on 8.4. Emulating it through a view
was measured at **6.1×** the cost of the table scan (`IMPROVEMENT-PLAN.d/1.7`),
which is why it was not done, and why MySQL is single-tenant only rather than
multi-tenant with a weaker backstop.

> **Corrected 2026-09-17.** This section previously said SQL Server bound its
> filter predicate "on the same seven multi-tenant tables PostgreSQL forces RLS
> on". Both halves of that have drifted: PostgreSQL now forces RLS on 16 tables
> and SQL Server filters 13, so the sets are neither seven nor the same. The
> numbers above are checked on every run by
> `engine/the_documented_tenant_coverage_is_measured_test.go`, so this table
> fails rather than rots.
>
> An earlier correction on 2026-08-09 is still accurate as far as it goes: RLS
> is not PostgreSQL-only, SQL Server has a real `SECURITY POLICY`. What it did
> not say is that the SQL Server policy is filter-only and off for the admin
> role, which is the part that decides how much weight it can carry.
