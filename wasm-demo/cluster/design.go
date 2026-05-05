// Package cluster — durable workflow cluster design analysis.
//
// This file works through the data model, access patterns, queue semantics,
// and database options for an elastic cluster that executes durable WASM
// workflows with checkpoint/replay.
//
// Build it into the demo binary for a structured walkthrough:
//   GOTOOLCHAIN=local /home/rcownie/go/bin/go build -o /tmp/cluster_demo ./cluster/
//   /tmp/cluster_demo

package main

import (
	"fmt"
	"strings"
)

func main() {
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("  Durable Workflow Cluster — Data Model & Queue Design")
	fmt.Println(strings.Repeat("=", 72))

	accessPatterns()
	whyNotJustAQueue()
	postgresDesign()
	splitArchitecture()
	recommendation()
}

func accessPatterns() {
	fmt.Println()
	fmt.Println("── 1. WHAT STATE EXISTS ──")
	fmt.Println()
	fmt.Println("  There are four distinct kinds of state, each with different")
	fmt.Println("  access patterns and consistency requirements:")
	fmt.Println()
	fmt.Println("  ┌─────────────────────┬──────────────────┬──────────────────────────┐")
	fmt.Println("  │ State               │ Access Pattern   │ Consistency Need         │")
	fmt.Println("  ├─────────────────────┼──────────────────┼──────────────────────────┤")
	fmt.Println("  │ WASM binary         │ Write once,      │ Eventual. Rare updates,  │")
	fmt.Println("  │ (workflow def)      │ read many        │ versioned immutable.     │")
	fmt.Println("  ├─────────────────────┼──────────────────┼──────────────────────────┤")
	fmt.Println("  │ Event history       │ Append-only,     │ STRICT order per wf.     │")
	fmt.Println("  │ (checkpoint log)    │ sequential read  │ Step N+1 cannot exist    │")
	fmt.Println("  │                     │ for replay.      │ without step N. Single   │")
	fmt.Println("  │                     │ Never updated.   │ writer per workflow.     │")
	fmt.Println("  ├─────────────────────┼──────────────────┼──────────────────────────┤")
	fmt.Println("  │ Work queue          │ Enqueue, atomic  │ At-least-once delivery.  │")
	fmt.Println("  │ (pending/resume)    │ claim, heartbeat,│ Must not lose tasks.      │")
	fmt.Println("  │                     │ complete/fail.   │ Visibility timeout for    │")
	fmt.Println("  │                     │ High throughput. │ worker crash detection.   │")
	fmt.Println("  ├─────────────────────┼──────────────────┼──────────────────────────┤")
	fmt.Println("  │ Timers              │ Insert future    │ Must fire within ~1s of   │")
	fmt.Println("  │ (sleep, retry-at)   │ time, poll due.  │ target. Late is better    │")
	fmt.Println("  │                     │ Dense key space. │ than never.               │")
	fmt.Println("  └─────────────────────┴──────────────────┴──────────────────────────┘")
	fmt.Println()
}

func whyNotJustAQueue() {
	fmt.Println("── 2. WHY NOT JUST USE A MESSAGE QUEUE? ──")
	fmt.Println()
	fmt.Println("  A naive approach: put each workflow step on a queue. When a worker")
	fmt.Println("  finishes step N, enqueue step N+1. But this breaks down:")
	fmt.Println()
	fmt.Println("  Problem 1 — Replay requires full history:")
	fmt.Println("    After a crash, the worker rebuilding state needs ALL prior steps")
	fmt.Println("    in order. A queue only gives you the current step. You'd need to")
	fmt.Println("    store the history somewhere anyway.")
	fmt.Println()
	fmt.Println("  Problem 2 — Compensation is stateful:")
	fmt.Println("    When shipping.CreateShipment fails, the compensation code calls")
	fmt.Println("    payments.Refund AND inventory.Release. These depend on data from")
	fmt.Println("    earlier steps (charge_id, reservation_id). With a pure queue,")
	fmt.Println("    step N+1 doesn't have access to step N's return values.")
	fmt.Println()
	fmt.Println("  Problem 3 — Non-deterministic branching:")
	fmt.Println("    if len(cart) == 0 { return error }   // 0 API calls")
	fmt.Println("    if inventoryResult.Status != \"reserved\" { compensate } // varies")
	fmt.Println("    The number and identity of API calls depends on DATA, not just")
	fmt.Println("    the workflow definition. A static queue topology can't capture this.")
	fmt.Println()
	fmt.Println("  So the queue is at the WORKFLOW INSTANCE level, not the STEP level.")
	fmt.Println("  Each workflow instance is a unit of work. The worker drives the")
	fmt.Println("  workflow through multiple steps internally, checkpointing as it goes.")
	fmt.Println()
}

