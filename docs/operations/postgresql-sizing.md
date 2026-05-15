# PostgreSQL Sizing Guide

> Generated: 2026-05-15
> Based on benchmark data from Session C (AMD Ryzen 5 5500U, 12 threads, NVMe SSD, PostgreSQL 16.13)

This guide provides PostgreSQL sizing recommendations for three throughput tiers: 1K, 10K, and 100K workflows per second. These numbers are derived from benchmark results and conservative estimations for production deployments.

---

## Throughput Tiers

### Tier 1: 1,000 workflows/s (Small)

**Workload profile:** ~100 steps per workflow, ~100K events/s total.

| Resource | Recommendation | Rationale |
|----------|---------------|-----------|
| vCPU | 2-4 cores | Claim latency ~146us at 100 concurrent workers. 2 cores handle this comfortably. |
| RAM | 8 GB | Enough for shared_buffers, work_mem per connection, and OS cache. |
| Storage | 100 GB NVMe SSD | At 1KB/event, 100K events/s = 86 GB/day. 100 GB = ~28 hours retention. Scale to 500 GB+ for longer retention. |
| IOPS | 3,000 | Event history INSERT at 5,353 events/s (batch=100) uses ~500 IOPS. Headroom for WAL, checkpoints. |

**Connection pool:** 20-50 connections
- `max_connections`: 50 (leave headroom for admin connections)
- PgBouncer in transaction mode recommended for >50 connections

### Tier 2: 10,000 workflows/s (Medium)

**Workload profile:** ~100 steps per workflow, ~1M events/s total.

| Resource | Recommendation | Rationale |
|----------|---------------|-----------|
| vCPU | 8-16 cores | Claim throughput scales with cores. Need parallel query execution for compaction reads. |
| RAM | 32-64 GB | Larger shared_buffers to cache hot event_history pages. |
| Storage | 1-2 TB NVMe SSD | 1M events/s = 864 GB/day. With 30-day retention: 26 TB. **Partition or reduce retention.** |
| IOPS | 10,000-20,000 | Heavy write workload from event history + compaction + heartbeats. |

**Connection pool:** 50-100 connections

### Tier 3: 100,000 workflows/s (Large)

**Workload profile:** ~100 steps per workflow, ~10M events/s total.

This tier **requires sharding**. A single PostgreSQL instance cannot sustain 10M events/s write throughput on commodity hardware. The benchmark data shows ~5,353 events/s per connection (batch write).

| Resource | Recommendation | Rationale |
|----------|---------------|-----------|
| Shards | 4-8 PostgreSQL instances | Each shard handles 1.25-2.5M events/s, within single-node capability. |
| Per shard vCPU | 16-32 cores | Maximize parallel WAL writes and compaction throughput. |
| Per shard RAM | 64-128 GB | Large shared_buffers critical for compaction performance. |
| Per shard Storage | 2-4 TB NVMe SSD | Even with sharding, storage requirements are significant. Consider tiered storage (NVMe hot tier, S3 cold tier). |
| Per shard IOPS | 20,000-40,000 | Provisioned IOPS class storage recommended (AWS io2, GCP pd-extreme). |

**Connection pool:** 100-200 per shard

---

## Connection Pool Sizing

Pool size depends on the bottleneck:

| Bottleneck | Formula | Example |
|------------|---------|---------|
| Claim query | `pool = concurrent_workers * 1.2` | 100 workers -> 120 pool connections |
| Event insert | `pool = events_per_second / (events_per_batch / batch_latency_s)` | 100K events/s / (100 events/batch / 0.018s) = 18 connections (at batch=100) |
| Heartbeat | `pool = wf_count / heartbeat_interval_s / throughput_per_conn` | 10K workflows / 10s / 978 updates/s = negligible |

**Rule of thumb:** Start with `pool = 2 * max_workers` and monitor `wait_event=sync` and active connection count.

**PgBouncer configuration:**

```ini
[databases]
cleat = host=localhost port=5432 dbname=cleat pool_size=100

[pgbouncer]
pool_mode = transaction
default_pool_size = 50
max_client_conn = 200
```

---

## IOPS Requirements

Based on benchmark measurements:

| Operation | IOPS per event | Notes |
|-----------|---------------|-------|
| Event INSERT (batch=1) | ~1 write | Single row write + WAL |
| Event INSERT (batch=10) | ~0.3 write | Amortized WAL overhead |
| Event INSERT (batch=100) | ~0.1 write | Max batching efficiency |
| Claim query | ~1 read + 1 write | SELECT + UPDATE SKIP LOCKED |
| Heartbeat | ~1 write | Small row update |
| Compaction scan | ~10 reads/KB | Sequential scan of event_history |

