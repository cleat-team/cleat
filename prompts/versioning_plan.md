# Cleat Workflow Versioning Plan

**Generated:** 2026-05-07
**Status:** Draft for review

---

## 1. Executive Summary

Workflows run for days to months. During that time, code and plugins change.
A workflow **must** continue executing against the exact code version it was
started with, and child workflows **must** get the correct version. This plan
builds on the existing WASM-loadable-module design in `wasm-demo/` and extends
it to cover plugins, child workflows, and migration paths.

The core insight: WASM makes workflow code a **versioned data artifact** stored
in the database. Deploying a new version is `INSERT INTO workflow_defs`, not a
service deployment. Workers are a stable runtime that loads the right WASM blob
for each instance.

---

## 2. What Already Exists

The codebase at `wasm-demo/worker/versioned_loader.go` and
`wasm-demo/cluster/versioning/main.go` already defines:

| Component | Status | Location |
|-----------|--------|----------|
| `WorkflowDef` table (name, version, wasm_bytes) | Designed, demo | `versioned_loader.go:27-33` |
| `WorkflowInstance` pinned to (def_name, def_version) | Designed, demo | `versioned_loader.go:37-44` |
| `WorkflowLoader.Load(name, version)` with LRU cache | Designed, demo | `versioned_loader.go:98-117` |
| `WorkflowLoader.Deploy(name, version, wasmBytes)` | Designed, demo | `versioned_loader.go:78-88` |
| Active version tracking for GC | Designed, demo | `versioned_loader.go:125-143` |
| Host interface versioning strategy | Documented | `versioning/main.go:163-198` |
| SDK `Version()` / `MinVersion()` host calls | Implemented | `durable/runtime.go:172-178` |
| ABI versioning (separate from workflow version) | Specified | `ABI.md:7` |
| Temporal comparison (task queues vs WASM loading) | Documented | `versioning/main.go` |

**What's missing:** plugin versioning, child workflow version resolution,
SDK-level migration APIs, version compatibility declarations, and the
host-side implementation of the loader against a real wazero runtime.

---

## 3. Design

### 3.1 Database Schema

```
┌──────────────────────────────────────────────┐
│                 workflow_defs                 │
├──────────────────────────────────────────────┤
│ name         TEXT     NOT NULL                │
│ version      INTEGER  NOT NULL                │
│ wasm_bytes   BYTEA    NOT NULL                │
│ abi_version  INTEGER  NOT NULL DEFAULT 1      │
│ plugin_deps  JSONB    NOT NULL DEFAULT '{}'   │
│   -- {"llm": ">=1.2.0", "blobstore": "~2.0"} │
│ min_version  INTEGER  NOT NULL DEFAULT 1      │
│   -- minimum compatible def version           │
│   -- used by child workflows to validate      │
│ created_at   TIMESTAMPTZ NOT NULL             │
│ deprecated   BOOLEAN   NOT NULL DEFAULT false │
│                                              │
│ PRIMARY KEY (name, version)                   │
└──────────────────────────────────────────────┘

┌──────────────────────────────────────────────┐
│              workflow_instances               │
├──────────────────────────────────────────────┤
│ id           TEXT     NOT NULL                │
│ def_name     TEXT     NOT NULL                │
│ def_version  INTEGER  NOT NULL                │
│ status       TEXT     NOT NULL                │
│ input        JSONB    NOT NULL                │
│ parent_id    TEXT     -- NULL for top-level   │
│ plugin_vers  JSONB    NOT NULL DEFAULT '{}'   │
│   -- resolved at start: {"llm": "1.2.0", ...}│
│ ...                                          │
│                                              │
│ FOREIGN KEY (def_name, def_version)           │
│   REFERENCES workflow_defs(name, version)     │
└──────────────────────────────────────────────┘

┌──────────────────────────────────────────────┐
│                 plugin_defs                   │
├──────────────────────────────────────────────┤
│ name         TEXT     NOT NULL                │
│ version      TEXT     NOT NULL  -- semver     │
│ wasm_bytes   BYTEA    -- NULL for host-native │
│ config       JSONB    NOT NULL DEFAULT '{}'   │
│ created_at   TIMESTAMPTZ NOT NULL             │
│ deprecated   BOOLEAN   NOT NULL DEFAULT false │
│                                              │
│ PRIMARY KEY (name, version)                   │
└──────────────────────────────────────────────┘
```