func postgresDesign() {
	fmt.Println("── 3. POSTGRES-ONLY DESIGN ──")
	fmt.Println()
	fmt.Println("  PostgreSQL can serve all four state types with a clean schema.")
	fmt.Println("  Here's the concrete DDL:")
	fmt.Println()

	ddl := `
-- ==========================================================================
-- Table 1: Workflow definitions (WASM binaries + metadata)
-- ==========================================================================
CREATE TABLE workflow_defs (
    name       TEXT NOT NULL,
    version    INT  NOT NULL DEFAULT 1,
    wasm_bytes BYTEA NOT NULL,          -- compiled WASM module (~50-200KB)
    created_at TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (name, version)
);
-- Access: read by (name, version). Workers cache in memory.
-- Latency: <1ms from buffer cache, ~10ms from disk.
-- Size:    ~200KB per version. 1000 versions = 200MB. Negligible.

-- ==========================================================================
-- Table 2: Workflow instances (the work queue)
-- ==========================================================================
CREATE TABLE workflow_instances (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    def_name        TEXT NOT NULL,
    def_version     INT  NOT NULL,
    status          TEXT NOT NULL DEFAULT 'ready',  -- ready|running|done|failed
    input           JSONB,               -- arguments for the workflow
    result          JSONB,               -- final return value
    error_msg       TEXT,                -- final error
    -- Queue fields:
    assigned_to     TEXT,                -- worker ID that claimed this
    assigned_at     TIMESTAMPTZ,
    heartbeat_at    TIMESTAMPTZ,         -- worker updates this to prove liveness
    next_wake_at    TIMESTAMPTZ NOT NULL DEFAULT now(), -- for timers/sleeps
    -- Locking:
    run_seq         INT NOT NULL DEFAULT 0,  -- optimistic lock version
    -- Indexes:
    created_at      TIMESTAMPTZ DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

-- The work-queue index: find runnable workflows efficiently.
-- A workflow is runnable when:
--   status = 'ready'                           (new work)
--   OR (status = 'running' AND heartbeat expired) (crashed worker, retry)
CREATE INDEX idx_runnable ON workflow_instances (next_wake_at, created_at)
    WHERE status IN ('ready', 'running')
      AND next_wake_at <= now();

-- ==========================================================================
-- Table 3: Event history (the checkpoint log)
-- ==========================================================================
CREATE TABLE event_history (
    workflow_id UUID    NOT NULL,
    step        INT     NOT NULL,          -- monotonically increasing
    service     TEXT    NOT NULL,          -- e.g. "catalog"
    operation   TEXT    NOT NULL,          -- e.g. "LookupItem"
    request     JSONB   NOT NULL,          -- the call arguments
    response    JSONB,                     -- the recorded result
    error       TEXT,                      -- if the call failed
    recorded_at TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (workflow_id, step)
);
-- Access: SELECT ... WHERE workflow_id = $1 ORDER BY step
--         for replay. B-tree on (workflow_id, step) is optimal.
-- Partition by workflow_id hash for scale.

-- ==========================================================================
-- Table 4: Timers (indexed future events)
-- ==========================================================================
-- We use workflow_instances.next_wake_at for this. When a workflow calls
-- time.Sleep(), the worker sets next_wake_at = now() + duration and
-- unassigns itself. The workflow goes back to the queue.
-- A separate table is only needed for complex timer patterns.
`
	fmt.Println("  " + strings.ReplaceAll(ddl, "\n", "\n  "))
	fmt.Println()

	fmt.Println("  Key operations implemented as SQL:")
	fmt.Println()

	fmt.Println(`  -- CLAIM A WORKFLOW TO EXECUTE (atomic dequeue):
  UPDATE workflow_instances
  SET status = 'running',
      assigned_to = $worker_id,
      assigned_at = now(),
      heartbeat_at = now(),
      run_seq = run_seq + 1
  WHERE id = (
      SELECT id FROM workflow_instances
      WHERE status IN ('ready', 'running')
        AND next_wake_at <= now()
        AND (status = 'ready'
             OR heartbeat_at < now() - INTERVAL '30 seconds')
      ORDER BY created_at
      LIMIT 1
      FOR UPDATE SKIP LOCKED
  )
  RETURNING *;

  -- WORKER HEARTBEAT (prevent re-assignment):
  UPDATE workflow_instances
  SET heartbeat_at = now()
  WHERE id = $workflow_id AND assigned_to = $worker_id;

  -- APPEND EVENT HISTORY STEP (only if no gap — prevents split brain):
  INSERT INTO event_history (workflow_id, step, service, operation, request, response, error)
  SELECT $workflow_id, $step, $service, $operation, $request, $response, $error
  WHERE NOT EXISTS (
      SELECT 1 FROM event_history
      WHERE workflow_id = $workflow_id AND step = $step
  );

  -- COMPLETE WORKFLOW:
  UPDATE workflow_instances
  SET status = 'done', result = $result, completed_at = now(), assigned_to = NULL
  WHERE id = $workflow_id AND assigned_to = $worker_id;

  -- SET TIMER (workflow sleeps until future time):
  UPDATE workflow_instances
  SET next_wake_at = $wake_time, assigned_to = NULL, status = 'ready'
  WHERE id = $workflow_id AND assigned_to = $worker_id;`)
	fmt.Println()
}

