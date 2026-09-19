# Deploying cleat to production

This guide covers configuration, monitoring, backups, scaling, health checks,
and graceful shutdown for running cleat in production.

> **Before a tenant's workflows can fetch anything**, egress must be permitted
> by both the operator and the tenant — see
> [egress-policy.md](egress-policy.md). An unconfigured tenant reaches nothing,
> which is deliberate.

## Configuration

### Database URL

The worker connects to PostgreSQL via a connection string. Set it via the
`--db` flag or the `CLEAT_DATABASE_URL` environment variable:

```bash
export CLEAT_DATABASE_URL="postgres://user:pass@db-host:5432/cleat?sslmode=require"
cleat-worker
```

For production, always use `sslmode=require` (or `verify-full` with a CA
certificate). Never disable SSL in production.

### Namespaces

Namespaces isolate workflow definitions and instances. Use them to separate
environments, teams, or tenants:

```bash
cleat deploy --db "$DATABASE_URL" --namespace staging --name place_order ./out/order.wasm
cleat-worker --db "$DATABASE_URL" --namespace staging
```

Each namespace has its own set of `workflow_defs` and `workflow_instances`.

### Worker concurrency

Control how many workflow instances a worker processes simultaneously:

```bash
cleat-worker --db "$DATABASE_URL" --concurrency 20
```

Set `--concurrency` based on available CPU and the workload's I/O profile. A
good starting point is 2-4x the number of CPU cores. Monitor CPU and database
connection usage to tune this value.

### Heartbeat interval

```bash
cleat-worker --db "$DATABASE_URL" --heartbeat 10s
```

The heartbeat interval controls how often the worker updates `heartbeat_at` in
`workflow_instances`. If a worker crashes, its claimed instances become
reclaimable after the heartbeat stops updating. Lower values mean faster
recovery but more database writes.

### Poll interval

```bash
cleat-worker --db "$DATABASE_URL" --poll 250ms
```

Controls how often the worker polls for new work when the queue is empty.
Lower values reduce latency for new workflows but increase database load.

## Managed PostgreSQL

RDS, Aurora, Cloud SQL and Azure Database for PostgreSQL are supported, and the
migration set applies on all of them as of this change. It did not before: 8 of
72 migrations failed against a role with no superuser, and the first failure was
`005_app_role.sql`, so cleat could not be **installed** on a managed instance at
all.

**What is different there, and it is not configuration.** None of these
platforms gives you a PostgreSQL superuser. AWS documents the RDS master role as

```sql
CREATE ROLE postgres WITH LOGIN NOSUPERUSER INHERIT CREATEDB CREATEROLE
  NOREPLICATION VALID UNTIL 'infinity'
```

and `rds_superuser`, `cloudsqlsuperuser` and `azure_pg_admin` are
highest-privileged roles rather than superusers. `TestTheMigrationSetAppliesWithoutASuperuser` builds a role of exactly that shape and applies the whole set
through it on every run.

### One capability is genuinely unavailable

`BYPASSRLS` can only be granted by a true superuser:

```
ERROR:  permission denied to create role
DETAIL:  Only roles with the BYPASSRLS attribute may create roles with the
         BYPASSRLS attribute.
```

So `023_cross_tenant_claim.sql` and `024_cross_tenant_schedules.sql` cannot
create `cleat_dispatcher`. They now **say so and continue** rather than aborting
the run: the functions are created owned by the migrating role, and the worker's
startup check reports the claim as unavailable, naming the missing attribute
rather than telling you to apply a file you cannot apply.

| capability | on managed PostgreSQL |
|---|---|
| single-tenant dispatch | works, unchanged |
| cross-tenant claim | **works**, with `--claim-strategy=rotate` — it claims each tenant's work under that tenant's own RLS context and needs no exemption |
| cross-tenant claim via `--claim-strategy=global` | unavailable; `admin.claim_workflows` has no exemption to use |
| **a non-default tenant's cron** | **does not fire.** `024`'s due-schedule read has no grant-free equivalent yet, so only the worker's own tenant's schedules fire. The worker warns about this at startup. |

That last row is the one to check against your requirements before deploying a
multi-tenant cleat on a managed instance.

