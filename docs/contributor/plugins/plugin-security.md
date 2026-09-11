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

`TenantScoped` is PostgreSQL-only. MySQL has no row-level security, and SQL
Server binds a tenant to a whole connection pool, which a per-request tenant
does not fit. On both, a plugin table is scoped by the Go predicate alone.

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
| Row-level security | yes, on every tenant-scoped table | only where `TenantScoped` is declared — one table today |
| The query's own `WHERE tenant_id` | yes | yes, and for most plugin tables it is the **only** layer |
| No DDL on `public` | a tenant role does not own the schema | same |

So for a plugin table without a policy, a forgotten `WHERE tenant_id = $1`
returns another tenant's rows, and nothing below it will catch that. That is
the gap cleat#1277 opened and cleat#1278 tracks the remainder of.

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

Installed plugins:
  Name                    Version    Deprecated    Capabilities
  ────────────────────────────────────────────────────────────────
  llm                     0.1.0      no            database=true, start_workflow=true
  blobstore               0.1.0      no            database=true, start_workflow=false
  example/hello-world     0.1.0      no            database=false, start_workflow=false
  acme/salesforce         1.2.0      no            database=true, start_workflow=false
```

### List with capabilities detail

```
$ cleat plugin list --verbose

  Plugin: example/hello-world v0.1.0
  ─────────────────────────────────────
  Author: example-corp
  Installed: 2026-05-01T10:30:00Z
  Deprecated: no
  Capabilities:
    database:        false
    start_workflow:  false
    signal_workflow: false
    http_routes:     false
    call_plugin:     []
  WASM size: 2.3 MB
  Checksum: sha256:abc123...
```

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

```
$ cleat plugin inspect example/hello-world@0.1.0

Plugin: example/hello-world v0.1.0
Author: example-corp
Repository: https://github.com/example/cleat-hello-world
Capabilities:
  database:        false
  start_workflow:  false
  signal_workflow: false

Host functions:
  greet(name: string) → message: string
    Returns a greeting for the given name. Idempotent: yes.

Checksum: sha256:abc123def456...
WASM URL: https://github.com/example/cleat-hello-world/releases/download/v0.1.0/plugin.wasm
```

### Inspect a local manifest

```
$ cleat plugin validate --manifest plugin.json --verbose

✓ Manifest is valid
  Name:        example/hello-world
  Version:     0.1.0
  Author:      example-corp
  Repository:  https://github.com/example/cleat-hello-world
  Capabilities:
    database:        false
    start_workflow:  false
    signal_workflow: false

Host functions:
  greet: (object) → (object), idempotent
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

1. Run `cleat plugin update example/hello-world` to see available upgrades:

   ```
   $ cleat plugin update example/hello-world
   
   Current: v0.1.0 (installed 2026-05-01)
   Available:
     v0.2.0  ─ 2026-05-15  ─ CHANGELOG: view
     v1.0.0  ─ 2026-06-01  ─ CHANGELOG: view
   ```

2. View the details of an upgrade:

   ```
   $ cleat plugin update example/hello-world@0.2.0 --show
   
   Plugin: example/hello-world
   Version: 0.2.0
   Checksum: sha256:789abc... (current: sha256:abc123...)
   Checksum changed: YES
   Capabilities: database=false, start_workflow=false (unchanged)
   
   Host functions:
     + greet_all(names: string[]) → messages: string[]
     greet(name: string) → message: string (unchanged)
   
   Proceed with upgrade? [y/N]
   ```

3. Confirm the upgrade. The CLI:
   - Downloads the new WASM binary
   - Verifies the checksum against the index
   - Creates a NEW row in `plugin_defs` (it never overwrites existing versions)
   - Displays a success message

### Important: version pinning

In-flight workflows stay on the old plugin version. The worker routes each
workflow invocation to the plugin version the workflow was started with. This
means:

- Upgrading a plugin does not affect running workflows
- You can safely upgrade during production without concern for in-flight
  disruption
- Old versions remain in `plugin_defs` until all workflows referencing them
  complete

### Checksum verification

On upgrade, the CLI verifies:

1. The downloaded WASM binary matches the checksum in the index
2. The checksum differs from the currently installed version (a same-checksum
   upgrade is a no-op)

If checksums don't match, the upgrade is refused:

```
ERROR: checksum mismatch: expected sha256:abc123..., got sha256:def456...
The downloaded binary does not match the index record. Upgrade refused.
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

```json
{
  "tenant_pool": {
    "max_open_per_tenant": 5,
    "max_idle_per_tenant": 2,
    "conn_max_lifetime": "5m",
    "pool_eviction_ttl": "15m"
  }
}
```

For deployments with many tenants, idle pools are evicted after TTL. A
connection to a tenant that hasn't been active for 15 minutes is dropped,
freeing the slot.

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

The worker exposes a health endpoint that includes plugin status:

```
GET /healthz

{
  "status": "ok",
  "plugins": {
    "llm": { "status": "ok" },
    "example/hello-world": {
      "status": "degraded",
      "error": "gas limit exceeded on last 3 calls"
    }
  },
  "tenants": {
    "active": 12,
    "pools": 12,
    "total_connections": 48
  }
}
```

### Dashboard recommendations

Create a dashboard with these panels:

1. **Installed plugins**: number, versions, deprecation status
2. **Per-tenant connection counts**: top 10 tenants by connections
3. **Plugin error rate**: time-series by plugin
4. **Host function latency**: p50/p95/p99 by function
5. **WASM resource exhaustion**: gas and memory limit hits
6. **Workflow failures**: breakdown by reason (plugin vs. code vs. timeout)
7. **Tenant pool sizes**: active pools, idle pools, eviction rate
