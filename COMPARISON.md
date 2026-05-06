# Cleat vs Temporal.io vs DBOS: Comparative Analysis

## What Cleat Is

Cleat is a durable execution framework for Go. Workflows are written in near-standard Go, compiled to WebAssembly via a transformer pipeline, stored as versioned WASM blobs in PostgreSQL, and executed on stateless wazero-based workers with automatic replay, checkpointing, and failover. The core abstraction is a single `HostCalls` interface — there is no workflow/activity distinction.

---

## Cleat's Advantages

### 1. WASM-Based Versioning (unique — neither competitor does this)

Workflow code is stored as versioned WASM blobs in the database. In-flight instances carry a `(def_name, def_version)` pointer, so they always replay against the exact code they started with.

- **Deploy is an INSERT**: `durable deploy` writes a row to `workflow_defs`. No worker redeployment needed.
- **Rollback is an UPDATE**: Set the old version as active again. No re-deploying old worker pools.
- **Workers are a stable runtime**: The worker binary changes only when the `HostCalls` ABI changes (rare). In Temporal, you must keep old worker pools running for the lifetime of in-flight workflows — sometimes months. In DBOS, you use application-level patching and versioning decorators within the same deployment.

The design doc calls the worker "a JVM for workflows" — the analogy is apt. Workflow business logic changes are database operations, not service deployments.

### 2. No Workflow/Activity Distinction

Temporal enforces a hard split: workflows are deterministic (logic only), activities are non-deterministic (API calls, DB queries). This is a constant source of confusion — new users routinely put `time.Now()` or network calls in workflows and get non-determinism errors at replay.

Cleat has a single `HostCalls` interface. Every function in the call chain that needs to interact with the outside world does so through `h.DurableCall(...)`. The transformer computes the transitive closure of durable functions and validates correctness at build time.

DBOS has a similar simplification (`@workflow` and `@step` decorators), but cleat's static analysis catches problems at build time rather than runtime.

### 3. Auto-Threading

In Temporal, you must explicitly pass context/activity stubs through every level of your call chain. In DBOS, each step function must be separately annotated. In cleat, you can declare `var h durable.HostCalls` at package level and the transformer automatically propagates `h` as a first parameter through all callers (`testdata/autothread/order.go`). This eliminates boilerplate that both Temporal and DBOS developers deal with daily.

### 4. Single Infrastructure, Cleaner Separation

Both cleat and DBOS use only PostgreSQL (Temporal requires a full server cluster plus a separate database). This is a significant operational advantage in cost, deployment complexity, and failure modes.

Cleat's version is architecturally cleaner than DBOS's: DBOS embeds the execution runtime as a library in your application — your app IS the worker. Workflow recovery is tied to application startup, and you can't independently scale workflow execution. Cleat's stateless workers are separate processes that can scale independently from the applications they orchestrate.

### 5. Built-in Observability from Event History

Every external interaction is recorded in `event_history` for replay. That same data provides structured logging, distributed tracing, metrics, and business-level querying — with zero instrumentation code. Temporal has this too, but cleat's event history schema is simpler and more directly queryable via SQL. DBOS requires explicit logging instrumentation.

### 6. Multi-Language Potential via WASM

The WASM boundary (14 host function imports) means any language that compiles to WASM can produce workflow modules. The worker doesn't know or care what language produced the WASM bytes. Temporal maintains separate SDKs in 6 languages, each reimplementing the deterministic runtime — bugs and behavioral differences between SDKs are an ongoing problem. DBOS has 4 separate SDKs (TypeScript, Python, Go, Java). In cleat, a language transformer generates a few hundred lines of adapter code — not a full runtime reimplementation.

**Caveat**: Go has a fully automated transformer pipeline (`durable build`). Rust has an automated transformer via the `durable-sdk` crate (with `HostCalls` struct and memory helpers) and the `durable-macro` proc-macro crate (`#[durable_entry]` attribute that generates WASM exports). Rust workflows compile with `durable build --target rust` (which wraps `cargo build --target wasm32-wasip1`) and execute correctly with full replay, cancellation, and compensation.

### 7. Stronger Static Analysis

The transformer pipeline validates workflow code at build time:

| Rule | What it catches |
|---|---|
| E001 | Non-deterministic float comparisons |
| E002-E003 | Goroutines and channels (forbidden) |
| E004-E007 | Forbidden imports: `time.Now`, `time.Sleep`, `net/http`, `database/sql`, `math/rand`, `os`, `reflect` |
| E008-E011 | Interface dispatch, function value calls, threading violations |
| W001-W002 | Warnings for potential issues |