### Row-level security is unaffected

Three migrations backfill columns on RLS-forced tables and relied on the
migrating connection being a superuser. They now drop `FORCE ROW LEVEL SECURITY`
for the length of the backfill and restore it — which returns the **table
owner's** exemption and nothing else, leaving every other role constrained
throughout. Each migration runs in its own transaction, so a failure rolls the
exemption back with everything else, and the test above asserts that no table is
left with RLS enabled but not forced.

## Monitoring

### Prometheus metrics

Start the worker with `--api-addr` to expose a `/metrics` endpoint:

```bash
cleat-worker --db "$DATABASE_URL" --api-addr :8080
```

Prometheus metrics are available at `http://localhost:8080/metrics`.

Available metrics include:

| Metric | Type | Description |
|--------|------|-------------|
| `cleat_workflows_started_total` | Counter | Total workflows started |
| `cleat_workflows_completed_total` | Counter | Total workflows completed successfully |
| `cleat_workflows_failed_total` | Counter | Total workflows that failed |
| `cleat_workflows_running` | Gauge | Currently running workflows |
| `cleat_workflow_duration_seconds` | Histogram | Workflow execution duration |
| `cleat_calls_total` | Counter | Total DurableCall invocations |
| `cleat_call_duration_seconds` | Histogram | DurableCall duration |
| `cleat_heartbeats_total` | Counter | Total heartbeat updates |

### Grafana dashboards

Recommended dashboard panels:

1. **Workflow throughput** -- started vs completed vs failed (rate per minute)
2. **Workflow duration** -- p50/p90/p99 histograms
3. **Active workflows** -- current running count by workflow type
4. **Call latency** -- external service call duration distribution
5. **Worker pool** -- concurrency utilization across workers
6. **Stale assignments** -- instances with expired heartbeats (potential crashes)

### Key database queries for monitoring

```sql
-- Workflow counts by status
SELECT status, COUNT(*) FROM workflow_instances GROUP BY status;

-- Stale assignments (potential worker crashes)
SELECT id, def_name, assigned_to, heartbeat_at
FROM workflow_instances
WHERE status = 'running'
  AND heartbeat_at < NOW() - INTERVAL '30 seconds';

-- Workflow duration distribution
SELECT
    def_name,
    AVG(EXTRACT(EPOCH FROM (completed_at - created_at))) AS avg_duration_seconds
FROM workflow_instances
WHERE status = 'completed'
GROUP BY def_name;
```

## Backups

Workflow state is stored entirely in PostgreSQL. Standard PostgreSQL backup
procedures apply.

### pg_dump

```bash
# Full database backup
pg_dump "postgres://user:pass@localhost/cleat?sslmode=require" \
    -f cleat-backup-$(date +%Y%m%d).sql

# Restore
psql "postgres://user:pass@localhost/cleat?sslmode=require" \
    -f cleat-backup-20250101.sql
```

### Backup considerations

- **WASM blobs** are stored in `workflow_defs.wasm_bytes` (BYTEA column). They
  are included in standard `pg_dump`.
- **Event history** can grow large. Consider archiving completed workflow
  instances separately.
- **Point-in-time recovery (PITR)** is recommended for production. Configure
  WAL archiving and use `pg_basebackup` for continuous archiving.

### Table sizes

Monitor table sizes to plan for history compaction (automatic compaction is on
the roadmap):

```sql
SELECT
    relname AS table_name,
    pg_size_pretty(pg_total_relation_size(relid)) AS total_size
FROM pg_catalog.pg_statio_user_tables
WHERE relname IN ('workflow_instances', 'event_history', 'workflow_defs', 'workflow_signals')
ORDER BY pg_total_relation_size(relid) DESC;
```

## Scaling

### Horizontal scaling

Workers are stateless and horizontally scalable. Multiple `cleat-worker`
instances can run concurrently against the same database:

```bash
# Worker 1
cleat-worker --db "$DATABASE_URL" --concurrency 10

# Worker 2 (different machine)
cleat-worker --db "$DATABASE_URL" --concurrency 10
```

