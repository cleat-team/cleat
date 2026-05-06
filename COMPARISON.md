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

**Caveat**: Only Go is implemented today. This is potential, not reality.

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

### 1. Build Pipeline Friction (biggest practical problem)

Every change requires: edit Go → `durable build` (5-stage pipeline: analyze → callgraph → closure → transform → wasm compile) → WASM binary → deploy → test. The WASM compilation step adds seconds to the inner dev loop.

Temporal workflows run directly as Go code — save, re-run tests, done. DBOS workflows are just annotated application code — no build step at all. The `durabletest.TestEnv` partly mitigates this for unit testing, but for integration testing or running workflows end-to-end, you must go through WASM.

### 2. Immaturity

This is a pre-1.0 project with no production track record. Temporal has been battle-tested at Netflix, Snap, Stripe, and thousands of other companies for 5+ years. DBOS has production users, significant VC funding, and claims benchmarks of 40K+ workflow steps/second on a single Postgres instance. Cleat has no case studies, no production deployments, and no community.

### 3. Missing Critical Features

| Feature | Cleat | Temporal | DBOS |
|---|---|---|---|
| Queries (read workflow state externally) | Gap | Mature | Via Conductor |
| Cron/scheduling | Gap | Built-in | Built-in |
| Web UI / dashboard | Gap | Mature UI | Conductor dashboard |
| Activity heartbeating | Partial (`DurableCallWithHeartbeat`) | Mature | Via steps |
| Server-side retry | Gap (SDK-level only) | Built-in | Built-in |
| Task queues / routing | Single worker pool | Rich routing model | Durable queues |
| History compaction | `ContinueAsNew` only | Automatic | Automatic |
| Multi-tenancy | Gap | Supported | Via namespaces |
| Cloud / managed offering | Gap | Temporal Cloud | DBOS Cloud |

### 4. WASM Boundary Overhead

Strings cross the WASM boundary via a pointer+length protocol through linear memory at a 10 MB scratch offset. For each `DurableCall`, the request is serialized, copied across the boundary, and the response is copied back. Temporal's in-process SDK avoids this cost. For I/O-bound workflows (API calls dominate runtime), this is negligible. For workflows with many small rapid durable calls, it adds up.

### 5. Go-Only Today

Temporal: Go, Java, TypeScript, Python, .NET, PHP, Ruby. DBOS: TypeScript, Python, Go, Java. Cleat: Go. The WASM multi-language vision is compelling but unrealized — and each new language requires building a transformer pipeline, which is substantial work (estimated 6-12 weeks per language).

### 6. TinyGo Dependency for Production Builds

Standard Go WASM binaries are large (the Go runtime compiled to WASM). TinyGo produces smaller binaries but is a subset of Go (no `reflect` in many cases, limited standard library coverage) and adds another toolchain dependency. The recent fix for TinyGo compilation with Go 1.26 (commit `2c651db`) suggests this is an ongoing maintenance burden.

### 7. Retry at SDK Level Bloats Event History

Cleat implements retry in the SDK: `RetryPolicy` controls exponential backoff between attempts, using `DurableSleep` between retries. Each attempt becomes a separate event in the history. Temporal has server-side retry — the server retries the activity without recording each attempt in workflow history. For workflows with many retried calls, cleat's approach produces larger event histories and slower replays.

### 8. No Task Routing

Temporal's task queue model lets you route different workflow types or activities to different worker pools (e.g., GPU workers for ML inference, high-memory workers for data processing). Cleat has a single `SKIP LOCKED` poll loop — all workflow types go to the same pool. You could work around this with separate worker deployments filtering by `def_name`, but there's no built-in support.

### 9. No Ecosystem

No community plugins, no Datadog/Grafana integrations, no CloudWatch/Stackdriver support, no Kubernetes operator, no Helm charts. Temporal has all of these. DBOS has fewer but is growing.

### 10. Child Workflows Are P2

Both Temporal and DBOS support child workflows as a first-class feature. In cleat, `ChildWorkflow` and `AwaitChild` exist in the SDK interface but are marked as planned (P2). This means you can't decompose a workflow into independently versioned, independently retryable sub-workflows with their own event histories.

### 11. Single Database Bottleneck (Unaddressed)

Both Temporal and DBOS have documented strategies for scaling beyond a single database instance. DBOS explicitly documents sharding workflows across multiple databases. Cleat's design doc doesn't address how to scale beyond a single Postgres instance. For high-throughput use cases, this is a gap.

---

## Suggested Improvements

### High Impact

1. **Dev mode (no WASM compilation)**: Support running workflows directly as Go code during development, using the same adapter layer that `durabletest.TestEnv` provides. This would eliminate the biggest source of friction in the development loop. The TestEnv already proves this is feasible — it just needs to be wired into the worker for local execution.

2. **Server-side retry**: Move retry logic into the host engine so retried attempts don't add events to workflow history. This is how Temporal does it and it's the right approach for long-running workflows with many retries.

3. **Web UI / dashboard**: Even a simple dashboard showing workflow instances, their status, event history, and allowing cancel/retry operations. This is table stakes for operational adoption. Could be a single-page app served by the worker or a separate lightweight service.

4. **Cron/scheduling**: Built-in support for recurring workflow execution. This is the most commonly requested feature in every workflow engine.

### Medium Impact

5. **Implement a second language (Rust)**: This would validate the multi-language WASM thesis and be a strong differentiator against both competitors. The design doc estimates ~8 weeks. Rust's `wasm32-wasip1` target and `wasm-bindgen` are production-ready.

6. **Task queues / routing**: Allow routing specific workflow types to specific worker pools. This enables heterogeneous worker fleets (GPU, high-memory, etc.) and workload isolation.

7. **Queries**: Let external systems read workflow state without signals. Critical for operational visibility — "what's the status of order #123?" without needing to deliver a signal.

8. **History compaction**: Automatic pruning/compaction of event history for long-running workflows with many steps. `ContinueAsNew` exists but requires manual orchestration.

### Lower Impact

9. **Cloud / managed offering**: A managed control plane would reduce adoption friction. Even a simple one that provisions workers and PostgreSQL.

10. **Ecosystem integrations**: OpenTelemetry export, Prometheus metrics endpoint, Datadog/Grafana dashboards, Kubernetes operator, Helm chart.

11. **Exactly-once semantics**: Server-level idempotency guarantees beyond the application-level `ON CONFLICT DO NOTHING` on event history.

12. **Multi-tenancy**: Namespace isolation for workflow definitions and instances. Important for platform use cases.

---

## Verdict

Cleat's WASM-based versioning is genuinely clever and architecturally unique. The idea of "workflow code as data" — deploy workflows via INSERT, workers as stable runtime — is a real insight that neither Temporal nor DBOS has.

The developer model is cleaner than Temporal's (no workflow/activity split) and the static analysis is stronger than DBOS's (build-time validation rather than runtime behavior). For a Go shop that wants a single-infrastructure durability story, cleat's design is coherent and well-motivated.

But it is a prototype, not a product. The build pipeline adds real friction, critical production features are missing (queries, scheduling, UI, server-side retry, task routing), and the Go-only reality undermines the multi-language WASM thesis until a second language ships.

**Cleat's path to competitiveness**: ship dev mode (eliminate build step for development), add server-side retry, build a basic web UI, and prove the multi-language thesis with a Rust target.