Temporal catches many of these at runtime (when replay diverges, producing cryptic errors). DBOS relies on developer discipline. Catching them at build time is strictly better.

### 8. Lightweight Testing Framework

`durabletest.TestEnv` avoids WASM compilation entirely — it provides a mock `HostCalls` with stub registration, simulated clock, signal delivery, and call history assertions (`AssertCalled`/`AssertNotCalled`). Tests run in milliseconds with deterministic time control. Temporal's test framework runs the actual workflow runtime, which is heavier. DBOS's testing story is less mature.

---

## Cleat's Disadvantages

### 1. Build Pipeline Friction — **FIXED**

~~Every change requires: edit Go → `durable build` (5-stage pipeline: analyze → callgraph → closure → transform → wasm compile) → WASM binary → deploy → test. The WASM compilation step adds seconds to the inner dev loop.~~

`durable dev` provides WASM-free local development. Workflows run directly as Go code with an HTTP-based service caller, eliminating the WASM compile step from the inner dev loop. Build the WASM only when you're ready to deploy.

Temporal workflows run directly as Go code — save, re-run tests, done. DBOS workflows are just annotated application code — no build step at all. The `durabletest.TestEnv` provides WASM-free unit testing, and `durable dev` extends this to full end-to-end local execution.

### 2. Immaturity

This is a pre-1.0 project with no production track record. Temporal has been battle-tested at Netflix, Snap, Stripe, and thousands of other companies for 5+ years. DBOS has production users, significant VC funding, and claims benchmarks of 40K+ workflow steps/second on a single Postgres instance. Cleat has no case studies, no production deployments, and no community.

### 3. Missing Critical Features

| Feature | Cleat | Temporal | DBOS |
|---|---|---|---|
| Queries (read workflow state externally) | Done (`SetQueryState` + `GET /api/workflows/:id/query?key=X`) | Mature | Via Conductor |
| Cron/scheduling | Done (built-in scheduler + REST API) | Built-in | Built-in |
| Web UI / dashboard | Done (Svelte SPA embedded in worker) | Mature UI | Conductor dashboard |
| Activity heartbeating | Done (`durable_call_heartbeat` host import + heartbeat goroutine) | Mature | Via steps |
| Server-side retry | Done (`durable_call_retry`, one-event-per-call) | Built-in | Built-in |
| Task queues / routing | Single worker pool | Rich routing model | Durable queues |
| History compaction | `ContinueAsNew` only | Automatic | Automatic |
| Multi-tenancy | Namespace isolation (worker `--namespace` flag) | Supported | Via namespaces |
| Cloud / managed offering | Gap | Temporal Cloud | DBOS Cloud |

### 4. WASM Boundary Overhead

Strings cross the WASM boundary via a pointer+length protocol through linear memory at a 10 MB scratch offset. For each `DurableCall`, the request is serialized, copied across the boundary, and the response is copied back. Temporal's in-process SDK avoids this cost. For I/O-bound workflows (API calls dominate runtime), this is negligible. For workflows with many small rapid durable calls, it adds up.

### 5. Rust Support (Second Language)

Temporal: Go, Java, TypeScript, Python, .NET, PHP, Ruby. DBOS: TypeScript, Python, Go, Java. Cleat: Go and Rust. The Rust pipeline uses the `durable-sdk` crate and `#[durable_entry]` proc-macro — no handwritten WASM FFI needed. Additional languages (C, Zig, TypeScript compiled to WASM) are possible since the host ABI is language-agnostic, but each requires a similar transformer and macros/code-generation support (estimated 2-4 weeks per language for languages with WASM support).

### 6. TinyGo Dependency for Production Builds

Standard Go WASM binaries are large (the Go runtime compiled to WASM). TinyGo produces smaller binaries but is a subset of Go (no `reflect` in many cases, limited standard library coverage) and adds another toolchain dependency. The recent fix for TinyGo compilation with Go 1.26 (commit `2c651db`) suggests this is an ongoing maintenance burden.

### 7. Retry at SDK Level Bloats Event History — **FIXED**

~~Cleat implements retry in the SDK: `RetryPolicy` controls exponential backoff between attempts, using `DurableSleep` between retries. Each attempt becomes a separate event in the history.~~

`durable_call_retry` is a host import that performs server-side retry with exponential backoff. Retries happen inside the engine without recording each attempt in event history — a single event is written regardless of how many attempts occurred. This matches Temporal's server-side retry behavior.

