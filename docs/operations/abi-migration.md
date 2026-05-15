# ABI migration guide

This guide covers migrating cleat workflow definitions between ABI (Application
Binary Interface) versions. It describes what the ABI is, how versions are
tracked, and the step-by-step process for migrating workflows from ABI v4 to
ABI v5.

## Overview

The cleat ABI defines the interface between compiled WASM workflow modules and
the cleat worker runtime. It includes:

- Host function signatures (import names, parameter layout, return encoding)
- Memory model (linear memory layout, buffer sizes for string I/O)
- Error code conventions
- SDK version expectations

Each workflow definition stored in `workflow_defs` carries an `abi_version`
column. When a workflow instance is replayed, the engine loads the WASM blob
recorded at its `def_version` and executes it using the host functions the
current worker provides. Compatibility between the worker's host function
implementation and the workflow's compiled ABI version is critical for correct
execution.

## ABI versioning and engine releases

| ABI version | cleat engine release | Notable changes |
|-------------|---------------------|-----------------|
| v1          | cleat 0.1.x         | Initial ABI: `DurableCall`, `DurableSleep`, `DurableAwaitSignals`, `DurableLog`, `Now`, `Version`, `MinVersion` |
| v2          | cleat 0.2.x         | Added `ChildWorkflow`, `AwaitChild`, `ContinueAsNew`, `PollCancellation`, `PollSignal`, `CreatePromise`, `AwaitPromise` |
| v3          | cleat 0.3.x         | Added `DurableCallWithRetry`, `DurableCallWithHeartbeat`, `AwaitAllChildren`, `RegisterUpdateHandler` |
| v4          | cleat 0.4.x         | Added `PluginCall`, `PluginCallStreaming`, `SideEffect`, `AcquireLock`, `ReleaseLock`, `SetQueryState` |
| v5          | cleat 0.5.x         | Host function calling convention change: `//go:wasmimport` direct imports replace function pointer indirection. Changed event history checksum algorithm to BLAKE3. |

## Compatibility matrix

The following table shows which engine versions can load and execute workflow
definitions at each ABI version.

| Engine version | ABI v1 | ABI v2 | ABI v3 | ABI v4 | ABI v5 |
|---------------|--------|--------|--------|--------|--------|
| cleat 0.1.x   | Yes    | No     | No     | No     | No     |
| cleat 0.2.x   | Yes    | Yes    | No     | No     | No     |
| cleat 0.3.x   | Yes    | Yes    | Yes    | No     | No     |
| cleat 0.4.x   | Yes    | Yes    | Yes    | Yes    | No     |
| cleat 0.5.x   | Yes    | Yes    | Yes    | Yes    | Yes    |

**Key principle**: cleat maintains backward ABI compatibility within each major
engine release. A cleat 0.5.x worker can load and execute workflow modules
compiled against ABI v1-v5. Forward compatibility (loading a v5 module on a
v4 worker) is not supported.

## Step-by-step migration: ABI v4 to ABI v5

### Prerequisites

Before starting the migration:

- [ ] All workers are running cleat 0.5.x or later (support both ABI v4 and v5)
- [ ] You have a staging environment that mirrors production
- [ ] You have tested the migration in staging first
- [ ] No workflow instances are actively being migrated during the procedure
      (new instances can start; existing instances continue at their current
      ABI version)
