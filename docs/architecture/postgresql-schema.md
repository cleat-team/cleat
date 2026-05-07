# PostgreSQL Schema

Cleat uses PostgreSQL as its sole infrastructure dependency, serving four
roles: blob store, state store, work queue, and timer service.

## Schema File

The canonical schema is in `schema.sql` at the project root. Run it against a
PostgreSQL 14+ database before deploying workflows:

```bash
psql -U postgres -d cleat -f schema.sql
```

## Core Tables

### workflow_defs

Stores compiled WASM blobs, versioned by workflow name. Each deploy creates a
new version.

```sql
CREATE TABLE workflow_defs (
    name TEXT NOT NULL,
    version INTEGER NOT NULL,
    wasm_bytes BYTEA NOT NULL,
    entry_points TEXT[] NOT NULL DEFAULT '{}',
    min_version INTEGER NOT NULL DEFAULT 0,
    max_history_length INTEGER NOT NULL DEFAULT 0,
    namespace TEXT NOT NULL DEFAULT 'default',
    dag_spec JSONB DEFAULT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version)
);
```

| Column | Type | Description |
|--------|------|-------------|
| `name` | TEXT | Workflow name (part of composite PK) |
| `version` | INTEGER | Monotonically increasing version number |
| `wasm_bytes` | BYTEA | Compiled WASM module binary |
| `entry_points` | TEXT[] | Exported entry point names (e.g., `{"place_order","cancel_order"}`) |
| `min_version` | INTEGER | Minimum compatible version for replay |
| `max_history_length` | INTEGER | Max events before compaction triggers (0 = default) |
| `namespace` | TEXT | Namespace for multi-tenant isolation |
| `dag_spec` | JSONB | DAG structure for visualization (optional) |
| `created_at` | TIMESTAMPTZ | Deployment timestamp |

**Indexes**:

- `idx_defs_active` on `(name, version DESC)` -- speeds up latest-version
  lookups for deployment.

### workflow_instances

Tracks individual workflow execution state. Serves as both state store and
work queue.

```sql
CREATE TABLE workflow_instances (
    id TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    def_version INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'ready',
    input JSONB NOT NULL DEFAULT '{}',
    assigned_to TEXT,
    heartbeat_at TIMESTAMPTZ,
    next_wake_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    result JSONB,
    error_msg TEXT,
    cancellation_requested BOOLEAN NOT NULL DEFAULT false,
    cancellation_reason TEXT,
    namespace TEXT NOT NULL DEFAULT 'default',
    parent_workflow_id TEXT,
    query_state JSONB DEFAULT '{}',
    sticky_worker_id TEXT,
    trace_id TEXT,
    FOREIGN KEY (def_name, def_version) REFERENCES workflow_defs(name, version)
);
```

| Column | Type | Description |
|--------|------|-------------|
| `id` | TEXT | Unique workflow instance ID (UUID) |
| `def_name` | TEXT | References `workflow_defs.name` |
| `def_version` | INTEGER | References `workflow_defs.version` |
| `status` | TEXT | `ready`, `running`, `completed`, `failed`, `suspended` |
| `input` | JSONB | Workflow input arguments |
| `assigned_to` | TEXT | Worker ID currently claiming this instance |
| `heartbeat_at` | TIMESTAMPTZ | Last heartbeat from the claiming worker |
| `next_wake_at` | TIMESTAMPTZ | When to retry (sleep/suspend/resume deadline) |
| `result` | JSONB | Workflow result (if completed) |
| `error_msg` | TEXT | Error message (if failed) |
| `cancellation_requested` | BOOLEAN | Whether cancellation has been requested |
| `cancellation_reason` | TEXT | Reason for cancellation |
| `namespace` | TEXT | Namespace for multi-tenant routing |
| `parent_workflow_id` | TEXT | Parent workflow for child workflows |
| `query_state` | JSONB | Queryable workflow state |
| `sticky_worker_id` | TEXT | Preferred worker for cache locality |
| `trace_id` | TEXT | OpenTelemetry trace ID for observability |

**Indexes**:

- `idx_instances_ready` on `(status, next_wake_at)` WHERE `status = 'ready'` --
  accelerates the worker poll loop. This is the most critical index for worker
  throughput.
- `idx_instances_heartbeat` on `(assigned_to, heartbeat_at)` WHERE
  `status = 'running'` -- enables monitoring and stale-assignment detection.
- `idx_instances_stale` on `(status, heartbeat_at)` WHERE `status = 'running'` --
  used by the reaper to reclaim instances with stale heartbeats.
- `idx_instances_namespace_ready` on `(namespace, status, next_wake_at)` WHERE
  `status = 'ready'` -- namespace-filtered claim lookups.
- `idx_instances_sticky` on `(sticky_worker_id)` WHERE `sticky_worker_id IS NOT
  NULL` -- sticky worker fast path.

### event_history

Ordered list of every cleat call, sleep, signal, defer, and child workflow
event. This is the core of the replay mechanism.

