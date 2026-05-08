# Cleat Plugin System Design

## Overview

A cleat plugin is a unit of extensibility that adds capabilities — database
tables, HTTP endpoints, workflow-callable host functions, background workers —
on top of cleat's shared PostgreSQL, auth, tenant isolation, and observability.

The plugin system has two complementary tiers:

1. **Compile-time Go plugins** — first-party plugins compiled into the worker
   binary via `init()` registration. These use the `Plugin` interface in
   `internal/plugin/`. Fast iteration, full Go tooling, no distribution
   concerns.

2. **WASM runtime plugins** — third-party plugins distributed as WASM binaries,
   loaded from the `plugin_defs` database table, compiled and instantiated by
   wazero at runtime. These have a stable host-function ABI that the cleat project
   commits to maintain across releases.

Each tier serves a different developer. First-party plugins are developed in the
cleat monorepo and ship with the official worker binary. Third-party plugins are
developed in separate repositories, published to a plugin index, and installed
by operators into their deployment.

---

## Part 1: Plugin Manifest and Cross-Language SDK Generation

### Problem

Each language SDK (TypeScript, Python, Rust, Java) currently has manually
maintained `Plugins` wrapper classes with typed methods for each plugin's host
functions. Adding a new plugin or a new function on an existing plugin requires
coordinated changes across 4+ SDKs. This doesn't scale, and the wrappers will
drift.

### Solution: Plugin manifest as source of truth

A plugin ships a manifest file (`plugin.json`) alongside its WASM binary. The
manifest describes the plugin's host functions — their names, input types,
output types, and whether they are streaming or idempotent.

```json
{
  "name": "blobstore",
  "version": "1.2.0",
  "description": "Content-addressed blob storage with S3 backend",
  "author": "cleat",
  "capabilities": {
    "database": true,
    "start_workflow": false,
    "signal_workflow": false,
    "http_routes": true
  },
  "host_functions": {
    "put": {
      "description": "Store a blob, returning its SHA-256 content address",
      "input": {
        "key": {"type": "string", "description": "Blob key"},
        "data": {"type": "bytes", "description": "Base64-encoded blob content"},
        "content_type": {"type": "string", "description": "MIME type", "optional": true},
        "ttl_seconds": {"type": "int64", "description": "Time-to-live in seconds", "optional": true}
      },
      "output": {
        "sha256": {"type": "string", "description": "SHA-256 hex digest"},
        "size": {"type": "int64", "description": "Size in bytes"}
      },
      "idempotent": false
    },
    "get": {
      "description": "Retrieve a blob by key",
      "input": {
        "key": {"type": "string"}
      },
      "output": {
        "data": {"type": "bytes"},
        "content_type": {"type": "string"},
        "sha256": {"type": "string"},
        "size": {"type": "int64"}
      },
      "idempotent": true
    },
    "delete": {
      "description": "Delete a blob by key",
      "input": {
        "key": {"type": "string"}
      },
      "output": {},
      "idempotent": false
    }
  },
  "types": {
    "BlobMetadata": {
      "fields": {
        "key": {"type": "string"},
        "sha256": {"type": "string"},
        "size": {"type": "int64"},
        "content_type": {"type": "string"},
        "created_at": {"type": "string", "format": "date-time"},
        "deleted_at": {"type": "string", "format": "date-time", "optional": true}
      }
    }
  }
}
```

### Code generation

From this manifest, a single CLI command generates typed wrappers for every
supported SDK:

```
cleat plugin generate-sdk --manifest plugin.json --lang typescript --out ./sdk/
cleat plugin generate-sdk --manifest plugin.json --lang python --out ./sdk/
cleat plugin generate-sdk --manifest plugin.json --lang rust --out ./sdk/
cleat plugin generate-sdk --manifest plugin.json --lang java --out ./sdk/
```

Each generated wrapper uses the language's native type system to expose the
host functions as typed methods. A workflow author calling `plugins.blobstore.put(key, data)`
gets IDE autocomplete, type checking, and inline documentation — in their
language, without anyone manually writing a wrapper.

