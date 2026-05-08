# Plugin System Implementation Plan

This plan implements the design described in `plugin-system-design.md`. It
is organized into 7 phases with dependencies. Each phase includes verification
criteria.

## Phase overview

| Phase | What | Dependencies | Estimated effort |
|-------|------|-------------|-----------------|
| 1 | Fix known bugs, wire up RLS | None | 3-4 days |
| 2 | Per-tenant PostgreSQL roles | Phase 1 | 4-5 days |
| 3 | Plugin manifest schema | None (runs in parallel with 1-2) | 2-3 days |
| 4 | Code generation from manifests | Phase 3 | 5-7 days |
| 5 | Third-party plugin CLI and index | Phase 3 | 3-4 days |
| 6 | Capability enforcement | Phase 2, Phase 3 | 3-4 days |
| 7 | Documentation, examples, migration guide | All | 2-3 days |

**Total: ~22-30 person-days**

---

## Phase 1: Fix Known Bugs and Wire Up RLS

### P1.1: Fix compaction dropping plugin_call events (Bug B1 from review)

`compaction.go` has no `EventTypePluginCall` case. Plugin call events are
misinterpreted as DurableCall events during compaction.

**Changes:**
- Add `EventCodePluginCall = 10` to the event type code map in `compaction.go`
- Add bidirectional mapping in both `eventTypeToCode` and `codeToEventType`
- Add `case EventTypePluginCall:` in `extractCompactionState` preserving
  `PluginName`, `PluginFunc`, `PluginInput`, `PluginOutput`, `PluginError`
- Add streaming variant `EventCodePluginCallStreamChunk = 11` with same treatment
- Write a compaction test that includes plugin_call events and verifies they
  survive the round-trip

**Files:** `internal/host/compaction.go`, `internal/host/compaction_test.go`

**Verification:** `go test ./internal/host/ -run TestCompactionWithPluginCalls -v` passes.

### P1.2: Fix migrations running after Init (Bug B2)

`plugin.LoadAll()` calls `Init()` before `RunMigrations()`. A plugin that
creates tables in migrations can't use them in `Init()`.

**Changes:**
- Split `LoadAll` into two explicit phases: `Discover()` then `RunMigrations()`
  then `InitAll()`
- Update `cmd/cleat-worker/main.go` to call them in that order
- The `Discover()` → `RunMigrations()` → `InitAll()` sequence is already
  partially separated; verify it's correct in the worker startup path

**Files:** `internal/plugin/registry.go`, `cmd/cleat-worker/main.go`

**Verification:** Test plugin with migrations that creates a table and queries
it in `Init()`. Currently this would fail; after the fix it succeeds.

### P1.3: Inject tenant context into plugin function calls (Bug B3)

`freshPluginCall` calls `fn(ctx, inputJSON)` with the raw WASM context. The
tenant ID from the workflow instance is not injected into the context.

**Changes:**
- In `internal/host/engine.go`, before calling the plugin function:
  ```go
  ctx = plugin.WithCallContext(ctx, plugin.CallContext{
      TenantID:   s.tenantID,
      WorkflowID: s.workflowID,
  })
  outputJSON, err := fn(ctx, inputJSON)
  ```
- `s.tenantID` comes from the workflow instance row. It's already loaded during
  `LoadInstance` — verify it's available in `execSession`.

**Files:** `internal/host/engine.go`

**Verification:** Plugin host function calls `plugin.CallContextFromContext(ctx)`
and receives the correct `TenantID` and `WorkflowID`. Add a unit test.

### P1.4: Fix double constructor call (Design issue D1)

`Register()` calls the constructor to get `Info()`, then `Discover()` calls it
again to get the instance. This was partially fixed by storing `PluginInfo`
alongside the constructor in `registryEntry`. The current code already does this.
Verify it's correct and add a test that constructor is called exactly once
during `Discover()`.

**Files:** `internal/plugin/registry.go`, `internal/plugin/plugin_test.go`

### P1.5: Fix "/" collision in lookup keys (Design issue D2)

`PluginRegistry.Lookup` uses `pluginName + "/" + funcName` as the key. Fix
to use `\x00` delimiter.

**Changes:**
- Change lookup key construction in `internal/host/` to use `\x00` separator
- Validate function names at registration time: reject names containing `\x00`
  and `/`

**Files:** `internal/host/plugin_registry.go`

### P1.6: Add duplicate function name detection (Design issue D3)

`FuncRegistry.Register` returns `error` but always returns nil.