**WAL write rate:** Each event INSERT generates ~200 bytes of WAL. At 100K events/s: ~20 MB/s WAL rate. This must be sustained by the disk subsystem.

---

## PostgreSQL Configuration Tuning

### Base configuration (8 GB RAM, 4 vCPU)

```ini
# Memory
shared_buffers = '2GB'           # 25% of RAM
effective_cache_size = '6GB'     # 75% of RAM
work_mem = '16MB'                # Per-operation sort memory
maintenance_work_mem = '512MB'   # For VACUUM, CREATE INDEX
wal_buffers = '16MB'             # WAL write buffer

# Connections
max_connections = '100'
superuser_reserved_connections = '5'

# Checkpoint tuning (for write-heavy workloads)
checkpoint_timeout = '15min'
checkpoint_completion_target = '0.9'
max_wal_size = '4GB'
min_wal_size = '1GB'

# Planner
random_page_cost = '1.1'         # NVMe SSD: lower than HDD default of 4
effective_io_concurrency = '200' # NVMe SSD: high concurrent I/O

# Autovacuum (tuned for event_history table)
autovacuum_max_workers = '4'
autovacuum_vacuum_scale_factor = '0.01'  # More frequent vacuums
autovacuum_analyze_scale_factor = '0.05'
autovacuum_vacuum_threshold = '1000'
autovacuum_naptime = '30s'

# Parallel query (for compaction scans)
max_parallel_workers_per_gather = '2'
max_parallel_workers = '4'
parallel_tuple_cost = '0.01'
parallel_setup_cost = '100'
```

### Medium configuration (32 GB RAM, 8 vCPU)

```ini
shared_buffers = '8GB'
effective_cache_size = '24GB'
work_mem = '32MB'
maintenance_work_mem = '1GB'
wal_buffers = '64MB'
max_connections = '200'
max_wal_size = '16GB'
min_wal_size = '4GB'
random_page_cost = '1.1'
effective_io_concurrency = '200'
autovacuum_max_workers = '6'
autovacuum_vacuum_scale_factor = '0.005'  # More aggressive vacuum
autovacuum_analyze_scale_factor = '0.02'
max_parallel_workers_per_gather = '4'
max_parallel_workers = '8'
```

### Large configuration (64 GB RAM, 16 vCPU per shard)

```ini
shared_buffers = '16GB'
effective_cache_size = '48GB'
work_mem = '64MB'
maintenance_work_mem = '2GB'
wal_buffers = '128MB'
max_connections = '300'
max_wal_size = '32GB'
min_wal_size = '8GB'
random_page_cost = '1.1'
effective_io_concurrency = '200'
autovacuum_max_workers = '8'
autovacuum_vacuum_scale_factor = '0.001'  # Aggressive vacuum
autovacuum_analyze_scale_factor = '0.01'
max_parallel_workers_per_gather = '4'
max_parallel_workers = '16'
```

---

## Index Recommendations

### Required indexes (created by default migrations)

```sql
-- Primary key: workflow_id + step for event_history
-- Already covered by PRIMARY KEY (workflow_id, step)

-- Claim query index (workflow_instances)
CREATE INDEX CONCURRENTLY idx_instances_claim
    ON workflow_instances (status, next_wake_at)
    WHERE status IN ('available', 'running')
    INCLUDE (id, def_name, def_version, input, generation);

-- Sticky claim index (workflow_instances)
CREATE INDEX CONCURRENTLY idx_instances_sticky
    ON workflow_instances (assigned_to, status, next_wake_at)
    WHERE assigned_to != '' AND status = 'running';

-- Tenant filter index (workflow_instances)
CREATE INDEX CONCURRENTLY idx_instances_tenant
    ON workflow_instances (tenant_id, status, created_at);

-- Expired events cleanup (event_history)
CREATE INDEX CONCURRENTLY idx_event_history_cleanup
    ON workflow_instances (status, completed_at)
    WHERE status IN ('done', 'failed') AND completed_at IS NOT NULL;

-- Heartbeat lookup (workflow_instances)
CREATE INDEX CONCURRENTLY idx_instances_heartbeat
    ON workflow_instances (assigned_to, status)
    WHERE status = 'running';
```

### Performance considerations

- The `idx_instances_claim` index is critical for claim throughput. Without it, claim queries perform sequential scans.
- The `idx_event_history_cleanup` index significantly speeds up the 24-hour retention cleanup cycle. Without it, the subquery in `DeleteExpiredEvents` scans the full `workflow_instances` table.
- Monitor index bloat with `pgstattuple` extension. Event history is write-only (no UPDATEs), so it should not bloat. Workflow_instances sees UPDATEs on status changes and may bloat over time.

