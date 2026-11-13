# Database Sharding

> **Sharding is EXPERIMENTAL in 0.6.0** (owner decision D80, 2026-11-13). It is not a supported configuration of
> this release: it has known gaps, it is not part of the release gate, and it can change or be removed without the
> notice a supported feature gets. Try it on a deployment you can rebuild.
>
> **Known gaps** (each is an open issue; none is scheduled for 0.6.0 unless a fix turns out to be needed for correctness):
>
> - Serving more than one tenant on a sharded worker is not supported: tenant provisioning, drop, suspend and resume, and a
>   library install answer `501`, and the rotating claim is unavailable (cleat#3606).
> - `cleat deploy`, `cleatctl deploy` and `cleatctl versions deprecate`, `restore` and `purge` write one database, so on
>   a sharded deployment they reach one shard and sit outside the cross-shard deploy lock; only the worker's API
>   (`POST /api/definitions` and `POST /api/versions/<name>/<v>/deprecate`, `/restore` and `/purge`) writes every shard
>   (cleat#3684).
> - A normal start verifies the schema version of the first shard only (cleat#3724).
> - A shard built with the old `psql` loop is not adopted by `--migrate-only` (cleat#3725).

Cleat can distribute workflow data across multiple PostgreSQL instances
(shards).  A `ShardedStore` implements the same `WorkflowStore` interface as the
single-node `PostgresStore`, so the worker daemon, API server, and CLI are meant
to work without code changes once configured; the gaps above are where they do
not.

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
| deploy | Written to every shard, the shard the name hashes to first, **under a lock on that shard** held until the last shard is written, so two deploys of one version queue instead of interleaving (cleat#3547; see below). |
| deprecate | Written to every shard, the shard the name hashes to first, **under the same lock** as a deploy of that version (cleat#3547). |
| purge | **Under the same lock**, taken before anything is asked. The DELETE is run on every shard and held open; if any shard refuses, they are all rolled back and nothing is removed anywhere. Otherwise they are committed, the other shards first and the name's own shard last (cleat#3687; see below for what that leaves). |
| list | Each (name, version) once; where copies differ, the name's own shard wins. |
| count of open runs | Summed over every shard. |
| load by name (WASM, config, DAG, version lookup) | Tries the name's own shard first, then the rest. |
| tags and routing rules | Live on the name's own shard only. They are read and written by name, never from a run's shard. |

There is no transaction across databases, so a write can reach some shards and
not others. The error names the shards written, the shards that failed and the
shards not tried, and says what repairs it: for a deploy or deprecate, run the
same command again.

Two deploys of one version with different bytes used to be able to interleave across
shards, so that one shard ended holding one deploy's bytes and another shard held
the other's, and both deploys returned success (cleat#3547). Replacing a
version's bytes is allowed on a single store, where the later deploy wins, so a
sharded store does not forbid it: it makes "later" one decision by taking a
PostgreSQL advisory lock on the shard the name hashes to, keyed on the tenant,
name and version, before the pre-check and releasing it after the last shard is
written. A deploy waits for the one in front of it for at most 60 seconds and then
fails with `another deploy of workflow "X" version N holds the cross-shard deploy
lock` and writes nothing (HTTP 409 on `POST /api/definitions`); run it again. A
deprecate or purge of the same version takes the same lock, so it queues behind a
deploy of that version, and a deploy behind it, and gives up the same way (HTTP 409
on the version endpoints). Without the lock, a deploy landing between a purge's
writes to the shards left one shard holding a definition the name's own shard no
longer had, and two deprecations could leave the shards disagreeing about the flag. The
lock is transaction-scoped, so a cancelled or crashed deploy cannot leave it held.
Each deploy in flight on one name uses two connections on that shard's pool. It
does not cover the CLI commands that reach one shard only (`cleat deploy --db`,
`cleatctl deploy`, `cleatctl versions deprecate`, `restore` and `purge`; see below), or two
processes started with different `--shards-file` lists, which would choose
different shards to lock; changing the list is a resharding, which routing does not
support either.

A purge is refused by any shard that still holds a run of that version,
**finished runs included**, until retention removes the row (the foreign key from
a run to its definition does not cascade). Version GC and `cleatctl versions
purge` check only runs that are not settled, so this is the usual reason a purge
fails, not a race. Because every shard's DELETE has run, and none is committed,
before the first commit, such a refusal changes nothing. The open DELETE keeps its
row locked, so a run that starts for that version while a purge is in flight waits
for the purge and then fails because the version is gone (measured as a non-superuser
login with row-level security on: the same version waited, a different version of
the name and a different name did not); a run already there makes its shard's
DELETE fail, which rolls every shard back. The deletes are held open for at most
the same 60 seconds as the lock, counted from the first DELETE until every one is
held and the purge starts committing; if that runs out first, every shard is rolled
back and the purge fails having removed nothing. The 60 seconds does not run during
the commits. A purge holds one connection per shard for as long as it runs.

**What a purge still can split (cleat#3687, remaining).** The commits are one
after another across databases, with no transaction over all of them. A commit
that fails stops the ones after it: the shards not yet committed are rolled back
and keep the definition, so what a failure leaves is exactly the shards that
committed before the failing one, without the definition. The same is true of a
process that dies between two commits, and of a caller that cancels between them.
The error names the shards written, the shard that failed and the shards not
attempted, and deploying the version again restores them; until then a new run
that hashes to a shard without the definition fails `workflow_payloads_def_fkey`.
The window is the time between the first and last commit, not the whole purge, and
it needs a failed commit, a crash or a cancel, not just a run starting. A deploy that would replace a version's bytes under open runs
(cleat#3497) is refused if **any** shard has such runs, and is checked on every
shard before any is written. That pre-check exists for PostgreSQL; on MySQL and
SQL Server each shard applies the rule to its own runs as it is written.

`cleat deploy --db` writes by raw SQL to the one database it is given, so
against a sharded deployment it reaches that shard only. `cleatctl` does the
same for everything it does: it opens one store from one `--dsn`, which is a
single database. So `cleatctl deploy` reaches one shard and does not take the
cross-shard lock, and neither do `cleatctl versions deprecate`, `versions restore`
and `versions purge`: each changes the one shard its `--dsn` names and leaves the
others as they were. A purge that way also skips the all-or-nothing handling of
the table above, because that lives in the sharded store the CLI does not open.
Only the worker's API goes through the sharded store and writes every shard,
under the lock: `POST /api/definitions` for a deploy, and
`POST /api/versions/<name>/<v>/deprecate`, `/restore` and `/purge` for the other
three. On a sharded deployment, use those and not the CLI. (cleat#3684 asks
whether the CLIs should refuse a database that is one shard of several; they do
not today.)

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

Every shard must have the full cleat schema.  Run the same deploy step you use
for a single-node deployment, with the shards file (step 2) on the command line:

```bash
cleat-worker --driver postgres --shards-file shards.json --migrate-only
```

It migrates the core schema of every shard, and the plugin tables on the first
shard (the admin shard, see `docs/reference/worker-config.md`).  A shard whose
migration chain belongs to another release is refused before any shard is
changed, and a shard that cannot be migrated fails the run and is named in the
log.  `--migrate-db` names the first shard's migration connection only: the other
shards are migrated through their own `conn_str`, so that login needs DDL rights.

A shard built another way -- for example with the `psql` loop this page used to
give, which records no migration ledger -- is not adopted by this: `--migrate-only`
tries to apply the whole chain to it and fails at the first object that already
exists.  Migrating such a shard in place is not covered here.

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