Example generated TypeScript output:

```typescript
// Auto-generated from plugin manifest: blobstore v1.2.0
// Do not edit by hand.

export interface BlobMetadata {
  key: string;
  sha256: string;
  size: number;
  content_type: string;
  created_at: string;
  deleted_at?: string;
}

export interface PutInput {
  key: string;
  data: Uint8Array;
  content_type?: string;
  ttl_seconds?: number;
}

export interface PutOutput {
  sha256: string;
  size: number;
}

export interface GetInput {
  key: string;
}

export interface GetOutput {
  data: Uint8Array;
  content_type: string;
  sha256: string;
  size: number;
}

export class BlobstorePlugin {
  constructor(private hostCalls: HostCalls) {}

  async put(input: PutInput): Promise<PutOutput> {
    const result = await this.hostCalls.pluginCall("blobstore", "put", JSON.stringify(input));
    return JSON.parse(result) as PutOutput;
  }

  async get(input: GetInput): Promise<GetOutput> {
    const result = await this.hostCalls.pluginCall("blobstore", "get", JSON.stringify(input));
    return JSON.parse(result) as GetOutput;
  }

  async delete(input: { key: string }): Promise<void> {
    await this.hostCalls.pluginCall("blobstore", "delete", JSON.stringify({ key: input.key }));
  }
}
```

The key property: **every SDK wrapper is a build artifact, not a maintained
source file.** The manifest is the source of truth. Adding a host function
means updating the manifest and regenerating — no hand-editing 4 sets of
wrapper code.

### Type system

The manifest type system is deliberately simple. It covers the types that
make sense in a JSON-over-WASM boundary:

| Type | JSON representation |
|------|---------------------|
| `string` | JSON string |
| `int64` | JSON number |
| `float64` | JSON number |
| `bool` | JSON boolean |
| `bytes` | Base64-encoded JSON string |
| `timestamp` | RFC 3339 JSON string |
| `uuid` | UUID-formatted JSON string |
| `object` | JSON object (inline `fields`) |
| `enum` | JSON string with `values` constraint |
| `array<T>` | JSON array |
| `optional<T>` | JSON value or `null` |
| `map<K,V>` | JSON object with string keys |

This covers every plugin host function in the current catalog. If a plugin
needs a type not expressible in this system, it can use raw `string` input
and document its format — the manifest system doesn't force abstraction.

### Integration with the Go plugin

For first-party Go plugins, the manifest also drives the `HasHostFunctions`
implementation. The Go side of the code generator produces a `RegisterHostFunctions`
that validates input against the schema and returns typed output:

```go
// Auto-generated from manifest. Do not edit.
func (p *BlobstorePlugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
    scope.Register(plugin.FuncOptions{Name: "put"}, func(ctx context.Context, inputJSON string) (string, error) {
        var input PutInput
        if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
            return "", fmt.Errorf("blobstore.put: invalid input: %w", err)
        }
        output, err := p.put(ctx, input)
        if err != nil {
            return "", err
        }
        outputJSON, err := json.Marshal(output)
        if err != nil {
            return "", fmt.Errorf("blobstore.put: marshal output: %w", err)
        }
        return string(outputJSON), nil
    })
    // ... get, delete, etc.
    return nil
}
```

This eliminates the manual JSON parsing/marshaling in every host function that
exists today.

---

## Part 2: Third-Party Plugin Development

### Distribution model

Third-party plugins are WASM binaries distributed via the `plugin_defs` database
table. An operator installs a third-party plugin with:

```
cleat plugin install acme/salesforce@^1.0.0
```

This fetches the plugin WASM binary from the index, verifies its checksum,
and inserts it into the local `plugin_defs` table. From that point on, workflows
can depend on `acme/salesforce` with semver constraints, and the worker's
PluginLoader resolves and loads the WASM module at runtime.

A third-party plugin author's workflow:

1. Write the plugin in Go (or any language that compiles to WASM targeting the
   cleat host-function ABI)
2. Write a `plugin.json` manifest describing host functions, types, and
   capabilities
