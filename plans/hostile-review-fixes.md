# Hostile Review Remediation Plan

Goal: fix every CRITICAL and HIGH issue, and as many MEDIUM issues as
possible.  Each phase can ship independently.  Phases are ordered by
dependency: you can't observe what you haven't fixed, you can't
performance-tune what you can't observe.

---

## Phase 1 — Stop the Bleeding (P0 one-liners and quick wins)

Ship in days.  Each fix is small, well-bounded, and testable in isolation.

### 1.1 Initialize `nowMs` with real time

File: `internal/host/imports.go`

Set `nowMs` to `time.Now().UnixMilli()` at worker startup and update it
on each poll/heartbeat cycle.  Also seed it during replay from the first
event's `created_at` timestamp so replay produces the same time sequence.

### 1.2 Wire auth middleware into the HTTP server

File: `cmd/cleat-worker/main.go`

Import `internal/auth` and wrap the mux with `auth.Middleware`.
Add `--require-auth` flag (default true when `--api-addr` is set).
If no API keys are configured and `--api-addr` is set, generate a random
key at startup and print it once to stdout.

The existing test `no_auth_header_passes_through` must change — when
auth is wired, unauthenticated requests return 401.

### 1.3 Add WASM execution timeout

Files: `internal/host/runtime.go`, `internal/host/engine.go`

Wrap the `fn.Call(ctx, ...)` with `context.WithTimeout`.  Default
timeout: 30 seconds per host-call boundary crossing.  After timeout,
the engine marks the workflow as failed with `ErrTimeout`.

Also add a per-workflow cumulative timeout configurable per workflow
definition (default: none/unlimited for long-running workflows).

### 1.4 Add HTTP request size limits

File: `cmd/cleat-worker/main.go`

Wrap every `json.NewDecoder(r.Body).Decode()` with
`http.MaxBytesReader`.  Default limit: 1 MB for workflow inputs,
64 KB for signals and updates.  Configurable via `--max-body-size`.

### 1.5 Add HTTP server timeouts

File: `cmd/cleat-worker/main.go:369`

Set `ReadTimeout: 30s`, `WriteTimeout: 60s`, `IdleTimeout: 120s` on
the `http.Server` struct.  Unify with the `StartAPIServer` function
in `app.go` so there aren't two divergent server setups.

### 1.6 Check `mem.Write` return value everywhere

Files: `internal/host/memory.go`, `internal/host/engine.go`,
`internal/host/runtime.go`

Change `writeWasmString` to return `(uint32, error)`.  At every call
site, check the error and return an error code to the WASM module
instead of silently succeeding.  Add a `writeWasmStringOrTrap` variant
that returns 0 on failure so callers don't silently believe data was
written.

### 1.7 Make DurableLog actually durable

File: `internal/host/engine.go:1242-1244`

Implement `DurableLog` to append a `durable_log` event to the event
history batch.  Include the message, level, and key-value pairs from
the WASM module.

---

## Phase 2 — Correctness Hardening

Ship in 1–2 weeks.  These fix the fundamental reliability guarantees
that cleat's design promises but the implementation doesn't deliver.

### 2.1 Make DurableCall exactly-once

Files: `internal/host/engine.go`, `internal/host/db.go`

**This is the hardest fix in the plan.**

The problem: response is held in memory during `freshCall`, and the DB
write happens later in `executeWorkflow`.  A crash between call success
and DB write means re-execution on replay.

Solution: two-pronged.

**Option A (preferred): Write-ahead event log.**  Before making the
external call, insert a "call intent" event into event_history with
status = `pending`.  After the call succeeds, update the event with
the response and status = `complete`.  On replay after crash, a
`pending` event means the call outcome is unknown — replay checks
with the external service (if it provides an idempotency or
status-check endpoint), or fails the workflow with `ErrAmbiguous`.

**Option B (simpler but weaker): Immediate flush.**  After every
`freshCall`, immediately flush that single event to the database
in its own transaction before returning to WASM.  This adds one
DB round-trip per call but guarantees the event is durable before
the WASM module sees the response.  If the worker crashes after the
flush but before the workflow completes, replay sees the event and
returns the cached response — true exactly-once.

Start with Option B for correctness; add Option A's idempotency-key
integration for external services that support it.

### 2.2 Make ContinueAsNew atomic

File: `cmd/cleat-worker/main.go`

Wrap `StartNewRun` and `CompleteWorkflow` in a single database
transaction.  If either fails, both roll back.  The workflow stays
in `running` state and another worker picks it up.

### 2.3 Make event/status updates atomic

File: `cmd/cleat-worker/main.go`

Batch `AppendEventHistoryBatch`, status transitions
(`CompleteWorkflow`/`FailWorkflow`), and `ReleaseWorkflow` into a
single transaction.  Use savepoints if partial failure is meaningful,
but the default should be: all writes from one execution segment
succeed or none do.

