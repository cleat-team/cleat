# Cleat Execution Project — Session Context

## What this project is

A design for a durable execution system ("Persistent Execution") that runs on an elastic cluster. Workflows are specified in near-standard Go, parsed with the Go standard library parser, analyzed for durable API call boundaries, and compiled to WASM modules stored in PostgreSQL. Stateless workers load the right WASM version for each workflow instance, handling checkpoint/replay and failover.

## Key architectural decisions and why

### WASM is about versioning, not security
The WASM compilation step is primarily about decoupling workflow code lifecycle from worker lifecycle. Workflows can run for weeks/months and must replay against the exact code they started with. WASM blobs stored in the DB by (name, version) let in-flight workflows keep using v1 while new workflows use v2, on the same worker binary. Security/sandboxing is a secondary benefit.

### PostgreSQL as the only infrastructure, not a message queue
Workflow steps are NOT independent queue messages. Replay requires full ordered event history. Compensation needs data from prior steps. Branching is data-dependent. A pure message queue per step doesn't work. PostgreSQL serves four roles: blob store (WASM), state store (event_history), work queue (SKIP LOCKED on workflow_instances), timer service (next_wake_at). Add Redis only at phase 4 if queue throughput becomes the bottleneck.

### Replay model, not checkpoint serialization
The system uses Temporal's replay approach — re-execute workflow from step 0, but return cached results for already-completed durable calls. This avoids serializing all local variables at each checkpoint. The tradeoff is that replay re-does computation between API calls; for I/O-bound workflows (API calls are the bottleneck), this is negligible.

### Explicit HostCalls boundary
The developer still marks API boundaries with `h.DurableCall(...)`. The original vision of transparent durability (parse Go, find all network calls, make them durable) proved impractical due to interface dispatch, reflection, and static analysis limits. The `HostCalls` struct with ~8 function fields is functionally similar to Temporal's `ExecuteActivity` but eliminates the workflow/activity distinction.

### Composability through function calls, not child workflows
Functions can call other functions at arbitrary depth, with durable API calls at any level. The transformer computes the transitive closure of durable functions. This eliminates Temporal's workflow/activity split and child workflow construct for most cases. Child workflows (separate event history, independent lifecycle) remain a P2 gap for cases that genuinely need them.

### Built-in observability from event history
Because every external interaction is recorded for durability, that same data provides structured logging, distributed tracing, metrics, and business queries. Zero instrumentation code in workflows. This fell out of the design naturally and is a key differentiator.

## Current state

### Designed and ready to implement (P0 + P1, 9 items)
- **P0:** Timers/sleep, retry policies, idempotency keys, dead letter queue, secrets/credentials
- **P1:** Signals/external events, workflow cancellation, DurableDefer (cleanup on exit), testing framework, transformer (Go → WASM)

### Concept only (P2 + P3, 7 items)
- **P2:** Queries, schema evolution, scheduling/CRON, child workflows
- **P3:** Multi-tenancy, workflow prioritization, history compaction

### Demo code (runnable, in wasm-demo/)
- `host/main.go` — Host runtime with checkpoint/replay, crash simulation, resume. Demonstrated correct replay with deterministic behavior.
- `worker/failover_worker.go` — Worker with DB failover handling: connection error detection, pause/resume, ownership verification, idempotent operations.
- `worker/versioned_loader.go` — Worker loading different WASM versions from the DB for different workflow instances.
- `cluster/design.go` — Data model and queue design walkthrough.
- `cluster/versioning/main.go` — Versioning problem analysis.
- `cluster/observability/main.go` — Built-in observability analysis.
- `cluster/performance/main.go` — Performance and cost analysis.
- `cluster/resilience/main.go` — PostgreSQL resilience analysis.
- `ast_transform_demo.go`, `ast_transform_demo_v2.go` — Early AST transformation experiments (superseded by the WASM approach).

All demos are standalone Go programs with simulated DB/APIs — no external dependencies beyond the Go standard library. Available at `/localssd/rcownie/cleat/wasm-demo/`.

### Design document
`/localssd/rcownie/cleat/cleat-execution-design.md` — 9 sections covering the full design with comparisons to Temporal, Azure Durable Functions, AWS Step Functions, Restate, and Inngest.

## Environment constraints
- Limited disk space (~145MB free). Go toolchain at `/tmp/go1.26.2/go/bin/go` (Go 1.26.2).
- Build with: `/tmp/go1.26.2/go/bin/go build -o /tmp/binary ./path/`
- No external Go dependencies can be installed (disk full). All demos use stdlib only.
- Working directory: `/localssd/rcownie/cleat/`

## User preferences observed
- Values operational simplicity over feature completeness. Prefers "one database instead of four services."
- Thinks in terms of migration paths and staged adoption — wants each phase to be incremental.
- Cares about correctness guarantees (idempotency, replay determinism, exactly-once semantics).
- Wants to eliminate boilerplate: DurableDefer emerged from asking "do we need a way to execute cleanup?" — the answer was yes, and it should be automatic.
- Asks architectural questions ("is this better or just different?") — wants honest comparisons, not cheerleading.
- Multi-language support is valuable but not urgent — Go first, Rust second.

## Key files
- `/localssd/rcownie/cleat/cleat-execution-design.md` — The full design document.
- `/localssd/rcownie/cleat/wasm-demo/` — Runnable demo code.