3. Run `cleat plugin build` to compile to WASM and bundle with the manifest
4. Publish to GitHub releases (or any HTTP-accessible URL)
5. Submit a PR to the cleat plugin index repository

### The plugin index

The index is a single Git repository (`github.com/rcownie/cleat-plugins`)
containing an `index.yaml` file:

```yaml
plugins:
  - name: acme/salesforce
    description: Salesforce CRM integration for cleat workflows
    author: acme-corp
    repository: https://github.com/acme/cleat-salesforce
    versions:
      - version: 0.1.0
        wasm_url: https://github.com/acme/cleat-salesforce/releases/download/v0.1.0/plugin.wasm
        manifest_url: https://github.com/acme/cleat-salesforce/releases/download/v0.1.0/plugin.json
        checksum: "sha256:abc123def456..."
        min_cleat_version: ">=1.0.0"

      - version: 0.2.0
        wasm_url: https://github.com/acme/cleat-salesforce/releases/download/v0.2.0/plugin.wasm
        manifest_url: https://github.com/acme/cleat-salesforce/releases/download/v0.2.0/plugin.json
        checksum: "sha256:789abc101112..."
        min_cleat_version: ">=1.1.0"

  - name: cleat/llm
    description: Unified LLM interface (chat, embedding, model listing)
    author: cleat
    repository: https://github.com/rcownie/cleat
    versions:
      - version: 1.0.0
        bundled: true
        description: "Ships with cleat-worker binary"
```

### Curated vs. community plugins

The index has two sections:

- **Curated** (`cleat/*`) — official plugins. Reviewed by the cleat project,
  tested in CI, guaranteed to work with the current cleat release. Listed in
  the default index file.

- **Community** (`<org>/*`) — third-party plugins. Submitted via PR to the
  index. Not reviewed by the cleat project beyond basic manifest validation
  and checksum verification. The operator accepts the risk.

The `cleat plugin install` command warns when installing a community plugin:

```
$ cleat plugin install acme/salesforce

WARNING: This plugin is maintained by a third party (acme-corp).
The cleat project has not reviewed its code or verified its security.
Installing a third-party plugin gives it access to your database,
workflows, and infrastructure within the scope defined by its
capability declaration.

Review the plugin source at: https://github.com/acme/cleat-salesforce

Capability declaration:
  database:        true   (scoped to tenant role)
  start_workflow:  false
  signal_workflow: true
  http_routes:     false

Install anyway? [y/N]
```

### How third-party plugins run

A WASM plugin runs inside the wazero sandbox. It cannot:
- Make syscalls (the WASI layer is not exposed to plugins)
- Access the filesystem
- Make network connections
- Import Go packages
- Access memory outside its linear memory

It CAN call cleat host function imports, which are the bridge to the outside
world. These are the same `cleat_*` imports that workflow WASM modules use,
plus `plugin_call` to call other plugins' host functions.

The wazero sandbox provides defense-in-depth against arbitrary code execution,
but it is NOT the primary security boundary. A plugin that can call
`cleat_plugin_call("llm", "chat", ...)` can still exfiltrate data to an
external LLM. The primary security boundary is the PostgreSQL roles and
capability system described in Part 3.

### Stable host-function ABI

For third-party plugins to work across cleat releases, the WASM import/export
contract must be stable. The imports that plugins depend on are:

```
cleat_plugin_call(name_ptr, name_len, func_ptr, func_len, input_ptr, input_len, response_ptr, response_max) -> i64
cleat_plugin_call_streaming(name_ptr, name_len, func_ptr, func_len, input_ptr, input_len, stream_id_ptr) -> i64
cleat_read_stream(stream_id, buf_ptr, buf_max) -> i64
cleat_close_stream(stream_id) -> i64
cleat_log(level, msg_ptr, msg_len)
cleat_get_tenant_id(buf_ptr, buf_max) -> i64
```

These are versioned via a `cleat_abi_version` export on the plugin WASM module.
The loader checks this version before instantiating. If a plugin requires a
newer ABI than the worker supports, the worker refuses to load it with a clear
error message.