### 2.4 Call version compatibility check at replay

File: `internal/host/engine.go`

Call `ValidateVersionCompatibility` before replay.  If the current
workflow version is incompatible with the event history version, fail
the replay with a clear error message listing what's incompatible.
Never silently diverge.

### 2.5 Make DurableDefer run on WASM trap/panic

Files: `internal/host/engine.go`, `internal/wasm/exports.go`

Wrap the WASM entry point call in a `defer`/`recover` block.  If the
WASM module panics or traps, invoke the defer function (a separate
WASM export) before tearing down the runtime.  If the defer function
itself traps, log the error and continue with cleanup.

### 2.6 Fix binary data in JSONB

Files: `internal/host/db.go`, `internal/host/engine.go`, `schema.sql`

Base64-encode all request/response strings before storing in JSONB.
Decode on read.  This prevents corruption of non-UTF-8 bytes and
null bytes.  Alternatively, change the `request` and `response` columns
from JSONB to BYTEA for the raw data and use a separate JSONB column
or metadata column for structured querying.

### 2.7 Increase output buffer and add overflow detection

File: `internal/host/memory.go`

Increase `outBufSize` from 64 KB to 1 MB.  Add a check: if the WASM
module writes more bytes than `maxOutLen`, return an error code instead
of silently truncating.  Add a streaming/chunked protocol for responses
larger than 1 MB (WASM writes chunks, host reassembles).

### 2.8 Remove WASI clock_time_get and random_get

File: `internal/host/runtime.go`

Replace `wasi_snapshot_preview1.MustInstantiate` with a custom WASI
instance that stubs out `clock_time_get` and `random_get` to return
errors.  Workflow code must use `cleat.Now()` and `cleat.Random()` —
calling `time.Now()` from WASM Go code will panic with a clear error.

---

## Phase 3 — Observability

Ship in 1–2 weeks.  Operators must be able to see what's happening.

### 3.1 Wire Prometheus metrics into execution path

Files: `cmd/cleat-worker/main.go`, `cmd/cleat-worker/metrics.go`,
`internal/host/engine.go`

Add `workflowsActive.Inc()`/`.Dec()` around `executeWorkflow`.
Record `workflowsCompleted`/`workflowsFailed` in `CompleteWorkflow`/
`FailWorkflow`.  Record `durableCallsTotal` in every `freshCall`.
Record duration histograms keyed by `def_name`, `status`, `task_queue`.

### 3.2 Fix histogram buckets

File: `cmd/cleat-worker/metrics.go`

Replace `prometheus.DefBuckets` with domain-appropriate buckets:
- Workflow duration: 0.1, 0.5, 1, 5, 10, 30, 60, 300, 600, 1800, 3600 seconds
- WASM compile: 0.01, 0.05, 0.1, 0.5, 1, 2, 5 seconds
- DB query: 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5 seconds
- Replay duration: same as workflow duration

### 3.3 Add background goroutine metrics

File: `cmd/cleat-worker/main.go`

For each background loop (reaper, compactor, concurrency key reaper,
scheduler, memory controller), add:
- A counter of successful iterations and failed iterations
- A gauge for the last iteration duration
- A gauge of items processed (stale instances reclaimed, events
  compacted, keys removed, schedules triggered)

### 3.4 Align Grafana dashboard with actual metric names

File: `monitoring/grafana/dashboard.json`

Rewrite all queries to use the metric names from `metrics.go`
(`cleat_workflows_active`, `cleat_workflows_completed_total`, etc.).

### 3.5 Add replay-vs-first-run metrics

File: `cmd/cleat-worker/metrics.go`

Add `cleat_replay_steps_total` counter and `cleat_fresh_steps_total`
counter, incremented from `replay*` and `fresh*` functions in the
engine.  Add separate histograms for replay duration and fresh-run
duration.

### 3.6 Add real benchmarks with WASM in the loop

File: `benchmarks/` (new file)

Create `benchmarks/wasm_bench_test.go` that compiles a workflow to WASM,
loads it in wazero, and executes it through the full engine.  Measure:
- End-to-end latency (from claim to completion)
- Steps/second with WASM boundary crossing
- Replay throughput vs fresh throughput
- 64 KB payload round-trip time
- Compilation + instantiation time

### 3.7 Persist structured error types

Files: `cmd/cleat-worker/main.go`, `internal/host/db.go`

Change `FailWorkflow` to accept a `CleatError` struct instead of a
string.  Store `error_code`, `error_op`, and `error_message` in
separate columns on `workflow_instances`.  Backfill existing rows.

---

## Phase 4 — Security Hardening

Ship in 1–2 weeks.  Close the attack surface.

### 4.1 Add rate limiting

File: `cmd/cleat-worker/main.go` (new middleware)

Add a token-bucket rate limiter using `golang.org/x/time/rate`.
Defaults: 100 requests/second per IP for the API, 10 workflow
starts/second.  Configurable via `--rate-limit` and
`--rate-limit-start`.

