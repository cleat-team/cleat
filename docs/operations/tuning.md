# Tuning cleat-worker

This guide provides specific guidance on tuning critical worker parameters.
For a reference listing of all flags, see `docs/reference/worker-config.md`.

## Concurrency (`--concurrency`)

Controls how many workflow instances a single worker executes simultaneously.

### Formula

```
concurrency = (target_throughput_rps × avg_workflow_duration_s)
```

### Workload profiles

| Profile | Concurrency | Rationale |
|---------|-------------|-----------|
| IO-bound (external API calls) | 20–50 | Most time spent waiting on external services; high concurrency hides latency |
| CPU-bound (WASM computation) | GOMAXPROCS × 2 | WASM execution is CPU-bound; oversubscribing beyond 2× cores adds scheduling overhead |
| Bursty (batch processing) | 10–20 | Higher values cause thundering-herd on database; moderate concurrency with poll backoff |
| Low-latency (sub-second workflows) | 5–10 | Fast workflows don't need high concurrency; more workers is better than more concurrency per worker |

### Warning signs

- **DB connection pool exhaustion**: if you see `too many clients` errors, reduce `--concurrency` or add PgBouncer. Each concurrent workflow uses one DB connection.
- **High CPU with idle workflows**: CPU is spent on WASM rather than waiting — reduce concurrency.
- **Workflows stuck in `running` state**: workers can't keep up — increase concurrency or add more workers.

### Memory interaction

`--concurrency` interacts with `--memory-soft-limit`. When system memory exceeds the
soft limit, the worker stops claiming new work even if concurrency slots are available.

## Heartbeat (`--heartbeat`)

Controls how often the worker updates its liveness in the database. If a worker
crashes, its claimed workflows are reclaimed after two missed heartbeats.

### Tradeoff

| Heartbeat | Recovery time | DB write rate |
|-----------|---------------|---------------|
| 2 s | ~4 s | High |
| 5 s (default) | ~10 s | Moderate |
| 15 s | ~30 s | Low |
| 30 s | ~60 s | Very low |

### Recommendations

| Deployment | Heartbeat | Reason |
|------------|-----------|--------|
| Single worker | 15–30 s | No other workers to reclaim; fast recovery is irrelevant |
| Multi-worker (stable) | 5–10 s | Balanced — fast enough for HA, low enough DB load |
| Kubernetes (preemptible) | 2–5 s | Nodes can disappear suddenly; fast reclaim prevents long stalls |
| Development | 30 s+ | Minimizes DB writes during debugging |

## Poll interval (`--poll`)

Controls how long the worker waits between dispatch-loop iterations when no
runnable workflows are found. Uses progressive backoff (up to 6× the configured
value).

### Tradeoff

| Poll interval | New-work latency | DB load when idle |
|---------------|-----------------|-------------------|
| 100 ms | ~100 ms | Moderate |
| 500 ms (default) | ~500 ms | Low |
| 2 s | ~2 s | Very low |
| 5 s | ~5 s | Minimal |

### Recommendations

| Workload pattern | Poll interval | Reason |
|-----------------|---------------|--------|
| Steady stream | 500 ms–1 s | Work is always available; poll is rarely exercised |
| Bursty (batch jobs) | 100–250 ms | Want low latency when a batch arrives |
| Low-volume (few workflows/hour) | 2–5 s | DB load matters more than latency |
| Event-driven (API starts) | 1–5 s | Most workflows are started via API, not polled |

## Memory limits

Three memory controls work together:

| Flag | What it does |
|------|-------------|
| `--wasm-memory-max-mb` | Per-WASM-module linear memory cap. A workflow exceeding this is killed. |
| `--memory-soft-limit` | When system memory exceeds this fraction (0.0–1.0), stop claiming new work. |
| `--memory-hard-limit` | When system memory exceeds this fraction, reject API workflow starts (HTTP 503). |

### Sizing

```
total_wasm_memory ≈ concurrent_workflows × wasm_memory_max_mb × 1.2 (overhead factor)
```

