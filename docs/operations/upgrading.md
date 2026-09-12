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

### Lock risk: set `lock_timeout` yourself, because no migration sets it

**Nothing in this repo sets `lock_timeout` or `lock_wait_timeout`.** A session
value you set before running the migrations is the only thing bounding how long
a migration waits for its lock — and how long your writers queue behind it.

Re-derive rather than trusting this paragraph; it is the kind of claim that
rots, and the version of this section before cleat#1334 asserted the opposite:

```
git grep -c lock_timeout -- migrations/ migration/ '*.go' plugins/
# no matches. Every occurrence in the repo is prose.
```

**Which migrations are exposed**: every one that runs `ALTER TABLE`. Do not work
from a list — it grows with each release, and the risk is not confined to the
expensive statements. `docs/schema-partitioning-design.md` measures a
*metadata-only* `ALTER` queued behind a single 10-second open reader at
`0.08s → 9.43s`, with `SELECT`s arriving after it blocked for 7.8–8.4s:

> Outage length is set by your longest open transaction, not by the DDL.

So an `ADD COLUMN` with no default is exposed on the same terms as a constraint
rebuild. List them for a given release with:

```
git diff --name-only <previous-tag>..<this-tag> -- migrations/ |
  while read -r f; do grep -l 'ALTER TABLE' "$f"; done
```

**Mitigation**:
- **Set the timeout on the connection the migration tool makes, not in a `psql`
  session.** This is the protection, not a supplement to one — and the
  distinction is the part that is easy to get wrong. Migrations are applied by
  the worker at startup (`cleat-worker --db "$DATABASE_URL"`), which opens its
  own connection from the DSN; a `SET lock_timeout = '5s';` you type into a
  separate `psql` session has no effect on it whatsoever. Put it in the DSN or
  the environment:

  ```
  # Postgres. lib/pq forwards `options` in the startup packet and reads
  # PGOPTIONS (connector.go: `Options string `postgres:"options" env:"PGOPTIONS"``).
  DATABASE_URL='postgres://.../cleat?options=-c%20lock_timeout%3D5s'
  # or, equivalently:
  PGOPTIONS='-c lock_timeout=5s' cleat-worker --db "$DATABASE_URL"

  # MySQL. go-sql-driver sends unrecognised DSN parameters as session
  # system variables on connect (dsn.go: `Params map[string]string`).
  DATABASE_URL='user:pw@tcp(host:3306)/cleat?lock_wait_timeout=5'
  ```

  A migration that cannot take its lock then fails fast and can be retried,
  instead of holding the write queue open behind it.
- **Check for long-running transactions first**, since they are what the
  timeout is protecting you from:
  ```sql
  SELECT pid, state, now() - xact_start AS age, query
  FROM pg_stat_activity
  WHERE xact_start IS NOT NULL AND now() - xact_start > interval '30 seconds'
  ORDER BY age DESC;
  ```
- Run during a maintenance window or off-peak hours.
- On Postgres each migration file runs in a single transaction, so a failed
  migration leaves no intermediate state visible to other sessions.
- On MySQL, `ALTER TABLE` implicitly commits. A migration that rebuilds a
  foreign key therefore has a brief window with no constraint in force, and a
  failure part-way through leaves the earlier statements applied.

**What this section used to say, recorded because it was actively harmful.**
Until cleat#1334 this was headed "Migration 007: Foreign Key CASCADE" and told
operators that the Postgres migration set `SET LOCAL lock_timeout = '30s'`
inside a `DO` block, that this *overrode* any session value they set, and that
shortening it meant editing the migration SQL.

Every part of that was false. There is no migration 007 on either dialect
(`ls migrations/postgres/007*`), the five `ON DELETE CASCADE` foreign keys it
claimed to add are declared in `001_schema.sql` from the beginning, and no
migration has ever set `lock_timeout`. The cost was not the missing guard on its
own: an operator who did the right thing was told their setting was inert, which
is a gap plus a reason not to look for it.

**Its second bullet was inert too, which is why this section is rewritten rather
than patched.** That one read "Set `lock_timeout` before running for the
migration runner's *other* statements: `SET lock_timeout = '5s';`" — correct
advice in the wrong place twice over. It scoped the setting to the statements
the phantom `DO` block supposedly did not cover, and it named a bare `SET`,
which applies to whichever session runs it. The migration runner connects from
the DSN, so a value set anywhere else never reaches it. Both bullets pointed an
operator away from the only thing that works.

Three further paragraphs went with it -- a pre-migration orphan check for
`concurrency_keys`, a "do not re-apply migration 007 manually" warning citing
the idempotency of its Postgres `DO` block and its MSSQL `IF EXISTS` guards, and
a rollback procedure for undoing the CASCADE. All three described the same
migration, so all three were instructions about a file that is not there.

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

Cleat requires PostgreSQL 16+. When upgrading PostgreSQL to a new major version,
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

- [ ] The new PostgreSQL version is 16+ (cleat requirement, per `tiers.yaml`'s
      `dialect_versions` -- corrected 2026-08-09; this checklist disagreed
      with the "PostgreSQL 16+" stated earlier in this same file)
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

## RLS Behavior Change: Fail-Open to Fail-Closed (v2.x)

### What changed

Row-level security (RLS) policies previously used a default-tenant fallback when
no tenant context was set. A query without `cleat.tenant_id` would silently
return data for the default tenant (`00000000-0000-0000-0000-000000000000`).

As of this release, queries without tenant context **fail with an error**:
- **PostgreSQL**: The RLS policy calls `cleat.assert_tenant_set()`, which throws
  `cleat.tenant_id is not set — tenant context required for RLS-scoped query`.
- **MSSQL**: The application layer returns an error (`tenant ID must be set
  before setting session context for an RLS-scoped transaction`) before any
  query reaches the database. The MSSQL RLS security policy was already
  fail-closed (NULL SESSION_CONTEXT blocks all rows), but the error was silent.

### Migration

Run migration `008_rls_fail_closed.sql`. The migration is idempotent:
- Creates or replaces the `cleat.assert_tenant_set()` function.
- Recreates RLS policies to use the new assert function (Postgres).

No application code changes are required for normal operation. The existing
`WithTenant()` pattern in the dispatch loop already sets tenant context before
every workflow operation.

### Impact on direct database access

Administrative queries that bypass the application (e.g., `psql` for debugging)
will fail against RLS-protected tables unless tenant context is set:

```sql
-- Before running queries against RLS-protected tables:
SELECT set_config('cleat.tenant_id', '<tenant-uuid>', false);
```

### Migration ordering

Migrations must be applied in order. Do not re-run migration `002_constraints.sql`
after migration `008_rls_fail_closed.sql` without first manually dropping the RLS
policies, because `002_constraints.sql` uses bare `CREATE POLICY` without
`DROP POLICY IF EXISTS` guards.

### MSSQL limitation

MSSQL security policies require inline table-valued functions for filter
predicates, which cannot throw errors. The MSSQL RLS policy remains silently
fail-closed. The application-layer error provides the explicit failure.