### 4.2 Implement redaction

File: `internal/host/engine.go` (new file: `internal/host/redact.go`)

Add a redaction pass before persisting events.  Redact fields matching
patterns: `*token*`, `*secret*`, `*key*`, `*password*`, `*Authorization*`,
`*credential*`.  Replace matched values with `[REDACTED]`.
Controlled by the (currently unused) `RedactionEnabled` config field.
Enable by default when auth is enabled.

### 4.3 Restrict plugin database access

Files: `internal/plugin/capabilities.go`, `internal/plugin/registry.go`

Add a `DatabaseAccess` capability level: `None`, `ReadOnly`,
`ReadWrite`.  Plugins default to `None` and must declare their
requirements.  At registration time, create a database handle that
enforces the declared access level (read-only transactions for
ReadOnly, full access for ReadWrite).

### 4.4 Cap retry maxAttempts

File: `internal/host/engine.go`

Add a worker-enforced ceiling: `maxAttempts` from WASM is clamped to
`min(maxAttempts, MaxRetryAttempts)` where `MaxRetryAttempts = 100`.
Add a `--max-retries` flag to override.

### 4.5 Fix DurableCallWithRetry context-aware backoff

File: `internal/host/engine.go:1633`

Replace `time.Sleep(...)` with:
```go
select {
case <-ctx.Done():
    return ctx.Err()
case <-time.After(time.Duration(backoffMs) * time.Millisecond):
}
```

### 4.6 Stop logging workflow results to stdout

File: `cmd/cleat-worker/main.go:775`

Remove the `result` string from the log line.  Log only the workflow
ID, status, and duration.  If operators need the result, they can
fetch it via the API.

### 4.7 Add event retention policy

Files: `internal/host/db.go`, `cmd/cleat-worker/main.go`

Add a background cleanup goroutine (or extend the compactor) to delete
event_history rows for `completed` and `failed` workflows older than
a configurable retention period.  Default: 30 days.  Configurable via
`--retention-days`.  A value of 0 disables cleanup (current behavior).

---

## Phase 5 — Performance

Ship in 1–2 weeks.  Benchmarks must exist before starting (Phase 3.6).

### 5.1 Enable wazero JIT compiler

File: `internal/host/runtime.go`

Replace `wazero.NewRuntime(ctx)` with:
```go
wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
```

This switches from interpreter to JIT-compiled WASM execution, roughly
10–50× faster for compute-bound workflow code.

### 5.2 Replace 200ms sleep with readiness polling

File: `internal/host/runtime.go`

Instead of `time.Sleep(200 * time.Millisecond)`, poll a shared memory
flag or use wazero's function export to check if `_start` has completed.
Use an exponential backoff: sleep 1ms initially, then 2ms, 4ms, up to
a cap of 100ms.  Most Go wasip1 binaries initialize in under 10ms.

### 5.3 Fix unbounded WASM byte cache

File: `cmd/cleat-worker/main.go`

Replace the unbounded `map[string][]byte` with an LRU cache that has:
- Max entry count (default 100)
- Max total bytes (default 500 MB)
- Eviction on either threshold
Expose cache hit/miss/size metrics to Prometheus.

### 5.4 Fix PluginLoader random eviction → LRU

File: `internal/host/plugin_loader.go:309`

Replace the `for k := range l.cache { delete(l.cache, k); break }` with
a proper LRU eviction using `container/list` (matching the pattern in
`versioned_loader.go`).

### 5.5 Batch heartbeat writes

Files: `cmd/cleat-worker/main.go`, `internal/host/db.go`

Replace N individual `UPDATE workflow_instances SET heartbeat_at = now()`
calls with a single batched UPDATE:
```sql
UPDATE workflow_instances SET heartbeat_at = now()
WHERE assigned_to = $1 AND status = 'running'
```
This reduces N round-trips to 1 per heartbeat interval.

### 5.6 Parallel shard claiming

File: `internal/host/sharded_store.go`

Replace sequential shard iteration with concurrent claims across all
shards using a goroutine per shard and a merged result channel.  This
reduces claim latency from `N * per_shard_latency` to
`max(per_shard_latency)`.

---

## Phase 6 — Data Integrity and Reliability Polish

Ship in 1–2 weeks.

### 6.1 Fix reaper timeout vs interval

File: `cmd/cleat-worker/main.go`

Set reaper timeout to `max(heartbeatInterval * 2, 10s)` to prevent
premature reaping while keeping recovery fast.

### 6.2 Add event history integrity checks

File: `internal/host/db.go`

Add a `checksum` column to `event_history` (SHA-256 of the event data).
Compute on insert, verify on load.  Add a `POST /api/workflows/:id/verify`
endpoint that re-checks all checksums.  Optional: add a `signed_events`
mode where the worker's key signs each event.

### 6.3 Implement dead letter queue status

