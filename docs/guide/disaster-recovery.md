# Disaster recovery

This guide covers recovery procedures for cleat deployments after a database
failure, including full restore from backup (RPO/RTO), cross-region failover,
and the implications of replaying workflows from event history after a restore.

## Recovery procedure from a full database restore

Cleat stores all workflow state in PostgreSQL. If the database is lost or
corrupted, recovery consists of restoring from backup and letting the worker
pool reclaim and replay stale workflows.

### 1. Restore from pg_dump or PITR

#### Restore from pg_dump

```bash
# Restore the latest backup
pg_restore "postgres://user:pass@db-host:5432/cleat?sslmode=require" \
    -d cleat cleat-backup-20250301.dump

# Or from a plain SQL dump
psql "postgres://user:pass@db-host:5432/cleat?sslmode=require" \
    -f cleat-backup-20250301.sql
```

#### Restore from point-in-time recovery (PITR)

If you have WAL archiving configured, use PITR to recover to the latest
possible transaction:

```bash
# Restore the base backup
pg_basebackup -D /var/lib/postgresql/16/main \
    -X fetch -P

# Configure recovery.conf or recovery.signal (PostgreSQL 12+)
# to replay WAL up to the point of failure

# Start PostgreSQL -- it replays WAL automatically
pg_ctlcluster 16 main start
```

