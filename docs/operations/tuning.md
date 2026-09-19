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

## Heartbeat (`--heartbeat`) and reclaim (`--reclaim-timeout`)

`--heartbeat` controls how often a worker proves it is alive by updating its
liveness row. `--reclaim-timeout` controls how long a run may go without one of
those before another worker may claim it.

**They used to be one knob and are now two.** The reclaim window was derived as
`max(2 x --heartbeat, 10s)`, so the only way to a longer window was a sparser
heartbeat. `--reclaim-timeout` defaults to `0`, which keeps exactly that
derivation — setting nothing changes nothing.

### Why you might want them apart

A database failover stops the heartbeat **without the worker being dead**. The
heartbeat is written to the same database the workflow's events are, so an
outage silences every worker at once, and at the default every run in the fleet
is reclaimable ten seconds in. Buying tolerance for that by raising
`--heartbeat` also makes heartbeats sparse, so a genuinely crashed worker's runs
stay stranded just as long — paying for an occasional outage with every crash.

```bash
# a 5-minute reclaim window while still checking in every 5 seconds
cleat-worker --heartbeat 5s --reclaim-timeout 5m
```

A value below `2 x --heartbeat` is **refused**, not clamped: it would reclaim
runs from workers that are alive and checking in normally.

### The derived window, if you leave `--reclaim-timeout` at 0

| `--heartbeat` | Reclaim window | Reaper poll | DB write rate |
|---|---|---|---|
| 2 s | **10 s** | 10 s | High |
| 5 s (default) | 10 s | 10 s | Moderate |
| 15 s | 30 s | 15 s | Low |
| 30 s | 60 s | 30 s | Very low |

**The 10-second floor is why the first two rows are the same.** Every heartbeat
of 5 s or below produces an identical 10 s reclaim window — `max(2 x hb, 10s)`.
Going below 5 s buys no reclaim speed at all; it only adds write load. This
table previously said a 2 s heartbeat gave ~4 s recovery, which the floor makes
impossible.

Re-derive rather than trusting the table:

```
max(2 x --heartbeat, 10s)     # cmd/cleat-worker/setup.go, Worker.reclaimAfter
```

### Recommendations

| Deployment | Heartbeat | Reclaim | Reason |
|---|---|---|---|
| Single worker | 15–30 s | leave at 0 | No other worker can reclaim, so the window is irrelevant |
| Multi-worker (stable) | 5–10 s | leave at 0 | Balanced |
| Kubernetes (preemptible) | 5 s | leave at 0 | Nodes vanish suddenly; **do not go below 5 s** — the floor means it changes nothing |
| Managed DB with failover | 5 s | `2–5 m` | Survive the failover without making crash recovery slower |
| Development | 30 s+ | leave at 0 | Minimizes DB writes while debugging |

## Event flush retry (`--flush-retry-window`)

An event flush that fails does not give up immediately — it retries with
exponential backoff until this window elapses, and only then is the step
reported as unpersisted.

`--flush-retry-window` defaults to `0`, meaning **750 ms**. That is not a new
number: the batch flush path has always retried five times at 50 ms doubling,
which is 50+100+200+400 = 750 ms of sleeping. Setting nothing leaves the batch
path exactly as it was.

### What changed at the default

The **direct** flush path — the one every step of a low-rate workflow takes —
made one attempt and no retry. The same event at a higher step rate went through
the batch path and got five. That asymmetry was not a decision; it was where the
retry happened to be written. Both paths now share the window.

### Sizing it for a failover

```bash
# ride out a managed-database failover, and keep the runs while doing so
cleat-worker --flush-retry-window 5m --reclaim-timeout 5m
```

**Raise `--reclaim-timeout` with it.** An outage long enough to need a long
retry also stops this worker's heartbeat, which is written to the same database
— so the moment the database returns, every run in the fleet is past its stale
window, the reaper takes them, and the retry that finally succeeds loses its
fence. A `--flush-retry-window` above the reclaim window buys nothing on its
own, and the worker says so at startup:

```
WARN --flush-retry-window 5m0s outlasts the reclaim window (10s): ...
```

The exception is a deployment where nothing else reaps — a single worker — in
which case the advice is safe to ignore. It is advice rather than a refusal for
exactly that case.

### What it costs

| | |
|---|---|
| A step whose flush fails permanently | stalls for the whole window |
| Errors the engine does not recognise | **are retried** — `errIsRetryable` defaults to true, so a permanent unknown error costs the full window |
| Shutdown | an in-flight retry can delay it by up to the window |
| A lost fence | **not** retried — it is another worker holding the claim, which no later attempt changes |
| A cancelled context | not retried |

Those exemptions are why the default is 750 ms rather than something failover-sized:
the retry sits on the engine's hottest write path, and only an operator who knows
their failover budget should be paying for one.

### Recommendations

| Deployment | `--flush-retry-window` | `--reclaim-timeout` |
|---|---|---|
| Local / development | leave at 0 | leave at 0 |
| Multi-worker, self-managed DB | leave at 0 | leave at 0 |
| Streaming replication failover | `30–60 s` | match it |
| Managed DB with Multi-AZ failover | `2–5 m` | match it |
| Single worker, long outages expected | `2–5 m` | leave at 0 (nothing else reaps) |

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
  --db "$CLEAT_DATABASE_URL" \
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
  --db "$CLEAT_DATABASE_URL" \
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
  --db "$CLEAT_DATABASE_URL" \
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