Files: `internal/host/db.go`, `cmd/cleat-worker/main.go`

Set status to `dead_lettered` when a workflow fails after exhausting
retries (as distinct from `failed` for non-retryable failures).  Wire
the existing dead-letter API endpoints to the correct status.

### 6.4 Fix streaming plugin goroutine leak

File: `internal/host/engine.go:907`

Add a `defer` in the plugin streaming handler to drain and close the
channel if the context is cancelled.  Use `select` on `ctx.Done()`
alongside `chunk := range chunkCh`.

### 6.5 Add compaction state size limit

File: `internal/host/compaction.go`

Cap the compacted event list at 10,000 entries (configurable).  Beyond
that, create a secondary summary record and only compact recent events.

### 6.6 Add event_history pagination in API

File: `cmd/cleat-worker/main.go`

Add `?offset=N&limit=M` query parameters to
`GET /api/workflows/:id/history`.  Default limit: 1,000 events.
The web UI fetches in pages.

---

## Phase 7 — Developer Experience and Testing

Ship in 2–3 weeks.

### 7.1 Add replay testing to TestEnv

File: `cleat/cleattest/cleattest.go`

Add `TestEnv.EnableReplay()` mode.  When enabled, the first execution
records all calls into an internal `[]CallRecord`.  Calling `Execute`
again replays from that history, returning cached responses for
matching calls and executing fresh for new ones.  Add `AssertReplayDivergence`
to verify replay state matches.

### 7.2 Fix SendSignalAndWait to use simulated clock

File: `cleat/cleattest/cleattest.go:982`

Replace `time.After(timeout)` with `env.clock.After(timeout)` so that
`AdvanceTime` correctly triggers signal wait timeouts.

### 7.3 Implement ContinueAsNew in TestEnv

File: `cleat/cleattest/cleattest.go`

Store the `ContinueAsNew` input and mark the current execution as
"continued."  Add `AssertContinued(t, expectedInput)` and
`LastContinuedInput()` for assertions.

### 7.4 Add generics tests to transformer pipeline

Files: `internal/analyzer/`, `internal/callgraph/`, `internal/closure/`,
`internal/transform/`, `internal/wasm/`

Write a test workflow using Go generics (type parameters, generic
methods, instantiated generic functions).  Run it through all 5
pipeline stages.  Fix any failures.  Add generics-specific vet checks
if needed.

### 7.5 Handle build tags in transformer

File: `internal/wasm/build.go`

Before copying files to the build directory, parse `//go:build`
constraints and evaluate them against `GOOS=wasip1 GOARCH=wasm`.
Skip files that are build-constrained out of the WASM target.
Warn on files with `_linux.go` or `_amd64.go` suffixes that will
be excluded by the compiler.

Also add build-constraint awareness to the closure validator so it
doesn't report errors in platform-specific code that won't compile.

### 7.6 Ensure TinyGo path in CI

File: `.github/workflows/` (or CI config)

Install TinyGo in CI.  Run the full test suite with `--target=tinygo`.
Do not silently skip.  Fail the build if the TinyGo path breaks.

### 7.7 Add workflow stack traces (DWARF-based)

Files: `internal/host/runtime.go`, `internal/host/engine.go`

Capture the WASM trap/panic information from wazero (which includes
the instruction pointer).  If the WASM module was compiled with DWARF
debug info (standard Go wasip1), resolve the IP to a source location.
Store the stack trace in the `error_msg` column on failure.

This may require contributing upstream to wazero to expose the IP at
trap time.  In the interim, log the raw IP and function index.

### 7.8 Split HostCalls interface into capability groups

Files: `cleat/runtime.go`, `cleat/cleattest/cleattest.go`, all SDKs

Break the 69-method interface into composable interfaces:
```go
type Caller interface { DurableCall(...); DurableCallJSON(...); ... }
type Timer interface { DurableSleep(...); Now(); ... }
type Signaler interface { AwaitSignals(...); SendSignalAndWait(...); ... }
type Lifecycle interface { ContinueAsNew(...); DurableDefer(...); ... }
```

`HostCalls` remains as the composite embedding all of them.  Mocks and
SDKs can implement only the interfaces they need.  TestEnv, localdev,
and embedded runner get smaller implementations.

---

## Phase 8 — Operations, Ecosystem, and Debts

Ship over 3–4 weeks.  Some items are one-time documentation; others
are features.

### 8.1 Add migration runner

Files: `cmd/cleat-worker/`, `internal/migration/` (new)

Write a lightweight migration runner that reads versioned SQL files
from the `migrations/` directory, tracks applied versions in a
`schema_migrations` table, and runs pending migrations in order at
worker startup.  Fail startup if migrations can't be applied.

### 8.2 Document upgrade path

File: `docs/guide/upgrading.md` (new)