### 8. No Task Routing

Temporal's task queue model lets you route different workflow types or activities to different worker pools (e.g., GPU workers for ML inference, high-memory workers for data processing). Cleat has a single `SKIP LOCKED` poll loop — all workflow types go to the same pool. You could work around this with separate worker deployments filtering by `def_name`, but there's no built-in support.

### 9. No Ecosystem

No community plugins, no Datadog/Grafana integrations, no CloudWatch/Stackdriver support, no Kubernetes operator, no Helm charts. Temporal has all of these. DBOS has fewer but is growing.

### 10. Single Database Bottleneck

Both Temporal and DBOS have documented strategies for scaling beyond a single database instance. DBOS explicitly documents sharding workflows across multiple databases. Cleat's design doc doesn't address how to scale beyond a single Postgres instance. For high-throughput use cases, this is a gap.

---

## Suggested Improvements

### High Impact

1. **Dev mode (no WASM compilation)** — **Done**: `durable dev` runs workflows directly as Go code during development with an HTTP-based service caller. Eliminates the WASM compile step from the inner dev loop.

2. **Server-side retry** — **Done**: `durable_call_retry` host import performs server-side retry inside the engine. Retries don't add events to workflow history — a single event is written regardless of attempts.

3. **Web UI / dashboard** — **Done**: Svelte 5 + Vite SPA embedded in the worker binary via `embed.FS`. Dashboard with summary cards and recent workflows, workflow list with status filters, workflow detail with event timeline, signal/cancel actions, and schedule management with CRUD.

4. **Cron/scheduling** — **Done**: Built-in scheduler runs in the worker via `scheduleLoop()`. Schedules managed via CLI (`durable schedule add|list|delete|enable|disable`) and REST API (`GET/POST/DELETE /api/schedules`). Uses standard 5-field cron expressions.

5. **Implement a second language (Rust)** — **Done**: The `durable-sdk` crate provides `HostCalls` struct with all 15 WASM host imports and memory helpers. The `durable-macro` proc-macro crate provides `#[durable_entry]` for automatic WASM export generation. Rust workflows compile via `durable build --target rust` (delegates to `cargo build --target wasm32-wasip1`).

6. **Queries** — **Done**: Workflows set query state via `h.SetQueryState(key, value)`. External systems read it via `GET /api/workflows/:id/query?key=X`. Query state is persisted as JSONB in the `workflow_instances` table.

### Medium Impact

7. **Task queues / routing**: Allow routing specific workflow types to specific worker pools. This enables heterogeneous worker fleets (GPU, high-memory, etc.) and workload isolation.

8. **History compaction**: Automatic pruning/compaction of event history for long-running workflows with many steps. `ContinueAsNew` exists but requires manual orchestration.

9. **Multi-tenancy beyond namespace isolation**: The `--namespace` flag on the worker provides basic namespace filtering, but there's no auth layer, per-namespace quotas, or tenant-aware API surface. Important for platform use cases.

### Lower Impact

10. **Cloud / managed offering**: A managed control plane would reduce adoption friction. Even a simple one that provisions workers and PostgreSQL.

11. **Ecosystem integrations**: OpenTelemetry export, Datadog/Grafana dashboards, Kubernetes operator, Helm chart. Prometheus metrics endpoint at `/metrics` already exists.

12. **Exactly-once semantics**: Server-level idempotency guarantees beyond the application-level `ON CONFLICT DO NOTHING` on event history.

---

## Verdict

Cleat's WASM-based versioning is genuinely clever and architecturally unique. The idea of "workflow code as data" — deploy workflows via INSERT, workers as stable runtime — is a real insight that neither Temporal nor DBOS has.

The developer model is cleaner than Temporal's (no workflow/activity split) and the static analysis is stronger than DBOS's (build-time validation rather than runtime behavior). For a Go shop that wants a single-infrastructure durability story, cleat's design is coherent and well-motivated.

It is approaching MVP readiness. Queries, cron/scheduling, web UI, activity heartbeating, server-side retry, Prometheus metrics, and both Go and Rust transformer pipelines are all implemented. The major remaining gaps for production use are task routing (directing workflow types to specific worker pools), automatic history compaction, and ecosystem integrations (Helm charts, Grafana dashboards).

**Cleat's path to competitiveness**: build a basic web UI, add cron/scheduling, and ship the Rust transformer pipeline — all three are now complete. The next priorities are task routing, dogfooding on real workloads, and documentation.