PITR provides the lowest data loss because it replays WAL segments up to the
moment of failure. The RPO is determined by the WAL archiving frequency (see
[RPO and RTO](#rpo-and-rto)).

### 2. All running workflows at backup time are now stale

After a restore, every workflow instance with `status = 'running'` is stale:

```sql
-- Before starting workers, inspect stale instances
SELECT id, def_name, assigned_to, heartbeat_at
FROM workflow_instances
WHERE status = 'running';
```

These instances were assigned to worker processes that no longer exist (or
whose heartbeat state is from a past backup). The restore does not carry
forward live connection state, so from the database's perspective, every
running instance has an expired heartbeat.

Important properties of stale instances:

- Their `assigned_to` field may reference a worker that is no longer running
- Their `heartbeat_at` timestamp is from before the backup (or PITR recovery
  point)
- Their `event_history` contains the full ordered list of every DurableCall,
  sleep, signal, and child workflow event that occurred before the backup
- No data is lost from completed or failed instances

### 3. Start workers; the reaper reclaims stale instances within 60 seconds

Start the cleat worker pool pointing at the restored database:

```bash
cleat-worker --db "$CLEAT_DATABASE_URL" --concurrency 20
```

The worker pool includes a **reaper** goroutine that runs every 10 seconds. On
each cycle, the reaper:

1. Queries for `running` instances where `heartbeat_at` is older than the
   heartbeat interval (default: 30 seconds) plus a grace period
2. Resets `assigned_to` to `NULL` and sets `heartbeat_at` to a past timestamp
3. Sets `status` to `ready` so the instances are picked up by the claim loop

```sql
-- The reaper runs this query on each cycle:
UPDATE workflow_instances
SET
    assigned_to = NULL,
    heartbeat_at = '1970-01-01T00:00:00Z',
    status = 'ready'
WHERE status = 'running'
  AND heartbeat_at < NOW() - INTERVAL '30 seconds'
RETURNING id;
```

Since all instances in the restored backup have `heartbeat_at` older than the
grace period, the reaper reclaims every stale instance within its first few
cycles. In practice, all instances are reclaimed within **60 seconds** of the
first worker starting.

To monitor the reaper's progress:

```bash
# Check instances transitioning from running to ready
curl http://localhost:8080/metrics | grep cleat_workflows_reclaimed
```

Or query the database directly:

```sql
-- After workers have been running for 60 seconds:
SELECT status, COUNT(*) FROM workflow_instances GROUP BY status;
```

### 4. Reclaimed workflows replay from event history

Once a stale instance is reclaimed (status set to `ready`), a worker claims it
and begins execution. Because the instance has an existing `event_history`, the
worker enters **replay mode**:

1. The worker loads the WASM module for `def_version` (stored in
   `workflow_defs`)
2. The worker initializes the WASM runtime with the instance's input
3. The worker walks through `event_history` in step order
4. For each completed event (e.g., a `DurableCall` with a response), the worker
   returns the cached response to the WASM module
5. The first uncompleted event becomes the current execution point -- the
   workflow continues from where it left off

This means:

- **Completed DurableCalls** return their cached responses -- no external
  service is called again
- **DurableSleep** calls that have elapsed are skipped (the cached wake-up time
  is in the past)
- **AwaitSignals** that already received a signal return the cached signal
  payload
- **The workflow arrives at its pre-failure state** and continues execution

#### Important: replay may produce different outcomes

If external services' state has changed between the backup and the recovery,
the workflow's behavior after replay may differ from the original execution.
Consider these scenarios:

**Scenario A: Idempotent service**

Your payment service accepts `idempotency_key` and returns the same result
on duplicate calls. During replay, `DurableCall("payments", "Charge", ...)`
returns the cached response. The service is never called. Result: correct.

**Scenario B: Non-idempotent service with state change**

A workflow called `DurableCall("shipping", "CreateShipment", ...)` and the
response was recorded in event history. The shipment was delivered before the
database failure. After restore and replay, the workflow continues past the
shipment call and attempts to call `DurableCall("notifications", "SendEmail", ...)`.
The email service still works. Result: correct.

**Scenario C: External state changed during downtime**

A workflow was awaiting a signal (`AwaitSignals("payment_received", ...)`) when
the database failed. After restore, the signal was sent during the downtime.
The event history does not contain the signal because it was sent after the
backup. After replay, the workflow resumes waiting for the signal. If the
external system considers the payment complete, it may never resend the signal.
Result: the workflow hangs until the signal timeout, then fails.

**Mitigation for Scenario C:**

- Configure alerting on workflows that exceed expected execution duration
- After recovery, check for workflows in `suspended` or `running` state that
  are waiting on external signals and verify the external system state matches
- If needed, resend signals via the REST API:

```bash
curl -X POST http://localhost:8080/api/workflows/<workflow_id>/signal \
    -H "Content-Type: application/json" \
    -d '{"signal_name": "payment_received", "payload": {...}}'
```

### 5. Verify recovery

After recovery, verify the system is healthy:

```bash
# Check all workflow distribution
curl http://localhost:8080/api/admin/stats

# Check for stuck workflows
curl http://localhost:8080/api/workflows?status=running&since=1h
```

```sql
-- Verify no instances are stuck in running with stale heartbeats
SELECT id, def_name, heartbeat_at
FROM workflow_instances
WHERE status = 'running'
  AND heartbeat_at < NOW() - INTERVAL '60 seconds';

-- Should return zero rows if reaper has cycled
```

## RPO and RTO

### Recovery Point Objective (RPO)

RPO depends entirely on your PostgreSQL backup frequency:

| Backup strategy | Typical RPO | Notes |
|----------------|-------------|-------|
| Daily pg_dump | 24 hours | Up to 24 hours of data loss |
| Hourly pg_dump | 1 hour | Trades backup load for lower RPO |
| Continuous WAL archiving (PITR) | Minutes (configurable) | RPO = WAL archive interval |
| Synchronous replica | Zero | No data loss; separate deployment |

**PGDump RPO formula**:

```
RPO = time_since_last_backup + time_to_restore
```

For example, with hourly pg_dump at :00 past each hour, a failure at 01:45
means up to 1 hour 45 minutes of data loss (last backup at 00:00, plus 45
minutes until the 01:00 backup was taken).

**PITR RPO formula**:

```
RPO = WAL_archive_frequency + replication_lag
```

With WAL archiving every 5 minutes, RPO is typically under 5 minutes.

### Recovery Time Objective (RTO)

RTO depends on the database restore time plus the reaper interval:

```
RTO = database_restore_time + 60_seconds
```

**Database restore time factors:**

| Factor | Impact |
|--------|--------|
| Database size | Larger databases take longer to restore |
| Backup format | `pg_restore -j` (parallel jobs) is faster than plain SQL |
| Storage speed | SSD vs. HDD, local vs. network storage |
| PITR WAL volume | More WAL segments to replay = longer recovery |

**Estimated restore times:**

| Database size | pg_dump restore (1 thread) | pg_restore (4 threads) | PITR replay |
|--------------|---------------------------|------------------------|-------------|
| 10 GB | 2-5 min | 1-2 min | 1-5 min |
| 100 GB | 20-40 min | 8-15 min | 10-30 min |
| 1 TB | 3-6 hours | 45-90 min | 1-3 hours |

**Reaper interval:**

The reaper runs every 10 seconds and reclaims all instances with stale
heartbeats. Since every instance in a restored backup has a stale heartbeat,
the reaper reclaims all of them within at most one reaper cycle after startup.
Adding startup time and the first claim cycle, the practical upper bound is
**60 seconds** of reaper overhead.

### Improving RPO and RTO

| Goal | Strategy |
|------|----------|
| Lower RPO | Increase backup frequency or enable continuous WAL archiving |
| Lower RTO | Use pg_restore with parallel jobs, smaller backup windows, faster storage |
| Both | Deploy a warm standby with streaming replication (see [Cross-region DR](#cross-region-disaster-recovery)) |

## Cross-region disaster recovery

For deployments that require recovery from a regional outage, cleat supports a
warm standby architecture using PostgreSQL streaming replication.

### Architecture

```
Primary region (us-east-1)           Standby region (us-west-2)
┌─────────────────────────────┐      ┌──────────────────────────────┐
│  Worker pool                │      │  Worker pool (read-only)     │
│  cleat-worker instances     │      │  cleat-worker instances      │
│       │                     │      │       │                      │
│       ▼                     │      │       ▼                      │
│  PostgreSQL primary (5432)  │◄─────│  PostgreSQL standby (5432)   │
│  Read/write                 │ WAL  │  Read-only, hot standby      │
└─────────────────────────────┘      └──────────────────────────────┘
```

### Setting up cross-region replication

#### 1. Configure the primary

In `postgresql.conf` on the primary:

```
wal_level = replica
max_wal_senders = 5
wal_keep_size = 1024    # Keep 1 GB of WAL for standby
```

#### 2. Create a replication user

```sql
CREATE ROLE replicator WITH LOGIN REPLICATION PASSWORD 'strong-password';
GRANT CONNECT ON DATABASE cleat TO replicator;
```

#### 3. Set up the standby

On the standby server, take a base backup and configure streaming replication:

```bash
# Take a base backup from the primary
pg_basebackup -h primary-host -D /var/lib/postgresql/16/main \
    -U replicator -X stream -P

# Create standby.signal file to mark this as a standby
touch /var/lib/postgresql/16/main/standby.signal

# Configure primary_conninfo in postgresql.conf
# (On PostgreSQL 12+, this goes in postgresql.conf, not recovery.conf)
```

In `postgresql.conf` on the standby:

```
primary_conninfo = 'host=primary-host port=5432 user=replicator password=strong-password sslmode=require'
primary_slot_name = 'standby_slot_west'
hot_standby = on
```

#### 4. Start the standby

```bash
pg_ctlcluster 16 main start
```

Verify replication is working:

```sql
-- On the primary:
SELECT slot_name, slot_type, active_pid, restart_lsn
FROM pg_replication_slots;

-- On the standby:
SELECT pg_is_in_recovery(), pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn();
```

### Workers in the standby region

In normal operation, the standby region runs workers in a **read-only** mode.
They connect to the standby database but do not claim or execute workflows:

```bash
# Standby region workers (read-only monitoring)
cleat-worker --db "$STANDBY_DATABASE_URL" --read-only
```

In read-only mode, workers:

- Expose health check and metrics endpoints
- Serve the web UI and REST API (read-only queries)
- Monitor workflow status
- Do **not** claim instances from the queue
- Do **not** execute workflow instances
- Do **not** write to the database

This gives the standby region observability into workflow state without
splitting the claim pool.

### Failing over to the standby region

When the primary region becomes unavailable, promote the standby to primary:

#### 1. Promote the standby

```bash
# Promote the standby to primary (read-write)
pg_ctlcluster 16 main promote
```

Or using `pg_ctl`:

```bash
pg_ctl promote -D /var/lib/postgresql/16/main
```

After promotion:

- The standby becomes a writable primary
- Streaming replication from the old primary stops
- The old primary (if it recovers) should NOT reconnect to avoid a split-brain

#### 2. Redirect workers

Update the worker pool in the standby region to connect to the newly promoted
primary in read-write mode:

```bash
# Workers in the standby region switch from read-only to full mode
cleat-worker --db "$PROMOTED_DATABASE_URL" --concurrency 20
```

Or, if using a DNS-based connection string, update the DNS record to point to
the promoted database and restart workers:

```bash
# After updating DNS, restart the worker pool
pkill -TERM cleat-worker
cleat-worker --db "$CLEAT_DATABASE_URL"
```

#### 3. Verify failover

```sql
-- The promoted standby should no longer be in recovery
SELECT pg_is_in_recovery();  -- false

-- Check that all tables are accessible
SELECT COUNT(*) FROM workflow_instances;
SELECT COUNT(*) FROM event_history;
```

#### 4. Let the reaper reclaim stale instances

During the failover window, running workflows became stale. The reaper reclaims
them within 60 seconds of the first worker starting. See [Recovery procedure from a full database restore](#recovery-procedure-from-a-full-database-restore) for details.

### Failing back to the primary region

After the original primary region is restored, fail back by reversing the
replication direction:

1. Set up the original primary as a standby replicating from the promoted
   standby (now the primary)
2. Wait for replication to catch up
3. Promote the original primary back to primary
4. Redirect workers back to the original primary

```bash
# On the original primary (which is now a standby candidate):
pg_ctlcluster 16 main stop

# Wipe and re-replicate from the promoted standby
rm -rf /var/lib/postgresql/16/main
pg_basebackup -h promoted-standby-host -D /var/lib/postgresql/16/main \
    -U replicator -X stream -P
touch /var/lib/postgresql/16/main/standby.signal
pg_ctlcluster 16 main start

# Once caught up, promote back:
pg_ctlcluster 16 main promote
```

During failback, the reaper again reclaims any instances that became stale
during the transition.

### Data loss on failover

Hot standby replication is **asynchronous** by default. If the primary fails
before transmitting all WAL to the standby, there is data loss:

```
Primary: [txn A] [txn B] [txn C] [CRASH]
Standby: [txn A] [txn B]
                        ^--- Transactions C is lost
```

To reduce data loss:

- Use **synchronous replication** for zero RPO (at the cost of higher write
  latency)
- Monitor replication lag and alert when it exceeds your RPO threshold

```sql
-- Check replication lag on the standby
SELECT
    pg_size_pretty(pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn())) AS replay_lag,
    pg_size_pretty(pg_wal_lsn_diff(pg_last_wal_receive_lsn(), '0/00000000')) AS receive_lag;
```

### Split-brain prevention

When failing over, ensure the old primary is isolated and cannot accept writes:

- Cut network connectivity to the old primary region
- Verify the old primary is shut down (`pg_ctlcluster 16 main stop`)
- If using a virtual IP or DNS, ensure it points only to the new primary

After the old primary is recovered, treat it as a standby candidate and verify
replication catches up before promoting it.

## Recovery testing

Regularly test your disaster recovery procedure to ensure it works:

### Quarterly DR drill checklist

- [ ] Restore the latest backup to a staging environment
- [ ] Start workers and verify the reaper reclaims stale instances
- [ ] Verify all reclaimed workflows complete successfully
- [ ] Verify that workflows waiting on external signals recover correctly
- [ ] Measure RTO (restore start to last workflow completing)
- [ ] Practice a cross-region failover (promote standby, redirect workers)
- [ ] Practice a failback (re-replicate original primary, promote, redirect)
- [ ] Document any gaps or improvements needed

### Automated recovery validation

For CI/CD integration, validate backups automatically:

```bash
# Restore the latest backup into a temporary database
createdb cleat_drill_$(date +%Y%m%d)
pg_restore -d cleat_drill_$(date +%Y%m%d) latest-backup.dump

# Start a worker in dry-run mode to verify replay
cleat-worker --db "postgres://user:pass@localhost/cleat_drill_$(date +%Y%m%d)" \
    --concurrency 1 --dry-run

# Clean up
dropdb cleat_drill_$(date +%Y%m%d)
```

## Related guides

- [Upgrading cleat](upgrading.md) -- worker binary upgrades, schema migrations,
  PostgreSQL major version upgrades, and rollback procedures
- [Zero-downtime deployment](zero-downtime-deploy.md) -- blue/green worker pool
  replacement with no downtime
- [Deploying to production](deploying-to-production.md) -- backup configuration,
  monitoring, and health checks