Document:
- How to upgrade the worker binary (rolling restart)
- How to upgrade the database schema (migration runner handles it)
- How to run old and new workers side by side during rollout
- How to roll back a worker upgrade
- How to roll back a workflow definition version
- PostgreSQL major version upgrade procedure

### 8.3 Document RPO/RTO/DR

File: `docs/guide/disaster-recovery.md` (new)

Document the recovery procedure from a full database restore:
1. Restore from pg_dump or PITR
2. All `running` workflows at backup time are now stale
3. Start workers; the reaper will reclaim stale instances within 60 seconds
4. Reclaimed workflows replay from event history
5. Note: if the external services' state has changed (e.g., an order was
   already shipped), the replay may produce different outcomes — this is
   inherent to any event-sourced system without external transaction
   coordination.

Document RPO (depends on backup frequency) and RTO (depends on
database restore time + reaper interval).

Document cross-region: recommend PostgreSQL streaming replication to a
warm standby.  Workers in the standby region connect to the standby
database (read-only until promoted).

### 8.4 Add worker drain API

File: `cmd/cleat-worker/main.go`

Add `POST /api/admin/drain` that:
1. Sets a `draining` flag
2. Stops claiming new work
3. Returns 202 Accepted with the number of in-flight workflows
4. `GET /api/admin/drain` returns the current count of in-flight workflows
5. When the count reaches 0, the worker exits cleanly

Add a `preStop` hook in the Helm chart that calls the drain endpoint.

### 8.5 Add search/filter by input content and error

Files: `internal/host/db.go`, `cmd/cleat-worker/main.go`

Add `?input_contains=X` and `?error_contains=X` query parameters to
`GET /api/workflows`.  Use PostgreSQL JSONB containment operators
(`@>`, `?`) or full-text search on a GIN index.  Add `?search=X` for
a combined search across input, result, and error_message.

### 8.6 Validate Python WASM end-to-end

Files: `sdk/python/`, tests

Write an end-to-end test: write a Python workflow, compile with
`componentize-py` to WASM, deploy to PostgreSQL, execute on a real
worker, verify the event history.  Fix any ABI mismatches between
the Python stubs and the actual cleat host imports.

### 8.7 Add plugin crash recovery boundaries

File: `internal/plugin/`

For Go-compiled plugins: wrap each host function call in a
`defer`/`recover`.  If a plugin panics, mark it as unhealthy, log the
stack trace, and return an error to the workflow.  Do not crash the
worker.

Longer term: migrate plugins to WASM-compiled modules using the same
wazero runtime as workflows.  This provides true isolation and enables
"deploy via INSERT" for plugins.

### 8.8 Write zero-downtime deployment guide

File: `docs/guide/zero-downtime-deploy.md` (new)

Document the step-by-step procedure for a zero-downtime worker upgrade:
1. Deploy new worker pool alongside old (blue/green)
2. New workers connect to the same database, claim from the same queues
3. Set old workers to drain (`POST /api/admin/drain` or SIGTERM)
4. Wait for in-flight workflows to complete
5. Remove old workers
6. If a rollback is needed, restart old worker binary and drain new ones

---

## Issues That Cannot (or Should Not) Be Fixed

Some findings in the hostile review point to real constraints or
tradeoffs, not bugs.  A competitor will frame them as weaknesses.
Here is why they aren't serious problems for cleat's target use cases
— and in several cases, why the "weakness" is actually a strength.

---

### 1. WASM boundary overhead is inherent and cannot be eliminated

**The competitor's argument:** Every string crossing the WASM boundary
costs a copy through linear memory.  Temporal runs workflows as native
Go code with zero serialization overhead.

**Why it doesn't matter for our use cases:**

Cleat workloads are I/O-bound.  A typical `DurableCall` takes 50–500 ms
waiting on an external API, database, or LLM.  The WASM boundary copy
costs single-digit microseconds — roughly 0.002% of the call latency.
Even at 1,000 calls per second per workflow, the copy overhead is under
10 ms of CPU per second of wall-clock time.

The cases where this overhead would dominate — compute-bound workflows
making millions of rapid calls with sub-millisecond external latency —
don't exist in practice.  An external API call inherently takes
milliseconds.  If your workflow is compute-bound, you're doing the
computation inside WASM (where JIT-compiled code runs at near-native
speed after Phase 5.1).  The boundary crossing is proportional to I/O
volume, not compute volume.

**The real tradeoff:** We pay ~2 µs per string crossing the boundary.
Temporal pays zero.  In exchange, we get versioned, sandboxed workflow
code decoupled from the worker lifecycle, deployable via INSERT, and
language-agnostic.  That tradeoff is correct for durable execution,
where the external calls are the bottleneck.  The benchmark suite
(Phase 3.6) will prove this quantitatively.

---

### 2. No goroutines or channels in workflows — parallelism requires child workflows

**The competitor's argument:** Go developers expect goroutines and
channels for concurrency.  Temporal lets you use them (carefully).
Cleat bans them entirely.  Fork-join of many concurrent operations
requires `AwaitAllChildren`, not goroutines.

