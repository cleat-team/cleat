# Production Hardening Plan

Addresses the 5 remaining weaknesses from COMPARISON.md: task routing,
history compaction, exactly-once semantics, ecosystem integrations, and
database sharding strategy.

---

## 1. Task Routing

### Problem

All workflow types compete in a single `SKIP LOCKED` queue. You can't route
GPU-bound ML workflows to GPU workers, high-memory data processing to
memory-optimized workers, or latency-sensitive workflows to dedicated pools.

### Design

Add a `task_queue TEXT NOT NULL DEFAULT 'default'` column to `workflow_defs`
and `workflow_instances`. Workers subscribe to one or more queues via
`--task-queue` (repeatable flag). The dispatch loop claims from matching
queues in round-robin order.

```
workflow_defs:
  + task_queue TEXT NOT NULL DEFAULT 'default'

workflow_instances:
  + task_queue TEXT NOT NULL DEFAULT 'default'

  New index:
  CREATE INDEX idx_instances_tenant_queue_ready
    ON workflow_instances(tenant_id, task_queue, status, next_wake_at)
    WHERE status = 'ready';
```

**ClaimWorkflow** query changes from:
```sql
WHERE status='ready' AND next_wake_at <= now() AND tenant_id = $2
```
to:
```sql
WHERE status='ready' AND next_wake_at <= now() AND tenant_id = $2 AND task_queue = ANY($3)
```

**Worker flag:**
```
--task-queue default --task-queue gpu --task-queue high-memory
```

**Deploy command:**
```
cleat deploy --task-queue gpu ./ml_workflow.wasm
```

If no `--task-queue` is specified, defaults to `'default'`.

### Files

| Action | File |
|--------|------|
| Modify | `schema.sql` — add `task_queue` columns and index |
| Modify | `internal/host/db.go` — add `task_queue` to queries |
| Modify | `cmd/cleat-worker/main.go` — repeatable `--task-queue` flag |
| Modify | `cmd/cleat/main.go` — `--task-queue` on deploy |
| Create | `migrations/003_task_routing.sql` |

### Effort: 1 week

---

## 2. History Compaction

### Problem

`event_history` grows linearly with workflow steps. A workflow with 10,000
steps produces 10,000 rows. Replay re-reads all of them. `ContinueAsNew` is
a manual workaround.

### Design

**Automatic compaction** after a threshold (default: 1000 events). When a
workflow instance exceeds the threshold, the compactor:

1. Takes the current event history (N rows)
2. Replays it to produce the current in-memory state
3. Serializes that state as a single `compaction_state JSONB` column on
   `workflow_instances`
4. Deletes all compacted event_history rows (keeps the last K events for
   recent visibility, default K=50)
5. Records a `compaction_marker` event as the new "step 0" pointing to the
   compacted state

On replay after compaction:
1. Load `compaction_state` from `workflow_instances`
2. Rehydrate the engine state from the checkpoint
3. Replay only the remaining events after the compaction marker

```
workflow_instances:
  + compaction_state JSONB        -- serialized engine state at compaction point
  + compacted_at TIMESTAMPTZ       -- when last compaction occurred
  + compaction_step INTEGER        -- the step number where compaction was applied
```

**Compaction trigger**: a background goroutine in the reaper loop that runs
every 5 minutes and compacts any instance where `event_history` count exceeds
the threshold.

**Compaction state format** (JSONB):
```json
{
  "version": 1,
  "completed_steps": [0, 1, 2, ..., 950],
  "pending_defers": [{"id": "abc", "desc": "cleanup"}],
  "open_children": [{"run_id": "xyz", "name": "sub_workflow"}],
  "signal_buffer": {},
  "query_state": {"progress": "95%"},
  "last_call_results": {
    "step_950": {"response": "...", "service": "payments", "operation": "Charge"}
  }
}
```

### Files

| Action | File |
|--------|------|
| Modify | `schema.sql` — add compaction columns |
| Create | `internal/host/compaction.go` — compactor logic |
| Modify | `internal/host/engine.go` — support replay from compaction state |
| Modify | `internal/host/db.go` — `CompactHistory`, `LoadCompactedHistory` |
| Modify | `cmd/cleat-worker/main.go` — compaction goroutine in reaper loop |
| Create | `migrations/004_history_compaction.sql` |

### Effort: 2 weeks

---

## 3. Exactly-Once Semantics

### Problem

Current idempotency is best-effort: `ON CONFLICT DO NOTHING` on event history
prevents duplicate event writes during replay, but there's no protection at
the API boundary. A client that retries `POST /api/workflows/:name/start` can
create duplicate workflow instances.

### Design

**Idempotency keys** at the workflow start boundary.

```sql
CREATE TABLE idempotency_keys (
    key_hash    BYTEA PRIMARY KEY,       -- SHA-256(idempotency_key)
    tenant_id   UUID NOT NULL,
    workflow_id TEXT,                     -- the workflow instance created
    result      JSONB,                    -- cached result if completed
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL      -- TTL for cleanup (default: 7 days)
);
```

