# Cleat Execution on an Elastic Cluster

## A design for resilient, composable, observable workflows with near-standard Go

---

## 1. Overview

This is a design for a durable execution system — the kind of thing that runs business workflows (orders, loan approvals, insurance claims, employee onboarding) that must survive machine crashes, network failures, and software bugs without losing state or producing incorrect results.

The system has three goals:

1. **Write normal Go.** Developers write business logic using ordinary Go functions, conditionals, loops, and library calls. Composability through function calls at arbitrary depth is a first-class requirement — a workflow can call a helper which calls another helper which makes an API call, and the system tracks the transitive closure automatically.

2. **Operational simplicity.** The entire infrastructure is a PostgreSQL database plus a pool of stateless worker processes. No separate queue service, no history service, no matching service. Deploying a new workflow version is an `INSERT`. Rolling back is an `UPDATE`.

3. **Built-in observability.** Because every external interaction must be recorded for durability (replay-after-crash), that same record serves as structured logging, distributed tracing, metrics, and business-level querying. The developer writes zero observability code.

**Cleat is NOT a database. It is a worker pool.** All state -- workflow instances, event history, schedules, deployment metadata -- lives in PostgreSQL. Workers are stateless compute: they claim work from Postgres, execute it in a sandboxed WASM runtime, and write results back. The database is YOURS -- managed Postgres (RDS, Cloud SQL, Crunchy), self-hosted Patroni, or anything that speaks the PostgreSQL wire protocol. Cleat just needs a connection string. Workers and database scale independently: add workers for throughput, scale Postgres the way you always do.

The system compiles workflow code to WASM, stores it as a versioned blob in the database, and executes it in a wazero runtime on any worker in the cluster. Workflow instances carry a `(def_name, def_version)` pointer to their code — so in-flight workflows always replay against the exact code they started with, even as new versions are deployed.

---

## 2. The Problem

Business processes are long-running, stateful, and must be correct. An order processing workflow might:

1. Validate inventory (API call to catalog service)
2. Reserve stock (API call to inventory service)
3. Charge the customer (API call to payments service)
4. Create a shipment (API call to shipping service)
5. Send confirmation (API call to notifications service)

At any point between these steps, the machine running the code could crash. The network could partition. The database could become briefly unreachable. A naive implementation would leave the process in an inconsistent state: the customer was charged but inventory was never reserved, or the shipment was created but the customer was never notified.

**Durable execution** solves this by recording the result of every external interaction in an append-only event history. When a crash occurs and the workflow is reassigned to another worker, the new worker replays the event history: it re-executes the workflow code from the beginning, but every external call returns the cached result from the history instead of making a real API call. Once the replay catches up to the point of the crash, execution continues with fresh API calls.

This guarantees that the workflow makes each external call **at most once** (the result is recorded and replayed) and **at least once** (the workflow is retried until it completes).

### The versioning problem

Workflows can run for days, weeks, or months. During that time, the workflow code will evolve. On Monday, Alice starts an order with PlaceOrder v1. On Tuesday, the team deploys PlaceOrder v2 with a different API call order. On Wednesday, Alice's workflow is still waiting for a manager approval. If the worker node crashes on Wednesday and a new worker replays Alice's workflow with v2 code, the replay **diverges**: v2 expects different API calls in a different order than what's recorded in Alice's event history.

The workflow code is part of the durable contract. In-flight instances must replay against the exact code they started with. This creates an operational burden: you must keep old code available until all instances that depend on it have completed.

---

## 3. Design

### 3.1 Developer experience

The developer writes workflows in near-standard Go. The only required import is a `HostCalls` struct that acts as the gateway to external services:

```go
func PlaceOrder(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    if len(cart) == 0 {
        return "", fmt.Errorf("cart is empty")
    }

    // Delegate to a composable helper — which itself makes API calls internally
    reservation, err := validateAndReserve(h, userID, cart)
    if err != nil {
        return "", err
    }

    charge, err := processPayment(h, userID, reservation.TotalCents)
    if err != nil {
        releaseReservation(h, reservation.ReservationID) // compensation
        return "", fmt.Errorf("payment failed: %w", err)
    }

    trackingID, err := fulfillOrder(h, reservation, charge)
    if err != nil {
        refundPayment(h, charge.ChargeID)                  // compensation
        releaseReservation(h, reservation.ReservationID)   // compensation
        return "", fmt.Errorf("fulfillment failed: %w", err)
    }

    _ = notifyCustomer(h, userID, trackingID)
    return trackingID, nil
}
```

Key properties of the developer experience:

- **No workflow/activity distinction.** The developer doesn't decide what's a "workflow" vs an "activity." They write functions that call other functions. The system discovers the API boundaries transitively.

- **Composable libraries.** `validateAndReserve` is a library function that calls `checkItemAvailability` (which calls `catalog.LookupItem` via `h.DurableCall`). This library can be reused across multiple workflows. The call graph analysis discovers the full transitive closure of durable functions.

- **Compensation is ordinary error handling.** The `if err != nil { releaseReservation(...) }` pattern is standard Go. No saga definition, no compensation framework. The system records the compensation calls in the event history like any other call.

- **No observability code.** The developer writes no log statements, no span contexts, no metric counters. The host runtime captures everything because it stands between the workflow and the outside world.

### 3.2 The HostCalls interface

`HostCalls` is the boundary between workflow code and the outside world. It's a small, stable interface:

```go
type HostCalls struct {
    DurableCall func(service, operation, requestJSON string) (responseJSON string, err error)
    DurableLog  func(message string)
    Now         func() int64
}
```

`DurableCall` is the only way a workflow can interact with external services. On first execution, the host makes the real HTTP call and records the result in the event history. On replay, the host returns the cached result without making the real call.

Because this interface has only 3-5 functions and changes rarely, it can be versioned with standard API evolution rules: add new functions safely, never change existing signatures, deprecate only when no active WASM modules import the old function.

### 3.3 Call graph analysis

A source transformer (using `go/parser`, `go/types`, and SSA from the Go standard library) analyzes the workflow code to:

1. **Identify durable leaves** — functions that call `DurableCall` (the network boundary).
2. **Compute the transitive closure** — any function that directly or transitively calls a durable leaf is in the durable set.
3. **Generate WASM bindings** — for each function in the durable set, generate `//go:wasmimport` stubs and a host adapter that bridges the user's `HostCalls` interface to the low-level WASM imports.
4. **Generate the WASM export** — a `//go:wasmexport` entry point that the host calls to start or resume the workflow.

The user's code is never modified. The transformer generates the glue layer and compiles the combined result to WASM via `GOOS=wasip1 GOARCH=wasm` (standard Go).

Critically, the transformer does NOT modify the user's business logic. It generates the adapter layer that sits between the user's clean Go code and the WASM host interface. The user's code calls `h.DurableCall(...)`. The generated adapter converts that to the low-level `//go:wasmimport` calls that cross the WASM boundary.

### 3.4 WASM compilation and versioning

Workflow code is compiled to WASM and stored in the database:

```sql
CREATE TABLE workflow_defs (
    name       TEXT NOT NULL,
    version    INT  NOT NULL DEFAULT 1,
    wasm_bytes BYTEA NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now(),
    deprecated BOOLEAN DEFAULT false,
    PRIMARY KEY (name, version)
);
```

Each workflow instance carries a pointer to its code version:

```sql
CREATE TABLE workflow_instances (
    id          UUID PRIMARY KEY,
    def_name    TEXT NOT NULL,
    def_version INT  NOT NULL,
    status      TEXT NOT NULL DEFAULT 'ready',
    input       JSONB,
    -- ... queue fields, heartbeat, etc.
);
```

This decouples workflow code lifecycle from worker lifecycle:

| Operation | Temporal | This system |
|---|---|---|
| Deploy new version | Deploy new worker pool + configure task queue routing | `INSERT INTO workflow_defs` |
| Old version still running | Keep old worker pool running (weeks/months) | Same workers load old WASM blob from DB |
| Rollback | Deploy old worker pool, reroute | `UPDATE workflow_defs SET deprecated = false` |
| Deprecate old version | Drain task queue, shut down worker pool | Query for active instances: `SELECT COUNT(*) FROM workflow_instances WHERE def_name = $1 AND def_version = $2 AND status IN ('ready', 'running')`. When 0, mark deprecated. |

The worker binary is a **stable runtime** — like a JVM for workflows. It changes only when the `HostCalls` interface changes, which should be rare. Workflow business logic changes are database operations, not service deployments.

### 3.5 Multi-language support

Because the worker/wasm boundary is defined by the host interface (8 functions), any language that compiles to WASM can produce workflow modules. The worker doesn't know or care what language produced the WASM bytes — it only calls the exported entry point and responds to host function imports.

Each language needs a transformer that generates the WASM bindings and host adapter for that language's idioms. The worker, database schema, event history format, and operations story are shared across all languages.

**Languages and feasibility:**

| Language | WASM compilation | Maturity | Transformer effort |
|---|---|---|---|
| **Go** | `GOOS=wasip1` | Production-ready | ~12 weeks (see 8.12) |
| **Rust** | `wasm32-wasip1` target, wasm-bindgen | Production-ready | ~8 weeks |
| **C** | Clang `--target=wasm32-wasip1` | Production-ready | ~6 weeks |
| **Zig** | `zig build-exe -target wasm32-wasip1` | Production-ready | ~6 weeks |
| **TypeScript** | AssemblyScript, Javy (Shopify) | Emerging, subset of JS | ~12 weeks |
| **C#** | NativeAOT + wasm-experimental (.NET 9+) | Emerging | ~10 weeks |
| **Python** | Pyodide / python2wasm | Not practical yet | Would need language subset |
| **Java/Kotlin** | TeaVM, GraalVM native-image | Experimental | Would need language subset |

The first tier (Go, Rust, C, Zig) covers a large fraction of backend engineering. Go for the majority of services, Rust for performance-sensitive or correctness-critical workflows, C/Zig for embedded and edge use cases.

**Why this is simpler than separate SDKs.** Temporal maintains separate SDKs in 6 languages, each reimplementing the deterministic runtime, event loop, coroutine shims, and activity/workflow abstractions. Bugs and behavioral differences between SDKs are an ongoing problem. In this system, the WASM interface IS the SDK: 8 host functions, one stable ABI. A language transformer generates a few hundred lines of adapter code that maps the language's idioms to those 8 functions — not a full runtime reimplementation.

**Example: a Rust workflow.** A Rust workflow using the generated host adapter:

```rust
use cleat::HostCalls;

pub fn place_order(h: &HostCalls, user_id: &str, cart: &[CartItem])
    -> Result<String, Error>
{
    if cart.is_empty() {
        return Err(Error::new("cart is empty"));
    }

    let reservation = validate_and_reserve(h, user_id, cart)?;

    let charge = process_payment(h, user_id, reservation.total_cents)
        .or_else(|e| {
            release_reservation(h, &reservation.reservation_id)?;
            Err(e)
        })?;

    let tracking_id = fulfill_order(h, &reservation, &charge)?;
    Ok(tracking_id)
}
```

The Rust transformer generates a `#[wasm_bindgen]` export that converts Rust types to/from WASM memory, and a `HostCalls` implementation backed by the 8 WASM host imports. The event history, checkpointing, replay, timers, signals, and cancellation behave identically regardless of whether the workflow was written in Go or Rust.

**Adoption path:** Ship Go first (largest audience, validates the architecture). Add Rust second (smaller but passionate audience, proves multi-language works). Prioritize beyond that based on demand.

### 3.6 Worker runtime

Workers are stateless Go processes. Each worker:

1. **Claims** a workflow instance from PostgreSQL using `SELECT ... FOR UPDATE SKIP LOCKED`.
2. **Loads** the WASM blob from `workflow_defs` for the instance's `(def_name, def_version)`.
3. **Loads** the event history from `event_history` for the instance.
4. **Replays** the WASM module, feeding cached results from the event history for steps that have already been recorded. The WASM module's `DurableCall` calls are intercepted by the host and returned from cache without making real API calls.
5. **Executes** new steps once replay catches up. Each `DurableCall` makes a real HTTP call, and the result is appended to `event_history`.
6. **Heartbeats** to `workflow_instances.heartbeat_at` every 5 seconds. If a worker crashes, another worker claims the instance after a 30-second timeout and replays from the last checkpoint.

The worker's event loop handles database failover gracefully. All database operations are idempotent:

- **Claim:** `FOR UPDATE SKIP LOCKED` prevents double-claim
- **Checkpoint:** `INSERT ... ON CONFLICT (workflow_id, step) DO NOTHING` prevents duplicate steps
- **Complete:** `UPDATE ... WHERE assigned_to = $worker_id` prevents double-completion
- **Release:** `UPDATE ... WHERE assigned_to = $worker_id` prevents double-release

When the database becomes unavailable (Patroni failover), the worker pauses in-flight workflows, waits for the new primary to become available, re-verifies ownership, and resumes. The in-memory WASM state is preserved during the outage.

### 3.6.1 Fencing token (split-brain prevention)

The heartbeat mechanism described above has a critical blind spot: if a worker's database connection drops briefly (network hiccup, Patroni failover, autovacuum storm), the heartbeat `UPDATE` silently fails. After the 30-second timeout elapses, another worker claims the workflow. But the original worker is still running — it has the WASM module loaded in memory and may be mid-execution. When its connection recovers, it continues executing and tries to write to `event_history`. The `ON CONFLICT DO NOTHING` clause on checkpoints prevents data corruption, but it does **not** tell the original worker to stop. Two workers execute the same workflow: split-brain.

**Fencing token (epoch).** A monotonically increasing `epoch` column on `workflow_instances` provides lease ownership:

```sql
ALTER TABLE workflow_instances ADD COLUMN epoch BIGINT NOT NULL DEFAULT 1;
```

- **Claiming with epoch increment.** The worker atomically claims ownership and increments the epoch in a single `UPDATE`:

  ```sql
  UPDATE workflow_instances
  SET assigned_to = $worker_id,
      heartbeat_at = now(),
      epoch = epoch + 1
  WHERE id = $id
    AND (assigned_to IS NULL
         OR heartbeat_at < now() - interval '30 seconds')
  RETURNING epoch;
  ```

  The returned epoch is stored in the worker's in-memory state for this workflow. Every subsequent operation uses this cached epoch.

- **Heartbeat with epoch check.** The worker includes its known epoch in each heartbeat:

  ```sql
  UPDATE workflow_instances
  SET heartbeat_at = now()
  WHERE id = $id
    AND assigned_to = $worker_id
    AND epoch = $known_epoch;
  ```

  If `rows_affected == 0`, the worker has been **fenced** — another worker claimed the workflow and incremented the epoch. The worker MUST stop immediately.

**Detection and response.** The worker checks `rows_affected` after every heartbeat cycle (every 5 seconds). When fencing is detected:

1. **Kill the WASM runtime.** Call `runtime.Close()` on the wazero `Module` instance for this workflow. This discards all in-memory WASM state immediately.
2. **Abort in-flight operations.** Cancel any outstanding `DurableCall` HTTP requests, `DurableSleep` timers, and `DurableAwaitSignals` waiters associated with this workflow. Do NOT write any new events to `event_history`.
3. **Log and release.** Emit an error-level log (`"fenced: worker {id} lost lease on workflow {workflow_id}"`), then release the workflow goroutine and all local resources.

As a defensive measure, the worker also performs a lightweight fencing check before every side-effecting operation (`DurableCall`, `DurableSleep`, checkpoint `INSERT`). After each heartbeat succeeds, the worker caches the confirmed epoch. Before making an API call, it runs a fast `SELECT epoch FROM workflow_instances WHERE id = $id`. If the epoch changed since the last heartbeat confirmation, the worker treats this as a fencing event and aborts immediately. This catches the case where the heartbeat loop missed the fencing signal (e.g., if a long-running `DurableCall` suppresses heartbeats for multiple cycles).

**Checkpoint guard.** Every checkpoint `INSERT` into `event_history` includes a transactional verification of the epoch:

```sql
INSERT INTO event_history (workflow_id, step, service, operation, ...)
SELECT $workflow_id, $step, $service, $operation, ...
WHERE EXISTS (
    SELECT 1 FROM workflow_instances
    WHERE id = $workflow_id
      AND assigned_to = $worker_id
      AND epoch = $known_epoch
);
```

If `rows_affected == 0`, the `EXISTS` subquery returned false — the epoch has been incremented by another worker. The transaction aborts and the worker treats this as a fencing event. This guards against the narrow race where a heartbeat succeeds but the workflow is re-claimed before the checkpoint is written.

### 3.7 PostgreSQL as the infrastructure

The entire system runs on a single PostgreSQL database (plus Patroni for HA). PostgreSQL serves four roles:

| Role | How | Table |
|---|---|---|
| Blob store | WASM binaries as BYTEA | `workflow_defs` |
| State store | Append-only event history | `event_history` |
| Work queue | `SELECT ... FOR UPDATE SKIP LOCKED` | `workflow_instances` |
| Timer service | Indexed `next_wake_at` column | `workflow_instances` |

**Why PostgreSQL instead of a message queue?** Workflow steps are not independent queue messages. Replay requires the full ordered history. Compensation actions need data from prior steps (`charge_id`, `reservation_id`). Branching is data-dependent (the set of API calls depends on the result of previous calls). The queue is at the **workflow instance** level, not the step level.

**Why PostgreSQL instead of Temporal's 4+ services?** Temporal requires operating a Frontend, History, Matching, and Worker service — each independently scaled, each stateful, each needing HA configuration. For most use cases, PostgreSQL handles the queue, state, and blob storage roles well enough that the operational simplicity dominates any scaling concerns.

**Resilience** is achieved through synchronous streaming replication (no lost commits), Patroni for automatic failover (~30s MTTR), WAL archiving to S3 for point-in-time recovery (protection against operator error), and application-level restricted database users (the worker app has `INSERT` and `UPDATE` permissions only — no `DROP`, `TRUNCATE`, or `DELETE`).

**Multiple cleat instances share one PostgreSQL cluster.** Because cleat is
stateless workers connecting to YOUR database, multiple worker pools can share
a single Postgres cluster. The `--schema` flag assigns each worker pool its own
PostgreSQL schema, providing full table-level isolation:
`team_a.workflow_instances` is a separate table from
`team_b.workflow_instances`.
The `--peer-schemas` flag enables cross-pool cooperation: if `team_a` starts a
child workflow defined in `team_b`'s schema, the host can resolve and claim it.
This gives teams the flexibility to operate isolated worker pools while still
enabling cross-team workflow composition when needed. Each schema gets its own
migration state, so schema changes are rolled out per pool, not globally.

### 3.8 Built-in observability

Because every `DurableCall` is intercepted by the host runtime and recorded in `event_history`, observability is a byproduct of the durability mechanism:

**Structured logging**: Every external call is recorded with workflow_id, step, timestamp, service, operation, full request, full response, duration_ms, error, worker_id, and WASM version. This is more detail than most hand-written log statements.

**Distributed tracing**: Each workflow execution is a trace (`workflow_id`). Each `DurableCall` is a span (`step` number). The trace is a simple query: `SELECT step, service, operation, duration_ms, error FROM event_history WHERE workflow_id = $1 ORDER BY step`.

**Metrics**: The host runtime emits Prometheus metrics without any workflow code instrumentation: workflow completion rates, per-version adoption, per-service latency percentiles, replay-from-cache counts, WASM load times.

**Business-level queries**: Because the event history is structured, you can ask business questions directly with SQL:

```sql
-- Orders that had payment failures after successful inventory reservation
SELECT COUNT(DISTINCT workflow_id)
FROM event_history
WHERE service = 'payments' AND operation = 'Charge' AND error IS NOT NULL
  AND workflow_id IN (
    SELECT workflow_id FROM event_history
    WHERE service = 'inventory' AND operation = 'Reserve' AND error IS NULL
  );
```

**Replay-based debugging**: A failed workflow can be replayed locally with the exact same WASM bytes, event history, and inputs. The only difference is that `DurableCall` returns cached responses instead of making real API calls. This is a time-travel debugger for business processes — no log grep, no reproduction steps.

The workflow author writes **zero observability code**. The host runtime stands between the workflow and the outside world, sees every interaction, records everything for durability, and that record IS observability.

---

## 4. Comparison to Existing Systems

### 4.1 Temporal

Temporal is the incumbent and the closest comparison. It is battle-tested at Uber, Netflix, and Snap scale.

| Concern | Temporal | This system |
|---|---|---|
| Write a workflow | Import SDK, use `workflow.ExecuteActivity()` | Write Go, use `h.DurableCall()` |
| Composability | Child workflows (separate history) or activity composition | Function calls (same history, same transaction scope) |
| Versioning | Task queues + multiple worker pools + `GetVersion()` | WASM blobs versioned in DB. Deploy = INSERT |
| Infrastructure | 4+ services (Frontend, History, Matching, Worker); can share a single PostgreSQL backend | PostgreSQL + stateless workers |
| Single-process mode | Possible with embedded services (dev/small scale) | Single binary + PostgreSQL (all scales) |
| Queue mechanism | Matching service (custom) | PostgreSQL SKIP LOCKED |
| State store | History service → Cassandra/MySQL/PostgreSQL | PostgreSQL event_history table |
| Observability | Temporal UI + SDK metrics + custom instrumentation | Automatic from event history |
| Security boundary | None (trusted code) | WASM sandbox |
| Maturity | Production since 2015, massive scale | Concept |
| Language support | Go, Java, TypeScript, Python, .NET, PHP | Any language that compiles to WASM (Go, Rust, C, etc.) |
| Ecosystem | UI, Cloud offering, metrics, debugging tools | None |

**Where Temporal wins:** Maturity, scale, ecosystem, multi-language SDKs, operational tooling. Temporal Cloud abstracts the operational complexity of running the four services — teams that adopt Temporal Cloud get the durability model without managing infrastructure. Temporal also supports PostgreSQL as its persistence backend, so the "4+ services" can share the same type of database this system uses. Temporal is the safe choice for a production system today.

**Where this system could win:** Operational simplicity. One database instead of four services to deploy, scale, monitor, and troubleshoot. Even though Temporal's services are architecturally distinct regardless of deployment topology (which means the operational complexity difference remains real), Temporal offers a single-process embedded mode for development and small-scale deployments, narrowing that gap. This system is a single binary plus PostgreSQL at all scales — from development to production. Additional advantages: no task queue routing, no worker pool lifecycle management, deploying a workflow is a database INSERT, built-in observability without instrumentation, and the WASM sandbox provides a security boundary that Temporal lacks.

### 4.2 Azure Durable Functions

Azure Durable Functions provides durable execution on top of Azure Functions and Azure Storage.

| Concern | Durable Functions | This system |
|---|---|---|
| Programming model | Orchestrator functions (deterministic) + activity functions | Near-standard Go with `DurableCall` boundary |
| Composability | Sub-orchestrations (separate histories) | Function calls (same history) |
| Versioning | In-flight instances tied to code version; must keep old functions deployed | WASM blobs in DB; worker is a stable runtime |
| Infrastructure | Azure Functions + Azure Storage (queues, tables, blobs) | PostgreSQL + stateless workers |
| Vendor lock-in | Azure only | Runs anywhere (cloud or on-prem) |
| Language support | C#, JavaScript, Python, Java, PowerShell | Any WASM-compilable language |

**Where Durable Functions wins:** Serverless model, Azure integration, no database to operate (Azure Storage is managed).

**Where this system could win:** Not tied to a single cloud provider. Simpler programming model (no orchestrator/activity distinction). One database instead of Storage accounts + queues + tables. The same binary runs in dev, on-prem, or any cloud.

### 4.3 AWS Step Functions

Step Functions expresses workflows as JSON state machines (Amazon States Language).

| Concern | Step Functions | This system |
|---|---|---|
| Expressiveness | JSON state machine (fork, join, choice, parallel, map) | Full Go with conditionals, loops, function calls |
| Composability | Nested state machines via ARN references | Function calls at arbitrary depth |
| Versioning | State machines are versioned; old executions use old versions | WASM blobs in DB; same pattern |
| Infrastructure | Fully managed AWS service | PostgreSQL + workers (self-managed) |
| Operations | Zero ops (managed) | Operate PostgreSQL + workers |

**Where Step Functions wins:** Fully managed, zero operations, deep AWS integration, visual workflow designer.

**Where this system could win:** Expressiveness — JSON state machines cannot express complex logic naturally. A 50-step state machine with nested branches is much harder to read and maintain than equivalent Go code. No per-state-machine-transition pricing.

### 4.4 Restate

Restate (restate.dev) is a newer entrant that makes existing service handlers durable by intercepting RPC calls.

| Concern | Restate | This system |
|---|---|---|
| Programming model | Write normal RPC handlers; Restate intercepts and makes them durable | Write Go with `DurableCall` boundary |
| Durability mechanism | Event log + replay (similar to Temporal) | Event log + replay (same concept) |
| Composability | Service-to-service calls within Restate context | Function calls within WASM module |
| Infrastructure | Restate server + application services | PostgreSQL + workers |

**Where Restate wins:** Even less intrusive — existing RPC handlers become durable without code changes. Strong fit for microservice architectures.

**Where this system could win:** Not tied to a specific RPC framework. Single database for everything. Versioning via WASM blobs.

### 4.5 Inngest

Inngest is an event-driven durable execution platform focused on serverless functions.

| Concern | Inngest | This system |
|---|---|---|
| Programming model | Step functions that respond to events | Near-standard Go workflows |
| Durability mechanism | Each step is individually retried with event sourcing | Replay-based with event history |
| Infrastructure | Inngest Cloud or self-hosted | PostgreSQL + workers |

**Where Inngest wins:** Strong event-driven model, good developer experience for event-sourced systems, managed cloud offering available.

**Where this system could win:** More general programming model (not tied to event-driven patterns). Full Go expressiveness.

---

## 5. Tradeoffs and Limitations

### 5.1 The explicit boundary is still there

The developer still needs to know which calls are external and mark them with `h.DurableCall(...)`. The original vision of transparent durability (parse any Go code, find all network calls automatically, make them durable) remains aspirational. In practice, statically identifying all network boundaries in a language with interface dispatch and reflection is extremely difficult. The `DurableCall` marker is functionally equivalent to Temporal's `ExecuteActivity` — it's an explicit API boundary that the runtime needs.

The advantage over Temporal is subtler: no workflow/activity distinction (activities must be registered, have type-safe interfaces, and can't call other activities directly), composability through function calls rather than child workflows, and WASM-based versioning that decouples code from worker deployment.

### 5.2 WASM compilation adds a build step

The transformation + WASM compilation pipeline adds complexity compared to `go build`. However, it's a one-time infrastructure investment that pays off in versioning and deployment simplicity.

### 5.3 Host interface versioning

The `HostCalls` interface between worker and WASM module must be backwards-compatible. New functions can be added, but existing function signatures cannot change. If a breaking change is needed, the worker must support multiple host API versions simultaneously (e.g., `durable_call` for v1 modules, `durable_call_v2` for v2 modules). This is manageable because the interface surface area is small (3-5 functions).

### 5.4 Go language restrictions for WASM

The design repeatedly describes workflow code as "near-standard Go," but the cumulative restrictions are substantial enough that the phrase is misleading. A more accurate characterization is **Go syntax with a durable execution DSL** — the developer writes Go-like code that compiles with the Go toolchain, but the standard library surface area available inside workflow functions is heavily restricted, and several core language idioms (goroutines, channels, map-iteration ordering) are forbidden or dangerous.

#### 5.4.1 Host-provided replacements (MUST use instead of standard library)

When a workflow function needs an operation that interacts with the outside world or with time, it MUST use the `*cleat.HostCalls` API rather than the corresponding standard library function:

| Goal                        | Standard library              | HostCalls replacement             |
|-----------------------------|-------------------------------|-----------------------------------|
| HTTP / RPC call             | `http.Client.Do()`            | `h.DurableCall(service, op, req)` |
| Current wall-clock time     | `time.Now()`                  | `h.Now()`                         |
| Non-deterministic sleep     | `time.Sleep()`                | `h.DurableSleep(durationMs)`      |
| Cleanup on any exit         | `defer`                       | `h.DurableDefer(fn)`              |
| Await external event        | Channel receive               | `h.DurableAwaitSignals()`         |
| Durable logging             | `log.Printf()` / `fmt.Println`| `h.DurableLog(message)`           |

These replacements ensure that every observable side effect is recorded in the event history and replayed deterministically during recovery.

##### 5.4.1.1 Durable time semantics in detail

`h.Now()` does **not** return the system wall-clock time. It returns the timestamp of the most recent durable event in the workflow's execution history. This is the mechanism that makes time deterministic across replays.

**How it works during original execution.** Each durable event (a call, a sleep, a signal, etc.) is timestamped with the wall-clock time when it is recorded. `h.Now()` always returns the timestamp of the last recorded event. Between durable events, time stands still — CPU-bound work like computation, string processing, or conditional branching takes zero virtual time.

**How it works with DurableSleep.** `h.DurableSleep(d)` is a durable event. Its recorded timestamp is the pre-sleep time *plus* the sleep duration, encoding the post-sleep virtual time. After `h.DurableSleep(5*time.Second)`, `h.Now()` returns the time after the 5-second sleep. The workflow is then suspended; when it resumes (via replay), the replay framework reads this timestamp from history, so `h.Now()` returns the same post-sleep time deterministically.

**How it works during replay.** Each event in the workflow's history carries its original timestamp. During replay, `h.Now()` returns the timestamp of the last replayed event — exactly the same values returned during the original execution. This is what makes replay deterministic even when workflow code branches on time.

**The replay frontier.** Replay consumes history events until none remain. At this point, the workflow transitions back to forward progress. `h.Now()` jumps to the current wall-clock time, and new events get fresh wall-clock timestamps. This "time skip" is correct: the workflow was suspended waiting for an external event (a timer, a signal, an activity result), and the new wall-clock time represents when that event actually arrived.

**Example.** Consider a workflow that:
1. Calls `h.Now()` → returns 1000 (the start time)
2. Calls `h.DurableCall("stripe", "charge", req)` → event timestamped at 1010; Now() returns 1010
3. Calls `h.DurableSleep(5000)` → event timestamped at 1010+5000=6010; Now() returns 6010; workflow suspends
4. [5 seconds pass in the real world]
5. Workflow resumes; replays the sleep event; Now() returns 6010
6. Calls `h.DurableCall("email", "send", req)` → event timestamped at ~6010 wall clock; Now() returns 6010

If this workflow is later replayed from the beginning (e.g., after a worker crash), all six steps produce identical `h.Now()` values: 1000, 1010, 6010, 6010, 6010, 6010.

#### 5.4.2 Prohibited packages and constructs

The following are PROHIBITED inside workflow functions (functions in the durable closure):

| Package / construct         | Reason                                          | Replacement                 |
|-----------------------------|-------------------------------------------------|-----------------------------|
| `net/http`                  | Unrecorded network call; breaks replay          | `h.DurableCall()`           |
| `database/sql`              | Direct database access; breaks replay           | `h.DurableCall()`           |
| `time.Now()`                | Non-deterministic; diverges on replay           | `h.Now()`                   |
| `time.Sleep()`              | Non-deterministic wall-clock delay              | `h.DurableSleep()`          |
| `math/rand`                 | Non-deterministic without deterministic seed    | `h.Random()`                |
| `reflect`                   | WASM support is limited; large binary overhead  | Avoid, or use outside       |
| Goroutines                  | Non-deterministic scheduling; breaks replay     | Child workflows (8.5)       |
| Channels                    | Non-deterministic ordering; breaks replay       | Signals / DurableAwait()    |
| Map iteration (order-sensitive) | Non-deterministic order in Go (see 5.5)    | Sorted slices (see 5.5)     |
| `os/exec`, `syscall`        | No OS-level access from WASM                    | `h.DurableCall()`           |
| Any package importing `net` or `os/exec` transitively | Breaks WASM compilation (see 8.12.3a) | Use only WASM-compatible libraries |

#### 5.4.3 The `HostCalls` threading requirement

Every function that directly or transitively calls a durable function MUST accept `*cleat.HostCalls` as its first parameter. This requirement propagates through the entire call graph:

```go
func validateAndReserve(h *cleat.HostCalls, userID string, items []Item) (Reservation, error) {
    // calls h.DurableCall(...) internally
    return lookupUser(h, userID, items)
}

func lookupUser(h *cleat.HostCalls, userID string, items []Item) (Reservation, error) {
    resp, err := h.DurableCall("users", "Lookup", userID)
    // ...
}
```

Even utility functions that do not directly call a host function but call another function that does are in the durable closure and need the parameter. Pure computational helpers (string formatting, arithmetic, data structure construction) that never transitively reach a host function do NOT need `*HostCalls` and pass through the transformer unchanged.

The transformer verifies this at build time (section 8.12.2, step 5). If a function in the durable closure lacks access to `*HostCalls`, the build fails with a clear error message pointing to the exact location.

#### 5.4.4 Impact assessment

Taken together, these restrictions mean that a developer cannot:

1. **Use most of Go's standard library.** The `net`, `os`, `syscall`, `database/sql`, `reflect`, and `time` packages are largely unavailable. Even indirect use — importing a third-party library that itself uses `net/http` — is blocked.
2. **Use goroutines or channels.** The core concurrency primitives of the language are off-limits inside workflow code. Parallelism must be expressed through child workflows, which carry serialization and durability overhead.
3. **Write reusable utility code without a `*HostCalls` parameter.** Any helper that might one day make a durable call already needs the parameter threaded through, which creates a visible API footprint throughout the codebase.

Developers evaluating this system should weigh these restrictions against the benefits of automatic durability, versioning, and observability. For workflows that consist primarily of sequenced API calls with error handling and compensation — the dominant pattern in business process automation — the restrictions are manageable. For workloads that rely on Go's standard library richness or goroutine-based concurrency, the restrictions may be prohibitive.

### 5.5 WASM determinism considerations

The system's correctness depends on deterministic replay: the same workflow code, given the same event history, must produce the same sequence of host calls. Because workflow code compiles to WASM and executes in a wazero runtime, any non-determinism introduced by the Go compiler, the WASM execution environment, or the Go runtime itself can cause replay divergence.

**Known non-determinism sources in Go → WASM:**

| Source | Description | Risk |
|--------|-------------|------|
| **Map iteration order** | Go intentionally randomizes map iteration order. If a ranged map contains a conditional that branches differently based on iteration order, replay produces a different host-call sequence. | High. Silent replay divergence with no compiler error. |
| **Wazero version differences** | Different wazero versions may produce different memory-layout or GC-timing behavior for the same WASM binary. | Low to medium. Mitigated by pinning wazero version per workflow definition. |
| **Floating-point behavior** | WASM specifies IEEE 754, but edge cases (NaN propagation, signed zero, FMA optimization) can differ between Go's compiler and the wazero interpreter or compiler mode. | Low in practice. Workflows should avoid floating-point for control-flow decisions. |

**Transformer detection strategy:**

1. **Map-iteration detection.** The transformer walks every `*ast.RangeStmt` in the durable closure. When the ranged expression has type `map[K]V`, the transformer checks whether the loop body contains any conditional branch (`if`, `switch`, `select`, short-circuit `&&`/`||`) whose condition references the loop key or value. If it does, the transformer emits a warning.

2. **Non-deterministic call detection.** Direct calls to `time.Now()`, `math/rand` functions (without explicit seed), and `runtime.Gosched()` are detected and rejected with a clear error.

3. **Goroutine and channel detection.** `go` statements and channel operations (`<-`, `<-chan`, `chan<-`) are detected and rejected.

**Go 1.24+ randomized map iteration.** Go 1.24 enables `GOEXPERIMENT=randatchash` by default, which adds a per-map-instance random hash seed so that iteration order varies not just across runs but across map instances within the same process. This design does **not** disable this randomization for WASM targets — workflow code runs with the same map-iteration randomization as any other Go code. The approach is to treat any map-iteration-dependent control flow as a determinism violation and require the developer to rewrite using sorted slices when order matters.

**Recommendations for deterministic workflows:**

1. **Use sorted slices, not maps, when iteration order affects control flow.**
2. **Avoid floating-point in control-flow conditions.** Use integer or fixed-point arithmetic for business logic that drives branching.
3. **Pin the wazero version** used to execute each workflow definition version. Store the wazero version alongside the WASM blob in `workflow_defs` so that replay always uses the same runtime.
4. **Test determinism explicitly.** The `durable test` framework (section 8.13) should run each test case twice: once fresh and once from a recorded history, comparing the host-call sequences for equality.

A `cleat vet` command statically detects potential non-determinism sources before compilation:

```
$ cleat vet ./workflows/
  Checking ./workflows/order.go...
    WARNING: order.go:42: map iteration with conditional on key 'id'
    WARNING: order.go:88: call to time.Now() -- use h.Now() instead
    ERROR:   order.go:120: goroutine in durable closure -- not allowed
  Found 2 warnings, 1 error
```

### 5.6 Data privacy and compliance

**The tension:** The system's core durability mechanism — recording full request and response JSON for every `DurableCall` in `event_history` — is fundamentally at odds with data privacy. Those event histories contain any PII that flowed through those calls: customer names, addresses, payment card PANs, email addresses, government IDs, and health information. The same property that makes event history valuable for observability makes it dangerous for privacy.

**Encryption at rest:**

1. **PostgreSQL column-level encryption via `pgcrypto`:** Sensitive columns can be encrypted with `pgp_sym_encrypt`/`pgp_sym_decrypt` using a key managed outside the database (AWS KMS, Vault transit engine). The worker decrypts on read.
2. **Full-database TDE:** Cloud-provider features (AWS RDS Encryption, Azure PostgreSQL TDE) encrypt the entire database at rest with no application changes.

Use TDE for defense-in-depth at the storage layer, plus application-level controls for access control and compliance.

**GDPR right of access (Article 15):** Finding all event history entries related to a given user requires a mapping from `user_id` to `workflow_id`. Workflow authors SHOULD include a `user_id` or `customer_id` field in the workflow input, and an index on `(input->>'user_id')` enables efficient lookups.

**GDPR right of erasure (Article 17):** Deleting personal data from an append-only event history is problematic. Three strategies:

- **Option A — In-place redaction:** Overwrite PII-containing fields in the event history with redacted values (`"card_number": "REDACTED"`) while preserving structural fields needed for replay (service, operation, step, timestamps, idempotency keys). Replay still works provided the workflow's branching logic does not depend on redacted field values.
- **Option B — Separate PII mapping table:** Store PII values in a separate table with opaque references in `event_history`. Adds significant operational complexity. Only worthwhile if regulations strictly require physical separation.
- **Option C — Retention-window compliance:** Accept immutability within the compliance window and rely on tiered storage expiry (Section 6.6). Simplest approach but only works if the compliance window aligns with a fixed retention period.

**Recommendation:** Implement Option A (in-place redaction) as the primary mechanism, with Option C as the fallback. Add a `pii_redaction_rules` column to `workflow_defs` that specifies JSON paths to redact per (service, operation) pair:

```json
{
    "payments": {
        "Charge": {
            "request": ["$.card_number", "$.cvv", "$.billing_address.street"],
            "response": ["$.charge_id.token"]
        }
    }
}
```

**Redacted data and replay correctness:** Workflow code must not branch on the content of `DurableCall` responses for fields that are subject to redaction. Responses used for routing decisions (status codes, boolean flags, entity IDs) should NOT be in redaction rules. PII values used purely for downstream API calls (e.g., an email address passed to a notification service) CAN be redacted because the workflow only passes them through.

**Service-level data classification:** Extend the service endpoint configuration with a `data_classification` per service. Services classified as `PCI` or `PII` that lack corresponding `pii_redaction_rules` should trigger a logged warning, alerting operators immediately rather than during a compliance audit.

### 5.7 Single database

PostgreSQL as the sole infrastructure is both the system's strength and its scaling limit. At very high throughput (hundreds of thousands of workflows/second), the database becomes a bottleneck. The staged migration path (add Redis for task queue, then swap to FoundationDB or CockroachDB for event history) addresses this, but each stage adds operational complexity.

### 5.8 Maturity

This is a design, not a production system. Temporal has a decade of production hardening. Every component described here — the WASM compilation pipeline, the wazero-based worker, the PostgreSQL schema and query patterns, the Patroni HA setup — needs to be built, tested, and hardened.

---

## 6. Performance and Scalability

### 6.1 Concurrency model

A worker runs N goroutines, each driving one workflow via wazero. The bottleneck is not CPU — workflows are I/O-bound, waiting on HTTP calls (10–500ms each) or human steps (hours to days).

Per-workflow memory consumption:

| Resource | Size |
|---|---|
| Goroutine stack | 4 KB (grows as needed) |
| WASM module memory (tinygo heap) | ~2 MB |
| Host overhead (buffers, event history, state) | ~1 MB |
| **Total per concurrent workflow** | **~3 MB** |

A worker with 8 GB of RAM (80% utilization) can handle approximately **2,100 concurrent workflows**. A worker with 16 GB handles approximately **4,300**. These are concurrently in-flight workflows — a workflow waiting three days for human approval consumes memory but near-zero CPU.

Workers are stateless and horizontally scalable. Adding more workers linearly increases concurrent workflow capacity.

### 6.2 Bottlenecks

The system has four potential bottlenecks, in order of when they become constraints:

**Bottleneck 1 — Event history INSERT throughput (the hard ceiling):**

Every `DurableCall` is an INSERT into `event_history`. On modest hardware, PostgreSQL handles ~20,000 INSERTs/second. On tuned high-end hardware, ~100,000 INSERTs/second. At an average of 8 steps per workflow, that yields:

| Hardware | Workflow completions/sec | Workflow completions/day |
|---|---|---|
| Modest (db.r6g.large, 2 vCPU) | 2,500 | 216 million |
| Tuned (db.r6g.8xlarge, 32 vCPU) | 12,500 | 1.08 billion |

This is the fundamental scaling ceiling. Everything else scales horizontally by adding more instances.

**Bottleneck 2 — Work queue claim throughput:**

A claim operation (SELECT ... FOR UPDATE SKIP LOCKED on the work queue) is required each time a workflow transitions from idle to active execution. Claims are not one per workflow — the count depends on lifecycle events:

| Source | Trigger | Multiplier |
|---|---|---|
| Initial claim | Workflow start | 1 per workflow |
| Sleep wake-up | DurableSleep timeout expires | 1 per sleep call |
| Signal delivery | Signal arrives while workflow is idle | 1 per signal |
| Retry reclaim | Retry attempt after failure | 1 per retry |

A workflow that calls DurableSleep 5 times requires 6 claims (start + 5 wake-ups). A workflow waiting on a signal is claimed when the signal arrives. A failed workflow is re-claimed for each retry attempt.

For a realistic mix averaging one sleep call, one signal delivery, and a 20% retry rate, expected claims per completed workflow is approximately 1 + 1 + 1 + 0.2 = 3.2, not 1.

Converting both constraints to workflow completions per second (at 8 steps per completion):

| Hardware | Completions/sec via INSERT throughput | Completions/sec via claim throughput |
|---|---|---|
| Modest (db.r6g.large) | 20,000 / 8 = 2,500 | 1,000 / 3.2 = 312 |
| Tuned (db.r6g.8xlarge) | 100,000 / 8 = 12,500 | 5,000 / 3.2 = 1,562 |

Claims constrain completions per second at roughly one-eighth the INSERT ceiling — a significantly narrower margin than the original one-to-one model implied. In practice, claim throughput is still not the first bottleneck reached for most configurations (worker memory constrains total concurrent workflows first, and the staged scaling plan introduces Redis-based queueing at Stage 4), but the margin is narrower and claim throughput matters at high scale.

**Bottleneck 2.5 — Heartbeat UPDATE throughput:**

The bottleneck analysis above models only INSERT throughput on event_history. It does not account for the heartbeat UPDATE workload on workflow_instances, which grows with cluster size.

Each active worker periodically records a heartbeat timestamp. Aggregate heartbeat write throughput:

```
heartbeats/sec = N_concurrent_workflows / heartbeat_interval_seconds
```

At 200,000 concurrent workflows with the default 5-second heartbeat interval: 200,000 / 5 = 40,000 UPDATEs/second. This alone can saturate a single PostgreSQL instance (roughly 20,000–100,000 writes/second for point updates). Unlike event_history INSERTs (append-only), each heartbeat UPDATE modifies an existing row. If heartbeat_at is indexed — necessary for efficient stale-worker detection queries — each UPDATE also modifies the B-tree index entry, increasing I/O cost per operation.

Mitigation options, from simplest to most impactful:

- **Batch heartbeats:** Instead of one UPDATE per workflow, the worker updates heartbeat_at for all workflows it owns in a single statement: `UPDATE workflow_instances SET heartbeat_at = now() WHERE worker_id = $1`. This reduces UPDATEs/sec from N_concurrent to N_workers (typically fewer than 100).
- **Increase the heartbeat interval:** Raising the interval from 5 seconds to 30 seconds reduces throughput by 6x. The tradeoff is slower dead-worker detection: up to 60 seconds (two missed intervals at 30s) versus 10 seconds (two missed at 5s) — acceptable for many workloads.
- **Skip the heartbeat_at index:** If stale-worker detection uses an out-of-band mechanism (Patroni session health checks or a worker-side lease), the UPDATE can be a HOT (Heap-Only Tuple) update that avoids index maintenance.

Without batching, heartbeat UPDATE throughput saturates PostgreSQL at roughly the same concurrent-workflow scale as INSERT throughput. With batching, the heartbeat load is negligible: a handful of UPDATEs per second regardless of workflow count.

**Bottleneck 3 — Worker memory:**

~3 MB per concurrent workflow. 100 workers with 8 GB each support approximately 200,000 concurrent workflows. Adding workers is a linear scale-out with no coordination overhead (workers are stateless).

**Bottleneck 4 — Event history reads (replay):**

Replay only happens on worker crash or restart. A workflow with 100 steps replays in ~1–10ms via an indexed B-tree scan on `(workflow_id, step)`. At a 1% crash rate with 10,000 concurrent workflows, ~100 replays/second generate negligible database load.

**Replication note — synchronous commit overhead:**

All throughput figures in this section assume asynchronous commit (local disk, no replication). Production deployments with Patroni using a synchronous standby — required for the "no lost commits" guarantee — incur additional latency:

- `synchronous_commit = on` causes each committed transaction to await fsync confirmation from the standby before returning to the client.
- Cross-AZ round-trip latency adds 1–3 ms per database operation.
- A single DurableCall involves 2–3 sequential database operations (heartbeat UPDATE + event_history INSERT + optionally a workflow_instances status UPDATE), adding 2–9 ms of synchronous replication latency to each API call.
- While aggregate write throughput is not directly reduced by synchronous replication (PostgreSQL pipelines concurrent WAL operations), the latency increase reduces the effective throughput ceiling for serialized operations and widens lock-contention windows.

As a rule of thumb, reduce the throughput ceilings in this section by 20–40% for cross-AZ synchronous replication deployments. For same-AZ synchronous replication (sub-millisecond latency), the impact is negligible.

### 6.3 Cost analysis

Approximate cloud pricing (AWS us-east-1, on-demand, mid-2026):

| Scenario | PostgreSQL | Workers | Storage | Total/month | Concurrent workflows |
|---|---|---|---|---|---|
| Development | $91 (db.r6g.large, 2 vCPU) | $31 (1 × t3.medium) | $1 (10 GB) | **$133** | ~100 |
| Small Production | $182 (db.r6g.large) | $182 (3 × t3.large, 8 GB) | $8 (100 GB) | **$382** | ~3,000 |
| Medium Production | $365 (db.r6g.xlarge, 4 vCPU) | $606 (10 × t3.large) | $40 (500 GB) | **$1,021** | ~10,000 |
| Large Production | $730 (db.r6g.2xlarge, 8 vCPU) | $3,635 (30 × t3.xlarge, 16 GB) | $160 (2 TB) | **$4,535** | ~60,000 |
| Very Large | $1,460 (db.r6g.4xlarge, 16 vCPU) | $12,118 (100 × t3.xlarge) | $800 (10 TB) | **$14,388** | ~200,000 |

Cost per workflow-hour amortized across the cluster:

| Scenario | $/concurrent-wf-hour | $/1,000 wf-completed★ |
|---|---|---|
| Development | $0.00167 | $0.167 |
| Small Production | $0.00017 | $0.017 |
| Medium Production | $0.00013 | $0.013 |
| Large Production | $0.00010 | $0.010 |
| Very Large | $0.00009 | $0.009 |

★ Assumes average workflow takes ~6 minutes wall-clock time. Longer-running workflows have lower per-completion costs since they spend most of their time idle.

> **HA cost note (self-managed):** The costs above are for single-AZ deployments with no high-availability standby. Production HA with Patroni (synchronous standby + etcd cluster) approximately doubles the PostgreSQL line items: the standby instance matches the primary's size, and the etcd cluster adds $30–60/month for three small instances. A medium production deployment (~$1,021/month in the table) becomes roughly $1,450/month with HA.
>
> **Managed database pricing:** With AWS RDS PostgreSQL (Multi-AZ, reserved instances, 1-year term), the PostgreSQL line items increase by approximately 30–50% over self-managed EC2, but eliminate the Patroni/etcd operational burden entirely. The HA standby, automated backups, PITR, and minor version upgrades are managed by AWS. For the medium production scenario: ~$1,131/month with RDS Multi-AZ vs. ~$1,450/month self-managed with Patroni. The managed option is both cheaper and simpler. Development/small-production scenarios benefit most: RDS Multi-AZ eliminates the fixed cost of operating etcd and configuring replication for setups that would otherwise be tempted to skip HA entirely.
>
> **Aurora Serverless v2:** For spiky or low-duty-cycle workloads (few workflows at night, bursts during business hours), Aurora Serverless v2 scales ACUs (Aurora Capacity Units) from 0.5 to 128 based on load. At idle (~0.5 ACU), the database costs ~$75/month instead of $220/month. This makes the system cost-competitive even for very small deployments where a full-time RDS instance would be overkill. The tradeoff is Aurora's proprietary storage layer — you trade PostgreSQL portability for cost efficiency and near-zero failover time (<1 second vs. 30–60 seconds for Patroni or standard RDS Multi-AZ).

### 6.4 Hard limits and mitigations

| Component | Hard limit | Mitigation |
|---|---|---|
| PostgreSQL writes | ~100K INSERTs/sec (single instance) | Partition `event_history` by hash; shard across multiple PG instances |
| PostgreSQL connections | ~500–1000 active before context-switch overhead | PgBouncer transaction pooling; 1,000 workers → ~50 PG connections |
| Worker memory | ~3 MB per concurrent workflow | Add more workers (stateless, horizontally scalable) |
| WASM module cache | ~200 KB per version; 10K versions = 2 GB (fits in worker RAM) | LRU eviction; rarely-used versions loaded from DB on demand |
| Network bandwidth | Workers → external APIs: typically <10 MB/sec at 1,000 calls/sec | Not a bottleneck for JSON API patterns |
| Patroni failover | ~30–60 seconds (etcd TTL + promote + reconnect) | Set heartbeat timeout to 90s (comfortably > 60s failover) |

### 6.5 Staged scaling plan

Each stage is incremental. The data model — `(workflow_id, step)` for event history, `workflow_id` for queuing — is partitionable from the start, so you can shard later without rewriting anything.

| Stage | Trigger | Changes | Capacity | Cost/mo |
|---|---|---|---|---|
| **1. Single PG** | Starting point | 1 PG (db.r6g.large), 3 workers, single event_history table, WASM in BYTEA, SKIP LOCKED queue | ~3K concurrent, ~500 steps/sec | ~$350 |
| **2. Vertical PG** | Event history >50 GB or >1K INSERTs/sec sustained | Upgrade PG (db.r6g.2xlarge), add read replica, 10 workers, WASM blobs to S3 | ~30K concurrent, ~5K steps/sec | ~$1,200 |
| **3. Partitioning** | Event history >500 GB or >5K INSERTs/sec sustained | `PARTITION BY HASH (workflow_id) PARTITIONS 16`, independent partition vacuuming, 30 workers | ~60K concurrent, ~10K steps/sec | ~$3,000 |
| **4. Split queue** | SKIP LOCKED claim latency >10ms p99 | Add Redis for task queue (XREADGROUP + XCLAIM), PostgreSQL remains state-of-record, write PG first then ACK Redis, 100 workers | ~200K concurrent, ~20K steps/sec | ~$8,000 |
| **5. Sharded state** | PostgreSQL write throughput saturated | Shard by workflow_id across multiple PG instances, or migrate to FoundationDB/CockroachDB, host-level connection routing | ~1M concurrent, ~100K steps/sec | ~$25,000+ |

### 6.6 Storage optimization: what goes to S3?

WASM blobs are small enough that S3 provides negligible savings:

| Item | Size per unit | 10K units | PG cost ($0.08/GB/mo) | S3 cost ($0.023/GB/mo) | Savings |
|---|---|---|---|---|---|
| WASM blob (tinygo) | ~200 KB | 2 GB | $0.16/mo | $0.05/mo | **$0.11/mo** |
| WASM blob (standard Go) | ~2 MB | 20 GB | $1.60/mo | $0.46/mo | **$1.14/mo** |

Even at 100,000 workflow versions, S3 saves ~$11/month. Not worth the operational complexity of a separate blob store. WASM blobs belong in PostgreSQL — they benefit from the same replication, backups, and PITR as the rest of the durable state.

Event history is the storage that grows unbounded. But it's structured, queryable data — not a blob. Moving it wholesale to S3 would destroy the SQL-based observability that makes the system valuable. The right approach is **tiered retention**:

| Tier | What | Where | Retention | Queryable? |
|---|---|---|---|---|
| Hot | In-flight workflow event history | PostgreSQL `event_history` | Until workflow completes | Full SQL |
| Warm | Recently completed history | PostgreSQL `event_history` | 30 days (configurable) | Full SQL |
| Cold | Older completed history | S3 (Parquet/JSON.gz) + pointer row in PG | Years (compliance) | Via Athena/Spark on S3 |

The migration to cold storage runs as a background process:

```
-- Move completed workflows older than 30 days to S3:
WITH archived AS (
  DELETE FROM event_history
  WHERE workflow_id IN (
    SELECT id FROM workflow_instances
    WHERE status IN ('done', 'failed')
      AND completed_at < now() - INTERVAL '30 days'
  )
  RETURNING *
)
SELECT aws_s3.query_export_to_s3(
  SELECT * FROM archived,
  's3://durable-workflows/archive/',
  format('history-%s.parquet', current_date)
);
```

A pointer row in a `workflow_archives` table records the S3 key for each archived workflow:

```sql
CREATE TABLE workflow_archives (
    workflow_id UUID PRIMARY KEY,
    archived_at TIMESTAMPTZ DEFAULT now(),
    s3_key TEXT NOT NULL,
    step_count INT NOT NULL
);
```

If a cold workflow needs to be replayed (rare — compliance audit, dispute resolution), the worker fetches the event history from S3, loads it into a temporary table, and replays. This is an infrequent operation with higher latency — acceptable for compliance use cases.

This tiered approach matters at scale. For 1 billion completed workflows at 8 steps (~8KB each), that's 8TB of event history, or $640/month in PostgreSQL storage. Moving everything older than 30 days to S3 cuts that to ~$100/month in S3 plus ~$50/month for recent history in PostgreSQL — a meaningful saving at that volume.

### 6.7 Cost comparison to Temporal

For a medium production deployment (~10,000 concurrent workflows), three infrastructure models:

| Resource | This system (self-managed PG) | This system (RDS Multi-AZ) | Temporal (self-hosted) | Temporal Cloud |
|---|---|---|---|---|
| Database | $365 (EC2 r6g.xlarge) | $475 (RDS r6g.xlarge, Multi-AZ) | Included in services | N/A (managed) |
| Workers | $606 (10 × t3.large) | $606 (10 × t3.large) | $606 (10 × t3.large) | N/A |
| Temporal services | N/A | N/A | $720 (Frontend + History + Matching) | N/A |
| Storage | $40 (100 GB) | $40 (100 GB) | $40 (100 GB) | N/A |
| Per-step pricing | N/A | N/A | N/A | $0.025/1K steps |
| **Total (fixed)** | **~$1,011/mo** | **~$1,121/mo** | **~$1,366/mo** | **$0 + usage** |

The self-managed system is cheaper than self-hosting Temporal because it eliminates the Frontend, History, and Matching services. But the gap narrows when comparing to RDS (the managed database adds ~$110/month) and vanishes entirely vs. Temporal Cloud below the crossover point.

**Crossover points vs. Temporal Cloud** (fixed infrastructure / per-step price):

| Infrastructure model | Monthly fixed cost | Crossover (steps/month) | Crossover (steps/sec avg) |
|---|---|---|---|
| Self-managed PostgreSQL | ~$1,011 | 40.4M | ~15 |
| RDS Multi-AZ | ~$1,121 | 44.8M | ~17 |
| Self-managed + Patroni HA | ~$1,450 | 58.0M | ~22 |
| Aurora Serverless v2 (avg 2 ACU) | ~$900 | 36.0M | ~14 |

Below the crossover, Temporal Cloud is cheaper (no idle infrastructure). Above it, fixed infrastructure wins. The crossover is highest for the HA self-managed case — the operational complexity of running Patroni also makes the cost argument weakest. The crossover is lowest for Aurora Serverless, which scales down during idle periods, making it the most cost-competitive option for variable workloads.

> **Engineering cost note:** The cost comparison excludes the engineering effort to build and maintain the self-managed system. The WASM transformer is estimated at 12 weeks (~$60,000 fully-loaded). For a team running 50M steps/month above the crossover, that investment is recouped in approximately 12 months of infrastructure savings vs. Temporal Cloud ($60,000 / $5,000 monthly savings). For a team below the crossover, it may never be recouped in pure cost terms.
>
> **Temporal Cloud pricing note:** Temporal Cloud's per-step pricing applies to workflow starts, signals, and queries — not just activity steps. A workflow with 8 activity steps, 2 signal receipts, and 1 start event would be billed for 11 "steps." The crossover calculations above use the blended $0.025/1K-steps rate and should be refined for specific workload patterns.

### 6.8 When is this system attractive? (audience segmentation)

The system's appeal varies substantially depending on how a team would host PostgreSQL anyway and what scale they operate at. This section is an honest assessment; Section 4 already compares feature-by-feature.

#### Segment A: Teams that self-manage PostgreSQL

**Strongest case.** These teams already operate PostgreSQL with Patroni, streaming replication, WAL archiving, and vacuum tuning. For them, the system truly is "just add workers." They get:

- **Operational simplicity that compounds.** They trade 4 Temporal services for 1 database they already know how to run. The Patroni setup they already maintain now also powers their durable execution. This is the cleanest version of the architecture's value proposition.
- **Cost crossover at ~40M steps/month.** Below that, Temporal Cloud is cheaper. Above it, self-managed infrastructure wins.
- **Full vendor portability.** Runs identically on AWS, GCP, Azure, and on-prem. No cloud lock-in for the database layer.

**Risk:** They're betting that the WASM transformer + worker runtime will be reliable enough to replace Temporal's decade of hardening. If the system has a subtle replay bug or a fencing token edge case, they're the ones debugging it at 3am.

#### Segment B: Teams that use managed PostgreSQL (RDS / Cloud SQL)

**Good case with a smaller operational advantage.** These teams don't want to operate PostgreSQL themselves. They get:

- **Operational simplicity that's real but narrower than Segment A.** They trade 4 Temporal services for 1 managed database. But the comparison is now "one managed thing vs. one managed thing" rather than "one thing to operate vs. four." The operational simplicity advantage shifts from "fewer services to run" to "fewer concepts to learn."
- **Cost crossover at ~45M steps/month (RDS Multi-AZ).** Slightly higher than self-managed, but still accessible.
- **Better HA story than self-managed.** RDS Multi-AZ failover takes ~30 seconds (vs. 30–60 for Patroni). Aurora failover takes <1 second. The fencing token design still matters for correctness, but the operational experience of failover is smoother.

**Risk:** The system's "single infrastructure" pitch is somewhat diluted. Temporal Cloud is also "one managed thing" — and its thing is purpose-built for durable execution (UI, debugging tools, SDKs in 6 languages). This system's thing is a general-purpose database. Teams must value the versioning model, programming model, and SQL-based observability enough to prefer a general-purpose database over a purpose-built platform.

#### Segment C: Teams that would use Temporal Cloud

**Weakest case.** These teams want zero infrastructure to operate. Temporal Cloud gives them the durability model, a mature UI, debugging tools, and SDKs in 6 languages — with nothing to manage. This system asks them to:

- **Operate worker infrastructure** (stateless, but still deployment, monitoring, scaling).
- **Build their own observability UI** (the event history is SQL-queryable, but operators don't write SQL).
- **Accept Go-only workflows** (vs. Temporal's 6-language SDK support).
- **Accept a pre-production codebase** (vs. Temporal's decade of hardening).

For this segment, the versioning model and programming model advantages are real but unlikely to outweigh Temporal Cloud's maturity and ecosystem. The system becomes attractive to Segment C only if:
- They're above the cost crossover AND have the engineering capacity to operate it.
- They deeply value the composable-function-call programming model over Temporal's workflow/activity split.
- They need the WASM sandbox security boundary (Temporal trusts workflow code completely).

#### Summary

| Segment | Operational simplicity win | Cost win | Primary risk |
|---|---|---|---|
| A (self-managed PG) | Large: 4 services → 1 DB they already run | Above ~40M steps/month | Betting on new code vs. Temporal's maturity |
| B (managed PG / RDS) | Moderate: fewer concepts, not fewer things to operate | Above ~45M steps/month | General-purpose DB vs. purpose-built platform |
| C (Temporal Cloud) | Negative: adds infra to operate | Requires scale + engineering capacity | Maturity gap too large for most teams |

The design is strongest for Segment A, viable for Segment B, and a hard sell for Segment C unless the programming model or versioning story is a decisive advantage for a specific use case.

---

## 7. Staged Adoption Path

### Phase 1: Prototype (weeks 1-4)

- Single PostgreSQL instance with the schema described above
- Worker that claims, replays, executes, and checkpoints workflows
- Manual WASM compilation (no transformer — hand-write the WASM bindings)
- One or two example workflows (order processing, approval flow)
- Goal: prove the core replay/checkpoint mechanism works

**What the Phase 1 demo does NOT demonstrate:**

The `wasm-demo/` directory contains proof-of-concept code that validates the core replay/checkpoint/recovery mechanism. It is intentionally a simulation — the infrastructure integration points are stubbed or simplified. Specifically, the demo does NOT demonstrate:

- **Actual WASM compilation or wazero runtime integration.** The workflow function is native Go, not compiled to WASM. The `DurableCall` boundary behaves identically in both modes, but the sandbox, import resolution, and module lifecycle are absent.
- **Multi-worker coordination with a real database.** The worker uses an in-memory `simulatedDB` struct, not PostgreSQL. There is no real `SELECT ... FOR UPDATE SKIP LOCKED`, no connection pooling, and no transaction conflict handling.
- **Signal delivery.** There is no host API endpoint for signals, no `pending_signals` table workflow, and no `DurableAwaitSignals` / `DurablePollSignal` host import.
- **DurableDefer execution loop.** The demo does not implement the defer registration, stack, or LIFO execution-on-exit protocol described in section 8.6b. Compensation is done via explicit error-handling branches in the workflow function.
- **Retry policies.** The demo host's simulated API calls always succeed (or return a hard-coded result). There is no HTTP timeout, no exponential backoff, no configurable `MaxAttempts` or `RetryableErrors`.
- **Patroni failover against a real cluster.** The failover simulation flips a boolean in an in-memory struct. There is no actual Patroni, no streaming replication, no WAL archiving, and no `pg_rewind`.
- **`SELECT ... FOR UPDATE SKIP LOCKED` against PostgreSQL.** The worker iterates an in-memory map instead of executing a real SQL queue query with row-level locking.
- **Heartbeat monitoring and stale-worker detection.** The simulated heartbeat returns true until a manual `simulateFailover` call, rather than using a real `UPDATE ... WHERE assigned_to = $worker` that returns zero rows when ownership is lost.

**What the demo DOES demonstrate correctly:** the core mechanism — deterministic replay from an event history, checkpoint-and-resume across a simulated crash, recovery of partial progress by a different worker, and full replay from a checkpoint without any external API calls. The Go-level invariants (step counter, cache-vs-live branching, divergence detection) are identical to what would run inside a wazero WASM module. The gaps listed above are all infrastructure integration — wiring the same logic to real PostgreSQL, Patroni, and a WASM runtime.

### Phase 2: Transformer (weeks 5-8)

- Go parser + type checker that identifies durable closures transitively
- Automatic generation of WASM bindings and exports
- Developer writes `h.DurableCall(...)` — transformer generates everything else
- Goal: developer experience is "write Go, run a command, get a WASM blob"

### Phase 3: Production hardening (weeks 9-12)

- Patroni HA for PostgreSQL (sync standby, automatic failover)
- WAL archiving to S3 for PITR
- Worker failover handling (pause on DB outage, resume after)
- Metrics, dashboards, alerting
- Replay-based debugging tool
- Goal: system is production-ready for internal workflows

### Phase 4: Scale (month 4+)

- Redis for task queue at high throughput
- PostgreSQL partitioning for large event history tables
- Cross-region DR via WAL shipping
- Multi-tenant isolation if needed
- Goal: system handles production scale for the organization

---

## 8. What's Missing

This section is an honest inventory of gaps — features that real-world durable execution systems need but that the current design does not yet address.

### 8.1 Signals and external events

**Design:** Signals are external events delivered to a running or waiting workflow. Think "the human clicked approve," "the payment webhook arrived," or "the shipping label was created." Signals are recorded in the event history so they replay deterministically. The pattern mirrors `DurableCall` — the signal is a host interaction that gets recorded and replayed.

**Host API for signal delivery:**

External clients send signals via a host API endpoint:

```
POST /api/v1/workflows/{workflow_id}/signals/{signal_name}
Body: {"approved_by": "manager@example.com", "approved_at": "2026-05-04T15:30:00Z"}
```

The host API handler:
1. Looks up the workflow instance. If not found or not in a signal-receivable state (`ready`, `running`), returns 404 or 409.
2. Records the signal in the event history as a `_durable.signal_received` event.
3. Determines the workflow's current state:
   - **If the workflow is queued** (`status = 'ready'` and `assigned_to IS NULL`): upserts a row into `pending_signals` and sets `next_wake_at = now()` to make the workflow immediately claimable.
   - **If the workflow is running on a worker** (`status = 'running'` and `assigned_to = 'worker-X'`): delivers the signal directly to the worker via HTTP (see below).
   - **If the workflow is completed/done/failed**: returns 409 (workflow no longer accepting signals).

**Live signal delivery (HTTP endpoint).** The host API and workers are separate processes, so signal delivery requires a network round-trip rather than an in-process channel. Workers expose a lightweight HTTP listener on a configurable port (e.g., `:9101`). This requires a `worker_address` column on `workflow_instances`:

```sql
ALTER TABLE workflow_instances ADD COLUMN worker_address TEXT;
```

The worker sets `worker_address` when claiming a workflow (alongside `assigned_to`):

```sql
UPDATE workflow_instances
SET assigned_to = $worker_id,
    worker_address = $listener_address,  -- e.g., "10.0.1.42:9101"
    heartbeat_at = now(),
    epoch = epoch + 1
WHERE id = $id
  AND (assigned_to IS NULL
       OR heartbeat_at < now() - interval '30 seconds');
```

Each worker's signal listener receives:

```
POST /v1/signals/{workflow_id}
Body: {"signal_name": "approved", "payload": {"approved_by": "..."}}
```

When the host API has a signal to deliver to a running workflow:
1. Reads `assigned_to` and `worker_address` from `workflow_instances`.
2. Sends an HTTP POST to `http://{worker_address}/v1/signals/{workflow_id}`.
3. If the worker responds with `200 OK`, the signal was delivered.
4. If the request fails (connection refused, timeout, or worker returns `404`), the worker may have crashed. The host API falls back to the queued path: it sets `next_wake_at = now()` so the workflow is immediately re-claimable, and the pending signal will be picked up by the next worker on claim.

**Why HTTP over LISTEN/NOTIFY.** PostgreSQL `LISTEN/NOTIFY` is lossy — if a worker is not actively listening when the `NOTIFY` is sent, the notification is silently dropped. Workers can miss notifications during connection flaps, Patroni failover, or when the listener goroutine is busy. HTTP delivery provides explicit acknowledgement (`200 OK` vs connection failure), works across any network topology, and does not tie signal delivery to PostgreSQL's notification mechanism.

**Race condition: worker releases workflow during signal delivery.** A window exists between the host API reading `assigned_to` + `worker_address` and the worker receiving the HTTP request. The system handles this at two points:

1. **Worker side.** Before releasing a workflow (on completion, failure, or release back to queue), the worker checks pending signals in `pending_signals` — if a signal arrived between the last heartbeat and the release, the worker re-processes it rather than dropping it.
2. **Host API side.** The host API pre-emptively sets `next_wake_at = now()` before attempting HTTP delivery, not after a failure. This ensures that if the delivery fails for any reason, the workflow is immediately claimable without waiting for the heartbeat timeout.

**Deduplication is enforced at the database level** regardless of delivery mechanism. The signal is always recorded in `event_history` first, and the `pending_signals` table has a `(workflow_id, signal_name)` primary key that rejects duplicates. Even if the HTTP notification fails, the signal is persisted and will be picked up on the next claim or replay.

**WASM-side API:**

```go
type HostCalls struct {
    DurableCall         func(service, operation, requestJSON string) (responseJSON string, err error)
    DurableSleep        func(durationMs int64)
    DurableAwaitSignals func(signalNames []string, timeoutMs int64) (signalName string, payloadJSON string, timedOut bool, err error)
    DurablePollSignal   func(signalNames []string) (signalName string, payloadJSON string, err error)
    DurableLog          func(message string)
    Now                 func() int64
}
```

`DurableAwaitSignals` blocks until one of the named signals arrives, or the timeout expires. It records a "waiting for signals" event in the history before yielding control. On replay, the host checks if a matching signal event exists later in the history and returns it immediately.

`DurablePollSignal` is non-blocking — it checks whether a signal already arrived (e.g., during an earlier step) and returns it, or returns empty if none is waiting. Useful for signals that might arrive during active processing rather than during an explicit wait.

**Usage example:**

```go
func ApprovalWorkflow(h *cleat.HostCalls, orderID string) (string, error) {
    // Step 0: Submit order for approval
    _, err := h.DurableCall("orders", "SubmitForApproval", fmt.Sprintf(`{"order_id":"%s"}`, orderID))
    if err != nil {
        return "", err
    }

    // Step 1: Wait for human approval (with 7-day timeout)
    signalName, payload, timedOut, err := h.DurableAwaitSignals(
        []string{"approved", "rejected"},
        7 * 24 * 3600 * 1000, // 7 days in ms
    )
    if err != nil {
        return "", fmt.Errorf("await failed: %w", err)
    }
    if timedOut {
        return "EXPIRED", nil
    }
    if signalName == "rejected" {
        return "REJECTED: " + payload, nil
    }

    // Step 2: Was approved — proceed with fulfillment
    _, err = h.DurableCall("orders", "Fulfill", fmt.Sprintf(`{"order_id":"%s","approval":%s}`, orderID, payload))
    if err != nil {
        return "", fmt.Errorf("fulfillment failed: %w", err)
    }

    return "APPROVED_AND_FULFILLED", nil
}
```

**Event history representation:**

Signal events use a special `service = '_durable'` prefix like timers:

```json
// Await started:
{"step": 1, "service": "_durable", "operation": "await_signals",
 "request": {"signal_names": ["approved", "rejected"], "timeout_ms": 604800000},
 "response": null}

// Signal received (inserted by the host API, not the worker):
{"step": 2, "service": "_durable", "operation": "signal_received",
 "request": {"signal_name": "approved", "payload": {"approved_by": "manager@example.com"}},
 "response": null}
```

**Replay behavior:**

1. Worker replays step 1 (the await). The host sees `operation = 'await_signals'`.
2. It scans forward in the event history looking for a matching `signal_received` event with a signal name in the awaited list.
3. If found (step 2), it returns the signal name and payload immediately — no actual waiting.
4. If not found (the signal hasn't arrived yet), it re-sets `next_wake_at` to `now() + remaining_timeout` and releases the workflow back to the queue. This is the "workflow was replayed before the signal arrived" case.
5. If the timeout expired without a signal, it returns `timedOut = true`.

**Pending signals table:**

```sql
CREATE TABLE pending_signals (
    workflow_id UUID NOT NULL,
    signal_name TEXT NOT NULL,
    payload JSONB,
    recorded_at TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (workflow_id, signal_name)
);
```

This table is a temporary holding area for signals that arrive while the workflow is queued. When a worker claims a workflow, it checks this table alongside loading the event history. If signals are found, they've also been recorded in the event history (the host API does both in a transaction), so the replay will encounter them. The worker cleans up pending signals after the workflow consumes them.

**Signal deduplication:** The same signal name can only be sent once (enforced by the `(workflow_id, signal_name)` primary key in `pending_signals` and the fact that the host API checks if a `signal_received` event already exists for that name before inserting). This prevents double-delivery during replay.

### 8.2 Queries (reading workflow state)

**Design:** The `DurableQuery` mechanism lets operators and other systems ask "what's the current business-level state of this workflow?" without modifying it. The event history has raw data, but reconstructing business-level state ("reservation confirmed, awaiting payment") requires replaying history through workflow code. Queries are read-only — they inspect the workflow's in-memory state and return a typed JSON response. They are NOT recorded in the event history and do not affect determinism.

**The concurrency problem:** The WASM module is actively running in a goroutine during execution. wazero does not support concurrent access to a module — you cannot call an export while the module is mid-execution in `DurableCall` or `DurableSleep`. Attempting to do so produces undefined behavior or a panic. This means the obvious approach (export a query handler from WASM, call it on demand) does not work while the workflow is executing.

**Revised design: cached state snapshots.** Queries are handled differently depending on the workflow's scheduling state:

- **If the workflow is running** (actively executing in a wazero goroutine on a worker): The host cannot call the WASM module concurrently. Instead, the query is answered from a **cached state snapshot** that the host updates at every durable boundary (after each `DurableCall` / `DurableSleep` / `DurableAwaitSignals` returns). The workflow author explicitly publishes queryable state using `h.SetQueryState(key, valueJSON)` at points where they want state to be externally visible:

  ```go
  h.SetQueryState("status", `"awaiting_approval"`)
  h.SetQueryState("reservation_id", `"resv_abc123"`)
  h.SetQueryState("charged_amount_cents", `"4200"`)
  h.SetQueryState("_location", `"order.go:127"`)
  ```

  `SetQueryState` merges the key-value pair into a JSON object (`query_state`) stored in the host's in-memory state and persisted to the `workflow_instances` row after each step. The host exposes this as a queryable snapshot via the API. `SetQueryState` is NOT recorded in the event history — it is derived state, not durable state. On replay, the same calls produce the same snapshot deterministically because the workflow's local variables are reconstructed from cached durable call responses.

- **If the workflow is queued** (sleeping, awaiting a signal, or waiting to be claimed): The host has no active WASM execution. It can safely load the WASM module and replay the event history in a fresh runtime. During replay, the workflow code calls `h.SetQueryState(...)` at the same points as during original execution. At the end of replay, the host collects the `query_state` snapshot and returns it. The WASM module is then unloaded. This is safe because no worker holds the workflow's fence.

- **If the workflow is completed / failed / cancelled:** The final `query_state` snapshot from the last durable boundary is preserved in the database and can be queried without loading WASM at all.

**Schema:**

```sql
ALTER TABLE workflow_instances ADD COLUMN query_state JSONB DEFAULT '{}'::jsonb;
-- Updated by the worker at each durable boundary alongside the event history INSERT
```

**Host API:**

```
GET /api/v1/workflows/{workflow_id}/query
Response: {"status": "awaiting_approval", "reservation_id": "resv_abc123", ...}

GET /api/v1/workflows/{workflow_id}/query?key=status
Response: "awaiting_approval"
```

The optional `key` parameter returns the value for a single key. If the key is absent from the snapshot, the endpoint returns `404`.

**Replay safety:** `SetQueryState` is not recorded in the event history, but it is deterministic. During replay, the same `DurableCall` responses produce the same local variables, which produce the same `SetQueryState` calls with the same values. The snapshot after replay is identical to the snapshot after the original execution at the same step. There is one edge case: a workflow that branched differently in a non-deterministic way (a bug) would diverge during replay, but this is a pre-existing replay failure that the fencing and dead letter mechanisms already handle.

**Why not exports?** The original design called for `//go:wasmexport` query handlers. This approach is incompatible with wazero's single-goroutine-per-module constraint. The snapshot approach avoids the concurrency problem entirely, works without WASM loaded for completed workflows, and has the side benefit of making query state visible in the SQL database to any tool that can run a `SELECT`.

**What gets queried:** The workflow author decides what is externally visible. This is a feature, not a limitation. Internal implementation details (retry counters, temporary variables, intermediate error states) can be kept private. The author explicitly publishes business-level state at points where it has semantic meaning.

**Implementation:**

1. Add `SetQueryState(key, value string)` to the `HostCalls` struct exposed to WASM.
2. The host-side implementation maintains a `map[string]string` in the runtime context, updated by each `SetQueryState` call.
3. After each durable boundary (after the event history INSERT for the step), the host serializes the map to JSON and writes it to `workflow_instances.query_state` in a separate UPDATE.
4. The host API `/query` endpoint reads `query_state` directly from the database. It does not need to communicate with a worker.

**Performance:** Writing `query_state` after every step adds a second database round-trip per durable boundary. If this becomes a bottleneck, the UPDATE can be batched: the host writes `query_state` only when the value changes, or on a periodic flush (every 5 steps, or every 500ms). For the initial implementation, write on every step — workflow code spends most of its time in HTTP calls, not in database writes.

**Source location annotations — Approach B (recommended):** The transformer emits `h.SetQueryState("_location", "order.go:127")` calls before each `DurableCall` site. This gives operators `file:line` granularity for execution position at the cost of a few extra WASM instructions. The `_location` key is updated at every durable boundary, so the query state always shows where the workflow last yielded.

```go
// Transformer output (conceptual):
h.SetQueryState("_location", "order.go:127")
resp1, err := h.DurableCall("inventory", "Reserve", inputJSON)
if err != nil { return "", err }
h.SetQueryState("reservation_id", resp1)
h.SetQueryState("_location", "order.go:131")

resp2, err := h.DurableCall("payments", "Charge", inputJSON2)
if err != nil { return "", err }
h.SetQueryState("charged_amount_cents", resp2)
```

**DWARF-based stack traces — future enhancement (Approach C):** Compile WASM with DWARF debug info and use wazero's DWARF support to map the instruction pointer to a source location. This requires wazero to expose the current instruction pointer during a host function call, which is not currently trivial (the WASM call stack is suspended). This is on the roadmap but not in the initial implementation.

**Comparison to Temporal's Queries:**

| Aspect | Temporal | This system |
|---|---|---|
| Handler definition | Export function with `@QueryMethod` | `h.SetQueryState(key, value)` at durable boundaries |
| Read-only guarantee | Enforced by SDK | Enforced by design (no query-side host access) |
| Concurrent access | Single-threaded per workflow | Avoided entirely via snapshot cache |
| Running workflow | Calls handler in same process | Reads DB snapshot written at last boundary |
| Queued workflow | N/A (always running) | Replays history in fresh WASM runtime |
| Completed workflow | N/A (not supported) | Returns final snapshot from DB |
| Typed responses | Via handler return type | Raw JSONB, typed at the application layer |

### 8.2b Operator UI and control surface

**The gap:** This is the biggest gap between this system and Temporal's operational experience. The system currently has SQL on `event_history`, a minimal HTTP API, and Prometheus metrics. This is powerful but raw. An operator debugging a stuck production workflow at 3am cannot be expected to write SQL.

**What Temporal provides that this system lacks:**

| Capability | Temporal | This system (current) | Gap severity |
|---|---|---|---|
| Workflow list/search UI | Web UI with query language | SQL on `custom_attributes` (no UI) | High |
| Workflow detail / event history viewer | Timeline visualization | Raw JSON from `event_history` | High |
| Current execution position | Stack trace, line number | None (WASM opaque) | High |
| Operational controls (cancel/signal/terminate) | UI buttons | Host API (curl only) | High |
| Workflow reset/replay | UI with event picker | Mechanism exists, no tooling | Medium |
| Query running workflows | Typed query handlers | `SetQueryState` snapshot (section 8.2) | High |
| Alerting on workflow state | Pre-built alerts | Prometheus metrics (wire yourself) | Medium |
| Workflow version dashboard | Version adoption view | SQL query | Low |
| Multi-tenant views | Namespace-scoped UI | No namespace concept yet | Medium |
| SDK observability (OTel) | Auto-instrumented | Event history IS tracing, but not OTLP | Medium |

**Design for the critical pieces:**

**1. Minimal web UI.** A single-page application (served by the host API, or a separate `durable-ui` binary) that provides:

- **Workflow list page:** Table with columns (workflow ID, type, version, status, started, last updated). Filter bar: status dropdown, workflow type text input, time range picker, custom attribute key-value pairs. Paginated (cursor-based). Backed by the `workflow_instances` table with GIN indexes on `custom_attributes`.

  ```
  GET /api/v1/workflows?status=failed&def=PlaceOrder&attr.customer_email=rob@example.com&page_size=50&cursor=eyJpZCI6ICJhYmMifQ==
  ```

  The response includes the cursor for the next page, total count estimate, and the result set.

- **Workflow detail page:**
  - Header card: workflow ID, definition name, version, status, created at, last updated at, duration so far, parent workflow ID (if a child), child workflow IDs (if any).
  - Query state card: the current `query_state` JSONB rendered as a key-value table, refreshed on page load. If the workflow is running, a "Refresh" button re-fetches `/query` to get the latest snapshot.
  - Event timeline: chronological list of every event, rendered as a vertical timeline. Each event shows: step number, service, operation, request (PII-collapsed by default), response (PII-collapsed), duration (bar chart), retry attempt, error (highlighted in red). Collapsible JSON viewer for request/response bodies.
  - Actions bar: buttons for Cancel, Terminate, Signal (with a text input for signal name + JSON payload), Reset (with an event step picker), Pause, Resume. Each button triggers a confirmation dialog with the relevant endpoint and payload previewed.

- **Schedule list page:** Table of CRON schedules (name, workflow def, cron expression, next fire time, last fire time, enabled/disabled toggle). Inline enable/disable switch that calls `PATCH /api/v1/schedules/{id}`.

- **Dead letter queue page:** Workflows in `dead_letter` status, grouped by failure reason, with counts. Each group expandable to show individual workflow IDs with the last error, step, and a "Retry" button that re-queues the workflow.

  ```
  POST /api/v1/workflows/{id}/retry
  ```

  This resets the workflow's retry count for the current step and sets `status = 'ready'`, `next_wake_at = now()`.

**Frontend tech:** Any modern framework (React, Vue, Svelte) in a single binary compiled with a Go web server that embeds the built frontend assets. The UI communicates exclusively with the host API — no direct database access. Estimated 2-3 weeks for a functional first version by one frontend developer working against the existing host API.

**2. Operational controls API.** Extend the host API beyond signals and cancellation to provide a full set of lifecycle management endpoints:

```
POST   /api/v1/workflows/{id}/cancel          # existing (section 8.6) — graceful, runs defers
POST   /api/v1/workflows/{id}/terminate       # NEW — force-kill, no defers
POST   /api/v1/workflows/{id}/signal/{name}   # existing (section 8.1)
POST   /api/v1/workflows/{id}/reset           # NEW — truncate history at step N, restart
GET    /api/v1/workflows/{id}/query           # NEW — get query_state snapshot (section 8.2)
GET    /api/v1/workflows/{id}/history          # NEW — paginated event history
POST   /api/v1/workflows/{id}/pause           # NEW — set status='paused', workers skip
POST   /api/v1/workflows/{id}/resume          # NEW — set status='ready', set next_wake_at=now()
PATCH  /api/v1/schedules/{id}                 # NEW — enable/disable a CRON schedule
POST   /api/v1/workflows/{id}/retry           # NEW — re-queue a dead-lettered workflow
```

**Terminate vs. Cancel:**

| Aspect | Cancel | Terminate |
|---|---|---|
| Runs defers? | Yes | No |
| Grace period | 30s (configurable) | None (immediate) |
| Event recorded | `cancellation_completed` | `terminated` |
| WASM runtime | Returns `ErrCancelled` from current call | `runtime.Close()` immediately |
| Status set | `cancelled` | `terminated` |
| Use case | Graceful shutdown with compensation | Stuck workflow (infinite loop, hung WASM) |

Terminate is a last resort. It skips `DurableDefer` cleanup, which may leave external resources (reservations, temporary data) dangling. Operators should use Cancel first and only Terminate when Cancel's grace period expires without the workflow responding.

**Terminate API details:**

```
POST /api/v1/workflows/{id}/terminate
Body: {"reason": "infinite loop — worker CPU at 100% for 5 minutes", "requested_by": "oncall-rotation"}

Response: 200 OK
{ "status": "terminated", "terminated_at": "2026-05-04T03:15:00Z" }
```

The host API handler:
1. Looks up the workflow instance. Returns 409 if already in a terminal state (`done`, `failed`, `cancelled`, `terminated`).
2. If the workflow is actively running on a worker (`assigned_to IS NOT NULL`), sends an HTTP DELETE to the worker's signal listener:
   ```
   DELETE /v1/workflows/{workflow_id}
   ```
   The worker kills the WASM runtime immediately — no `DurableDefer` execution, no response propagation.
3. If the workflow is queued, marks it `status = 'terminated'` directly — no WASM runtime to kill.
4. Records a `terminated` event in the event history:

   ```json
   {"step": 6, "service": "_durable", "operation": "terminated",
    "request": {"reason": "infinite loop — worker CPU at 100% for 5 minutes"},
    "response": null}
   ```

5. Sets `status = 'terminated'` and clears `assigned_to`.

**Reset API details:**

```
POST /api/v1/workflows/{id}/reset
Body: {"truncate_at_step": 4, "reason": "bug in step 5 payment handler, fixed in v3"}

Response: 200 OK
{ "new_workflow_id": "original-uuid", "truncated_history": true, "reset_at_step": 4 }
```

The host API handler:
1. Validates that `truncate_at_step` is within the event history bounds and that the workflow is in a terminal-ish state (`failed`, `cancelled`, `dead_letter`, or `done`). For `done` workflows, reset creates a fresh execution of the same workflow — like restarting a completed process.
2. Truncates `event_history` to entries with `step < truncate_at_step`. The old history (steps >= truncate_at_step) is archived to `event_history_cold` (section 8.16) with a `reset_at_step` marker.
3. Resets `status = 'ready'`, `next_wake_at = now()`, clears `assigned_to`, clears `error_message`, clears `dead_letter_count`, increments `reset_count`.
4. The next worker to claim the workflow replays steps 0..truncate_at_step-1 from cache and executes step `truncate_at_step` forward with fresh API calls.

The reset does NOT change the workflow's `def_version`. If the intent is to try a newer version, the operator must also deploy a new definition and update the workflow instance. This is a separate operation.

**Reset vs. retry:**

| Aspect | Reset | Retry |
|---|---|---|
| What changes | Truncates history at a chosen step | Re-queues at the failed step |
| Replays from | Beginning of truncated history | Point of failure |
| Fresh calls from | `truncate_at_step` forward | Same step, same request |
| Use case | Bug fix, re-run from a safe point | Transient failure, retry same operation |

**3. Execution position visibility.** WASM is opaque — the host cannot inspect the WASM call stack during workflow execution. Three approaches, in increasing order of effort:

- **Approach A — Step counter (trivial, already exists).** Show "workflow is on step 14 of ~20" from `MAX(step) FROM event_history WHERE workflow_id = $1`. Provides a progress indicator but not a code location. Default fallback if no other approach is enabled.

- **Approach B — Source annotations (moderate, recommended).** Described in section 8.2 above. The transformer injects `h.SetQueryState("_location", "order.go:127")` calls before each `DurableCall` site. The `_location` key is updated at every durable boundary, so the detail page can display "Last known position: order.go:127" in the header card. This covers 90% of debugging scenarios ("it's stuck waiting for a payment response at order.go:127") with negligible implementation cost.

- **Approach C — DWARF debug info (hard, future).** Compile WASM with `-dwarfdump` or equivalent, and use wazero's DWARF support to map the instruction pointer to a source location. This requires the wazero runtime to expose the current instruction pointer during a host function call, which is not straightforward — the WASM call stack is suspended when the host function is invoked. This is a research project, not a design. Estimated 2-3 weeks if viable.

**UI integration:** The workflow detail page displays the execution position as a single line: "Step 14 of ~22 (last known source: `order.go:127`)". If source annotations are enabled (Approach B), the line also shows the file path. If only the step counter is available (Approach A), the line reads "Step 14 of ~22" with a tooltip explaining that source annotations were not compiled into this workflow definition.

**4. Alerting and dashboards.** The system ships with configuration files (not code) that expose the existing Prometheus metrics in a consumable form:

- **Grafana dashboard (`deploy/grafana/durable-dashboard.json`):** A pre-built dashboard JSON that visualizes:
  - Workflow completion rate (status changes per minute, stacked by status)
  - Per-type latency percentiles (p50, p90, p99 from `event_history.duration_ms`)
  - Dead letter queue size (count of `dead_letter` workflows)
  - Queue depth (`SELECT count(*) FROM workflow_instances WHERE status = 'ready'`)
  - Active workers (`SELECT count(DISTINCT assigned_to) FROM workflow_instances WHERE status = 'running'`)
  - Heartbeat latency (worker-to-database round-trip time)
  - Event history INSERT rate (rows/second into `event_history`)
  - WASM load time (time to compile and instantiate a WASM module)
  - Per-service call volume and error rate (from `event_history.service`)

- **Alert rules (`deploy/prometheus/durable-alerts.yml`):** Pre-built Prometheus Alertmanager rules:
  ```yaml
  groups:
    - name: durable-execution
      rules:
        - alert: HighWorkflowFailureRate
          expr: rate(workflow_completions{status="failed"}[5m]) / rate(workflow_completions[5m]) > 0.1
          for: 5m
          labels: { severity: critical }
          annotations: { summary: "Workflow failure rate > 10% over 5m" }
  
        - alert: DeadLetterQueueGrowing
          expr: durable_dead_letter_count > 100
          for: 2m
          labels: { severity: warning }
          annotations: { summary: "Dead letter queue has {{ $value }} workflows" }
  
        - alert: StuckWorkflow
          expr: time() - durable_last_progress_seconds{status="running"} > 600
          for: 1m
          labels: { severity: warning }
          annotations: { summary: "Workflow {{ $labels.workflow_id }} has no progress in 10m" }
  
        - alert: NoActiveWorkers
          expr: durable_active_workers == 0
          for: 1m
          labels: { severity: critical }
          annotations: { summary: "No workers are claiming workflows" }
  
        - alert: DatabaseConnectionFailures
          expr: rate(durable_db_errors[5m]) > 0
          for: 2m
          labels: { severity: critical }
          annotations: { summary: "Database connection errors detected" }
  ```

- **Health endpoint:**

  ```
  GET /health
  
  Response:
  {
    "status": "ok",
    "database": { "connected": true, "latency_ms": 2, "pool_size": 10, "pool_used": 3 },
    "worker_pool": { "running": 12, "idle": 3, "max": 20 },
    "scheduler_leader": { "is_leader": true, "last_election": "2026-05-04T00:00:00Z" }
  }
  ```

  The `/health` endpoint is a standard HTTP endpoint exposed by the host API. It checks:
  - Database connectivity: runs `SELECT 1` against PostgreSQL, measures latency.
  - Worker pool: returns the number of running and idle workers (known via heartbeat).
  - Scheduler leadership: returns whether this node is the scheduler leader and when the last election occurred.

**5. OpenTelemetry bridge.** The event history IS distributed tracing — each workflow is a trace (`workflow_id`), each `DurableCall` is a span (`step`). Bridge to OTLP so existing observability tools (Jaeger, Honeycomb, Datadog) can consume it without writing SQL:

**Mapping:**

| Event history field | OTLP span field |
|---|---|
| `workflow_id` | `trace_id` (as hex) |
| `step` | `span_id`: SHA256(`workflow_id || ':' || step`)[:16] |
| `step - 1` | `parent_span_id`: SHA256(`workflow_id || ':' || (step - 1)`)[:16] |
| `service || '.' || operation` | `name` (e.g., "payments.Charge") |
| `service`, `operation`, `step`, `duration_ms`, `worker_id`, `def_name`, `def_version` | `attributes` |
| `error IS NOT NULL` | `status = ERROR`, `status.message = error` |
| `request`, `response` | `attributes` (PII-sensitive — opt-in, off by default) |

**Implementation:** A background goroutine in the worker process runs a periodic query:

```sql
SELECT * FROM event_history
WHERE exported_to_otlp = false
ORDER BY workflow_id, step
LIMIT 1000;
```

It batches events into OTLP span exports using the OpenTelemetry Go SDK (`go.opentelemetry.io/otel`), sends them to a configurable OTLP endpoint (`OTEL_EXPORTER_OTLP_ENDPOINT`), and marks them as exported with:

```sql
UPDATE event_history SET exported_to_otlp = true
WHERE (workflow_id, step) IN (...);
```

Add a boolean column:

```sql
ALTER TABLE event_history ADD COLUMN exported_to_otlp BOOLEAN DEFAULT false;
CREATE INDEX idx_event_history_otlp ON event_history (exported_to_otlp) WHERE exported_to_otlp = false;
```

The bridge is optional. The event history is the source of truth either way. But for teams that already have Jaeger or Honeycomb dashboards for their microservices, being able to query "show me all spans for workflow abc-123" in the same tool without writing SQL is a significant operational win.

**PII and data privacy:** The OTLP bridge defaults to excluding `request` and `response` bodies from span attributes. Operators can opt in by setting `DURABLE_OTLP_INCLUDE_PAYLOADS=true`, with the understanding that PII (customer emails, addresses, payment amounts) may be present in the payloads. The `service`, `operation`, `duration_ms`, and `error` fields are always exported because they contain no user data.

**Implementation prioritization:**

| Phase | What | Effort | Why |
|---|---|---|---|
| **P0** | Query mechanism (section 8.2 — `SetQueryState` + host API) | 1 week | Operators are blind without it |
| **P0** | Operational controls API (terminate, reset, pause, resume, retry) | 1 week | Can't manage production workflows with curl alone |
| **P0** | Minimal web UI (list + detail + actions) | 2-3 weeks | SQL is not an operator interface |
| **P1** | Source annotations for execution position | 0.5 weeks | Transformer already walks AST; adding a line number is trivial |
| **P1** | Grafana dashboard + alert rules | 0.5 weeks | Makes metrics usable without custom dashboard build |
| **P1** | OTLP bridge | 1 week | Leverages existing observability investments |
| **P2** | Health endpoint | 0.5 weeks | Trivial to implement, useful for load balancers |
| **P2** | DWARF-based stack traces | 2-3 weeks | Nice to have; Approach B covers 90% of needs |
| **P2** | Workflow reset UI | 1 week | Mechanism exists; UI is the gap |
| **P2** | Schedule list + management UI | 1 week | Schedules work via API; UI makes them discoverable |

**Total P0+P1 effort: ~7 weeks.** This is the minimum to make the system operationally viable for a team that is not the original authors.

**Honest assessment:** This operational surface is where Temporal's decade of investment shows most. Temporal's UI, visibility store, and debugging tools are the product of years of iteration with real operators. This system would start with a basic but functional UI and improve incrementally.

The durability mechanism (replay, fencing, event history) is the hard technical problem. The operational surface is the hard product problem. Both need to be solved for the system to be viable for production use by a team that doesn't include the system's creators.

### 8.3 Timers and durable sleep

**Design:** The host provides two timer-related functions to the WASM module: `DurableSleep(duration)` and `DurableTimer(deadline)`. Both follow the same pattern — record the timer in the event history, yield control back to the host, release the workflow to the queue with `next_wake_at` set, and on replay return immediately.

```go
// In the cleat runtime (imported by workflow code):
type HostCalls struct {
    DurableCall  func(service, operation, requestJSON string) (responseJSON string, err error)
    DurableSleep func(durationMs int64)  // milliseconds
    DurableLog   func(message string)
    Now          func() int64
}
```

**Execution flow for `DurableSleep(3 * time.Hour)`:**
1. The WASM module calls `h.DurableSleep(10800000)`. This is a host import — the WASM module blocks, and the host takes over.
2. The host records a timer event in the event history:

   ```json
   {"step": 5, "type": "timer", "event": "sleep", "duration_ms": 10800000, "wake_at": "2026-05-07T12:00:00Z"}
   ```

3. The host sets `next_wake_at = now() + 3 hours` on the workflow instance, sets `status = 'ready'`, and releases it back to the queue. The goroutine for this workflow exits.
4. At the wake time, the `idx_runnable` index makes the workflow visible to claim. A worker claims it.
5. The worker replays the event history. When it reaches step 5 (the timer event), the `DurableSleep` host function sees that this timer was already recorded and **returns immediately without sleeping**.
6. Execution continues to step 6.

**On replay:** The `DurableSleep` implementation checks whether the current step in the event history is a timer event. If so, and the timer's `wake_at` is in the past (it fired, the workflow was claimed), it returns immediately. If the timer hasn't fired yet (this shouldn't happen — the workflow wouldn't have been claimed), it re-sets `next_wake_at` and releases again.

**Event history schema extension:** Timer events use a special record type in the `event_history` table, distinguished by `service = '_durable'` and `operation = 'sleep'` (or `'await'`):

```sql
-- A timer event in event_history:
INSERT INTO event_history (workflow_id, step, service, operation, request, response)
VALUES ($1, $2, '_durable', 'sleep',
    '{"duration_ms": 10800000}',
    '{"wake_at": "2026-05-07T12:00:00Z"}');
```

**DurableAwait for signals:** When combined with signals (8.1), a workflow can sleep until either a signal arrives or a timeout:

```go
result, err := h.DurableAwait([]string{"approval_signal", "rejection_signal"}, 86400000)
// Blocks until "approval_signal" or "rejection_signal" arrives, or 24 hours pass.
// Returns the signal name and optional payload.
```

### 8.4 Retry policies per durable call

**Design:** Retry is configured through a `RetryPolicy` attached to each `DurableCall`. The host handles all retry logic — the WASM module makes one call and gets either a success response or a terminal error after retries are exhausted. Failed attempts are NOT recorded as separate steps in the event history; only the final outcome (success after N retries, or terminal failure) is recorded.

**API design:**

```go
type RetryPolicy struct {
    MaxAttempts     int           // 0 = no retry (default), -1 = infinite (until DLQ limit)
    InitialBackoff  time.Duration // default: 1 second
    MaxBackoff      time.Duration // default: 60 seconds
    BackoffFactor   float64       // default: 2.0 (exponential)
    RetryableErrors []string      // substrings to match in error messages; empty = all errors retryable
    Timeout         time.Duration // per-attempt HTTP timeout
}

// Usage: per-call overrides
result, err := h.DurableCall("payments", "Charge", req,
    &durable.RetryPolicy{
        MaxAttempts:    5,
        InitialBackoff: 1 * time.Second,
        MaxBackoff:     30 * time.Second,
        RetryableErrors: []string{"timeout", "rate_limited", "5xx"},
        Timeout:        10 * time.Second,
    })

// Or: service-level defaults configured in the worker
```

**Retry defaults configuration** (per worker, via JSON config file or env vars):

```json
{
  "retry_defaults": {
    "payments.Charge": {
      "max_attempts": 5,
      "initial_backoff_ms": 1000,
      "max_backoff_ms": 30000,
      "backoff_factor": 2.0,
      "retryable_errors": ["timeout", "rate_limited", "5xx"],
      "timeout_ms": 10000
    },
    "notifications.*": {
      "max_attempts": 2,
      "initial_backoff_ms": 500,
      "backoff_factor": 1.0
    },
    "*": {
      "max_attempts": 3,
      "initial_backoff_ms": 1000,
      "max_backoff_ms": 60000
    }
  }
}
```

**Host retry flow:**
1. WASM calls `DurableCall("payments", "Charge", req, retryPolicy)`.
2. Host makes HTTP call with the configured timeout.
3. On success → records result in event history, returns to WASM.
4. On retryable error and attempts remain → backs off, retries. Nothing recorded yet.
5. On terminal failure (non-retryable error or max attempts exhausted) → records the error in event history, returns to WASM. The workflow can decide to compensate or fail.
6. On replay → the recorded result (success or final error) is returned. No retries happen during replay.

**Why retries aren't separate event history entries:** If each retry was a step, a call that succeeded on attempt 5 would replay as 1 real call + 4 skipped calls = 5 event history entries, but only 1 step of workflow progress. This bloats history and complicates the step counter. Recording only the final outcome keeps the event history clean.

### 8.5 Child workflows

**Design:** Child workflows are separately managed workflow instances started from a parent workflow. Each child gets its own row in `workflow_instances`, its own event history, and its own lifecycle (timeout, retry, cancellation). The parent can start a child and optionally wait for its result, or fire-and-forget. The parent-child relationship creates a tree of workflow instances rooted at the original top-level workflow.

**API design:**

Two new host functions extend `HostCalls`:

```go
type ChildWorkflowOptions struct {
    Timeout                  time.Duration // max wall-clock time for the child (default: none)
    RetryPolicy              *RetryPolicy  // per-child retry policy (default: inherit parent defaults)
    FireAndForget            bool          // if true, parent does not wait for result (default: false)
    CancelChildrenOnParentCancel *bool     // nil = true (default: cancel children when parent is cancelled)
}

type HostCalls struct {
    // ... existing functions ...
    DurableChildWorkflow func(defName string, inputJSON string, opts *ChildWorkflowOptions) (runID string, err error)
    DurableAwaitChild    func(runID string) (outputJSON string, err error)
}
```

`DurableChildWorkflow` starts a new workflow instance and returns its `runID`. If `FireAndForget` is `false` (default), the parent **must** call `DurableAwaitChild(runID)` to get the result; the parent blocks until the child completes. If `FireAndForget` is `true`, the parent calls `DurableChildWorkflow` and never waits -- the child runs independently and the parent cannot receive its result.

**Usage example -- parallel fraud checks:**

```go
func OrderWorkflow(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    // Step 0: Reserve inventory
    reservation, err := reserveInventory(h, userID, cart)
    if err != nil {
        return "", err
    }

    // Step 1: Start parallel fraud checks for each item
    var childRunIDs []string
    for _, item := range cart {
        input, _ := json.Marshal(item)
        runID, err := h.DurableChildWorkflow("FraudCheck", string(input), &durable.ChildWorkflowOptions{
            Timeout: 30 * time.Second,
            RetryPolicy: &durable.RetryPolicy{
                MaxAttempts:    3,
                InitialBackoff: 1 * time.Second,
            },
        })
        if err != nil {
            return "", fmt.Errorf("failed to start fraud check for item %s: %w", item.ID, err)
        }
        childRunIDs = append(childRunIDs, runID)
    }

    // Step 2: Await all children
    for _, runID := range childRunIDs {
        result, err := h.DurableAwaitChild(runID)
        if err != nil {
            return "", fmt.Errorf("fraud check %s failed: %w", runID, err)
        }
        // process result...
        _ = result
    }

    // Step 3: Continue with order processing
    return fulfillOrder(h, reservation)
}
```

**Usage example -- fire-and-forget audit log:**

```go
func OrderWorkflow(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    // ... order processing ...

    // Fire-and-forget: audit log runs independently, never waited on
    _, _ = h.DurableChildWorkflow("AuditLog", auditPayload, &durable.ChildWorkflowOptions{
        FireAndForget: true,
        CancelChildrenOnParentCancel: boolPtr(false), // audit runs even if parent is cancelled
    })

    return trackingID, nil
}
```

**Schema additions:**

A `parent_workflow_id` column on `workflow_instances` creates the parent-child tree:

```sql
ALTER TABLE workflow_instances ADD COLUMN parent_workflow_id UUID REFERENCES workflow_instances(id);
CREATE INDEX idx_workflow_instances_parent ON workflow_instances (parent_workflow_id);
```

The root workflow has `parent_workflow_id IS NULL`. All children at any depth carry a reference to their immediate parent. The tree can be queried at any time:

```sql
-- Find all children of a given workflow
SELECT id, def_name, status, created_at
FROM workflow_instances
WHERE parent_workflow_id = $parent_id;

-- Find the root of any workflow's tree (useful for tenant-scoped queries)
WITH RECURSIVE ancestors AS (
    SELECT id, parent_workflow_id, 1 AS depth
    FROM workflow_instances WHERE id = $workflow_id
    UNION ALL
    SELECT wi.id, wi.parent_workflow_id, a.depth + 1
    FROM workflow_instances wi
    JOIN ancestors a ON wi.id = a.parent_workflow_id
)
SELECT * FROM ancestors ORDER BY depth DESC LIMIT 1;
```

**Execution flow -- `DurableChildWorkflow` (FireAndForget=false):**

1. The WASM module calls `h.DurableChildWorkflow("FraudCheck", input, opts)`.
2. The host records a `child_started` event in the parent's event history:

   ```json
   {"step": 4, "service": "_durable", "operation": "child_started",
    "request": {"def_name": "FraudCheck", "input": {"item_id": "SKU-123", "amount_cents": 4999},
                "child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
                "timeout_ms": 30000, "fire_and_forget": false},
    "response": null}
   ```

3. The host INSERTs a new row in `workflow_instances` for the child:

   ```sql
   INSERT INTO workflow_instances (id, def_name, def_version, status, input, parent_workflow_id, next_wake_at)
   VALUES ($child_run_id, 'FraudCheck', $latest_version, 'ready', $input_json, $parent_id, now());
   ```

   The child is immediately claimable (`next_wake_at = now()`). The `def_version` is resolved at **compile time**: when the parent workflow is built, `cleat build` detects all `h.ChildWorkflow("name", ...)` calls, resolves each child name to its latest non-deprecated version in the database (or reads pinned versions from `cleat.lock`), and embeds the resolved version map in the WASM binary's metadata. At runtime, the host reads the pinned versions from WASM metadata and uses them when spawning children. This means a parent always spawns the exact child version it was compiled against — a key property of cleat's WASM-based versioning. The pinned version can be overridden at runtime via `ChildWorkflowOptions.Version`.

4. The host returns the `child_run_id` to the parent WASM module. Step 1 (starting the child) is complete. The parent now has the child's run ID in its local variable (e.g., `runID`).

5. The parent WASM module calls `h.DurableAwaitChild(runID)`. The host records a `child_awaiting` event in the parent's event history:

   ```json
   {"step": 5, "service": "_durable", "operation": "child_awaiting",
    "request": {"child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
    "response": null}
   ```

6. The host sets `next_wake_at = NULL` on the parent (parent sleeps until the child completes), sets `status = 'ready'`, and releases the parent back to the queue. The parent's goroutine exits.

7. Meanwhile, the child is claimed by any available worker, executes its workflow function, and reaches a terminal state (`done`, `failed`, or `cancelled`).

8. **Child completion handler.** When a child workflow reaches a terminal state, the worker that completed the child runs a post-completion check. It queries:

   ```sql
   SELECT parent_workflow_id, status
   FROM workflow_instances
   WHERE id = $child_id;
   ```

   If `parent_workflow_id` IS NOT NULL, the worker checks the parent's state. If the parent is in `ready` status and the most recent event in its history is `child_awaiting` for this child, the worker records the child's result in the parent's event history:

   ```json
   {"step": 6, "service": "_durable", "operation": "child_completed",
    "request": {"child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
    "response": {"output": {"status": "approved", "score": 42}, "duration_ms": 1234}}
   ```

   Then the worker sets `next_wake_at = now()` on the parent, making it immediately claimable:

   ```sql
   UPDATE workflow_instances
   SET next_wake_at = now()
   WHERE id = $parent_id;
   ```

9. A worker claims the parent, replays its event history. During replay of step 5 (`child_awaiting`), the host scans forward in the parent's history. It finds step 6 (`child_completed`) with a matching `child_run_id`. The host returns the result from the `response` field immediately -- no actual waiting.

10. If the child **failed** instead of completing successfully, the child worker records a `child_failed` event in the parent's history:

    ```json
    {"step": 6, "service": "_durable", "operation": "child_failed",
     "request": {"child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
     "response": {"error": "fraud check rejected: amount exceeds threshold", "error_code": "FRAUD_REJECTED"}}
    ```

    On replay, `DurableAwaitChild` sees `child_failed` and returns an error to the parent WASM module. The parent's error handling can decide whether to compensate or fail.

11. If the child timed out (exceeded its `Timeout` in `ChildWorkflowOptions`), the child's claim-lease mechanism detects the timeout, marks the child as `failed` with a timeout error, and follows the same completion-handler path to notify the parent. The event is recorded as `child_failed` with a `"timeout"` error code.

**Fire-and-forget variant:**

When `FireAndForget = true`, the execution flow is simpler:

1. The WASM module calls `h.DurableChildWorkflow("AuditLog", input, &opts)`.
2. The host records a `child_started` event with `fire_and_forget: true`.
3. The host INSERTs the child row in `workflow_instances` with `parent_workflow_id` set.
4. The host returns the `child_run_id` to the parent. The parent **does not** call `DurableAwaitChild`. The parent continues executing immediately -- no blocking, no `child_awaiting` event.
5. The child runs independently. When it completes, the child's completion handler checks for a parent but finds no awaiting event; no parent notification is needed.
6. On replay, the host sees the `child_started` event with `fire_and_forget: true` and returns the cached `child_run_id` immediately. No child is re-started.

The `DurableChildWorkflow` still returns an error if the child instance cannot be created (e.g., the child's `def_name` does not exist, or limit checks fail). But after creation, the parent never waits.

**Cancellation cascading:**

When a parent workflow is cancelled, children are cancelled by default. The parent has a grace period to run compensation (section 8.6), and the host handles children during that period.

**Cascade flow:**

1. A cancellation request arrives for the parent (section 8.6 flow).
2. The host sets `cancelled = true` on the parent and records a `cancellation_requested` event.
3. Before the parent runs its compensation, the host queries for non-terminal children:

   ```sql
   SELECT id, def_name, status
   FROM workflow_instances
   WHERE parent_workflow_id = $parent_id
     AND status NOT IN ('done', 'failed', 'cancelled');
   ```

4. For each non-terminal child, the host inserts a `cancellation_requested` event in the child's event history and sets `cancelled = true` on the child instance. If the child is queued (`status = 'ready'`), the host sets `next_wake_at = now()` to make it claimable. If the child is running on a worker, the host notifies the worker via the same signal/cancellation HTTP endpoint used for top-level cancellations.
5. When the child detects cancellation (during its own `DurableCall`, `DurableSleep`, or via `DurablePollCancellation`), it runs its compensation and exits. The child's completion handler writes a `child_cancelled` event in the parent's event history:

   ```json
   {"step": 6, "service": "_durable", "operation": "child_cancelled",
    "request": {"child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
    "response": {"error": "parent cancelled: customer called support", "compensation": ["inventory.Release"]}}
   ```

6. If the parent was awaiting this child (`DurableAwaitChild`), the `child_cancelled` event causes `DurableAwaitChild` to return `ErrCancelled` to the parent. The parent's error handling can then run its own compensation.

**Parent close policy:** Each child workflow has a `ParentClosePolicy` that determines what happens when the parent completes or fails. Three policies are supported, matching Temporal's model:

- **ABANDON** (default): The child continues running independently regardless of the parent's fate.
- **REQUEST_CANCEL**: A cancellation request is delivered to the child, giving it a grace period to run compensation before exiting.
- **TERMINATE**: The child is immediately marked as failed without running compensation.

The policy is set via `ChildWorkflowOptions.ParentClosePolicy`:

```go
_, _ = h.DurableChildWorkflow("AuditLog", input, &durable.ChildWorkflowOptions{
    FireAndForget: true,
    ParentClosePolicy: durable.ParentClosePolicyAbandon,
})
```

**Event history representation -- full lifecycle:**

```json
// 1. Child started:
{"step": 4, "service": "_durable", "operation": "child_started",
 "request": {"def_name": "FraudCheck", "input": {"item_id": "SKU-123", "amount_cents": 4999},
             "child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
             "timeout_ms": 30000, "fire_and_forget": false},
 "response": null}

// 2. Parent awaits child:
{"step": 5, "service": "_durable", "operation": "child_awaiting",
 "request": {"child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
 "response": null}

// 3. Child completes successfully:
{"step": 6, "service": "_durable", "operation": "child_completed",
 "request": {"child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
 "response": {"output": {"status": "approved", "score": 42}, "duration_ms": 1234}}

// Alternative 3a. Child fails:
{"step": 6, "service": "_durable", "operation": "child_failed",
 "request": {"child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
 "response": {"error": "fraud check rejected: amount exceeds threshold",
              "error_code": "FRAUD_REJECTED", "duration_ms": 5678}}

// Alternative 3b. Child cancelled (due to parent cancellation):
{"step": 6, "service": "_durable", "operation": "child_cancelled",
 "request": {"child_run_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890"},
 "response": {"error": "parent cancelled: customer called support",
              "duration_ms": 890}}
```

**Replay behavior:**

Like all durable operations, child workflow interactions are deterministic because they are recorded in the event history.

**Replay of `DurableChildWorkflow`:**

1. The WASM module calls `h.DurableChildWorkflow("FraudCheck", input, opts)`.
2. The host checks if the current step in the event history is a `child_started` event.
3. If yes, the host returns the cached `child_run_id` from the event history. **No new child is started** -- the child already exists in `workflow_instances` and was created during the original execution.
4. If the event at the current step is NOT a `child_started` event (this should not happen in a well-formed history), the host treats it as a fatal replay error and fails the workflow.

**Replay of `DurableAwaitChild`:**

1. The WASM module calls `h.DurableAwaitChild(runID)`.
2. The host scans forward in the parent's event history for the next `child_completed`, `child_failed`, or `child_cancelled` event where `child_run_id` matches.
3. **If found:** The host returns the result (success with output, or error) immediately from the history. The parent continues executing.
4. **If not found** (the child is still running or hasn't been claimed yet): The host re-records a `child_awaiting` event at the current step, sets `next_wake_at = NULL` on the parent, sets `status = 'ready'`, and releases the parent back to the queue. This is the "parent was replayed before the child completed" case -- the parent re-enters the waiting state and will be woken when the child completes.

**Replay of fire-and-forget:**

1. The WASM module calls `h.DurableChildWorkflow("AuditLog", input, &opts)` with `FireAndForget: true`.
2. The host sees the `child_started` event with `fire_and_forget: true` in the history.
3. The host returns the cached `child_run_id`. The parent continues immediately.
4. No `DurableAwaitChild` call follows -- the parent's code path diverges (it only calls `DurableAwaitChild` for children it intends to wait on).

**Limits and guardrails:**

Child workflows create real database rows and consume worker throughput. Without limits, a buggy parent could create millions of children and exhaust database connections or worker capacity.

| Limit | Default | Enforcement |
|---|---|---|
| **Max child depth** | 10 | Checked at `DurableChildWorkflow` time. The host walks the `parent_workflow_id` chain. If `depth >= 10`, returns an error: `"child workflow depth exceeds maximum (10)"`. |
| **Max concurrent children** | 100 | Checked at `DurableChildWorkflow` time. The host counts active children for this parent (`SELECT COUNT(*) FROM workflow_instances WHERE parent_workflow_id = $1 AND status NOT IN ('done', 'failed', 'cancelled')`). If `>= 100`, returns an error: `"too many concurrent children (limit: 100)"`. |
| **Max total children per parent** | 1000 | Checked at `DurableChildWorkflow` time. The host counts all children ever started by this parent (regardless of status). If `>= 1000`, returns an error: `"too many total children (limit: 1000)"`. |
| **Child timeout** | 0 (no timeout) | Enforced by the child's claim-lease mechanism. If `ChildWorkflowOptions.Timeout > 0`, the child's `next_wake_at` is set to `now() + timeout` during creation. If the child hasn't completed by then, it is claimed by a worker that detects the timeout, sets the child's status to `failed` with `timeout error`, and notifies the parent. |

These limits are configurable per worker via a configuration file or environment variables. Exceeding any limit returns an error from `DurableChildWorkflow` -- the error is recorded in the parent's event history as a `child_started` failure, and the parent can handle it (e.g., skip the child for this item and log a warning).

**Comparison to Temporal:**

Temporal's child workflows share the same fundamental design -- separate event histories, independent lifecycles, parent-child instance trees. The main differences:

| Aspect | Temporal | cleat |
|---|---|---|
| **API** | `ExecuteChildWorkflow()` returns a `Future`; `future.Get()` blocks | `DurableChildWorkflow()` returns `runID`; `DurableAwaitChild(runID)` blocks |
| **Separate histories** | Yes, child has its own event history | Yes, same approach |
| **Parent-close policy** | `ParentClosePolicy` enum (`ABANDON`, `TERMINATE`, `REQUEST_CANCEL`) per child | Same: `ParentClosePolicy` enum (`ABANDON`, `TERMINATE`, `REQUEST_CANCEL`) per child |
| **Fire-and-forget** | `ParentClosePolicy.ABANDON` + never call `.Get()` | `FireAndForget: true` option on `ChildWorkflowOptions` |
| **Cancellation propagation** | Explicit per child via `ParentClosePolicy` | Same: per-child `ParentClosePolicy`; `enforceParentClosePolicy` runs on parent terminal transition |
| **Depth limit** | Not enforced by Temporal (operational concern) | Default 10, configurable |
| **Fan-out limit** | Not enforced by Temporal (operational concern) | Default 100 concurrent, configurable |
| **Version resolution** | Child uses the same task queue and worker binary (same version) | Compile-time pinning: child version is embedded in parent's WASM metadata at build time; pinned via `cleat.lock` |
| **Exactly-once creation** | Via workflow ID + dedup in Temporal server | Atomic transaction: child row + parent `child_workflow` event committed together; reply deduplicated via `PRIMARY KEY (workflow_id, step)` |

Temporal's approach gives more fine-grained control (separate parent-close policies per child) at the cost of complexity -- every child needs an explicit policy decision. This design defaults to sensible behavior: children are cancelled with their parent (preventing orphan workflows), and fire-and-forget is an explicit opt-in (`FireAndForget: true`). The limits are enforced at the database level rather than being purely operational concerns, which prevents runaway resource consumption from buggy workflow code.

### 8.6 Workflow cancellation

**Design:** Cancellation is a request to stop a running workflow, delivered through the same host-API + event-history pattern as signals. It gives the workflow a grace period to run compensation logic, then force-terminates if the workflow doesn't exit in time.

**Host API for cancellation:**

```
POST /api/v1/workflows/{workflow_id}/cancel
Body: {"reason": "customer called support", "requested_by": "agent-42"}
```

The host API handler:
1. Looks up the workflow instance. If already completed, returns 409.
2. Inserts a cancellation event into the event history:

   ```json
   {"step": 6, "service": "_durable", "operation": "cancellation_requested",
    "request": {"reason": "customer called support", "requested_by": "agent-42"},
    "response": null}
   ```

3. Records `cancelled = true` and `cancel_reason` on the workflow instance.
4. If the workflow is running on a worker, notifies the worker goroutine via the same in-process channel used for signals.
5. If the workflow is queued, sets `next_wake_at = now()` to make it immediately claimable.

**Schema additions:**

```sql
ALTER TABLE workflow_instances ADD COLUMN cancelled BOOLEAN DEFAULT false;
ALTER TABLE workflow_instances ADD COLUMN cancel_reason TEXT;
ALTER TABLE workflow_instances ADD COLUMN cancel_requested_at TIMESTAMPTZ;
ALTER TABLE workflow_instances ADD COLUMN cancel_grace_until TIMESTAMPTZ;

ALTER TABLE workflow_defs ADD COLUMN cancel_grace_period_ms INT DEFAULT 30000; -- 30 seconds
```

**WASM-side API:**

```go
type HostCalls struct {
    // ... existing functions ...
    DurablePollCancellation func() (cancelled bool, reason string)
}
```

`DurablePollCancellation` is non-blocking. It checks whether a cancellation has been requested for this workflow. The workflow calls it at natural cancellation points — after each `DurableCall`, at the top of processing loops, or explicitly before starting an expensive operation.

**How cancellation propagates:**

The host checks cancellation at every interaction point automatically. The WASM module doesn't need to call `DurablePollCancellation` before every `DurableCall` — the host does it implicitly:

1. **During `DurableCall`:** Before making the HTTP call, the host checks if the workflow is cancelled. If so, it records a `cancellation_completed` event and returns a special `ErrCancelled` error to the WASM module. The workflow's error handling can catch this and run compensation.

2. **During `DurableSleep` / `DurableAwaitSignals`:** If the workflow is sleeping or waiting for signals and a cancellation arrives, the host immediately wakes the workflow and returns `ErrCancelled` from the sleep/await call.

3. **Explicit check:** The workflow can call `h.DurablePollCancellation()` at any time to check proactively.

**Usage example — cancellation-aware workflow:**

```go
func OrderWorkflow(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    // Step 0: Reserve inventory
    reservation, err := reserveInventory(h, userID, cart)
    if err != nil {
        return "", err // nothing to compensate
    }

    // Step 1: Charge customer
    charge, err := chargeCustomer(h, userID, reservation.TotalCents)
    if err != nil {
        // Check if this was a cancellation (host returns ErrCancelled from DurableCall)
        if errors.Is(err, durable.ErrCancelled) {
            releaseReservation(h, reservation.ReservationID)
            return "", err // propagate cancellation after compensation
        }
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("payment failed: %w", err)
    }

    // Step 2: Create shipment
    trackingID, err := createShipment(h, reservation, charge)
    if err != nil {
        if errors.Is(err, durable.ErrCancelled) {
            // Compensate: refund AND release before exiting
            refundPayment(h, charge.ChargeID)
            releaseReservation(h, reservation.ReservationID)
            return "", err
        }
        refundPayment(h, charge.ChargeID)
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("shipping failed: %w", err)
    }

    return trackingID, nil
}
```

**Grace period and force-termination:**

When a cancellation is requested, the host starts a grace period timer (default 30 seconds, configurable per workflow definition). The workflow is expected to detect the cancellation, run compensation, and return within the grace period.

If the workflow hasn't returned by the time the grace period expires:
1. The host force-terminates the WASM module (calls `runtime.Close()` on the wazero instance).
2. The host records a `cancellation_forced` event in the event history.
3. The workflow instance is marked `status = 'cancelled'` with a note that force-termination occurred.

```json
// Grace period expired — force termination:
{"step": 10, "service": "_durable", "operation": "cancellation_forced",
 "request": {"reason": "grace period expired", "grace_period_ms": 30000},
 "response": null}
```

**Graceful completion after cancellation:**

If the workflow handles the cancellation gracefully:

```json
// Workflow compensated and exited:
{"step": 9, "service": "_durable", "operation": "cancellation_completed",
 "request": {"compensation": ["inventory.Release", "payments.Refund"]},
 "response": null}
```

The workflow instance is marked `status = 'cancelled'`.

**Replay behavior:**

Cancellation is deterministic because it's recorded in the event history. On replay:
1. The worker replays steps until it hits the `cancellation_requested` event.
2. After this point, every `DurableCall` and `DurableSleep` returns `ErrCancelled`.
3. The workflow follows the same error-handling paths (compensation) as it did during the original execution.
4. The replay reproduces the same compensation calls, which are already in the event history, so they're returned from cache.

**Cancellation vs failure vs dead letter:**

| Outcome | Trigger | Status | Compensation runs? |
|---|---|---|---|
| Normal completion | Workflow returns nil error | `done` | N/A |
| Workflow error | Workflow returns error | `failed` | Yes (in error handling) |
| Cancellation (graceful) | External cancel, workflow handles within grace period | `cancelled` | Yes (in cancellation handling) |
| Cancellation (forced) | External cancel, grace period expires | `cancelled` | Partial (whatever ran before force-termination) |
| Dead letter | Repeated failure on same step | `dead_letter` | Depends on where failure occurred |

### 8.6b DurableDefer — cleanup on any exit

**Design:** `DurableDefer` is the durable equivalent of Go's `defer`. It registers a cleanup function that runs exactly once when the workflow exits — whether by successful return, error return, or cancellation. The host guarantees execution, records it in the event history, and skips it on replay.

Without `DurableDefer`, compensation is manual and error-prone:

```go
// Current design — manual compensation in every branch:
reservation, err := reserveInventory(h, userID, cart)
if err != nil {
    return "", err
}
charge, err := chargeCustomer(h, userID, reservation.TotalCents)
if err != nil {
    releaseReservation(h, reservation.ReservationID) // don't forget this
    return "", fmt.Errorf("payment failed: %w", err)
}
trackingID, err := createShipment(h, reservation, charge)
if err != nil {
    refundPayment(h, charge.ChargeID)                // and this
    releaseReservation(h, reservation.ReservationID) // and this
    return "", fmt.Errorf("shipping failed: %w", err)
}
```

With `DurableDefer`, the cleanup is registered once, right after resource acquisition:

```go
// With DurableDefer — register cleanup once, runs on any exit path.
reservation, err := reserveInventory(h, userID, cart)
if err != nil {
    return "", err
}
h.DurableDefer(func() error {
    return releaseReservation(h, reservation.ReservationID)
})

charge, err := chargeCustomer(h, userID, reservation.TotalCents)
if err != nil {
    return "", fmt.Errorf("payment failed: %w", err)
    // releaseReservation runs automatically here
}
h.DurableDefer(func() error {
    return refundPayment(h, charge.ChargeID)
})

trackingID, err := createShipment(h, reservation, charge)
if err != nil {
    return "", fmt.Errorf("shipping failed: %w", err)
    // refundPayment AND releaseReservation run automatically (LIFO order)
}

// On successful return, nothing is compensated.
// On cancellation, both defers run in LIFO order: refund → release.
return trackingID, nil
```

**API:**

```go
type HostCalls struct {
    // ... existing functions ...
    DurableDefer func(fn func() error)
}
```

**Execution model (formal state machine):**

When the workflow function returns (for any reason — success, error, cancellation, or panic unwinding), the host transitions from **IDLE** into **defer execution mode**:

```
States: IDLE -> EXECUTING_DEFERS -> COMPLETE | CANCELLATION_FORCED
```

- **IDLE.** The workflow function is still running. Each call to `DurableDefer` pushes a closure onto an in-memory defer stack and records a `defer_registered` event in the history. No defers are being executed.
- **EXECUTING_DEFERS.** The workflow function has returned. The host pops defers from the stack in LIFO order and executes each one:

  1. The host calls a WASM export named `execute_defer(defer_id)`, passing the defer's unique identifier. This invokes the closure that was registered by the workflow.
  2. Inside the closure, `DurableCall`, `DurablePollSignal`, `DurableLog`, and `Now` are available and handled normally. Each `DurableCall` executes the real API call and records the result in `event_history` under the current step counter.
  3. When the closure returns successfully, the host records a `defer_executed` event in the event history and pops the next defer from the stack (LIFO order).
  4. After all defers have executed, the host transitions to **COMPLETE**.

- **CANCELLATION_FORCED.** The grace period expired during defer execution (see below).

**LIFO execution order:** Defers run in last-in-first-out order, matching Go's `defer`. This is critical: you refund the payment before releasing the inventory because the refund might need the charge ID from the payment step.

**What happens on each exit path:**

| Exit path | Defers run? | Workflow status |
|---|---|---|
| `return value, nil` | No (success — nothing to undo) | `done` |
| `return "", err` | Yes — all registered defers in LIFO order | `failed` |
| Cancellation (`ErrCancelled`) | Yes — all registered defers in LIFO order | `cancelled` |
| Panic in WASM module | Yes — defers still run during panic unwinding | `failed` |

Note that on successful return, defers do NOT run. The developer decides which path does compensation by choosing when to call `DurableDefer`. If you want cleanup on success too (e.g., always send a notification), call `DurableDefer` for that specific action.

**Event history representation:**

```json
// Defer registered:
{"step": 3, "service": "_durable", "operation": "defer_registered",
 "request": {"defer_id": 1, "description": "releaseReservation(resv_123)"},
 "response": null}

// Defer executed during cancellation:
{"step": 8, "service": "_durable", "operation": "defer_executed",
 "request": {"defer_id": 1, "parent_exit": "cancelled"},
 "response": null}
```

**Edge cases and restrictions:**

- **DurableSleep in a defer is an error.** Defers are compensation actions — they must make exactly the API calls needed to undo prior work and return without suspending. Sleeping inside a defer introduces state inconsistency (the workflow has already logically exited) and makes the grace period unreliable. If a defer calls `DurableSleep`, the host returns a runtime error (`"defer cannot sleep"`) and the defer fails. On defer failure, the remaining defers are NOT executed.

- **DurableDefer inside a defer is allowed (nesting).** If a defer's closure calls `DurableDefer`, the new defer is pushed onto the execution stack. The host records a `defer_registered` event for it, and it executes in LIFO order after the current defer finishes.

- **DurableAwaitSignals in a defer is an error.** The workflow has already exited and cannot wait for external input. Same treatment as sleep.

- **Grace period expiry mid-defer.** The host tracks a configurable grace period (default: 30 seconds) during which defers must complete. If the grace period expires while a defer is still executing:
  1. The host kills the WASM runtime instance via `runtime.Close()`.
  2. The host records a `cancellation_forced` event in the event history.
  3. The workflow status is set to `cancelled` with `cancellation_note = 'grace_period_expired'`.
  4. Remaining unexecuted defers are skipped — compensation was best-effort within the grace period.

- **Worker crash mid-defer.** If the worker crashes while executing a defer, the workflow is re-claimed by another worker after the 30-second heartbeat timeout. On replay:
  1. The host replays the event history, encountering `defer_registered` events for each defer that was registered before the crash.
  2. The host also encounters `defer_executed` events for defers that completed before the crash.
  3. The host builds the defer stack from `defer_registered` events in registration order. It marks defers as "already executed" if a matching `defer_executed` event exists.
  4. When the workflow function returns during replay, the host enters EXECUTING_DEFERS mode. It pops defers in LIFO order, skipping those already marked as executed.
  5. The first unexecuted defer is invoked via `execute_defer(defer_id)` for real. Already-completed defers are replayed from cache — their `defer_executed` events serve as the source of truth and the closures are not re-executed.

**Testing DurableDefer:**

```go
func TestWorkflow_DefersRunOnError(t *testing.T) {
    h := durable.NewTestHost(t)

    h.MockCall("inventory", "Reserve", `{"reservation_id":"resv_123"}`, nil)
    h.MockCall("payments", "Charge", "", errors.New("insufficient funds"))
    // The defer should call releaseReservation:
    h.MockCall("inventory", "Release", `{"status":"released"}`, nil)

    _, err := OrderWorkflow(h, "user-1", testCart)

    assert.Error(t, err)
    h.AssertCalled(t, "inventory", "Release") // defer was executed
}

func TestWorkflow_DefersRunInLIFO_OnCancellation(t *testing.T)    h := durable.NewTestHost(t)

    h.MockCall("inventory", "Reserve", `{"reservation_id":"resv_123"}`, nil)
    h.MockCall("payments", "Charge", `{"charge_id":"chg_456"}`, nil)
    h.Cancel("customer request")
    // Defers run in LIFO: refund first, then release.
    h.MockCall("payments", "Refund", `{"status":"refunded"}`, nil)
    h.MockCall("inventory", "Release", `{"status":"released"}`, nil)

    _, err := OrderWorkflow(h, "user-1", testCart)

    assert.True(t, errors.Is(err, durable.ErrCancelled))
    // Verify LIFO order:
    h.AssertCalls(t, []durable.ExpectedCall{
        {Service: "inventory", Operation: "Reserve"},
        {Service: "payments", Operation: "Charge"},
        {Service: "payments", Operation: "Refund"},    // LIFO: first defer
        {Service: "inventory", Operation: "Release"},  // LIFO: second defer
    })
}
```

### 8.7 Idempotency for external calls

**Design:** The host generates an idempotency key for every `DurableCall` and injects it into the outbound HTTP request. The key is deterministic — derived from `(workflow_id, step)` — so that the same call always uses the same key, regardless of which worker executes it or how many times it retries.

**Key derivation:**

```
idempotency_key = SHA256(workflow_id || ":" || step)[:16]
// Example: "order-alice-001:4" → "a1b2c3d4e5f6g7h8"
```

The step number is the position in the event history. It never changes for a given logical operation, even across worker crashes and retries. The step number is assigned when the WASM module makes the call — it's the `currentStep` counter in the worker execution loop.

**HTTP header injection:** The host's HTTP client adds the idempotency key to the request:

```go
func (h *host) executeHTTPCall(service, op string, requestJSON string, idempotencyKey string) (string, error) {
    req, _ := http.NewRequest("POST", h.resolveEndpoint(service, op), strings.NewReader(requestJSON))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Idempotency-Key", idempotencyKey)
    req.Header.Set("X-Workflow-Id", h.workflowID)
    req.Header.Set("X-Step-Number", strconv.Itoa(h.currentStep))
    // ... execute with timeout from retry policy
}
```

**Event history records the key:** The idempotency key is stored in the event history alongside the request and response, making it auditable:

```json
{
  "step": 4,
  "service": "payments",
  "operation": "Charge",
  "idempotency_key": "a1b2c3d4e5f6g7h8",
  "attempt": 3,
  "request": {"amount_cents": 3299},
  "response": {"charge_id": "chg_xyz", "status": "captured"},
  "duration_ms": 87
}
```

The `attempt` field tracks which retry succeeded, for debugging.

**What this guarantees:** Even if the host crashes after the payment service processed the charge but before the response was recorded, the next worker will replay step 4, generate the SAME idempotency key, and the payment service will return the original result (or 409 Conflict with the original result) rather than processing a duplicate charge.

**Services that don't support idempotency:** For services without idempotency support, the host can still detect potential duplicates. If step N has no recorded response in the event history but the worker is replaying, it logs a warning and makes the call anyway — this is the best-effort case. Most payment-grade APIs (Stripe, Adyen, Braintree, Square) support idempotency keys. The design can't fix services that don't.

**Event history schema addition:**

```sql
ALTER TABLE event_history ADD COLUMN idempotency_key TEXT;
ALTER TABLE event_history ADD COLUMN attempt INT DEFAULT 1;
```

### 8.8 Secrets, credentials, and operation-level authorization

**Design:** The WASM module never sees credentials. It calls `h.DurableCall("payments", "Charge", request)`. The host resolves credentials based on the `service` name, using a pluggable `CredentialResolver` interface:

```go
// CredentialResolver resolves credentials for a given service.
// Implementations: EnvResolver, VaultResolver, AWSSecretsManagerResolver.
type CredentialResolver interface {
    Resolve(service string) (*Credentials, error)
}

type Credentials struct {
    Type         string            // "bearer", "basic", "api_key", "oauth2"
    Token        string            // bearer token or API key value
    Username     string            // for basic auth
    Password     string            // for basic auth
    TokenURL     string            // for oauth2 client credentials
    ClientID     string            // for oauth2
    ClientSecret string            // for oauth2
    Headers      map[string]string // additional headers to inject
}
```

**Worker configuration:**

```json
{
  "credential_resolver": "vault",
  "credential_config": {
    "vault_addr": "https://vault.internal:8200",
    "vault_path": "secret/data/durable-workflows"
  },
  "service_endpoints": {
    "payments": "https://payments.internal/api",
    "catalog": "https://catalog.internal/api",
    "inventory": "https://inventory.internal/api"
  },
  "service_allowlist": ["payments", "catalog", "inventory", "shipping", "notifications"]
}
```

The `service_allowlist` provides a worker-wide defense-in-depth layer: even if a WASM module tries to call `h.DurableCall("spam_api", "Send", ...)`, the host rejects it. However, the worker-wide allowlist is coarse — any workflow running on this worker can call any service in the list. A workflow that should only call `payments.Charge` can also call `payments.Refund` or `payments.CreateCustomer`.

**Operation-level allowlist:**

To provide per-workflow authorization, add an `operation_allowlist` column to `workflow_defs`:

```sql
ALTER TABLE workflow_defs ADD COLUMN operation_allowlist JSONB;
```

The column specifies which operations each workflow definition is permitted to call, as a map from service name to a list of allowed operation names:

```json
{
    "payments":   ["Charge", "GetDefaultMethod"],
    "inventory":  ["Reserve", "Release"]
}
```

This scopes each workflow definition to exactly the operations it needs, following the principle of least privilege.

**Authorization check flow:**

On every `DurableCall(service, operation, request)`:

```
WASM: h.DurableCall("payments", "Charge", `{"amount": 3299}`)
        │
        ▼
Host:  1. Check worker-wide service_allowlist: is "payments" allowed?
           NO → return error (security rejection, NOT recorded in event history)
        │
        ▼
       2. Check per-workflow-def operation_allowlist:
           If operation_allowlist is set for this workflow_def:
             Is "payments"."Charge" in the list?
               NO → return error (security rejection, NOT recorded in event history)
           If operation_allowlist is NULL or empty: skip check (backward compatible)
        │
        ▼
       3. Look up endpoint for "payments" → https://payments.internal/api/Charge
       4. Resolve credentials for "payments" → Bearer sk_live_abc123
       5. Generate idempotency key → SHA256(workflow_id:step)
       6. POST https://payments.internal/api/Charge
            Headers: Authorization: Bearer sk_live_abc123
                     Idempotency-Key: a1b2c3d4e5f6g7h8
                     Content-Type: application/json
            Body: {"amount": 3299}
       7. Record result in event history
       8. Return result to WASM
```

Key properties:

- **Security rejections are not recorded in event history.** If a WASM module attempts an unauthorized call, the host returns an error immediately without appending to `event_history`. This avoids creating a record of the failed attempt in the durable history (which would confuse replay), and prevents information leakage about which operations exist but are unauthorized.
- **Defense-in-depth.** Even if a WASM module is compromised, the `operation_allowlist` limits the blast radius to the operations declared for that workflow definition.
- **Backward compatible.** If `operation_allowlist` is NULL (existing `workflow_defs`), the per-workflow check is skipped and only the worker-wide `service_allowlist` applies.

**Optional static verification:**

The transformer can optionally verify at build time that all `DurableCall` sites in the source code reference operations in the declared allowlist:

```go
h.DurableCall("payments", "Charge", request)   // OK — in allowlist
h.DurableCall("payments", "Refund", request)   // WARNING — not in allowlist for this workflow_def
```

The transformer reads the `operation_allowlist` from a build-time configuration file (e.g., `workflow.policy.yaml`) and emits warnings or errors for any `DurableCall` that references a (service, operation) pair not in the list. This catches allowlist drift during development: if a developer adds a new API call but forgets to update the `operation_allowlist`, the build fails.

Dynamic arguments (e.g., `h.DurableCall(serviceVar, opVar, req)`) cannot be verified at build time and fall through to runtime enforcement.

**Credential lifecycle:**

The WASM module never touches the token. The credential lifecycle (rotation, revocation) is managed at the host level, independently of workflow code.

### 8.9 Input/output schema evolution

Moved from original 8.9.

**Design:** Workflows evolve. Fields are added to inputs, APIs change order, and sometimes the entire input format needs restructuring. The fundamental constraint is that in-flight instances must replay against the code that produced their event history -- so changing a workflow's input schema without breaking running instances requires explicit design. There are two approaches, each appropriate for different kinds of changes:

1. **Version markers** for minor, backward-compatible changes where the workflow code can handle both old and new paths.
2. **Migration functions** for breaking changes where in-flight instances must be transformed to the new schema.

#### 8.9.1 Approach A: Version markers (minor changes)

For changes that don't require migrating in-flight instances -- the workflow code contains both old and new code paths, and a version marker in the event history tells it which path to take during replay. This is analogous to Temporal's `GetVersion()`.

**Host API addition:**

```go
type HostCalls struct {
    // ... existing functions ...
    DurableVersion func(changeID string, minSupported, maxSupported int) int
}
```

**Usage example:**

```go
func PlaceOrder(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    v := h.DurableVersion("AddDiscountCode", 1, 2)
    if v >= 2 {
        // New path: apply discount code
        if input.DiscountCode != "" {
            h.DurableCall("payments", "ApplyDiscount", ...)
        }
    }
    // ... rest of workflow (shared between v1 and v2)
}
```

**How it works:**

- **First execution with v2 code:** `DurableVersion` records a `version_marker` event in the event history and returns `maxSupported` (2 in this example). The workflow takes the new code path.
- **Replay of a v1 history** (no version marker exists for this `changeID`): returns `minSupported` (1). The workflow takes the old code path.
- **Replay of a v2 history** (the marker was recorded during original execution): returns the recorded version from the marker event. The workflow takes the same path it took originally.

The version marker is the durable source of truth. During replay, the host returns the recorded version from the event history -- it never calls the new code path if the original execution didn't.

**Event history representation:**

```json
{"step": 2, "service": "_durable", "operation": "version_marker",
 "request": {"change_id": "AddDiscountCode", "version": 2},
 "response": null}
```

**Constraints:**

- `changeID` must be unique per workflow definition. Using the same ID for two different changes produces ambiguous event history.
- `minSupported` can never decrease. Once you raise the floor, you commit to supporting at least that version. A deployment that lowered `minSupported` would break replay for instances that recorded a marker at the original floor.
- `maxSupported` can only increase over time. Decreasing it would orphan markers already recorded at the higher version.
- Old code paths can be removed only when all in-flight instances have a version marker at or above the removal threshold. Operators can verify by querying for instances without the relevant marker event.
- The transformer can verify that all `changeID` strings in the source code match a declared set, preventing typos and orphaned markers.

#### 8.9.2 Approach B: Migration functions (major changes)

For breaking changes where in-flight instances MUST be migrated to new code. This is an operator-initiated process, not automatic. It is heavyweight but gives complete control over the shape of the migration.

**Schema additions to `workflow_defs`:**

```sql
ALTER TABLE workflow_defs ADD COLUMN version_migration_policy JSONB DEFAULT '{"strategy": "none"}';
-- strategy: "none" | "marker" | "migration"

ALTER TABLE workflow_defs ADD COLUMN migrations JSONB;
-- e.g., {"from_version": 1, "to_versions": [2],
--         "input_transform": "migrateV1ToV2", "history_transform": "migrateHistoryV1ToV2"}
```

When `strategy` is `"migration"`, a `workflow_migrations` table stores the migration WASM modules:

```sql
CREATE TABLE workflow_migrations (
    def_name      TEXT NOT NULL,
    from_version  INT  NOT NULL,
    to_version    INT  NOT NULL,
    wasm_bytes    BYTEA NOT NULL,   -- compiled migration WASM
    created_at    TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (def_name, from_version, to_version)
);
```

**Migration WASM interface:**

A migration function is itself a WASM module that exports two functions:

```go
//go:wasmexport migrate_input
func migrateInput(oldInputJSON string) (newInputJSON string, errCode int32)

//go:wasmexport migrate_history
func migrateHistory(oldHistoryJSON string) (newHistoryJSON string, errCode int32)
```

The migration module is compiled from standard Go (or any language that compiles to WASM). A typical input migration adds default values for new fields:

```go
//go:wasmexport migrate_input
func migrateInput(oldInputJSON string) (string, int32) {
    var old struct {
        UserID string     `json:"user_id"`
        Cart   []CartItem `json:"cart"`
    }
    if err := json.Unmarshal([]byte(oldInputJSON), &old); err != nil {
        return "", 1
    }
    new := struct {
        UserID       string     `json:"user_id"`
        Cart         []CartItem `json:"cart"`
        DiscountCode string     `json:"discount_code"`
    }{
        UserID:       old.UserID,
        Cart:         old.Cart,
        DiscountCode: "", // default for migrated instances
    }
    b, _ := json.Marshal(new)
    return string(b), 0
}
```

The history migration transforms the old event history to match the new code's expectations -- reordering steps, remapping service names, or splicing in new calls:

```go
//go:wasmexport migrate_history
func migrateHistory(oldHistoryJSON string) (string, int32) {
    // Transform event history: reorder steps, remap service names, etc.
    // ...
    return newHistoryJSON, 0
}
```

**The migration process:**

When an operator runs:

```
durable migrate PlaceOrder --from 1 --to 2
```

The system:

1. **Queries all in-flight v1 instances:**

   ```sql
   SELECT id, input
   FROM workflow_instances
   WHERE def_name = 'PlaceOrder'
     AND def_version = 1
     AND status IN ('ready', 'running');
   ```

2. **For each instance:** sends a cancellation signal to gracefully stop at the next durable boundary. This ensures the event history is quiescent -- no concurrent writes.

3. **Loads the migration WASM** from `workflow_migrations` for `(PlaceOrder, 1, 2)` and executes it in a fresh wazero runtime:
   - Calls `migrate_input(oldInput)` to produce the new input.
   - Calls `migrate_history(oldHistoryJSON)` to produce the new event history.

4. **Creates a new v2 instance** with the migrated schema:

   ```sql
   INSERT INTO workflow_instances (
       id, def_name, def_version, status, input
   ) VALUES (
       gen_random_uuid(), 'PlaceOrder', 2, 'ready', $migrated_input
   );
   ```

5. **Seeds the new instance's event history** with the migrated history:

   ```sql
   INSERT INTO event_history (workflow_id, step, service, operation, request, response)
   SELECT $new_id, step, service, operation, request, response
   FROM jsonb_to_recordset($migrated_history)
   AS t(step INT, service TEXT, operation TEXT, request JSONB, response JSONB);
   ```

6. **Links old and new instances** and marks the old one as `migrated`:

   ```sql
   UPDATE workflow_instances SET status = 'migrated', migrated_to = $new_id WHERE id = $old_id;
   ```

7. The new instance is now claimable by a worker, which replays the migrated history against v2 code and continues execution from where the old instance left off.

#### 8.9.3 Schema additions

All schema changes for schema evolution:

```sql
ALTER TABLE workflow_defs ADD COLUMN version_migration_policy JSONB DEFAULT '{"strategy": "none"}';
ALTER TABLE workflow_defs ADD COLUMN migrations JSONB;

CREATE TABLE workflow_migrations (
    def_name      TEXT NOT NULL,
    from_version  INT  NOT NULL,
    to_version    INT  NOT NULL,
    wasm_bytes    BYTEA NOT NULL,
    created_at    TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (def_name, from_version, to_version)
);

ALTER TABLE workflow_instances ADD COLUMN migrated_to UUID REFERENCES workflow_instances(id);
ALTER TABLE workflow_instances ADD COLUMN migrated_from UUID REFERENCES workflow_instances(id);
```

#### 8.9.4 When to use which

| Change | Approach | Example |
|---|---|---|
| Add optional field to input | Version marker | `discountCode` (defaults to `""`) |
| Reorder API calls | Version marker | Move fraud check before payment |
| Change API call parameters | Version marker | New field in Charge request |
| Add a new required field | Migration function | Now requires `tax_id` |
| Change input format completely | Migration function | JSON -> protobuf |
| Split one workflow into two | Migration function | PlaceOrder -> ValidateOrder + FulfillOrder |

#### 8.9.5 Adding to the gaps summary

This section addresses two gaps in the current design, which should be reflected in the gaps table (8.18):

- **Version markers (minor changes)** -- P2, Small effort. A single new `HostCalls` function (`DurableVersion`) plus event history support for `version_marker` events. The transformer needs to verify `changeID` uniqueness.
- **Migration functions (major changes)** -- P2, Large effort. Requires a `workflow_migrations` table, a WASM runtime for migration modules, the `durable migrate` CLI command, and careful orchestration to quiesce instances before migration. The reward is the ability to evolve workflows without the operational burden of keeping old code running indefinitely.

### 8.10 Poison pill handling and dead letter queue

**Design:** Each workflow definition has a `failure_policy` that controls what happens when a `DurableCall` fails terminally (after all retries are exhausted). The workflow instance transitions to `dead_letter` status instead of being retried forever.

**Schema additions:**

```sql
ALTER TABLE workflow_defs ADD COLUMN failure_policy JSONB DEFAULT '{
  "max_consecutive_failures": 3,
  "dead_letter_after": "3 failures on the same step",
  "backoff_between_attempts_ms": [1000, 10000, 60000]
}';

ALTER TABLE workflow_instances ADD COLUMN consecutive_failures INT DEFAULT 0;
ALTER TABLE workflow_instances ADD COLUMN last_failed_step INT;
ALTER TABLE workflow_instances ADD COLUMN dead_letter_reason TEXT;
```

The `status` column gains a new value: `'dead_letter'` (in addition to `'ready'`, `'running'`, `'done'`, `'failed'`).

**How it works:**

1. A workflow executing step 4 (`payments.Charge`) fails terminally — retries exhausted, non-retryable error, or host determines it's a permanent failure.
2. The host checks: was this failure on the same step as the last failure? If so, `consecutive_failures++`. If a different step, `consecutive_failures = 1`. The `last_failed_step` is updated.
3. If `consecutive_failures >= max_consecutive_failures`, the host transitions the workflow to `dead_letter` status with a reason:

   ```sql
   UPDATE workflow_instances
   SET status = 'dead_letter',
       dead_letter_reason = $1,
       completed_at = now(),
       assigned_to = NULL
   WHERE id = $2;
   ```

4. Otherwise, the host sets `next_wake_at` using exponential backoff from the failure policy and releases the workflow back to `'ready'`:

   ```sql
   UPDATE workflow_instances
   SET status = 'ready',
       next_wake_at = $backoff_time,
       consecutive_failures = $failures,
       last_failed_step = $step,
       assigned_to = NULL
   WHERE id = $1;
   ```

5. Dead-lettered workflows are visible in the database and can be queried for alerting:

   ```sql
   -- Workflows that need human attention:
   SELECT id, def_name, def_version, dead_letter_reason, completed_at
   FROM workflow_instances
   WHERE status = 'dead_letter'
     AND completed_at > now() - INTERVAL '1 hour'
   ORDER BY completed_at DESC;
   ```

**Retry vs dead-letter distinction:** Retries (8.4) handle transient failures within a single `DurableCall`. The dead letter queue handles WORKFLOW-LEVEL failure — after all per-call retries are exhausted, the workflow itself fails, and if it fails on the same step repeatedly, it goes to DLQ. These are two different layers:

- **Per-call retry:** "The payments API returned a 503. Retry with backoff up to 5 times." (Host handles, no event history entries for attempts.)
- **Workflow-level failure:** "Step 4 has failed terminally 3 times in a row. Something is fundamentally wrong. Stop retrying this workflow and alert a human."

### 8.11 Scheduling and CRON

**The gap:** How do you run a workflow every hour? Every Monday at 9am? The design covers workflows triggered by an API call, but not periodic scheduled workflows.

**What's needed:** A scheduler component (or a separate lightweight process) that inserts workflow instances into the queue on a cron schedule. The schedule definition is stored in PostgreSQL. This is a small addition — the core workflow execution doesn't change.

#### 8.11.1 Scheduler architecture

Two options exist for deploying the scheduler:

**Option A (chosen) — Co-located scheduler goroutine in each worker.** Every worker runs a background goroutine that participates in leader election. The leader scans `workflow_schedules`, fires due schedules, and advances `next_fire_at`. If the leader dies, another worker's scheduler goroutine acquires the lock and takes over within one scheduling cycle. No separate binary, no separate deployment.

**Option B — Standalone `durable-scheduler` binary.** An independent process that does the same thing but runs separately from the worker pool. This is the traditional approach used by job schedulers in microservice architectures.

**Why Option A:**

- **One less binary to deploy.** The scheduler is a goroutine in the existing worker process. There is no separate service to build, configure, monitor, or scale.
- **Leverages existing DB connection pool.** The worker already maintains a pool of PostgreSQL connections. The scheduler shares it — no separate database connection management.
- **Leader election via PostgreSQL advisory locks is battle-tested.** `pg_try_advisory_lock` with a fixed lock ID provides distributed mutual exclusion without external dependencies (no etcd, no ZooKeeper, no Consul). If every worker runs `SELECT pg_try_advisory_lock(42)` in a loop, exactly one succeeds at a time. If that worker crashes, the connection drops and the lock is automatically released — no TTL, no heartbeat, no lease expiry window. Failover is instant.
- **Simpler operations.** Rolling upgrade, scaling, and monitoring are the same as any other worker change. No separate scheduler lifecycle to manage.

The single downside is that the scheduler goroutine shares the worker's CPU and memory. A scheduling cycle is sub-100ms, so the overhead is negligible even at thousands of schedules.

#### 8.11.2 Leader election

The scheduler uses `pg_try_advisory_lock(42)`, where `42` is a fixed application-wide lock ID for the scheduler role.

```go
// Scheduler leader election — runs every ~10 seconds.
func (w *Worker) schedulerCycle(ctx context.Context) {
    // Attempt to acquire the scheduler advisory lock.
    // pg_try_advisory_lock returns immediately (non-blocking):
    //   true  → this worker is now the scheduler leader
    //   false → another worker holds the lock; skip this cycle
    var acquired bool
    err := w.db.QueryRow(ctx, "SELECT pg_try_advisory_lock(42)").Scan(&acquired)
    if err != nil || !acquired {
        return // not the leader
    }
    // Release the lock immediately after the cycle completes.
    defer w.db.Exec(ctx, "SELECT pg_advisory_unlock(42)")

    // ... fire due schedules ...
}
```

**Key properties:**

- **Short-held lock (sub-100ms).** The lock is acquired, the scheduling cycle runs (SELECT + fire + UPDATE next_fire_at), and the lock is released. If the leader crashes mid-cycle, the lock is released automatically when the database connection drops. No stale-leader window.
- **No long-lived lease.** Unlike leader-election schemes that hold a lock for minutes (keeping a lease alive via heartbeats), this design acquires and releases the lock on every cycle. If the leader dies between cycles, the next cycle on any surviving worker acquires the lock instantly.
- **Bully detection is free.** If the leader is slow (e.g., a scheduling cycle takes longer than the 10-second interval), the worker skips the next cycle — it still holds the lock from the previous cycle, and `pg_try_advisory_lock` returns false for all workers. This is correct: the leader is still running, just slow. The backlog is processed on the next cycle.

**Lock ID collision consideration.** The fixed lock ID `42` is global to all advisory locks used by the worker process. If other parts of the system (e.g., queue claiming, signal delivery) also use `pg_try_advisory_lock` with specific IDs, the scheduler's ID `42` must be unique. A convention of reserving lock IDs in a documentation block is sufficient:

```
-- PostgreSQL advisory lock ID reservations
-- 1–9:   Reserved for future use
-- 10–39: Reserved for application-level coordination
-- 42:    Scheduler leader election
```

#### 8.11.3 Schedule table

The `workflow_schedules` table stores each cron schedule as a row. The schema extends the initial sketch with columns for timezone, overlap prevention, timeout, retry policy, and monitoring:

```sql
CREATE TABLE workflow_schedules (
    name                           TEXT PRIMARY KEY,
    def_name                       TEXT NOT NULL,
    def_version                    INT NOT NULL DEFAULT 1,
    cron_expr                      TEXT NOT NULL,            -- standard 5-field cron
    timezone                       TEXT NOT NULL DEFAULT 'UTC',
    input                          JSONB,                    -- static input payload for every run
    next_fire_at                   TIMESTAMPTZ NOT NULL,     -- pre-computed next fire time
    last_fired_at                  TIMESTAMPTZ,              -- when the last run was started
    enabled                        BOOLEAN DEFAULT true,
    created_at                     TIMESTAMPTZ DEFAULT now(),
    updated_at                     TIMESTAMPTZ DEFAULT now(),

    -- Overlap prevention:
    max_concurrent_instances       INT DEFAULT 1,

    -- Retry/timeout for scheduled runs:
    timeout_seconds                INT,                      -- max runtime before cancellation
    retry_policy                   JSONB,                    -- retries for failed scheduled runs

    -- Misfire monitoring:
    misfire_alarm_threshold_seconds INT,                     -- warn if run doesn't start within this window

    -- Catch-up policy after downtime:
    catch_up_policy                TEXT DEFAULT 'immediate'  -- 'immediate', 'skip', or 'backfill_limit:N'
);
```

**Column details:**

| Column | Purpose |
|---|---|
| `name` | Human-readable schedule name (e.g., `"hourly-inventory-sync"`). Acts as the logical identity for idempotency (see 8.11.8). |
| `def_name`, `def_version` | The workflow definition to execute on each scheduled fire. References `workflow_defs(name, version)`. |
| `cron_expr` | Standard 5-field cron expression: `minute hour day-of-month month day-of-week`. Seconds-field and year-field extensions are not supported. |
| `timezone` | The timezone in which the cron expression is evaluated. Defaults to UTC. Supports IANA timezone names (`"America/New_York"`, `"Asia/Tokyo"`). |
| `input` | Static JSON payload injected as the workflow input on every scheduled run. Overridden if `trigger` is used with an ad-hoc payload. |
| `next_fire_at` | Pre-computed timestamp of the next scheduled fire. The scheduler updates this after each successful fire. Set to `infinity` if the cron expression will never fire again (e.g., a one-time schedule that has passed). |
| `last_fired_at` | Timestamp of the most recent fire that started a workflow instance (not necessarily completed). |
| `max_concurrent_instances` | Maximum number of concurrently running instances of this schedule (see 8.11.6). 0 means no limit. |
| `timeout_seconds` | If set, the scheduled workflow instance is automatically cancelled after this many seconds. Implemented via the existing workflow timeout mechanism (section 8.3). |
| `retry_policy` | JSONB blob matching the same `retry_policy` schema used by `DurableCall` (section 8.4): `{"max_attempts": 3, "initial_interval_ms": 1000, "backoff_coefficient": 2.0}`. Applies to the entire scheduled workflow run. |
| `misfire_alarm_threshold_seconds` | See 8.11.11. |
| `catch_up_policy` | See 8.11.9. |

#### 8.11.4 Scheduling cycle

The scheduler leader runs a cycle every ~10 seconds. Each cycle consists of:

```
1. Acquire advisory lock (pg_try_advisory_lock)
2. SELECT due schedules
3. FOR each schedule:
   a. Check overlap prevention
   b. Generate deterministic workflow ID
   c. INSERT workflow_instance
   d. Compute next_fire_at and UPDATE the schedule row
4. Release advisory lock (pg_advisory_unlock)
```

**SQL for the scheduling cycle:**

```sql
-- Step 1: Acquire lock
SELECT pg_try_advisory_lock(42);

-- Step 2: Find due schedules (use SKIP LOCKED for safety even though
-- only the leader reads this table — prevents conflicts with CLI operations)
SELECT * FROM workflow_schedules
WHERE enabled = true
  AND next_fire_at <= now()
ORDER BY next_fire_at
FOR UPDATE SKIP LOCKED;

-- Step 3a: For each schedule, check overlap (Go-side logic):
count := db.QueryRow(`
    SELECT COUNT(*) FROM workflow_instances
    WHERE def_name = $1
      AND status IN ('ready', 'running')
      AND input->>'_scheduled_by' = $2
`, schedule.def_name, schedule.name)

if count >= schedule.max_concurrent_instances {
    // Skip this firing — previous instance is still running.
    // next_fire_at is NOT advanced; the next cycle will retry.
    continue
}

-- Step 3b: Deterministic workflow ID (Go-side):
fireTime := schedule.next_fire_at
wfID := sha256(schedule.name + ":" + fireTime.Format(time.RFC3339Nano))

-- Step 3c: Insert the workflow instance:
INSERT INTO workflow_instances (id, def_name, def_version, status, input)
VALUES ($wfID, $def_name, $def_version, 'ready',
        injectScheduledBy($input, $schedule_name))
ON CONFLICT (id) DO NOTHING;

-- If INSERT affected 0 rows, the workflow ID already exists
-- (previous cycle already fired). Skip to step 4 without updating next_fire_at.

-- Step 3d: Advance next_fire_at:
UPDATE workflow_schedules
SET next_fire_at = cron_next(cron_expr, timezone, now()),
    last_fired_at = now(),
    updated_at = now()
WHERE name = $name
  AND next_fire_at = $old_next_fire_at;  -- skip if advanced by another cycle
```

**The `cron_next()` function.** The scheduler computes the next fire time from the cron expression and current time. This can be implemented in either Go or PostgreSQL:

- **Go (recommended):** Use the `github.com/robfig/cron/v3` library. The scheduler loads all due schedules, iterates them in Go, and computes `cron.Schedule.Next(now)` for each. This is simpler to test, debug, and extend.
- **PostgreSQL:** A PL/pgSQL function `cron_next(cron_expr TEXT, tz TEXT, from TIMESTAMPTZ) → TIMESTAMPTZ` could be used, but maintaining cron-parsing logic in SQL is more complex than in Go.

The scheduler uses the Go approach. The `next_fire_at` UPDATE is a single parameterized query per schedule.

**Schedule that will never fire again.** If `cron_next()` returns a time equal to `next_fire_at` (i.e., the cron expression has no future occurrences), the scheduler sets `next_fire_at = 'infinity'` and logs a warning. The schedule remains in the table (for audit) but is effectively retired. The operator can delete or replace it manually.

#### 8.11.5 Error handling in the scheduling cycle

The scheduling cycle is designed to be resilient to individual schedule failures:

- **Per-schedule transaction isolation.** Each schedule fire is a separate database transaction. If one schedule fails (e.g., the workflow INSERT hits a constraint violation), the others are unaffected. The leader does not abort the entire cycle.
- **Invalid cron expression.** The cron expression is validated at CREATE/UPDATE time (see 8.11.10). If a previously valid expression becomes invalid (e.g., due to a data corruption bug), the scheduler logs an error and skips that schedule, leaving `next_fire_at` unchanged. The schedule is retried on the next cycle.
- **Orphaned schedule (def_name/def_version deleted).** If the referenced `workflow_defs` row no longer exists, the INSERT into `workflow_instances` succeeds (there is no foreign key constraint — by design, to avoid blocking workflow_def cleanup when historic instances exist), but the worker that claims the instance will fail to load the WASM blob. This produces a clear error on the instance and does not affect the scheduler. A background check (or a CLI command) can alert on schedules referencing non-existent defs.
- **Deadline for the cycle.** The scheduling cycle has a configurable deadline (default 30 seconds). If the cycle exceeds this deadline, the leader logs a warning and aborts the remainder. This prevents a stuck schedule (e.g., a slow `cron_next` computation on a huge number of schedules) from blocking the leader indefinitely.

#### 8.11.6 Overlap prevention

Without overlap prevention, a schedule that fires every 5 minutes could start a new workflow instance every 5 minutes even if the previous instance is still running. This is undesirable for schedules that should have at most one active instance at a time (e.g., a nightly reconciliation job that takes 45 minutes).

The `max_concurrent_instances` column controls this:

```go
// Before firing, count in-flight instances for this schedule.
var count int
err := db.QueryRow(ctx, `
    SELECT COUNT(*) FROM workflow_instances
    WHERE def_name = $1
      AND status IN ('ready', 'running')
      AND input->>'_scheduled_by' = $2
`, schedule.def_name, schedule.name).Scan(&count)

if err != nil {
    // Log and skip this schedule — next cycle will retry.
    log.Error("overlap check failed", "schedule", schedule.name, "err", err)
    continue
}
if schedule.max_concurrent_instances > 0 && count >= schedule.max_concurrent_instances {
    log.Debug("skipping scheduled fire — max concurrent reached",
        "schedule", schedule.name, "count", count)
    continue
}
```

**What happens to `next_fire_at` when a fire is skipped?** The scheduler does NOT advance `next_fire_at`. The schedule remains due. On the next cycle, the overlap check runs again. If the previous instance has completed, the count drops below the threshold and the fire proceeds. If it is still running, the fire is skipped again. This means the schedule will fire immediately after the previous instance finishes, not at the next cron-aligned time. A `backfill_limit:1` catch_up policy (see 8.11.9) can be combined with this to limit back-to-back fires after long-running instances.

**`_scheduled_by` injection.** Every workflow instance created by the scheduler has an `_scheduled_by` field injected into its `input` JSONB:

```go
func injectScheduledBy(input json.RawMessage, scheduleName string) json.RawMessage {
    var m map[string]any
    json.Unmarshal(input, &m)
    if m == nil {
        m = make(map[string]any)
    }
    m["_scheduled_by"] = scheduleName
    result, _ := json.Marshal(m)
    return result
}
```

This field is used for two purposes:
1. **Overlap prevention queries** (shown above) — identifying which instances belong to which schedule.
2. **Observability** — a workflow instance created by a schedule is traceable back to its schedule name in the instance's input field. Operators can query `SELECT * FROM workflow_instances WHERE input->>'_scheduled_by' = 'hourly-inventory-sync'`.

#### 8.11.7 Human-readable schedule listing (CLI)

Operators need to manage schedules without editing the database directly. The CLI provides these commands:

```
cleat schedules list                                 -- list all schedules
cleat schedules get <name>                           -- show schedule details
cleat schedules create <name> --def <def>            -- create a new schedule
    --cron "0 * * * *" --tz "America/New_York"
    --input '{"key": "val"}' --max-concurrent 1
    --timeout 3600 --retry '{"max_attempts": 3}'
    --misfire-threshold 300
    --catch-up immediate
cleat schedules update <name> --cron "*/30 * * * *"  -- update schedule fields
cleat schedules delete <name>                         -- remove a schedule
cleat schedules pause <name>                          -- set enabled = false
cleat schedules resume <name>                         -- set enabled = true
cleat schedules trigger <name> [--input '...']        -- fire immediately (ad-hoc)
```

**`cleat schedules trigger`** fires the schedule immediately regardless of `next_fire_at` or `enabled` status. It:
1. Creates a workflow instance with the schedule's `def_name`, `def_version`, and input.
2. Does NOT advance `next_fire_at` (the cron schedule is unaffected).
3. Uses a deterministic workflow ID based on `schedule_name + ":" + "trigger-" + UUID` to allow multiple ad-hoc triggers of the same schedule. This means ad-hoc triggers are idempotent per trigger call.

**`cleat schedules create`** validates the cron expression on the client side before inserting the row. The server also validates on write (see 8.11.10).

#### 8.11.8 Idempotency for scheduled runs

Each scheduled run must be idempotent to prevent duplicate workflow instances. The scheduler uses a deterministic workflow ID based on the schedule name and the scheduled fire time:

```go
// deterministicID computes a UUID-format identifier from schedule + fire time.
func deterministicID(scheduleName string, fireAt time.Time) uuid.UUID {
    raw := sha256.Sum256([]byte(scheduleName + ":" + fireAt.Format(time.RFC3339Nano)))
    // Use the first 16 bytes as a UUIDv4 (set version+variant bits).
    return uuid.Must(uuid.FromBytes(raw[:16]))
}
```

**How idempotency works end-to-end:**

1. The scheduler computes `wfID = deterministicID(schedule.name, schedule.next_fire_at)`.
2. It executes `INSERT INTO workflow_instances (id, ...) VALUES ($wfID, ...)`.
3. If the same schedule+fireAt combination was already inserted (e.g., the leader fired this schedule, then crashed before advancing `next_fire_at`, and a new leader re-reads the same `next_fire_at`), the INSERT hits the `workflow_instances` PRIMARY KEY constraint and does nothing (`ON CONFLICT (id) DO NOTHING`).
4. The scheduler checks `rows_affected`. If 0, the instance already exists — it skips the fire and the `next_fire_at` UPDATE.

This covers the race scenarios:

| Scenario | What happens |
|---|---|
| Leader fires, crashes before advancing `next_fire_at` | New leader re-reads same `next_fire_at`, tries same INSERT, gets 0 rows affected, skips. |
| Leader fires, advances `next_fire_at`, but INSERT not yet committed | `INSERT` and `next_fire_at` UPDATE are in the same transaction. If the transaction is rolled back, neither takes effect — the next leader will fire. |
| Two leaders due to advisory lock race (extremely unlikely) | Both compute the same deterministic workflow ID. The `ON CONFLICT DO NOTHING` ensures exactly one INSERT succeeds. |
| CLI `trigger` fires at the same time as a scheduled fire | The scheduled fire uses `deterministicID(scheduleName, next_fire_at)`. The trigger uses `deterministicID(scheduleName, "trigger-" + UUID)`. They are different IDs. Both instances are correctly created. |

#### 8.11.9 Handling missed fires (catch-up policy)

When the entire system is down (maintenance window, database failover, all workers scaled to zero), scheduled fires are missed. On restart, the scheduler finds schedules where `next_fire_at < now()` — potentially by hours or days. The `catch_up_policy` column determines what happens:

| `catch_up_policy` | Behavior |
|---|---|
| `immediate` (default) | Fire every missed occurrence, one per cycle, until `next_fire_at > now()`. For a schedule that fires every 5 minutes with 2 hours of downtime, this creates 24 workflow instances. |
| `skip` | Advance `next_fire_at` to the next future time without firing any missed occurrences. Equivalent to "just catch up to now." |
| `backfill_limit:N` | Fire up to N missed occurrences, then advance `next_fire_at` past the rest. N=1 means "fire once to catch up, then skip ahead." N=5 means "fire up to 5 missed runs, skip the rest." |

**Implementation in the scheduling cycle:**

```go
// After INSERT succeeds, advance next_fire_at based on catch_up policy.
switch schedule.catchUpPolicy {
case "immediate":
    // Advance one cron step from the current next_fire_at.
    // The next cycle will see the advanced time and fire again if still due.
    newNext := cronNext(schedule.cronExpr, schedule.timezone, schedule.next_fire_at)

case "skip":
    // Jump straight to the first future fire time.
    newNext := cronNext(schedule.cronExpr, schedule.timezone, time.Now())

case backfillLimit:
    // Parse the limit from the policy string ("backfill_limit:5" → 5).
    limit := parseBackfillLimit(schedule.catchUpPolicy)
    if schedule.backfillCount >= limit {
        // Reached the limit — skip remaining missed fires.
        newNext := cronNext(schedule.cronExpr, schedule.timezone, time.Now())
        schedule.backfillCount = 0 // reset for next time
    } else {
        // Advance one step, increment counter.
        newNext := cronNext(schedule.cronExpr, schedule.timezone, schedule.next_fire_at)
        schedule.backfillCount++
    }
}
```

**Backfill tracking.** The `backfill_count` state for `backfill_limit` is tracked per schedule. This can be stored as an additional column or in-memory (since only the leader tracks it, and if the leader changes, the new leader starts at 0 — acceptable because the policy is a best-effort limit, not a hard guarantee):

```sql
ALTER TABLE workflow_schedules ADD COLUMN backfill_count INT DEFAULT 0;
```

Reset to 0 whenever `next_fire_at` catches up to `now()` or the schedule fires without being in backfill mode.

#### 8.11.10 Cron syntax and validation

The scheduler validates cron expressions at schedule creation and update time (in the CLI and API handler).

**Supported cron syntax:**

```
┌───────── minute (0–59)
│ ┌───────── hour (0–23)
│ │ ┌───────── day of month (1–31)
│ │ │ ┌───────── month (1–12)
│ │ │ │ ┌───────── day of week (0–7, where 0 and 7 are Sunday)
│ │ │ │ │
* * * * *
```

**Supported features:**

| Feature | Example | Description |
|---|---|---|
| Wildcard | `*` | Every value |
| Range | `9-17` | Every value within the range |
| Step | `*/15` | Every N values |
| List | `1,3,5` | Multiple values |
| Combination | `0 9-17/2 * * 1-5` | Every 2 hours from 9am to 5pm, weekdays |

**Unsupported syntax:**

- Seconds field (6-field cron) — not supported. Use 5-field and set first field to minute.
- Year field (7-field cron) — not supported.
- Shorthands (`@every`, `@hourly`, `@daily`) — not supported at the database level. The CLI may expand shorthands to 5-field expressions before INSERT.
- Non-standard aliases — not supported. `"0 * * * *"` is valid. `"@hourly"` is not.

**Validation happens at two levels:**

1. **CLI/API (write time).** The `cleat schedules create` and `cleat schedules update` commands validate the cron expression using `robfig/cron`. If the expression does not parse, the command returns an error:

   ```
   $ cleat schedules create hourly-sync --cron "bad-expr" --def nightly-report
   Error: invalid cron expression "bad-expr": Expected exactly 5 fields, got 1
   ```

2. **Leader (read-time defense in depth).** The scheduler parses each schedule's cron expression before computing `next_fire_at`. If parsing fails, the scheduler logs a warning and skips that schedule. This should never happen if validation is working, but it prevents a single corrupt cron expression from crashing the scheduling cycle:

   ```go
   sched, err := cron.ParseStandard(schedule.cronExpr)
   if err != nil {
       log.Warn("skipping schedule with invalid cron expression",
           "schedule", schedule.name, "cron", schedule.cronExpr, "err", err)
       continue
   }
   ```

#### 8.11.11 Misfire monitoring

A "misfire" occurs when a scheduled run does not start within an expected window of its scheduled fire time. This can happen if the scheduler leader is down, if the database is unavailable, or if the scheduling cycle is delayed by a backlog.

The `misfire_alarm_threshold_seconds` column defines the maximum acceptable delay between `next_fire_at` and the actual fire time. If the delay exceeds this threshold, the scheduler emits a metric and optionally logs a warning:

```go
// After a successful fire, check for misfire.
delay := time.Since(schedule.next_fire_at)
if schedule.misfireThreshold > 0 && delay.Seconds() > float64(schedule.misfireThreshold) {
    metrics.Inc("scheduler.misfire", "schedule", schedule.name)
    log.Warn("scheduled fire misfired",
        "schedule", schedule.name,
        "scheduled_at", schedule.next_fire_at,
        "actual_delay_seconds", delay.Seconds(),
        "threshold_seconds", schedule.misfireThreshold)
}
```

**Metrics exposed:**

- `scheduler.misfire{schedule="<name>"}` — counter of misfires (incremented per fire that exceeds threshold)
- `scheduler.fire_delay_seconds{schedule="<name>"}` — histogram of the delay between `next_fire_at` and actual fire time
- `scheduler.leader{worker="<worker_id>"}` — gauge (1 if this worker is the leader, 0 otherwise)
- `scheduler.cycle_duration_seconds` — histogram of scheduling cycle duration

These metrics enable alerting rules:

```
# Alert if any schedule misfires more than 3 times in 10 minutes.
rule: rate(scheduler_misfire[10m]) > 0.005
for: 5m
```

#### 8.11.12 Schedule modification during active fires

A schedule can be modified (updated or deleted) while a workflow instance created by that schedule is still running. The behavior depends on the operation:

- **Pause (`enabled = false`).** The scheduler skips this schedule on the next cycle. Already-running instances continue to completion. The pause takes effect immediately — no in-flight instance is cancelled.
- **Update `cron_expr` or `input`.** The `next_fire_at` is recomputed from the current time (not from the old `next_fire_at`). The next fire will use the new cron expression and input. Already-running instances are unaffected — they retain the input they were started with.
- **Update `def_name` or `def_version`.** The next fire will start the new workflow definition. Already-running instances continue with the old definition.
- **Delete.** The schedule row is removed. Already-running instances are unaffected. To prevent accidental deletion of active schedules, the CLI requires a `--force` flag if any instances with `_scheduled_by = <name>` are still in `ready` or `running` status.

#### 8.11.13 Database indexes

```sql
-- Primary lookup for the scheduling cycle:
CREATE INDEX idx_workflow_schedules_next_fire_enabled
    ON workflow_schedules (next_fire_at)
    WHERE enabled = true;

-- Overlap prevention query:
CREATE INDEX idx_workflow_instances_scheduled_by
    ON workflow_instances ((input->>'_scheduled_by'))
    WHERE input->>'_scheduled_by' IS NOT NULL;

-- Lookup by schedule name (CLI get/update/delete):
-- Already covered by the PRIMARY KEY on name.
```

#### 8.11.14 Summary

The scheduler is a lightweight addition to the existing worker architecture. It reuses the worker's PostgreSQL connection pool, requires no new infrastructure, and adds approximately 300 lines of Go code for the goroutine loop, leader election, cron parsing, and schedule firing logic. The `workflow_schedules` table is small (hundreds to low thousands of rows), making the scheduling cycle fast even without aggressive indexing. The design's key guarantees are:

- **At-most-once per schedule+time:** Deterministic workflow IDs plus `ON CONFLICT DO NOTHING` prevent duplicate runs.
- **Leader failover in <10 seconds:** Advisory lock release on connection drop means the next cycle on any surviving worker picks up.
- **No interference with existing workflows:** The scheduler only writes to `workflow_instances` and `workflow_schedules`. It does not touch `event_history`, `workflow_defs`, or any worker-internal state.
- **Observable by default:** Every scheduled run is traceable via the `_scheduled_by` field in the workflow input. Misfire metrics and delay histograms provide visibility into scheduler health.

### 8.12 Transformer — Go source to WASM module

**Design:** The transformer reads the user's Go package, analyzes the call graph to find all functions in the durable closure, verifies that `*HostCalls` is threaded correctly, generates WASM binding code, and compiles the result to a WASM binary ready for database storage. The user's source files are never modified.

#### 8.12.1 Pipeline overview

```
User's Go source           Transformer                          Output
─────────────────          ───────────                          ──────
workflows/
├── order.go          →    1. Parse (go/parser)            →   workflows/
├── payment.go       →    2. Type-check (go/types)         →   ├── order.go (unchanged)
├── shipping.go      →    3. Call graph analysis           →   ├── payment.go (unchanged)
└── types.go         →    4. Durable closure computation   →   ├── shipping.go (unchanged)
                            5. HostCalls threading check         ├── types.go (unchanged)
                            6. Generate WASM imports             ├── gen_wasm_imports.go
                            7. Generate host adapter             ├── gen_host_adapter.go
                            8. Generate WASM exports             ├── gen_wasm_exports.go
                            9. Compile (go build -wasm)          └── place_order.wasm
```

#### 8.12.2 Step-by-step

**Step 1–2: Parse and type-check.** The transformer loads the package using `golang.org/x/tools/go/packages`, which runs both `go/parser` and `go/types`:

```go
cfg := &packages.Config{
    Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
          packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax,
}
pkgs, err := packages.Load(cfg, "./workflows")
```

This gives us the AST (`*ast.File`), type information (`*types.Info`), and the full set of source files.

**Step 3: Call graph analysis.** For each function in the package, the transformer walks its AST body and collects all function calls:

```go
type CallGraph struct {
    Funcs      map[string]*types.Func           // all functions in the package
    Calls      map[string][]string               // caller → list of callees
    CalledBy   map[string][]string               // callee → list of callers
}
```

The call graph is built by visiting every `*ast.CallExpr` in the package and resolving the callee using type information. Calls to external packages (like `fmt.Sprintf`) are included but marked as external — they don't participate in the durable closure unless they transitively call a durable leaf.

**Step 4: Durable closure computation.** A function is a "durable leaf" if it directly calls any of the `HostCalls` functions that require durability:

```go
var durableLeafFunctions = map[string]bool{
    "DurableCall":         true,
    "DurableSleep":        true,
    "DurableAwaitSignals": true,
    "DurableDefer":        true,
}
```

A function is in the "durable closure" if it is a durable leaf, or if it calls any function in the durable closure. The transformer computes this by iterating the call graph to a fixed point.

**Step 5: HostCalls threading verification.** For every function in the durable closure, the transformer verifies that `*cleat.HostCalls` is available. The acceptable patterns are:

1. **Direct parameter:** `func foo(h *cleat.HostCalls, ...)` — `h` is the first parameter.
2. **Received from a caller:** `func bar(args...)` calls `foo(h, args...)` where `h` comes from bar's own parameter or a local variable derived from it.

The transformer tracks the flow of every `*HostCalls` value through assignments and call sites. If a function in the durable closure has no path to a `*HostCalls`, the transformer reports an error:

```
error: shipping.go:15: function createShipment is in the durable closure
       (it calls DurableCall) but has no access to *cleat.HostCalls.
       Add 'h *cleat.HostCalls' as the first parameter.
```

If a function is NOT in the durable closure (pure computation, no API calls), it does NOT need the parameter. Pure functions pass through the transformer unchanged.

**Step 6: Generate WASM imports.** For each `HostCalls` function that the durable closure actually uses, the transformer generates a `//go:wasmimport` stub. These are written to `gen_wasm_imports.go`:

```go
//go:build wasip1

package main

import "unsafe"

//go:wasmimport env durable_call
func durableCallImport(
    svcPtr unsafe.Pointer, svcLen uint32,
    opPtr unsafe.Pointer, opLen uint32,
    reqPtr unsafe.Pointer, reqLen uint32,
) (respPtr unsafe.Pointer, respLen uint32, errCode int32)

//go:wasmimport env durable_sleep
func durableSleepImport(durationMs int64)

//go:wasmimport env durable_await_signals
func durableAwaitSignalsImport(
    namesPtr unsafe.Pointer, namesLen uint32,
    timeoutMs int64,
) (sigNamePtr unsafe.Pointer, sigNameLen uint32, payloadPtr unsafe.Pointer, payloadLen uint32, timedOut int32)

// ... etc for durable_log, durable_now, durable_poll_cancellation, durable_defer_execute
```

The function set is determined by analysis — if the workflow never calls `DurableSleep`, the `durable_sleep` import is omitted.

**Step 7: Generate host adapter.** The host adapter bridges the user's `HostCalls` to the low-level WASM imports. It converts Go strings to WASM linear memory pointers and back. Written to `gen_host_adapter.go`:

```go
//go:build wasip1

package main

import (
    "durable" // the user's durable package (HostCalls, etc.)
    "encoding/json"
    "unsafe"
)

// makeHostCalls constructs a HostCalls backed by the WASM host imports.
func makeHostCalls(mem *wasmMemory) *cleat.HostCalls {
    return &durable.HostCalls{
        DurableCall: func(service, op, request string) (string, error) {
            svcPtr, svcLen := mem.allocString(service)
            opPtr, opLen := mem.allocString(op)
            reqPtr, reqLen := mem.allocString(request)

            respPtr, respLen, errCode := durableCallImport(svcPtr, svcLen, opPtr, opLen, reqPtr, reqLen)
            if errCode != 0 {
                return "", mem.readError(errCode)
            }
            return mem.readString(respPtr, respLen), nil
        },
        DurableSleep: func(durationMs int64) {
            durableSleepImport(durationMs)
        },
        DurableAwaitSignals: func(signalNames []string, timeoutMs int64) (string, string, bool, error) {
            namesJSON, _ := json.Marshal(signalNames)
            namesPtr, namesLen := mem.allocBytes(namesJSON)

            sigNamePtr, sigNameLen, payloadPtr, payloadLen, timedOut :=
                durableAwaitSignalsImport(namesPtr, namesLen, timeoutMs)

            if timedOut != 0 {
                return "", "", true, nil
            }
            return mem.readString(sigNamePtr, sigNameLen),
                   mem.readString(payloadPtr, payloadLen),
                   false, nil
        },
        DurableDefer: func(fn func() error) {
            // Store fn in a side table with an ID.
            // Call durable_defer_register(deferID).
            // The host calls back into the WASM module to execute the defer.
        },
        DurableLog: func(msg string) {
            ptr, l := mem.allocString(msg)
            durableLogImport(ptr, l)
        },
        Now: func() int64 {
            return durableNowImport()
        },
    }
}
```

The `wasmMemory` type manages the WASM module's linear memory — allocation, deallocation, and string/bytes conversion:

```go
type wasmMemory struct {
    allocFn   func(size uint32) unsafe.Pointer // calls malloc in WASM
    freeFn    func(ptr unsafe.Pointer)         // calls free in WASM
}
```

**Step 8: Generate WASM exports.** For each workflow entry point (exported function whose first parameter is `*cleat.HostCalls`), the transformer generates a `//go:wasmexport` function. These are the functions the host runtime calls to start or resume a workflow:

```go
//go:wasmexport place_order
func placeOrder(argsPtr unsafe.Pointer, argsLen uint32) (resultPtr unsafe.Pointer, resultLen uint32, errCode int32) {
    mem := getMemory()

    // 1. Deserialize arguments from WASM memory.
    argsJSON := mem.readString(argsPtr, argsLen)
    var args struct {
        UserID string           `json:"user_id"`
        Cart   []workflows.CartItem `json:"cart"`
    }
    if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
        return mem.writeError(err)
    }

    // 2. Build the HostCalls adapter.
    h := makeHostCalls(mem)

    // 3. Set up the defer execution mechanism.
    h.DeferRunner = newDeferRunner(mem)

    // 4. Call the user's workflow function.
    trackingID, err := workflows.PlaceOrder(h, args.UserID, args.Cart)

    // 5. Execute any registered defers if the workflow is exiting.
    h.DeferRunner.executeAll(err != nil)

    // 6. Serialize the result back to WASM memory.
    if err != nil {
        return mem.writeError(err)
    }
    return mem.writeJSON(trackingID)
}

//go:wasmexport place_order_query_current_state
func placeOrderQueryCurrentState(argsPtr unsafe.Pointer, argsLen uint32) (resultPtr unsafe.Pointer, resultLen uint32) {
    // Query handler — read-only, not recorded in event history.
    // ...
}
```

Multiple exports can be generated from a single package — one `//go:wasmexport` per workflow entry point, plus one per query handler if queries are defined.

**Step 9: Compile to WASM.** The transformer invokes the Go compiler on the combined source (user files + generated files):

```
GOOS=wasip1 GOARCH=wasm go build -o place_order.wasm .
```

The result is a single WASM binary containing the user's workflow code, the generated imports/adapter/exports, and the Go runtime (minimal for `wasip1`). The binary is ready for `INSERT INTO workflow_defs (name, version, wasm_bytes)`.

For smaller binaries, the transformer supports a `--tinygo` flag that uses tinygo instead of standard Go:

```
tinygo build -target=wasip1 -o place_order.wasm .
```

This produces ~50–200 KB binaries instead of ~1–2 MB.

#### 8.12.3 What the transformer does NOT do

- **Modify user source files.** All generated code goes into `gen_*.go` files. User code is never rewritten.
- **Change function signatures.** The transformer verifies that `*HostCalls` is threaded correctly; it does not add or remove parameters.
- **Handle arbitrary Go.** The transformer rejects constructs that WASM or the durability model doesn't support: goroutines within workflows, channel operations, `time.Now()` (use `h.Now()` instead), `math/rand` (use `h.Random()` if needed), map iteration in contexts where order matters.
- **Handle external packages.** The transformer analyzes only the target package. If a workflow calls into a library package, the library must also be structured to pass `*HostCalls` if it's in the durable closure.

#### 8.12.3a Cross-package and multi-module analysis

The transformer as described above analyzes a single target package, but real-world workflows span multiple packages: the workflow entry point calls helpers in `internal/validation`, which calls domain logic in `internal/pricing`, which calls `h.DurableCall` in `internal/clients`.

**Package patterns.** The transformer accepts a package pattern and analyzes all matching packages as a single compilation unit:

```
cleat build ./workflows/...
```

This loads all packages matching `./workflows/...`, building a unified call graph across package boundaries. The durable closure is computed on the union: any function in any analyzed package that transitively reaches a host function is subject to the `HostCalls` threading requirement and the WASM restrictions.

**Dependency restrictions.** When the transformer resolves a call to an external package (one outside the analyzed pattern), it must determine whether that package can compile to WASM. The following transitively imported packages cause the build to fail:

| Transitive dependency        | Reason                              |
|------------------------------|-------------------------------------|
| `net`                        | Links TCP stack; unsupported in WASM|
| `net/http`                   | Builds on `net`; unsupported        |
| `os/exec`                    | Process execution; unsupported      |
| `syscall`                    | OS syscalls; unsupported in `wasip1`|
| `reflect`                    | Limited WASM support; unpredictable |

If a workflow's dependency tree contains any of these at any depth, the compilation step fails. The transformer detects this before invoking the compiler by analyzing the `go/packages` metadata, and produces a clear error:

```
error: package workflows imports internal/clients, which imports
       github.com/someone/http-client (imports net/http).
       net/http is not available in WASM targets.
       Use h.DurableCall() instead, or move this dependency outside
       workflow functions.
```

This means that **you cannot use arbitrary Go libraries inside workflow functions**. A library that makes HTTP calls, accesses the filesystem, or uses reflection for control flow is incompatible — even if the workflow itself never directly calls those functions.

**WASM-compatible library concept.** The system defines a set of pre-approved, WASM-compilable Go packages for common workflow needs:

| Category                    | Approved approach                           |
|-----------------------------|---------------------------------------------|
| JSON manipulation           | `encoding/json` (standard library; works)   |
| String processing           | `strings`, `fmt`, `regexp` (standard lib)   |
| Basic math                  | `math` (avoid floating-point for branching) |
| Date handling               | `h.Now()` backed by host function           |
| UUID generation             | Use `h.DurableCall(...)` or pass from caller|
| Data validation             | Pure validation functions (no I/O)          |
| Serialization               | `encoding/json`, custom pure-Go serializers |

Libraries outside this set must be verified manually for WASM compatibility. A `durable check-deps` command analyzes whether a given package's full transitive dependency tree is WASM-compatible, without invoking the full transformer:

```
$ durable check-deps ./workflows/...
  Checking ./workflows (and 5 matching sub-packages)...
    ✓ encoding/json (standard library, OK)
    ✓ github.com/google/uuid (v1.6.0) -- check: no prohibited imports, OK
    ✗ github.com/segmentio/kafka-go (v0.4.47) -- imports net, FAIL
    ✗ internal/clients/payment.go -- imports net/http transitively, FAIL
  Result: 12 packages OK, 2 packages FAIL
```

This is suitable for CI pipelines: it runs in seconds (no WASM compilation) and fails a pull request if a workflow's dependency tree becomes incompatible.

#### 8.12.4 Developer workflow

```
$ cleat build ./workflows/
  Analyzing package workflows...
  Found 12 functions, 3 entry points, 8 in durable closure.
  Durable leaf functions: catalogLookup, reserveInventory, chargeCustomer,
                           createShipment, releaseReservation, refundPayment,
                           notifyCustomer, getDefaultPaymentMethod
  Verifying HostCalls threading... OK
  Generating WASM imports (7 host functions used)... OK
  Generating host adapter... OK
  Generating WASM exports (3 entry points + 2 query handlers)... OK
  Compiling WASM module (tinygo)... OK
  Wrote place_order.wasm (156 KB)

$ cleat deploy ./workflows/place_order.wasm
  Deployed PlaceOrder v7 (156 KB)
  → INSERT INTO workflow_defs (name, version, wasm_bytes) VALUES (...)

$ durable test ./workflows/
  Testing PlaceOrder...
    PASS TestPlaceOrder_Success (0.003s)
    PASS TestPlaceOrder_PaymentFails (0.002s)
    PASS TestPlaceOrder_Cancelled (0.002s)
    PASS TestApprovalWorkflow_Timeout (0.001s)
  4 tests passed, 0 failed
```

#### 8.12.5 Implementation plan

| Phase | What | Effort |
|---|---|---|
| **1. Parser + call graph** | Load packages, build call graph, compute durable closure | 2 weeks |
| **2. HostCalls verification** | Track `*HostCalls` flow through the call graph, report errors | 1 week |
| **3. WASM import generation** | Generate `//go:wasmimport` stubs for used host functions | 1 week |
| **4. Host adapter generation** | Generate the adapter that bridges HostCalls to WASM imports, including wasmMemory allocator | 2 weeks |
| **5. WASM export generation** | Generate `//go:wasmexport` entry points with arg serialization | 1 week |
| **6. Defer execution engine** | Side-table for deferred functions, host callback for defer execution | 2 weeks |
| **7. Compilation + CLI** | `go build`/`tinygo build` integration, `cleat build/deploy/test` CLI | 2 weeks |
| **8. Validation rules** | Reject unsupported Go constructs, produce clear error messages | 1 week |
| **Total** | | **~12 weeks** |

This is a 3-month engineering effort for a production-quality transformer that handles real-world workflow code. The largest subtasks are the host adapter code generation (the string/bytes conversion across the WASM boundary must be correct and fast) and the defer execution engine (which requires the host to call back into the WASM module to execute deferred functions).

### 8.13 Testing framework

**Design:** A `TestHost` that implements the full `HostCalls` interface with mock control, call recording, and assertion helpers. Tests run synchronously with no infrastructure — no PostgreSQL, no WASM compilation, no workers. The workflow code runs as ordinary Go functions.

The `HostCalls` struct with function fields makes this natural: the test host provides function implementations that return mock data and record every call.

**Core test host API:**

```go
package durable

import (
    "errors"
    "fmt"
    "sync"
    "testing"
    "time"
)

// ErrCancelled is returned by DurableCall/DurableSleep/DurableAwaitSignals
// when the workflow has been cancelled.
var ErrCancelled = errors.New("workflow cancelled")

// TestHost implements HostCalls for unit testing workflows.
// It runs synchronously, requires no infrastructure, and provides
// full control over mock responses, signals, timers, and cancellation.
type TestHost struct {
    t           *testing.T
    mu          sync.Mutex
    stepCounter int
    calls       []RecordedCall
    mocks       map[string]*MockQueue   // key: "service.operation"
    mockDefault MockResponse            // returned when no mock queued (default: fatal)
    signals     map[string]string       // signalName → payload (pre-delivered)
    timeouts    map[string]bool         // signalNames that should time out
    cancelled   bool
    cancelReason string
    currentTime time.Time
    sleepLog    []int64                 // durations passed to DurableSleep

    // Retry simulation: if non-zero, the host simulates N-1 failures before
    // returning the mock response. Set to 0 (default) for instant responses.
    simulateRetries int
}

type MockResponse struct {
    Response string
    Error    error
}

// MockQueue is a FIFO queue of mock responses for a specific service.operation.
// Successive calls consume from the front. When empty, the default is used.
type MockQueue struct {
    Responses []MockResponse
}

type RecordedCall struct {
    Step         int
    Service      string
    Operation    string
    RequestJSON  string
    ResponseJSON string
    Error        error
}

type ExpectedCall struct {
    Service   string
    Operation string
    // RequestContains checks that the request JSON contains this substring.
    // Empty = no check.
    RequestContains string
}

// NewTestHost creates a test host. The default behavior for unmocked calls
// is to fail the test immediately (fail-fast).
func NewTestHost(t *testing.T) *TestHost {
    return &TestHost{
        t:           t,
        mocks:       make(map[string]*MockQueue),
        signals:     make(map[string]string),
        timeouts:    make(map[string]bool),
        currentTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
    }
}

// --- Mock configuration ---

// MockCall registers a mock response for the next call to service.operation.
// Multiple calls stack — the first call consumes the first mock, etc.
func (h *TestHost) MockCall(service, operation, response string, err error) {
    h.mu.Lock()
    defer h.mu.Unlock()
    key := service + "." + operation
    if h.mocks[key] == nil {
        h.mocks[key] = &MockQueue{}
    }
    h.mocks[key].Responses = append(h.mocks[key].Responses, MockResponse{
        Response: response, Error: err,
    })
}

// --- Signal simulation ---

// DeliverSignal simulates an external signal arriving before the workflow starts.
// For signals that arrive mid-workflow, use DeliverSignalAtStep.
func (h *TestHost) DeliverSignal(name, payload string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    h.signals[name] = payload
}

// SimulateTimeout configures DurableAwaitSignals to time out for the given
// signal names rather than returning a signal.
func (h *TestHost) SimulateTimeout(signalNames ...string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    for _, name := range signalNames {
        h.timeouts[name] = true
    }
}

// --- Cancellation simulation ---

// Cancel simulates an external cancellation request. The next DurableCall,
// DurableSleep, or DurableAwaitSignals will return ErrCancelled.
func (h *TestHost) Cancel(reason string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    h.cancelled = true
    h.cancelReason = reason
}

// --- Retry simulation ---

// SetSimulateRetries configures the host to simulate N failed attempts before
// returning the mock response. Each failed attempt increments the call counter
// but returns a transient error. The final attempt returns the mock.
// Set to 0 (default) for no retry simulation.
func (h *TestHost) SetSimulateRetries(n int) {
    h.simulateRetries = n
}

// --- Time control ---

// AdvanceTime advances the test host's clock. This affects DurableSleep
// (which appears to complete instantly when time is advanced past the
// sleep duration) and Now().
func (h *TestHost) AdvanceTime(d time.Duration) {
    h.currentTime = h.currentTime.Add(d)
}

// --- HostCalls implementation ---

func (h *TestHost) DurableCall(service, operation, requestJSON string) (string, error) {
    h.mu.Lock()
    defer h.mu.Unlock()

    // 1. Check cancellation.
    if h.cancelled {
        h.recordCall(service, operation, requestJSON, "", ErrCancelled)
        return "", fmt.Errorf("%w: %s", ErrCancelled, h.cancelReason)
    }

    // 2. Simulate retries (host-level, not recorded as separate steps).
    key := service + "." + operation
    for attempt := 0; attempt < h.simulateRetries; attempt++ {
        // Transient failure — not recorded in calls log (matches production behavior).
    }

    // 3. Consume mock response.
    mock, ok := h.popMock(key)
    if !ok {
        h.t.Fatalf("unexpected DurableCall: %s.%s(%s) — no mock registered. "+
            "Use h.MockCall(\"%s\", \"%s\", ...) to register one.",
            service, operation, truncate(requestJSON, 60), service, operation)
    }

    h.recordCall(service, operation, requestJSON, mock.Response, mock.Error)
    if mock.Error != nil {
        return "", mock.Error
    }
    return mock.Response, nil
}

func (h *TestHost) DurableSleep(durationMs int64) {
    h.mu.Lock()
    defer h.mu.Unlock()
    h.sleepLog = append(h.sleepLog, durationMs)

    if h.cancelled {
        // Don't record as a call — cancellation interrupts sleep
        return
    }
    // In test mode, sleep returns immediately. Tests use AdvanceTime()
    // to verify timeout behavior in DurableAwaitSignals instead.
}

func (h *TestHost) DurableAwaitSignals(signalNames []string, timeoutMs int64) (string, string, bool, error) {
    h.mu.Lock()
    defer h.mu.Unlock()

    if h.cancelled {
        return "", "", false, fmt.Errorf("%w: %s", ErrCancelled, h.cancelReason)
    }

    // Check for pre-delivered signals.
    for _, name := range signalNames {
        if payload, ok := h.signals[name]; ok {
            delete(h.signals, name)
            return name, payload, false, nil
        }
    }

    // Check for simulated timeout.
    for _, name := range signalNames {
        if h.timeouts[name] {
            return "", "", true, nil
        }
    }

    // No signal and no timeout configured — the test didn't set up either.
    h.t.Fatalf("DurableAwaitSignals(%v, %d) — no signal delivered and no timeout configured. "+
        "Use h.DeliverSignal(name, payload) or h.SimulateTimeout(name).",
        signalNames, timeoutMs)
    return "", "", false, nil
}

func (h *TestHost) DurablePollCancellation() (bool, string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    return h.cancelled, h.cancelReason
}

func (h *TestHost) DurablePollSignal(signalNames []string) (string, string, error) {
    h.mu.Lock()
    defer h.mu.Unlock()
    for _, name := range signalNames {
        if payload, ok := h.signals[name]; ok {
            delete(h.signals, name)
            return name, payload, nil
        }
    }
    return "", "", nil
}

func (h *TestHost) DurableLog(message string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    h.calls = append(h.calls, RecordedCall{
        Step: h.stepCounter, Service: "_log", Operation: "log",
        RequestJSON: message,
    })
}

func (h *TestHost) Now() int64 {
    h.mu.Lock()
    defer h.mu.Unlock()
    return h.currentTime.UnixMilli()
}

// --- Internal helpers ---

func (h *TestHost) popMock(key string) (MockResponse, bool) {
    q, ok := h.mocks[key]
    if !ok || len(q.Responses) == 0 {
        return MockResponse{}, false
    }
    m := q.Responses[0]
    q.Responses = q.Responses[1:]
    return m, true
}

func (h *TestHost) recordCall(service, operation, request, response string, err error) {
    h.calls = append(h.calls, RecordedCall{
        Step:         h.stepCounter,
        Service:      service,
        Operation:    operation,
        RequestJSON:  request,
        ResponseJSON: response,
        Error:        err,
    })
    h.stepCounter++
}

// --- Assertions ---

// AssertCalls verifies that the recorded calls match the expected sequence.
func (h *TestHost) AssertCalls(expected []ExpectedCall) {
    h.mu.Lock()
    defer h.mu.Unlock()

    if len(h.calls) != len(expected) {
        h.t.Errorf("call count mismatch: got %d, want %d\nCalls:\n%s\nExpected:\n%s",
            len(h.calls), len(expected), h.formatCalls(), h.formatExpected(expected))
        return
    }
    for i, ec := range expected {
        actual := h.calls[i]
        if actual.Service != ec.Service || actual.Operation != ec.Operation {
            h.t.Errorf("call %d mismatch: got %s.%s, want %s.%s",
                i, actual.Service, actual.Operation, ec.Service, ec.Operation)
        }
        if ec.RequestContains != "" && !contains(actual.RequestJSON, ec.RequestContains) {
            h.t.Errorf("call %d (%s.%s): request does not contain %q\ngot: %s",
                i, ec.Service, ec.Operation, ec.RequestContains, actual.RequestJSON)
        }
    }
}

// AssertCalled verifies a specific service.operation was called at least once.
func (h *TestHost) AssertCalled(service, operation string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    for _, c := range h.calls {
        if c.Service == service && c.Operation == operation {
            return
        }
    }
    h.t.Errorf("expected call %s.%s was not made\nCalls:\n%s", service, operation, h.formatCalls())
}

// AssertNotCalled verifies a specific service.operation was never called.
func (h *TestHost) AssertNotCalled(service, operation string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    for _, c := range h.calls {
        if c.Service == service && c.Operation == operation {
            h.t.Errorf("unexpected call %s.%s was made\nCalls:\n%s", service, operation, h.formatCalls())
            return
        }
    }
}

// AssertCallCount verifies the exact number of times a service.operation was called.
func (h *TestHost) AssertCallCount(service, operation string, want int) {
    h.mu.Lock()
    defer h.mu.Unlock()
    count := 0
    for _, c := range h.calls {
        if c.Service == service && c.Operation == operation {
            count++
        }
    }
    if count != want {
        h.t.Errorf("call count for %s.%s: got %d, want %d", service, operation, count, want)
    }
}

// AssertSleeps verifies the sequence of sleep durations (in milliseconds).
func (h *TestHost) AssertSleeps(expectedMs []int64) {
    h.mu.Lock()
    defer h.mu.Unlock()
    if len(h.sleepLog) != len(expectedMs) {
        h.t.Errorf("sleep count mismatch: got %d, want %d", len(h.sleepLog), len(expectedMs))
        return
    }
    for i, got := range h.sleepLog {
        if got != expectedMs[i] {
            h.t.Errorf("sleep %d: got %dms, want %dms", i, got, expectedMs[i])
        }
    }
}

func (h *TestHost) formatCalls() string {
    var s string
    for _, c := range h.calls {
       	errStr := ""
        if c.Error != nil {
            errStr = " (error: " + c.Error.Error() + ")"
        }
        s += fmt.Sprintf("  %s.%s(%s) → %s%s\n",
            c.Service, c.Operation, truncate(c.RequestJSON, 40), truncate(c.ResponseJSON, 40), errStr)
    }
    return s
}

func (h *TestHost) formatExpected(expected []ExpectedCall) string {
    var s string
    for _, e := range expected {
        s += fmt.Sprintf("  %s.%s\n", e.Service, e.Operation)
    }
    return s
}

func truncate(s string, n int) string {
    if len(s) > n {
        return s[:n] + "..."
    }
    return s
}

func contains(s, substr string) bool {
    return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
    for i := 0; i <= len(s)-len(substr); i++ {
        if s[i:i+len(substr)] == substr {
            return true
        }
    }
    return false
}
```

**Usage examples:**

**Happy path:**
```go
func TestPlaceOrder_Success(t *testing.T) {
    h := durable.NewTestHost(t)

    h.MockCall("catalog", "LookupItem", `{"found":true}`, nil)
    h.MockCall("catalog", "LookupItem", `{"found":true}`, nil)
    h.MockCall("inventory", "Reserve", `{"reservation_id":"resv_123"}`, nil)
    h.MockCall("payments", "GetDefaultMethod", `{"token":"tok_456"}`, nil)
    h.MockCall("payments", "Charge", `{"charge_id":"chg_789"}`, nil)
    h.MockCall("shipping", "CreateShipment", `{"tracking_id":"TRACK-999"}`, nil)
    h.MockCall("notifications", "SendEmail", `{"status":"sent"}`, nil)

    trackingID, err := PlaceOrder(h, "user-1", []CartItem{
        {SKU: "ABC-123", Quantity: 2}, {SKU: "XYZ-789", Quantity: 1},
    })

    assert.NoError(t, err)
    assert.Equal(t, "TRACK-999", trackingID)

    h.AssertCalls(t, []durable.ExpectedCall{
        {Service: "catalog", Operation: "LookupItem", RequestContains: "ABC-123"},
        {Service: "catalog", Operation: "LookupItem", RequestContains: "XYZ-789"},
        {Service: "inventory", Operation: "Reserve"},
        {Service: "payments", Operation: "GetDefaultMethod"},
        {Service: "payments", Operation: "Charge"},
        {Service: "shipping", Operation: "CreateShipment"},
        {Service: "notifications", Operation: "SendEmail"},
    })
}
```

**Compensation path:**
```go
func TestPlaceOrder_PaymentFails_CompensatesInventory(t *testing.T) {
    h := durable.NewTestHost(t)

    h.MockCall("catalog", "LookupItem", `{"found":true}`, nil)
    h.MockCall("inventory", "Reserve", `{"reservation_id":"resv_123"}`, nil)
    h.MockCall("payments", "GetDefaultMethod", `{"token":"tok_456"}`, nil)
    h.MockCall("payments", "Charge", "", errors.New("insufficient funds"))
    h.MockCall("inventory", "Release", `{"status":"released"}`, nil) // compensation

    _, err := PlaceOrder(h, "user-1", []CartItem{{SKU: "ABC-123", Quantity: 1}})

    assert.Error(t, err)
    assert.Contains(t, err.Error(), "payment failed")

    // Verify compensation ran and shipping never happened.
    h.AssertCalled(t, "inventory", "Release")
    h.AssertNotCalled(t, "shipping", "CreateShipment")
    h.AssertNotCalled(t, "notifications", "SendEmail")
}
```

**Signal handling:**
```go
func TestApprovalWorkflow_Approved(t *testing.T) {
    h := durable.NewTestHost(t)

    h.MockCall("orders", "SubmitForApproval", `{"status":"pending"}`, nil)
    h.DeliverSignal("approved", `{"approved_by":"manager@example.com"}`)
    h.MockCall("orders", "Fulfill", `{"status":"fulfilled"}`, nil)

    result, err := ApprovalWorkflow(h, "order-123")

    assert.NoError(t, err)
    assert.Equal(t, "APPROVED_AND_FULFILLED", result)
}

func TestApprovalWorkflow_Timeout(t *testing.T) {
    h := durable.NewTestHost(t)

    h.MockCall("orders", "SubmitForApproval", `{"status":"pending"}`, nil)
    h.SimulateTimeout("approved", "rejected")

    result, err := ApprovalWorkflow(h, "order-123")

    assert.NoError(t, err)
    assert.Equal(t, "EXPIRED", result)

    // Fulfill was never called.
    h.AssertNotCalled(t, "orders", "Fulfill")
}
```

**Cancellation with compensation:**
```go
func TestOrderWorkflow_CancelledMidFlight(t *testing.T) {
    h := durable.NewTestHost(t)

    h.MockCall("inventory", "Reserve", `{"reservation_id":"resv_123"}`, nil)
    h.MockCall("payments", "Charge", `{"charge_id":"chg_456"}`, nil)

    // Cancel — the next DurableCall returns ErrCancelled.
    h.Cancel("customer called support")

    // Compensation mocks (the workflow should call these before returning).
    h.MockCall("payments", "Refund", `{"status":"refunded"}`, nil)
    h.MockCall("inventory", "Release", `{"status":"released"}`, nil)

    _, err := OrderWorkflow(h, "user-1", testCart)

    assert.True(t, errors.Is(err, durable.ErrCancelled))
    h.AssertCalled(t, "payments", "Refund")
    h.AssertCalled(t, "inventory", "Release")
    h.AssertNotCalled(t, "shipping", "CreateShipment")
}
```

**Retry behavior verification:**
```go
func TestWorkflow_RetriesBeforeSuccess(t *testing.T) {
    h := durable.NewTestHost(t)

    h.SetSimulateRetries(3) // fail 3 times, succeed on 4th
    h.MockCall("payments", "Charge", `{"charge_id":"chg_789"}`, nil)

    _, err := ChargeCustomerWorkflow(h, "user-1", 3299)

    assert.NoError(t, err)
    // The call is recorded once (the successful attempt), not 4 times.
    // This matches production behavior where retries don't create event history entries.
    h.AssertCallCount(t, "payments", "Charge", 1)
}
```

**Design principles:**
1. **Same code as production.** The workflow function takes a `*cleat.HostCalls` — the same struct whether it's the test host or the production WASM adapter. No build tags, no test-only code paths.
2. **Fail-fast by default.** Unmocked calls immediately fail the test with a clear message showing what to mock. This prevents tests that accidentally call real services.
3. **Ordered mocks.** Multiple calls to the same `service.operation` consume mocks in FIFO order. This naturally tests sequences where the same API returns different results (e.g., polling: "pending" → "pending" → "complete").
4. **Separate concerns.** Mock configuration (MockCall, DeliverSignal, Cancel) is separate from assertions (AssertCalls, AssertCalled, AssertNotCalled). Tests read as "given these mocks, when this workflow runs, then expect these calls."
5. **Retry transparency.** Retries happen at the host level (matching production), so test assertions see the final result, not intermediate attempts. Use `SetSimulateRetries` to verify retry behavior.
6. **Time control.** `AdvanceTime` and `SimulateTimeout` give full control over timer behavior without actual delays. Tests complete in microseconds.

### 8.14 Multi-tenancy

**The gap:** In an organization with multiple teams writing workflows, you need isolation: Team A's workflows shouldn't be able to call Team B's internal services, and Team A shouldn't see Team B's workflow instances. The current design has no isolation boundary — every workflow definition, instance, and credential lives in a flat global namespace.

**Design:** A `namespace` is the unit of isolation. Each team gets one or more namespaces. Every API call, worker claim, UI view, and credential resolution is scoped to a namespace. Namespaces own their workflow definitions, instances, service endpoint configuration, schedules, and credential resolution paths.

**Namespace model.** A new `namespaces` table stores the configuration for each namespace:

```sql
CREATE TABLE namespaces (
    name TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now(),
    -- Service configuration (moved from worker-level config):
    service_endpoints JSONB NOT NULL,     -- {"payments": "https://payments.team-a.internal/api", ...}
    service_allowlist TEXT[] NOT NULL,    -- ["payments", "inventory", ...]
    credential_resolver TEXT NOT NULL,    -- "vault", "env", or "aws-secrets"
    credential_config JSONB,              -- resolver-specific config (Vault path, AWS region, etc.)
    -- Limits:
    max_concurrent_workflows INT DEFAULT 1000,
    max_workflow_duration_seconds INT,    -- workflows exceeding this are force-cancelled
    -- Retention:
    event_history_retention_days INT DEFAULT 30,
    -- Organizational metadata:
    owner_team TEXT,                      -- for directory/contact purposes
    description TEXT,
    -- Cross-namespace communication:
    cross_namespace_call_allowlist TEXT[] DEFAULT '{}',    -- namespaces this namespace may call into
    cross_namespace_receive_allowlist TEXT[] DEFAULT '{}'  -- namespaces that may call into this one
);
```

A `default` namespace is created at database initialization time. Single-tenant deployments map to this namespace. Adding a second namespace makes the deployment multi-tenant. This preserves backward compatibility — no existing API, worker, or query breaks because existing data lives in `default`.

**Database isolation — two approaches.** The system supports two isolation mechanisms, chosen at deployment time or per-namespace for mixed deployments.

**Approach A (lightweight) — namespace column (recommended default).** A `namespace TEXT NOT NULL` column is added to every table that stores per-namespace data. Every query includes `WHERE namespace = $1`. Workers are configured with a namespace they serve (or a list of namespaces). This is a single database with simple operations, one set of migrations, and one connection pool. Strong enough for most organizations — the namespace column is always checked, and application-level authorization prevents accidental cross-namespace access.

**Approach B (strong isolation) — PostgreSQL schemas.** Each namespace gets its own PostgreSQL schema (`payments_team.workflow_instances`, `fulfillment_team.workflow_instances`). The `search_path` is set per-connection. Schema-level `GRANT` statements provide database-enforced isolation: a worker connected to the `payments_team` schema literally cannot see rows in `fulfillment_team`. This prevents cross-namespace access even if there is a bug in the application-layer namespace filter. However, schema migrations must run once per namespace, connection pooling requires connection tagging or per-namespace pools, and operational tooling (backup, monitoring, querying) must be namespace-aware.

**Recommendation:** Use Approach A as the default. Use Approach B for namespaces with strict compliance requirements, such as PCI-scoped payment processing namespaces alongside non-PCI namespaces. Both approaches coexist in the same deployment: PCI namespaces use separate schemas, non-PCI namespaces share a schema with a `namespace` column.

**Schema additions — Approach A (namespace column).** Columns are added to existing tables:

```sql
ALTER TABLE workflow_defs ADD COLUMN namespace TEXT NOT NULL DEFAULT 'default';
ALTER TABLE workflow_instances ADD COLUMN namespace TEXT NOT NULL DEFAULT 'default';
ALTER TABLE event_history ADD COLUMN namespace TEXT NOT NULL DEFAULT 'default';
ALTER TABLE workflow_schedules ADD COLUMN namespace TEXT NOT NULL DEFAULT 'default';
ALTER TABLE pending_signals ADD COLUMN namespace TEXT NOT NULL DEFAULT 'default';
ALTER TABLE workflow_migrations ADD COLUMN namespace TEXT NOT NULL DEFAULT 'default';
```

Primary keys are updated to include namespace. For existing data, the `DEFAULT 'default'` clause ensures the migration is a fast `ADD COLUMN` without a table rewrite (assuming the column is set `NOT NULL` and default is applied).

```sql
-- Composite primary keys including namespace:
-- workflow_defs:       PRIMARY KEY (namespace, name, version)
-- workflow_instances:  PRIMARY KEY (namespace, id)
-- event_history:       PRIMARY KEY (namespace, workflow_id, step)
-- workflow_schedules:  PRIMARY KEY (namespace, name)
-- pending_signals:     PRIMARY KEY (namespace, workflow_id, signal_name)
-- workflow_migrations: PRIMARY KEY (namespace, id)
```

Indexes are updated similarly. The `parent_workflow_id` foreign key on `workflow_instances` (section 8.5) gains a `namespace` component so cross-namespace parent-child relationships are explicitly tracked.

**Credential isolation.** Each namespace resolves credentials independently. A namespace-scoped version of the `CredentialResolver` interface replaces the original:

```go
type CredentialResolver interface {
    Resolve(namespace, service string) (*Credentials, error)
}
```

The Vault path or AWS Secrets Manager path becomes namespace-specific. For example, the Vault resolver constructs paths like `secret/data/durable-workflows/{namespace}/{service}`. Namespace A's `payments` token (resolved to `secret/data/durable-workflows/team-a/payments`) is different from Namespace B's `payments` token (resolved to `secret/data/durable-workflows/team-b/payments`).

Resolver implementations:

| Resolver | Pre-namespace behavior | Post-namespace behavior |
|---|---|---|
| `env` | Read from `DURABLE_PAYMENTS_TOKEN` env var | Read from `DURABLE_NS_{NAMESPACE}_PAYMENTS_TOKEN` |
| `vault` | Fixed path `secret/data/durable-workflows` | Path is `secret/data/durable-workflows/{namespace}/{service}` |
| `aws-secrets` | Lookup `durable-workflows/payments` secret | Lookup `durable-workflows/{namespace}/payments` secret |

The worker-level `service_endpoints`, `service_allowlist`, and `credential_config` fields are deprecated in favor of the per-namespace configuration stored in the `namespaces` table (section 8.14.1). Workers load this configuration at startup and cache it in memory, refreshing periodically or on notification.

**Worker namespace binding.** Workers can be configured in two modes:

**Namespace-scoped (simple, recommended).** A single environment variable pins the worker to one namespace:

```
DURABLE_NAMESPACE=payments-team
```

The worker's claim query includes `WHERE namespace = 'payments-team'`. The worker loads only the `payments-team` namespace's configuration (service endpoints, allowlist, credential resolver). This is the simplest model — one worker process, one namespace, no risk of cross-namespace contamination. Each team operates their own worker pool, or a shared operations team runs namespace-scoped workers for each tenant.

**Multi-namespace (efficient, weaker isolation).** A worker is configured with a list of allowed namespaces:

```
DURABLE_ALLOWED_NAMESPACES=payments-team,fulfillment-team
```

The worker's claim query uses `WHERE namespace = ANY($allowed_namespaces)`. The worker loads all listed namespaces' configurations and resolves the correct configuration for each workflow based on its namespace at claim time. This is more resource-efficient (a smaller pool of workers handles more throughput) but introduces the risk that a bug in the worker's namespace routing could let a `payments-team` workflow accidentally use `fulfillment-team`'s credentials. Defense-in-depth mitigations are documented below.

**Defense-in-depth for multi-namespace workers.** Because multi-namespace workers handle credentials from multiple tenants in a single process, additional protections apply:

1. **Credential cache isolation.** The credential cache is keyed by `(namespace, service)`, not just `service`. A cache lookup for `("payments-team", "payments")` never returns the cached token for `("fulfillment-team", "payments")`.
2. **Configuration immutability per claim.** Once a worker claims a workflow instance, the namespace configuration for that workflow is pinned — it cannot be swapped mid-execution even if the worker's configuration is reloaded.
3. **Fail-closed routing.** If a workflow instance's namespace does not match any known namespace in the worker's allowed list, the workflow fails with an `ErrUnauthorizedNamespace` error. The worker does not fall back to a default namespace.

**Host API scoping.** Every API endpoint is namespace-scoped:

```
POST   /api/v1/namespaces/{namespace}/workflows                    -- start workflow
GET    /api/v1/namespaces/{namespace}/workflows                    -- list workflows (with search/filter)
GET    /api/v1/namespaces/{namespace}/workflows/{id}               -- get workflow details
GET    /api/v1/namespaces/{namespace}/workflows/{id}/history       -- get event history
POST   /api/v1/namespaces/{namespace}/workflows/{id}/cancel        -- cancel workflow
POST   /api/v1/namespaces/{namespace}/workflows/{id}/terminate     -- terminate workflow
POST   /api/v1/namespaces/{namespace}/workflows/{id}/signal/{name} -- send signal
POST   /api/v1/namespaces/{namespace}/workflows/{id}/reset         -- reset workflow
POST   /api/v1/namespaces/{namespace}/workflows/{id}/pause         -- pause workflow
POST   /api/v1/namespaces/{namespace}/workflows/{id}/resume        -- resume workflow
POST   /api/v1/namespaces/{namespace}/workflows/{id}/retry         -- retry dead-lettered workflow
GET    /api/v1/namespaces/{namespace}/schedules                    -- list schedules
PATCH  /api/v1/namespaces/{namespace}/schedules/{id}               -- modify schedule
```

The `{namespace}` parameter maps to the `namespace` column in the database. The host API handler validates that:

1. The namespace exists in the `namespaces` table. Returns 404 if not.
2. The caller has access to the namespace (see authorization below).
3. All subsequent queries include `WHERE namespace = $namespace` (or use the appropriate schema in Approach B).

**Admin endpoints for namespace management:**

```
POST   /api/v1/admin/namespaces                    -- create namespace
GET    /api/v1/admin/namespaces                     -- list all namespaces
GET    /api/v1/admin/namespaces/{namespace}         -- get namespace config
PUT    /api/v1/admin/namespaces/{namespace}         -- update namespace config
DELETE /api/v1/admin/namespaces/{namespace}         -- delete namespace (only if empty)
```

These endpoints require operator-level authorization (separate from workflow-level auth). Namespace creation is an operator action, not a self-service team action, because it involves database migration and credential provisioning.

**Authorization for namespace access.** The API validates namespace access at two levels:

1. **Operator-level:** Operators with access to all namespaces (typically SRE / platform team) use a wildcard or admin token. They see all namespaces in the UI and can access any namespace's API.
2. **Team-level:** Team members have access to specific namespaces, configured via the operator's identity provider. A team member's token carries a `namespaces` claim: `["payments-team", "payments-qa"]`. The API filters all queries to only these namespaces.

The authorization check is a middleware layer applied to all namespace-scoped endpoints:

```
1. Extract namespace from URL path.
2. Extract caller identity from request (bearer token, mTLS cert, session cookie).
3. Resolve caller's allowed namespaces from the identity provider (OIDC claims, LDAP group membership, or static config).
4. If caller's allowed namespaces do not include the requested namespace: return 403 Forbidden.
5. If caller's allowed namespaces include the requested namespace: proceed with the request, scoped to that namespace.
```

**UI scoping (section 8.2b).** The web UI integrates namespace scoping:

- **Namespace picker.** A dropdown in the top navigation bar lists all namespaces the current operator has access to. Selecting a namespace scopes the entire UI session to that namespace — workflow lists, detail pages, schedule views, and search results all reflect the selected namespace.
- **Single-namespace mode.** Operators with access to only one namespace never see the picker. The namespace is shown as a static label in the nav bar. This avoids unnecessary UI complexity for operators managing a single team's workflows.
- **Admin mode.** Operators with access to all namespaces (admin/wildcard) see the picker with all namespaces listed. An additional admin-only "Namespaces" page provides namespace management: create, edit config, view per-namespace health metrics.
- **Search scoping.** All search and visibility endpoints (section 8.17) are scoped to the selected namespace. The `WHERE namespace = $1` clause is added automatically. An operator searching across namespaces must use the admin API with explicit `namespace` filters.
- **Bookmarkable URLs.** The UI uses URL-based namespace routing: `/namespaces/payments-team/workflows/...`. This means a bookmark or shared link to a specific workflow instance includes the namespace, so the recipient sees the correct view regardless of their active namespace selection.

**Cross-namespace communication.** One namespace's workflow can call another namespace's workflow as a child workflow (section 8.5), provided both namespaces explicitly opt in:

**Allowlist model.** Two columns on the `namespaces` table control cross-namespace flow:

- `cross_namespace_call_allowlist`: Namespaces the calling namespace is allowed to start child workflows in. On the caller side.
- `cross_namespace_receive_allowlist`: Namespaces that are allowed to start child workflows in this namespace. On the receiver side.

A call from namespace A to namespace B succeeds only if:
1. Namespace A has `'B'` in its `cross_namespace_call_allowlist`.
2. Namespace B has `'A'` in its `cross_namespace_receive_allowlist`.

This bidirectional check ensures that both teams explicitly consent to the cross-namespace relationship. It prevents namespace A from silently depending on namespace B's workflows without namespace B's knowledge.

**Child workflow execution context.** When a cross-namespace child workflow runs:

- **Credentials.** The child workflow uses the receiving namespace's (B's) credential resolver and service endpoints. The parent's (A's) credentials are never exposed to the child. This prevents credential escalation.
- **Service allowlist.** The child workflow is subject to namespace B's `service_allowlist`, not namespace A's. The child can only call services that namespace B has configured.
- **Event history isolation.** The parent can see the child's workflow ID and status (done, failed, cancelled) but cannot view the child's event history. Parent A sees `{"operation": "child_completed", "child_run_id": "...", "status": "done"}` in its event history, but not the child's request/response bodies. Event history access requires access to both namespaces at the operator level.
- **Visibility.** Child workflows created by a cross-namespace call are owned by the receiving namespace. They appear in namespace B's workflow list, not namespace A's. The `parent_workflow_id` references the parent's workflow ID, but the parent's ID is stored with its namespace qualifier: `(namespace_A, parent_uuid)`.

**Default: disabled.** Both `cross_namespace_call_allowlist` and `cross_namespace_receive_allowlist` default to empty arrays. Most teams will never configure cross-namespace calls. Cross-namespace communication is an explicit opt-in, not a capability that needs to be locked down.

**Implementation plan.** Multi-tenancy is implemented in five phases:

| Phase | What | Effort |
|---|---|---|
| 1. Namespace column + namespaces table | Create `namespaces` table. Add `namespace` columns to all existing tables. Write migration path for existing data (populate `'default'`). Update primary keys and indexes. Create `default` namespace at init. | 0.5 weeks |
| 2. Namespace-scoped configuration | Move `service_endpoints`, `service_allowlist`, `credential_resolver`, and `credential_config` from worker-level config to the `namespaces` table. Update `CredentialResolver` interface with namespace parameter. Update Vault, env, and AWS Secrets Manager resolvers. Worker loads namespace config at startup. | 1 week |
| 3. Worker namespace binding | Implement namespace-scoped claiming (`WHERE namespace = $1`). Implement multi-namespace claiming (`WHERE namespace = ANY(...)`). Add `DURABLE_NAMESPACE` and `DURABLE_ALLOWED_NAMESPACES` env vars. Add credential cache isolation. | 0.5 weeks |
| 4. Host API and UI scoping | Add namespace parameter to all API endpoints. Implement authorization middleware (namespace access check). Add namespace picker to UI. Scope all searches and dashboard views to selected namespace. Implement admin namespace management endpoints. | 0.5 weeks |
| 5. Cross-namespace calls | Implement bidirectional allowlist check. Implement namespace-scoped child workflow execution (child uses receiving namespace's credentials and allowlist). Implement event history isolation for cross-namespace children. | 1 week |
| **Total** | | **~3.5 weeks** |

Phase 1 blocks everything. Phases 2 and 3 can proceed in parallel once the namespace infrastructure exists. Phase 4 depends on phases 2 and 3. Phase 5 is independent and can be deferred — cross-namespace calls are an optional feature.

### 8.15 Workflow prioritization

**The gap:** All workflows are equal in the SKIP LOCKED queue. An urgent order should jump ahead of a batch reconciliation run. Without prioritization, latency-sensitive workflows can be delayed indefinitely behind background jobs, and operators have no mechanism to expedite critical workflows.

**What's needed:** A priority column on `workflow_instances` that the claim query uses as its primary sort key. The design covers named priority levels, starvation prevention, inheritance for child and scheduled workflows, dynamic priority changes, namespace-level limits, and monitoring.

#### 8.15.1 Priority levels

Priorities are integers where higher values are more urgent. A small set of named levels establishes conventions:

| Level | Value | Purpose |
|---|---|---|
| P0 (critical) | 100 | System-critical, must process immediately |
| P1 (high) | 75 | Urgent business operations (customer-facing) |
| P2 (normal) | 50 | Default for all workflows |
| P3 (low) | 25 | Background / batch jobs |
| P4 (best-effort) | 0 | Can be dropped under load |

The gaps between named levels leave room for inserting intermediate priorities (e.g., P1.5 at 62). The named levels are conventions; any integer in [0, 100] is valid. Values outside this range are clamped at the nearest boundary.

#### 8.15.2 Schema changes

A single column and a partial index on `workflow_instances`:

```sql
ALTER TABLE workflow_instances
    ADD COLUMN priority INT NOT NULL DEFAULT 50;

CREATE INDEX idx_workflow_priority
    ON workflow_instances (priority DESC, created_at ASC)
    WHERE status = 'ready';
```

The partial index only covers claimable workflows (`status = 'ready'`), keeping it small and fast to maintain. The index is ordered by `priority DESC, created_at ASC` to match the claim query ordering. Workflows that are running, complete, or failed are not indexed.

#### 8.15.3 Modified claim query

The worker's claim query orders by priority before creation time, capturing the highest-priority available work first. Each worker claims up to `$batch_size` workflows per polling cycle:

```sql
SELECT * FROM workflow_instances
WHERE status = 'ready'
  AND next_wake_at <= now()
  AND namespace = $worker_namespace    -- per 8.14 worker namespace binding
ORDER BY priority DESC, created_at ASC
LIMIT $batch_size
FOR UPDATE SKIP LOCKED;
```

After the SELECT locks the rows, the worker updates each claimed row to assign ownership:

```sql
UPDATE workflow_instances
SET assigned_to = $worker_id,
    heartbeat_at = now(),
    epoch = epoch + 1
WHERE id = ANY($claimed_ids)
  AND assigned_to IS NULL;  -- safety: only claim unassigned rows
```

The batch size is a worker configuration parameter. A reasonable default is 10. A smaller batch (e.g., 1) maximizes fairness at the cost of higher polling overhead. A larger batch (e.g., 100) maximizes throughput at the cost of a longer claim transaction and more in-flight workflows per worker.

**Why not a single UPDATE with a subquery?** The idiomatic PostgreSQL pattern would be:

```sql
UPDATE workflow_instances
SET assigned_to = $worker_id, ...
WHERE id = (
    SELECT id FROM workflow_instances
    WHERE status = 'ready' AND next_wake_at <= now()
    ORDER BY priority DESC, created_at ASC
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;
```

This doesn't work well with SKIP LOCKED: the inner SELECT locks exactly one row (the `LIMIT 1` applies inside the subquery), so only one worker can claim at a time even though other workers could be claiming different rows. The two-phase approach (SELECT FOR UPDATE SKIP LOCKED, then UPDATE) allows multiple workers to claim concurrently while preserving priority ordering within each batch.

#### 8.15.4 Starvation prevention

With pure priority ordering, a sustained stream of P0/P1 workflows can starve P3/P4 workflows indefinitely. To prevent this, add a `max_wait_seconds` column that sets a ceiling on how long a workflow can wait before it receives a priority boost:

```sql
ALTER TABLE workflow_instances
    ADD COLUMN max_wait_seconds INT NOT NULL DEFAULT 3600;  -- 1 hour
```

A background goroutine in each worker runs every ~60 seconds and claims workflows that have exceeded their maximum wait time:

```sql
SELECT * FROM workflow_instances
WHERE status = 'ready'
  AND next_wake_at <= now()
  AND namespace = $worker_namespace
  AND created_at < now() - interval '1 second' * max_wait_seconds
ORDER BY created_at ASC
LIMIT $batch_size
FOR UPDATE SKIP LOCKED;
```

This "priority boost" query ignores the priority column entirely and orders by `created_at ASC` only, picking the oldest starved workflows. The boost is modest — it only applies when a workflow has actually exceeded its wait threshold, and it only guarantees that at least `$batch_size` starved workflows are claimed per 60-second cycle. Under moderate load, this is enough to prevent total starvation. Under extreme load (sustained P0 flood exceeding the cluster's total capacity), even starved workflows may wait — but that is a capacity problem, not a scheduling problem.

The `max_wait_seconds` default of 3600 (1 hour) is conservative. Operators can tune it per workflow at start time:

```go
id, err := durable.StartWorkflow(ctx, "BatchReconciliation", input,
    durable.WithMaxWaitSeconds(7200))  // can wait up to 2 hours
```

#### 8.15.5 Start priority, inheritance, and schedule priority

**Workflow start.** Priority is set when starting a workflow via a functional option:

```go
// Default: P2 (normal)
id, err := durable.StartWorkflow(ctx, "PlaceOrder", input)

// Explicit priority:
id, err := durable.StartWorkflow(ctx, "PlaceOrder", input,
    durable.WithPriority(durable.PriorityHigh))

// Intermediate priority (between named levels):
id, err := durable.StartWorkflow(ctx, "PlaceOrder", input,
    durable.WithPriority(62))
```

**Default assignment:**

| Trigger | Default priority |
|---|---|
| API-triggered workflow | P2 (50) |
| Scheduled workflow | Inherited from `workflow_schedules.priority` |
| Child workflow | Inherited from parent |

**Scheduled workflow priority.** The `workflow_schedules` table gains a `priority` column:

```sql
ALTER TABLE workflow_schedules
    ADD COLUMN priority INT NOT NULL DEFAULT 50;
```

The scheduler inserts scheduled workflow instances at the schedule's priority:

```sql
INSERT INTO workflow_instances (id, def_name, def_version, status, input, priority, namespace)
VALUES ($wfID, $def_name, $def_version, 'ready',
        injectScheduledBy($input, $schedule_name),
        (SELECT priority FROM workflow_schedules WHERE name = $schedule_name AND namespace = $namespace),
        $namespace);
```

**Child workflow inheritance.** By default, a child workflow inherits the parent's priority. The caller can override with `ChildWorkflowOptions.Priority`:

```go
type ChildWorkflowOptions struct {
    // ... existing fields (Timeout, RetryPolicy, FireAndForget, CancelChildrenOnParentCancel) ...
    Priority *int  // nil = inherit parent priority (default); non-nil = override
}
```

```go
// Child inherits parent's priority (P2 default):
runID, err := h.DurableChildWorkflow("FraudCheck", input, &durable.ChildWorkflowOptions{})

// Child override to lower priority (fraud check is background work):
opts := &durable.ChildWorkflowOptions{
    Priority: intPtr(durable.PriorityLow),  // P3 (25)
}
runID, err := h.DurableChildWorkflow("FraudCheck", input, opts)
```

**Signal wakeups do not change priority.** When a signal wakes a workflow, the workflow keeps whatever priority it was started with. A workflow's priority is a property of the instance, not the wakeup event. Operators who need priority changes on signal should use the dynamic priority change mechanism (section 8.15.6).

#### 8.15.6 Dynamic priority changes

An operator can change a running or queued workflow's priority via a REST endpoint. Consistent with the namespace-scoped API (section 8.14):

```
PATCH /api/v1/namespaces/{namespace}/workflows/{id}/priority
Content-Type: application/json

{"priority": 100}
```

This updates the `priority` column directly:

```sql
UPDATE workflow_instances
SET priority = $priority
WHERE id = $id AND namespace = $namespace;
```

- **If the workflow is queued** (status = `ready`), the change takes effect immediately on the next claim cycle. The partial index `idx_workflow_priority` automatically reflects the new priority.
- **If the workflow is running** (status = `running`), the new priority takes effect on the next wake-up (after a sleep or signal). The running worker reads the updated priority from the database when it re-enters the claim-and-execute loop for the next step. The in-memory priority cached by the worker is refreshed from `workflow_instances.priority` on each checkpoint.
- **Validation:** The endpoint enforces namespace `max_priority` limits: the request is rejected with HTTP 422 if `priority > namespace.max_priority`.

The UI exposes the current priority in the workflow detail page with a set of buttons (P0–P4) or a dropdown. The UI also shows the workflow's wait time so operators can identify starved workflows at a glance.

#### 8.15.7 Per-namespace priority limits

To prevent abuse (e.g., a noisy tenant running everything at P0), namespaces have a `max_priority` setting:

```sql
ALTER TABLE namespaces
    ADD COLUMN max_priority INT NOT NULL DEFAULT 100;
```

A workflow cannot be started or updated with a priority higher than its namespace's `max_priority`:

- **Workflow start:** The host validates `requested_priority <= namespace.max_priority` before creating the instance. If the check fails, the start returns an error.
- **Dynamic update:** The PATCH endpoint enforces the same check. Returns HTTP 422 (Unprocessable Entity) with a descriptive error message.
- **Child workflows:** The parent's namespace constraint applies. A child cannot be started with a priority exceeding the namespace's max, even if the parent's own priority is lower.

A `max_priority` of 100 means all priorities are allowed. A namespace with `max_priority = 50` cannot create P0 or P1 workflows. A namespace with `max_priority = 0` can only create P4 (best-effort) workflows. This column sits naturally alongside the existing `max_concurrent_workflows` and `max_workflow_duration_seconds` limits in the `namespaces` table.

Example configuration:

```json
{
  "name": "production-team",
  "max_priority": 100
}
{
  "name": "batch-processing-team",
  "max_priority": 25
}
```

#### 8.15.8 Priority in the dead letter queue

When a workflow exhausts its retries and enters the dead letter queue (section 8.6), its priority is preserved in the `priority` column. When an operator retries a DLQ'd workflow, it re-enters the queue at its original priority unless the operator overrides:

```
POST /api/v1/namespaces/{namespace}/workflows/{id}/dlq/retry
Content-Type: application/json

{"priority": 75}  // optional; uses original priority if absent
```

If no override is provided, the instance's existing priority determines its position in the claim order.

#### 8.15.9 Priority metrics

The host emits Prometheus metrics segmented by priority level:

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `durable_workflows_claimed_total` | Counter | `priority` | Claim rate by priority level |
| `durable_workflows_wait_seconds` | Histogram | `priority` | p50/p95/p99 wait time between `created_at` and claim, by priority |
| `durable_workflows_starved_total` | Counter | (none) | Count of priority-boost claims (starvation prevention) |
| `durable_workflows_running` | Gauge | `priority` | Current count of running workflows by priority |

These metrics let operators:
- Detect whether low-priority workflows are being starved (high `wait_seconds` for P3/P4 with low `claimed_total`).
- Observe the effect of dynamic priority changes (a workflow moved from P3 to P1 should appear in the P1 `claimed_total` after the change).
- Set alerts on `durable_workflows_starved_total` — a high rate of starvation boosts suggests the cluster may be under-provisioned or the priority mix is skewed.

#### 8.15.10 Interaction with staged scaling

When the queue moves from PostgreSQL SKIP LOCKED to Redis (Stage 4 in section 6.5), Redis sorted sets provide a natural representation of the priority queue:

```
ZADD workflow_queue:{namespace} {score} {workflow_id}
ZPOPMAX workflow_queue:{namespace} {batch_size}
```

The `ZPOPMAX` operation atomically pops the highest-scoring (highest-priority) entries, which maps directly to the `ORDER BY priority DESC, created_at ASC` logic used in PostgreSQL. To handle tie-breaking within the same priority (older workflows should be claimed first), the score is encoded as a composite:

```
score = priority * 10^15 + (max_timestamp - created_at_timestamp)
```

This encoding ensures that within the same priority level, older workflows sort higher. The `max_timestamp` is a fixed epoch timestamp large enough to accommodate any workflow's creation time (e.g., `10^15` microseconds = ~31 years from epoch). As workflows age, their encoded score increases, so they drift toward the top of their priority band.

The starvation prevention mechanism becomes a separate Redis scan:

```
ZRANGEBYSCORE workflow_queue:{namespace} -inf {threshold_score}
```

where `threshold_score` corresponds to the oldest acceptable age. Entries below this threshold have been waiting too long and are claimed regardless of priority.

The PostgreSQL-based design (priority column + ordered claim query) is deliberately structured so that the data model maps cleanly to this Redis representation. The `(priority, created_at)` index mirrors the composite score, the batch claim query mirrors `ZPOPMAX` with batch size, and the starvation query mirrors `ZRANGEBYSCORE`. This means the migration from PostgreSQL to Redis is a data-migration and query-replacement exercise, not a design pivot — the system's behavior is identical in both backends.

#### 8.15.11 Implementation effort

| Phase | What | Effort |
|---|---|---|
| 1. Schema + claim query | Add `priority` column, partial index, modify claim query with batch, starvation goroutine | 0.5 weeks |
| 2. API + inheritance | Start/child/schedule priority parameters, inheritance logic, PATCH priority endpoint | 0.5 weeks |
| 3. UI + dynamic changes | Priority display and change in workflow detail view, starved-workflow highlighting | 0.5 weeks |
| 4. Metrics + namespace limits | Priority-segmented Prometheus metrics, namespace `max_priority` enforcement | 0.5 weeks |
| **Total** | | **~2 weeks** |

Phase 1 is the foundation — everything else depends on the priority column existing and the claim query using it. Phases 2 and 3 can proceed in parallel once Phase 1 is deployed. Phase 4 (namespace limits) depends on section 8.14 (Multi-tenancy) being implemented; if namespaces do not exist yet, the `max_priority` column and its enforcement can be deferred.

### 8.16 History size limits and compaction

**The gap:** A workflow that runs for months with thousands of steps produces a large event history. Replaying 10,000 steps on every resume or query is expensive — the worker loads all rows from `event_history`, the database scans an ever-growing table, and the WASM module re-executes the workflow function from step 0 for every cached result. Storage cost grows linearly with step count. Temporal recommends keeping histories under ~50K events and provides "Continue-As-New" to start a fresh history.

A true snapshot (serializing the WASM module's live Go variables at a point in time and resuming from there) would require WASM module cooperation or a serializable heap inspector that does not exist outside research JVMs. The local variables after step 500 are the product of 500 cached API responses flowing through the workflow's conditional logic — we cannot reconstruct them without replay.

**Design:** This section defines two complementary mechanisms. Auto-compaction reduces storage and I/O cost transparently (no developer effort). Continue-As-New gives the workflow author an explicit reset point (developer effort, but clean zero-replay-cost restart).

---

### Mechanism 1: Auto-compaction (cold storage)

Rather than snapshotting live variables, we split the event history into hot and cold tiers. The most recent K steps (default: 100) stay in the existing `event_history` table for fast indexed access. Older steps are moved to `event_history_cold`, a compact table that stores groups of steps as single compressed JSONB rows. Replay still starts from step 0, but cold steps are loaded via a single decompress-in-Go instead of thousands of row reads.

**Cold storage schema:**

```sql
CREATE TABLE event_history_cold (
    workflow_id UUID NOT NULL,
    first_step  INT NOT NULL,
    last_step   INT NOT NULL,
    steps_compressed JSONB NOT NULL,  -- gzip-compressed + base64-encoded array of step records
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workflow_id, first_step)
);
```

Each row in `event_history_cold` holds a chunk of consecutive steps (default: 500 steps per chunk), stored as a single compressed JSONB blob. A chunk follows the same structure as `event_history` rows:

```json
{
  "steps": [
    {
      "step": 0,
      "service": "inventory",
      "operation": "Reserve",
      "request": "{\"user_id\":\"alice\",\"cart\":[...]}",
      "response": "{\"reservation_id\":\"resv_123\"}",
      "error": null,
      "recorded_at": "2026-04-01T12:00:01Z"
    },
    {
      "step": 1,
      "service": "payments",
      "operation": "Charge",
      ...
    }
  ]
}
```

Storage savings come from:
- No per-step PostgreSQL tuple overhead (~28 bytes per row + index entries eliminated)
- Gzip compression of the serialized JSON array achieves ~10x reduction for typical step payloads (JSON request/response blobs compress well)
- No per-step B-tree index entries on `event_history` (only 2-3 index entries per cold chunk instead of per-step)

**The compactor process:**

A background goroutine in the worker ("the compactor") periodically moves old steps from `event_history` to `event_history_cold`. It uses an advisory lock to ensure only one worker runs compaction at a time:

```sql
-- Acquire the compaction advisory lock (PostgreSQL session-level lock)
SELECT pg_try_advisory_lock(42) AS acquired;
```

The compaction loop:

```
while true:
  1. SELECT workflow_id FROM event_history
     GROUP BY workflow_id
     HAVING MAX(step) > $1   -- workflows exceeding compact_after_steps
     ORDER BY MAX(step) DESC
     LIMIT 10;

  2. For each workflow_id:
     a. SELECT step, service, operation, request, response, error, recorded_at
        FROM event_history
        WHERE workflow_id = $workflow_id
          AND step < MAX(step) - $hot_keep_steps
        ORDER BY step;

     b. Chunk the returned rows into groups of $chunk_size (default: 500).

     c. For each chunk:
        - Serialize to JSON
        - gzip-compress and base64-encode
        - INSERT INTO event_history_cold (...) VALUES (...) ON CONFLICT DO NOTHING;

     d. DELETE FROM event_history
        WHERE workflow_id = $workflow_id
          AND step < MAX(step) - $hot_keep_steps;

  3. SLEEP $compaction_interval (default: 15 minutes).
```

**Safety considerations:**

- The `DELETE` in step 2d is safe because `event_history` is append-only — no existing step is ever modified, only moved. The cold table preserves the exact data in chunked form.
- A check before compaction ensures the workflow is not currently claimed by a running worker (checked via `assigned_to` and `heartbeat_at`). If it is, compaction is deferred to the next cycle for that workflow. This prevents a race between a live worker appending steps and the compactor moving them.
- If the compactor crashes mid-cycle, the partially written cold rows are harmless (they duplicate data still in `event_history`). The next compactor cycle picks up from where it left off — the `INSERT` in step 2c uses `ON CONFLICT (workflow_id, first_step) DO NOTHING` for idempotency, and the `DELETE` in step 2d is a no-op on already-deleted rows.

**Replay with cold history:**

When a worker loads a workflow's event history, it now queries both tables:

```go
func loadEventHistory(ctx context.Context, db *sql.DB, workflowID uuid.UUID) ([]StepRecord, error) {
    // Hot steps — fast index scan on event_history
    hotRows, err := db.Query(ctx, `
        SELECT step, service, operation, request, response, error
        FROM event_history
        WHERE workflow_id = $1
        ORDER BY step
    `, workflowID)
    if err != nil {
        return nil, fmt.Errorf("load hot history: %w", err)
    }
    defer hotRows.Close()

    // Cold steps — decompressed in Go
    coldRows, err := db.Query(ctx, `
        SELECT first_step, last_step, steps_compressed
        FROM event_history_cold
        WHERE workflow_id = $1
        ORDER BY first_step
    `, workflowID)
    if err != nil {
        return nil, fmt.Errorf("load cold history: %w", err)
    }
    defer coldRows.Close()

    // Merge: cold steps (by chunk) then hot steps (individual)
    var steps []StepRecord
    for coldRows.Next() {
        var compressed []byte
        // decompress gzip+base64
        decompressed := decompress(compressed)
        var chunk struct{ Steps []StepRecord }
        json.Unmarshal(decompressed, &chunk)
        steps = append(steps, chunk.Steps...)
    }
    for hotRows.Next() {
        // append individual step records
    }
    return steps, nil
}
```

The merged slice is passed to the replay engine as before. The WASM module does not know that some steps came from cold storage — it sees the same ordered sequence of step records. The replay computation is unchanged (the WASM module still re-executes the workflow from step 0 for each cached result), but the database I/O is dramatically reduced:

- 10,000 steps: 20 cold rows (at 500 steps/chunk) + 100 hot rows = 120 database rows, vs 10,000 without compaction
- Each cold row is ~50-200 KB compressed vs ~1-20 KB per uncompressed `event_history` row — fewer round trips, less network I/O
- The `event_history` table stays small (only hot steps), so index scans remain fast

---

### Mechanism 2: Continue-As-New (explicit restart)

Auto-compaction reduces storage and I/O but does not reduce replay computation. For workflows with natural boundaries — a monthly billing cycle, a per-order fulfillment, a subscription renewal — the developer can explicitly start a new clean history.

**API:**

```go
type HostCalls struct {
    // ... existing functions ...
    DurableContinueAsNew func(inputJSON string) error
}
```

`DurableContinueAsNew` is a terminal call. When invoked:

1. The host records a `continue_as_new` event in the current event history, capturing the new input.
2. The host completes the current workflow instance with status `continued`.
3. The host creates a new workflow instance with the same `def_name` and `def_version`, the new `input`, and status `queued`.
4. The old instance is linked to the new instance, and vice versa.

The new instance has its own event history starting at step 0. It is claimed and executed by a worker independently.

**Usage example — monthly billing cycle:**

```go
func MonthlyBilling(h *cleat.HostCalls, billingPeriod string) error {
    // Step 0: Get all active subscriptions
    subscriptions, err := getActiveSubscriptions(h, billingPeriod)
    if err != nil {
        return fmt.Errorf("get subscriptions: %w", err)
    }

    // Step 1..N: Process each subscription
    for _, sub := range subscriptions {
        err := chargeSubscription(h, sub)
        if err != nil {
            logFailedCharge(h, sub, err)
        }
    }

    // Step N+1: Continue to next month
    nextPeriod := advanceMonth(billingPeriod)
    return h.DurableContinueAsNew(nextPeriod)
}
```

The workflow runs forever (one month at a time), but each individual instance has a short event history — at most the number of subscriptions being billed, never accumulating across months.

**Schema additions:**

```sql
ALTER TABLE workflow_instances ADD COLUMN continued_to   UUID REFERENCES workflow_instances(id);
ALTER TABLE workflow_instances ADD COLUMN continued_from UUID REFERENCES workflow_instances(id);

CREATE INDEX idx_workflow_instances_continued_to   ON workflow_instances (continued_to);
CREATE INDEX idx_workflow_instances_continued_from ON workflow_instances (continued_from);
```

A new status value is added to the workflow lifecycle:

```sql
ALTER TABLE workflow_instances DROP CONSTRAINT IF EXISTS valid_status;
ALTER TABLE workflow_instances ADD CONSTRAINT valid_status CHECK (
    status IN ('queued', 'running', 'done', 'failed', 'cancelled', 'continued')
);
```

**Workflow lifecycle with Continue-As-New:**

```
timeline:

  instance A (status: running)
    ├─ step 0: getActiveSubscriptions
    ├─ step 1: chargeSubscription(sub_1)
    ├─ step 2: chargeSubscription(sub_2)
    ├─ ...
    ├─ step N: chargeSubscription(sub_N)
    └─ step N+1: continue_as_new (calls DurableContinueAsNew(nextPeriod))
         │
         ├─ instance A.status → 'continued'
         └─ instance B created (status: 'queued')
              │
              ├─ continued_from   = instance_A.id
              ├─ continued_to     = NULL (for now)
              ├─ def_name         = instance_A.def_name
              ├─ def_version      = instance_A.def_version
              └─ input            = nextPeriod
                   │
                   ├─ [worker claims instance B]
                   ├─ [replays step 0..N with new history]
                   └─ [eventually calls DurableContinueAsNew again]
                        └─ creates instance C...
```

The linking columns support recursive queries to trace the full workflow chain:

```sql
-- Find the entire chain of continuations for a workflow
WITH RECURSIVE chain AS (
    SELECT id, continued_to, def_name, status, input, created_at
    FROM workflow_instances WHERE id = $start_id
    UNION ALL
    SELECT wi.id, wi.continued_to, wi.def_name, wi.status, wi.input, wi.created_at
    FROM workflow_instances wi
    INNER JOIN chain ON chain.continued_to = wi.id
)
SELECT * FROM chain ORDER BY created_at;
```

**Event history representation:**

```json
{"step": 25, "service": "_durable", "operation": "continue_as_new",
 "request": {"input": "\"2026-05\"", "reason": "end_of_billing_period"},
 "response": {"new_instance_id": "b3f1a2c4-..."}}
```

**What Continue-As-New does NOT do:**

- It does **not** transfer local variables. The new instance starts with fresh state. Any data that must survive the boundary must be in the new `input`. This is identical to Temporal's design.
- It does **not** preserve the event history. The old history is preserved for audit but is not consulted by the new instance.
- It does **not** transfer signals, timers, or open defers. The new instance starts clean. If the workflow needs to carry forward a timer (e.g., "30 days until next billing"), the new input should encode the schedule.

---

### Configuration

Both mechanisms are controlled by per-workflow-definition configuration, stored as a JSONB column on `workflow_defs`:

```sql
ALTER TABLE workflow_defs ADD COLUMN history_policy JSONB NOT NULL DEFAULT '{
    "auto_compact": true,
    "compact_after_steps": 1000,
    "hot_keep_steps": 100,
    "cold_chunk_size": 500,
    "compaction_interval_minutes": 15,
    "max_steps_before_warn": 50000,
    "cold_compress": true
}';
```

| Field | Default | Description |
|---|---|---|
| `auto_compact` | `true` | Enable automatic cold-storage compaction |
| `compact_after_steps` | `1000` | Workflows with more steps than this trigger compaction |
| `hot_keep_steps` | `100` | Number of most recent steps to keep in `event_history` |
| `cold_chunk_size` | `500` | Steps per cold-storage row |
| `compaction_interval_minutes` | `15` | How often the compactor runs |
| `max_steps_before_warn` | `50000` | Alert threshold for workflows that exceed this without compaction or continuation |
| `cold_compress` | `true` | Enable gzip compression of cold chunks (disable for CPU-constrained setups) |

### Triggering and monitoring

**Compaction trigger:** The compactor is triggered by two conditions:
1. **Scheduled:** Every `compaction_interval_minutes`, the compactor goroutine wakes and scans for workflows exceeding `compact_after_steps`.
2. **Size-threshold:** If `event_history` grows beyond a configured table size (default: 10 GB), the compactor is triggered immediately. This is checked after every checkpoint `INSERT` by the worker.

**Metrics (Prometheus):**

| Metric | Labels | Description |
|---|---|---|
| `durable_steps_total` | `workflow_id`, `def_name` | Total steps in event history (hot + cold combined) |
| `durable_steps_cold` | `workflow_id`, `def_name` | Steps moved to cold storage |
| `durable_steps_hot` | `workflow_id`, `def_name` | Steps remaining in hot table |
| `durable_cold_chunks` | `workflow_id` | Number of cold-storage rows for this workflow |
| `durable_compaction_duration_seconds` | None | Time spent in last compaction cycle |
| `durable_compaction_rows_moved` | None | Steps moved to cold storage in last cycle |
| `durable_steps_compression_ratio` | None | Ratio of uncompressed to compressed cold chunk size |
| `durable_continue_as_new_count` | `def_name` | Number of Continue-As-New invocations |
| `durable_workflow_chain_length` | `workflow_id` | Number of continuations in a workflow chain |

**Alerts:**

- `durable_workflow_chain_length > 1000`: A workflow chain has grown too long. The developer should investigate whether the chain creates operational problems (orphaned instances, audit confusion). The alert recommends reviewing the continuation logic.
- `durable_steps_total > max_steps_before_warn`: A workflow has exceeded the alert threshold. The developer should consider whether the workflow should call `DurableContinueAsNew` or whether the auto-compaction threshold should be lowered.
- `durable_compaction_duration_seconds > 300`: Compaction is taking too long. The database may be under-provisioned, or the compaction batch size may be too large.

### Interaction with other features

- **Fencing token:** The compactor checks `assigned_to` and `epoch` before compacting a workflow. If a workflow is actively running, compaction is deferred. This prevents the compactor from racing with a live worker appending steps.
- **Child workflows:** Continue-As-New does not affect child workflows. The parent-child tree is preserved across continuation instances. The new instance gets a new `id` but the `parent_workflow_id` column still points to the old (continued) instance's parent.
- **Signals and cancellation:** Cancel requests and signals posted to the old instance's `workflow_id` are forwarded to the new instance at the database level. The worker checks `continued_to` on claim and redirects pending signals. This is handled by a trigger or application-level lookup in the signal delivery path (see section 8.1).

### Comparison to Temporal

Temporal's Continue-As-New requires the developer to explicitly call it, passing all state as input. It creates a new event history. Temporal does not provide automatic compaction — the burden is entirely on the developer.

This design provides both:

| Aspect | Temporal | This design |
|---|---|---|
| Automatic compaction | None | Cold-storage tiering; transparent to the workflow |
| Explicit reset | Continue-As-New | `DurableContinueAsNew` (same semantics) |
| Storage reduction | Only through Continue-As-New (developer must act) | Automatic via cold storage + optional Continue-As-New |
| Replay computation reduction | Only through Continue-As-New | Only through Continue-As-New (auto-compaction does not reduce replay CPU) |
| Single-table simplicity | N/A | Cold storage adds one table; admin overhead is minimal |
| Operates on running workflows | No (only at explicit boundaries) | Yes (compactor works on any workflow, including actively running ones) |

The auto-compaction handles the common case (long-running workflows that do not have natural reset points) without developer effort. The explicit `DurableContinueAsNew` handles the ideal case (periodic workflows with clean boundaries) and provides the same zero-replay-cost restart as Temporal.

### Implementation plan

| Phase | What | Effort |
|---|---|---|
| 1. Cold storage table + migration | Create `event_history_cold` table, add `history_policy` column to `workflow_defs`, implement compactor goroutine with advisory lock, add decompress-on-load to `loadEventHistory`, add metrics | 1 week |
| 2. Continue-As-New | `DurableContinueAsNew` host function, `continued_to`/`continued_from` columns, new `continued` status, forward signals/cancellation to continued instance, add metrics | 1 week |
| 3. Monitoring + alerting | Prometheus metrics for step counts, compaction duration, chain length; alert rules; basic admin dashboard panel showing per-workflow history size | 0.5 week |
| Total | | ~2.5 weeks |

This is comparable in scope to child workflows (section 8.5, estimated ~3 weeks) and smaller than the transformer (section 8.12, estimated ~12 weeks). It is important for production longevity — without it, any workflow that runs for months will eventually become unmanageable.

### 8.17 Workflow search and visibility

**The gap:** The operator has no way to find a specific workflow instance without knowing its UUID. In a production system with thousands of running workflows, an operator needs to answer questions like "show me all failed PlaceOrder workflows for customer rob@example.com" or "list all workflows that reference order ORD-12345."

**Custom attributes:**

Add a `custom_attributes JSONB` column to `workflow_instances`:

```sql
ALTER TABLE workflow_instances ADD COLUMN custom_attributes JSONB;
CREATE INDEX idx_workflow_attrs ON workflow_instances USING GIN (custom_attributes);
```

Workflow authors set custom attributes at workflow start time. The `DurableStartWorkflow` API accepts an optional `custom_attributes` parameter:

```go
id, err := durable.StartWorkflow(ctx, "PlaceOrder", input,
    durable.WithCustomAttributes(map[string]string{
        "customer_email": "rob@example.com",
        "order_id":       "ORD-12345",
    }))
```

The attributes are stored as a flat JSONB object. Because the column has a GIN index, operators can query workflows by attribute:

```sql
-- Find all workflows for a specific customer
SELECT id, def_name, def_version, status, created_at
FROM workflow_instances
WHERE custom_attributes @> '{"customer_email": "rob@example.com"}';

-- Find failed workflows related to a specific order
SELECT id, def_name, status, error_message
FROM workflow_instances
WHERE custom_attributes @> '{"order_id": "ORD-12345"}'
  AND status = 'failed';
```

**Operator CLI/API (minimal):**

```
# CLI
durable workflows list --status failed --def PlaceOrder --attr customer_email=rob@example.com
durable workflows get --id <uuid>

# HTTP API
GET /api/v1/workflows?status=failed&def=PlaceOrder&attr.customer_email=rob@example.com
```

This is a minimal visibility solution. Temporal's visibility store supports more advanced queries (e.g., "workflows where step X took > 30s"), which would require indexing aggregated data from `event_history` — a future enhancement.

### 8.18 Summary of gaps by importance

Designs now exist for all P0 and P1 gaps. The operator UX surface (8.2b) adds a new category: features needed to make the system usable by operators who didn't build it.

| Priority | Gap | Design status | Effort |
|---|---|---|---|
| **P0** | Timers and durable sleep | **Designed** (8.3) | Medium |
| **P0** | Retry policies per durable call | **Designed** (8.4) | Small |
| **P0** | Idempotency for external calls | **Designed** (8.7) | Small |
| **P0** | Poison pill / dead letter queue | **Designed** (8.10) | Small |
| **P0** | Secrets and credentials | **Designed** (8.8) | Small |
| **P1** | Signals and external events | **Designed** (8.1) | Large |
| **P1** | Workflow cancellation | **Designed** (8.6) | Medium |
| **P1** | DurableDefer (cleanup on exit) | **Designed** (8.6b) | Medium |
| **P1** | Testing framework | **Designed** (8.13) | Medium |
| **P1** | Transformer implementation | **Designed** (8.12) | ~12 weeks |
| **P1** | Workflow state queries | **Designed** (8.2) | 1 week |
| **P1** | Operational controls API (terminate/reset/pause) | **Designed** (8.2b) | 1 week |
| **P1** | Minimal web UI (list/detail/actions) | **Designed** (8.2b) | 2–3 weeks |
| **P2** | Schema evolution — version markers | **Designed** (8.9) | Small |
| **P2** | Schema evolution — migration functions | **Designed** (8.9) | Large |
| **P2** | Scheduling / CRON | **Designed** (8.11) | Small |
| **P2** | Child workflows | **Designed** (8.5) | Medium |
| **P2** | Workflow search and visibility | **Designed** (8.17) | Medium |
| **P2** | History size limits / compaction | **Designed** (8.16) | ~2.5 weeks |
| **P2** | Source annotations for execution position | **Designed** (8.2b) | 0.5 weeks |
| **P2** | Grafana dashboard + alert rules | **Designed** (8.2b) | 0.5 weeks |
| **P2** | OpenTelemetry bridge | **Designed** (8.2b) | 1 week |
| **P3** | Multi-tenancy | **Designed** (8.14) | ~3.5 weeks |
| **P3** | Workflow prioritization | **Designed** (8.15) | ~2 weeks |

All gaps now have concrete designs — P0 (5 items), P1 (8 items), P2 (8 items), and P3 (2 items). Every section from 8.1 through 8.17 has been designed with API signatures, SQL schema, event history representations, and replay semantics. The transformer (8.12) at ~12 weeks remains the largest single piece of engineering work. The operator UX surface (8.2b) adds ~5 weeks — the minimum to make the system usable by someone who didn't build it. Excluding the transformer, all designed items sum to approximately ~25 weeks of implementation effort.

The durability mechanism (replay, fencing, event history, WASM versioning) is the hard technical problem. The operational surface (UI, queries, controls, dashboards) is where Temporal's decade of investment shows most. Both need to be solved for the system to be viable.

---

## 9. Alternative Design Choices

This section explores three fundamental architectural alternatives to the decisions made in the design above. This is not a defense of the current choices — it is an honest assessment of whether different paths would produce a better system. Each question includes a comparison table, a discussion of the key tradeoffs, and a clear recommendation.

---

### 9.1 Replay vs. Resume (checkpoint serialization)

The current design uses **replay**: on resume, re-execute from step 0, returning cached results for completed calls. The alternative is **checkpoint serialization**: snapshot the program state (all live variables, call stack) at each durable boundary, and on resume restore that snapshot into a fresh WASM module — zero replay, instant resume.

#### The case for checkpoint serialization

Replay is O(n) where n = steps in history. A workflow with 10,000 steps replays 10,000 function calls on every resume. This is usually fast (cached responses, no I/O) but it is wasted CPU and adds latency proportional to history length. Temporal's Continue-As-New recommendation is a workaround for replay being O(n) — Temporal explicitly recommends keeping histories under 50,000 events. A workflow that runs for months with daily steps spends more wall-clock time replaying than executing fresh work.

Checkpoint serialization eliminates this entirely: resume from the snapshot, zero replay. The time to resume is O(1): deserialize the checkpoint, restore WASM memory, jump to the step counter, continue.

#### The case against (why we chose replay)

WASM linear memory is just a byte array. In theory, snapshotting it IS possible — dump the entire memory to bytes, store alongside the event history, restore on resume. The WASM module is the same version (guaranteed by our versioning system), so the memory layout is identical. But three complications make this much harder than it sounds:

**1. The Go runtime lives in the same memory space.** The WASM module's memory contains not just the workflow's application variables but the entire Go runtime: the heap with all allocated objects, goroutine stacks, scheduler state, GC metadata, type descriptors, and the defer/panic stack. Serializing this requires the WASM module to cooperate — it needs to expose `serialize_state()` and `restore_state()` exports. The transformer would need to generate code that walks all live variables, traverses the goroutine stacks, and writes everything to a known region of memory. This is essentially implementing a full serialization library for Go's runtime.

**2. Pointer relocation.** Go values contain pointers. When memory is serialized to bytes and restored into a fresh WASM module, the module may load at a different base address (especially across different workers or wazero versions). Every pointer in the serialized state must be relocated — the offset added to every pointer address during deserialization. This is a classic problem (same as core dump analysis or JVM heap dump restore) and requires knowing the location of every pointer in the heap. Go's GC already tracks pointer locations; exposing this metadata for serialization is feasible but requires deep coupling with the Go runtime.

**3. Internal runtime state is opaque.** The Go runtime's internal scheduler state, GC roots, finalizer queue, and goroutine parking state are not captured by a memory dump alone. Restoring just the application state into a fresh WASM module means initializing a fresh Go runtime and then populating application state. But the Go runtime does not expose hooks to inject serialized goroutine stacks, heap objects, or scheduler state. Achieving this would require either forking the Go runtime or using cgo/unscoped memory access tricks that are not available in WASM.

#### A hybrid: snapshot + replay fallback

The workflow author marks certain variables as "checkpointable" — the ones that survive across durable boundaries (local variables that hold the results of previous `DurableCall` responses). The transformer generates serialization code for just those marked variables:

```go
//durable:checkpoint
var reservation Reservation  // serialized at each durable boundary

//durable:checkpoint
var charge Charge

// NOT checkpointed — rederived on replay:
// var tempCount int
```

At each checkpoint: serialize marked variables plus step counter into a snapshot row in the database. On resume: restore the snapshot into a fresh WASM module, skip replay of all steps before the checkpoint, and continue execution. If the snapshot is unavailable (cold storage migration, version mismatch, worker running an incompatible wazero version): fall back to full replay from the event history. The event history remains the source of truth — the snapshot is a performance optimization.

This hybrid approach gives fast resume most of the time with replay as a safety net. It handles the version-mismatch case particularly well: if the WASM module was recompiled with a different wazero version and the memory layout changed, replay from history still works because the event history is layout-independent.

#### Comparison table

| | Replay (current) | Full memory snapshot | Hybrid (marked vars) |
|---|---|---|---|
| Resume latency | O(n) steps | O(1) | O(1) typically, O(n) on fallback |
| Transformer complexity | Medium | Very High (Go runtime coupling) | High (type-specific serialization) |
| History compaction urgency | High (O(n) replay cost) | Low (snapshot is sufficient) | Low |
| Workflow author burden | None | None | Must mark checkpointable vars |
| Portability across wazero versions | Yes (replay is deterministic) | No (memory layout is runtime-version-specific) | Limited (careful serialization of marked vars helps) |
| Handles arbitrary Go types | Yes (replay discovers them) | No (need full runtime dump) | Limited to serializable types |
| Cold-storage replay performance | Must decompress and replay all steps | N/A (snapshot replaces history for resume) | Snapshot avoids cold-step decompression |
| Event history still needed? | Yes (source of truth) | Yes (for audit, debugging) | Yes (source of truth, fallback) |

#### Recommendation

The hybrid approach is promising but adds roughly 4-6 weeks to the transformer implementation plan — designing the annotation syntax, generating type-specific serializers for Go's type system, handling edge cases (pointers, interface values, cyclic references), and implementing the fallback mechanism. This is not justified in the initial build when replay performance is acceptable for most workloads.

The right time to revisit this is after the transformer is built and the system is running real workflows. At that point, instrumentation will reveal whether replay latency is an actual bottleneck for your workload profile. If a workflow runs 50,000 steps and takes 500ms to replay on every resume, the investment in snapshotting starts to make sense. If typical workflows have 10-50 steps and replay in under 1ms, it never will.

The event history remains the source of truth either way. A snapshot is a cache — valuable when it works, harmless when it does not.

---

### 9.2 WASM vs. Compact Intermediate Representation

The current design stores compiled WASM binaries in the database. A simple workflow is ~3 MB with standard Go and ~200 KB with tinygo. The alternatives explored here ask whether storing something other than WASM would simplify the pipeline, reduce storage, or improve portability.

#### Option A: Store source code, recompile on demand

Eliminates WASM storage entirely — just store the Go source. Each worker compiles on load (or caches compilation). This is appealing in theory but fails in practice for three reasons:

- **Compilation speed.** Go compilation takes seconds to minutes. A worker that loads a workflow on every claim (potentially thousands per second) cannot afford synchronous compilation. Pre-compilation caching would be required, at which point we are back to storing compiled artifacts.
- **Non-deterministic output.** Different Go versions, different target OS/arch flags, and different tinygo versions produce different WASM binaries from the same source. Replay correctness requires byte-for-byte identical WASM. Pin-the-Go-version is a possible mitigation but defeats the purpose of storing source (you cannot upgrade the toolchain without recompiling every stored workflow).
- **Toolchain dependency on every worker.** Every worker node would need the Go compiler (and tinygo, and the correct version) installed. This complicates worker deployment and version management.

**Verdict:** Non-starter. The operational complexity and replay-correctness risks far outweigh the storage savings.

#### Option B: Store an analyzed AST or SSA representation

The transformer already parses Go and builds a call graph. Instead of generating WASM bindings and compiling, the transformer could store the analyzed AST. Workers would interpret the AST directly (tree-walking interpreter) or JIT-compile it to WASM at load time.

AST storage is tiny: a few KB for a typical workflow. But interpreting Go ASTs correctly is extraordinarily difficult. Go's semantics include:

- Defer and panic/recover with stack unwinding
- Closures with captured variables
- Interface dispatch (dynamic method lookup)
- Reflection (`reflect` package)
- Goroutines and channels (if supported)
- Type assertions and type switches
- Method sets and embedding

Building a correct Go interpreter is a multi-year project. The JIT-to-WASM path is equally complex — it requires reimplementing the Go runtime's concurrency model, memory model, and type system on top of WASM. The existing Go toolchain already solves this problem by compiling Go to WASM. There is no advantage to rebuilding it.

**Verdict:** Non-starter. The existing Go-to-WASM compiler is the right solution for executing Go code. Building a competing Go runtime inside a WASM interpreter serves no purpose.

#### Option C: Store a custom bytecode / state machine IR

The transformer compiles Go to a custom intermediate representation (IR) designed for durable execution. The IR is a control flow graph with nodes for: durable call, condition, loop, variable assignment, return, error handling, compensation. Workers execute the IR via a lightweight interpreter.

The IR can be very compact: a workflow with 10 durable calls might be under 5 KB. The interpreter is deterministic by construction. Static analysis is trivial because the IR encodes the program's structure at a higher level than WASM.

But this approach has a fundamental problem: **library support.** A workflow that calls `encoding/json.Marshal`, `strings.HasPrefix`, or `fmt.Sprintf` inside a loop cannot use those functions unless they are also compiled to the custom IR. The entire Go standard library and any third-party dependency would need to be recompiled to IR. This is not merely a large effort — it is unbounded. Every library that the workflow imports transitively must be compiled to IR, and any library that uses Go features the IR does not support (goroutines, reflection, cgo) cannot be used at all.

Building a compiler from Go to a custom IR is feasible for a restricted subset of Go (no standard library, no third-party dependencies). That is a significant regression from the current design, which supports any WASM-compatible Go code, including most of the standard library.

**Verdict:** Feasible for a toy system. Impractical for real-world use where workflows need standard library functions and third-party packages.

#### Option D: WASM + compact metadata (hybrid — recommended enhancement)

Keep WASM as the executable artifact (sandbox, determinism, Go library compatibility, mature toolchain). Additionally, generate a compact metadata blob alongside the WASM that describes the workflow's structure:

- Control flow graph (durable call sites, branches, loops)
- Variable types and lifetimes
- Allowlist of permitted operations
- Source location annotations for each durable call
- Parameter and return types for each durable function

Store both the WASM blob and the metadata blob in `workflow_defs`:

```sql
ALTER TABLE workflow_defs ADD COLUMN metadata JSONB;
```

The metadata is optional — if missing, the system still works. It enables:

- **Fast static checks.** Does this workflow call any deprecated operations? Query the metadata without loading the WASM.
- **Workflow search indexing.** Which workflows transitively call `payments.Charge`? The metadata encodes the call graph as structured data.
- **UI features.** Show the workflow's step structure, parameter types, and error paths without running or decompiling the WASM.
- **Optimized replay.** Pre-load the expected call sequence from metadata, detect divergence early.

The metadata is generated by the transformer, which already has the call graph and type information during analysis. Adding a JSON-emitting step to the transformer is a small effort (~1 week) with significant tooling benefits.

#### Comparison table

| | WASM (current) | Source code | Custom IR | AST interpreter | WASM + metadata (hybrid) |
|---|---|---|---|---|---|
| Storage size | 200 KB – 3 MB | < 5 KB | < 10 KB | < 50 KB | 200 KB – 3 MB + < 5 KB metadata |
| Worker runtime | wazero (existing) | Go toolchain on every worker | Custom interpreter (~6 months) | Go interpreter (~2 years) | wazero + metadata parser |
| Go library compatibility | Yes (all WASM-compatible) | Yes (compile-time) | No (must rebuild all libs) | Partial | Yes (same as WASM) |
| Deterministic execution | Yes (WASM spec) | Depends on Go toolchain version | Yes (interpreter is deterministic) | Yes | Yes (WASM) |
| Sandbox security | Yes (WASM memory isolation) | No (native compilation) | Yes (interpreter sandbox) | Yes | Yes (WASM) |
| Execution speed | Near-native (wazero JIT) | Near-native | Slow (interpreted) | Very slow | Near-native |
| Build pipeline | go build (existing) | None (store source) | Custom compiler | Custom compiler | go build + metadata emit |
| Versioning story | WASM blob = versioned artifact | Source = versioned, rebuild on load | IR blob = versioned artifact | AST = versioned artifact | WASM blob = versioned artifact |
| Static analysis effort | High (parse WASM) | High (parse Go source) | Trivial (IR is designed for it) | Medium | Low (metadata is pre-analyzed) |
| Storage cost per 10K versions | $2.40/mo (tinygo) – $24/mo (std Go) | $0.01/mo | $0.02/mo | $0.08/mo | $2.40/mo + negligible |

#### Recommendation

Stay with WASM. The alternatives require building a compiler or interpreter for a language with Go's complexity, which is a multi-year project. The WASM approach leverages the existing Go toolchain — the transformer only needs to generate bindings, not implement Go semantics. The storage cost concern is overstated: at $0.08/GB/month for PostgreSQL storage, storing 10,000 workflow versions at 200 KB each costs roughly $2.40/month. Even at 100,000 versions, it is $24/month. The operational simplicity of using a standard compilation target outweighs the storage savings of a custom IR.

However, **Option D (WASM + compact metadata)** has genuine value and should be part of the transformer implementation. Generating a structured metadata blob from the call graph and type analysis that the transformer already performs is low-hanging fruit. It enables static analysis, workflow search, UI features, and optimized replay without changing the execution model.

Storage cost is not the reason to consider alternatives — WASM is cheap enough. The barriers to custom IRs are not engineering effort but the unbounded problem of library support. The metadata enhancement addresses the real gap (static analysis and tooling) without touching the execution path.

---

### 9.3 Go Syntax vs. Custom DSL

The current design uses Go syntax with restrictions. The alternative is a custom domain-specific language purpose-built for durable execution.

#### The case for a DSL

The restrictions in section 5.4 would become first-class features in a DSL. A language designed specifically for durable execution could have:

- **First-class durable calls.** The DSL could use a dedicated syntax for external operations, eliminating the need for the `h.DurableCall(...)` convention and the threading of `*HostCalls` through every function in the call graph.
- **Built-in compensation.** Sagas, compensations, and rollbacks could be language primitives rather than manual error-handling patterns.
- **Static determinism guarantees.** The compiler could reject non-deterministic constructs (map-iteration-dependent control flow, floating-point branching, unseeded randomness) at compile time with clear error messages.
- **Compact compilation to IR.** A purpose-built DSL compiles more naturally to a compact IR (section 9.2, Option C) than full Go does, because the DSL's semantics are a subset of what the IR supports.
- **Visual editing.** A DSL with a restricted structure could be rendered and edited in a visual workflow designer (like AWS Step Functions or Camunda), making workflows accessible to non-developer operators.

#### The case against (why we chose Go)

**Ecosystem is the killer.** A new language has no libraries. Every common operation — JSON parsing, string manipulation, date handling, cryptographic hashing, HTTP client helpers — must be rebuilt or wrapped. The Go standard library is thousands of packages, and the third-party ecosystem is millions. A DSL cannot offer any of this without an FFI bridge to Go, at which point the DSL is syntactic sugar over Go, not an independent language.

**Developer adoption is much harder.** "Learn our DSL" is a heavier ask than "write Go with these restrictions." Go is already the 10th most-used language on GitHub. New Go developers can be productive in the system on day one. A DSL requires ramp-up time, training materials, examples, and a mental context switch when moving between workflow code and non-workflow code.

**Tooling must be built from scratch.** The Go compiler, formatter (`gofmt`), linter (`staticcheck`, `golangci-lint`), IDE support (gopls, LSP), and testing framework (`go test`, `testing` package) all work with Go syntax. A DSL would need all of this built from scratch or adapted via language servers — each a significant engineering investment.

#### A middle ground: Go-like DSL that compiles to Go

Define a language that looks like Go but adds durability-specific constructs. Compile it to standard Go code that uses the `HostCalls` interface. The compiled Go then compiles to WASM via the existing pipeline. Example syntax:

```go
workflow PlaceOrder(userID string, cart []CartItem) (string, error) {
    if len(cart) == 0 {
        return "", errors.New("cart is empty")
    }

    reservation := validateAndReserve(userID, cart)?
    charge := processPayment(userID, reservation.TotalCents)?
    trackingID := fulfillOrder(reservation, charge)?
    return trackingID, nil
}
```

The `?` operator means "if this call returns an error, run compensation and exit." The compiler generates the try/catch/compensate boilerplate. The `validateAndReserve`, `processPayment`, and `fulfillOrder` functions are regular Go functions that call `h.DurableCall(...)` — they live outside the DSL and are compiled to WASM through the normal Go pipeline.

Advantages: the surface syntax is cleaner, and the `?` operator eliminates the manual error-propagation boilerplate that makes some workflows verbose. But the underlying execution is the same Go+WASM pipeline. Existing Go libraries can still be used. Go's tooling works on the generated code.

Disadvantages: an extra compilation step. Developers write in the DSL, the DSL compiler produces Go, the Go compiler produces WASM. Debugging requires mapping between three representations. The DSL adds complexity to the build pipeline (the developer must now learn the DSL syntax and understand how it maps to Go) for marginal benefit over Go with good error messages.

#### Another middle ground: Go + compiler-enforced restrictions with directives

Keep Go syntax exactly. The transformer enforces restrictions at build time with clear error messages. Add comment directives that document intent and enable better validation:

```go
//durable:workflow
func PlaceOrder(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    //durable:call
    reservation, err := validateAndReserve(h, userID, cart)
    if err != nil {
        return "", err
    }

    //durable:call
    charge, err := processPayment(h, userID, reservation.TotalCents)
    if err != nil {
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("payment failed: %w", err)
    }
    // ...
}
```

These directives serve as documentation and allow the transformer to verify developer intent against its static analysis:

- A function marked `//durable:workflow` that does not transitively call any durable function is an error — either the annotation is wrong or the workflow is a no-op.
- A call marked `//durable:call` that does not resolve to a function in the durable closure is a warning — either the annotation is stale or the developer expected this call to be durable when it is not.
- A `//durable:call` annotation on a function that directly calls `h.DurableCall(...)` but is called from a non-durable context is an error — the developer intended this as a durable boundary but the transformer disagrees.

This is essentially the current design with better tooling. The difference is in presentation: instead of "Go minus what WASM does not support," it is "durable Go — a verified subset of Go for durable execution." The error messages from `cleat vet` become the developer's primary interaction with the restriction system, replacing the trial-and-error of "does this compile to WASM?" with authoritative, specific guidance.

#### Comparison table

| | Go + restrictions (current) | Go-like DSL compiled to Go | Pure custom DSL |
|---|---|---|---|
| Developer learning curve | Low (Go devs already know Go) | Medium (new syntax, familiar semantics) | High (new language, new tooling) |
| Library ecosystem | Full Go ecosystem (WASM-compatible subset) | Full Go ecosystem (via compilation to Go) | None (must build or FFI everything) |
| Tooling (IDE, lint, test) | Standard Go tooling | Standard Go tooling (on generated code) | Must build from scratch |
| Error message quality | Good (Go compiler + transformer warnings) | Better (DSL-specific errors for durability concepts) | Best (language designed for the domain) |
| Compensation syntax | Manual error handling | `?` operator + auto-compensation | Built-in saga/compensation |
| Build pipeline complexity | 2 steps (transform + go build) | 3 steps (dsl -> go -> wasm) | 2 steps (dsl -> wasm) |
| Effort to build | ~12 weeks (transformer) | ~20 weeks (transformer + DSL compiler) | ~52+ weeks (language + runtime + ecosystem) |
| Visual editing support | Difficult (full Go is too expressive) | Possible (restricted syntax) | Natural (designed for it) |
| Risk of restriction confusion | Medium (developers discover restrictions reactively) | Low (restrictions are syntactic) | None (language is the restriction) |

#### Recommendation

Stay with Go syntax. The pure DSL is a multi-year project that would produce a language with no library ecosystem, no tooling, and a steep adoption curve — for a benefit (cleaner compensation syntax, static determinism guarantees) that can largely be achieved through better transformer error messages and the annotation directives described above.

The middle-ground DSL (Go-like syntax compiled to Go) adds an extra build step and a new syntax to learn, for marginal benefit over the current approach with `//durable:workflow` and `//durable:call` directives. The `?` operator for automatic compensation is genuinely nice, but not worth adding a language to the pipeline.

The single highest-impact investment for the developer experience is not a DSL — it is making the transformer's error messages exceptional. A developer who writes Go naturally and gets a clear, specific error when they violate a restriction ("durable call found inside a non-durable function: `order.go:42` — calls `net/http.Do` which is not available in WASM") has a better experience than a developer who must learn a new language to avoid those errors in the first place.

Add `//durable:workflow` and `//durable:call` directives to the transformer design. They serve as documentation, enable cross-referencing between developer intent and static analysis, and make the restriction system feel like an assistant rather than a gatekeeper. This is a small addition (~1 week to the transformer plan) that improves the developer experience more than inventing a new syntax.

---

### Summary of Recommendations

| Question | Recommendation | Rationale |
|---|---|---|
| Replay vs. checkpoint serialization | Stay with replay. Add hybrid snapshot after the transformer is built, if profiling shows replay latency is a bottleneck. | Snapshot adds significant transformer complexity (~4-6 weeks) for an optimization that may not be needed. The event history is the source of truth either way. |
| WASM vs. compact IR | Stay with WASM. Add compact metadata generation alongside WASM (~1 week). | The alternatives require multi-year projects to rebuild what the Go toolchain already provides. The metadata enhancement enables static analysis and tooling without changing the execution model. |
| Go syntax vs. custom DSL | Stay with Go syntax. Add `//durable:workflow` and `//durable:call` directives for better validation (~1 week). | A DSL cannot compete with Go's ecosystem, tooling, or developer familiarity. The best path is making the transformer's error messages exceptional, not inventing a new language. |

The common thread: **the current architectural choices are correct for the initial build.** WASM is the right compilation target, replay is the right durability mechanism, and Go is the right language. The alternatives considered above are either multi-year projects that duplicate existing infrastructure (custom IR, pure DSL) or optimizations that should be deferred until instrumentation justifies them (checkpoint serialization).

The one concrete change to the current plan is **adding metadata generation and annotation directives to the transformer** (~2 weeks total effort). These are low-risk additions that enable significant tooling improvements without touching the execution path. Everything else in this section is a matter of priority: build the system, measure it, and revisit these alternatives when you have data.

---

## 10. Leaning Into Structural Advantages

The comparison in Section 4 frames this system as a Temporal alternative that trades maturity for operational simplicity. That framing is honest but incomplete. It suggests the trade is one-dimensional: "give up Temporal's ecosystem, get a simpler deployment." In fact, there are capabilities this architecture enables that Temporal cannot replicate regardless of engineering investment — they are consequences of architectural choices that Temporal made differently, not features Temporal could add.

This section explores those structural advantages. They fall into four categories: versioning as data rather than operations, SQL-based visibility as a first-class feature, the WASM sandbox as a platform foundation, and function-call composability without ceremony. Each is examined with concrete examples and honest assessment of the gaps that remain.

### 10.1 Versioning as Data, Not Operations

Temporal's versioning model is constrained by its architecture. Workflow code runs on workers as native Go processes, so deploying a new version means deploying new worker binaries, configuring task queue routing, and keeping old workers alive until all in-flight workflows drain. Temporal's `GetVersion()` API provides SDK-level branching for minor code changes, but it does not eliminate the operational burden of worker pool lifecycle management — it defers it. Old branches must remain in the codebase indefinitely, and developers must reason about which branches are still reachable.

This system's versioning is different because the code artifact (WASM blob) is decoupled from the execution runtime (worker). Deploying a new version is a database `INSERT`. Rolling back is an `UPDATE`. Workers are a stable runtime that changes only when the `HostCalls` interface changes — a rare event by design.

The WASM-in-DB approach is not merely operationally simpler. It opens versioning capabilities that Temporal's worker-pool model cannot offer, because Temporal cannot version code independently of running infrastructure.

#### 10.1a Gradual Rollout / Canary Deployments

A new workflow version should not go from zero to 100% traffic in one deploy. Add a `rollout_percent` column to `workflow_defs`:

```sql
ALTER TABLE workflow_defs ADD COLUMN rollout_percent INT NOT NULL DEFAULT 100;
```

When choosing a version for a new workflow instance, the worker queries the pool of non-deprecated versions and selects one weighted by `rollout_percent`:

```sql
SELECT name, version, rollout_percent FROM workflow_defs
WHERE name = 'PlaceOrder' AND deprecated = false;
```

Rollout workflow:
1. `INSERT INTO workflow_defs (name, version, rollout_percent, wasm_bytes) VALUES ('PlaceOrder', 2, 1, ...)` — v2 gets 1% of new workflows.
2. Monitor error rates, latency, and business outcomes for an hour.
3. `UPDATE workflow_defs SET rollout_percent = 25 WHERE name = 'PlaceOrder' AND version = 2` — increase to 25%.
4. `UPDATE workflow_defs SET rollout_percent = 100, deprecated = true WHERE name = 'PlaceOrder' AND version = 1` — full rollout, v1 deprecated for new instances.
5. Rollback: `UPDATE workflow_defs SET rollout_percent = 0 WHERE name = 'PlaceOrder' AND version = 2` — zero new workflows get v2. No worker restart needed.

Temporal can approximate this with task queue routing: deploy v2 workers to a separate task queue, configure a `WorkerOptions.DynamicRoutingRule`, and gradually shift traffic. But this requires operating parallel worker pools, monitoring their health, and reconfiguring routing rules at the infrastructure level. Every version change touches deployment pipelines. In this system, a version change is a SQL `UPDATE` that takes effect on the next worker claim cycle (within seconds).

#### 10.1b A/B Testing of Workflow Versions

For a configurable percentage of new workflow starts, execute both v1 and v2 on the same input. Compare:
- **Output equivalence:** Does v2 return the same result structure as v1?
- **Call sequence equivalence:** Does v2 call the same sequence of `(service, operation)` pairs?
- **Latency:** Is v2 faster or slower per step?
- **Error rates:** Does v2 fail on inputs v1 handled successfully?

The worker records comparison results in a dedicated table:

```sql
CREATE TABLE workflow_version_comparisons (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_name   TEXT NOT NULL,
    v1_version      INT NOT NULL,
    v2_version      INT NOT NULL,
    instance_id     UUID NOT NULL,
    input_json      JSONB NOT NULL,
    v1_result       JSONB,
    v1_error        TEXT,
    v2_result       JSONB,
    v2_error        TEXT,
    v1_duration_ms  INT,
    v2_duration_ms  INT,
    calls_match     BOOLEAN,  -- did both versions call the same service.operation sequence?
    results_match   BOOLEAN,  -- are the output JSON structures equivalent?
    created_at      TIMESTAMPTZ DEFAULT now()
);
```

This is genuinely impossible in Temporal without orchestrating parallel workflow execution externally. Temporal's architecture does not know what code a workflow is running — it only schedules tasks to workers. Running the same workflow against two different code versions requires explicitly launching two workflow instances, tracking both, and comparing results manually. In this system, the worker knows the code version because it loaded the WASM blob — comparison is a natural extension of the claim-and-execute loop.

#### 10.1c Shadow Mode / Dark Launch

Before committing to a canary rollout, run v2 in shadow mode alongside v1. For every v1 workflow start, the worker also starts a v2 instance with the same input. v2's event history is recorded, but its outputs are discarded — no external side effects. The host intercepts v2's `DurableCall` responses and writes them to a shadow event log without executing real API calls.

After a shadow period, query the results:

```sql
-- Find behavioral divergences between v1 and v2 in shadow mode
SELECT wi.id, wi.def_version,
       eh1.step, eh1.operation, eh1.response AS v1_response,
       eh2.response AS v2_response
FROM workflow_instances wi
JOIN event_history eh1 ON eh1.workflow_id = wi.id AND wi.def_version = 1
JOIN event_history eh2 ON eh2.workflow_id = wi.id AND wi.def_version = 2
WHERE wi.def_name = 'PlaceOrder'
  AND eh1.service != eh2.service OR eh1.operation != eh2.operation
  AND eh1.step = eh2.step;
```

Dark launch detects semantic regressions that unit tests miss — call ordering changes, different error handling paths, unexpected input-output mappings — without any production risk. In Temporal, shadow execution requires running two separate worker pools, routing the same task to both, and building external infrastructure to compare results. It is rarely done in practice for exactly this reason.

#### 10.1d Automatic Rollback on Error Rate Spike

The worker's monitoring loop tracks error rates per `(def_name, version)` using a sliding window over the last N checkpoints:

```sql
SELECT def_name, def_version,
       COUNT(*) FILTER (WHERE error IS NOT NULL)::float / COUNT(*) AS error_rate
FROM event_history
WHERE created_at > now() - interval '5 minutes'
GROUP BY def_name, def_version;
```

When v2's error rate exceeds v1's by a configurable threshold (e.g., 2x for 3 consecutive windows), the worker automatically sets v2's `rollout_percent` to 0 and emits an alert:

```sql
UPDATE workflow_defs SET rollout_percent = 0
WHERE name = 'PlaceOrder' AND version = 2
  AND (
    SELECT error_rate_v2 > error_rate_v1 * 2
    FROM (... sliding window query ...)
  );
RETURNING name, version;
```

Temporal can integrate with external monitoring (Datadog, Grafana) and manual task queue reconfiguration, but it has no built-in mechanism to connect error rate observations to deployment rollback. The feedback loop requires external orchestration. In this system, the worker already sees every checkpoint, already knows which version produced it, and already has write-access to the version configuration table. The feedback loop is a periodic query and an `UPDATE`.

#### 10.1e Cross-Version Result Validation

For a configurable subset of workflows, the worker executes both v1 and v2 on the same input, compares the full call sequence and return value, and flags any divergence. The comparison runs after the primary execution completes — it is a background validation, not a synchronous double-execution.

```sql
CREATE TABLE version_divergences (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_name   TEXT NOT NULL,
    instance_id     UUID NOT NULL,
    primary_version INT NOT NULL,
    check_version   INT NOT NULL,
    divergence_type TEXT NOT NULL,  -- 'call_sequence', 'return_value', 'error_difference'
    details         JSONB,
    created_at      TIMESTAMPTZ DEFAULT now()
);
```

This catches subtle behavioral regressions that escape unit tests: a change in retry logic that causes a different error to surface, a different ordering of compensating calls, a different default value for an optional parameter. Because the worker can load and execute any version of a workflow from the database, cross-version comparison is a natural capability. In Temporal, comparing two SDK versions requires deploying two worker pools, routing the same task to both, and building external result-collection infrastructure — a project most teams never undertake.

#### 10.1f Version Deprecation Automation

A background job queries for workflow versions that have zero in-flight instances and automatically marks them as deprecated:

```sql
WITH versions_with_zero_inflight AS (
    SELECT wd.name, wd.version
    FROM workflow_defs wd
    WHERE wd.deprecated = false
      AND NOT EXISTS (
        SELECT 1 FROM workflow_instances wi
        WHERE wi.def_name = wd.name
          AND wi.def_version = wd.version
          AND wi.status IN ('ready', 'running')
    )
)
UPDATE workflow_defs wd
SET deprecated = true
FROM versions_with_zero_inflight v
WHERE wd.name = v.name AND wd.version = v.version
RETURNING wd.name, wd.version;
```

When a version is auto-deprecated, the job sends a notification to the team Slack channel: "PlaceOrder v1 has zero in-flight instances and has been deprecated. The WASM blob is retained in the database for replay and debugging. No action required."

This eliminates the operational question every Temporal team faces: "Are we still running v1 anywhere?" The answer is a SQL query. Temporal teams must monitor worker pool traffic, check if any workers are still processing v1 task queues, and manually drain old pools. In this system, deprecation is automatic once the last workflow completes.

#### 10.1g Time-Travel Debugging

Because every workflow instance records its `(def_name, def_version)` and the database stores every version's WASM blob, any historical workflow can be replayed with the exact code that produced it. This enables a debugging workflow that Temporal cannot match without keeping every old worker binary:

1. The operator selects a failed workflow instance in the UI.
2. The debugging tool loads the WASM blob for `(def_name, def_version)` from `workflow_defs`.
3. The tool loads the full event history from `event_history`.
4. The tool replays the WASM module locally — on the operator's laptop or in a debug container — with the same wazero runtime version that executed it originally.
5. The operator sets breakpoints, inspects WASM memory at any step, and single-steps through the replay.

```bash
durable debug --workflow-id abc-123
# Starting replay of PlaceOrder v2 (WASM blob from 2026-03-15)
# [0] catalog.LookupItem → OK (5ms, from history)
# [1] inventory.Reserve → OK (12ms, from history)
# [2] payments.Charge → ERROR: "insufficient_funds" (8ms, from history)
# [3] payments.Refund → OK (4ms, from history, but Charge was a no-op — BUG)
# 
# Workflow returned error: "payment failed: insufficient_funds"
# Compensation refund issued but Charge never completed.
# 
> breakpoint step 2
> inspect request
```

This is not merely a debugging convenience. It is a structural capability: because the code artifact and the execution record are both stored in standard PostgreSQL tables, any tool that can read those tables can reproduce the exact execution. There is no Temporal Cloud API to query, no worker binary to re-deploy, no SDK version to match.

#### 10.1h Dependency Tracking

The transformer already computes the transitive closure of durable calls during build time. Instead of discarding this information after WASM generation, persist it as structured metadata:

```sql
CREATE TABLE workflow_dependencies (
    def_name    TEXT NOT NULL,
    def_version INT  NOT NULL,
    service     TEXT NOT NULL,
    operation   TEXT NOT NULL,
    call_count  INT,         -- how many distinct call sites call this operation
    PRIMARY KEY (def_name, def_version, service, operation)
);
```

Now operations teams can answer questions that are genuinely impossible in Temporal without running the code:

```sql
-- Which workflow versions call payments.Charge?
SELECT def_name, def_version FROM workflow_dependencies
WHERE service = 'payments' AND operation = 'Charge';

-- Which workflows are affected by the inventory.Reserve schema change?
SELECT DISTINCT def_name FROM workflow_dependencies
WHERE service = 'inventory' AND operation = 'Reserve';

-- Show me all workflows that call both payments.Charge AND shipping.CreateLabel
SELECT a.def_name, a.def_version
FROM workflow_dependencies a
JOIN workflow_dependencies b
  ON a.def_name = b.def_name AND a.def_version = b.def_version
WHERE a.service = 'payments' AND a.operation = 'Charge'
  AND b.service = 'shipping' AND b.operation = 'CreateLabel';
```

Temporal's SDK-based approach requires running the workflow to discover which activities it calls. Static analysis is possible in principle (parse the SDK source) but Temporal does not provide this capability out of the box. The transformer in this system already has the call graph — storing it as queryable metadata is a small addition with significant operational value.

**Versioning capability summary:**

| Capability | Temporal | This system | Why Temporal can't easily replicate |
|---|---|---|---|
| Deploy new version | Build + deploy worker pool, configure task queue routing | `INSERT INTO workflow_defs` | Code runs on infrastructure; decoupling requires WASM |
| Canary rollout | Parallel worker pools with routing rules | `UPDATE SET rollout_percent = N` | No "deploy 1% of traffic" primitive in Temporal's architecture |
| A/B comparison | Manual: launch two workflows, compare results | Built into worker: load v1 and v2, compare | Temporal doesn't know which code version a worker is running |
| Shadow mode | Not available; side effects are real | Host intercepts calls, records only | Temporal's activities execute real side effects |
| Auto-rollback | External monitoring + manual task queue reconfig | `UPDATE SET rollout_percent = 0` | Feedback loop requires external orchestration |
| Cross-version validation | Two worker pools + external comparison | Worker loads both versions, compares internally | Code version is opaque to Temporal's runtime |
| Auto-deprecation | Manual drain of old worker pools | SQL query + `UPDATE SET deprecated = true` | Temporal ties code version to worker deployment |
| Time-travel debugging | Keep every old worker binary | Load WASM blob from DB, replay locally | Worker binary is not versioned as a retrievable artifact |
| Dependency tracking | Not available; would need to parse SDK source | Static analysis from transformer metadata | Temporal has no build-time call graph analysis |

### 10.2 SQL-Based Visibility as a Superpower

Temporal's visibility store is a separate service with its own query language and API. It can answer "what workflows are running?" and "what's the status of workflow X?" but it cannot `JOIN` with application data, aggregate across workflows with standard SQL, or integrate with the organization's existing analytics tooling.

This system's event history lives in PostgreSQL. That means every tool that speaks PostgreSQL — Grafana, Metabase, Superset, `psql`, custom scripts, ORMs — can query workflow execution data without any intermediate API or custom integration. This is not an incidental property; it is a consequence of using PostgreSQL as the infrastructure substrate.

The goal is to make SQL-based visibility a first-class feature, not a byproduct that operators discover by accident.

#### 10.2a Curated Views

Ship a standard set of PostgreSQL views that answer common operational questions without writing SQL from scratch:

```sql
-- Workflow summary with latest event status
CREATE VIEW v_workflow_status AS
SELECT
    wi.id,
    wi.def_name,
    wi.def_version,
    wi.status,
    wi.created_at,
    wi.updated_at,
    wi.custom_attributes->>'order_id' AS order_id,
    wi.custom_attributes->>'customer_id' AS customer_id,
    jsonb_object_keys(wi.custom_attributes) AS attribute_keys,
    wi.assigned_to,
    wi.heartbeat_at,
    wi.next_wake_at,
    CASE
        WHEN wi.status = 'running' AND wi.heartbeat_at < now() - interval '30 seconds'
        THEN 'zombie'
        ELSE NULL
    END AS health
FROM workflow_instances wi;

-- Failed workflows with last error
CREATE VIEW v_failed_workflows AS
SELECT DISTINCT ON (wi.id)
    wi.id,
    wi.def_name,
    wi.def_version,
    wi.custom_attributes,
    eh.step AS failed_at_step,
    eh.service AS failed_service,
    eh.operation AS failed_operation,
    eh.error AS last_error,
    eh.created_at AS failed_at,
    wi.created_at,
    wi.created_at - eh.created_at AS running_duration
FROM workflow_instances wi
JOIN event_history eh ON eh.workflow_id = wi.id
WHERE wi.status = 'failed'
ORDER BY wi.id, eh.step DESC;

-- Slowest operations across all workflows
CREATE VIEW v_slow_operations AS
SELECT
    service,
    operation,
    COUNT(*) AS call_count,
    percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms) AS p50_ms,
    percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms) AS p95_ms,
    percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms) AS p99_ms,
    COUNT(*) FILTER (WHERE error IS NOT NULL)::float / COUNT(*) AS error_rate
FROM event_history
WHERE created_at > now() - interval '24 hours'
GROUP BY service, operation
ORDER BY p95_ms DESC;
```

These views are part of the system schema, created on `durable init`. Operators query them with standard SQL — no learning curve, no custom query language.

#### 10.2b Materialized Dashboards

For dashboards that refresh frequently, use PostgreSQL materialized views to pre-compute aggregates:

```sql
CREATE MATERIALIZED VIEW mv_daily_workflow_stats AS
SELECT
    def_name,
    def_version,
    date_trunc('hour', created_at) AS hour,
    status,
    COUNT(*) AS count,
    AVG(EXTRACT(EPOCH FROM (updated_at - created_at))) AS avg_duration_seconds
FROM workflow_instances
WHERE created_at > now() - interval '7 days'
GROUP BY def_name, def_version, date_trunc('hour', created_at), status;

CREATE UNIQUE INDEX ON mv_daily_workflow_stats (def_name, def_version, hour, status);
```

Refresh the materialized view every 60 seconds via `pg_cron` or a worker-side periodic query. Grafana connects to PostgreSQL directly and visualizes the materialized view — sub-millisecond query times for dashboard refreshes, no application backend needed.

This is genuinely simpler than Temporal's visibility approach, which requires exporting data from the visibility store to a separate analytics database for complex queries. Temporal's ListWorkflowExecutions API supports filters and pagination but not aggregation, joins, or window functions. Any cross-workflow analysis requires an external pipeline.

#### 10.2c Application Data JOINs

The event history is most powerful when it can JOIN with application domain tables. To enable this, workflows should populate a `business_keys` JSONB column on `workflow_instances`:

```sql
ALTER TABLE workflow_instances ADD COLUMN business_keys JSONB;
```

Workflows populate business keys at start time:

```go
h.SetBusinessKeys(map[string]string{
    "order_id":    orderID,
    "customer_id": customerID,
    "region":      region,
})
```

Now operators can JOIN workflow data with application tables:

```sql
-- Show payment events for all orders from German customers
SELECT eh.step, eh.operation, eh.request, eh.response, eh.duration_ms
FROM event_history eh
JOIN workflow_instances wi ON wi.id = eh.workflow_id
JOIN customers c ON c.id = wi.business_keys->>'customer_id'
WHERE c.country = 'DE'
  AND eh.service = 'payments'
  AND eh.created_at > now() - interval '7 days'
ORDER BY eh.created_at DESC;

-- Find orders where payment succeeded but shipment failed
SELECT wi.business_keys->>'order_id' AS order_id,
       wi.def_version,
       eh_pay.created_at AS payment_time,
       eh_ship.created_at AS shipment_time,
       eh_ship.error AS shipment_error
FROM workflow_instances wi
JOIN event_history eh_pay
  ON eh_pay.workflow_id = wi.id
 AND eh_pay.service = 'payments' AND eh_pay.operation = 'Charge'
 AND eh_pay.error IS NULL
JOIN event_history eh_ship
  ON eh_ship.workflow_id = wi.id
 AND eh_ship.service = 'shipping' AND eh_ship.operation = 'CreateLabel'
 AND eh_ship.error IS NOT NULL;
```

Temporal's visibility store stores a fixed set of search attributes (string key-value pairs indexed for search). These cannot be used in `JOIN` operations, aggregations, or arbitrary queries. They are search filters, not a queryable data model. The business keys column in this system is a PostgreSQL JSONB column — it benefits from full SQL query capability, GIN indexing, and composability with the rest of the database.

#### 10.2d Compliance Queries as a Feature

GDPR Article 15 gives data subjects the right to know what data an organization holds about them and how it has been processed. This requires tracing every API call that touched a specific data subject's data.

This is a standard SQL query in this system:

```sql
-- Every API call that touched customer X's data
SELECT
    wi.id AS workflow_id,
    wi.def_name,
    wi.def_version,
    eh.created_at AS event_time,
    eh.service,
    eh.operation,
    eh.request,
    eh.response
FROM workflow_instances wi
JOIN event_history eh ON eh.workflow_id = wi.id
WHERE wi.business_keys->>'customer_id' = 'c12345'
ORDER BY eh.created_at;
```

A compliance officer can run this query directly in any PostgreSQL client. The result is a complete, tamper-evident record of every interaction that involved that customer — what was requested, what was returned, when it happened, and which workflow version was responsible.

Temporal's visibility store cannot answer this question without custom instrumentation. You would need to add a search attribute for `customer_id`, ensure every workflow sets it, and then export the workflow list to an external system that queries each workflow's event history individually. Each event history query is a separate API call. For a customer with hundreds of related workflows, this means hundreds of API calls and significant client-side assembly.

For GDPR, SOC 2, or SOX compliance audits, the SQL-based approach is not a convenience — it is a structural advantage that reduces the engineering effort of compliance from a dedicated project to a `SELECT` statement.

### 10.3 WASM Sandbox as a Platform Enabler

Temporal trusts workflow code completely. Activities run in the same process as the worker, with the same operating system permissions, the same filesystem access, and the same memory space. This is a reasonable default for a single-team deployment, but it limits what Temporal can offer in multi-tenant or compliance-sensitive environments.

This system compiles workflow code to WASM and executes it in a wazero sandbox. The WASM module cannot access the filesystem, the network, or the host process's memory. Every interaction with the outside world goes through the `HostCalls` interface, which the worker controls completely. This sandbox is not a security feature to add later — it is a structural consequence of the WASM execution model.

#### 10.3a Multi-Tenant Workflow Execution

Different teams' workflows run in isolated WASM sandboxes within the same worker process. Team A's workflow cannot crash Team B's worker, cannot access Team B's memory, and cannot read Team B's secrets. If Team A's workflow enters an infinite loop, the wazero runtime enforces a configurable CPU cycle limit and kills the module — Team B's workflows on the same worker continue unaffected.

This eliminates the need for separate worker pools per team. A single worker pool handles workflows from the payments team, the inventory team, and the shipping team simultaneously. Temporal requires separate worker pools (or separate task queues with careful routing) to provide similar isolation, because Temporal activities are native code running in the worker process — a segfault or memory corruption in one activity takes down all activities on that worker.

The isolation is not perfect — the worker process itself is shared, and a worker crash (e.g., OOM from the host process, not from WASM) takes down all workflows on that worker. But the surface area for cross-tenant impact is dramatically smaller than Temporal's model.

#### 10.3b Third-Party / Untrusted Workflow Execution

Because the WASM sandbox enforces operation-level authorization (Section 8.8), a platform operator can let customers upload their own workflow code with precise control over what it can do:

```sql
INSERT INTO workflow_defs (name, version, wasm_bytes, operation_allowlist)
VALUES ('customer_123_approval_flow', 1, $wasm, '{
    "notifications": ["SendEmail"],
    "approvals":     ["SubmitForReview", "CheckStatus"],
    "audit":         ["LogEvent"]
}');
```

The customer's workflow can send email notifications, submit approvals, and log audit events — but it cannot call `payments.Charge`, `inventory.Reserve`, or any other sensitive operation. The allowlist is enforced by the host at runtime, and the WASM module has no way to bypass it.

This enables a new category of product: **workflow-as-a-service**, where a platform provides a durable execution substrate and customers write their own business logic within a sandboxed environment. The platform operator:

- Controls which external services the customer's code can reach
- Audits every operation the customer's code attempted (including rejected operations)
- Enforces resource limits (memory, CPU cycles, concurrent instances) per customer
- Charges based on workflow runs, WASM storage, or both

Temporal Cloud does not support untrusted workflow execution. Customer code runs in the customer's own workers, outside Temporal's control. Temporal Server operators cannot sandbox workflow code because it runs natively. The WASM sandbox in this system is not a feature — it is a fundamental architectural property that enables a business model Temporal cannot offer.

#### 10.3c Resource Limits Per Workflow

WASM modules in wazero have bounded resource consumption that the host can configure per instance:

- **Memory limit:** A WASM module cannot allocate beyond its configured maximum memory (e.g., 64 MB). A workflow with a memory leak is killed by the runtime, not by the OOM killer.
- **CPU limit:** Wazero supports gas metering — each WASM instruction consumes a configurable gas cost. When the gas budget is exhausted, execution stops. An infinite loop in workflow code is bounded.
- **No filesystem access:** WASM has no OS-level filesystem access. A workflow cannot read `/etc/passwd`, write to `/tmp`, or open a network socket.
- **No goroutine leakage:** The Go runtime inside WASM manages its own goroutines, but they are invisible to the host OS. A workflow that spawns unbounded goroutines consumes only its own memory allocation.

These resource limits mean that a single misbehaving workflow cannot disrupt other workflows on the same worker, cannot exhaust the worker's memory, and cannot consume unbounded CPU. In Temporal, an activity that allocates memory in a tight loop can OOM the entire worker process, taking down all workflows on that worker. Temporal provides no per-workflow or per-activity resource isolation.

#### 10.3d Auditability of Workflow Behavior

Because all I/O goes through the host's `DurableCall`, the host has a complete, tamper-evident audit log of every external interaction the workflow attempted — including calls that were rejected by the allowlist:

```sql
-- Every operation attempted by workflow abc-123, including rejected ones
SELECT step, service, operation, request, error, created_at
FROM event_history
WHERE workflow_id = 'abc-123'
  AND event_type IN ('durable_call', 'rejected_call')
ORDER BY step;
```

A `rejected_call` event is recorded when the workflow attempts to call an operation that is not in its `operation_allowlist`. The host returns an error to the WASM module, but the attempt is recorded permanently:

```json
{
  "step": 4,
  "event_type": "rejected_call",
  "service": "payments",
  "operation": "Refund",
  "request": "{\"charge_id\": \"ch_123\", \"amount\": 2999}",
  "error": "operation 'payments.Refund' is not in the allowlist for PlaceOrder v2",
  "created_at": "2026-04-15T14:30:00Z"
}
```

This audit trail is built into the architecture. In Temporal, auditing which activities a workflow attempted requires modifying the activity implementation to emit audit events — a manual, error-prone process that is easily skipped or misconfigured. In this system, auditing is a side effect of the sandbox boundary: the host is the only way in or out, and it records everything.

**WASM sandbox capability summary:**

| Capability | Temporal | This system |
|---|---|---|
| Multi-tenant isolation | Separate worker pools per tenant | WASM sandboxes within a shared worker pool |
| Untrusted code execution | Not supported | Sandboxed WASM with operation allowlist |
| Per-workflow resource limits | None (native code in worker process) | Bounded memory, CPU (gas metering), no FS access |
| Audit log of all attempted operations | Manual instrumentation | Automatic: host records every call including rejections |
| Workflow crash isolation | Activity crash kills entire worker | WASM module crash is contained |

### 10.4 Composability Without Ceremony

Temporal's workflow/activity split is a well-intentioned abstraction that creates operational friction. Activities must be registered, have typed interfaces separate from their implementation, and cannot call other activities directly. Cross-activity composition requires child workflows (with separate event histories) or inline activity calls from the workflow function. The result is that Temporal codebases tend to have a flat activity layer — activities rarely compose because composing them is difficult.

This system eliminates the workflow/activity distinction entirely. Any function can call any other function. The transformer discovers the durable closure automatically. Composition is function calls, not child workflow orchestration.

#### 10.4a Shared Function Libraries

A team publishes `durable-payments` — a Go package of durable functions for payment processing. Other teams import it and call its functions directly:

```go
import "internal/durable-payments"

func PlaceOrder(h *cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    total := calculateTotal(cart)
    
    method, err := payments.GetDefaultMethod(h, userID)
    if err != nil {
        return "", err
    }
    
    charge, err := payments.Charge(h, method.ID, total)
    if err != nil {
        return "", err
    }
    
    return charge.ID, nil
}
```

The transformer analyzes `PlaceOrder`, discovers that it calls `payments.GetDefaultMethod` and `payments.Charge`, then transitively discovers what those functions call. The full call graph — spanning multiple Go packages — is compiled into a single WASM module. The developer does not register activities, does not define typed interfaces separate from implementations, and does not think about the transitive closure.

This is impossible in Temporal. Activity implementations can be shared as Go functions, but they cannot call other activities — activities are opaque to the Temporal runtime when called directly. The workflow must orchestrate all activity calls explicitly. A `payments.Charge` function that internally calls `payments.ValidateMethod`, `ledger.CreateTransaction`, and `notifications.SendReceipt` would need to be either a workflow (with its own event history) or three separate activities called from the parent workflow.

The difference compounds: in this system, building a library of durable functions is as natural as building any Go library. In Temporal, building a library of composable activities requires careful API design around the workflow/activity boundary.

#### 10.4b Automatic Compensation Scoping

In a function call graph where A calls B which calls C, and C's durable call fails, the compensation chain runs C's cleanup → B's cleanup → A's cleanup automatically. With `DurableDefer` (Section 8.6b), cleanup is registered at resource acquisition time:

```go
func PlaceOrder(h *cleat.HostCalls, userID string) error {
    reservation, err := reserveInventory(h, userID)
    if err != nil {
        return err
    }
    h.DurableDefer(func() error {
        return releaseReservation(h, reservation.ID)
    })

    charge, err := processPayment(h, userID, reservation.TotalCents)
    if err != nil {
        return fmt.Errorf("payment failed: %w", err)
        // releaseReservation runs automatically via DurableDefer
    }
    h.DurableDefer(func() error {
        return refundPayment(h, charge.ID)
    })

    // On success, nothing is compensated.
    // On error or cancellation, defers run LIFO: refund → release.
    return nil
}
```

Temporal supports the Saga pattern for compensation, but it requires explicit saga definition — the developer declares the compensation steps separately from the business logic. The Saga is a separate concern that must be maintained in parallel with the workflow definition. In this system, compensation is colocated with resource acquisition (the `DurableDefer` call is right after `reserveInventory`), and the scope is automatically correct because Go's closure captures the resource identifier. Adding a new resource with cleanup is a single `DurableDefer` call — not a saga step registration in a separate definition.

For deeply nested call graphs, `DurableDefer` scoping is genuinely cleaner than sagas. A helper function `processPayment` that acquires a payment lock and registers its own defer will have that defer run when `processPayment`'s scope exits — not when the entire workflow exits. This gives fine-grained compensation that matches the call graph structure without central coordination.

#### 10.4c Testing at Every Level

The `TestHost` (Section 8.13) lets you test a workflow at any level of the call graph:

```go
func TestPlaceOrder_EntireWorkflow(t *testing.T) {
    h := durable.NewTestHost(t)
    h.MockCall("inventory", "Reserve", `{"id": "res_123"}`, nil)
    h.MockCall("payments", "Charge", `{"id": "ch_123"}`, nil)
    h.MockCall("shipping", "CreateLabel", `{"tracking": "1Z999AA10123456784"}`, nil)
    h.MockCall("notifications", "Send", "", nil)
    
    result, err := PlaceOrder(h, "user_42", sampleCart)
    assert.NoError(t, err)
    assert.Equal(t, "1Z999AA10123456784", result)
}

func TestProcessPayment_SubtreeOnly(t *testing.T) {
    h := durable.NewTestHost(t)
    h.MockCall("payments", "GetDefaultMethod", `{"id": "pm_123"}`, nil)
    h.MockCall("payments", "Charge", `{"id": "ch_123"}`, nil)
    
    charge, err := processPayment(h, "user_42", 2999)
    assert.NoError(t, err)
    assert.Equal(t, "ch_123", charge.ID)
    // processPayment's internal DurableDefer cleanup is also tested
}

func TestProcessPayment_CompensationOnFailure(t *testing.T) {
    h := durable.NewTestHost(t)
    h.MockCall("payments", "GetDefaultMethod", `{"id": "pm_123"}`, nil)
    h.MockCall("payments", "Charge", "", errors.New("insufficient_funds"))
    // processPayment registers a DurableDefer that should NOT need to run
    // because Charge failed before it was registered... or SHOULD it?
    // The test host records which defers were registered and executed.
    
    _, err := processPayment(h, "user_42", 2999)
    assert.Error(t, err)
    calls := h.RecordedCalls()
    assert.Len(t, calls, 2) // GetDefaultMethod + Charge
    // No refund call — Charge never succeeded
}
```

The ability to test any subtree of the call graph — not just the top-level workflow — is a consequence of the function-call composition model. Every durable function is independently testable with the same `TestHost`. Temporal requires either a full Temporal test server (with all the infrastructure overhead) or mocking at the activity interface level. Testing a single activity in isolation is straightforward, but testing a specific combination of activities (the subtree) requires either a child workflow or careful test setup.

#### 10.4d Gradual Adoption

A team can adopt this system without a big-bang migration. Start by wrapping one API call in `h.DurableCall`:

```go
// Before: direct HTTP call
resp, err := http.Post("https://payments.internal/charge", "application/json", body)

// After: durable call — same logic, added durability
resp, err := h.DurableCall("payments", "Charge", string(body))
```

This single change adds the API call to the event history. If the worker crashes before recording the response, the call is retried. If it crashes after recording the response, the cached response is returned on replay. The developer gets durability for one call without changing any other code.

From there, expand outward:
1. Wrap the next API call in the same function.
2. Move the function to the durable closure (add `*HostCalls` parameter).
3. Add compensation with `DurableDefer`.
4. Split the function into composable helpers.
5. Extract a shared durable library.

Each step adds durability without changing the fundamental code structure. There is no "this function is now a workflow, not a regular function" threshold. Every durable call is individually additive.

Temporal cannot offer this gradual adoption. Using Temporal requires adopting the full SDK — workflow functions, activity functions, worker registration, task queues, and the SDK runtime. There is no "make one API call durable without the rest" mode. The entire function must be structured as a Temporal workflow, or none of it.

**Composability summary:**

| Aspect | Temporal | This system |
|---|---|---|
| Activity composition | Child workflows or inline from workflow function | Function calls at any depth |
| Shared libraries | Activity implementations without cross-activity calls | Full Go packages with composable durable functions |
| Compensation | Explicit Saga definition | `DurableDefer` at resource acquisition time |
| Testing | Test server or activity mocking | `TestHost` at any call-graph level |
| Adoption | Big-bang: adopt SDK fully | Incremental: one `DurableCall` at a time |

### 10.5 Where Temporal Still Wins

This section has argued that the architecture enables capabilities Temporal cannot replicate. That does not mean Temporal is the wrong choice for most teams. Honesty about where Temporal still dominates is essential for an informed decision.

**Massive scale.** Temporal runs at Uber, Netflix, and Snap — millions of workflows per day, tens of thousands of workers, global replication. This system's PostgreSQL-centric architecture is untested beyond a single-database scale. Temporal's history service partitions state across multiple Cassandra/MySQL/PostgreSQL shards. This system would need partitioning, read replicas, and connection pooling at similar scale — none of which are designed yet.

**Ecosystem maturity.** Temporal has a production-grade web UI, a managed Cloud offering (Temporal Cloud), SDKs in six languages with active maintenance, a community of thousands of developers, debugging tools, performance benchmarks, and a decade of production incidents that have hardened every edge case. This system has a design document, a WASM demo, and a transformer that does not exist yet.

**Operational experience.** Temporal has failed in every way a system can fail — corrupted event histories, split-brain in multi-datacenter deployments, replay divergence from SDK bugs, history service OOM from unbounded event sizes. Each failure produced a fix, a runbook entry, and a blog post. This system has none of that experience. The first team to run it in production will discover edge cases that Temporal's teams discovered years ago.

**Multi-language SDK support.** Temporal has production SDKs in Go, Java, TypeScript, Python, .NET, and PHP, all maintained by a dedicated team. This system has a Go transformer (planned) and a Rust transformer (planned). Other languages are theoretical until someone builds the transformer. The WASM interface approach reduces the per-language effort, but the gap between "six production SDKs" and "one prototype transformer" is real.

**Hiring and team familiarity.** Temporal experience is an increasingly common resume item. Teams adopting Temporal can hire engineers who already understand the programming model. This system's programming model is simpler (no workflow/activity distinction) but unfamiliar — there is no "durable Go" community, no Stack Overflow answers, no conference talks. The team that adopts this system owns the knowledge themselves.

**Trust and risk assessment.** Temporal is a known quantity. The risk of a production incident is well-understood because Temporal's failure modes are documented and mitigated. This system is new. The risk assessment for a production deployment must include "the system itself may have a critical bug that we discover at 3am." No design document can eliminate this risk — only production experience can.

### 10.6 The Pitch

For a team that:

- **Values deployment simplicity over ecosystem maturity.** If the team already runs PostgreSQL (or is comfortable operating it), the infrastructure footprint is one database instead of four services. Deploying a workflow is an `INSERT`. There is no CI/CD pipeline for worker pools per version.

- **Has complex nested business logic that does not fit the workflow/activity model.** If the business process has deeply nested function calls, cross-cutting compensation, and reusable sub-processes, the function-call composition model is genuinely more natural than Temporal's flat activity layer.

- **Needs SQL-based observability for compliance or operational reasons.** If GDPR, SOC 2, or internal audit requires tracing every API call that touched a customer's data, this system provides that as a `SELECT` query — not a multi-month instrumentation project.

- **Wants smooth version migration without managing worker pools.** If the team has been burned by Temporal versioning complexity — keeping old worker pools alive for weeks, debugging `GetVersion()` branching, or accidentally routing workflows to the wrong worker pool — the `INSERT`-and-forget versioning model directly addresses that pain.

- **Is willing to invest in a new approach for structural advantages.** This system is not a drop-in Temporal replacement. It is a different architectural philosophy with different strengths. The maturity gap is real, and bridging it requires engineering investment, tolerance for unknown unknowns, and a team that can debug its own infrastructure.

The capabilities described in this section — versioning as data, SQL-based visibility, WASM sandbox isolation, and function-call composability — are not features Temporal could add in a quarter or a year. They are consequences of architectural choices that this system made differently: WASM as the code artifact, PostgreSQL as the infrastructure, and function calls as the composition primitive. If these capabilities matter to your use case, the maturity gap may be worth bridging. If they do not, Temporal is the safer choice today, and it will remain the safer choice for the foreseeable future.

---

## 11. Summary

This design attempts to answer the question: what if durable execution could be provided by a database and a stable worker runtime, rather than a distributed system of custom stateful services?

The key architectural bets are:

1. **WASM as a versioned code artifact.** Workflow code is compiled, stored in the database, and loaded on demand. This decouples workflow deployment from worker deployment and solves the long-running workflow versioning problem without task queues or overlapping worker pools.

2. **PostgreSQL as the sole infrastructure.** The database serves as blob store, state store, work queue, and timer service. This eliminates the operational complexity of running separate queue, history, and matching services.

3. **The event history as universal observability.** Because every external interaction is recorded for durability, that same record provides structured logging, distributed tracing, metrics, and business-level querying without any instrumentation code in workflows.

4. **Composability through function calls.** There is no workflow/activity distinction. A workflow can call a helper function which calls another helper which makes an API call. The transformer discovers the transitive closure of durable functions. Libraries are reusable across workflows naturally.

The result is a system where writing a durable workflow feels like writing ordinary Go, deploying a new version is a database INSERT, and understanding what happened is a SQL query. The tradeoff is that this is a new system — it lacks Temporal's maturity, ecosystem, and battle-testing. But for teams that value operational simplicity and are willing to invest in a new approach, the design offers a genuine alternative to the current state of the art.
