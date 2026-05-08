# Cleat Risk Mitigation Plan

## How to Use This Plan

Each phase is ordered by priority within the phase. The phases are designed to be sequential but each phase delivers standalone value — you can stop after any phase and have a meaningfully better system.

**Effort estimates** are in engineering-weeks for one senior engineer familiar with the codebase. They assume the work includes tests and documentation updates.

---

## Phase 0: Emergency Triage (1 week)

These are the "stop the bleeding" items. Do them before anything else.

### 0.1 Wire Up Auth Middleware (0.5 days)

**Risk:** #1 (Critical)

The `auth.Middleware(db)` function already exists and is tested. Wire it into the HTTP mux in `cmd/cleat-worker/main.go`.

```go
// In main(), after constructing the mux, before registering handlers:
handler := auth.Middleware(db)(mux)
```

- Wrap the mux with the auth middleware
- Skip auth for `/healthz` only (the middleware already supports this — it passes through requests without an API key)
- Add a flag `--require-auth` (default `true` in production, `false` for local dev)
- Add integration tests that verify 401 responses for unauthenticated requests

**Validation:** `curl -X POST http://worker:8080/api/workflows/my-wf/start` returns 401.

### 0.2 Add URL Allowlist for HTTP Fetch (1 day)

**Risk:** #2 (Critical)

Replace the unrestricted `handleHTTPFetch` with an allowlist-based approach.

Implementation:
1. Add an `--egress-allowlist` flag or config file that specifies allowed URL patterns (glob or regex)
2. Parse the URL in `handleHTTPFetch` and validate against the allowlist **before** making the request
3. Add a `--disable-http-fetch` flag to completely disable the feature for environments that don't need it
4. Add a check that prevents fetching from private IP ranges by default (RFC 1918, 169.254.0.0/16, ::1, etc.)
5. Use a shared `http.Transport` with a custom `DialContext` that enforces IP-level restrictions (prevents DNS rebinding bypasses)

The IP-level enforcement is critical: URL validation alone is insufficient because DNS can resolve to internal IPs after the URL check passes.

**Validation:** A workflow calling `http://169.254.169.254/latest/meta-data/` gets an error, not a response.

### 0.3 Add Request Body Size Limits (0.5 days)

**Risk:** #10 (Medium)

Add `http.MaxBytesReader` to all POST/PUT endpoints:

```go
r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
```

Apply to: `handleStartWorkflow`, `handleSignal`, `handleWorkflowUpdate`, `handleCreateSchedule`, `handleResolvePromise`, `handleRejectPromise`.

**Validation:** POST with body > 1MB returns 413.

---

## Phase 1: Production Hardening (3 weeks)

These make the system safe for internal production use with trusted workloads.

### 1.1 Implement Dead Letter Queue (3 days)

**Risk:** #5 (High)

Add a `max_attempts` column to `workflow_instances` (default NULL = unlimited for backward compatibility). Add a `--max-attempts` worker flag (default 10).

Implementation:
1. Increment `attempt_count` on each claim
2. When `attempt_count >= max_attempts`, move the workflow to a new status: `dead_lettered` instead of `ready`
3. Add API endpoints:
   - `GET /api/dead-letters` — list dead-lettered workflows
   - `POST /api/workflows/:id/retry` — move a workflow from `dead_lettered` back to `ready` (resets attempt count)
   - `DELETE /api/workflows/:id` — permanently delete a dead-lettered workflow
4. Add a `dead_letter_reason` column storing the last error message and timestamp
5. Add Prometheus metrics: `cleat_workflows_dead_lettered_total`

**Validation:** A workflow that consistently fails moves to `dead_lettered` after N attempts and stops consuming worker capacity.

### 1.2 Add Workflow Execution Timeouts (2 days)

**Risk:** #13 (Medium)

Add a `workflow_timeout_seconds` column to `workflow_defs` (default NULL = no limit). The worker enforces the timeout by wrapping the WASM execution context.