---

## Part 3: Security and Multitenancy

### The threat model

Third-party plugins are untrusted code. They run inside the worker process with
access to the database, the ability to start and signal workflows, and the
ability to call other plugins' host functions.

The threats:
1. **Cross-tenant data access** — plugin for tenant X reads tenant Y's data
2. **Privilege escalation** — plugin escapes its tenant scope, modifies core
   tables, creates roles
3. **Workflow injection** — plugin starts or signals workflows across tenants
4. **Exfiltration via host functions** — plugin calls `llm.chat` or
   `slacknotify.send` to exfiltrate data to external systems
5. **Supply chain** — a benign plugin publishes a malicious update
6. **Denial of service** — plugin consumes all connections, CPU, or memory

### Per-tenant PostgreSQL schemas

The current design has RLS policies in the migration that are never activated.
No Go code calls `SET LOCAL cleat.tenant_id`, so `current_setting()` always
returns NULL, and the COALESCE resolves to the default tenant. Tenant isolation
currently relies entirely on application-level cooperation — every query must
manually include `WHERE tenant_id = $1`. Worse, everything runs as a single
owner-privileged PostgreSQL user.

The fix: each tenant gets a PostgreSQL **login role** and its own **schema**.

```
Database: cleat
├── public.*              — core tables (workflow_instances, event_history, ...)
│                           Owned by cleat_owner. GRANT SELECT/INSERT/UPDATE/DELETE
│                           to tenant roles, with RLS filtering by tenant_id.
│
├── tenant_a1b2c3.*       — tenant X's schema
│   ├── blob_index         — blobstore tables (owned by tenant, full DDL)
│   ├── blob_content
│   ├── webhook_config     — webhookingest tables
│   └── ...                — any tables the tenant's plugins create
│
├── tenant_d4e5f6.*       — tenant Y's schema
│   └── ...
│
├── admin.*                — tenants, tenant_api_keys, plugin_defs,
│   │                       plugin_migrations
│   │                       Only cleat_owner has access.
│
└── plugin_shared.*        — shared plugin metadata (plugin_tables registry)
```

Each tenant's `search_path` is `tenant_<uuid>, public`. A tenant role querying
`blob_index` resolves to its own schema. A tenant role querying
`workflow_instances` resolves to `public` with RLS enforcement.

**Schema: provisioning a tenant**

```sql
-- 1. Create login role (no superuser, no create role, no create DB).
CREATE ROLE cleat_tenant_<uuid> WITH LOGIN PASSWORD '<random>'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT;

-- 2. Create the tenant's schema.
CREATE SCHEMA AUTHORIZATION cleat_tenant_<uuid>;
-- This schema is OWNED by the tenant role, so the role can CREATE/ALTER/DROP
-- tables within it.

-- 3. Set search_path.
ALTER ROLE cleat_tenant_<uuid> SET search_path = 'tenant_<uuid>, public';

-- 4. Grant access to core tables in public.
GRANT SELECT, INSERT, UPDATE, DELETE
    ON public.workflow_defs, public.workflow_instances, public.event_history,
       public.workflow_signals, public.workflow_schedules, public.workflow_promises
    TO cleat_tenant_<uuid>;

GRANT USAGE ON SCHEMA public TO cleat_tenant_<uuid>;
GRANT USAGE ON SCHEMA plugin_shared TO cleat_tenant_<uuid>;

-- 5. Set the tenant_id session variable (activates RLS on public tables).
ALTER ROLE cleat_tenant_<uuid> SET cleat.tenant_id = '<uuid>';

-- 6. Register the plugin tables this tenant can use.
-- Called per-plugin when the operator enables a plugin for this tenant.
SELECT grant_plugin_to_tenant('blobstore', '<uuid>');
```

### Why schemas, not SET ROLE on a shared connection