**Why this isn't a weakness:**

Goroutines are non-deterministic by design.  The Go runtime schedules
them arbitrarily, and that scheduling changes across Go versions,
platforms, and CPU configurations.  If a workflow's correctness depends
on which goroutine wins a race, replay will diverge.  Temporal "allows"
goroutines but then produces cryptic non-determinism errors at runtime
when you get it wrong — errors that are famously painful to debug.

Cleat catches goroutine use at build time with a clear error message
(E001: "goroutines introduce non-deterministic scheduling across
replays").  This is strictly better than Temporal's runtime detection.

**Why child workflows are the right alternative:** A child workflow
has its own event history, its own lifecycle, and its own replay
guarantees.  `AwaitAllChildren` gives you the fork-join pattern with
provable determinism.  The event history shows exactly which children
ran, in what order their results arrived, and what each produced.
Debugging a child workflow is easier than debugging a goroutine race
because the execution is recorded and replayable.

The only thing you lose is shared-memory parallelism (multiple
goroutines mutating the same map).  That pattern is incompatible with
event sourcing regardless of framework — you can't replay shared-memory
mutations deterministically without recording every read and write.
Neither Temporal nor cleat supports this.  Cleat is just honest about
it at build time instead of runtime.

---

### 3. The transformer pipeline is a maintenance burden

**The competitor's argument:** ~3,000 lines across 5 packages doing
AST manipulation must be updated for every Go language feature.
Generics aren't handled yet (Phase 7.4 fixes this).  This is fragile.

**Why it's worth it:**

The pipeline replaces what would otherwise be manual boilerplate in
every workflow.  Without it, every developer would need to:
- Manually thread `HostCalls` through every function in the call chain
- Write WASM export wrappers by hand
- Declare WASM imports for every host function they use
- Maintain import allowlists manually

Temporal requires explicit `ctx` threading through every function,
separate activity registration, and explicit stub creation.  DBOS
requires decorators on every step function.  Cleat's pipeline
automates all of this — you write standard Go functions and the
pipeline wires them up.

**The maintenance burden is bounded:** Go's language surface has been
stable since generics landed in 1.18.  The AST structures the pipeline
operates on (`FuncDecl`, `CallExpr`, `BlockStmt`) haven't changed in a
decade and won't.  New Go features (iterator functions in 1.23,
`range` over integers in 1.22) are syntax sugar that desugars before
the pipeline sees it.  The pipeline operates on type-checked AST nodes
from `go/packages`, which already resolve generics to concrete
instantiations.

Phase 7.4 will close the generics gap.  After that, the pipeline
maintenance cost is adding new forbidden-import checks when Go's stdlib
adds non-deterministic packages — roughly one line per new package,
every 6 months.

Compare this to Temporal maintaining 7 separate SDK runtimes, each
reimplementing the deterministic execution engine.  Cleat's 3,000-line
pipeline is a fraction of the maintenance burden of one Temporal SDK.

---

### 4. Python WASM binaries are 19.2 MB

**The competitor's argument:** Storing 19.2 MB CPython-WASM binaries
in PostgreSQL `BYTEA` columns is impractical.  10 versions = 192 MB
before replication.  At 100 versions, you're storing 2 GB of WASM.

**Why this isn't a problem for the Go-native use case (95% of users):**

Go-compiled WASM binaries are 1–5 MB with standard Go, and 100–500 KB
with TinyGo.  The Python path exists for teams that need it, but cleat
is a Go-native system.  The design doc states this explicitly: "Go
first, Rust second."

**For the Python use case that does exist:**

Python workflows are inherently coarser-grained than Go workflows.
You don't write 100 versions of a Python workflow that calls an LLM
and sends a Slack message.  You write 2–3 versions and iterate slowly.
The storage cost of 3 versions × 19 MB = 57 MB is negligible compared
to the event_history storage those workflows will generate (hundreds
of KB per execution × thousands of executions).

If storage becomes a concern at scale, the mitigation is
straightforward: move `wasm_bytes` to S3/MinIO with a PostgreSQL
pointer, using the existing `blobstore` plugin.  This is a one-day
engineering task, not an architectural redesign.  We'll do it when
someone reports it as a problem, not before.

---

### 5. Plugins are compiled into the worker binary — "deploy via INSERT" doesn't apply to them

**The competitor's argument:** Workflow code deploys via INSERT, but
plugin code requires a worker redeploy.  This contradicts the
architectural premise.

**Why the contradiction is smaller than it looks:**

There are two categories of plugin: infrastructure plugins (blobstore,
kvstore, ratelimiter, auditlog) and integration plugins (slacknotify,
pagerdutyalert, webhookingest).

Infrastructure plugins change rarely — the blobstore plugin talks to
S3/MinIO, and that protocol hasn't changed in 15 years.  These are
compiled into the worker because they need database access, connection
pooling, and lifecycle management that a WASM sandbox can't provide.
Redeploying the worker once a quarter to update these is acceptable.