---

## Autovacuum Tuning for event_history

The `event_history` table is append-only with DELETEs from retention cleanup. This is a challenging workload for autovacuum.

### Per-table autovacuum configuration

```sql
ALTER TABLE event_history SET (
    autovacuum_vacuum_scale_factor = 0.001,
    autovacuum_vacuum_threshold = 10000,
    autovacuum_analyze_scale_factor = 0.01,
    autovacuum_analyze_threshold = 10000,
    autovacuum_vacuum_cost_limit = 1000,   -- Higher cost limit = faster vacuum
    autovacuum_vacuum_cost_delay = 0       -- No delay = maximum throughput
);

ALTER TABLE workflow_instances SET (
    autovacuum_vacuum_scale_factor = 0.01,
    autovacuum_vacuum_threshold = 1000,
    autovacuum_analyze_scale_factor = 0.05,
    autovacuum_analyze_threshold = 1000
);
```

### Monitoring autovacuum

```sql
-- Check vacuum progress
SELECT relname, n_dead_tup, last_vacuum, last_autovacuum
FROM pg_stat_user_tables
WHERE relname IN ('event_history', 'workflow_instances');

-- Check if vacuum is keeping up
SELECT relname,
       n_dead_tup,
       n_live_tup,
       round(n_dead_tup * 100.0 / GREATEST(n_live_tup + n_dead_tup, 1), 2) AS dead_pct
FROM pg_stat_user_tables
WHERE relname = 'event_history';
```

If `dead_pct` exceeds 20%, increase autovacuum frequency or decrease the retention period.

---

## Migration Considerations

### Adding an index without locking

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_name
    ON table_name (columns);
```

- `CONCURRENTLY` allows reads and writes to continue during index creation.
- Takes 2-5x longer than a blocking CREATE INDEX.
- Monitor with `pg_stat_progress_create_index`.

### Adding a column with a default value

```sql
-- PostgreSQL 11+: Adding a column with a non-volatile DEFAULT is instant
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS new_col TEXT DEFAULT '';
```

- In PostgreSQL 11+, adding a column with a constant DEFAULT rewrites only the metadata, not the table.
- Adding a column without a DEFAULT is also instant.

### Adding a column with a NOT NULL constraint

```sql
-- Step 1: Add column without NOT NULL
ALTER TABLE event_history ADD COLUMN IF NOT EXISTS new_col TEXT;

-- Step 2: Backfill in batches (use low lock timeout for production)
SET lock_timeout = '5s';
UPDATE event_history SET new_col = '' WHERE new_col IS NULL AND workflow_id IN (
    SELECT workflow_id FROM event_history WHERE new_col IS NULL LIMIT 1000
);

-- Step 3: Add NOT NULL constraint
ALTER TABLE event_history ALTER COLUMN new_col SET NOT NULL;
```

### Table partitioning for event_history

For Tier 2 and Tier 3 deployments, consider partitioning `event_history` by time:

```sql
-- Create partitioned table
CREATE TABLE event_history (
    workflow_id TEXT NOT NULL,
    step INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- ... other columns
    PRIMARY KEY (workflow_id, step, created_at)
) PARTITION BY RANGE (created_at);

-- Create monthly partitions
CREATE TABLE event_history_2026_01
    PARTITION OF event_history
    FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE event_history_2026_02
    PARTITION OF event_history
    FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');

