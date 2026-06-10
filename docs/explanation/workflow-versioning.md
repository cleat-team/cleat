# Workflow Versioning

Every compiled workflow is a versioned WASM artifact stored in the database.
Workflows run for days to months, and during that time code changes. A workflow
**must** continue executing against the exact code version it was started with,
and child workflows **must** get the correct version.

## How versions work

Workflow versions are monotonic integers (1, 2, 3, ...). Each `(name, version)`
pair identifies a specific WASM binary in the `workflow_defs` table. Workflow
instances are pinned to their `def_version` at creation time and never change.

Deploying a new version is `INSERT INTO workflow_defs`, not a service
deployment. Workers are a stable runtime that loads the right WASM blob for
each instance.

## Deployment tags (channels)

Tags let you label versions for different purposes:

| Tag | Convention | Example |
|-----|------------|---------|
| `stable` | Production default | The version serving live traffic |
| `canary` | Pre-release testing | New version getting a small % of traffic |
| `latest` | Always MAX(version) | Resolved automatically, not stored |
| Custom | A/B experiments | `experiment-b`, `redesign-v2` |

Tags are stored in the `workflow_tags` table. The UNIQUE constraint on
`(workflow_name, tag)` means each tag points to exactly one version per
workflow. Moving `stable` from v4 to v5 is an UPDATE.

```sql
-- Assign a tag
INSERT INTO workflow_tags (workflow_name, version, tag)
VALUES ('Payment', 3, 'stable')
ON CONFLICT (workflow_name, tag) DO UPDATE SET version = 3;

-- Move the stable tag to a new version
UPDATE workflow_tags SET version = 5
WHERE workflow_name = 'Payment' AND tag = 'stable';
```

Or use `cleatctl`:
```
cleatctl versions tag Payment 3 stable
cleatctl versions untag Payment stable
cleatctl versions tag Payment 5 stable  # move the tag
```

## Child workflow binding policies

When a parent workflow spawns a child, the engine resolves which version of the
child to use. The policy is embedded in the parent's WASM metadata at build
time (in the `cleat.metadata` custom section).

### Available policies

| Policy | Behavior | Use case |
|--------|----------|----------|
| `frozen` | Use the exact versions pinned at build time. Never resolve at runtime. | **Production stability**: old parent always uses old children, even when newer child versions exist |
| `stable` | Resolve to whichever version currently has the `stable` tag at child creation time. | **Managed rollouts**: move the `stable` tag to upgrade all parents at once |
| `latest` | Always use the highest non-deprecated version at runtime. | **Development**: always pick up latest changes |
| `tag:X` | Always resolve to the version with tag `X`. | **A/B testing**: `tag:experiment-b` or `tag:canary` |
| (empty) | Backwards-compatible default. If `ChildVersions` is populated → `frozen`. Otherwise → `latest`. | Existing workflows without a policy |

### Resolution priority

When creating a child workflow:

1. **Explicit version** — `ChildWorkflowOptions{Version: N}` in workflow code (overrides everything)
2. **Runtime override** — `CLEAT_CHILD_BINDING_OVERRIDE=latest` env var or `--child-binding-override` flag
3. **Binding policy** — from WASM metadata (`frozen`, `stable`, `latest`, `tag:X`)
4. **Fallback** — database resolves to `MAX(version)` where `NOT deprecated`

### Setting the policy at build time

```
# Production: pin to whatever is tagged "stable" right now
cleat build --channel stable --tenant <tenant-uuid>

# Development: always use latest at runtime
cleat build --channel latest

# Canary: follow the "canary" tag
cleat build --channel canary --tenant <tenant-uuid>

# Custom tag
cleat build --channel experiment-b --tenant <tenant-uuid>
```

The `--channel` flag determines both:
- Which versions are resolved into `ChildVersions` in the WASM metadata
- What `ChildBindingPolicy` is embedded (controls runtime behavior)

**Default**: `--channel stable` when `--db` is provided, `--channel latest` otherwise.

The `--tenant` flag (or `CLEAT_TENANT_ID` env var) scopes all build-time database queries
to a specific tenant. This is required in multi-tenant installations so that tag resolution
(e.g., "what version has the `stable` tag?") returns the correct answer for the target tenant.
When omitted, the zero UUID is used (suitable for single-tenant setups).

### How the lock file works

`cleat build --channel stable` writes a `cleat.lock` file:
```json
{
  "version": 2,
  "policy": "stable",
  "entries": {
    "Payment": 3,
    "Inventory": 2,
    "Notification": 7
  }
}
```

The lock file is a snapshot of what was resolved at build time. It gives you
reproducible builds without a database connection. Commit it to version control.

