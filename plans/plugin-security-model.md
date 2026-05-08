# Cleat Plugin Security Model

## Actors

| Actor | Identity | Scope | Trust level |
|-------|----------|-------|-------------|
| **cleat_owner** | PostgreSQL login role that owns `public.*` and `admin.*` | The whole cluster. Runs migrations, claims workflows, provisions tenants. | Fully trusted — this is the operator's database connection |
| **Tenant** | PostgreSQL login role `cleat_tenant_<uuid>` with password auth | One schema (`tenant_<uuid>`), plus RLS-filtered access to `public.*` | Untrusted to the operator, but each tenant trusts its own plugins |
| **First-party plugin** | Go code compiled into the worker binary, vetted by the cleat project | Within its tenant's scope — same as the tenant role | Trusted by the operator (reviewed, compiled in) |
| **Third-party plugin** | WASM binary loaded from `plugin_defs`, authored by an external developer | Within its tenant's scope, further constrained by capability declaration | **Untrusted** — the whole point of this document |
| **Workflow code** | WASM binary deployed by a tenant developer | Cannot execute SQL directly. Calls plugin host functions via `plugin_call`. | Untrusted |

---

## What a third-party plugin can read

### Within its tenant

| Data | How | Constraint |
|------|-----|------------|
| The tenant's own rows in `workflow_instances` | `SELECT * FROM workflow_instances` via `*sql.DB` | RLS on `public.workflow_instances` filters by `tenant_id` |
| The tenant's own rows in `event_history` | `SELECT * FROM event_history` via `*sql.DB` | RLS on `public.event_history` filters by `tenant_id` |
| The tenant's own rows in `workflow_signals` | Same pattern | RLS |
| The tenant's own rows in `workflow_schedules` | Same pattern | RLS |
| Tables the plugin created in `tenant_<uuid>.*` | Direct `SELECT` — the plugin owns these tables | Full read access within own schema |
| Tables created by OTHER plugins for the SAME tenant | `SELECT * FROM tenant_<uuid>.other_plugin_table` via `*sql.DB` or `plugin_call` | The tenant role has access to all tables in its schema. Plugins share a tenant's data space. |
| Workflow input/payload | `plugin.CallContextFromContext(ctx)` in host function | Engine injects this. Plugin receives the tenant ID and workflow ID of the calling workflow. |
| Plugin configuration | `env.Config` at `Init()` time | Set by the operator when installing or enabling the plugin |

### Cross-tenant: none

A plugin **cannot** read another tenant's data. There is no GRANT on other
tenants' schemas, and RLS on `public.*` prevents it. PostgreSQL enforces this.

```
-- This returns nothing, regardless of what the plugin writes in the query.
SELECT * FROM tenant_Y.some_table;  -- ERROR: permission denied for schema tenant_Y

-- This returns only the plugin's own tenant's rows.
SELECT * FROM public.workflow_instances;  -- RLS filters to tenant_id = <own-uuid>
```

---

## What a third-party plugin can write

### Within its tenant

| Action | Scope | Constraint |
|--------|-------|------------|
| `INSERT`/`UPDATE`/`DELETE` on `public.workflow_instances` | Only its own tenant's rows | RLS — the `USING` clause prevents touching other tenants' rows. But a plugin CAN modify its own tenant's workflows (e.g., change status, cancel executions). |
| `INSERT` on `public.event_history` | Only rows tagged with its tenant's `tenant_id` | If the plugin inserts an event with `tenant_id = <other-tenant>`, the RLS policy on `INSERT` (via `WITH CHECK`) blocks it. |
| `INSERT`/`UPDATE`/`DELETE` in its own schema | Full DML | The plugin owns `tenant_<uuid>.*`. It can write whatever it wants to tables it created. |
| Write to tables created by other plugins for the same tenant | Full DML | Plugins within a tenant share the schema. A malicious plugin A can corrupt plugin B's data. |
| `CREATE TABLE`, `ALTER TABLE`, `DROP TABLE` | Within `tenant_<uuid>.*` only | The tenant role owns its schema. It can evolve its tables. It cannot touch `public.*` or `admin.*`. |

### Across tenants: none

A plugin **cannot** write to another tenant's schema, and RLS prevents writing
to another tenant's rows in `public.*`. A plugin **cannot** `CREATE TABLE` in
`public` or `admin`.

### Workflow side effects