- [ ] Rollback plan is prepared (see [Rollback procedure](#rollback-procedure))

### Step 1: Deploy the new engine (supports both old and new ABI)

Deploy cleat 0.5.x workers using a blue/green deployment (see
[zero-downtime-deploy.md](zero-downtime-deploy.md)). The new engine can execute
workflows compiled at any ABI version v1-v5.

```bash
# Deploy cleat 0.5.x workers alongside existing 0.4.x workers
cleat-worker-v0.5.x --db "$DATABASE_URL" --concurrency 20
```

During this phase:

- Existing ABI v4 workflows continue to execute on either worker pool
- New workflows deployed with the old SDK (compiling to ABI v4) still work
- No behavior change is visible to running workflows

### Step 2: Migrate workflows to the new ABI version

Re-deploy each workflow definition using a cleat 0.5.x SDK (Go, Rust, Python,
or AssemblyScript). The SDK compiles the workflow WASM module with ABI v5.

```bash
# Build workflow with the new SDK
cd my-workflow
cleat build --sdk go-0.5.x -o order_processor.wasm

# Deploy the new version (creates a new version in workflow_defs)
cleat deploy order_processor order_processor.wasm
```

Each deploy creates a new `def_version` in `workflow_defs` with `abi_version=5`.
New workflow instances use the latest deployed version. Existing instances at
older versions continue to execute at their current ABI version.

### Step 3: Verify with a canary workflow

Before migrating all workflows, run a canary:

```bash
# Deploy a single non-critical workflow with the new ABI
cleat deploy canary-workflow canary.wasm

# Start a test instance
cleat start canary-workflow '{"test": true}'

# Monitor execution
cleatctl versions list canary-workflow
cleatctl versions active

# Verify the workflow completes successfully
curl http://localhost:8080/api/workflows?status=completed
```

Check that:
- The new version shows `abi_version=5` in `cleatctl versions list`
- The workflow executes and produces correct results
- Event history records are generated correctly
- Plugin calls (if any) work correctly across the ABI boundary

### Step 4: Migrate remaining workflows

Repeat Step 2 for each workflow definition. The order does not matter -- each
workflow definition is independent.

```bash
# Deploy all workflows with the new SDK
for wf in order-processor payment-handler notification-sender; do
    cleat build --sdk go-0.5.x -o "${wf}.wasm" "./cmd/${wf}"
    cleat deploy "${wf}" "${wf}.wasm"
done
```

### Step 5: Verify all workflows

```bash
# Check that no workflows remain at ABI v4
cleatctl versions list | grep "ABI: 5"

# Verify active instance counts are healthy
cleatctl versions active

# Check for any stale or deprecated versions
curl http://localhost:8080/api/versions/stale
```

### Step 6: Deprecate old ABI v4 versions

Once all new workflow instances are running at ABI v5 and there are no active
instances at ABI v4, deprecate the old versions:

```bash
# Deprecate each old version
cleatctl versions deprecate order-processor 3
cleatctl versions deprecate payment-handler 2
```

Use the GC command to clean up deprecated versions after the grace period:

```bash
# Run garbage collection (removes deprecated versions older than 7 days)
cleatctl versions gc
```

### Step 7: Decommission old workers

After all workflow definitions have been migrated to ABI v5 and no instances
remain at older ABI versions, you can remove the old worker pool. The cleat
0.5.x workers handle everything.

```bash
# Drain old workers
curl -X POST http://old-worker:8080/api/admin/drain

# Remove old worker pool
# (Orchestrator-dependent: scale to zero, remove deployment, etc.)
```

## Breaking changes checklist

Review each item when migrating workflow code to a new ABI version.

### ABI v4 to v5

- [ ] **Event history checksums**: The checksum algorithm changed to BLAKE3.
  Existing event history records with SHA-256 checksums are verified during
  replay using the original algorithm. New events use BLAKE3. No action needed
  for existing records, but if you have external tooling that reads or validates
  event history checksums, update it to handle both algorithms.
- [ ] **Host function calling convention**: ABI v5 uses `//go:wasmimport`
  directives instead of function pointer tables. Workflow code must be
  recompiled with the v5 SDK. Old compiled WASM modules will not work on v5
  workers without recompilation.
- [ ] **Error codes**: No changes to error code semantics between v4 and v5.
- [ ] **Plugin host functions**: The `PluginCall` and `PluginCallStreaming`
  signatures are unchanged. Plugins do not need to be recompiled.
- [ ] **Memory model**: No changes to linear memory layout or buffer sizes.
- [ ] **Signal correlation IDs**: No changes.

### General checklist (all ABI upgrades)

- [ ] Recompile all workflow WASM modules with the new SDK
- [ ] Verify that all plugin dependencies are compatible
- [ ] Test in staging before production
- [ ] Prepare rollback plan (see below)
- [ ] Update CI/CD pipelines to use the new SDK version
- [ ] Verify monitoring and alerting still works after migration

## cleatctl commands for ABI management

### List all versions with ABI info

```bash
# Show all workflow definitions with ABI version
cleatctl versions list

# Show versions for a specific workflow
cleatctl versions list order-processor
```

Example output:

```
Workflow: order-processor
Version  ABI  MinVer  Deprecated  Active  Created
5        5    4       false       12      2025-06-01
4        4    3       false       3       2025-05-15
3        4    2       true        0       2025-04-01
```

### Check active instances by ABI version

```bash
# Show active instance counts grouped by (workflow, version)
cleatctl versions active

# Find deprecated versions ready for cleanup
curl http://localhost:8080/api/versions/stale
```

### Identify workflows at specific ABI versions

You can query the database directly to find workflows at a given ABI version:

```sql
-- List all versions at ABI v4
SELECT name, version, abi_version, deprecated, created_at
FROM workflow_defs
WHERE abi_version = 4
ORDER BY name, version;

-- Count active instances by ABI version
SELECT d.abi_version, COUNT(i.id) AS active_instances
FROM workflow_instances i
JOIN workflow_defs d ON d.name = i.def_name AND d.version = i.def_version
WHERE i.status IN ('ready', 'running')
GROUP BY d.abi_version
ORDER BY d.abi_version;
```

### Garbage collect old versions

```bash
# Preview what would be removed
cleatctl versions gc --dry-run

# Remove deprecated versions older than the stale threshold (7 days default)
cleatctl versions gc
```

## Rollback procedure

If the ABI migration causes issues, roll back by reverting the workflow
definition versions and restoring the old worker pool.

### Step 1: Restore old worker pool

If you removed the old (ABI v4-compatible) worker pool, bring it back:

```bash
# Start old workers alongside the new pool
cleat-worker-v0.4.x --db "$DATABASE_URL" --concurrency 20
```

### Step 2: Roll back workflow definition versions

Use `cleat rollback` to point new instances back to the last known-good ABI v4
version:

```bash
# See available versions
cleat versions order-processor

# Rollback to the last ABI v4 version
cleat rollback order-processor 4
```

After rollback:
- New workflow instances use version 4 (ABI v4)
- Existing instances at version 5 continue until they complete
- No data loss -- version 5 remains in the database

### Step 3: Drain the new worker pool

Drain v5 workers so that new instances are claimed by v4 workers:

```bash
# Drain each new worker
for w in worker-a worker-b worker-c; do
    curl -X POST "http://${w}:8080/api/admin/drain"
done
```

### Step 4: Verify rollback

```bash
# Check that new instances are running at ABI v4
cleatctl versions active

# Verify workflow execution
curl http://localhost:8080/api/workflows?status=running
```

## Zero-downtime migration pattern

The ABI migration can be performed with zero downtime by using cleat's backward
ABI compatibility. The pattern is:

1. **Deploy new workers**: Start cleat 0.5.x workers alongside existing
   workers. Both pools handle any ABI version.

2. **Verify new workers**: Ensure the new pool is healthy and executing
   workflows correctly.

3. **Redeploy workflows**: Deploy new versions with ABI v5. New instances start
   at v5; existing instances continue at v4.

4. **Drain old workers**: Once all instances are at ABI v5, drain old workers.
   Any remaining v4 instances are reclaimed by new workers (which support ABI
   v4) and continue execution via replay.

5. **Remove old workers**: After the old pool is fully drained and all instances
   are migrated, remove old workers.

This pattern avoids any window where workflows cannot be executed because the
running pool supports both ABI versions.

```bash
# Phase 1: Deploy new pool (supports v4 and v5)
cleat-worker-v0.5.x --db "$DATABASE_URL" --concurrency 20

# Phase 2: Deploy workflows with v5 ABI
cleat deploy order-processor order-processor-v5.wasm

# Phase 3: Drain old pool (holds only v4 instances at this point)
curl -X POST http://old-worker:8080/api/admin/drain

# Phase 4: Remove old pool
# (Orchestrator-dependent)
```

## Related guides

- [Upgrading cleat](upgrading.md) -- worker binary upgrades, schema migrations,
  PostgreSQL upgrades, and rollback procedures
- [Zero-downtime deployment](zero-downtime-deploy.md) -- blue/green worker pool
  replacement with no downtime
- [Disaster recovery](disaster-recovery.md) -- recovery from full database
  restore, RPO/RTO, and cross-region failover
- [Deploying to production](deploying-to-production.md) -- configuration,
  monitoring, health checks, and graceful shutdown