With policy `"frozen"`, the lock file entries are authoritative — the child
version is exactly what was pinned. With policy `"stable"`, the entries record
what was stable at build time (for auditing), but runtime resolution re-queries
the `stable` tag so you can move the tag without rebuilding.

## Use cases

### 1. Development: always pick up latest child changes

```
cleat build --channel latest
```

The parent's WASM metadata gets `child_binding_policy: "latest"`. At runtime,
every `ChildWorkflow("Payment", ...)` call resolves to the highest-numbered
non-deprecated version. No lock file needed.

To temporarily force latest without rebuilding:
```
cleat-worker --child-binding-override=latest
# or
export CLEAT_CHILD_BINDING_OVERRIDE=latest
```

This is scoped to the worker process — useful for debugging a single developer's
environment without affecting other workers.

### 2. Production: old parent uses old children (frozen)

```
cleat build --channel stable
# Then edit the cleat.lock and change policy to "frozen" before embedding
```

Or set the policy explicitly at build time (future CLI option):
```
cleat build --channel stable --binding-policy frozen
```

With `"frozen"`, the parent workflow compiled as version 1 will always spawn
child `Payment` version 3 (what was pinned at build time), even after
`Payment` v5 is deployed. This guarantees that an old parent runs all the way
through using the child versions it was tested with.

### 3. Managed rollout: move the stable tag

```
# Deploy new child version
cleat deploy Payment --wasm payment-v5.wasm

# Test it as canary first
cleatctl versions tag Payment 5 canary

# Build a canary parent that follows the canary tag
cleat build --channel canary

# After validation, promote to stable
cleatctl versions tag Payment 5 stable
```

Parents built with `--channel stable` and `"stable"` policy automatically pick
up the new version next time they spawn a child — no rebuild needed.

### 4. A/B testing

Deploy two versions and tag them:
```
cleatctl versions tag Payment 4 experiment-a
cleatctl versions tag Payment 5 experiment-b
```

Build two parent variants:
```
cleat build --channel experiment-a -o parent-a.wasm
cleat build --channel experiment-b -o parent-b.wasm
```

At the top level, use routing rules for traffic splitting:
```
cleatctl routing set Payment 4 --weight 0.9   # 90% to v4
cleatctl routing set Payment 5 --weight 0.1   # 10% to v5
```

When starting workflows via the API, the system evaluates routing rules and
picks a version by weighted random selection.

### 5. Debugging a stuck workflow

If a production workflow is using an old child version and you suspect a bug
is fixed in the latest:

```
# Restart the worker with the override
cleat-worker --child-binding-override=latest
```

Or for a specific workflow start:
```
curl -X POST /api/workflows/OrderProcessor/start \
  -H "X-Cleat-Child-Binding: latest" \
  -d '{"input": {...}}'
```

This forces all child workflows to use the latest version, overriding whatever
policy is in the WASM metadata. Remove the override when done debugging.

## Tenant isolation

All versioning data is tenant-scoped:
- `workflow_defs` is isolated by tenant (via RLS in Postgres, tenant column in MySQL)
- `workflow_tags` follows the same tenant isolation
- `workflow_routing` is tenant-scoped
- `workflow_instances.def_version` is immutable per-instance

A tag set in one tenant does not affect another tenant. This means `stable` in
tenant-A's Payment workflow can point to v3 while tenant-B's points to v5.

## Plugin versions are separate

Workflow versions and plugin versions are different concepts. Plugins use
semver (e.g., `>=1.2.0`) because they're consumed as libraries with
compatibility ranges. Workflow versions use monotonic integers because
they're discrete versions of a business process. See
[plugin-system.md](plugin-system.md) for plugin versioning.

## Version lifecycle

```
Deploy v1  →  Deploy v2  →  Deploy v3  →  Deprecate v1  →  Purge v1
                                                              (zero active instances)
```

- `cleatctl deploy workflow Payment --wasm payment.wasm` — deploys a new version
- `cleatctl versions deprecate Payment 1` — prevents new instances from using v1
- `cleatctl versions purge Payment 1` — permanently deletes the WASM binary
- `cleatctl versions gc` — garbage-collects versions with zero active instances

Deprecated versions are excluded from `latest` resolution but can still be
referenced explicitly or by tag.

## Continue-As-New

Long-running workflows can upgrade to a new version mid-flight:
```go
// In workflow code:
h.ContinueAsNewWithVersion(newInput, 3) // restart with version 3
```

This creates a new instance with `def_version=3`, carrying forward state.
The new instance runs the new code from that point. See the SDK docs for
details on state migration between versions.