**Changes:**
- In the adapter's `Register`, detect duplicate function names within a plugin
  and return an error

**Files:** `internal/host/plugin_registry.go`

### P1.7: Wire up RLS: set `cleat.tenant_id` on every connection

The RLS policies are in place but never activated. Wire them up.

**Changes:**
- Add a `SET LOCAL cleat.tenant_id = '<tenant_uuid>'` call before each query
  that runs on behalf of a tenant
- In `PostgresStore`, add `TenantID` field and call `SET LOCAL` in each method
  that operates on tenant-scoped data
- In the worker dispatch loop, set the tenant ID from the workflow instance
  onto the store before beginning execution

**Files:** `internal/host/db.go`, `cmd/cleat-worker/main.go`

**Verification:** Integration test: create two tenants, insert workflows for
both, run a query as tenant A and verify tenant B's workflows are invisible.
This test passes only if RLS is active.

---

## Phase 2: Per-Tenant PostgreSQL Roles

### P2.1: Role creation and GRANT infrastructure

**Changes:**
- New migration `009_tenant_roles.sql`:
  ```sql
  -- Create a role per tenant. Called when a tenant is provisioned.
  CREATE OR REPLACE FUNCTION create_tenant_role(tenant_uuid UUID) RETURNS void AS $$
  DECLARE
      role_name TEXT;
  BEGIN
      role_name := 'cleat_tenant_' || replace(tenant_uuid::text, '-', '_');
      
      EXECUTE format('CREATE ROLE %I', role_name);
      EXECUTE format('ALTER ROLE %I SET cleat.tenant_id = %L', role_name, tenant_uuid);
      
      -- Grant table access. RLS filters by tenant_id.
      EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON workflow_defs TO %I', role_name);
      EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON workflow_instances TO %I', role_name);
      EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON event_history TO %I', role_name);
      EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON workflow_signals TO %I', role_name);
      EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON workflow_schedules TO %I', role_name);
      EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON workflow_promises TO %I', role_name);
      
      -- Grant USAGE on sequences for serial/identity columns.
      -- (None currently; add if needed.)
  END;
  $$ LANGUAGE plpgsql;

  CREATE OR REPLACE FUNCTION drop_tenant_role(tenant_uuid UUID) RETURNS void AS $$
  DECLARE
      role_name TEXT;
  BEGIN
      role_name := 'cleat_tenant_' || replace(tenant_uuid::text, '-', '_');
      EXECUTE format('DROP ROLE IF EXISTS %I', role_name);
  END;
  $$ LANGUAGE plpgsql;
  ```
- Add `create_tenant_role` call to the tenant creation flow
- Add `drop_tenant_role` call to the tenant deletion flow

**Files:** `migrations/009_tenant_roles.sql`, `internal/auth/tenant.go` (or new file)

### P2.2: TenantDB wrapper

**Changes:**
- New file `internal/plugin/tenant_db.go`:
  ```go
  // TenantDB wraps a *sql.DB to enforce tenant-scoped access via SET ROLE.
  type TenantDB struct {
      ownerDB  *sql.DB
      tenantID uuid.UUID
      roleName string
  }

  // Implements a subset of *sql.DB — ExecContext, QueryContext, QueryRowContext.
  // Each call acquires a connection from the pool, sets the role, runs the
  // query, and resets the role.
  ```
- `TenantDB` implements the same interface as `*sql.DB` for the methods
  plugins actually use (ExecContext, QueryContext, QueryRowContext, PrepareContext)

**Files:** `internal/plugin/tenant_db.go`

### P2.3: Per-tenant pool in worker

**Changes:**
- In `cmd/cleat-worker/main.go`, maintain a map of `tenantID → *TenantDB`
- When a workflow is claimed for tenant X, retrieve or create the `TenantDB`
  for that tenant
- Pass the `TenantDB` (not the raw `*sql.DB`) into the execution context
  so that plugin host functions receive tenant-scoped database access
- `TenantDB` is cached per tenant. For deployments with many tenants, use a
  bounded cache with LRU eviction

**Files:** `cmd/cleat-worker/main.go`, `internal/host/engine.go`

### P2.4: Plugin table grants

When a tenant enables a plugin, grant access to the plugin's tables:

**Changes:**
- New function `grant_plugin_to_tenant(plugin_name TEXT, tenant_uuid UUID)`:
  ```sql
  CREATE OR REPLACE FUNCTION grant_plugin_to_tenant(plugin_name TEXT, tenant_uuid UUID) RETURNS void AS $$
  DECLARE
      role_name TEXT;
  BEGIN
      role_name := 'cleat_tenant_' || replace(tenant_uuid::text, '-', '_');
      
      -- Grant access to plugin-specific tables.
      -- Each plugin registers its tables in a plugin_tables metadata table.
      FOR table_name IN
          SELECT t.table_name
          FROM plugin_tables t
          WHERE t.plugin_name = grant_plugin_to_tenant.plugin_name
      LOOP
          EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO %I', table_name, role_name);
      END LOOP;
  END;
  $$ LANGUAGE plpgsql;
  ```
- Each plugin migration that creates tables also inserts into `plugin_tables`
- When a tenant is deleted, all their plugin grants are revoked by dropping the
  role (cascade)

**Files:** `migrations/009_tenant_roles.sql`, each plugin's migration

### P2.5: Identity for the worker's owner connection

The worker connects to PostgreSQL as a role that:
- Owns all core tables (can run DDL during migrations)
- Can `SET ROLE` to any tenant role
- Can call `create_tenant_role` and `drop_tenant_role`

This role does NOT execute workflow or plugin code. It only handles:
- Migrations
- Workflow claiming (the `ClaimWorkflow` query runs as owner)
- Tenant provisioning
- Admin API endpoints

Workflow execution and plugin host functions run as the tenant role.

**Files:** deployment docs, `cmd/cleat-worker/main.go`

---

## Phase 3: Plugin Manifest Schema

### P3.1: JSON Schema for plugin manifests

**Changes:**
- New file `schemas/plugin-manifest.schema.json`:
  JSON Schema (draft-2020-12) that validates the manifest format described in
  the design doc. Covers:
  - Required fields: `name`, `version`, `description`, `author`
  - `host_functions`: map of function name to `{description, input, output, idempotent}`
  - `types`: map of type name to `{fields: {name: {type, description, optional}}}`
  - `capabilities`: `{database, start_workflow, signal_workflow, http_routes,
     http_middleware, background_worker, call_plugin}`
  - Version constraint: `min_cleat_version`
  - Plugin naming: enforce `org/name` format for third-party, `name` for
    official

**Files:** `schemas/plugin-manifest.schema.json`

### P3.2: Manifest validation in CLI

**Changes:**
- Add `cleat plugin validate --manifest plugin.json` that validates against the
  schema
- Run this validation in `cleat plugin build` (WASM compilation wrapper)
- Run this validation in the index when a PR is submitted

**Files:** new CLI subcommand in `cmd/cleat/`

### P3.3: manifest.yaml support

Many plugin authors prefer YAML for readability. Support both:

**Changes:**
- Accept `plugin.json` or `plugin.yaml`
- If both exist, `plugin.json` takes precedence (machine-generated trumps
  hand-written)
- Validation runs on the parsed structure regardless of source format

**Files:** manifest loading code

---

## Phase 4: Code Generation from Manifests

### P4.1: Manifest parser and type IR

**Changes:**
- New package `internal/plugingen/` or `cmd/cleat-gen-plugin/`:
  - Parse `plugin.json` into Go structs
  - Resolve type references (host function inputs/outputs reference named types)
  - Build an intermediate representation (IR) of the plugin's API surface

### P4.2: TypeScript code generator

**Changes:**
- From the IR, generate TypeScript code:
  - Type interfaces for every named type
  - Input/output interfaces for each host function
  - A `Plugins.<name>` wrapper class with typed methods
  - Import from `HostCalls` in the cleat-as package
- Output path: `packages/cleat-as/assembly/plugins/<plugin-name>.ts`
- Generated files are checked into the repo for official plugins
- Third-party plugins generate into the user's workflow project

### P4.3: Python code generator

**Changes:**
- Generate Python code with:
  - `@dataclass` types for every named type
  - Typed method signatures on the `Plugins` class
  - Type stubs (`.pyi`) for IDE support
- Output path: `python-sdk/cleat_sdk/plugins/<plugin_name>.py`

### P4.4: Rust code generator

**Changes:**
- Generate Rust code with:
  - `#[derive(Serialize, Deserialize)]` structs for every named type
  - Typed methods on the `Plugins` struct
- Output path: `crates/cleat-sdk/src/plugins/<plugin_name>.rs`

### P4.5: Go code generator (first-party plugins)

**Changes:**
- Generate Go code for the `HasHostFunctions` implementation:
  - Typed `PluginFunc` wrappers with JSON parsing/marshaling
  - Schema validation on input (opt-in via `--with-validation`)
