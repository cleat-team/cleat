# Plugin Versioning Through Worker Pool Routing

## The Problem

Plugins are compiled into the worker binary. When you deploy a new worker with
plugin v2, all workflows execute with v2 — including in-flight ones that
started with v1. If v2 behaves differently, replay diverges.

Cleat's core pitch — "deploy workflows via INSERT, in-flight instances replay
against their exact original code" — breaks for plugins because plugins aren't
in the database. They're in the worker.

## The Solution

**Workers declare their plugin versions. Workflows record which plugins they
need. The scheduler only routes workflows to compatible workers.**

No plugin WASM. No forced backward compatibility. No separate VMs. Just the
scheduler doing what it already does — matching work to workers — with one
more filter.

### How it works

1. **Worker declares plugins at startup**: from the registry, constructs
   `{"slack-notify": "1.0.0", "blobstore": "2.0.0"}`.

2. **Workflow records plugin dependencies on first use**: when a workflow
   calls a plugin host function, the engine records `(plugin_name, version)`
   in the instance's `required_plugin_versions` column. Before the first
   plugin call, the column is empty — any worker can execute it.

3. **Scheduler matches**: `ClaimWorkflow` adds a compatibility check —
   the worker's plugin map must be a superset of the workflow's required
   plugins (JSONB containment).

4. **Drain and retire for upgrades**: deploy a new worker pool with plugin
   v2. Old pool keeps running with v1. In-flight workflows on old pool
   complete naturally. When old pool has zero in-flight workflows, shut it
   down.

### Concrete example

```
Time 0: Deploy worker pool A with slack-notify v1.0.0
        Worker declares: {"slack-notify": "1.0.0"}

Time 1: Workflow W starts on pool A
        W calls slack_send_message → engine records required_plugins = {"slack-notify": "1.0.0"}

Time 2: Deploy worker pool B with slack-notify v2.0.0
        Worker declares: {"slack-notify": "2.0.0"}

Time 3: Scheduler tries to claim W:
        - Pool B (v2.0.0): W needs v1.0.0, B has v2.0.0 → NOT COMPATIBLE → skip
        - Pool A (v1.0.0): W needs v1.0.0, A has v1.0.0 → COMPATIBLE → claim

Time 4: W completes on pool A. Pool A has zero in-flight workflows. Shut it down.

Time 5: New workflow W2 starts. No required plugins yet.
        Either pool can claim it. Pool B claims it.
        W2 calls slack_send_message → engine records required_plugins = {"slack-notify": "2.0.0"}
```

## Schema

```sql
-- Per-instance tracking of required plugin versions
ALTER TABLE workflow_instances 
ADD COLUMN required_plugin_versions JSONB NOT NULL DEFAULT '{}';
-- e.g., {"slack-notify": "1.0.0", "blobstore": "2.0.0"}
```

No new tables. No heartbeat protocol. The worker constructs its plugin map
in-memory and passes it to `ClaimWorkflow` as a parameter.

## ClaimWorkflow Query Change

```sql
UPDATE workflow_instances 
SET status = 'running', assigned_to = $1, heartbeat_at = now()
WHERE id = (
    SELECT id FROM workflow_instances 
    WHERE status = 'ready' 
      AND next_wake_at <= now() 
      AND tenant_id = $2 
      AND task_queue = ANY($3)
      -- Plugin compatibility: workflow's required plugins must be
      -- a subset of this worker's available plugins.
      -- Uses JSONB containment: a <@ b means "a is contained in b"
      AND (required_plugin_versions = '{}' 
           OR required_plugin_versions <@ $4::jsonb)
    ORDER BY created_at 
    LIMIT 1 
    FOR UPDATE SKIP LOCKED
) RETURNING ...
```

`$4` is the worker's plugin map as a JSONB string:
`'{"slack-notify": "1.0.0", "blobstore": "2.0.0"}'`

JSONB containment semantics:
- `'{}' <@ '{"slack-notify": "1.0.0"}'` → TRUE (empty set is always contained)
- `'{"slack-notify": "1.0.0"}' <@ '{"slack-notify": "1.0.0"}'` → TRUE (exact match)
- `'{"slack-notify": "1.0.0"}' <@ '{"slack-notify": "2.0.0"}'` → FALSE (version mismatch)
- `'{"slack-notify": "1.0.0"}' <@ '{"slack-notify": "1.0.0", "blobstore": "2.0.0"}'` → TRUE (superset)

This correctly prevents v2 workers from claiming v1-dependent workflows.

