# Upgrading cleat

This guide covers all upgrade scenarios for a cleat deployment: worker binary
upgrades, database schema migrations, workflow definition version changes,
rollback procedures, and PostgreSQL major version upgrades.

## Migration is a deploy step

**A worker does not migrate the database when it starts** (cleat#2117; this changed
in 0.3.0). A normal start *verifies* that the schema is not behind the binary and
refuses to start, with the remediation in its message, if it is. So every release
migrates first, then rolls the workers:

```bash
# 1. Once per release, from anywhere that can reach the database. Use a role with
#    DDL rights (--migrate-db), which the workers' runtime role should not have.
cleat-worker --migrate-only --db "$CLEAT_DATABASE_URL" [--migrate-db "$MIGRATOR_DATABASE_URL"]

# 2. Then start or restart the workers (the rolling restart below).
```

`--migrate-only` applies the core and plugin migrations and exits `0`; any failure is
non-zero. It is idempotent, needs no master key, and is safe if two run at once. How
each deployment shape does step 1:

| deployment | the migration step |
|---|---|
| Helm | a `pre-install`/`pre-upgrade` hook Job (`charts/cleat/templates/migrate-job.yaml`); `migration.*` values |
| raw Kubernetes | apply `k8s/migrate-job.yaml`, `kubectl wait`, then apply the Deployment |
| systemd / .deb | `ExecStartPre=cleat-worker --migrate-only` in the shipped unit |
| docker compose | a one-shot `migrate` service the workers `depends_on` (`docker-compose.cluster.yml`) |
| single node, development | `cleat-worker --migrate-on-start`: the worker migrates itself, as before |

**A schema ahead of the binary starts.** During a rolling upgrade the migration job
runs before the last old workers have been replaced; those workers see a schema newer
than they know. They start, with a warning naming both versions, rather than refuse and
wedge the rollout. This relies on migrations staying additive within a release line.
A schema *behind* the binary is refused.

**Before you upgrade from a release that migrated on start:** anything that started a
worker against a fresh or older database and relied on it migrating now needs a
`--migrate-only` step or `--migrate-on-start`. A worker started without either on an
un-migrated database exits with the message above.

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
cleat-worker --db "$CLEAT_DATABASE_URL" --concurrency 20
```

The worker on SIGTERM will:

1. Stop claiming new workflow instances from the database
2. Wait for all in-flight workflow executions to complete, for at most
   `--shutdown-grace` (default 20s)
3. Cancel whatever is still running and **release** it (never fail it) by
   clearing `assigned_to`, so another worker replays it from its durable history
4. Exit

Other workers in the pool immediately pick up any instances the shutting-down
worker releases. A durable call that was still running when the grace ended can
run again on the worker that picks the run up (at-least-once); see
[What SIGTERM does](zero-downtime-deploy.md#what-sigterm-does).

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
cleat-worker --db "$CLEAT_DATABASE_URL" --concurrency 20 --new-flag value

# Bad: mismatched binary and config
# cleat-worker v1 with --new-flag => error
# cleat-worker v2 without --new-flag => uses default
```

## Database schema migration

### Automatic migration (recommended)