A per-query `SET ROLE` / `RESET ROLE` on a shared owner connection is
vulnerable to escape. The plugin code runs Go compiled into the worker binary.
If the connection is owner-privileged and the wrapper calls `SET ROLE` before
running the plugin's SQL, the plugin can inject `RESET ROLE` into its own
query string and regain owner privileges. Similarly, a malicious WASM plugin
that calls a host function implemented with per-query role switching could
attempt the same escape through the host function's SQL.

With per-tenant login roles and separate connection pools, there is no
`SET ROLE` call to escape. The connection IS the tenant. The worker connects
directly as `cleat_tenant_<uuid>` with that role's password. The role has no
`CREATEROLE`, no superuser, no membership in other roles. It can `RESET ROLE`
all it wants — it resets to itself.

### Per-tenant connection pools

The worker maintains a pool per tenant. Each pool connects as the tenant's
login role:

```go
type TenantPools struct {
    ownerDB *sql.DB  // cleat_owner — for claiming, migrations, admin
    mu      sync.Mutex
    pools   map[uuid.UUID]*sql.DB
}

func (tp *TenantPools) For(ctx context.Context, tenantID uuid.UUID) (*sql.DB, error) {
    tp.mu.Lock()
    pool, ok := tp.pools[tenantID]
    tp.mu.Unlock()
    if ok {
        return pool, nil
    }

    // Look up tenant credentials from the admin table (running as owner).
    var roleName, password string
    err := tp.ownerDB.QueryRowContext(ctx,
        `SELECT role_name, password FROM admin.tenant_roles WHERE tenant_id = $1`,
        tenantID).Scan(&roleName, &password)
    if err != nil {
        return nil, fmt.Errorf("lookup tenant role: %w", err)
    }

    // Open a new pool that connects AS the tenant.
    connStr := tp.buildTenantDSN(roleName, password)
    pool, err := sql.Open("postgres", connStr)
    if err != nil {
        return nil, err
    }
    pool.SetMaxOpenConns(5) // per-tenant, conservative
    pool.SetMaxIdleConns(2)
    pool.SetConnMaxLifetime(5 * time.Minute)

    tp.mu.Lock()
    tp.pools[tenantID] = pool
    tp.mu.Unlock()
    return pool, nil
}
```

The `Pool()` method on `TenantPools` creates a pool with a bounded size per
tenant (default 5 connections). For deployments with many tenants, idle pools
are evicted after a TTL. For deployments with thousands of tenants, a shared
pool with per-connection tenant auth can replace per-tenant pools — but that's
an optimization, not an architecture change.

**What PostgreSQL enforces**

| Action | Outcome |
|--------|---------|
| Plugin queries `SELECT * FROM workflow_instances WHERE tenant_id = <X>` | Succeeds — role has GRANT, RLS matches |
| Plugin queries `SELECT * FROM workflow_instances` (no tenant filter) | Returns only own tenant's rows — RLS enforces |
| Plugin queries `SELECT * FROM workflow_instances WHERE tenant_id = <Y>` | Returns empty — RLS blocks other tenant's rows |
| Plugin queries `SELECT * FROM tenants` | Permission denied — no GRANT on admin tables |
| Plugin runs `DROP TABLE public.workflow_instances` | Permission denied — no DDL on public |
| Plugin runs `CREATE TABLE my_cache (...)` | Succeeds — table created in own schema |
| Plugin runs `ALTER TABLE blob_index ADD COLUMN priority INT` | Succeeds — owns its own tables |
| Plugin runs `DROP TABLE blob_index CASCADE` | Succeeds — owns its own tables |
| Plugin queries `SELECT * FROM tenant_Y.blob_index` | Permission denied — no USAGE on other schemas |
| Plugin runs `SET ROLE cleat_owner` | Permission denied — not a member |
| Plugin runs `RESET ROLE` | No-op — resets to itself |
| Plugin runs `CREATE ROLE hacker WITH LOGIN` | Permission denied — NOCREATEROLE |

### Plugin table management

When the blobstore plugin is enabled for a tenant, it creates its tables in the
tenant's schema. The plugin migration system is extended to create tables in
`${tenant_schema}.table_name` rather than `public.table_name`:

```sql
-- Plugin migration (runs during plugin Init or `cleat plugin enable`).
-- ${TENANT_SCHEMA} is expanded at runtime.
CREATE TABLE IF NOT EXISTS ${TENANT_SCHEMA}.blob_index (
    key         TEXT NOT NULL,
    sha256      TEXT NOT NULL,
    size        BIGINT NOT NULL,
    content_type TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ,
    PRIMARY KEY (key)
);

CREATE TABLE IF NOT EXISTS ${TENANT_SCHEMA}.blob_content (
    sha256    TEXT PRIMARY KEY,
    data      BYTEA NOT NULL,
    ref_count INT NOT NULL DEFAULT 1
);
```

Because the tenant role owns its schema, the plugin can subsequently
`ALTER TABLE`, `CREATE INDEX`, or `DROP TABLE` as its code evolves. Schema
evolution is the plugin's responsibility within its own namespace.

A shared `plugin_tables` table in the `admin` schema tracks which tables each
plugin manages, so the tenant provisioning function knows what to grant:

```sql
CREATE TABLE admin.plugin_tables (
    plugin_name  TEXT NOT NULL,
    table_name   TEXT NOT NULL,
    PRIMARY KEY (plugin_name, table_name)
);

-- Register on plugin install.
INSERT INTO admin.plugin_tables VALUES ('blobstore', 'blob_index');
INSERT INTO admin.plugin_tables VALUES ('blobstore', 'blob_content');
```

### Tenant deletion

Dropping a tenant is clean and complete:

```sql
-- 1. Drop the schema CASCADE — every table the tenant's plugins created is gone.
DROP SCHEMA tenant_<uuid> CASCADE;

-- 2. Drop the role.
DROP ROLE cleat_tenant_<uuid>;

-- 3. Remove from tenants table.
DELETE FROM admin.tenants WHERE tenant_id = '<uuid>';
```

Three statements, and everything the tenant ever touched — workflows, events,
plugin data — is removed. No table-by-table cleanup, no orphaned rows.

### RLS remains as defense-in-depth

The existing RLS policies on `public.*` tables continue to filter by
`tenant_id`. Each tenant role has `ALTER ROLE ... SET cleat.tenant_id` which
PostgreSQL applies on connection. Both mechanisms are active:

- **Schema isolation**: a tenant can't even see another tenant's plugin tables
- **RLS on public tables**: if a role is misconfigured (too many GRANTs), RLS
  still filters rows by tenant_id
- **No DDL on public**: the tenant role doesn't own the public schema

### Connection limit defense-in-depth

The tenant role's connection limit prevents a single tenant from exhausting
the server:

```sql
ALTER ROLE cleat_tenant_<uuid> CONNECTION LIMIT 10;
```

The worker's per-tenant pool respects this limit. A plugin that tries to open
100 connections gets PostgreSQL errors, not server exhaustion.

### Capability declaration

Plugins declare what capabilities they need in `plugin.json`:

```json
{
  "capabilities": {
    "database": true,
    "start_workflow": false,
    "signal_workflow": true,
    "http_routes": false,
    "http_middleware": false,
    "call_plugin": ["llm", "slacknotify"],
    "background_worker": false
  }
}
```

The loader enforces:
- `database: false` → no `*sql.DB` in Environment (compile-time Go plugins) or
  no `cleat_db_*` imports (WASM plugins)
- `start_workflow: false` → `StartWorkflow` field is nil
- `signal_workflow: false` → `SignalWorkflow` field is nil
- `call_plugin: ["llm"]` → can only call `plugin_call` for those named plugins
- `http_routes: false` → `RegisterRoutes` is not called

For WASM plugins, the capability declaration is in the manifest. For Go
compile-time plugins, it's derived from the optional interfaces the plugin
implements. A plugin that doesn't implement `HasRoutes` can't register HTTP
handlers regardless of what it declares.

### Supply-chain integrity

Version pinning with checksum verification:

1. `plugin.json` includes a `checksum` field for the WASM binary
2. The index file also records the checksum
3. `cleat plugin install` verifies the downloaded WASM matches the checksum
   from BOTH the index and the manifest (if different, installation is refused)
