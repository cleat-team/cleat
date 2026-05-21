# Upgrading cleat

This guide covers all upgrade scenarios for a cleat deployment: worker binary
upgrades, database schema migrations, workflow definition version changes,
rollback procedures, and PostgreSQL major version upgrades.

## Worker binary upgrade (rolling restart)

Cleat workers are stateless and horizontally scalable, making rolling upgrades
straightforward. The worker handles SIGTERM gracefully, so you can rely on
orchestrator-driven rolling updates.

### Manual rolling restart

If you manage workers directly (systemd, supervisor, or bare processes):

```bash
# For each worker node:
# 1. Install the new binary
cp cleat-worker-v2 /usr/local/bin/cleat-worker

# 2. Send SIGTERM to the running worker
kill -TERM $(pgrep cleat-worker)

# 3. Wait for the worker to shut down gracefully,
#    then start the new binary
cleat-worker --db "$DATABASE_URL" --concurrency 20
```

The worker on SIGTERM will:

1. Stop claiming new workflow instances from the database
2. Wait for all in-flight workflow executions to complete (with an internal
   timeout)
3. Release claimed instances by clearing `assigned_to` and updating
   `heartbeat_at` to a past timestamp
4. Exit

Other workers in the pool immediately pick up any instances the shutting-down
worker releases.

### Kubernetes rolling update

```bash
# Update the worker image tag in your deployment manifest
kubectl set image deployment/cleat-worker \
    cleat-worker=cleat/worker:2.0.0

# Or edit the manifest directly
kubectl edit deployment cleat-worker
```

Kubernetes handles the rollout automatically:

1. A new pod starts with the updated image
2. The old pod receives SIGTERM from the kubelet
3. The old pod drains its in-flight workflows
4. The new pod begins claiming from the queue
5. Repeat for each pod until all are updated

Verify the rollout:

```bash
kubectl rollout status deployment/cleat-worker
```

### Configuration changes during upgrade

If the new binary introduces or removes CLI flags, update your worker
configuration alongside the binary. The binary rejects unknown flags at
startup, so you must coordinate the binary and configuration change:

```bash
# Good: update config and binary together
cleat-worker --db "$DATABASE_URL" --concurrency 20 --new-flag value

# Bad: mismatched binary and config
# cleat-worker v1 with --new-flag => error
# cleat-worker v2 without --new-flag => uses default
```

## Database schema migration

### Automatic migration (recommended)

Starting from cleat v0.7.0, the worker checks the schema version at startup
and applies pending migrations automatically before entering the dispatch loop.
No manual steps are needed:

```bash
# Simply start the worker -- it applies migrations if needed
cleat-worker --db "$DATABASE_URL"
```

The worker logs applied migrations:

```
INFO[0000] Applied schema migration 002_add_promises_table  duration=12ms
INFO[0000] Schema is up to date at version 003
```

### Manual migration

If you prefer to apply migrations outside the worker startup path, run the
migration tool directly:

```bash
# Apply all pending migrations
cleat migrate up --db "$DATABASE_URL"

# Check migration status
cleat migrate status --db "$DATABASE_URL"

# Output:
# Migration 001_initial_schema ........ applied (2025-01-15)
# Migration 002_add_promises_table ... applied (2025-02-01)
# Migration 003_add_concurrency_keys . pending
```

Migrations are idempotent. Running `cleat migrate up` multiple times only
applies migrations that have not yet been applied.

### Migration files

Each migration is a numbered SQL file in the project's `migrations/` directory
with an up/down pair:

```
migrations/
  001_initial_schema.up.sql
  001_initial_schema.down.sql
  002_add_promises_table.up.sql
  002_add_promises_table.down.sql
```

