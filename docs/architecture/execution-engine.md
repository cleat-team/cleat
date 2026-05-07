# Execution Engine

The execution engine is the heart of the cleat worker daemon. It manages
workflow instance claiming, WASM execution, event replay, checkpointing,
and lifecycle management.

## Claim Loop

The worker dispatch loop runs continuously, polling PostgreSQL for runnable
workflow instances:

```
dispatchLoop:
  for {
    1. Count in-flight workflows (sync.Map)
    2. Calculate free slots: concurrency - inflight
    3. If no free slots: sleep(pollInterval), continue
    4. Claim sticky workflows (low-contention fast path)
    5. Fill remaining capacity with general batch claim
    6. For each claimed instance: go executeWorkflow(wf)
    7. If no work found: progressive backoff (up to 6x pollInterval)
  }
```

### Claim Query

The core claim pattern uses PostgreSQL's `SELECT ... FOR UPDATE SKIP LOCKED`:

```sql
SELECT * FROM workflow_instances
WHERE namespace = $1
  AND status = 'ready'
  AND next_wake_at <= now()
ORDER BY created_at ASC
LIMIT $2
FOR UPDATE SKIP LOCKED;
```

The `ORDER BY created_at ASC` ensures FIFO ordering. `SKIP LOCKED` allows
multiple workers to claim different instances concurrently without blocking.

### Sticky Workflow Fast Path

After claiming an instance, the worker records itself as the sticky worker for
that instance. Subsequent polls first try the sticky fast path -- reclaiming
instances that were previously assigned to this worker. This improves cache
locality (WASM module already loaded, database connections warm).

```sql
SELECT * FROM workflow_instances
WHERE sticky_worker_id = $1
  AND status = 'ready'
  AND next_wake_at <= now()
LIMIT $2
FOR UPDATE SKIP LOCKED;
```

### Batch Size

Claims are batched with a maximum of 20 instances per query. If fewer instances
are available, the dispatcher fills remaining capacity from the general pool.

### Backpressure

The worker tracks consecutive database errors. If errors exceed a threshold, a
circuit breaker opens and the worker backs off with exponential backoff (up to
30s). When the database recovers, the circuit resets and normal polling resumes.

## Replay Mechanism

Cleat uses a deterministic replay model inspired by Temporal. When a workflow
instance is claimed:

1. **Load history**: All events for the instance are loaded from
   `event_history`, ordered by step number.
2. **Compile WASM**: The module is compiled and instantiated in wazero.
3. **Replay**: The entry point export is called. For each step:
   - If the event history has an event at this step, the cached response is
     returned to the WASM module -- the call is NOT re-executed.
   - If the event history does NOT have an event at this step (this is the
     first time execution reaches here), the call is executed for real, and the
     response is recorded as a new event.
4. **Persist**: After the workflow completes (or suspends), new events are
   batched and written to `event_history`.

### Determinism Requirements

For correct replay, workflows must be deterministic:

- `time.Now()` is replaced by `h.Now()` (returns recorded time).
- `rand` is replaced by `h.Random()` (returns recorded value).
- All external communication goes through `h.DurableCall()` (recorded).
- Concurrency primitives (goroutines, channels) within a single WASM module are
  safe but external goroutines are not.

### Replay Cache

The engine maintains an in-memory cache of event responses indexed by step
number. During replay, when the WASM module calls `cleat_call` at step N, the
engine:

1. Checks if event N exists in history.
2. If yes: return the cached response from event N (skip execution).
3. If no: execute the call, record the response, append event N.

This is the same pattern used by Temporal's workflow replay.

## Checkpointing

Checkpointing is event-driven rather than time-driven. After each
`DurableCall`, the event is persisted to PostgreSQL before the next step
begins. This means:

- The checkpoint granularity is one durable call.
- Computation between durable calls is NOT checkpointed -- it is re-done
  during replay.
- For I/O-bound workflows (the common case), redoing computation between
  calls is negligible.
- No serialization of local variables is needed.

### Event Types

The engine supports 30+ event types (see `internal/host/engine.go`). The core
types used by the replay loop:

| Event Type | Description |
|------------|-------------|
| `call` | A `DurableCall` with request/response |
| `sleep` | A `DurableSleep` with duration |
| `await_signals` | Waiting for one or more signals |
| `signal_received` | A signal was delivered |
| `defer` | A `DurableDefer` registration |
| `child_workflow` | A child workflow was started |
| `await_child` | Waiting for a child to complete |
| `continue_as_new` | The workflow continued as a new run |
| `plugin_call` | A plugin host function was invoked |
| `heartbeat` | An activity heartbeat event |

## Heartbeat / Keepalive

### Worker Heartbeat

A background goroutine in the worker updates `heartbeat_at` for all in-flight
instances at a configurable interval (default 5s, flag: `--heartbeat`):

```go
UPDATE workflow_instances
SET heartbeat_at = now()
WHERE id = $1 AND assigned_to = $2;
```

If the heartbeat fails (e.g., database connection loss), the worker enters a
reconnect loop with exponential backoff.

### Instance Liveness

The heartbeat timestamp serves two purposes:

1. **Ownership verification**: The heartbeat `UPDATE` checks `assigned_to = $2`.
   If another worker has stolen the instance (e.g., after a network partition
   resolved), the heartbeat silently fails and the worker releases the instance.
2. **Stale detection**: The reaper uses `heartbeat_at` to find instances whose
   worker has crashed.

## Reaper

A background goroutine runs every 30 seconds to reclaim instances with stale
heartbeats:

```sql
UPDATE workflow_instances
SET status = 'ready', assigned_to = NULL
WHERE status = 'running'
  AND heartbeat_at < now() - interval '30 seconds'
RETURNING id;
```

The 30-second window (2x the default 5s heartbeat interval + margin) ensures
that slow heartbeats due to GC pauses or transient load do not cause false
positives.

The reaper also handles:

- **Concurrency key cleanup**: Removes expired concurrency keys every 60s.
- **Idempotency key cleanup**: Removes expired idempotency keys every hour.

## Schedule Loop

A background goroutine fires due cron schedules every 15 seconds:

1. Query `workflow_schedules` for rows where `next_run_at <= now()` AND
   `enabled = true`.
2. For each due schedule, start a new workflow instance with the latest
   deployed version.
3. Compute the next run time using the cron expression and update
   `next_run_at`.

## Compaction

When a workflow's event history grows beyond a configurable threshold
(`--compaction-threshold`, default 1000 events), the engine can compact the
history. Compaction replaces sequential events with summary snapshots,
reducing storage and replay time for long-running workflows.

The compaction loop runs every 5 minutes by default (`--compaction-interval`).

## Database Connection Management

The worker uses a connection pool configured as:

```go
db.SetMaxOpenConns(concurrency + 5)   // Allow headroom for heartbeats, etc.
db.SetMaxIdleConns(5)
db.SetConnMaxLifetime(5 * time.Minute)
```

### Connection Failure Handling

When the engine detects a database connection error:

1. The dispatch loop backs off with exponential backoff (1s, 2s, 4s, ... up to
   30s).
2. In-flight workflows continue executing but cannot persist new events.
3. On reconnect, claimed instances are released and re-queued for another
   worker to claim.
4. The reaper handles instances from workers that did not reconnect.

## Shutdown

On SIGINT/SIGTERM:

1. The context is cancelled, stopping the dispatch, heartbeat, reaper,
   schedule, and compaction loops.
2. The worker waits for all in-flight workflow goroutines to complete.
3. Background plugin workers are given 30 seconds to shut down gracefully
   before force-exit.

## WASM Module Cache

WASM binaries are cached in memory on the worker, keyed by
`def_name:def_version`. Cache lookup is protected by `sync.RWMutex` (RLock for
reads, Lock for writes). This avoids repeated database loads of the
`wasm_bytes` column (which can be several MB per module).

## Tracing

When the worker is configured with an OTLP endpoint, each workflow execution
creates a root span. Each event step creates a child span with attributes for
step number, event type, service, and operation. See `internal/telemetry/` for
implementation details.