| Action | Constraint |
|--------|------------|
| Start a new workflow via `env.StartWorkflow` | The new workflow runs in the same tenant. `StartWorkflow` is only available if the plugin's capability declaration allows it. |
| Signal a running workflow via `env.SignalWorkflow` | Can only signal workflows within the same tenant. The `SignalWorkflow` implementation filters by `tenant_id`. Also gated by capability declaration. |
| Call another plugin's host function via `plugin_call` | The call runs within the same tenant's scope. Gated by `call_plugin` capability list. |

---

## What a third-party plugin CANNOT do

| Action | Blocked by |
|--------|------------|
| Read another tenant's data | Schema permissions (no USAGE on other schemas) + RLS on `public.*` |
| Write another tenant's data | Schema permissions + RLS `WITH CHECK` |
| `CREATE TABLE` in `public` | Permission denied — tenant doesn't own `public` |
| `DROP TABLE public.workflow_instances` | Permission denied — no DDL on `public` |
| `CREATE ROLE`, `ALTER ROLE`, `DROP ROLE` | `NOCREATEROLE` on tenant login role |
| `CREATE DATABASE` | `NOCREATEDB` on tenant login role |
| Read `admin.tenants`, `admin.tenant_api_keys`, `admin.plugin_tables` | No GRANT on `admin` schema |
| `RESET ROLE` to become `cleat_owner` | Tenant role is not a member of `cleat_owner`. `RESET ROLE` resets to itself. |
| `SET ROLE cleat_owner` | Permission denied — not a member |
| Open more than N connections | `CONNECTION LIMIT` on the tenant role |
| Access the filesystem | wazero sandbox (WASM plugins); Go binary already has filesystem access (compile-time plugins, but they're trusted) |
| Make network connections (WASM plugins) | wazero sandbox — no WASI sockets exposed |
| Consume unbounded CPU or memory (WASM plugins) | wazero gas metering and memory limits |
| Start a workflow in another tenant | `StartWorkflow` implementation scopes to `CallContext.TenantID` |
| Signal a workflow in another tenant | `SignalWorkflow` implementation filters by tenant |
| Register HTTP routes (if capability denies it) | Capability enforcement — `HasRoutes` not called, or WASM import not provided |
| Call a plugin host function not in its `call_plugin` list | Engine rejects `plugin_call` for unlisted plugins |

---

## Blast radius of a badly-behaved plugin

This is the most important section. Assume a third-party plugin is actively
malicious — what's the worst it can do?

### Scenario 1: Plugin tries to read every tenant's workflows

```go
// Malicious plugin host function.
rows, _ := db.QueryContext(ctx, "SELECT * FROM public.workflow_instances")
```

**Result:** Returns only rows where `tenant_id = <own-tenant>`. RLS appends the
filter automatically. No cross-tenant disclosure. Operator sees nothing unusual.

### Scenario 2: Plugin tries to read another tenant's schema

```go
rows, _ := db.QueryContext(ctx, "SELECT * FROM tenant_Y.secrets")
```

**Result:** `ERROR: permission denied for schema tenant_Y`. PostgreSQL rejects
it. The error appears in the plugin's event history. The workflow fails with a
clear error.

### Scenario 3: Plugin tries to drop a core table

```go
db.ExecContext(ctx, "DROP TABLE public.workflow_instances")
```

**Result:** `ERROR: must be owner of table workflow_instances`. The tenant
doesn't own `public.*` tables. Nothing happens.

### Scenario 4: Plugin tries to escalate to owner

```go
db.ExecContext(ctx, "SET ROLE cleat_owner")
```

**Result:** `ERROR: permission denied to set role "cleat_owner"`. The tenant
role isn't a member of `cleat_owner`. Nothing happens.

### Scenario 5: Plugin corrupts its own tenant's data

```go
// Malicious plugin that claims to be a "PDF generator" but actually
// deletes all workflows for its tenant.
db.ExecContext(ctx, "DELETE FROM public.workflow_instances")
```

**Result:** All of the tenant's workflow instances are deleted. RLS doesn't
prevent this because the tenant owns these rows. This is the worst case for a
single tenant.

**Blast radius: ONE tenant.** The operator can restore from backup. Other
tenants are unaffected. The malicious plugin is flagged and removed from the
index.

### Scenario 6: Plugin corrupts another plugin's data for the same tenant

```go
// Plugin A ("PDF generator") drops tables created by Plugin B ("invoice store").
db.ExecContext(ctx, "DROP TABLE tenant_<uuid>.invoices")
```

**Result:** Plugin B's data is gone. Both plugins share the same schema.

**Blast radius: ONE tenant, cross-plugin within that tenant.** This is a real
gap — plugins within a tenant are not isolated from each other. Mitigations:
- Only install plugins you trust for a given tenant
- The `call_plugin` capability list limits which plugins can interact
- Future: per-plugin schemas (`tenant_<uuid>_<plugin>`) for stronger isolation
  within a tenant

### Scenario 7: Plugin exfiltrates data through an external API

```go
// Plugin calls the LLM plugin to send tenant data to an external model.
var allData []Row
db.QueryContext(ctx, "SELECT * FROM tenant_X.my_table").Scan(&allData)
// Calls plugin_call("llm", "chat", toJSON(allData)) — data leaves the database
```

**Result:** The tenant's data reaches an external LLM API.

**Blast radius: ONE tenant's data that the plugin can read.** This is a real
threat. Mitigations:
- The `call_plugin` capability list controls WHICH plugins a plugin can call.
  If a plugin doesn't declare `"call_plugin": ["llm"]`, the engine rejects
  `plugin_call("llm", ...)`.
- For first-party plugins compiled into the worker: they're trusted. The
  operator chose to include them.
- For third-party plugins: the capability declaration and operator review of
  the plugin's source code are the primary defenses.

### Scenario 8: Plugin consumes all connections for its tenant

```go
// Plugin opens connections in a loop.
for i := 0; i < 1000; i++ {
    db.ExecContext(ctx, "SELECT pg_sleep(60)")
}
```

**Result:** The tenant's connection limit (default 10) is hit. Further queries
from this tenant fail with `too many connections for role`. Other tenants are
unaffected — each has its own pool and its own connection limit.

**Blast radius: ONE tenant, self-inflicted denial of service.** The tenant's
own workflows stall. Other tenants continue normally.

### Scenario 9: Plugin starts thousands of workflows

```go
// Plugin calls StartWorkflow in a loop.
for i := 0; i < 10000; i++ {
    env.StartWorkflow(ctx, "expensive_workflow", input)
}
```

**Result:** If the plugin's capability declaration includes `start_workflow:
true`, the engine allows it. Thousands of workflows are created **for the
plugin's own tenant**. The worker pool processes them normally.

**Blast radius: ONE tenant's workflow quota / resource consumption.** Mitigation:
- `start_workflow` defaults to `false` in the capability declaration. An
  operator must explicitly enable it.
- Future: per-tenant rate limiting on workflow creation.

---

## Summary table

| Attack | Cross-tenant? | Cross-plugin? | Data loss? | Blocked by |
|--------|:---:|:---:|:---:|------------|
| Read other tenant's data | — | — | — | Schema permissions + RLS |
| Write other tenant's data | — | — | — | Schema permissions + RLS |
| Drop core tables | — | — | — | Table ownership |
| Escalate to `cleat_owner` | — | — | — | `NOCREATEROLE`, no role membership |
| Delete own tenant's workflows | No | Yes | Yes (one tenant) | Nothing — the tenant owns its data |
| Corrupt another plugin's tables | No | Yes | Yes (one tenant, one plugin) | Nothing currently — same schema |
| Exfiltrate via `llm.chat` | No | — | No (disclosure) | `call_plugin` capability list |
| Exhaust connections | No | Yes | No (DoS) | `CONNECTION LIMIT` per role |
| Spawn flood of workflows | No | — | No (DoS) | `start_workflow` capability gate |

---

## Differences by plugin type

| Concern | First-party Go plugin | Third-party WASM plugin |
|--------|----------------------|------------------------|
| Code review | Reviewed by cleat project, compiled into worker | Not reviewed; operator must review or accept risk |
| Filesystem access | Yes (it's a Go binary) | No (wazero sandbox) |
| Network access | Yes (Go stdlib) | No (no WASI sockets) |
| Memory safety | Go GC + race detector in tests | wazero linear memory sandbox |
| CPU limits | None (goroutine scheduling) | Gas metering (instruction budget) |
| Memory limits | None (Go heap) | Per-instance memory cap |
| SQL injection escape | Can call `RESET ROLE`, but connection IS the tenant — no escalation possible | Cannot execute raw SQL — only calls typed host functions |
| Can corrupt own data | Yes | Yes |
| Can corrupt other plugins | Yes | Yes (within same tenant) |
| Supply chain | Go module proxy + `go.sum` | Checksum in index + pinned versions |
| Upgrade path | Rebuild worker binary | `cleat plugin update` |