-- Retention: drop old partitions instead of DELETE
DROP TABLE IF EXISTS event_history_2025_12;
```

Partitioning benefits:
- Retention cleanup becomes a metadata operation (DROP TABLE) instead of a bulk DELETE
- Query performance improves for time-range scans
- Autovacuum per-partition is cheaper than a single large table

---

## Throughput Ceiling: Single-Node PostgreSQL Saturation

Based on benchmark data, here is the estimated saturation point for a single well-provisioned PostgreSQL node.

### Saturation Estimates

| Metric | Ceiling | Bottleneck | Evidence |
|--------|---------|------------|----------|
| Max event INSERTs/s (batch=1) | ~1,000/s per connection | WAL write rate | 1,092 events/s measured (batch=1) |
| Max event INSERTs/s (batch=100) | ~5,000/s per connection | Transaction commit rate | 5,353 events/s measured (batch=100) |
| Max claim queries/s | ~7,000/s | Row lock contention on claim index | 142us per claim at 1000 concurrent workers = ~7,000 claims/s |
| Max heartbeats/s | ~1,000/s per connection | UPDATE rate on workflow_instances | 977 updates/s measured |
| Max compaction scan (10K events) | ~847 events/s | Sequential scan + row decoding | 847 events/s measured |
| Max compaction scan (100K events) | ~8,255 events/s | Sequential scan (cached) | 8,255 events/s measured |

### Overall Single-Node Saturation

A single PostgreSQL node (8-16 vCPU, 32-64 GB RAM, NVMe SSD) reaches saturation at approximately:

| Workflow type | Max workflows/s | Limiting factor |
|---------------|----------------|-----------------|
| Simple (10 steps/workflow) | ~10,000 wf/s | Event history INSERT, ~100K events/s |
| Saga (100 steps/workflow) | ~500 wf/s | Event history INSERT, ~50K events/s |
| Saga (1000 steps/workflow) | ~50 wf/s | Event history INSERT, ~50K events/s |

### Saturation Signs

Monitor for these symptoms:

1. **CPU: 80%+ sustained**: PostgreSQL is CPU-bound, typically from WAL writing or query processing.
2. **WAL rate > 50 MB/s**: Disk write bandwidth becoming the bottleneck.
3. **Checkpoint frequency increasing**: `pg_stat_bgwriter.checkpoints_timed` dropping below `checkpoint_timeout` indicates checkpoint pressure.
4. **Replication lag growing**: Standby replicas cannot keep up with WAL rate.
5. **Claim query latency > 1ms**: Row lock contention increasing due to high concurrent claim attempts.

### Scaling Strategy

**Before sharding (optimize single node):**

1. Increase batch size for event INSERTs (batch=100 gives 5x throughput vs batch=1)
2. Tune `max_wal_size` to reduce checkpoint frequency
3. Increase `shared_buffers` to cache hot event history pages
4. Use NVMe SSD with provisioned IOPS
5. Partition event_history by time for faster retention cleanup

**When to shard:**

| Indicator | Threshold | Action |
|-----------|-----------|--------|
| Events/s | > 500,000 | Add 2nd shard |
| Event history size | > 500 GB | Partition or shard |
| WAL rate | > 50 MB/s sustained | Add shard |
| Retention cleanup takes > 1 hour | | Shard or partition |

**When to add read replicas:**

1. Replay-heavy workloads where LoadEventHistory dominates read throughput
2. Compaction is competing with write workload for I/O
3. Reporting/analytics queries on event_history

### Observed Limits

Measured on the benchmark hardware (AMD Ryzen 5 5500U Docker PostgreSQL):

| Operation | Observed limit |
|-----------|---------------|
| Max event INSERT throughput (single connection) | 5,353 events/s (batch=100) |
| Max claim throughput (1000 concurrent workers) | ~7,000 claims/s |
| Max heartbeat throughput | ~978 updates/s per connection |
| Compaction scan (10K events, paginated) | 847 events/s |
| Compaction scan (100K events, paginated) | 8,255 events/s (cached) |
| Max concurrent workflows (claimed) | 1,000+ (with acceptable latency) |

**Note:** These numbers are from a Docker PostgreSQL with defaults (shared_buffers=128MB, no tuning). Production tuning should improve throughput by 2-5x.

---

## Operational Runbooks

### Immediate bottleneck check

```sql
-- Top wait events
SELECT wait_event_type, wait_event, COUNT(*) as count
FROM pg_stat_activity
WHERE state = 'active' AND wait_event IS NOT NULL
GROUP BY 1, 2 ORDER BY 3 DESC;

-- Slow queries
SELECT query, calls, total_exec_time / calls as avg_ms, rows
FROM pg_stat_statements
ORDER BY total_exec_time DESC LIMIT 10;

-- Table bloat
SELECT schemaname, tablename, n_dead_tup, n_live_tup
FROM pg_stat_user_tables
ORDER BY n_dead_tup DESC;
```

### Emergency retention cleanup

If retention cleanup starts falling behind:

```sql
-- Manual one-time cleanup with smaller batches
DO $$
DECLARE
    deleted BIGINT;
BEGIN
    LOOP
        DELETE FROM event_history
        WHERE workflow_id IN (
            SELECT id FROM workflow_instances
            WHERE status IN ('done', 'failed')
              AND completed_at < NOW() - INTERVAL '30 days'
            LIMIT 1000
        );
        GET DIAGNOSTICS deleted = ROW_COUNT;
        IF deleted = 0 THEN EXIT; END IF;
        COMMIT;
        PERFORM pg_sleep(0.1);  -- Throttle
    END LOOP;
END $$;
```

### Connection storm recovery

```sql
-- Kill idle connections from worker
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE state = 'idle'
  AND usename = 'cleat'
  AND pid != pg_backend_pid();
```