4. On upgrade, `cleat plugin update` shows the checksum change and requires
   explicit confirmation
5. Deploying a new plugin version always creates a new row in `plugin_defs`
   with a new version — it never overwrites existing versions
6. In-flight workflows stay on the old plugin version via the worker pool
   routing mechanism already designed in `plugin-versioning.md`

### Namespace convention

Official plugins use bare names (`llm`, `blobstore`, `kvstore`). Third-party
plugins use an organization prefix (`acme/salesforce`, `examplecorp/jira`).
This makes the trust boundary visible in workflow code:

```typescript
// Clear from reading: one is official, one is third-party
const summary = await plugins.llm.chat({ ... });
const cases = await plugins["acme/salesforce"].listCases({ ... });
```

The plugin index rejects submissions that use the `cleat/` prefix unless they
are from the cleat project. The loader prevents a WASM plugin from claiming
a name that collides with a registered Go compile-time plugin.

### Denial of service

For WASM plugins: wazero provides gas metering (instruction count limits) and
memory limits. The runtime configures these per plugin. A plugin that exceeds
its instruction budget is killed with a clear error in the workflow event
history.

For Go compile-time plugins: no sandboxing beyond goroutine recovery (already
implemented — a panicking plugin is caught and disabled).

---

## Part 4: Architecture Summary

### Two tiers, one interface

```
┌──────────────────────────────────────────────────────────────┐
│                      cleat-worker binary                      │
│                                                               │
│  ┌──────────────────────────┐  ┌───────────────────────────┐ │
│  │  Compile-time Go plugins │  │  WASM runtime plugins      │ │
│  │                          │  │                            │ │
│  │  init() → Register()     │  │  plugin_defs table →       │ │
│  │  Discover() → InitAll()  │  │  PluginLoader.LoadPlugin() │ │
│  │                          │  │  wazero.Instantiate()      │ │
│  │  Plugin interface        │  │                            │ │
│  │  Optional interfaces     │  │  Host function imports     │ │
│  │  (HasRoutes, HasMigrations│  │  (cleat_plugin_call,      │ │
│  │   HasHostFunctions, etc.)│  │   cleat_log, etc.)         │ │
│  └──────────┬───────────────┘  └──────────┬────────────────┘ │
│             │                              │                   │
│             └──────────┬───────────────────┘                   │
│                        │                                       │
│               ┌────────▼────────┐                             │
│               │  Per-tenant DB  │                             │
│               │  pools          │                             │
│               │  Each connects  │                             │
│               │  AS tenant role │                             │
│               └────────┬────────┘                             │
│                        │                                       │
│               ┌────────▼────────┐                             │
│               │   PostgreSQL    │                             │
│               │  public.*       │  ← RLS filters by tenant_id │
│               │  tenant_<X>.*   │  ← owned by tenant, full DDL│
│               │  tenant_<Y>.*   │  ← invisible to tenant X    │
│               │  admin.*        │  ← cleat_owner only         │
│               └─────────────────┘                             │
└──────────────────────────────────────────────────────────────┘

     ┌─────────────────────────────────────────────┐
     │           Plugin Index (Git repo)            │
     │                                              │
     │  index.yaml                                  │
     │  ├── cleat/llm (bundled, reviewed)           │
     │  ├── cleat/blobstore (bundled, reviewed)     │
     │  ├── acme/salesforce (community, unreviewed) │
     │  └── examplecorp/jira (community, unreviewed)│
     │                                              │
     │  cleat plugin install → fetch WASM →         │
     │  verify checksum → insert into plugin_defs   │
     └─────────────────────────────────────────────┘
```

### Deployment flow

