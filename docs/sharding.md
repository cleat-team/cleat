# Database Sharding

Cleat supports horizontal scaling by distributing workflow data across multiple
PostgreSQL instances (shards).  A `ShardedStore` implements the same
`WorkflowStore` interface as the single-node `PostgresStore`, so the worker
daemon, API server, and CLI all work without code changes once configured.

---

## When to Shard

- You have more than one PostgreSQL instance and want to spread load.
- A single database has become a throughput bottleneck – `ClaimWorkflow` uses
  `SELECT ... FOR UPDATE SKIP LOCKED` which is per-shard, so adding shards
  increases the overall dispatch rate.
- You want to colocate tenants on different physical databases for isolation
  or geographic locality.

---

## How it Works

### Consistent Hashing

Most operations (heartbeat, complete, fail, release, event history, signals)
resolve to a single shard via a SHA-256 hash of the **workflow ID**:

    shard_index = sha256(workflowID) % num_shards

This means a workflow always routes to the same shard without a lookup table.

### Definition-Level Data

Workflow definitions (`workflow_defs` rows), WASM binaries, and version lists
are expected to be **replicated to every shard**.  The `ShardedStore` tries
each shard when loading definitions and returns the first success.

### Fan-Out Operations

Three operations must touch **every shard**:

| Operation | Behaviour |
|-----------|-----------|
| `ClaimWorkflow` | Polls each shard, returns the first claimable instance. |
| `ReapStaleInstances` | Runs on every shard; the returned count is the sum. |
| `GetDueSchedules` | Returns the union of due schedules across all shards (deduped by name). |

### Schedule CRUD

Schedule operations (`CreateSchedule`, `DeleteSchedule`, `SetEnabled`,
`UpdateScheduleNextRun`) are broadcast to every shard so that each shard can
independently fire its own cron ticks.

### New Workflow Placement

`StartNewRun` picks a shard by hashing the **definition name**.  All runs of
the same workflow type land on the same shard, which simplifies capacity
planning and keeps related data together.

Child workflows are placed on the **same shard as the parent** (hash of the
parent workflow ID).

---

## Setup

### 1. Create the Schema on Each Shard

Every shard must have the full cleat schema.  Use the same migration scripts
you use for a single-node deployment:

```bash
psql "$SHARD_CONN_STR" < schema.sql
```

### 2. Write a Shard Configuration File

The config is a JSON array.  Each entry has a `name`, a `conn_str`
(PostgreSQL connection URL), and an optional `tenants` list (metadata for
your reference – the store does not use it for routing).

**Example** (`examples/sharding.json`):

```json
[
  {
    "name": "shard-us-east",
    "conn_str": "postgres://user:pass@us-east.example.com:5432/cleat?sslmode=disable",
    "tenants": ["tenant-a", "tenant-b"]
  },
  {
    "name": "shard-us-west",
    "conn_str": "postgres://user:pass@us-west.example.com:5432/cleat?sslmode=disable",
    "tenants": ["tenant-c"]
  }
]
```

### 3. Start the Worker with `--shards-file`

```bash
cleat-worker \
  --shards-file /etc/cleat/sharding.json \
  --task-queue default \
  --concurrency 10
```

When `--shards-file` is provided the `--db` flag is ignored.  Without
`--shards-file` the worker falls back to single-node `--db` mode.

---

## Limitations

- **No automatic rebalancing.**  If you add or remove shards you must drain
  the old shards and migrate workflow data manually (see below).
- **Cross-shard transactions are not supported.**  A single workflow and its
  event history always live on one shard.
- **ListWorkflows** merges results from every shard.  For large datasets
  consider using shard-specific queries (the API does not support shard
  filtering yet).
- **Schedules are replicated** on every shard.  Each shard fires independently;
  duplicate firing is prevented by the `SKIP LOCKED` mechanism in
  `GetDueSchedules`.

---

## Rebalancing (Adding / Removing Shards)

Because shard assignment is based on `sha256(workflowID) % n`, changing `n`
changes the routing for **most** workflows.  A migration involves:

1. Provision the new shard with an empty schema.
2. Stop writes to the old shards (or accept a period of dual routing).
3. For each workflow, determine its new shard and migrate the row and its
   event history.
4. Update the `--shards-file` and restart workers.

A future release may add a **shard-aware export/import** command to the
`durable` CLI.

---

## Observability / Monitoring

The `ShardedStore` exposes `Shards()` which returns the list of `*Shard`
structs, each holding its `*sql.DB` and `*PostgresStore`.  This allows
you to collect per-shard metrics:

- Number of active workflows per shard
- Database connection pool stats (`db.Stats()`)
- Claim latency per shard

Example snippet for Prometheus-style metrics:

```go
for _, sh := range shardedStore.Shards() {
    stats := sh.DB.Stats()
    fmt.Printf("shard_connections{shard=%q} %d\n", sh.Config.Name, stats.OpenConnections)
}
```