Implementation:
1. In `executeWorkflow`, create a `context.WithTimeout` before calling `engine.Replay`
2. If the context is cancelled due to timeout, call `FailWorkflow` with an appropriate error message
3. Add `--default-workflow-timeout` flag (default 300s = 5 minutes) for workflows without an explicit timeout
4. Add a `--wasm-instance-timeout` flag for the maximum time a single WASM invocation can take (applied via wazero's `WithSysNanosleep`, or via Go context cancellation)

Note: wazero does not support CPU time limits natively, so this uses wall-clock timeouts via Go context cancellation. This means a WASM module in a tight loop won't be interrupted until the context deadline — but the Go runtime's goroutine preemption (every 10ms in Go 1.14+) ensures the context is checked eventually.

**Validation:** A workflow with `while true {}` is terminated after the timeout.

### 1.3 Fix WASM Init Race Condition (2 days)

**Risk:** #6 (High)

Replace the `time.Sleep(200ms)` hack with a proper synchronization mechanism.

Implementation:
1. Add a WASM export `__cleat_ready` that the Go runtime calls after `_start` initialization completes
2. In `InitModule`, start `_start` in a goroutine with a timeout context
3. After initialization, the module calls `__cleat_ready` (a host function that closes a channel or sets an atomic flag)
4. Apply a generous timeout (e.g., 5 seconds) for initialization — if `__cleat_ready` isn't called, return an error
5. For non-Go modules (no `_start`), maintain the no-op path

This requires updating the WASM SDKs (Go, Rust, Python, TypeScript) to emit the `__cleat_ready` export. For backward compatibility, if the module doesn't export `__cleat_ready`, fall back to the 200ms sleep with a warning log.

**Validation:** Under CPU pressure (e.g., `stress --cpu 32`), module initialization still completes reliably.

### 1.4 Add WASM Cache Eviction (1 day)

**Risk:** #12 (Medium)

Replace the `map[string][]byte` cache with a size-bounded LRU cache.

Implementation:
1. Use `container/list` for LRU ordering, `map[string]*list.Element` for lookup
2. Track total cached bytes
3. Add flags: `--wasm-cache-max-entries` (default 100) and `--wasm-cache-max-bytes` (default 256MB)
4. On insertion, evict least-recently-used entries until within limits
5. Add Prometheus metrics: `cleat_wasm_cache_entries`, `cleat_wasm_cache_bytes`, `cleat_wasm_cache_hits_total`, `cleat_wasm_cache_misses_total`

**Validation:** Deploy 200 versions of a workflow; cache size never exceeds configured limits; hit rate is measurable.

### 1.5 Fix WorkflowStore Interface Segregation (3 days)

**Risk:** #16 (Medium)

Split `WorkflowStore` into focused interfaces:

```
WorkflowClaimer      — ClaimWorkflow, ClaimWorkflows, ClaimStickyWorkflows
EventHistoryStore    — LoadEventHistory, AppendEventHistoryBatch, CompactHistory, LoadCompactionState, GetCompactionCandidates
WASMStore            — LoadWASM, DeployWorkflowDef, ListWorkflowDefs, GetWorkflowDef
WorkflowLifecycle    — Heartbeat, CompleteWorkflow, FailWorkflow, ReleaseWorkflow, StartNewRun, StartChildWorkflow
ScheduleStore        — CreateSchedule, ListSchedules, DeleteSchedule, SetScheduleEnabled, GetDueSchedules, UpdateScheduleNextRun
VersionStore         — ListVersions, ResolveLatestVersion, ValidateVersion, MarkVersionDeprecated, PurgeWorkflowDef, CountActiveInstances
ConfigStore          — LoadWorkflowConfig, LoadDAGSpec
TraceStore           — TraceWorkflow
ConcurrencyKeyStore  — (already exists)
PromiseStore         — (already exists)
SignalStore          — (already exists)
```

The `PostgresStore` struct implements all of them. Callers depend on the narrow interfaces they need. The `WorkflowStore` interface is temporarily retained as a composition of the sub-interfaces for backward compatibility, then deprecated.

**Validation:** Tests that mock only the interfaces they need compile and pass.

### 1.6 Standardize Error Handling (2 days)

**Risk:** #8 (High), #21 (Low)

1. Define error classification in `internal/host/errors.go`:
```go
type ErrorCode int
const (
    ErrUnknown ErrorCode = iota
    ErrTransient          // retryable (DB connection, timeout)
    ErrPermanent          // non-retryable (invalid input, not found)
    ErrCancelled          // workflow cancelled
    ErrTimeout            // execution timeout
)
```

2. Update all error returns to use typed errors:
```go
type CleatError struct {
    Code    ErrorCode
    Op      string  // operation that failed
    WorkflowID string
    Err     error   // underlying error
}
func (e *CleatError) Error() string { ... }
func (e *CleatError) Unwrap() error { return e.Err }
func (e *CleatError) Retryable() bool { return e.Code == ErrTransient }
```

3. Fix the error handling inconsistency in `executeWorkflow`:
   - DB connection errors → `ReleaseWorkflow` (retryable, already correct in most places)
   - WASM compilation errors → `FailWorkflow` (permanent)
   - Plugin lookup errors → `FailWorkflow` (permanent, already correct)
   - Timeout errors → `FailWorkflow` with clear error message (new)

4. Replace the string-matching `isConnectionError` with proper error type checking using `errors.Is` or `errors.As`.

**Validation:** All existing behavioral tests pass; new tests verify error classification for each error path.

---

## Phase 2: Observability (2 weeks)

### 2.1 Switch to Structured Logging (2 days)

**Risk:** #18 (Medium)

Replace all `log.Printf` calls with `slog`:

```go
slog.Info("workflow claimed", "workflow_id", wf.ID, "def_name", wf.DefName, "worker_id", w.id)
slog.Error("execution failed", "workflow_id", wf.ID, "error", err)
slog.Warn("DB connection lost", "worker_id", w.id, "consecutive_errors", w.consecutiveDBErrors)
```

- Add `--log-level` flag (debug, info, warn, error)
- Add `--log-format` flag (text, json) for production log aggregation
- Always include `worker_id` and `workflow_id` in log context where available

### 2.2 Add Proper Prometheus Histograms (2 days)

**Risk:** #20 (Low-Medium)

Replace manual counter math with proper Prometheus client library usage. Add histogram metrics:
- `cleat_workflow_duration_seconds` — end-to-end workflow execution time
- `cleat_replay_duration_seconds` — replay phase only (proper histogram with multiple buckets)
- `cleat_db_query_duration_seconds` — database operation latency (by operation: claim, history_load, history_save, etc.)
- `cleat_wasm_compile_duration_seconds` — WASM compilation time
- `cleat_dispatch_latency_seconds` — time from workflow ready to workflow claimed

Fix `metricsWorkflowsActive` — it's never incremented. Track it properly via increment on claim, decrement on complete/fail/release.

### 2.3 Wire Up OpenTelemetry (3 days)

**Risk:** #19 (Low-Medium)

The OTel SDK is already in `go.mod` but unused. Wire it up:

1. Create a tracer provider in `main()` using `otlptracehttp`
2. Create spans for:
   - Workflow dispatch (claim → execute → complete/fail/release)
   - Each WASM export call
   - Each database operation
   - Each `DurableCall` (service call)
   - Each `plugin_call`
3. Propagate the `trace_id` from `workflow_instances.trace_id` to the span context
4. Pass trace context through `context.Context` to all service callers and plugins
5. Add `--otel-endpoint` flag for the OTLP collector URL
6. Add `--otel-disabled` flag (default `true` for backward compatibility — opt-in for now)

**Validation:** With a Jaeger instance running, workflow executions produce trace waterfalls showing each step.

### 2.4 Deepen Health Check (0.5 days)

**Risk:** (Observability gap)

Add to `/healthz`:
- Database ping (`db.PingContext(ctx)`)
- Dispatch loop liveness (last claim timestamp within 2x poll interval)
- Return degraded status if any check fails, with per-component status

```json
{
  "ok": true,
  "components": {
    "database": {"ok": true, "latency_ms": 2},
    "dispatch_loop": {"ok": true, "last_claim_ms_ago": 350},
    "heartbeat_loop": {"ok": true},
    "memory": {"ok": true, "pressure": 0.42}
  }
}
```

---

## Phase 3: Multi-Tenant / Public-Facing Readiness (4 weeks)

### 3.1 Implement Event History Secret Redaction (3 days)

**Risk:** #3 (High)

Add a redaction mechanism to the event history system:

1. Add a `sensitive_fields` list to workflow definitions (JSONPath expressions)
2. In `AppendEventHistoryBatch`, redact matching fields before insertion
3. Redact both `request` and `response` fields in call events, and `plugin_input` / `plugin_output` in plugin events
4. Replace redacted values with `[REDACTED]`
5. Add a `--redaction-enabled` flag (default `true`)
6. The redaction happens server-side in the worker — WASM code never sees the redacted values

Additionally, add SDK-level support for marking individual parameters as sensitive (e.g., `cleat.SensitiveField("api_key", value)`), which the cleat compiler uses to annotate the WASM module's metadata. The worker reads these annotations at deploy time and adds them to the `sensitive_fields` list automatically.

### 3.2 Database Credential Management (2 days)

**Risk:** #4 (High)

1. Add support for reading database credentials from environment variables separately from the connection URL:
   - `CLEAT_DB_HOST`, `CLEAT_DB_PORT`, `CLEAT_DB_NAME`, `CLEAT_DB_USER`, `CLEAT_DB_PASSWORD`
   - The `--db` flag still accepts a URL but overrides credentials from env vars
2. Add support for reading credentials from files (Docker secrets pattern):
   - `CLEAT_DB_PASSWORD_FILE` — path to a file containing only the password
3. Add support for `PGSSLMODE`, `PGSSLCERT`, `PGSSLKEY` environment variables
4. Document in SECURITY.md that `--db` should not be used in production (use env vars)
5. For shard configs, add `password_file` as an alternative to `conn_str`; read the password from the file

### 3.3 Fix Worker ID Generation (0.5 days)

**Risk:** #9 (Medium)

Change `generateWorkerID()` to use 16 random bytes (128 bits), hex-encoded to 32 characters. Trivial change, eliminates collision risk.

### 3.4 Implement Schedule Leader Election (2 days)

**Risk:** #14 (Medium)

Replace the per-worker schedule loop with proper leader election:

1. Add a `worker_leases` table with `(lease_name, worker_id, expires_at)`
2. The schedule loop becomes: try to acquire the `scheduler` lease via `INSERT ... ON CONFLICT DO NOTHING`
3. Only the worker holding the lease runs the schedule loop
4. Lease expires after 30s with heartbeat renewal every 15s
5. If the lease-holding worker dies, another worker picks it up after lease expiry

Alternative (simpler): Use PostgreSQL advisory lock (`pg_try_advisory_lock`) for the scheduler lease. This avoids a new table.

### 3.5 Add `http.MaxBytesReader` to All Handlers (Already in Phase 0)

### 3.6 Implement `DurableLog` (1 day)

**Risk:** #22 (Low)

Implement `DurableLog` by writing to the event history as a `log` event type, or emit to the structured logger with workflow context. The event history approach makes logs durable and replayable; the logger approach makes them visible in production monitoring. Do both: write to event history AND emit a structured log line.

---

## Phase 4: Performance & Scalability (3 weeks)

### 4.1 Lazy Event History Loading (4 days)

**Risk:** #7 (High)

Instead of loading the full event history on every resume, load events lazily:

1. Store events in pages (e.g., 100 events per page) with a page index
2. On resume, load only the first page plus the compaction state
3. During replay, when `stepCount` reaches the end of a page, load the next page
4. This reduces memory usage and load time for long-running workflows from O(n) to O(1) in the common case

Alternative (simpler but less effective): Always compact after N events and keep the tail size small.

### 4.2 Event History Indexing (2 days)

Add database indexes:
- `CREATE INDEX ON event_history (workflow_id, step)` — already exists as PK?
- `CREATE INDEX ON workflow_instances (status, next_wake_at) WHERE status = 'ready'` — partial index for claim queries
- `CREATE INDEX ON workflow_instances (sticky_worker_id, status) WHERE sticky_worker_id IS NOT NULL` — partial index for sticky claims

### 4.3 Connection Pool Guidance (1 day)

Add documentation and validation:
- Document the formula: `max_connections = workers * (concurrency + 5)` and warn that PostgreSQL defaults to 100
- Add startup validation: query `SHOW max_connections` and warn if the configured pool would exceed 80% of it
- Add a `--db-max-open-conns` flag to decouple pool size from concurrency
- Consider PgBouncer guidance for multi-worker deployments

### 4.4 WASM Module Compilation Cache (2 days)

Add a compilation cache at the `Runtime` level (separate from the byte-level cache):
1. Use wazero's `CompileModule` result as a cache keyed by `(defName, defVersion)`
2. This avoids re-compiling WASM for every workflow resume — currently `executeWorkflow` calls `CompileModule` on every execution
3. A workflow that wakes 20 times currently compiles the WASM 20 times; this reduces it to 1

---

## Phase 5: Code Quality & Maintainability (Ongoing)

### 5.1 Split `EventRecord` Into Typed Event Structs

Create a type-safe event hierarchy:
```go
type Event interface { Step() int; Type() EventType }

type CallEvent struct { step int; Service string; Op string; ... }
func (e CallEvent) Step() int { return e.step }
func (e CallEvent) Type() EventType { return EventTypeCall }

type SleepEvent struct { step int; DurationMs int64 }
// ... etc
```

This eliminates the 30-field struct and the `eventRecordToPayload`/`populateFromPayload` switch statements. The database schema doesn't need to change — serialize/deserialize at the store boundary.

### 5.2 Extract `main()` Into Composable Functions

Split `main()` into:
- `parseConfig()` → `Config`
- `connectDatabase(Config) → *sql.DB`
- `initPlugins(Config, *sql.DB) → Plugins`
- `buildWorker(Config, Plugins) → *Worker`
- `startAPIServer(Config, *Worker) → *http.Server`
- `run(Config, *Worker)`

Each function is independently testable.

### 5.3 Add Integration Test Harness

The project has good unit test coverage (60%+) but needs integration tests:
1. Start a real PostgreSQL in a test container
2. Deploy a WASM workflow
3. Start a worker
4. Submit a workflow and verify it completes end-to-end
5. Test failure modes: kill the database mid-execution, kill the worker mid-execution, send a signal during sleep

### 5.4 Fix `sync.Map` Iteration

Replace `sync.Map` for `inflight` and `execEngines` with `map[string]*WorkflowInstance` protected by a `sync.RWMutex`. This is slightly less performant under extreme concurrency but guarantees consistency, which is more important for correctness.

---

## Effort Summary

| Phase | Weeks | Cumulative | Risk Reduction |
|-------|-------|------------|----------------|
| 0: Emergency Triage | 1 | 1 | Critical security holes closed |
| 1: Production Hardening | 3 | 4 | Safe for internal prod use |
| 2: Observability | 2 | 6 | Debuggable in production |
| 3: Multi-Tenant Readiness | 4 | 10 | Could serve external customers |
| 4: Performance & Scalability | 3 | 13 | Handles 10x current workload |
| 5: Code Quality | Ongoing | — | Sustainable development velocity |

**Total: ~13 engineering-weeks to reach "credible product" state (Phases 0-3).**

If the project is pre-revenue and team size is small, the pragmatic target is Phases 0+1 (4 weeks) — that gets you to "safe for internal/prototype use." Phase 2 (observability) should be done before any customer deploys to production. Phase 3 is required before you can call it multi-tenant. Phase 4 is required before you can brag about scale. Phase 5 is ongoing hygiene.
