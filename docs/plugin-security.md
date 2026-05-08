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

### Per-tenant schemas

Each tenant gets its own PostgreSQL login role and schema:

```
Database: cleat
├── public.*              — core tables (workflow_instances, event_history, ...)
│                           Owned by cleat_owner. RLS filters by tenant_id.
│
├── tenant_a1b2c3.*       — tenant X's schema
│   └── plugin tables live here
│
├── tenant_d4e5f6.*       — tenant Y's schema
│
├── admin.*               — admin tables (cleat_owner only)
└── plugin_shared.*       — shared plugin metadata
```

The worker connects to PostgreSQL as the **tenant's login role**, not as
`cleat_owner`, when executing workflow or plugin code. This means:

- A plugin querying `SELECT * FROM workflow_instances` only sees its own
  tenant's rows (RLS enforces this).
- A plugin querying `SELECT * FROM tenant_Y.some_table` gets a permission
  error -- it has no access to other tenants' schemas.
- A plugin running `SET ROLE cleat_owner` fails -- the tenant role is not a
  member of `cleat_owner`.
- A plugin running `RESET ROLE` is harmless -- it resets to itself.

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

### RLS as defense-in-depth

The existing RLS policies on `public.*` tables remain active. Even if a tenant
role gains access to a system table it shouldn't, RLS filters by `tenant_id`.
Both mechanisms work together:

- **Schema isolation**: tenant can't see other tenants' plugin tables
- **RLS on public tables**: tenant can't see other tenants' rows
- **No DDL on public**: tenant doesn't own the public schema

### Tenant deletion

Deleting a tenant is a clean, three-statement operation:

```sql
DROP SCHEMA tenant_<uuid> CASCADE;
DROP ROLE cleat_tenant_<uuid>;
DELETE FROM admin.tenants WHERE tenant_id = '<uuid>';
```

All plugin data is removed atomically with the schema. No orphaned rows, no
table-by-table cleanup.

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
  "https://raw.githubusercontent.com/rcownie/cleat-plugins/main/index.yaml",
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
   -- Unexpected table creations in the tenant's schema
   SELECT table_name
   FROM information_schema.tables
   WHERE table_schema = 'tenant_<uuid>'
   AND table_name NOT IN (known_plugin_tables);
   ```

#### Step 4: Remove (after all workflows complete)

Once all workflows referencing the compromised version have completed, you can
remove the WASM binary:

```
cleat plugin uninstall example/compromised 0.1.0 --purge
```

This removes the entry from `plugin_defs`. It does NOT remove any data the
plugin may have written to the tenant's schema -- that requires manual cleanup.

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

For WASM plugins, the wazero sandbox enforces:

| Resource | Default limit | Configuration |
|----------|--------------|---------------|
| Memory | 50 MB | `--plugin-memory-limit` on the worker |
| CPU instructions (gas) | 10 million per call | `--plugin-gas-limit` on the worker |
| Instance count | 100 concurrent | Fixed; adjust per deployment |

A plugin that exceeds gas limits is killed with an error in the workflow event
history:

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