Integration plugins are the ones that churn with business requirements
("add a Slack notification for this workflow," "add a webhook for that
event").  These are the right target for WASM-compiled plugins (Phase
8.7 long-term).  The current Go-compiled plugins are a pragmatic
stepping stone — they work today, and the migration to WASM plugins
will be transparent to workflow code because both expose the same
host function ABI.

The split is analogous to database extensions vs. stored procedures.
Infrastructure plugins are extensions (compiled in, rarely change).
Integration plugins will become stored procedures (deployed via INSERT,
churn freely).

---

### 6. No managed cloud offering

**The competitor's argument:** Temporal Cloud and DBOS Cloud remove
operational burden.  Cleat requires self-managed PostgreSQL and
workers.

**Why this matters less than it appears (for now):**

Cleat runs on one PostgreSQL instance and one Go binary.  The
operational burden is: provision a PostgreSQL database (every cloud
has a managed offering), run the worker binary (a single static binary
with no system dependencies), point it at the database.  That's it.

Compare to self-hosting Temporal: you run the Temporal server (which
itself requires a database, plus Elasticsearch or Cassandra for
visibility), plus your worker pools.  Cleat self-hosted is arguably
simpler than Temporal Cloud because the entire system is two moving
parts.

A managed cleat offering would provide: automated provisioning, managed
upgrades, monitoring dashboards, and support.  Those are valuable but
not required for early production use.  The target early adopter is a
Go shop that already runs PostgreSQL and already deploys Go binaries.
For them, running cleat is adding one more binary to their existing
infrastructure — not adopting a new platform.

We'll build a managed offering when we have users who need it.  Building
it before we have users is solving a problem that doesn't exist yet.

---

### 7. Temporal has 7 mature SDKs; cleat has 1 (Go) plus work-in-progress

**The competitor's argument:** Temporal's Java SDK has years of
production use.  Cleat's Rust SDK is fresh.  Python is stubs.  This
limits adoption to Go shops.

**Why Go-first is the right strategy:**

Cleat's core value proposition is: write near-standard Go, compile to
WASM, deploy via INSERT.  The five-stage transformer pipeline is Go's
killer feature — no other framework automates the workflow compilation
pipeline to this degree.  Spreading engineering effort across 7
languages before Go is rock-solid would dilute the one thing cleat
does better than anyone.

Go is the dominant language for infrastructure and backend services at
the companies most likely to adopt a durable execution engine.
Stripe, Netflix, Uber, HashiCorp, CockroachDB — all Go shops.  Winning
Go is winning the market that matters for this category.

The WASM ABI (15 host imports, pointer+length protocol, packed i64
returns) is language-agnostic by design.  The Rust SDK proves this —
47 host imports implemented against the same ABI, with a proc-macro
for export generation.  Each new language needs an SDK (~2 weeks) and
a transformer (varies by language).  This is a small fraction of the
effort required to build a new Temporal SDK, which needs a full
deterministic runtime reimplementation.

The multi-language story is: Go is production-ready today.  Rust is
next.  Python, Java, and TypeScript are community-friendly on-ramps
that will mature as the community grows.  This is the same path
Temporal took — Go and Java first, others followed over years.

---

### 8. Cross-shard transactions don't exist

**The competitor's argument:** Operations spanning shards (parent
workflow on shard A, child on shard B) aren't atomic.  Temporal and
DBOS can do cross-shard operations within a single database.

**Why this is a non-issue at current scale targets:**

A single PostgreSQL instance handles 500–1,000 steps/second before
sharding is recommended.  At 100 ms average step latency, that's
50–100 concurrent workflows.  Most applications never exceed this.

The cross-shard use case is: a parent workflow on shard A starts a
child on shard B, and the worker crashes between the parent's event
write and the child's creation.  The fix on replay: the parent's event
history shows "child workflow started with ID X."  The worker checks
if child X exists on shard B.  If not, it creates it (idempotently).
If yes, it carries on.  This is eventual consistency via replay — the
same mechanism that handles single-shard crash recovery.

True cross-shard atomicity (2PC) is not needed because replay already
handles the crash-recovery case.  The child creation is idempotent by
child workflow ID.  The worst case is that a child runs twice if the
parent's "child started" event isn't persisted before the crash — and
this is exactly the same at-most-once problem that Phase 2.1 fixes for
DurableCall.  Once Phase 2.1 is done, child workflows on different
shards will be exactly-once too.

---

### 9. No FIPS compliance (TinyGo lacks `crypto/tls`)

**The competitor's argument:** Standard Go WASM has `crypto/tls` but
produces ~5 MB binaries.  TinyGo produces small binaries but lacks
`crypto/tls`.  Government and regulated-industry users require FIPS.

**Why this affects approximately zero early adopters:**

