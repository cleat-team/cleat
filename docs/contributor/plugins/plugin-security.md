# Plugin Security Guide for Operators

This guide covers the operational security procedures for running plugins in a
cleat deployment. It builds on the [plugin security model](../plans/plugin-security-model.md)
design document and provides day-to-day operational guidance.

## Table of Contents

1. [How tenant isolation works](#1-how-tenant-isolation-works)
2. [How to configure capability limits](#2-how-to-configure-capability-limits)
3. [How to audit installed plugins](#3-how-to-audit-installed-plugins)
4. [How to inspect a plugin before installing](#4-how-to-inspect-a-plugin-before-installing)
5. [How to approve plugin upgrades](#5-how-to-approve-plugin-upgrades)
6. [How to run a private plugin index](#6-how-to-run-a-private-plugin-index)
7. [Incident response](#7-incident-response)
8. [Connection limits and resource isolation](#8-connection-limits-and-resource-isolation)
9. [Monitoring](#9-monitoring)

---

## 1. How tenant isolation works

### What it protects against: two trust surfaces, not one

Almost every design decision in this section turns on a distinction that is easy
to collapse, and collapsing it produces the wrong answer in a predictable
direction — reaching for a stricter mechanism to defend against something that
is not in the model.

**Workflow code is untrusted.** It is WASM, sandboxed, with no database handle
and no way to issue SQL. It reaches a plugin only through a host call, where
`engine.pluginCallContext` bridges the workflow's own tenant before the plugin
function runs. A workflow cannot set a session variable, cannot open a
connection, and cannot name a tenant other than its own.

**Plugin code is trusted.** It runs in-process, holds a `*sql.DB`, and can issue
any statement it likes. Plugins are closer to device drivers than to user
programs: they can wreck things, and the deployment relies on them not to.
Installing one is a deliberate act by an operator, which is what sections 3 to 6
of this guide are about.

So the row-level policies on plugin tables **are not a wall against the plugin**.
A plugin that wanted another tenant's rows does not need to defeat a policy; it
can simply issue the query. The policies exist to catch the plugin's
**mistakes** — a forgotten `WHERE tenant_id = $1`, a background sweep that runs
on a context carrying no tenant — which is the failure cleat#1277 was filed
about and whose miss rate does not improve on its own.

#### What follows from this, concretely

Two mechanisms can lift the tenant scoping for a legitimate cross-tenant sweep:
a **granted database role**, which the application cannot give itself, or a
**session variable**, which it can. The first is strictly stronger against an
application that has been taken over, and PostgreSQL uses it
(`SET LOCAL ROLE cleat_sweep`).

SQL Server has no `SET ROLE` — database role membership is a property of the
connection — so cleat uses a second `SESSION_CONTEXT` key there instead. That is
a real asymmetry, and it is **acceptable rather than regrettable** for the reason
above: against trusted code that already holds the connection, the role's one
advantage does not exist. What it buys against a *mistake* is identical, because
`plugin.AcrossAllTenants` is an explicit call that refuses an empty reason and
shows up in a diff.

The risk that remains is not that the bypass can be taken. It is that it is
**convenient** — one line, and the obvious reach when a fail-closed policy makes
a sweep return nothing, which is exactly what a correct policy looks like from
the inside when you forgot to thread the tenant through. That is why every
cross-tenant bypass in the tree is declared in a ledger with a typed reason
(`plugin/a_cross_tenant_bypass_is_declared_test.go`, cleat#1623), and why a
statement naming a tenant-scoped table on a bare context fails at authoring time
rather than at three in the morning (cleat#1552 step 3).

### Where the tables are

```
Database: cleat
├── public.*              — core tables (workflow_instances, event_history, ...)
│                           Owned by cleat_owner. RLS filters by tenant_id.
│                           PLUGIN TABLES ARE ALSO HERE.
│
├── tenant_a1b2c3.*       — tenant X's schema, created by admin.create_tenant
├── tenant_d4e5f6.*       — tenant Y's schema
│
└── admin.*               — admin tables (cleat_owner only)
```

**Plugin tables live in the same schema as the core tables, not in a per-tenant
one.** This section described the opposite until cleat#1279; the per-tenant
placement was designed and never wired up, and the documentation was written
as though it had been.

What is true: `admin.create_tenant` does create a `tenant_<uuid>` schema and a
login role, and `admin.grant_plugin_to_tenant` exists to `GRANT` plugin tables
to that role. But it reads `admin.plugin_tables`, which nothing populates --
`plugin.RegisterPluginTables` is the only writer and **has no production
caller** (it has a full unit-test suite, so grepping the name finds plenty of
hits; grep for calls outside `_test.go` files). So the grant loop is always
zero-iteration, and no plugin migration issues `CREATE SCHEMA` anywhere.

Plugin migrations pin `search_path = public` unconditionally
(`plugin/migration.go`), so a plugin table lands in `public` even when the
worker runs with a non-default `--schema`. That has its own consequence,
tracked as cleat#1287.

### What actually isolates a plugin's rows

Every plugin table carries a `tenant_id` column. Two things scope it:

1. **The plugin's own `WHERE tenant_id = $1`**, hand-written at each query
   site. This is the only protection for most plugin tables.
2. **A row-level security policy**, for tables that declare `TenantScoped` on
   their migration (cleat#1280). The runtime emits `ENABLE`/`FORCE ROW LEVEL
   SECURITY` and a policy filtering on `cleat.assert_tenant_set()`, and the
   plugin database adapter supplies that value transaction-locally whenever
   the request context carries a tenant.

**Only one table has a policy today** (`kv_store`). Re-derive rather than
trusting this sentence:

```bash
grep -rn 'TenantScoped: \[\]string' plugins/*/migrations.go
```

The rest are scoped by the Go predicate and nothing else, and a forgotten
predicate returns another tenant's rows with no error. Why the remaining
plugins cannot simply adopt a policy is cleat#1278: a tenant reaches a plugin
only on the HTTP path, so for a plugin with a cross-tenant background sweep,
"add a fail-closed policy" and "silently empty the sweep" are the same change.

`TenantScoped` installs a policy on **PostgreSQL and SQL Server**. On MySQL it
is accepted and does nothing: MySQL has no row-level security, and a plugin
table is scoped there by the Go predicate alone.

Until cleat#1552 this paragraph said the field was PostgreSQL-only, and gave
one reason for both other dialects. MySQL has no row-level security and never
will have anything to install. SQL Server does, and the stated obstacle — that
it "binds a tenant to a whole connection pool, which a per-request tenant does
not fit" — was wrong: plugins are not handed a connector-scoped pool
(`getPluginDB` gives them the main or plugin pool), and `sp_set_session_context`
is cleared when `database/sql` recycles a connection.

Two differences from the PostgreSQL arm are worth knowing, both measured:

* **A read with no tenant returns an empty table rather than raising.**
  PostgreSQL's `cleat.assert_tenant_set()` raises; a SQL Server filter predicate
  must be an inline table-valued function, which has no body to raise from.
* **Writes are covered by `BLOCK` predicates, not by the filter.** A SQL Server
  `FILTER PREDICATE` hides rows from reads and does not refuse writes at all, so
  the policy carries `ADD BLOCK PREDICATE … AFTER INSERT` and `AFTER UPDATE`
  as well. PostgreSQL needs no equivalent: `FOR ALL … USING` defaults its
  `WITH CHECK` to the `USING` expression.

* **A `DELETE` is a read for the filter predicate's purposes**, which is the
  half that costs an operator. The predicate hides rows from `DELETE` exactly as
  it hides them from `SELECT`, so a statement issued with no tenant key removes
  nothing and reports `(0 rows affected)` -- the one outcome indistinguishable
  from "already clean". Measured as `sa` with `IS_SRVROLEMEMBER('sysadmin') = 1`:
  privilege is not what gets you past a security policy on this dialect. The
  `BLOCK` predicates do not help, because they refuse a write naming the *wrong*
  tenant and this one names the right one on a connection that has not said who
  it is.

### The same BLOCK predicates now cover the core tables too

Until cleat#2205 (migration `103_a_filtered_write_is_a_blocked_write.sql`,
2026-09-24) the asymmetry above was worse on the **core** `dbo.*` tables than on
plugin ones: `dbo.fn_tenant_filter` carried a `FILTER PREDICATE` only, so an
`INSERT` or `UPDATE` on `workflow_instances`, `workflow_defs`, and the other
core tenant-scoped tables could stamp or move a row into the *wrong* tenant
with no refusal at all — plugin tables had had `BLOCK` predicates on
`fn_plugin_tenant_filter` since cleat#1552, and core tables did not. 103 closes
that gap the same way: `AFTER INSERT`, `AFTER UPDATE` and `BEFORE UPDATE` on
`fn_tenant_filter`, derived live from `sys.security_predicates` against every
table that already carries the `FILTER` predicate, so a table a later
migration adds is covered automatically with no edit to 103 itself.

**This changes what a migration is allowed to do.** Before 103, a migration
connecting as a plain login with no `SESSION_CONTEXT` set could freely
`INSERT`/`UPDATE` a `tenant_id`-bearing row on a core table — SQL Server does
not exempt `sa` or `sysadmin` from RLS the way PostgreSQL exempts a superuser
or table owner, but there was simply nothing to refuse it. There is now: with
no session context, `SESSION_CONTEXT(N'tenant_id')` is `NULL`, and
`@tenant_id = NULL` is never true, so `BLOCK` refuses the write outright.

**If your migration or backfill writes a `tenant_id`-bearing row to a
core table that carries this policy, it must do one of:**

* disable the table's policy for the backfill's duration —
  `ALTER SECURITY POLICY dbo.<policy> WITH (STATE = OFF)`, the backfill, then
  `WITH (STATE = ON)` inside a `BEGIN TRY`/`BEGIN CATCH` that re-enables it on
  failure too. This turns off `FILTER` and `BLOCK` together, needs no optional
  migration installed, and is the **only** option available to a *shipped*
  migration, which cannot assume `cross_tenant_claim.sql` has been applied.
  `migrations/mssql/077_every_entity_records_when_it_last_changed.sql` and
  `078_two_entities_record_when_they_were_created.sql` already use exactly
  this pattern; or
* run as a login that is a member of the `cleat_admin` role, with
  `migrations/mssql/optional/cross_tenant_claim.sql`'s admin-bypass predicate
  form installed (it bypasses `BLOCK` exactly as it bypasses `FILTER`, since
  both share `fn_tenant_filter`) — available to an operator's own backfill
  script, not to a shipped migration; or
* call `EXEC sp_set_session_context @key=N'tenant_id', @value=<tenant>` on its
  own connection, per tenant, before the write — the same pattern
  `engine`'s tenant-scoped stores and tests already use, since
  `sp_set_session_context` is connection-scoped and does not survive a pooled
  connection's `sp_reset_connection`.

As of 2026-09-24, 077 and 078 are the only migrations after `002_defaults.sql`
that write to one of these tables, and both predate 103 — the `STATE = OFF/ON`
pattern they already use is exactly what the next one needs too.

Dropping a tenant works on both dialects. `admin.drop_tenant` on SQL Server is
`migrations/mssql/074_a_dropped_tenants_rows_go_with_it.sql`, and it finds
tenant-owned tables by asking `sys.columns` which ones carry a `tenant_id`
column rather than by reading a registry -- which reaches core and plugin tables
in one query, because on this dialect both live in `dbo`. PostgreSQL reads
`admin.plugin_tables` because `--schema` can put its plugin tables in a schema
the function would otherwise have to guess.

This paragraph said "still PostgreSQL-only: `admin.drop_tenant` reads
`admin.plugin_tables`, which SQL Server does not have". Both halves were wrong.
`admin.plugin_tables` has existed on SQL Server since
`migrations/mssql/001_schema.sql:117` -- in the pre-066 two-column shape, with
no producer -- and the dialect needed no registry to get tenant deletion.
cleat#1635.

### Which role a plugin runs as

**Plugin code does not use the per-tenant pools described below.** It gets the
main pool, or a dedicated plugin pool when one is configured
(`getPluginDB` / `getPluginReadOnlyDB` in `cmd/cleat-worker`), connecting as
whatever role the worker's DSN names. The per-tenant pools are used for
**workflow execution** (`cmd/cleat-worker/setup.go`), not for plugin queries.

This matters for what the core tables guarantee a plugin. A plugin reading
`workflow_instances` is filtered by that table's RLS policy only if its
connection is subject to RLS -- a superuser bypasses it unconditionally, and
the table's owner bypasses it unless the table is `FORCE`d. `engine.CheckRLSEnforced`
exists to detect exactly that, and the worker refuses to start on a bypassing
connection when `-rls-check` is left at its default.

### What `DatabaseAccessReadOnly` guarantees, and what it does not

A plugin declaring `DatabaseAccessReadOnly` is handed an `engine.ReadOnlyDB`
(`getPluginReadOnlyDB`). **That type is defence in depth, not a security
boundary on its own**, and the difference is dialect-dependent.

Three layers, which do not cover the same ground:

| layer | postgres | mysql | sql server |
|---|---|---|---|
| `Exec` refused in Go | yes | yes | yes |
| the **database** refuses a write inside the read transaction | yes | yes | **no** |
| the connection's own privileges | whatever you granted | whatever you granted | whatever you granted |

The second row is the one that matters, because the first does not cover
`Query`. `Exec` is the route a caller uses deliberately; `Query` takes any
statement string and has an ordinary reason to be handed
`INSERT ... RETURNING`. Every read now runs inside a transaction so that the
database can refuse it (cleat#1621) -- but **SQL Server has no read-only
transaction**: go-mssqldb rejects the option and T-SQL has no statement that
makes an open transaction read-only. There, layer 2 does not exist.

**So grant the privileges.** A plugin that must not write should be given a
connection whose database user has no `INSERT`, `UPDATE` or `DELETE` on the
tables it can reach:

```sql
-- PostgreSQL: a role for read-only plugins
CREATE ROLE cleat_plugin_ro LOGIN PASSWORD '...';
GRANT CONNECT ON DATABASE cleat TO cleat_plugin_ro;
GRANT USAGE ON SCHEMA public TO cleat_plugin_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO cleat_plugin_ro;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO cleat_plugin_ro;
```

```sql
-- SQL Server: the db_datareader role is exactly this, and is the ONLY
-- enforcement available there.
CREATE LOGIN cleat_plugin_ro WITH PASSWORD = '...';
CREATE USER cleat_plugin_ro FOR LOGIN cleat_plugin_ro;
ALTER ROLE db_datareader ADD MEMBER cleat_plugin_ro;
```

Point the plugin pool at that connection. This is the only guarantee that
holds on all three dialects, and on SQL Server it is the only one there is.

Note it interacts with the row above: a read-only connection is still subject
to the RLS question. Least privilege stops a plugin *writing*; it does not
stop it *reading another tenant's rows*, which is what `engine.CheckRLSEnforced`
and the tenant scoping are for.

### Per-tenant connection pools

The worker maintains a separate connection pool per tenant:

```go
type TenantPools struct {
    ownerDB *sql.DB  // cleat_owner — for claiming, migrations, admin
    pools   map[uuid.UUID]*sql.DB  // one pool per tenant
}
```

Each pool connects as the tenant's login role. Default pool settings:

| Setting | Default | Rationale |
|---------|---------|-----------|
| Max open connections | 5 | Prevents one tenant from exhausting server connections |
| Max idle connections | 2 | Keeps frequently-used connections warm |
| Connection max lifetime | 5 minutes | Ensures credentials are refreshed regularly |

### What this means for your database server

For N tenants, the cluster sees at most `N * 5` connections from the worker
(from the tenant pools) plus 1 connection from the owner pool. With 100 tenants,
that's at most 501 connections. Adjust `max_connections` in PostgreSQL
accordingly.

### Defence in depth, and where it is one layer rather than two

The RLS policies on the **core** `public.*` tables are real and active: even if
a tenant role reaches a table it should not, the policy filters by `tenant_id`.

For **plugin** tables the picture is thinner than this section used to claim,
and the claim was load-bearing — it listed "schema isolation" as a layer that
does not exist:

| Layer | Core tables | Plugin tables |
|---|---|---|
| Schema isolation | n/a — both live in `public` | **none**; there is no per-plugin or per-tenant schema |
| Row-level security | yes, on every tenant-scoped table | wherever `TenantScoped` is declared, on PostgreSQL and SQL Server |
| The query's own `WHERE tenant_id` | yes | yes, and for most plugin tables it is the **only** layer |
| No DDL on `public` | a tenant role does not own the schema | same |

So for a plugin table without a policy, a forgotten `WHERE tenant_id = $1`
returns another tenant's rows, and nothing below it will catch that. That is
the gap cleat#1277 opened and cleat#1278 tracks the remainder of.

> **This table said "one table today" until 2026-09-15**, which was true when it
> was written and wrong by more than an order of magnitude by the time anyone
> read it again: cleat#1512 scoped ten more, and the count has kept moving. The
> number is gone rather than updated, because a census of a growing population
> is guaranteed to go wrong and the only question is when. Ask instead:
>
> ```
> # which plugin tables declare a policy?
> grep -rhoE 'TenantScoped: \[\]string\{[^}]*\}' plugins/*/*.go |
>   grep -oE '"[a-z_]+"' | sort -u
> ```
>
> MySQL remains the permanent exception: it has no row-level security, so a
> declaration there is accepted and installs nothing.

### Tenant deletion

**Deleting a tenant does not delete its plugin data.** This section claimed the
opposite until cleat#1279, and the claim followed from the same wrong premise
as the schema diagram above: plugin tables are in `public`, so dropping the
tenant's schema does not reach them.

`admin.drop_tenant` drops the `tenant_<uuid>` schema and role and deletes from
the core tables **by name**. A dropped tenant's `kv_store`, `audit_events`,
`oauth_sessions`, `webhook_events` and `blob_index` rows survive in `public`
indefinitely. Re-derive which tables it does cover:

```bash
sed -n '/FUNCTION admin.drop_tenant/,/\$\$ LANGUAGE/p' \
  migrations/postgres/059_a_dropped_tenants_definitions_go_with_it.sql | grep 'DELETE FROM'
```

Tracked as cleat#1289. **Treat tenant deletion as incomplete for plugin data
and clean up explicitly** until that lands.

Two properties make the gap harder to notice than an ordinary missing
`DELETE`:

- A plugin table with a `TenantScoped` policy (cleat#1280) now holds rows
  behind a policy keyed on a tenant that no longer exists — so they are
  **unreadable and undeleted** rather than merely undeleted.
- A `DELETE` issued against such a table by a role the policy applies to
  removes nothing **and reports success**. Measured: with a different tenant
  set in the session, `DELETE FROM kv_store WHERE tenant_id = '<dropped>'`
  returns `DELETE 0` and commits, and the rows remain. With no tenant set it
  raises instead. So the careless path fails loudly and the careful path fails
  silently, and `DELETE 0` is also the correct result for "this tenant had no
  rows" — no row count can tell the two apart.

---

## 2. How to configure capability limits

### Default limits for community plugins

You configure capability limits in the cleat-worker config file:

```json
{
  "plugin_capability_limits": {
    "database": true,
    "start_workflow": false,
    "signal_workflow": true,
    "http_routes": false,
    "http_middleware": false,
    "background_worker": false,
    "call_plugin": []
  }
}
```

These limits apply to **all third-party (community) plugins** unless
overridden per-plugin. The defaults shown above are the built-in defaults.

### Per-plugin overrides

You can set different limits for specific plugins:

```json
{
  "plugin_capability_limits": {
    "default": {
      "database": true,
      "start_workflow": false,
      "signal_workflow": true,
      "call_plugin": []
    },
    "overrides": {
      "trusted-corp/my-plugin": {
        "start_workflow": true,
        "call_plugin": ["llm", "slacknotify"]
      },
      "example/pdf-generator": {
        "database": false,
        "call_plugin": ["llm"]
      }
    }
  }
}
```

### What happens when limits are exceeded

When `cleat plugin install` is run, the CLI validates the plugin's declared
capabilities against the configured limits:

```
$ cleat plugin install example/my-plugin@^1.0.0

ERROR: capability check failed:
  - start_workflow denied (not in limits)
  - call_plugin "slacknotify" denied (not in limits)

Installation refused. Contact your operator to adjust capability limits.
```

A plugin whose manifest declares capabilities exceeding the limits is **never
installed**. This is a hard enforcement point -- there is no force flag.

### Changing limits after installation

If you raise limits after installing, the plugin can use the new capabilities
immediately (no redeploy needed). If you lower limits, the plugin's behavior
during subsequent calls is constrained by the new limits; in-flight workflows
are not interrupted.

---

## 3. How to audit installed plugins

### List installed plugins

```
$ cleat plugin list

NAME                         VERSION         INSTALLED AT                   STATUS
-------------------------------------------------------------------------------------
llm                          0.1.0           2026-05-01T10:30:00Z           active
blobstore                    0.1.0           2026-05-01T10:30:00Z           active
example/hello-world          0.1.0           2026-05-01T10:30:00Z           active
acme/salesforce              1.2.0           2026-05-01T10:30:00Z           active
```

There is no `--verbose` flag, and `plugin list` reads only `name, version,
created_at, deprecated` from `plugin_defs` -- it has no per-plugin
capabilities detail view. For a plugin's declared capabilities, read its
manifest (`plugin validate` below reports whether the manifest itself is
well-formed, not its contents).

### List by tenant

To see which plugins a tenant uses:

```sql
SELECT plugin_name, plugin_version
FROM admin.tenant_plugins
WHERE tenant_id = '<uuid>';
```

### SQL queries for audit

You can query the `plugin_defs` table directly for advanced auditing:

```sql
-- All plugins installed in the last 7 days
SELECT name, version, created_at
FROM plugin_defs
WHERE created_at > now() - interval '7 days';

-- Active (non-deprecated) plugins sorted by install date
SELECT name, version, created_at
FROM plugin_defs
WHERE deprecated = false
ORDER BY created_at DESC;
```

---

## 4. How to inspect a plugin before installing

### Inspect from the index

There is no separate `inspect` subcommand. `plugin install --dry-run` fetches
the index entry, prints it, and stops before downloading or deploying
anything:

```
$ cleat plugin install --dry-run --yes example/hello-world@0.1.0

Plugin: example/hello-world
  Description: Greets a user by name
  Author: example-corp
  Repository: https://github.com/example/cleat-hello-world
  Version: 0.1.0
  Version description: Initial release
  Requires cleat >= 0.2.0

  SECURITY WARNING: This is a third-party plugin.
  Plugins have access to your database and infrastructure.
  Only install plugins from trusted sources.
  Review the source code and manifest before installing.

Dry run: no changes were made.
  Would download: https://github.com/example/cleat-hello-world/releases/download/v0.1.0/plugin.wasm
  Would verify checksum: abc123def456
  Would deploy to database (set --db or CLEAT_DATABASE_URL): example/hello-world v0.1.0
```

Neither host-function signatures nor a manifest's declared capabilities are
shown here -- the index carries none of that. To see them, download the
plugin's manifest yourself and run `plugin validate` on it (below).

### Inspect a local manifest

`plugin validate` checks the manifest is well-formed and prints nothing but
`valid` or the validation errors -- it does not echo back the manifest's
fields. Read the manifest file itself (a plain JSON file) for its
capabilities and host functions:

```
$ cleat plugin validate --manifest plugin.json

valid
```

### Manual checks before installing

For community plugins (not reviewed by the cleat project), you should:

1. **Review the source code**: Visit the plugin's `repository` URL and read
   the source. Pay attention to:
   - What external APIs the plugin calls
   - What database queries it executes
   - How it handles errors and edge cases
2. **Verify the author**: Check that the author listed in the manifest matches
   the repository owner.
3. **Check the release**: Verify the GitHub release includes the exact WASM
   binary and manifest files.
4. **Verify the checksum**: Download the WASM binary and compute its SHA-256
   hash independently:

   ```
   curl -LO https://github.com/example/cleat-hello-world/releases/download/v0.1.0/plugin.wasm
   sha256sum plugin.wasm
   ```

   Compare the result with the checksum in the index entry.

---

## 5. How to approve plugin upgrades

### The upgrade flow

There is no `cleat plugin update --show`, and `plugin update` never installs
anything -- it only reports whether a newer version exists. The command that
performs an upgrade is `cleat plugin install <name>@<version>`.

1. Check for an update:

   ```
   $ cleat plugin update example/hello-world

   example/hello-world: v0.1.0 -> v0.2.0 (update available)
   ```

   `checkSinglePluginUpdate` (`cmd/cleat/plugin_cmd.go`) always prints exactly one
   line for a given name, one of: `not installed`, `error querying: <err>`,
   `<version> (not found in index)`, `<old> -> <new> (update available)`, or
   `<version> (latest)`. There is no per-version listing, no install date, and no
   CHANGELOG link -- `--all` runs the same one-line check for every installed
   plugin instead of taking a name.

2. Install the new version:

   ```
   $ cleat plugin install example/hello-world@0.2.0

   Plugin: example/hello-world
     Description: Greets people by name
     Author: Example Org
     Version: 0.2.0

     SECURITY WARNING: This is a third-party plugin.
     Plugins have access to your database and infrastructure.
     Only install plugins from trusted sources.
     Review the source code and manifest before installing.

   Install this plugin? [y/N] y
   Downloading example/hello-world v0.2.0...
     Downloaded 4821 bytes
   Verifying checksum... OK
   Successfully installed example/hello-world v0.2.0
   ```

   There is no per-field diff against the installed version -- no checksum
   comparison, no capability list, no host-function delta. The security warning
   only prints for a plugin whose author is not official (`entry.IsOfficial()`);
   `--yes` skips the confirmation prompt, and `--dry-run` prints what would be
   downloaded/verified/deployed and stops before doing any of it.

3. What `cleat plugin install` actually does, in order (`runPluginInstall`,
   `cmd/cleat/plugin_cmd.go`):
   - Resolves `<name>[@<constraint>]` against the index and prints the info block
     above.
   - Returns immediately if the resolved version is bundled with `cleat-worker` --
     nothing to install.
   - Prints the security warning for a non-official plugin.
   - Prompts for confirmation, unless `--yes`.
   - On `--dry-run`, prints what it would do and stops.
   - Downloads the WASM binary and verifies its checksum (see below).
   - Deploys it via `DeployPlugin` (`engine/plugin_loader.go`). Versions are
     immutable (cleat#2135): a genuinely new version gets a new row and
     existing rows are untouched; reinstalling the same name and version is a
     no-op if the WASM bytes are byte-identical to what is already stored, and
     is refused -- naming both checksums -- if they differ. There is no
     override; publishing different code at an existing version requires a
     new version string.
   - Prints a one-line success message.

### Important: version pinning

In-flight workflows stay on the old plugin version. The worker routes each
workflow invocation to the plugin version the workflow was started with. This
means:

- Upgrading a plugin does not affect running workflows
- You can safely upgrade during production without concern for in-flight
  disruption
- Old versions remain in `plugin_defs` until all workflows referencing them
  complete. Versions are immutable (cleat#2135): reinstalling that exact name
  and version is a no-op if the bytes are unchanged, and is refused if they
  differ -- nothing can overwrite the row in place

### Checksum verification

On install, the CLI verifies the downloaded WASM binary against the checksum in
the index entry (`plugin.VerifyChecksum`, `plugin/index.go`). `DeployPlugin`
separately compares the downloaded bytes against whatever is already stored at
that `(name, version)`: reinstalling an unchanged version is a no-op, and
reinstalling with different bytes is refused (cleat#2135).

If the checksum does not match, the install is refused inline, with no separate
`ERROR:` banner:

```
Downloading example/hello-world v0.2.0...
  Downloaded 4821 bytes
Verifying checksum... failed: checksum mismatch: expected sha256:abc123..., got sha256:def456...
```

---

## 6. How to run a private plugin index

### Use case

Running a private plugin index allows you to distribute internal plugins within
your organization without publishing them to the public index.

### Setup

1. Create a private Git repository (GitHub, GitLab, Bitbucket, or any Git
   hosting service).

2. Create an `index.yaml` file at the root:

   ```yaml
   plugins:
     - name: internal/secret-sauce
       description: Internal business logic plugin
       author: my-company
       repository: https://git.internal/my-company/cleat-secret-sauce
       versions:
         - version: 0.1.0
           wasm_url: https://artifacts.internal/my-company/plugin.wasm
           manifest_url: https://artifacts.internal/my-company/plugin.json
           checksum: "sha256:abc123def456..."
           min_cleat_version: ">=1.0.0"

     - name: internal/audit-logger
       description: Audit logging for internal workflows
       author: my-company
       repository: https://git.internal/my-company/cleat-audit-logger
       versions:
         - version: 1.0.0
           wasm_url: https://artifacts.internal/my-company/audit-plugin.wasm
           checksum: "sha256:789abc..."
   ```

3. Host the WASM binaries on an internal artifact server (your CI/CD pipeline
   uploads them there). The `wasm_url` can be an `https://`, `s3://`, or any
   HTTP(S) URL accessible from the CLI.

4. Configure the CLI to use your private index:

   ```
   cleat config set plugin_index_url https://git.internal/my-company/cleat-private-index
   ```

   Or set the `CLEAT_PLUGIN_INDEX_URL` environment variable:

   ```
   export CLEAT_PLUGIN_INDEX_URL=https://git.internal/my-company/cleat-private-index/raw/main/index.yaml
   ```

### Using the public AND private index together

The CLI supports multiple index URLs. Run the public index for open-source
plugins and your private index for internal plugins:

```
cleat config set plugin_index_urls [
  "https://raw.githubusercontent.com/cleat-team/cleat-plugins/main/index.yaml",
  "https://git.internal/my-company/cleat-private-index/raw/main/index.yaml"
]
```

The CLI searches all configured indexes in order and returns the first match.

### CI/CD integration

Add plugin publishing to your CI/CD pipeline:

```yaml
# .github/workflows/publish-plugin.yml
jobs:
  publish:
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
      - run: make build
      - run: cleat plugin validate --manifest plugin.json
      - run: |
          CHECKSUM=$(sha256sum plugin.wasm | cut -d' ' -f1)
          # Upload WASM to artifact server
          aws s3 cp plugin.wasm s3://internal-artifacts/plugins/my-plugin/
          # Update index.yaml with new version and checksum
          sed -i "s/checksum: \"sha256:.*\"/checksum: \"sha256:$CHECKSUM\"/" index.yaml
      - uses: actions/upload-artifact@v4
        with:
          name: updated-index
          path: index.yaml
```

---

## 7. Incident response

### Scenario: A plugin is compromised

If you discover that an installed plugin has a vulnerability or is actively
malicious, follow these steps:

#### Step 1: Deprecate the compromised version

```
$ cleat plugin uninstall example/compromised 0.1.0

WARNING: This deprecates the plugin version. In-flight workflows will
continue to use this version until they complete.

Proceed? [y/N] y
✓ Plugin example/compromised v0.1.0 deprecated
```

This marks the version as `deprecated = true` in `plugin_defs`. New workflows
will not be scheduled against deprecated versions. In-flight workflows
continue to completion on the deprecated version (they depend on its behavior
for replay).

#### Step 2: Revoke capabilities (if urgent)

If the compromise is active and dangerous, immediately reduce the plugin's
capabilities to the minimum via the worker config:

```json
{
  "plugin_capability_limits": {
    "overrides": {
      "example/compromised": {
        "database": false,
        "start_workflow": false,
        "signal_workflow": false,
        "call_plugin": []
      }
    }
  }
}
```

Then restart the worker to apply the new limits. Any host function calls that
exceed the limits will fail at runtime.

#### Step 3: Investigate

1. Check the audit log for the tenant that had the plugin installed:

   ```sql
   -- Queries run by the plugin's tenant
   SELECT query, query_start, user
   FROM pg_stat_activity
   WHERE usename = 'cleat_tenant_<uuid>';
   ```

2. Check the worker logs for unusual error patterns from the plugin:

   ```
   journalctl -u cleat-worker | grep example/compromised
   ```

3. Review the plugin's event_history entries:

   ```sql
   SELECT * FROM event_history
   WHERE plugin_name = 'example/compromised'
   ORDER BY created_at DESC
   LIMIT 100;
   ```

4. If the plugin had `database: true`, check for unexpected data changes:

   ```sql
   -- Unexpected table creations. Plugin tables are in the worker's schema
   -- (public by default), NOT in a per-tenant one -- this query named
   -- 'tenant_<uuid>' until cleat#1279 and returned nothing during an
   -- investigation, which reads as "the plugin created nothing".
   SELECT table_name
   FROM information_schema.tables
   WHERE table_schema = current_schema()
   AND table_name NOT IN (known_plugin_tables);
   ```

   A plugin's rows are harder to scope than its tables, because every plugin
   table sits in one shared schema keyed by `tenant_id`. Check the tables the
   plugin declares, filtered by the affected tenant, rather than looking for a
   schema that holds its data.

#### Step 4: Remove (after all workflows complete)

Once all workflows referencing the compromised version have completed, you can
remove the WASM binary:

```
cleat plugin uninstall example/compromised 0.1.0 --purge
```

This removes the entry from `plugin_defs`. It does NOT remove any data the
plugin wrote -- that requires manual cleanup, and there is no schema to drop:
plugin tables live in the worker's schema alongside the core tables, so removal
is table-by-table and row-by-row (cleat#1279). Note also that dropping the
tenant does not do it for you -- see **Tenant deletion** above and cleat#1289.

#### Step 5: Notify affected tenants

If the compromised plugin had access to tenant data, notify the affected tenants
according to your data breach policy. Provide:
- What data the plugin had access to
- What time period the plugin was installed
- What steps you've taken (deprecation, capability revocation, removal)
- What the tenant should do (rotate API keys, review workflows)

### Scenario: A plugin is consuming excessive resources

1. Check connection counts:

   ```sql
   SELECT usename, count(*) as connections
   FROM pg_stat_activity
   GROUP BY usename;
   ```

2. Reduce the tenant's connection limit temporarily:

   ```sql
   ALTER ROLE cleat_tenant_<uuid> CONNECTION LIMIT 3;
   ```

3. Identify the problematic plugin from worker metrics (see [Monitoring](#9)).

4. Deprecate the problematic version or reduce its capabilities.

5. After remediation, restore the connection limit:

   ```sql
   ALTER ROLE cleat_tenant_<uuid> CONNECTION LIMIT 10;
   ```

### Scenario: Supply chain attack via plugin update

If a plugin author's account is compromised and a malicious update is published:

1. **Do not update** -- pin to the known-good version.
2. Deprecate the malicious version in your private index mirror.
3. Remove the malicious version from your local `plugin_defs` table:

   ```sql
   UPDATE plugin_defs SET deprecated = true
   WHERE name = 'example/compromised' AND version = '1.2.3';
   ```

4. Report to the index maintainers so they can remove the malicious entry.
5. If any workflow was executed with the malicious update, investigate
   following the procedures above.

---

## 8. Connection limits and resource isolation

### PostgreSQL connection limits

Each tenant role has a connection limit:

```sql
ALTER ROLE cleat_tenant_<uuid> CONNECTION LIMIT 10;
```

This prevents a single tenant (or a buggy plugin within that tenant) from
exhausting all database connections. The worker's per-tenant pool is configured
to stay within this limit:

```go
pool.SetMaxOpenConns(5) // well under the connection limit
pool.SetMaxIdleConns(2)
```

### WASM sandbox resource limits

> **Corrected 2026-09-06 — this table described enforcement that does not
> exist, and it is left visible rather than deleted so the gap is not silently
> re-closed.** Measured on this tree:
>
> - `--plugin-memory-limit` and `--plugin-gas-limit` are not flags.
>   `grep -rn 'plugin-memory-limit\|plugin-gas-limit' --include='*.go' .`
>   returns nothing; `cmd/cleat-worker/config.go` has `--plugin-config` and
>   `--max-plugin-connections` and no others in this family.
> - Nothing in production compiles a plugin module at all.
>   `PluginLoader.LoadPlugin` has **no non-test callers**, and the only two
>   non-test `NewPluginLoader` calls are in `cmd/cleat/plugin_cmd.go`, both
>   passing a nil `*Runtime` (they deploy and list; they do not execute).
>   `cmd/cleat-worker` constructs no `PluginLoader`.
> - So the sandbox named below is real code with real wazero types, and it is
>   not on any path a workflow reaches. **A limits table for an unwired
>   execution path is the most flattering possible error**: it reads as
>   defence-in-depth and measures nothing.
>
> The wazero attribution itself is *not* the error here — `PluginLoader` is
> genuinely wazero-typed (`wazero.CompiledModule`), unlike the worker paths
> corrected elsewhere in this sweep. Tracked as its own item; see
> IMPROVEMENT-PLAN §3.314.

The limits below are the design intent, not the shipped behaviour:

| Resource | Intended limit | Intended configuration |
|----------|--------------|---------------|
| Memory | 50 MB | `--plugin-memory-limit` on the worker (does not exist) |
| CPU instructions (gas) | 10 million per call | `--plugin-gas-limit` on the worker (does not exist) |
| Instance count | 100 concurrent | Fixed; adjust per deployment |

The intended behaviour when a plugin exceeds its gas limit is an error in the
workflow event history:

```
Plugin "example/hello-world" host function "greet" exceeded instruction budget.
```

### Guidance for sizing

| Deployment size | Max tenants per worker | Total DB connections | Notes |
|---------------|----------------------|---------------------|-------|
| Small | 10-20 | 50-100 | Single worker, modest hardware |
| Medium | 50-100 | 250-500 | Multiple workers, balanced load |
| Large | 500+ | 2500+ | Sharded deployment, per-shard pools |

### Configuring pool sizes per worker

**None of the configuration below exists.** It described an intended design and
read as shipped behaviour; cleat#1470 measured that no part of it is
implemented. Kept as a sketch of the intent, marked, rather than deleted —
cleat#1486 is where the real policy is being designed, and the shape here is
part of its input.

```json
// DOES NOT EXIST -- intended design only, see cleat#1486
{
  "tenant_pool": {
    "max_open_per_tenant": 5,
    "max_idle_per_tenant": 2,
    "conn_max_lifetime": "5m",
    "pool_eviction_ttl": "15m"
  }
}
```

`git grep -n 'pool_eviction_ttl\|max_open_per_tenant' -- '*.go'` returns
nothing. There is no config file of this shape and no key of any of these
names.

**What exists instead.** One flag, `--tenant-pool-max-conns` (default 25),
applied per tenant pool. There is no TTL and **no eviction of any kind**:

* `TenantPools.EvictIdle` (`plugin/tenant_db.go:131`) returns `0`
  unconditionally and has no caller.
* `TenantPools` records no last-used timestamps, so a TTL policy would have
  nothing to read even if it were called.

So a worker opens a pool per tenant it has ever touched and holds it for the
worker's lifetime. **The count is unbounded in tenant count and never
decreases**, whatever the traffic pattern. Size for the tenants a worker will
serve, not for the tenants active at any moment.

The sizing table above should be read the same way: it is guidance for
provisioning, not a description of a mechanism that reclaims anything.

---

## 9. Monitoring

### What to watch

#### Per-tenant connection counts

Track the number of database connections per tenant role. A sudden spike in
connections from one tenant may indicate a runaway plugin or a denial of
service.

```sql
-- Active connections by tenant role
SELECT usename, count(*) as connections, state
FROM pg_stat_activity
WHERE usename LIKE 'cleat_tenant_%'
GROUP BY usename, state
ORDER BY connections DESC;
```

Set up an alert when any tenant exceeds 80% of its connection limit.

#### Plugin error rates

The worker exports error counts per plugin. Monitor for sudden increases:

```
cleat_plugin_errors_total{plugin="example/hello-world",func="greet"}
```

Alert on:
- Error rate > 5% for any host function
- Error rate increasing over 15 minutes without explanation
- Any "capability violation" errors (may indicate a plugin trying to exceed
  its allowed capabilities)

#### Host function latency

Monitor latency percentiles per plugin host function:

```
cleat_plugin_call_duration_seconds{plugin="example/hello-world",func="greet",quantile="0.99"}
```

Alert on:
- p99 latency > 5 seconds (indicates plugin is struggling or blocking)
- Latency consistently increasing over time

#### WASM gas/memory exhaustion

Track how often plugins hit resource limits:

```
cleat_plugin_gas_exhausted_total{plugin="example/hello-world"}
cleat_plugin_memory_exhausted_total{plugin="example/hello-world"}
```

Alert on any exhaustion events -- they indicate a plugin that needs its limits
adjusted or has a bug causing infinite loops.

#### Workflow impact

If a plugin host function fails, it may cause workflow failures:

```
cleat_workflow_failures_total{reason="plugin_error",plugin="example/hello-world"}
```

Alert on any plugin-related workflow failures. These are always operator-actionable.

### Worker health endpoint

A plugin that implements `plugin.HasHealth` (`Health() error`) is polled every 10 seconds by the worker, off
the request path, and a plugin that reports an error makes `/livez`, `/readyz` and `/healthz` answer 200
with `"degraded": true` and the reason code `plugin_unhealthy`. The public bodies never name the plugin or
quote its message; the authenticated `GET /api/admin/health` does:

```
GET /api/admin/health          (needs an API key)

{ "live": true, "ready": true, "degraded": ["plugin_unhealthy"],
  "plugins": { "audit-log": "audit-log lost 3 event(s) ..." }, ... }
```

See [Health, readiness and telling a database incident from a worker incident](../../operations/health-and-incidents.md).

### Dashboard recommendations

Create a dashboard with these panels:

1. **Installed plugins**: number, versions, deprecation status
2. **Per-tenant connection counts**: top 10 tenants by connections
3. **Plugin error rate**: time-series by plugin
4. **Host function latency**: p50/p95/p99 by function
5. **WASM resource exhaustion**: gas and memory limit hits
6. **Workflow failures**: breakdown by reason (plugin vs. code vs. timeout)
7. **Tenant pool sizes**: active pools, idle pools, eviction rate