### 3.2 How a Workflow Gets the Right Version

```
                      START WORKFLOW
                            │
                            ▼
              ┌─────────────────────────┐
              │ 1. Resolve def_version   │
              │    If caller specified:  │
              │      use that            │
              │    Else:                 │
              │      SELECT MAX(version) │
              │      FROM workflow_defs  │
              │      WHERE name = $1     │
              │        AND NOT deprecated│
              └───────────┬─────────────┘
                          │
                          ▼
              ┌─────────────────────────┐
              │ 2. Resolve plugin vers  │
              │    Read plugin_deps from│
              │    workflow_defs row    │
              │    Resolve each dep to  │
              │    best matching plugin │
              │    version (semver)     │
              │    Store resolved vers  │
              │    in instance row      │
              └───────────┬─────────────┘
                          │
                          ▼
              ┌─────────────────────────┐
              │ 3. INSERT instance row  │
              │    with def_version +   │
              │    resolved plugin_vers │
              └───────────┬─────────────┘
                          │
                          ▼
              ┌─────────────────────────┐
              │ 4. Worker claims task   │
              │    Loads WASM blob for  │
              │    (def_name, def_ver)  │
              │    Loads plugin WASM    │
              │    blobs for resolved   │
              │    plugin versions      │
              │    Instantiates wazero  │
              │    module with all deps │
              └─────────────────────────┘
```

### 3.3 Child Workflow Version Resolution

When a parent spawns a child workflow, the child's version is resolved by
the **first applicable** rule:

| Rule | Description | Example |
|------|-------------|---------|
| **Explicit pin** | Caller passes `version: 3` | `h.childWorkflow("Payment", input, {version: 3})` |
| **Parent's version** | Child uses same version as parent | Default for tightly-coupled workflows |
| **Latest compatible** | `SELECT MAX(version) FROM workflow_defs WHERE name = $1 AND min_version <= parent_version AND NOT deprecated` | For loosely-coupled services |
| **Pinned default** | Defined in workflow_defs.default_child_version | Override per workflow def |

**SDK surface (Go):**
```go
type ChildWorkflowOptions struct {
    Version int  // 0 = use default resolution
}

// HostCalls interface additions:
ChildWorkflowWithOptions(name string, inputJSON string, opts ChildWorkflowOptions) (runID string, err error)
```

**SDK surface (Python):**
```python
@dataclass
class ChildWorkflowOptions:
    version: int = 0  # 0 = default resolution

def child_workflow(self, name: str, input_json: str,
                   options: ChildWorkflowOptions = None) -> str:
    ...
```

### 3.4 Plugin Versioning

Plugins (LLM, blobstore, Slack, etc.) are versioned separately from
workflows. A workflow declares its plugin dependencies at compile time,
and the host resolves them at instance creation time.

**Workflow declares plugin deps** (in a manifest or code annotation):
```go
//go:cleat-plugin llm >=1.2.0
//go:cleat-plugin blobstore ~2.0.0
```

**Host resolves at start time:**
1. Read `plugin_deps` from `workflow_defs` row
2. For each dependency, find best matching version from `plugin_defs`
3. Pin resolved versions in the `workflow_instances.plugin_vers` JSONB column
4. Worker loads both the workflow WASM and all plugin WASM modules

**Why pin at instance creation, not compile time:**
- A workflow instance may run for months
- Plugin v1.2.1 (patch) should be safe to pick up mid-flight
- Plugin v2.0.0 (major) must NOT be picked up mid-flight
- Pinning at instance creation freezes the resolved versions

**Compatibility semantics:**

| Constraint | Meaning | When to use |
|-----------|---------|-------------|
| `>=1.2.0` | Any version >= 1.2.0, including 2.x | Fully compatible API |
| `~1.2.0` | >=1.2.0, <1.3.0 (patch updates only) | Bug fixes OK, new features risk |
| `^1.2.0` | >=1.2.0, <2.0.0 (minor updates OK) | Backward-compatible API |
| `=1.2.0` | Exactly 1.2.0 | Pinned for compliance/audit |

### 3.5 In-Flight Workflow Migration (Continue-As-New Upgrade)

When a long-running workflow needs to upgrade to a new version:

```
  Old version (v1)                    New version (v3)
  ─────────────────                   ─────────────────
  step 1: ReserveInventory            step 1: ReserveInventory
  step 2: awaitApproval (7 days)      step 2: awaitApproval (picks up here)
  step 3: ChargePayment  ←──────────  step 3: ChargePayment (new logic)
  step 4: ShipOrder                   step 4: ShipOrder
                                      step 5: SendConfirmationEmail (NEW!)
```

**SDK surface:**
```go
// ContinueAsNewWithVersion restarts the workflow with new input and
// optionally a new version. If version is 0, uses the latest.
ContinueAsNewWithVersion(newInputJSON string, version int) error
```

Under the hood:
1. Workflow calls `continue_as_new(input, version=3)`
2. Host records a `ContinueAsNew` event in the event history
3. Host creates a NEW instance row with `def_version=3`, carrying forward
   any needed state (scoped state, promises, signals)
4. When the new instance starts, it replays the old event history up to
   the `ContinueAsNew` event, then executes the new code from that point

### 3.6 SDK Build-Time Version Stamping

Every compiled WASM module must carry its identity. The SDKs embed this at
build time:

**Go (tinygo):**
```go
// Injected via -ldflags at build time
var WorkflowName = "PlaceOrder"
var WorkflowVersion = 3
var MinCompatibleVersion = 1
var ABIVersion = 1
```

**Python (componentize-py):**
```python
# In pyproject.toml:
# [tool.cleat]
# workflow_name = "place_order"
# workflow_version = 3
# min_compatible_version = 1
```

**AS (asconfig.json):**
```json
{
  "cleat": {
    "workflowName": "place_order",
    "workflowVersion": 3,
    "minCompatibleVersion": 1
  }
}
```

**Rust (Cargo.toml):**
```toml
[package.metadata.cleat]
workflow_name = "place_order"
workflow_version = 3
min_compatible_version = 1
```

These are compiled into constants that the host can read from the WASM
module's exports (or custom sections) without instantiating the module.
This enables tooling like `cleat deploy` to extract metadata before
inserting into `workflow_defs`.

### 3.7 Host Interface Versioning

Covered in detail in `wasm-demo/cluster/versioning/main.go:163-198`. Summary:

| Change | Safety | Strategy |
|--------|--------|----------|
| Add new host function | Safe | Old modules don't import it |
| Change host function signature | Unsafe | Version the import name: `durable_call_v2` |
| Remove host function | Unsafe | Deprecation period; query DB for active users |
| Change bit-packing format | Unsafe | Bump ABI version; multi-ABI worker needed |

The host interface changes slowly (months/years). Workflow code changes
frequently (days/weeks). These are **different cadences**, decoupled by
WASM. The worker binary only changes when the host interface changes.

---

## 4. Implementation Phases

### Phase 1: Database Schema + Loader (Week 1-2)
**Goal:** Host runtime can store and load versioned WASM modules.

| Task | Effort |
|------|--------|
| Implement `workflow_defs` table schema + migrations | 4h |
| Implement `plugin_defs` table schema + migrations | 2h |
| Add `def_version` and `plugin_vers` columns to `workflow_instances` | 2h |
| Implement `WorkflowLoader` against real database (SQL, not demo map) | 8h |
| Implement `PluginLoader` with semver resolution | 8h |
| Add wazero module instantiation from DB-loaded WASM bytes | 8h |
| LRU cache for WASM modules (avoid DB hit per workflow execution) | 4h |

### Phase 2: Build-Time Version Stamping (Week 2-3)
**Goal:** Every compiled WASM module carries its identity.

| Task | Effort |
|------|--------|
| Go: Implement `workflow_name`/`workflow_version` in custom WASM section | 6h |
| Python: componentize-py metadata extraction | 6h |
| AS: asconfig.json metadata → WASM custom section | 6h |
| Rust: Cargo.toml metadata → WASM custom section | 4h |
| `cleat deploy` CLI: extract metadata, INSERT into workflow_defs | 8h |
| `cleat deploy-plugin` CLI: deploy plugin WASM to plugin_defs | 4h |

### Phase 3: Plugin Versioning (Week 3-4)
**Goal:** Workflows declare plugin deps; host resolves and pins at start.

| Task | Effort |
|------|--------|
| Define plugin dependency manifest format for each SDK | 4h |
| Host: resolve plugin deps at instance creation time (semver matching) | 8h |
| Host: load plugin WASM modules alongside workflow module in wazero | 8h |
| Host: expose plugin functions to workflow module via host imports | 8h |
| SDK: add `plugin_deps` declaration to build-time metadata | 4h |
| Documentation: plugin development guide | 4h |