Migrations are applied in numeric order. Down migrations are used only during
rollback (see [Rolling back a schema migration](#rolling-back-a-schema-migration)).

### Safety checks

Before applying a migration, the worker or migration tool runs sanity checks:

- The migration must not lock critical tables for extended periods
- `CREATE INDEX CONCURRENTLY` is used for large-table indexes
- `ADD COLUMN` must have a `DEFAULT` value or be nullable (no table rewrites)
- The migration must not drop columns that are still referenced by running
  workflow instances

If a migration fails, the worker logs the error and exits. Fix the migration
and restart.

### Migration 007: Foreign Key CASCADE

Migration 007 adds `ON DELETE CASCADE` to all foreign keys referencing
`workflow_instances(id)`. This affects five child tables: `event_history`,
`workflow_signals`, `workflow_promises`, `concurrency_keys`, and
`workflow_update_requests`.

**What changes**: Each FK is dropped and re-added with `ON DELETE CASCADE`.
In MySQL, `concurrency_keys` also receives its FK constraint for the first
time (it was missing in the original schema).

**Why**: Without CASCADE, deleting a workflow instance required manually
deleting child rows first. The `DeleteDeadLetteredWorkflows` reaper did this,
but other code paths that deleted workflow instances could leave orphaned
rows in child tables.

**Lock risk — HIGH**: Each `ALTER TABLE ... DROP CONSTRAINT ... ADD CONSTRAINT`
takes an `ACCESS EXCLUSIVE` lock (Postgres) or equivalent on the child table.
On large `event_history` tables (millions of rows), this blocks all writes for
the duration of the constraint validation scan.

**Mitigation**:
- The Postgres migration itself uses `SET LOCAL lock_timeout = '30s'` within the
  DO block, which overrides any session-level `lock_timeout` you may have set.
  If you want a shorter timeout for the migration, you must edit the migration
  SQL (the `SET LOCAL` inside the DO block takes precedence).
- Set `lock_timeout` before running for the migration runner's other statements:
  `SET lock_timeout = '5s';` (Postgres) or `SET SESSION lock_wait_timeout = 5;`
  (MySQL)
- Run during a maintenance window or off-peak hours
- For Postgres, the entire DO block runs in a single transaction; no intermediate
  states are visible to other sessions
- For MySQL, ALTER TABLE implicitly commits, creating a brief no-FK window between
  DROP and re-ADD on each table. Run during a quiet period
- Pre-validate by checking for orphaned rows:
  ```sql
  SELECT COUNT(*) FROM event_history eh
  LEFT JOIN workflow_instances wi ON eh.workflow_id = wi.id
  WHERE wi.id IS NULL;
  ```
  This should return 0 on a healthy installation.
- Estimated time: proportional to the largest child table. On a table with 10M
  rows, expect 30-120 seconds per ALTER.

**MySQL orphan check for concurrency_keys**: Before the migration, verify there
are no orphaned `concurrency_keys` rows:
```sql
SELECT COUNT(*) FROM concurrency_keys ck
LEFT JOIN workflow_instances wi ON ck.workflow_id = wi.id
WHERE wi.id IS NULL;
```
If this returns > 0, clean up orphaned rows first:
```sql
DELETE ck FROM concurrency_keys ck
LEFT JOIN workflow_instances wi ON ck.workflow_id = wi.id
WHERE wi.id IS NULL;
```

**Re-running the migration**: The migration runner prevents re-execution via
`schema_migrations` tracking. If manually re-applied: the Postgres DO block is
idempotent (DROP + re-ADD arrives at the same state); MSSQL `IF EXISTS` guards
make it idempotent; MySQL re-application would fail on the `ADD FOREIGN KEY`
step for concurrency_keys (FK already exists). Do not re-apply migration 007
manually.

**Rollback**: Migration 007 has no automatic rollback. Once CASCADE is applied,
deletes are silently destructive. To undo, re-apply the constraints without
`ON DELETE CASCADE` (reverse of the migration DDL). Contact support for a
rollback script if needed.

## Running old and new workers side by side

Because workers are stateless and read workflow definitions from the database,
old and new worker binaries can coexist during a rollout. The key compatibility
rules are:

### Database schema compatibility

- **Minor/patch upgrades**: database schema changes are backward compatible.
  Old workers ignore new columns and new tables. New workers handle the older
  schema because migrations add columns with NULL defaults.
- **Major upgrades**: the schema may change in non-backward-compatible ways.
  Migrations are applied before any worker connects, so all workers see the
  same schema. See [Major version upgrades](#major-version-upgrades).

### Workflow definition compatibility

- Old workers can execute workflows deployed as WASM blobs at any version.
  The WASM binary is self-contained with its own host call imports.
- New workers can execute older WASM blobs because the host call interface
  is backward compatible within the same major version.
- If the new binary introduces new host functions, old workers may fail to
  execute workflows that use them. In practice, this is avoided because old
  workers drain before new workflows are deployed.

### Practical rollout window

```bash
# Phase 1: both worker versions run concurrently
# Old workers (v1) handling existing workflows
cleat-worker-v1 --db "$DATABASE_URL"

# New workers (v2) also connect and claim from the queue
cleat-worker-v2 --db "$DATABASE_URL"

# Phase 2: old workers are drained (see zero-downtime deploy guide)
kill -TERM $(pgrep cleat-worker-v1)

# Phase 3: only new workers remain
cleat-worker-v2 --db "$DATABASE_URL"
```

During the coexistence window:

- Both worker versions claim from the same `SELECT ... FOR UPDATE SKIP LOCKED`
  queue. Each instance is claimed by exactly one worker.
- New workers execute WASM modules at their recorded version, which has been
  compiled against the host interface of the worker that compiled it.
- No workflow runs on two different binaries at the same time; each instance is
  assigned to a single worker and replayed there.

## Rolling back a worker upgrade

If a worker binary upgrade causes issues, roll back by restarting the previous
binary:

### Manual rollback

```bash
# 1. Stop the new worker
kill -TERM $(pgrep cleat-worker)

# 2. Install the previous binary
cp cleat-worker-v1 /usr/local/bin/cleat-worker

# 3. Restart
cleat-worker --db "$DATABASE_URL"
```

### Kubernetes rollback

```bash
# Rollback to the previous revision
kubectl rollout undo deployment/cleat-worker

# Or rollback to a specific revision
kubectl rollout undo deployment/cleat-worker --to-revision=3

# Verify the rollback
kubectl rollout status deployment/cleat-worker
```

### Rolling back a schema migration

If a database migration is the source of the problem, you can roll it back
using the down migration:

```bash
# Rollback the last migration
cleat migrate down --db "$DATABASE_URL"

# Rollback to a specific version
cleat migrate down --db "$DATABASE_URL" --target 001
```

After the migration rollback, start the old worker binary:

```bash
cleat-worker-v1 --db "$DATABASE_URL"
```

**Important**: Rolling back a migration may cause data loss if the rolled-back
migration added columns or tables that are now in use. Down migrations should
be tested in a staging environment before production use.

## Rolling back a workflow definition version

Cleat stores WASM blobs in the database with version numbers. Each
`cleat deploy` creates a new version. The `cleat rollback` command lets you
revert a workflow definition to a previous version:

```bash
# List versions of a workflow
cleat versions place_order

# Output:
# Version 3 (latest) - deployed 2025-03-01
# Version 2 - deployed 2025-02-15
# Version 1 - deployed 2025-02-01

# Rollback to version 2
cleat rollback place_order 2
```

After rollback:

- New workflow instances of `place_order` use version 2.
- Running instances at version 3 continue until they complete. If a running
  instance needs to replay (e.g., after recovery), it replays using the version
  recorded in `workflow_instances.def_version`, which is version 3.
- Completed and failed instances are unaffected.
- Version 3 remains in the database and can be restored with another rollback.

### What `cleat rollback` does

`cleat rollback` updates the active version pointer in `workflow_defs`. It
does **not**:

- Terminate running instances
- Delete the newer version from the database
- Change the WASM binary stored for any version
- Replay completed instances

The active version is used only for **new** workflow instances. Running
instances continue with the version they started on, which is correct for
deterministic replay.

### Rollback considerations

- **WASM compatibility**: the rolled-back version must be compatible with the
  worker binary version running in the pool. If the worker binary was also
  upgraded, check that the old WASM blob imports host functions that exist in
  the current worker.
- **Input schema**: if the input format changed between versions, new instances
  created with the old version must receive input in the old format. Update any
  callers or API consumers accordingly.
- **Side effects**: rollback changes the behavior of new instances only.
  Existing running instances at the newer version continue with that version's
  logic. This is intentional and safe because replay must be deterministic.

## PostgreSQL major version upgrade

Cleat requires PostgreSQL 14+. When upgrading PostgreSQL to a new major version,
follow this procedure.

### Procedure

#### 1. Schedule maintenance window

PostgreSQL major version upgrades require downtime because:

- The database must be stopped during the upgrade
- System catalogs are rewritten (cannot run in-place)
- Cleat workers cannot operate without a database connection

Plan for the upgrade to take 2-10x longer than a `pg_dump | pg_restore` of
your database size (system catalog upgrade is I/O intensive).

#### 2. Drain workers

Drain the worker pool before taking the database offline:

```bash
# Set all workers to drain mode via the admin API
curl -X POST http://localhost:8080/api/admin/drain

# Or send SIGTERM to each worker
pkill -TERM cleat-worker

# Wait for all workers to exit (check with pgrep)
```

#### 3. Verify no in-flight workflows

```sql
SELECT COUNT(*) FROM workflow_instances WHERE status = 'running';
```

If any workflows are still running, wait for the drain to complete before
proceeding.

#### 4. Shut down the old PostgreSQL

```bash
pg_ctlcluster 15 main stop
```

#### 5. Run pg_upgrade

Use `pg_upgrade` (recommended) for in-place major version upgrades:

```bash
# Install the new PostgreSQL version alongside the old one
# (exact package names vary by distribution)

# Run pg_upgrade
pg_upgrade \
    --old-datadir /var/lib/postgresql/15/main \
    --new-datadir /var/lib/postgresql/16/main \
    --old-bindir /usr/lib/postgresql/15/bin \
    --new-bindir /usr/lib/postgresql/16/bin \
    --link

# The --link flag uses hard links to avoid copying data files,
# making the upgrade near-instantaneous for large databases.

# If --link is not available, use --copy (slower but safer
# when filesystems differ).
```

#### 6. Update connection strings

Update the database URL in all worker configurations, environment variables,
and deployment manifests to point to the new PostgreSQL version:

```bash
export CLEAT_DATABASE_URL="postgres://user:pass@db-host:5433/cleat?sslmode=require"
```

Note the port change: PostgreSQL 16 defaults to port 5433 if installed
alongside version 15 on the same host.

#### 7. Start workers

Restart the cleat worker pool:

```bash
cleat-worker --db "$CLEAT_DATABASE_URL"
```

#### 8. Verify

```sql
-- Check that all tables are accessible
SELECT COUNT(*) FROM workflow_instances;
SELECT COUNT(*) FROM event_history;

-- Check the reaper reclaims any stale instances
SELECT id, status, assigned_to, heartbeat_at
FROM workflow_instances
WHERE status = 'running'
  AND heartbeat_at < NOW() - INTERVAL '30 seconds';
```

#### 9. Remove old PostgreSQL cluster

After confirming the upgrade is stable:

```bash
pg_dropcluster 15 main
```

### Alternative: pg_dump/pg_restore

For very large databases, `pg_upgrade` is significantly faster. However,
`pg_dump` / `pg_restore` is a viable alternative:

```bash
# On old server
pg_dump "postgres://user:pass@old-host:5432/cleat?sslmode=require" \
    -Fc -f cleat-backup.dump

# On new server
pg_restore "postgres://user:pass@new-host:5432/cleat?sslmode=require" \
    -d cleat cleat-backup.dump
```

After restore, all `running` instances are stale. The reaper reclaims them
within 60 seconds, and they replay from their event history.

### Compatibility checklist

Before upgrading PostgreSQL, verify:

- [ ] The new PostgreSQL version is 14+ (cleat requirement)
- [ ] The `pg_upgrade` path from your current version to the target version
      is supported (see [PostgreSQL documentation](https://www.postgresql.org/docs/current/pgupgrade.html))
- [ ] All workers have been drained
- [ ] A full backup exists before the upgrade
- [ ] Connection strings have been updated to point to the new server
- [ ] The new server has the same extensions installed (`pgcrypto` for UUID
      generation, if used)
- [ ] Indexes are rebuilt (pg_upgrade with `--link` does not rebuild indexes;
      run `REINDEX` after upgrade for optimal performance)

## Related guides

- [Zero-downtime deployment](zero-downtime-deploy.md) -- blue/green worker pool
  replacement with no downtime
- [Disaster recovery](disaster-recovery.md) -- recovery from full database
  restore, RPO/RTO, and cross-region failover
- [Deploying to production](deploying-to-production.md) -- configuration,
  monitoring, scaling, and health checks