FIPS compliance matters for: US federal government, defense contractors,
and a subset of financial services.  These organizations don't adopt
pre-1.0 open-source workflow engines.  By the time cleat has the
maturity and track record that these organizations require, TinyGo's
`crypto/tls` support will have improved, or we'll have an alternative
compilation path.

For the Go standard compiler path (which does have `crypto/tls` and
can be FIPS-validated via the Go crypto package's FIPS mode), the 5 MB
binary size is a non-issue.  Storing 50 versions of a 5 MB workflow =
250 MB.  At $0.10/GB-month for managed PostgreSQL storage, that's
$0.025/month.  The operational simplicity of "everything in PostgreSQL"
is worth vastly more than $0.025/month.

If a FIPS-requiring customer appears and TinyGo doesn't support
`crypto/tls` yet, they use the standard Go compiler path.  The binary
size difference is irrelevant at their scale.

---

### 10. The 69-method HostCalls interface is too large

**The competitor's argument:** Adding a method breaks all mock
implementations across 4+ language SDKs.  Violates the Go proverb
"the bigger the interface, the weaker the abstraction."

**Why this is getting fixed, not just dismissed:**

Phase 7.8 splits the interface into composable capability groups
(`Caller`, `Timer`, `Signaler`, `Lifecycle`, etc.).  `HostCalls`
remains as the composite for workflow code, but mocks and SDKs
implement only the groups they need.  Adding a new method adds it
to one group, not to every mock.

**Why the large interface won't be a problem after the split:**

The interface grew to 69 methods because workflows genuinely need
these capabilities: calling external services, sleeping, awaiting
signals, spawning children, querying state, acquiring locks, continuing
as new, and so on.  Each method corresponds to a capability that some
workflow needs.  Splitting into groups preserves the discoverability of
the flat interface for workflow authors (you still import one package
and get one `HostCalls` value) while making the implementation side
manageable.

Temporal's Go SDK has a similar surface area spread across multiple
interfaces and context values.  The difference is that Temporal's
surface grew organically over 5 years and is well-documented; cleat's
was designed upfront and needs the same organizational polish.  Phase
7.8 delivers that polish.

---

### 11. WASM sandbox debugging is fundamentally limited

**The competitor's argument:** You can't attach delve to a running
WASM workflow.  You can't set breakpoints.  You can't inspect local
variables.  Temporal workflows are native Go — fully debuggable.

**Why this is a real limitation with a real mitigation:**

Yes, WASM debugging is worse than native Go debugging.  Phase 7.7
adds DWARF-based stack traces, so you'll at least get source locations
on failure.  But interactive debugging of live WASM workflows won't
match native Go debugging for the foreseeable future.

**Why it matters less than you'd think:**

The dominant debugging pattern for durable execution is NOT interactive
debugging of live workflows.  It's:
1. Workflow fails in production
2. Pull the event history and WASM bytes
3. Replay locally with `cleat dev` (WASM-free, fully debuggable Go)
4. Fix the bug
5. Deploy the new version

This is the same pattern Temporal users follow — you don't attach
delve to production workflows at Netflix.  You replay them locally.
Cleat's local replay (`cleat dev`) is actually better than Temporal's
for this: no Temporal server needed, no workflow/activity distinction
to set up, just a Go function you can debug normally.

For development of new workflows, `cleat dev` and `cleattest.TestEnv`
(plus Phase 7.1's replay support) provide a fast, debuggable inner
loop.  WASM compilation only happens at deploy time.  This is the same
pattern as shipping a Go binary: you debug natively, you ship compiled.

---

### Summary

Of the 57 findings in the hostile review, 48 are fixable (Phases 1–8)
and 9 are architectural constraints or tradeoffs.  Of those 9:

- 4 are inherent tradeoffs where the alternative is worse (WASM
  boundary, no goroutines, transformer maintenance, interface size)
- 3 are scale/ecosystem issues that self-resolve with adoption
  (managed cloud, SDK maturity, ecosystem integrations)
- 2 are niche constraints that don't apply to the target user (FIPS,
  Python WASM size)
- 0 are correctness or security risks that cannot be mitigated

A competitor will cite all 11 of these.  Our response should be:
"These aren't bugs.  They're architectural choices with documented
rationale.  Here's the tradeoff analysis.  Which of these affects a
workload you're actually running?"

---

## Execution Order Dependency Graph

```
Phase 1 (stop bleeding)
  ├─> Phase 2 (correctness) ──> Phase 6 (reliability polish)
  ├─> Phase 3 (observability) ──> Phase 5 (performance)
  ├─> Phase 4 (security)
  ├─> Phase 7 (developer experience)
  └─> Phase 8 (operations)
```

Phases 3, 4, 7, and 8 are largely independent and can be worked in
parallel by different team members.

Phase 5 must come after Phase 3 (you need benchmarks first).
Phase 6 should come after Phase 2 (don't polish what's broken).