- Output path: `plugins/<name>/host_functions.gen.go`

### P4.6: Regenerate all existing plugin wrappers

**Changes:**
- Write manifests for all existing plugins (llm, pgvector, blobstore, kvstore,
  etc.)
- Delete the hand-maintained `Plugins` wrapper code in each SDK
- Regenerate from manifests
- Run all SDK tests to verify the generated code is equivalent

**Verification:** All existing SDK tests pass with generated wrappers. No
behavior change — the types are the same, just generated instead of
hand-written.

### P4.7: Integration with `cleat build`

**Changes:**
- `cleat build` accepts `--plugin-sdk <path>` pointing to a directory of
  generated plugin wrappers
- For TypeScript projects, the generated wrappers are imported by the
  workflow code and compiled into the WASM binary
- The transformer doesn't need to change — `PluginCall` in the generated
  wrapper is the same `HostCalls.pluginCall()` it already handles

---

## Phase 5: Third-Party Plugin CLI and Index

### P5.1: `cleat plugin install`

**Changes:**
- New CLI subcommand: `cleat plugin install <name>@<constraint>`
  - Fetches the index (`cleat-plugins` repo, cached locally)
  - Resolves the semver constraint to the best matching non-deprecated version
  - Downloads the WASM binary from `wasm_url`
  - Verifies checksum against index
  - Displays capability declaration and security warning (for community plugins)
  - Prompts for confirmation
  - Inserts into local `plugin_defs` table via the worker's deploy endpoint
  - Also downloads the manifest and generates SDK wrappers locally (opt-in
    with `--generate-sdk`)

### P5.2: `cleat plugin list`

**Changes:**
- Lists installed plugins from `plugin_defs` table (name, version, deprecated,
  installed date)
- Option to list available plugins from the index (`--available`)

### P5.3: `cleat plugin update`

**Changes:**
- `cleat plugin update <name>` — shows available updates, highlights checksum
  changes, prompts for confirmation
- `cleat plugin update --all` — shows all available updates

### P5.4: `cleat plugin uninstall`

**Changes:**
- `cleat plugin uninstall <name> <version>` — deprecates the version in
  `plugin_defs`. Does not delete — in-flight workflows may still depend on it.
- Warns if there are active workflows using this plugin version

### P5.5: Plugin index repository

**Changes:**
- Create `github.com/rcownie/cleat-plugins` repository
- `index.yaml` with initial entries for all official plugins
- `README.md` explaining how to submit a plugin
- GitHub Actions workflow that validates every PR:
  - `plugin.json` passes JSON Schema validation
  - Checksum in the PR matches the binary at `wasm_url`
  - Plugin name is not `cleat/*` unless submitted by a maintainer
  - Plugin name doesn't collide with an existing entry

### P5.6: `cleat plugin build` for plugin authors

**Changes:**
- `cleat plugin build --manifest plugin.json --source ./... --output plugin.wasm`
  compiles a Go plugin to WASM and bundles the manifest
- `cleat plugin publish --manifest plugin.json --version 0.1.0` guides the
  author through publishing (GitHub release + PR to index)

---

## Phase 6: Capability Enforcement

### P6.1: Capability struct and validation

**Changes:**
- New type `Capabilities` in `internal/plugin/`:
  ```go
  type Capabilities struct {
      Database        bool     `json:"database"`
      StartWorkflow   bool     `json:"start_workflow"`
      SignalWorkflow  bool     `json:"signal_workflow"`
      HTTPRoutes      bool     `json:"http_routes"`
      HTTPMiddleware   bool     `json:"http_middleware"`
      CallPlugin      []string `json:"call_plugin"`
      BackgroundWorker bool    `json:"background_worker"`
  }
  ```
- Go compile-time plugins derive capabilities from the optional interfaces they
  implement. `HasRoutes` → `HTTPRoutes: true`. `HasBackground` →
  `BackgroundWorker: true`. `HasHostFunctions` → `Database: true` (currently
  all host functions get DB access; this can be refined later).
- WASM plugins declare capabilities in `plugin.json`.
- Validation function: `ValidateCapabilities(declared, granted Capabilities) error`
  that returns a clear error if a plugin tries to use a capability it hasn't
  been granted.

### P6.2: Enforce in the engine

**Changes:**
- In `internal/host/engine.go`, before calling a plugin host function, verify
  the plugin's declared capabilities allow it
- If a plugin declares `call_plugin: ["llm"]` and calls `slacknotify.send`,
  reject the call