### Phase 4: Child Workflow Versioning (Week 4-5)
**Goal:** Child workflows get the correct version automatically.

| Task | Effort |
|------|--------|
| Implement child workflow version resolution rules | 8h |
| Add `ChildWorkflowOptions` to Go SDK | 4h |
| Add `ChildWorkflowOptions` to Python SDK | 4h |
| Add version parameter to AS/Java/Rust child workflow APIs | 6h |
| Host: validate child version exists and is compatible | 4h |
| Tests: versioned child workflows across multiple SDKs | 8h |

### Phase 5: In-Flight Migration (Week 5-6)
**Goal:** Long-running workflows can upgrade to a new version.

| Task | Effort |
|------|--------|
| Implement `ContinueAsNewWithVersion` in host runtime | 8h |
| Event history compaction: carry forward state across versions | 8h |
| SDK: add `continue_as_new(input, version)` to all SDKs | 8h |
| Validation: verify new version is compatible with old event history | 8h |
| Documentation: migration guide with examples | 4h |

### Phase 6: GC, Observability, Tooling (Week 6-7)
**Goal:** Operational readiness for versioned workflows in production.

| Task | Effort |
|------|--------|
| Garbage collection: mark versions deprecated when zero active instances | 4h |
| Metrics: active instances by (name, version), load counts, cache hit rates | 4h |
| `cleat versions list <workflow>` CLI | 4h |
| `cleat versions deprecate <workflow> <version>` CLI | 2h |
| `cleat versions active` CLI — show which versions have running instances | 4h |
| Dashboard: version distribution across running workflows | 8h |
| Alert: workflows stuck on deprecated versions > N days | 4h |

---

## 5. Total Effort

| Phase | Description | Person-Days |
|-------|-------------|-------------|
| P1 | Database Schema + Loader | 5 days |
| P2 | Build-Time Version Stamping | 5 days |
| P3 | Plugin Versioning | 5 days |
| P4 | Child Workflow Versioning | 5 days |
| P5 | In-Flight Migration | 5 days |
| P6 | GC, Observability, Tooling | 4 days |
| **Total** | | **~29 person-days** |

---

## 6. Key Design Decisions

1. **Version per instance, not per worker.** Each instance row carries its
   `def_version`. The worker loads the right WASM blob at execution time.
   This is the fundamental difference from Temporal's task-queue approach.

2. **Plugin versions pinned at instance creation, not compile time.**
   This allows patch-level plugin updates to be picked up without
   recompiling workflows, while preventing major version churn mid-flight.

3. **WASM custom sections for metadata.** The host can read workflow
   identity (name, version, min_version, plugin_deps) from the WASM
   binary without instantiating the module. This enables tooling like
   `cleat deploy` to validate before inserting into the database.

4. **Semver for plugins, integer versions for workflows.** Plugins use
   semver because they're consumed as libraries with compatibility ranges.
   Workflows use monotonic integers because they're discrete versions of
   a business process — there's no "compatible range" for workflow logic.

5. **Host interface versions independently.** The worker binary provides
   host functions to WASM modules. This interface changes slowly and is
   versioned separately from workflow definitions. A single worker binary
   can host any workflow version that targets a supported ABI version.

6. **Continue-as-new for migration, not hot-patching.** Temporal's
   `GetVersion()` patches in-workflow logic. Cleat uses
   `continue_as_new` to restart the workflow on a new version, which is
   simpler and preserves deterministic replay guarantees.

---

## 7. Risks

1. **WASM module size.** A 200KB WASM blob per version, stored N times
   for N versions. Mitigation: deduplication at the storage layer, LRU
   cache in workers, only keep versions with active instances.

2. **Semver resolution complexity.** Plugin dependency resolution at
   instance creation time requires a semver library and careful testing
   of edge cases (pre-release versions, conflicting constraints).

3. **Event history compatibility across versions.** Continue-as-new
   migration requires the new version to understand the old version's
   event history. Mitigation: `min_version` field ensures the new
   version declares compatibility with old event formats.

4. **Plugin WASM interface stability.** If a plugin's host interface
   changes, workflows compiled against the old interface break.
   Mitigation: plugin host interfaces follow the same additive-only
   policy as the main host interface.