### Previewing a retention sweep

Retention deletes event history and, when the opt-in arms are enabled, the
workflow records themselves. Neither is recoverable. Before running a sweep —
especially one with an `older_than` override, which can scope far wider than the
configured window — ask what it would remove:

```
POST /api/admin/retention/sweep
{"older_than": "720h", "dry_run": true}
```

The response has the same shape as a real sweep, with `"dry_run": true` added, so
a preview and the sweep that follows it can be diffed directly. Disabled arms
appear in `skipped` exactly as they do in a real sweep, so a zero is never
ambiguous between "that arm is off" and "nothing matched".

**The counts are best effort, not a guarantee.** They are read at one instant
from a live database: by the time you run the sweep, workflows will have
completed and rows will have aged past the cutoff. Treat the numbers as the right
order of magnitude — enough to catch a mistyped window, which is what the preview
is for — and not as a list of rows that will be deleted. Expect a preview and the
sweep that follows it to differ by whatever the workload did in between.

A preview takes no locks and writes nothing, so it is safe to run against a busy
worker. It also does not advance the retention last-run metric, so checking a
preview does not look like a retention pass to your dashboards.

## Database connection pool

**A worker opens several independent pools, not one.** `concurrency + 5` is the
core pool alone, and sizing from it under-provisions a default worker by a
factor of five.

Per pool, with the gate each sits behind (cleat#1470):

| pool | size | default | opened when |
|---|---|---|---|
| core | `--concurrency + 5` | **15** | always |
| plugin | `--max-plugin-connections` | **10** | that flag `> 0` |
| adaptive flusher | `--batch-flush-max-connections` | **50** | unless `--batch-flush-disabled` *or* `--no-per-step-flush` |
| shard | 15 **per shard** | — | only when sharding is configured |
| migrate | 2 | — | only with `--migrate-db`, and only at boot |
| tenant | `--tenant-pool-max-conns` **per tenant** | 25 × *T*<sub>active</sub> | **always** on SQL Server and MySQL; on PostgreSQL only with `--tenant-isolation=role` |

*T*<sub>active</sub>, not *T*: every per-tenant pool is built with a five-minute
`ConnMaxLifetime`, so a tenant that is not executing anything holds no
connections. This term is a ceiling for tenants working at once, which
`--concurrency` already bounds — not a cost per tenant the worker has ever seen.

`only with --tenant-isolation=role` was wrong here for the same reason the
worker's own connection census was: that mode is PostgreSQL-only, while
`MSSQLStoreFactory` and `MySQLStoreFactory` pool per tenant by construction —
SQL Server because its RLS reads a per-connection `SESSION_CONTEXT`, MySQL
because each tenant has its own database.

```
default single-node worker, no sharding, no --migrate-db:
    15 (core) + 10 (plugin) + 50 (flusher) = 75
```

**The adaptive flusher's 50 is default-on and is two thirds of that.** Both of
its gates — `--batch-flush-disabled` and `--no-per-step-flush` — default to
`false`, so it reads like an opt-in feature and is not one. If you size for
`concurrency + 5` you will be short by 60 per worker, and the symptom is
connection exhaustion under load.

**The tenant pool is unbounded in tenant count.** With
`--tenant-isolation=role` a worker opens a pool per tenant it has touched and
does not release them, so there is no fixed total to quote — see cleat#1470.
Budget for the tenants a worker will actually serve.

Nothing in the worker sums these or logs the total at startup, so the
arithmetic above is the only place it exists.

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

> **Migrations must not go through a transaction-mode pooler.** `pool_mode =
> transaction` hands each transaction whichever server backend is free, so
> session state does not persist across statements — and the migration path
> depends on exactly that:
>
> | session-scoped thing | where |
> |---|---|
> | `pg_advisory_lock`, serialising migrations across workers | `migration/runner.go:209`, `plugin/migration.go:177` |
> | `SET search_path = public`, held across the plugin run | `plugin/migration.go:181` |
>
> The lock is taken on one backend and the unlock may land on another, so the
> serialisation is silently absent — at **every worker boot**, on the path that
> applies schema changes, which is the one place two workers must not proceed
> at once. Nothing errors; the lock simply does not lock.
>
> **Use `--migrate-db`, which already exists for this shape of problem.** Point
> `--db` at PgBouncer and `--migrate-db` at a direct connection:
>
> ```bash
> cleat-worker \
>     --db "postgres://cleat@pgbouncer:6432/cleat" \
>     --migrate-db "postgres://cleat@postgres:5432/cleat"
> ```
>
> Steady-state traffic keeps its pooling; the migration connection gets the
> session it requires. (`--migrate-db` was added for privilege separation — an
> unprivileged `--db` role with a DDL-capable migration role — and serves both
> purposes.)
>
> `session` pooling does not have this problem, and `statement` pooling is worse.
> Verify with `git grep -n pg_advisory -- migration/ plugin/` before assuming
> this note is still current; cleat#1310.

Then connect workers to PgBouncer:

```bash
cleat-worker --db "postgres://user:pass@pgbouncer:6432/cleat"
```