func splitArchitecture() {
	fmt.Println("── 4. SPLIT ARCHITECTURE FOR HIGHER SCALE ──")
	fmt.Println()
	fmt.Println("  When you outgrow a single Postgres instance, split by concern:")
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                     WORKER POOL                             │")
	fmt.Println("  │  ┌──────────┐  ┌──────────┐  ┌──────────┐                 │")
	fmt.Println("  │  │ Worker 1 │  │ Worker 2 │  │ Worker N │  (Go processes)  │")
	fmt.Println("  │  │ wazero   │  │ wazero   │  │ wazero   │                 │")
	fmt.Println("  │  └────┬─────┘  └────┬─────┘  └────┬─────┘                 │")
	fmt.Println("  └───────┼─────────────┼─────────────┼────────────────────────┘")
	fmt.Println("          │             │             │")
	fmt.Println("          ▼             ▼             ▼")
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                   WORK QUEUE LAYER                           │")
	fmt.Println("  │                                                                │")
	fmt.Println("  │  Redis Streams  or  SQS  or  Kafka consumer groups            │")
	fmt.Println("  │                                                                │")
	fmt.Println("  │  Responsibilities:                                             │")
	fmt.Println("  │  • At-least-once delivery with visibility timeout              │")
	fmt.Println("  │  • Heartbeat monitoring (XCLAIM / ChangeMessageVisibility)    │")
	fmt.Println("  │  • Dead-letter queue for repeatedly failing workflows         │")
	fmt.Println("  │  • Priority lanes (production vs batch vs retry)              │")
	fmt.Println("  └──────────────────────────┬───────────────────────────────────┘")
	fmt.Println("                             │")
	fmt.Println("                             ▼")
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                   STATE STORE (source of truth)               │")
	fmt.Println("  │                                                                │")
	fmt.Println("  │  FoundationDB  or  CockroachDB  or  sharded Postgres          │")
	fmt.Println("  │                                                                │")
	fmt.Println("  │  Responsibilities:                                             │")
	fmt.Println("  │  • Event history (ordered, immutable, sequential access)      │")
	fmt.Println("  │  • Workflow instance state (mutable, conditional updates)     │")
	fmt.Println("  │  • Strong consistency for the (workflow_id, step) primary key │")
	fmt.Println("  └──────────────────────────────────────────────────────────────┘")
	fmt.Println("                             │")
	fmt.Println("                             ▼")
	fmt.Println("  ┌──────────────────────────────────────────────────────────────┐")
	fmt.Println("  │                   BLOB STORE                                  │")
	fmt.Println("  │                                                                │")
	fmt.Println("  │  S3 / GCS / MinIO                                             │")
	fmt.Println("  │                                                                │")
	fmt.Println("  │  Responsibilities:                                             │")
	fmt.Println("  │  • WASM binaries (~50-200KB each, versioned, immutable)       │")
	fmt.Println("  │  • Workers cache locally with ETag-based validation           │")
	fmt.Println("  └──────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  Why this split works:")
	fmt.Println()
	fmt.Println("  The event history is the HARD problem. It needs:")
	fmt.Println("    • Strict ordering per workflow instance")
	fmt.Println("    • Conditional append (step N only after step N-1)")
	fmt.Println("    • Efficient sequential scan for replay")
	fmt.Println("    • Potentially unbounded retention (years of history)")
	fmt.Println()
	fmt.Println("  This is exactly what a log-structured or ordered-KV store is")
	fmt.Println("  designed for. FoundationDB's ordered key space with transactions")
	fmt.Println("  is a particularly good fit because you get:")
	fmt.Println("    • history:{workflow_id}:{step} — natural ordering")
	fmt.Println("    • Atomic conditional insert (step N+1 only if step N exists)")
	fmt.Println("    • Range reads for replay (scan from step 0)")
	fmt.Println()
	fmt.Println("  The queue is the THROUGHPUT problem. It needs:")
	fmt.Println("    • Millions of visible/invisible state transitions per second")
	fmt.Println("    • At-least-once semantics (not exactly-once)")
	fmt.Println("    • Partitioning by workflow_id for horizontal scale")
	fmt.Println()
	fmt.Println("  Redis Streams shines here:")
	fmt.Println("    • Consumer groups with XREADGROUP for partitioned consumption")
	fmt.Println("    • XPENDING + XCLAIM for timeout-based reassignment")
	fmt.Println("    • XACK for explicit completion")
	fmt.Println("    • ~100K messages/second per node, linear scaling")
	fmt.Println()
}