`SELECT ... FOR UPDATE SKIP LOCKED` ensures each workflow instance is claimed by
exactly one worker. No coordination between workers is needed.

### Vertical scaling

Increase `--concurrency` on a single worker to handle more workflows per
machine. Monitor:

- **CPU usage** -- WASM execution is CPU-bound for compute-heavy workflows
- **Database connections** -- each worker uses one database connection per
  concurrent workflow plus overhead
- **Memory** -- WASM modules are cached in memory (keyed by `def_name:def_version`)

### Connection pooling

Use PgBouncer or similar connection pooler between workers and PostgreSQL:

```
cleat-worker --db "postgres://user:pass@pgbouncer:6432/cleat?sslmode=require"
```

This reduces the number of database connections needed when running many
workers.

### WASM module caching

WASM modules are cached in memory to avoid repeated database loads. The cache
is keyed by `def_name:def_version`. Performance considerations:

- **Cold start** -- first invocation of a workflow version loads WASM from
  database (typically 1-5 MB) into memory
- **Warm executions** -- subsequent invocations use the cached module
- **Cache eviction** -- currently unbounded; monitor worker memory usage

## Health checks

### Worker health endpoint

When `--api-addr` is set, the worker exposes a health check endpoint:

```bash
curl http://localhost:8080/health
```

Response (healthy):

```json
{
    "status": "ok",
    "workflows_running": 5,
    "last_heartbeat": "2025-01-01T12:00:00Z",
    "database_connected": true
}
```

Use this endpoint for:

- **Load balancer health checks** -- ensure the worker is accepting work
- **Container orchestrator probes** (Kubernetes liveness/readiness probes)
- **Monitoring alerts** -- alert if `/health` returns non-200

### Database connectivity check

The health endpoint verifies database connectivity by running a lightweight
query. If the database connection is lost, the worker will:

1. Return `status: "degraded"` from the health endpoint
2. Attempt reconnection with exponential backoff
3. Continue serving the web UI and API (read-only mode)

### Graceful shutdown

The worker handles SIGINT and SIGTERM for graceful shutdown:

```bash
cleat-worker --db "$DATABASE_URL"

# In another terminal:
kill -TERM <worker_pid>
```

On receiving the signal, the worker:

1. Stops accepting new work (no longer claims instances from the database)
2. Waits for all in-flight workflow executions to complete (with an internal
   timeout)
3. Releases claimed instances by clearing `assigned_to` and updating
   `heartbeat_at` to a past timestamp (so other workers can reclaim them)
4. Exits

For containerized deployments, ensure your orchestrator sends SIGTERM and
provides adequate termination grace period (at least 30 seconds for workflows
with heartbeats).

## Capacity planning

### Estimating worker count

```
workers = (workflows_per_second * avg_duration_seconds) / concurrency_per_worker
```

Example: if you process 10 workflows/second, each taking 5 seconds, with
`--concurrency 20`:

```
workers = (10 * 5) / 20 = 2.5 => 3 workers
```

### Database sizing

- **event_history** is the largest table. Each DurableCall creates one row.
  Estimate: 500 bytes per call event.
- **workflow_instances** grows with the number of active workflows. Each row is
  approximately 1 KB plus the JSON `input` and `result` fields.
- **workflow_defs** stores WASM blobs (typically 1-5 MB each). Only one row per
  deployed version.

## Production checklist

Before going live:

- [ ] Database SSL is configured (`sslmode=require` or `verify-full`)
- [ ] Database backups are configured (pg_dump or PITR)
- [ ] Prometheus metrics endpoint is enabled (`--api-addr`)
- [ ] Grafana dashboards are set up for workflow throughput and latency
- [ ] Health check endpoint is integrated with load balancer / orchestrator
- [ ] Graceful shutdown timeout is adequate for workflow execution duration
- [ ] Connection pooling is configured (PgBouncer or equivalent)
- [ ] Namespaces are configured for environment isolation
- [ ] Worker `--concurrency` is tuned for the workload
- [ ] Alerting is configured for stale heartbeats and failed workflows
- [ ] Indexes are in place (see [schema documentation](../README.md#indexes))
- [ ] Event history growth is monitored (plan for archival strategy)