```
1. Operator: cleat plugin install acme/salesforce@^1.0.0
   → CLI queries index.yaml for acme/salesforce
   → Resolves ^1.0.0 → 1.2.0 (highest matching non-deprecated)
   → Downloads plugin.wasm from wasm_url
   → Verifies sha256 checksum against index
   → Inserts into plugin_defs (name, version, wasm_bytes, config)
   → Displays capability declaration
   → Warns if community plugin
   → Prompts for confirmation

2. Developer: references plugin in workflow code
   → import { salesforce } from "@cleat/acme-salesforce" (generated from manifest)
   → const cases = await salesforce.listCases({ status: "open" })

3. cleat build
   → Transformer sees salesforce.listCases call
   → Emits cleat_plugin_call("acme/salesforce", "list_cases", input_json)
   → Records plugin dep in workflow metadata

4. cleat deploy
   → WASM binary + plugin_deps stored in workflow_defs

5. Worker picks up workflow
   → Sees required_plugin_versions includes {"acme/salesforce": "1.2.0"}
   → Worker's plugin map must contain acme/salesforce
   → PluginLoader.LoadPlugin("acme/salesforce", "1.2.0")
   → Compiles and caches WASM module
   → Instantiates with configured gas/memory limits
   → Workflow execution proceeds
```

### What ships with the worker

The official cleat-worker binary includes all `cleat/*` plugins compiled in.
Third-party plugins are loaded from the database at runtime. An operator who
only wants official plugins never touches the WASM plugin path. An operator
who wants third-party plugins runs `cleat plugin install` — the WASM loader
already exists in the worker binary.

Operators who want a minimal worker can build a custom binary with only the
plugins they need, just as they do today:

```go
// my-worker/main.go
import (
    _ "github.com/rcownie/cleat/plugins/kvstore"
    _ "github.com/rcownie/cleat/plugins/blobstore"
)
```

---

## Part 5: Key Design Decisions

### Why WASM for third-party plugins instead of Go dynamic linking

Go's `plugin` package (`plugin.Open`) requires:
- Same Go compiler version as the host binary
- Same dependency versions
- Linux or macOS only (no Windows)
- CGO enabled

This makes it unusable for distributed plugins. WASM + wazero has none of
these constraints. A plugin compiled to WASM from Go (or Rust, or Zig, or C)
runs identically on any platform cleat supports.

### Why a Git-repo index instead of a registry service

A registry service (package server with search, auth, metrics) is the right
long-term answer, but it's infrastructure that needs to be run and maintained.
A Git repo with an index file is zero-infrastructure — GitHub serves it,
PRs handle submissions, git history provides audit trail.

The index file format is designed to be machine-readable so that the CLI can
point at any index URL — a company can run their own internal index for
private plugins by hosting the same file format in their own repo.

### Why per-tenant schemas and not one shared schema

The shared-schema approach (all tenants' data in `public.*` with RLS filtering)
is fragile. A single query missing `WHERE tenant_id = $1` leaks data across
tenants. Every plugin author, every stored procedure, every ad-hoc query must
remember the filter. The `SET ROLE` approach on a shared connection pool is
also fragile — the plugin's Go code runs as the owner user, and `RESET ROLE`
from within a query string can escape.

Per-tenant schemas with per-tenant login roles make isolation structural:
- A connection to the database IS a tenant. There is no role to escape from,
  no session to hijack, no `SET ROLE` call to inject.
- A table lives in one tenant's schema or another's. There is no `WHERE`
  clause to forget.
- A plugin can own and evolve its tables (CREATE, ALTER, DROP) without
  affecting any other tenant's data.
- Tenant deletion is `DROP SCHEMA ... CASCADE` — atomic, complete, verifiable.

For operators who need physical isolation (compliance, noisy-neighbor), the
sharded store already supports per-shard placement; a tenant can be placed on a
dedicated shard with its own schema there.

### Why capability declaration and not full sandboxing

full sandboxing (every WASM call mediated by a capability token, like Wasmtime's
WASI preview 2) would be ideal but is a multi-year project. The capability
declaration in the manifest is implementable in weeks and catches the common
cases: a plugin that shouldn't touch the database doesn't get a DB handle; a
plugin that can't start workflows doesn't get the function pointer. Combined
with PostgreSQL roles, this provides defense-in-depth at the database and
application layers.