**0.3.0 requires a fresh database. There is no upgrade path from v0.2.0** --
the schema rebaseline (`event_history` partitioning plus migration
compaction, cleat#2059) breaks compatibility with any pre-0.3.0 database on
purpose, and is the last change before the 0.3.0 tag. Provision a fresh
database for 0.3.0; there is no cleat v0.7.0, and no version of cleat before
0.3.0 to migrate from.

The guidance below describes ordinary migrations between later releases,
once those exist: migrate as a deploy step (see
[Migration is a deploy step](#migration-is-a-deploy-step)), then start the workers,
which verify the schema at startup and refuse to start if it is behind:

```bash
cleat-worker --migrate-only --db "$CLEAT_DATABASE_URL"
cleat-worker --db "$CLEAT_DATABASE_URL"     # verifies; does not migrate
```

The migration run logs what it applies:

```
INFO[0000] Applied schema migration 002_add_promises_table  duration=12ms
INFO[0000] Schema is up to date at version 003
```

### The migration command

**Migrations are applied by `cleat-worker --migrate-only`** (cleat#2117), a deploy
step described [above](#migration-is-a-deploy-step). It builds the same runner a worker
used to run at startup, applies every pending migration (core, then plugin) and exits.
There is still no `migrate` subcommand on `cleat` or on `cleatctl` (cleat#1315); an
older version of this section offered `cleat migrate up` and `cleat migrate status`,
which never existed. Check the surface rather than trusting this paragraph:

```bash
cleat 2>&1 | grep 'Valid commands'
```

Migrations are idempotent: the runner records each applied version in
`schema_migrations` and skips it thereafter, so running `--migrate-only` repeatedly
applies nothing twice.

**To see what has been applied**, read the tracking table directly:

```sql
SELECT version, applied_at FROM schema_migrations ORDER BY version;
```

**To see what a worker would refuse**, start it without `--migrate-on-start`: it
verifies the schema, changes nothing, and if a migration is missing says which.

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
  `cleat-worker --migrate-only --db "$CLEAT_DATABASE_URL"` (or by a `--migrate-on-start` worker), which opens its
  own connection from the DSN; a `SET lock_timeout = '5s';` you type into a
  separate `psql` session has no effect on it whatsoever. Put it in the DSN or
  the environment:

  ```
  # Postgres. lib/pq forwards `options` in the startup packet and reads
  # PGOPTIONS (connector.go: `Options string `postgres:"options" env:"PGOPTIONS"``).
  CLEAT_DATABASE_URL='postgres://.../cleat?options=-c%20lock_timeout%3D5s'
  # or, equivalently:
  PGOPTIONS='-c lock_timeout=5s' cleat-worker --migrate-only --db "$CLEAT_DATABASE_URL"

  # MySQL. go-sql-driver sends unrecognised DSN parameters as session
  # system variables on connect (dsn.go: `Params map[string]string`).
  CLEAT_DATABASE_URL='user:pw@tcp(host:3306)/cleat?lock_wait_timeout=5'
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
cleat-worker-v1 --db "$CLEAT_DATABASE_URL"

# New workers (v2) also connect and claim from the queue
cleat-worker-v2 --db "$CLEAT_DATABASE_URL"

# Phase 2: old workers are drained (see zero-downtime deploy guide)
kill -TERM $(pgrep cleat-worker-v1)

# Phase 3: only new workers remain
cleat-worker-v2 --db "$CLEAT_DATABASE_URL"
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
cleat-worker --db "$CLEAT_DATABASE_URL"
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

### Rolling back a schema migration — there is no down path

**Schema migrations are one-way. Nothing in cleat reverses them**, and the
answer to "the migration is the problem" is not a command:

| | |
|---|---|
| `*.down.sql` files in the tree | **0** (`git ls-files 'migrations/**/*.down.sql'`) |
| a `Down` function in `migration/runner.go` | none |
| a `migrate` subcommand on `cleat` or `cleatctl` | none |

This section used to prescribe `cleat migrate down --db "$CLEAT_DATABASE_URL"` and
`--target 001`. The command, the flag and the migration files are all absent
(cleat#1315), so an operator reaching for the documented way back was reaching
for something that has never existed — at the moment they could least afford
the detour.

**What to do instead**, in order of preference:

1. **Roll the worker binary back and leave the schema forward.** This is the
   supported path for a minor or patch upgrade, because those schema changes
   are backward compatible by policy — see *Database schema compatibility*
   below. An older worker runs against a newer schema.
2. **Restore from backup** if the schema change itself must be undone. Take the
   backup *before* the upgrade; this is the only way back from a major-version
   schema change, and it is why the checklist asks for one.
3. **Write a forward migration** that undoes what the previous one did, if the
   database cannot be taken offline for a restore. It is a new numbered file,
   not a rollback.

Removing a column or table by hand is not on this list. The schema is reached
by procedures as well as by Go (`finalize_workflow_status` and its siblings),
so a hand-edited schema can satisfy every Go query and still break at a call
this document cannot enumerate.

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

`cleat rollback` writes a routing rule pinning new runs to the named version.

It is a row in `workflow_routing` at weight 1.0, replacing any existing rules
for that workflow in one transaction. That table is what the worker already
consults before falling back to the latest version, so a rollback needs no
separate resolution path. **`workflow_defs` has no active-version column** --
an earlier version of this document said it did, and the command wrote nothing
at all (cleat#1887).

The pin **persists** across later deploys. Deploying a newer version after a
rollback does not re-expose it; run `cleat rollback --clear <name>` to return
the workflow to latest-wins. This is deliberate: a deploy silently clearing the
pin would re-ship the version an operator had withdrawn.

A rollback is refused if the workflow has weighted routing rules, rather than
discarding a live experiment; clear them first.

It does **not**:

- Terminate running instances
- Delete the newer version from the database
- Change the WASM binary stored for any version
- Replay completed instances

The pinned version is used only for **new** workflow instances. Running
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

Cleat requires PostgreSQL 16+, and since migration 077 that is enforced rather
than advisory: the runner refuses to apply anything to an older server, naming
the version and the reason. `WITH INHERIT FALSE` in 077 is PostgreSQL 16 syntax
and carries the cross-tenant isolation boundary -- see
`docs/explanation/postgresql-schema.md`, "PostgreSQL 16 is required".

When upgrading PostgreSQL to a new major version,
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
# Set all workers to drain mode via the admin API (needs --enable-admin-api and an API key:
# see admin-api.md)
curl -X POST -H "Authorization: Bearer $CLEAT_API_KEY" http://localhost:8080/api/admin/drain

# Or send SIGTERM to each worker. It waits up to --shutdown-grace for runs to finish, then exits.
pkill -TERM cleat-worker

# Wait for all workers to exit (check with pgrep). The admin API drain only cordons (stops claiming); it does
# not exit the process.
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
