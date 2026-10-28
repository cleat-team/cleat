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

Workflow definitions (`workflow_defs` rows) and their WASM binaries are
**replicated to every shard**: a run's rows are placed by its run ID, and the
foreign key from a run to its definition is inside one database, so every shard
that can hold a run needs the definition. Until cleat#3545 a deploy wrote the
definition to one shard only, and a run on any other shard failed with
`workflow_payloads_def_fkey`.

| Operation | Behaviour |
|-----------|-----------|
| deploy, deprecate | Written to every shard, the shard the name hashes to first. |
| purge | Removed from the other shards first and that one last, and only if the rest succeeded. |
| list | Each (name, version) once; where copies differ, the name's own shard wins. |
| count of open runs | Summed over every shard. |
| load by name (WASM, config, DAG, version lookup) | Tries the name's own shard first, then the rest. |
| tags and routing rules | Live on the name's own shard only. They are read and written by name, never from a run's shard. |

There is no transaction across databases, so a write can reach some shards and
not others. The error names the shards written, the shards that failed and the
shards not tried, and every write is idempotent: run the same command again to
repair it. A deploy that would replace a version's bytes under open runs
(cleat#3497) is refused if **any** shard has such runs, and is checked on every
shard before any is written. That pre-check exists for PostgreSQL; on MySQL and
SQL Server each shard applies the rule to its own runs as it is written.

`cleat deploy --db` writes by raw SQL to the one database it is given, so
against a sharded deployment it reaches that shard only; deploy through the
worker or `cleatctl` instead.

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

`StartNewRun` picks a shard by hashing the **run ID**, so runs of one workflow
type spread over every shard. (This section said the definition name until
cleat#3545; that was never what the code did.)

Child workflows are placed on the **same shard as the parent** (hash of the
parent workflow ID).

---

## Setup

### 1. Create the Schema on Each Shard

Every shard must have the full cleat schema.  Use the same migration scripts
you use for a single-node deployment:

```bash
for f in migrations/postgres/*.sql; do psql "$SHARD_CONN_STR" -f "$f"; done
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