```sql
CREATE TABLE event_history (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    step INTEGER NOT NULL,
    event_type TEXT NOT NULL DEFAULT 'call',
    service TEXT,
    operation TEXT,
    request JSONB,
    response JSONB,
    error TEXT,
    duration_ms BIGINT,
    signal_names TEXT,
    timeout_ms BIGINT,
    signal_name TEXT,
    signal_payload JSONB,
    defer_description TEXT,
    defer_id TEXT,
    child_name TEXT,
    child_input JSONB,
    run_id TEXT,
    new_input JSONB,
    plugin_name TEXT,
    plugin_func TEXT,
    plugin_input JSONB,
    plugin_output JSONB,
    plugin_error TEXT,
    promise_name TEXT,
    promise_id TEXT,
    promise_result TEXT,
    promise_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, step)
);
```

The `event_type` column classifies each event. Supported types include:
`call`, `sleep`, `await_signals`, `signal_received`, `defer`,
`child_workflow`, `await_child`, `continue_as_new`, `heartbeat`,
`plugin_call`, `create_promise`, `await_promise`, `plugin_call_stream_chunk`,
`run_detached`, and others.

Events are appended in sequential order. During replay, the engine walks
events by `step` number, returning cached responses for completed calls and
executing real calls for the first uncompleted step.

### workflow_signals

External signals delivered to running workflows.

```sql
CREATE TABLE workflow_signals (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    signal_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    delivered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, signal_name)
);
```

The engine checks for signals during `AwaitSignals` and `PollSignal`. Signal
delivery is recorded in `event_history` as `signal_received` events for
deterministic replay.

### Additional Tables

#### workflow_schedules

Cron-based recurring workflow execution. Created via `cleat schedule add` or
the REST API.

```sql
CREATE TABLE workflow_schedules (
    name TEXT PRIMARY KEY,
    def_name TEXT NOT NULL,
    entry_point TEXT NOT NULL DEFAULT '',
    cron_expression TEXT NOT NULL,
    input JSONB NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT true,
    next_run_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_run_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### workflow_promises

Inter-workflow promise coordination (for cross-workflow data passing).

```sql
CREATE TABLE workflow_promises (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    promise_id TEXT NOT NULL,
    promise_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, promise_id)
);
```

#### concurrency_keys

Per-key concurrency control -- ensures only one workflow holds a given key at
a time.

```sql
CREATE TABLE concurrency_keys (
    key_hash BYTEA PRIMARY KEY,
    key_text TEXT NOT NULL,
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
```

#### workflow_update_requests

Update handler requests (in-flight workflow mutations).

```sql
CREATE TABLE workflow_update_requests (
    workflow_id TEXT NOT NULL REFERENCES workflow_instances(id),
    update_name TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    promise_id TEXT,
    status TEXT NOT NULL DEFAULT 'pending',
    result JSONB,
    error_msg TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    PRIMARY KEY (workflow_id, update_name)
);
```

## Key Indexes Summary

| Index | Table | Purpose | Uniqueness |
|-------|-------|---------|------------|
| `idx_instances_ready` | `workflow_instances` | Worker poll loop: find runnable instances | Non-unique, partial |
| `idx_instances_namespace_ready` | `workflow_instances` | Namespace-scoped poll loop | Non-unique, partial |
| `idx_instances_heartbeat` | `workflow_instances` | Heartbeat monitoring | Non-unique, partial |
| `idx_instances_stale` | `workflow_instances` | Reaper: stale heartbeat detection | Non-unique, partial |
| `idx_instances_sticky` | `workflow_instances` | Sticky worker fast path | Non-unique, partial |
| `idx_defs_active` | `workflow_defs` | Latest-version lookup | Non-unique |
| `idx_promises_status` | `workflow_promises` | Promise resolution lookup | Non-unique |
| `idx_concurrency_keys_workflow` | `concurrency_keys` | Key-to-workflow lookup | Non-unique |
| `idx_update_requests_pending` | `workflow_update_requests` | Pending update lookup | Non-unique |

## Migration Strategy

### Current State

Schema migrations are currently **manual**. There is no automated migration
tool. Changes are applied by running `schema.sql` (which uses
`CREATE TABLE IF NOT EXISTS` and `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`
for idempotent application).

### Future Plans

- **Auto-migration** at worker startup: the worker will check the schema
  version and apply pending migrations before entering the dispatch loop.
- **Versioned migrations**: each migration will be a numbered SQL file in a
  `migrations/` directory with an up/down pair.
- **Plugin migrations**: plugins implementing `plugin.HasMigrations` can
  register their own migrations, which are applied during plugin initialization
  in dependency order.

## Connection Management

The worker uses Go's `database/sql` connection pool:

```go
db.SetMaxOpenConns(concurrency + 5)   // Allow headroom for heartbeats, etc.
db.SetMaxIdleConns(5)
db.SetConnMaxLifetime(5 * time.Minute)
```

### Sharded Deployments

For multi-tenant deployments, the worker supports sharded databases. Each
shard has its own connection string and tenant assignment. The sharded store
dispatches workflow operations to the correct shard by tenant ID. See
`internal/host/sharded_store.go` and `cmd/cleat-worker/main.go` for
implementation details.