## Engine Integration

When a plugin host function is first called during workflow execution, the
engine records the dependency:

```go
// In execSession, when a plugin host function is called:
func (s *execSession) recordPluginDependency(pluginName, version string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    if s.requiredPlugins == nil {
        s.requiredPlugins = make(map[string]string)
    }
    
    if _, exists := s.requiredPlugins[pluginName]; !exists {
        s.requiredPlugins[pluginName] = version
        // Persist immediately so the scheduler sees it on next claim
        go s.store.UpdateRequiredPlugins(s.ctx, s.workflowID, s.requiredPlugins)
    }
}
```

On the first call to a plugin host function, the dependency is recorded.
On subsequent calls to the same plugin (same version), nothing happens.
If a workflow never calls a plugin, `required_plugin_versions` stays `{}`.

## Worker Plugin Map

```go
// In cmd/durable-worker/main.go or internal/plugin/registry.go
func GetWorkerPluginMap() map[string]string {
    plugins := make(map[string]string)
    for _, info := range plugin.List() {
        plugins[info.Name] = info.Version
    }
    return plugins
}
```

Passed to ClaimWorkflow as a `jsonb` parameter:
```go
pluginJSON, _ := json.Marshal(plugin.GetWorkerPluginMap())
wf, err := store.ClaimWorkflow(ctx, workerID, tenantID, taskQueues, string(pluginJSON))
```

## What This Enables

### Plugin upgrades with zero downtime
1. Build new worker binary with plugin v2
2. Deploy new worker pool (K8s Deployment, Helm upgrade with new image tag)
3. Old pool continues processing v1-dependent workflows
4. New pool picks up new workflows (and old workflows that haven't called the plugin yet)
5. Old pool drains naturally
6. Shut down old pool when in-flight count reaches zero

### Plugin rollback
Same process in reverse. Deploy workers with old plugin version. New
workflows that already depend on the newer version stay on the newer pool
until they complete.

### Plugin removal
1. Remove the plugin import from the worker
2. Deploy new pool without the plugin
3. Old pool handles workflows that still need it
4. Once those workflows complete, old pool is retired
5. Future workflows can't use the removed plugin (host function won't be registered)

### Per-tenant plugin versions (future)
If tenant A needs plugin v1 and tenant B needs plugin v2, deploy two worker
pools — one with v1, one with v2. Both pools claim from the same `task_queue`
but filter by their respective plugin maps. The scheduler naturally routes
v1-dependent workflows to pool A and v2-dependent workflows to pool B.

## Limitations

### Version matching is exact, not ranged
`"1.0.0" != "1.1.0"` even if they're backward compatible. This is conservative
but correct — we'd rather refuse to claim than risk divergence. If a plugin
author guarantees backward compatibility, they should not bump the version
number for compatible changes, or they should implement ranged matching later.

### Workers must declare all compiled-in plugins
Even plugins that don't add host functions (like rate-limiters or dashboard
widgets) appear in the worker's plugin map. This is fine — it just means the
worker's declared map is slightly larger, which doesn't affect scheduling
(superset is always compatible).

### Plugin dependencies on other plugins aren't tracked
If plugin A depends on plugin B, and both are compiled into the worker, the
worker declares both. If a workflow only uses A's host functions, only A
appears in `required_plugin_versions`. B is implicitly available because the
worker declared it. This works because plugins are compiled together — you
can't have A without B in the same worker binary.

## Cost

One new column (`required_plugin_versions JSONB`). One additional WHERE
clause in `ClaimWorkflow`. One call to `UpdateRequiredPlugins` on first
plugin use per workflow. No new tables. No heartbeat changes. No new
infrastructure.

## Relationship to Workflow Versioning

Workflow code versioning (WASM blobs in `workflow_defs`) and plugin versioning
(worker pool routing) are complementary:

| What | Where versioned | How matched |
|------|----------------|-------------|
| Workflow code | `workflow_defs.wasm_bytes` by `(name, version)` | `workflow_instances.def_version` loads exact blob |
| Plugin code | Compiled into worker binary | Scheduler matches `required_plugin_versions <@ worker_plugin_map` |

Workflows don't know or care about plugins until they call one. When they do,
the dependency is recorded. From that point on, the scheduler ensures they
run on a compatible worker. The workflow code (WASM) and the plugin code (Go)
are independently versioned but jointly scheduled.

This is the same guarantee Temporal provides for activities — an activity
implementation can change, but in-flight workflows keep using the old worker
pool. Cleat extends this pattern to plugins.
