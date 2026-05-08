# Cleat Worker Architecture

## 1. How Worker Processes Run

The worker is a single Go binary (`cmd/cleat-worker/main.go`) that runs as a
long-lived daemon. Startup flow:

1. Parse flags: `--db` (PostgreSQL URL), `--concurrency`, `--task-queue`,
   `--namespace`, `--api-addr`, `--shards-file`, etc.
2. Generate a random 8-hex-char worker ID.
3. Open a database connection (single DB or sharded).
4. Load plugins, run migrations, register host functions.
5. Start an optional HTTP API server (`/healthz`, `/metrics`,
   `/api/workflows/...`, `/api/schedules/...`).
6. Call `w.Run()` which starts six background goroutines.

**WASM runtime**: [wazero](https://github.com/tetratelabs/wazero) — a
zero-dependency WebAssembly runtime. Workflow WASM modules are stored in the
`workflow_defs` table and loaded on demand with an in-memory cache
(`main.go:971-991`).

### Background Goroutines

| Goroutine              | Interval   | Purpose |
|------------------------|------------|---------|
| `heartbeatLoop`        | 5s         | Heartbeat every in-flight workflow to prove liveness |
| `reaperLoop`           | 30s        | Reset stale workflows (crashed worker) back to `ready` |
| `concurrencyKeyReaperLoop` | 60s   | Delete expired concurrency keys |
| `dispatchLoop`         | continuous | Poll PostgreSQL for runnable workflows (main work loop) |
| `scheduleLoop`         | 15s        | Fire due cron schedules |
| `compactionLoop`       | 5m         | Compact long event histories |

### Workflow Execution Lifecycle (`executeWorkflow`, main.go:569)

1. Load WASM bytes for `def_name` + `def_version`.
2. Load event history from `event_history` table.
3. Load compaction state if present.
4. Create a wazero runtime, instantiate the WASM module.
5. Call `engine.Replay()` — replays existing history through the WASM code:
   - For steps already in history: return cached results (deterministic replay).
   - For new steps: make actual calls, record new events.
6. On **completion**: persist new events, mark `done`/`failed`.
7. On **suspension** (sleep/await signals): release to `ready` with `next_wake_at`.
8. On **`continue_as_new`**: start a new run, complete the current one.

---

## 2. How Workflows Are Scheduled

### A. Pull-based Queue Polling (primary)

The dispatch loop uses `SELECT ... FOR UPDATE SKIP LOCKED` on
`workflow_instances` to atomically claim work.

```sql
UPDATE workflow_instances
SET status = 'running', assigned_to = $1, heartbeat_at = now()
WHERE id IN (
    SELECT id FROM workflow_instances
    WHERE status = 'ready'
      AND next_wake_at <= now()
      AND namespace = $2
      AND task_queue = ANY($3)
    ORDER BY CASE WHEN sticky_worker_id = $1 THEN 0 ELSE 1 END, created_at
    LIMIT $4
    FOR UPDATE SKIP LOCKED
)
RETURNING id, def_name, def_version, status, input, assigned_to, next_wake_at, tenant_id
```

Key properties:
- **`SKIP LOCKED`** — multiple workers poll the same queue without contention.
- **`sticky_worker_id` ordering** — priority to previously-running worker (keeps
  WASM cache warm, Feature 10).
- **`task_queue` filtering** — partition work across worker pools
  (`default`, `gpu`, `high-memory`).
- **`namespace` scoping** — tenant isolation.
- **`next_wake_at <= now()`** — suspended workflows only wake when timer expires.
- **Progressive backoff** — idle poll interval grows from 500ms to 3s; resets
  when work is found.
- **Two-phase claim**: `ClaimStickyWorkflows` first (fast path on
  `idx_instances_sticky`), then `ClaimWorkflows` to fill remaining capacity.

### B. Cron-based Scheduled Workflows

The `scheduleLoop` (every 15s) queries `workflow_schedules` for entries where
`next_run_at <= now()` (also using `SKIP LOCKED`). It creates a new workflow
instance and computes the next cron fire time.

### C. External Triggers

- **HTTP API** — `POST /api/workflows/:name/start`
- **Child workflows** — spawned by running workflows
- **`ContinueAsNew`** — a workflow restarts itself with new input

---

## 3. How Worker Nodes Are Added and Removed

Cleat uses a **shared-nothing, database-mediated** design. There is no leader
election, no membership protocol, no direct coordination between workers.

### Adding a Node

Start a new `cleat-worker` process pointing at the same PostgreSQL database.
It immediately begins polling for work. No registration needed.

### Graceful Removal

On SIGINT/SIGTERM:
1. `cancel()` stops the dispatch loop.
2. `wg.Wait()` drains in-flight workflows.
3. Suspended workflows are released back to `ready` with their `next_wake_at` preserved.

### Crash Recovery (Zombie Reaper)

If a worker crashes, the **reaper loop** runs every 30s on every surviving
worker:

```sql
UPDATE workflow_instances
SET status = 'ready', assigned_to = NULL, heartbeat_at = NULL
WHERE status = 'running'
  AND heartbeat_at < now() - '30 seconds'::interval
```

This resets orphaned workflows to `ready`. Another worker claims and replays
them from the last persisted event history — deterministic replay
reconstructs the exact same state.

### Heartbeat-based Ownership

Each worker heartbeats every 5s per in-flight workflow. `Heartbeat()` only
succeeds if `assigned_to` still matches — if another worker stole the
workflow (via reaper), the heartbeat returns `false` and the original worker
drops it locally.

### Sharding

`ShardedStore` distributes data across multiple PostgreSQL instances using
SHA-256 consistent hashing on the workflow ID. All shards are polled during
dispatch.

### Tenancy

PostgreSQL Row-Level Security via `set_config('cleat.tenant_id', ...)` scopes
queries per tenant. Optional per-tenant connection pools.

---

## 4. Concurrency Model

### How many workflows run concurrently?

Each worker runs up to `--concurrency` (default: 10) workflows concurrently.
The dispatch loop (`main.go:466-477`) counts in-flight workflows from
`w.inflight` (a `sync.Map`), computes `free = concurrency - count`, and
claims at most `min(free, 20)` workflows per poll cycle.

Each claimed workflow runs in its own goroutine via `go w.executeWorkflow(wf)`
(`main.go:564`).

### Is this matched to available memory?

**No, there is no dynamic memory awareness.** The `--concurrency` flag is a
static integer. Each concurrent workflow requires:
- One wazero runtime instance (~few MB).
- One WASM module instantiation (size of the compiled `.wasm` file).
- Event history in memory (grows with workflow length until compaction).
- Goroutine stack (~2KB initially, grows as needed).

A workflow doing heavy in-memory work (large JSON processing, many child
workflows, long histories) can consume significant RAM. If concurrency is set
too high for the node's available memory, the worker will OOM.

### Recommended sizing approach

1. **Measure** the peak RSS of a single workflow instance for your heaviest
   workflow type (run it in isolation, watch `cleat_workflows_active` and
   process RSS).
2. **Set `--concurrency`** to `floor(available_memory / peak_per_workflow_rss)`
   with a safety margin (e.g., 70%).
3. **Use `--task-queue`** to segregate memory-heavy workflows onto dedicated,
   lower-concurrency worker pools (e.g., a `high-memory` queue with
   `--concurrency=2` on larger instances).

There is currently no built-in mechanism for the worker to measure its own
memory pressure and shed load or reject claims. This would be a natural future
improvement (e.g., a soft memory limit with backpressure).