**StartNewRun with idempotency:**
```go
func (s *PostgresStore) StartNewRun(ctx context.Context, defName, entryPoint, input string, idempotencyKey string) (string, bool, error) {
    if idempotencyKey != "" {
        keyHash := sha256.Sum256([]byte(idempotencyKey))
        
        // Check if we've seen this key before
        var existingWorkflowID string
        var existingResult []byte
        err := s.db.QueryRowContext(ctx,
            `SELECT workflow_id, result FROM idempotency_keys 
             WHERE key_hash = $1 AND tenant_id = $2 AND expires_at > now()`,
            keyHash[:], s.tenantID).Scan(&existingWorkflowID, &existingResult)
        
        if err == nil {
            // Key exists — return existing workflow
            if existingResult != nil {
                return existingWorkflowID, true, nil // already completed
            }
            return existingWorkflowID, false, nil // still running
        }
        
        // New key — insert and proceed
        // ... (in transaction with the workflow insert)
    }
    // ... normal insert ...
}
```

The API handler extracts the idempotency key from the request:
```
POST /api/workflows/:name/start
Idempotency-Key: <client-generated-unique-key>
```

On workflow completion, the result is written back to `idempotency_keys`.
A background job cleans up expired keys.

### Files

| Action | File |
|--------|------|
| Modify | `schema.sql` — add `idempotency_keys` table |
| Modify | `internal/host/db.go` — idempotency-aware `StartNewRun`, `CompleteWorkflow` |
| Modify | `cmd/cleat-worker/main.go` — extract `Idempotency-Key` header in handler |
| Create | `migrations/005_exactly_once.sql` |

### Effort: 1-2 weeks

---

## 4. Ecosystem Integrations

### 4a. Helm Chart

A minimal Helm chart for deploying cleat on Kubernetes.

```
charts/cleat/
  Chart.yaml
  values.yaml
  templates/
    configmap.yaml         -- database URL, tenant config
    deployment.yaml        -- worker deployment
    service.yaml           -- HTTP API service
    hpa.yaml               -- autoscaling
    serviceaccount.yaml     -- RBAC
    secret.yaml            -- API keys (optional)
```

**values.yaml** highlights:
```yaml
replicaCount: 3
image:
  repository: ghcr.io/rcownie/cleat-worker
  tag: latest

worker:
  concurrency: 10
  tenantId: "00000000-0000-0000-0000-000000000000"
  taskQueues:
    - default

postgres:
  host: postgres
  port: 5432
  database: cleat
  # Secret reference for credentials
  existingSecret: cleat-postgres-creds

resources:
  requests: {cpu: 100m, memory: 128Mi}
  limits:   {cpu: 500m, memory: 512Mi}

autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPU: 70
  targetMemory: 80

monitoring:
  serviceMonitor:
    enabled: true      # Prometheus Operator ServiceMonitor
```

### 4b. Grafana Dashboard

A JSON dashboard template for the Prometheus metrics already exported at
`/metrics`. Key panels:

1. **Workflow throughput**: `rate(durable_workflows_completed_total[5m])`
2. **Workflow latency (p50/p95/p99)**: histogram of execution duration
3. **Active workflows by status**: gauge of ready/running/failed
4. **Claim latency**: how long workers wait to claim work
5. **Event history size**: distribution of events per workflow
6. **Worker pool saturation**: in-flight goroutines vs concurrency cap
7. **DB errors**: rate of database operation failures
8. **WASM compile time**: how long module compilation takes
9. **Tenant breakdown**: workflows/sec per tenant (when multi-tenant)

The dashboard JSON goes in `monitoring/grafana-dashboard.json`.

### 4c. OpenTelemetry Trace Export

Wire the existing `trace_id` (already generated and stored) into OTLP export.
The worker already records start/end times for each workflow execution. Add:

```go
// internal/telemetry/tracing.go
func InitTracing(ctx context.Context, endpoint string) (*trace.TracerProvider, error) {
    exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(endpoint))
    // ...
    tp := trace.NewTracerProvider(trace.WithBatcher(exp))
    otel.SetTracerProvider(tp)
    return tp, nil
}
```

Each workflow execution becomes a trace span. Each `DurableCall` event becomes
a child span. The span attributes include `tenant_id`, `def_name`, `def_version`,
`workflow_id`, `step`. This gives you a full distributed trace of every workflow
execution without any instrumentation code in the workflow itself.

Worker flag: `--otel-endpoint localhost:4317` (OTLP gRPC).

### Files

| Action | File |
|--------|------|
| Create | `charts/cleat/` — Helm chart |
| Create | `monitoring/grafana-dashboard.json` |
| Create | `internal/telemetry/tracing.go` |
| Modify | `cmd/cleat-worker/main.go` — `--otel-endpoint` flag, init tracing |
| Modify | `go.mod` — add `go.opentelemetry.io/otel` deps |

