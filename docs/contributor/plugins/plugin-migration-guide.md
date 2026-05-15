# Plugin Migration Guide

This guide covers two migration paths:

1. **First-party Go plugins** adopting the manifest system to use code
   generation instead of manual JSON parsing
2. **Existing tenants** adopting PostgreSQL role-based isolation for stronger
   multi-tenant security

---

## Table of Contents

1. [Why migrate](#1-why-migrate)
2. [Migration path for Go compile-time plugins](#2-migration-path-for-go-compile-time-plugins)
3. [Migration path for SDK wrappers](#3-migration-path-for-sdk-wrappers)
4. [Migration path for tenant isolation](#4-migration-path-for-tenant-isolation)
5. [Verification checklist](#5-verification-checklist)
6. [Rollback](#6-rollback)

---

## 1. Why migrate

### For Go plugins: reduce boilerplate

Currently, every host function in a first-party Go plugin manually parses JSON
input, validates fields, marshals JSON output, and registers itself with the
`FuncRegistry`. With the manifest system, you:

- Write a `plugin.json` describing your host functions (types, names,
  idempotency)
- Run `cleat-gen-plugin` to generate typed wrappers
- Delete the hand-written JSON parsing and registration code

### For SDKs: eliminate drift

Currently, each language SDK (TypeScript, Python, Rust) has hand-maintained
`Plugins` wrapper classes with typed methods for every plugin's host functions.
Adding a new function requires coordinated changes across 4+ SDKs. With the
manifest system:

- The manifest is the source of truth for all SDKs
- SDK wrappers are build artifacts, not maintained source files
- Adding a host function means updating the manifest and regenerating

### For tenants: stronger isolation

The per-tenant PostgreSQL role system provides structural isolation between
tenants. Without it, tenant isolation relies on every query including
`WHERE tenant_id = $1`, which is fragile.

---

## 2. Migration path for Go compile-time plugins

### Step 1: Write a plugin.json manifest

Create a `plugin.json` in your plugin directory describing the plugin's host
functions. See the [plugin developer guide](plugin-developer-guide.md) for
the manifest format, and the [llm plugin.json](../plugins/llm/plugin.json) as a
reference example.

**Before** (hand-written registration):

```go
// plugins/myplugin/myplugin.go
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
    scope.Register(plugin.FuncOptions{Name: "do_thing"}, func(ctx context.Context, inputJSON string) (string, error) {
        var input DoThingInput
        if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
            return "", fmt.Errorf("do_thing: invalid input: %w", err)
        }
        result, err := p.doThing(ctx, input)
        if err != nil {
            return "", err
        }
        output, err := json.Marshal(result)
        if err != nil {
            return "", fmt.Errorf("do_thing: marshal output: %w", err)
        }
        return string(output), nil
    })
    return nil
}
```

**After** (manifest + generated code):

```json
{
  "name": "myplugin",
  "version": "1.0.0",
  "description": "My plugin description",
  "author": "cleat",
  "host_functions": {
    "do_thing": {
      "description": "Does a thing",
      "input": { "type": "DoThingInput" },
      "output": { "type": "DoThingOutput" },
      "idempotent": false
    }
  },
  "types": {
    "DoThingInput": {
      "type": "object",
      "fields": {
        "param1": { "type": "string", "description": "First parameter" },
        "param2": { "type": "int64", "description": "Second parameter" }
      }
    },
    "DoThingOutput": {
      "type": "object",
      "fields": {
        "result": { "type": "string", "description": "The result" }
      }
    }
  }
}
```

### Step 2: Generate the Go wrapper

```bash
cleat-gen-plugin --manifest plugins/myplugin/plugin.json --lang go --out plugins/myplugin/host_functions.gen.go
```

This generates a file like:

```go
// Auto-generated from plugin manifest: myplugin v1.0.0
// Do not edit by hand.

package myplugin

import (
    "encoding/json"
    "fmt"
)

// Generated Registration and types for myplugin
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
    scope.Register(plugin.FuncOptions{Name: "do_thing"}, func(ctx context.Context, inputJSON string) (string, error) {
        var input DoThingInput
        if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
            return "", fmt.Errorf("myplugin.do_thing: invalid input: %w", err)
        }
        output, err := p.doThing(ctx, input)
        if err != nil {
            return "", err
        }
        outputJSON, err := json.Marshal(output)
        if err != nil {
            return "", fmt.Errorf("myplugin.do_thing: marshal output: %w", err)
        }
        return string(outputJSON), nil
    })
    return nil
}
```

### Step 3: Update RegisterHostFunctions

Replace your hand-written `RegisterHostFunctions` with the generated one. If
your existing implementation does more than just register functions (e.g.,
validates configuration, initializes state), move that setup to `Init()`.

Your `RegisterHostFunctions` should become a one-liner (or just call the
generated function):

```go
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
    // Generated code handles registration, JSON parsing, and marshaling.
    return p.registerGeneratedFunctions(scope)
}
```

### Step 4: Delete hand-written input/output types

If you had hand-written Go structs for input/output types in your plugin, they
can be replaced by the generated ones from the manifest. Remove the
duplicates.

### Step 5: Update the worker binary import

No changes needed here. The worker binary imports the plugin package the same
way as before:

```go
// cmd/cleat-worker/main.go (unchanged)
import (
    _ "github.com/cleat-team/cleat/plugins/myplugin"
)
```

### Step 6: Verify

```bash
go build ./...
go test ./plugins/myplugin/...
```

---

## 3. Migration path for SDK wrappers

### Step 1: Write or update the plugin manifest

If you haven't already, write a `plugin.json` manifest for your plugin (see
[Step 1](#step-1-write-a-pluginjson-manifest) above).

### Step 2: Delete hand-written Plugins wrappers

For each SDK, remove the hand-maintained wrapper files:

**TypeScript:**

```bash
# Before: hand-written wrappers in plugins/
rm packages/cleat-as/assembly/plugins/myplugin.ts
```

**Python:**

```bash
rm python-sdk/cleat_sdk/plugins/myplugin.py
rm python-sdk/cleat_sdk/plugins/myplugin.pyi
```

**Rust:**

```bash
rm crates/cleat-sdk/src/plugins/myplugin.rs
```

### Step 3: Regenerate from manifest

```bash
cleat-gen-plugin --manifest plugins/myplugin/plugin.json --lang typescript --out packages/cleat-as/assembly/plugins/myplugin.ts
cleat-gen-plugin --manifest plugins/myplugin/plugin.json --lang python --out python-sdk/cleat_sdk/plugins/myplugin.py
cleat-gen-plugin --manifest plugins/myplugin/plugin.json --lang rust --out crates/cleat-sdk/src/plugins/myplugin.rs
```

### Step 4: Verify SDK tests pass

```bash
# TypeScript
cd packages/cleat-as && npm test

# Python
cd python-sdk && pytest

# Rust
cd crates/cleat-sdk && cargo test
```

The generated wrappers should produce identical behavior to the hand-written
ones. If a test fails, check:

- Generated type names match what tests expect (check `toPascalCase` conversion)
- Optional field handling is correct (the generator uses `| undefined` /
  `Optional[T]` correctly)
- Array and map types generated correctly

---

## 4. Migration path for tenant isolation

### Step 1: Run the tenant roles migration

Apply migration 009, which creates the `create_tenant_role` and
`drop_tenant_role` PostgreSQL functions:

```bash
cleat migrate up
```

This adds:

- `admin.tenants` table (tenants with their UUIDs and metadata)
- `admin.tenant_api_keys` table (API keys for each tenant)
- `admin.tenant_plugins` table (which plugins each tenant has enabled)
- `admin.plugin_tables` table (which tables each plugin manages)
- `admin.tenant_roles` table (tracks which PostgreSQL role each tenant uses)
- `create_tenant_role(uuid)` function (creates a login role + schema for a
  tenant)
- `drop_tenant_role(uuid)` function (drops the role and schema)

### Step 2: Enable tenant roles (opt-in)

The per-tenant DB path is opt-in. Set the `--tenant-roles` flag on the worker
to enable it:

```json
{
  "tenant_roles": {
    "enabled": true,
    "default_max_connections": 5,
    "pool_eviction_ttl": "15m"
  }
}
```

Without this flag, the worker continues to use the existing single-role
connection pool. Existing tenants continue to work unchanged.

### Step 3: Provision roles for existing tenants

Run the provisioning command for each existing tenant:

```bash
cleat tenant provision-role <tenant-uuid>
```

This:

1. Creates a PostgreSQL login role `cleat_tenant_<uuid>` with a random password
2. Creates a schema `tenant_<uuid>` owned by the role
3. Grants SELECT/INSERT/UPDATE/DELETE on core tables (`workflow_instances`,
   `event_history`, etc.)
4. Sets `cleat.tenant_id` session variable for RLS enforcement
5. Records the role in `admin.tenant_roles`

### Step 4: Enable plugins for tenants

For each plugin each tenant uses:

```bash
cleat tenant enable-plugin <tenant-uuid> <plugin-name>
```

This calls `grant_plugin_to_tenant()` to grant access to the plugin's tables
in the tenant's schema.

### Step 5: Migrate plugin data to tenant schemas

If a plugin has existing data in `public.*` tables that should be moved to the
tenant's schema, migrate it:

```bash
cleat tenant migrate-data <tenant-uuid> <plugin-name>
```

This copies data from the plugin's old `public.*` tables to the tenant's schema
tables and verifies integrity.

### Step 6: Verify isolation

Create a test that confirms tenant isolation works:

```sql
-- As tenant A
SELECT * FROM workflow_instances;  -- should return only tenant A's workflows
```

```sql
-- As tenant B
SELECT * FROM workflow_instances;  -- should return only tenant B's workflows
```

```sql
-- Attempt cross-tenant access (should fail)
SELECT * FROM tenant_a1b2c3.some_table;  -- ERROR: permission denied for schema
```

### Rollback: disable tenant roles

If issues arise, disable tenant roles:

```json
{
  "tenant_roles": {
    "enabled": false
  }
}
```

The worker falls back to the owner connection for all queries. This is a
safe rollback because:

- No data is lost (the tenant schemas and roles remain in the database)
- The RLS policies remain in place but are no longer primary enforcement
- Plugin data in tenant schemas is accessible through the owner connection

To fully roll back after confirming no issues with the fallback:

```sql
DROP OWNED BY cleat_tenant_<uuid>;
DROP ROLE cleat_tenant_<uuid>;
```

---

## 5. Verification checklist

### For Go plugin migration

- [ ] `plugin.json` passes `cleat plugin validate`
- [ ] Generated `host_functions.gen.go` compiles with `go build ./...`
- [ ] Hand-written JSON parsing removed
- [ ] Hand-written input/output type structs removed (replaced by generated)
- [ ] Plugin tests pass: `go test ./plugins/<name>/...`
- [ ] Integration tests pass: `go test ./internal/plugin/...`
- [ ] Worker binary builds: `go build ./cmd/cleat-worker/...`

### For SDK wrapper migration

- [ ] Hand-written SDK wrappers deleted for all languages
- [ ] Generated SDK wrappers compile without errors
- [ ] All SDK tests pass (TypeScript, Python, Rust)
- [ ] Workflow examples using the plugin still compile and run

### For tenant isolation migration

- [ ] Migration 009 applied: `cleat migrate up`
- [ ] Tenant roles created for all existing tenants
- [ ] Plugin access granted for each tenant's enabled plugins
- [ ] `cleat tenant list` shows all tenants with roles
- [ ] `cleat plugin list` shows all installed plugins correctly
- [ ] Cross-tenant access test passes (tenant A can't see tenant B's data)
- [ ] Worker starts successfully with `--tenant-roles` flag
- [ ] Rollback plan tested (disable `--tenant-roles`, confirm everything works)

---

## 6. Rollback

### Plugin migration rollback

If the generated code has issues:

1. Delete the generated `host_functions.gen.go` file
2. Restore the hand-written `RegisterHostFunctions` from git:
   ```bash
   git checkout -- plugins/myplugin/myplugin.go
   ```
3. Rebuild the worker:
   ```bash
   go build ./cmd/cleat-worker/
   ```

### SDK wrapper rollback

1. Restore the hand-written wrappers from git:
   ```bash
   git checkout -- packages/cleat-as/assembly/plugins/myplugin.ts
   git checkout -- python-sdk/cleat_sdk/plugins/myplugin.py
   git checkout -- crates/cleat-sdk/src/plugins/myplugin.rs
   ```
2. Delete the generated files:
   ```bash
   rm packages/cleat-as/assembly/plugins/myplugin.ts  # generated one
   ```
3. Run SDK tests to verify.

### Tenant isolation rollback

The tenant roles system is opt-in. To disable:

```json
// In worker config:
{ "tenant_roles": { "enabled": false } }
```

The worker reverts to using the owner connection. The tenant roles remain in
the database and can be re-enabled later.