- If a plugin declares `database: false` but the host function implementation
  tries to use the DB, the `TenantDB` wrapper is nil and the call panics with
  a clear message (fail-closed)

### P6.3: Enforce at load time for WASM plugins

**Changes:**
- When loading a WASM plugin, check its declared capabilities against a
  permitted set. The operator configures which capabilities third-party
  plugins are allowed:
  ```json
  // In cleat-worker config:
  {
    "plugin_capability_limits": {
      "community": {
        "database": true,          // scoped to tenant role
        "start_workflow": false,   // deny by default
        "signal_workflow": true,
        "call_plugin": ["llm"],    // only allow calling these plugins
        "background_worker": false
      }
    }
  }
  ```
- A community plugin that declares `start_workflow: true` when the limit says
  `false` is refused at load time.

### P6.4: Enforce in the `TenantDB` wrapper and Environment construction

**Changes:**
- `plugin.Environment` construction checks capabilities:
  - `Database: false` → `DB` field is nil
  - `StartWorkflow: false` → `StartWorkflow` field is nil
  - `SignalWorkflow: false` → `SignalWorkflow` field is nil
- Plugins that try to use a nil field panic (fail-closed)

---

## Phase 7: Documentation, Examples, and Migration Guide

### P7.1: Third-party plugin authoring guide

**Changes:**
- New file `docs/third-party-plugin-guide.md`:
  - Step-by-step: write manifest, implement functions, build WASM, test, publish
  - Host function ABI reference (the `cleat_*` imports, their signatures, and
    stability guarantees)
  - Manifest format reference with examples
  - Testing workflow (test helper that mocks the host ABI)
  - Publishing workflow (GitHub release + PR to index)
  - Capability declaration guidance
  - Community plugin naming convention (`org/name`)

### P7.2: Operator security guide

**Changes:**
- New section in `docs/plugin-developer-guide.md` or separate
  `docs/plugin-security.md`:
  - How tenant roles work
  - How to configure capability limits for third-party plugins
  - How to audit installed plugins and their capabilities
  - How to approve plugin upgrades
  - How to run a private plugin index for internal plugins
  - Incident response: what to do if a plugin is compromised

### P7.3: Example third-party plugin

**Changes:**
- New directory `examples/third-party-plugin/`:
  - `plugin.json` manifest
  - `main.go` plugin implementation (~100 lines)
  - `Makefile` showing `cleat plugin build` and `cleat plugin publish`
  - README explaining the workflow

### P7.4: Migration guide for existing plugins

**Changes:**
- Document how existing first-party plugins migrate to the manifest system:
  - Write `plugin.json`
  - Delete hand-written `Plugins` wrappers
  - Run `cleat plugin generate-sdk` to regenerate
  - Verify tests pass
- Document how existing tenants get PostgreSQL roles:
  - Migration `009` creates roles for all existing tenants
  - Worker update is backward-compatible (the per-tenant DB path is opt-in
    via a `--tenant-roles` flag initially)

### P7.5: End-to-end integration test

**Changes:**
- New test in `internal/plugin/integration_test.go` (or similar):
  1. Create two tenants with separate roles
  2. Deploy a workflow for tenant A that uses a plugin
  3. Deploy a workflow for tenant B that uses a different plugin
  4. Execute both workflows concurrently
  5. Verify:
     - Tenant A's workflow can't see tenant B's data
     - Tenant A's workflow can't query tables for plugins tenant A hasn't enabled
     - Plugin A can't call plugin B's host functions (capability enforcement)
     - RLS is active (query without tenant filter returns only own data)
```

---

## Risk Register

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| `TenantDB` wrapper introduces latency per-query | Medium | Medium | Benchmark; `SET ROLE` is a few microseconds. If it's a problem, use a connection pool with pre-set roles |
| Existing tenants break when roles are enforced | Medium | High | Phase 2 introduces roles as opt-in. Default behavior (no role switching) continues to work. Rollout is gradual |
| Manifest type system is too restrictive for some plugins | Low | Medium | Plugins can use raw `string` input/output and document their format. Manifest types are opt-in sugar |
| Third-party WASM plugins have worse performance than Go compile-time | Medium | Low | Hot-path plugins stay as Go compile-time. WASM is for community plugins where distribution trumps performance |
| Checksum verification in the index is bypassed by a compromised GitHub release | Low | High | Require checksum confirmation on upgrade; display diff from previous version. Future: signed manifests |
| Code generation diverges from hand-maintained wrappers | Medium | Medium | Phase 4.6 regenerates all existing wrappers and runs the full SDK test suite before merging |