### Effort: 1 week

---

## 5. Database Sharding Strategy

### Problem

A single PostgreSQL instance has finite capacity. For high-throughput use
cases (thousands of workflow steps/sec), you need to shard across multiple
databases.

### Design

**Shard by tenant_id** — the simplest and most operationally sound approach.

```
shards:
  shard-0: tenant_a, tenant_b, tenant_c  → postgres-0:5432/cleat
  shard-1: tenant_d, tenant_e            → postgres-1:5432/cleat
  shard-2: tenant_f                      → postgres-2:5432/cleat
```

Each shard is an independent PostgreSQL database with the full cleat schema.
The worker holds a `map[string]*PostgresStore` keyed by shard name, each with
its own `*sql.DB` connection pool.

**Shard registry** — a lightweight config or a single "control plane" database:

```yaml
# shards.yaml
shards:
  - name: shard-0
    conn: "postgres://user:pass@pg-0:5432/cleat?sslmode=disable"
    tenants: ["aaaaaaaa-*", "bbbbbbbb-*"]   # glob patterns for tenant IDs
  - name: shard-1
    conn: "postgres://user:pass@pg-1:5432/cleat?sslmode=disable"
    tenants: ["cccccccc-*"]
```

The worker:
1. Loads `shards.yaml` at startup
2. Creates a `*PostgresStore` per shard
3. The dispatch loop polls all shards in parallel (one goroutine per shard)
4. Tenant-scoped operations (ClaimWorkflow, deploy) route to the correct shard
   by hashing `tenant_id` against the shard map

```go
type ShardedStore struct {
    shards      []*Shard
    tenantMap   map[uuid.UUID]*Shard  // tenant → shard lookup
}

type Shard struct {
    Name   string
    Store  *PostgresStore
    DB     *sql.DB
}

func (ss *ShardedStore) ClaimWorkflow(ctx context.Context, workerID string) (*WorkflowInstance, error) {
    // Poll all shards in round-robin or random order
    for _, shard := range ss.shuffledShards() {
        wf, err := shard.Store.ClaimWorkflow(ctx, workerID)
        if wf != nil {
            return wf, nil
        }
    }
    return nil, nil // no work on any shard
}
```

**Migration strategy**: each shard runs the same migrations independently.
The migration tool runs against each shard connection in sequence.

**Limitations this design accepts:**
- No cross-shard child workflows (a child must live on the same shard as its
  parent — enforced by `tenant_id` inheritance)
- No cross-shard transactions
- Shard rebalancing requires manual tenant migration (export from old shard,
  import to new shard)

**When to shard**: document that a single `db.r6g.xlarge` (4 vCPU, 32 GB RAM)
can handle ~500-1000 workflow steps/second with the current schema. Sharding
is needed when you exceed this or when you need geographic distribution.

### Files

| Action | File |
|--------|------|
| Create | `internal/host/sharded_store.go` — `ShardedStore` implementation |
| Modify | `cmd/cleat-worker/main.go` — `--shards-file` flag, multi-shard dispatch |
| Create | `docs/sharding.md` — documentation with capacity planning |

### Effort: 1-2 weeks

---

## Implementation Order

| Week | Deliverable | Dependencies |
|------|-------------|--------------|
| 1 | Task routing | None |
| 2 | Helm chart + Grafana dashboard | None |
| 3 | History compaction | None |
| 4 | Exactly-once semantics | None |
| 5 | OpenTelemetry tracing | None |
| 6 | Sharding strategy + docs | None (all independent) |

All six are independent — they can be built in any order or in parallel by
multiple engineers.

## Verification

For each deliverable:

1. **Task routing**: Deploy two workers with different `--task-queue` flags.
   Start workflows targeting each queue. Verify they route to the correct
   worker pool.

2. **History compaction**: Run a workflow with 2000 steps. Verify compaction
   fires at 1000, history row count drops, replay produces the same result.

3. **Exactly-once**: Fire the same `Idempotency-Key` twice at the start
   endpoint. Verify only one workflow instance is created and the second
   call returns the existing workflow ID.

4. **Helm chart**: `helm install cleat ./charts/cleat` on a kind cluster.
   Verify the worker starts, connects to Postgres, and the web UI is
   accessible.

5. **Grafana dashboard**: Import `grafana-dashboard.json`. Verify all panels
   show data after running a few workflows.

6. **OTel tracing**: Run `docker run jaegertracing/all-in-one`. Start worker
   with `--otel-endpoint localhost:4317`. Execute a workflow. Verify trace
   appears in Jaeger UI.

7. **Sharding**: Configure two shards. Deploy a tenant to each. Verify the
   worker polls both. Verify tenant A's workflows don't appear in shard B.