func recommendation() {
	fmt.Println("── 5. RECOMMENDED PATH ──")
	fmt.Println()
	fmt.Println("  Phase 1 — Prototype (week 1-4):")
	fmt.Println("  ┌─────────────────────────────────────────────────────────────┐")
	fmt.Println("  │  SINGLE POSTGRES INSTANCE for everything.                   │")
	fmt.Println("  │                                                             │")
	fmt.Println("  │  Why:                                                       │")
	fmt.Println("  │  • One database to operate, backup, and reason about        │")
	fmt.Println("  │  • SKIP LOCKED dequeue handles ~5K workflows/sec easily     │")
	fmt.Println("  │  • Event history queries are simple indexed scans           │")
	fmt.Println("  │  • WASM blobs in BYTEA are fine until you have >10K versions│")
	fmt.Println("  │  • LISTEN/NOTIFY gives instant worker wake-up               │")
	fmt.Println("  │  • You can always split later — the data model doesn't      │")
	fmt.Println("  │    change, only the storage backend                          │")
	fmt.Println("  └─────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  Phase 2 — Scale out (when needed):")
	fmt.Println("  ┌─────────────────────────────────────────────────────────────┐")
	fmt.Println("  │  ADD REDIS for the work queue.                              │")
	fmt.Println("  │                                                             │")
	fmt.Println("  │  • Workers claim from Redis Streams consumer groups         │")
	fmt.Println("  │  • Postgres remains the state-of-record                    │")
	fmt.Println("  │  • On claim, worker loads full state from Postgres          │")
	fmt.Println("  │  • On step complete, worker writes to Postgres FIRST,       │")
	fmt.Println("  │    then ACKs in Redis (write-behind for durability)         │")
	fmt.Println("  │  • Redis failure → workflows are picked up by timeout       │")
	fmt.Println("  │    and replayed from Postgres state (at-least-once)         │")
	fmt.Println("  └─────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  Phase 3 — Global scale (if needed):")
	fmt.Println("  ┌─────────────────────────────────────────────────────────────┐")
	fmt.Println("  │  SWAP POSTGRES for FoundationDB or sharded CockroachDB.     │")
	fmt.Println("  │                                                             │")
	fmt.Println("  │  • Event history is the reason — it grows unbounded and    │")
	fmt.Println("  │    needs horizontal write scaling.                          │")
	fmt.Println("  │  • FoundationDB's ordered KV with transactions is a         │")
	fmt.Println("  │    natural fit for (workflow_id, step) ordering.            │")
	fmt.Println("  │  • CockroachDB gives you familiar SQL with horizontal       │")
	fmt.Println("  │    sharding, but adds cross-shard transaction latency.      │")
	fmt.Println("  └─────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  ── Concrete cluster topology ──")
	fmt.Println()
	fmt.Println(`                    ┌──────────────┐
                    │   Deployer   │  (CLI/CI pushes WASM binaries)
                    └──────┬───────┘
                           │ INSERT INTO workflow_defs
                           ▼
              ┌─────────────────────────┐
              │   PostgreSQL (primary)   │
              │   • workflow_defs        │
              │   • workflow_instances   │
              │   • event_history        │
              └────┬──────────┬─────────┘
                   │          │
              read/write   LISTEN/NOTIFY
                   │          │
                   ▼          ▼
     ┌─────────────────────────────────────────────┐
     │              Worker Pool                     │
     │                                              │
     │  ┌──────────┐  ┌──────────┐  ┌──────────┐  │
     │  │ Worker 1 │  │ Worker 2 │  │ Worker N │  │
     │  │          │  │          │  │          │  │
     │  │ Claim    │  │ Claim    │  │ Claim    │  │
     │  │ workflow │  │ workflow │  │ workflow │  │
     │  │   ↓      │  │   ↓      │  │   ↓      │  │
     │  │ Load WASM│  │ Load WASM│  │ Load WASM│  │
     │  │   ↓      │  │   ↓      │  │   ↓      │  │
     │  │ Replay   │  │ Replay   │  │ Replay   │  │
     │  │   ↓      │  │   ↓      │  │   ↓      │  │
     │  │ Execute  │  │ Execute  │  │ Execute  │  │
     │  │   ↓      │  │   ↓      │  │   ↓      │  │
     │  │ Heartbeat│  │ Heartbeat│  │ Heartbeat│  │
     │  └──────────┘  └──────────┘  └──────────┘  │
     │                                              │
     │  Each worker:                                │
     │  • Runs N goroutines, each driving one       │
     │    workflow (wazero WASM runtime)            │
     │  • Heartbeats every 5s to Postgres           │
     │  • Caches WASM modules in memory (LRU)       │
     │  • Checkpoints after each durable_call       │
     └─────────────────────────────────────────────┘`)
	fmt.Println()
	fmt.Println("  ── Why this is better than Temporal's architecture ──")
	fmt.Println()
	fmt.Println("  Temporal has 4+ services (Frontend, History, Matching, Worker)")
	fmt.Println("  communicating via gRPC, each independently scaled, with a")
	fmt.Println("  persistence layer underneath. This is necessary for Temporal's")
	fmt.Println("  scale (Uber-scale) but adds enormous operational complexity.")
	fmt.Println()
	fmt.Println("  This design collapses everything into: workers + database.")
	fmt.Println("  The database IS the queue (via SKIP LOCKED), the state store,")
	fmt.Println("  and the blob store. The insight is that for most use cases,")
	fmt.Println("  PostgreSQL handles all three roles well enough that the")
	fmt.Println("  operational simplicity dominates any scaling concerns.")
	fmt.Println()
	fmt.Println("  When you DO need to scale, you split the queue (Redis) from the")
	fmt.Println("  state store (Postgres/FDB), and the data model doesn't change —")
	fmt.Println("  it's the same workflow_id + step model, just different backends.")
	fmt.Println()
}