Example: 10 concurrent workflows × 32 MB per module × 1.2 = ~384 MB WASM memory.
Add ~256 MB for the Go runtime and caches → ~640 MB total recommended.

### Recommendations

| Machine size | Concurrency | wasm-memory-max-mb | memory-soft-limit |
|-------------|-------------|-------------------|-------------------|
| 1 GB | 5 | 32 | 0.70 |
| 2 GB | 10 | 32 | 0.75 |
| 4 GB | 20 | 64 | 0.75 |
| 8 GB | 40 | 64 | 0.80 |

## WASM cache sizing

Compiled WASM modules are cached in memory (LRU eviction). Disk cache is optional
(`--wasm-cache-dir`).

### Estimating cache size

```
cache_entries ≈ number_of_workflow_definitions × 3 (versions)
cache_memory_mb ≈ cache_entries × 2 MB (avg WASM module size)
```

Example: 5 workflow definitions × 3 versions × 2 MB = ~30 MB cache. Set
`--wasm-cache-max-entries` to 15 and `--wasm-cache-max-mb` to 50 to leave headroom.

### Disk cache

When `--wasm-cache-dir` is set, compiled modules persist across worker restarts.
This eliminates cold-start compilation latency (typically 100–500 ms per module).
Recommended for production.

Set `--wasm-disk-cache-max-files` to 2× `--wasm-cache-max-entries` to allow for
version churn.

## Rate limiting (`--rate-limit`, `--rate-limit-burst`)

IP-based token-bucket rate limiter on the HTTP API.

### Estimating limits

```
requests_per_second ≈ expected_active_users × 2 (workflow starts + queries per user per second)
burst = rps × 2 (handle brief spikes)
```

### Recommendations

| Environment | Rate limit | Burst | Reason |
|-------------|-----------|-------|--------|
| Development | 1000 | 2000 | No meaningful limit |
| Staging | 100 | 200 | Simulate production constraints |
| Production (internal) | 500 | 1000 | Trusted clients behind VPN |
| Production (public) | 50 | 100 | Untrusted clients; add API-key-based limiting |

## Quick-start profiles

### Development (low resource)

```bash
cleat-worker \
  --db "$DATABASE_URL" \
  --concurrency 2 \
  --heartbeat 30s \
  --poll 2s \
  --wasm-memory-max-mb 32 \
  --memory-soft-limit 0.90 \
  --rate-limit 1000
```

### Production (reliable)

```bash
cleat-worker \
  --db "$DATABASE_URL" \
  --concurrency 10 \
  --heartbeat 5s \
  --poll 500ms \
  --wasm-memory-max-mb 32 \
  --memory-soft-limit 0.80 \
  --memory-hard-limit 0.95 \
  --wasm-cache-dir /var/cache/cleat/wasm \
  --rate-limit 100
```

### High-throughput (optimized for speed)

```bash
cleat-worker \
  --db "$DATABASE_URL" \
  --concurrency 40 \
  --heartbeat 3s \
  --poll 100ms \
  --wasm-memory-max-mb 64 \
  --memory-soft-limit 0.75 \
  --memory-hard-limit 0.90 \
  --wasm-cache-dir /var/cache/cleat/wasm \
  --wasm-cache-max-entries 200 \
  --wasm-cache-max-mb 1000 \
  --retention-days 7
```

## Database connection pool

Each concurrent workflow holds one database connection. The worker also uses a
few connections for housekeeping (reaper, compactor, health checks).

```
total_db_connections ≈ concurrency + 5 (housekeeping)
```

If you run multiple workers, multiply by the worker count. Use PgBouncer in
transaction mode between workers and PostgreSQL to reduce the total connection
count.

### PgBouncer configuration

```ini
[databases]
cleat = host=db-host dbname=cleat

[pgbouncer]
pool_mode = transaction
max_client_conn = 200
default_pool_size = 25
```

Then connect workers to PgBouncer:

```bash
cleat-worker --db "postgres://user:pass@pgbouncer:6432/cleat"
```
