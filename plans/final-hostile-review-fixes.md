# Final Hostile Review Fixes — Multi-Stream Plan

## Status Summary

Of the original 57 findings (14 CRITICAL, 21 HIGH, 22 MEDIUM):

| Severity | Fixed | Partially Fixed | Still Open | Architectural Tradeoff |
|----------|-------|-----------------|------------|------------------------|
| CRITICAL (14) | 10 | 3 | 1 | 0 |
| HIGH (21) | 19 | 1 | 1 | 0 |
| MEDIUM (22) | 13 | 2 | 1 | 6 |

**Total actionable remaining: 9 issues** (1 CRITICAL open, 1 CRITICAL partially fixed, 2 CRITICAL intentionally deferred, 1 HIGH open, 1 HIGH partially fixed, 1 MEDIUM open, 2 MEDIUM partially fixed)

Architectural tradeoffs accepted as-is: cross-shard transactions, no goroutines, WASM boundary overhead, transformer maintenance burden, Python WASM binary size, plugin binary coupling.

---

## Stream G: Correctness Close-Out (the last engine gaps)

**Theme:** The final correctness issues that affect workflow execution guarantees.
Two bugs and one missing sentinel. All touch the execution engine and database layer.

**Files touched:** `internal/host/engine.go`, `internal/host/db.go`, `internal/host/errors.go`, `cmd/cleat-worker/main.go`

### Phase G1 — Atomic ContinueAsNew (CRITICAL #4)

**Problem:** In `cmd/cleat-worker/main.go` lines 1092-1133, the ContinueAsNew path does:

1. `AppendEventHistoryBatch` — persists events in its own transaction (line 1097)
2. `store.ContinueAsNew` — creates new run + completes old run in a SEPARATE transaction (line 1122)

A crash between step 1 and step 2 leaves events durably recorded but the old workflow still `running` and the new run never created. On replay, the events are found but the run ID doesn't exist — the worker has no way to recover.

**What's already correct:** `Store.ContinueAsNew` (db.go lines 842-886) is itself atomic — it creates the new run and completes the old run in a single transaction. The problem is only that event append is outside that transaction.

**Fix:** Add an `AppendEvents` parameter to `ContinueAsNew` so that event persistence happens inside the same database transaction as the run creation and completion. In `main.go`, pass the event batch directly to `store.ContinueAsNew` instead of calling `AppendEventHistoryBatch` separately. The store method appends events, creates the new run, and completes the old run in one `sql.Tx`.

This is the same pattern already used by `FinalizeWorkflowSegment` (db.go lines 888-949) which bundles event append + status update atomically.

**Changes:**
- `db.go`: Add `events []EventRecord` parameter to `ContinueAsNew` on `WorkflowStore` interface and all 4 implementations (Postgres, MySQL, MSSQL, Sharded)
- `db.go`: In each `ContinueAsNew` implementation, call the existing `appendEventsInTx` helper before run creation
- `main.go` lines 1092-1133: Remove the separate `AppendEventHistoryBatch` call; pass events directly to the single `ContinueAsNew` call
- `main.go`: Remove the now-unnecessary error handling for the orphaned-events case

**Estimated:** ~80 lines across db.go (4 backends) + ~20 lines in main.go. ~1 day.

**Verification:** Integration test that:
1. Starts a ContinueAsNew workflow
2. Injects a crash (via test hook) after events are staged but before ContinueAsNew completes
3. On replay, verifies the workflow state is consistent — either the old run is still running (full rollback) or the new run exists and the old run is completed (full commit). No orphaned events.

---

### Phase G2 — ErrAmbiguous for DurableCall (CRITICAL #3 remaining gap)

**Problem:** The exactly-once flush mechanism (`flushEvent` at engine.go lines 2924-2948, called at lines 854-858) guarantees that a DurableCall event is persisted before the response is returned to WASM. This prevents duplicate calls on replay.

However, there is no handling for the case where the external call succeeded but the event flush failed. On retry/replay, the worker sees no event and re-executes the call — a duplicate side effect. The `ON CONFLICT DO NOTHING` dedup only helps when the event WAS persisted (replay finds it), not when the event was never written but the external call did execute.

**Fix:** Add `ErrAmbiguous` to `internal/host/errors.go` as a new error code. Add a `FlushCallEvent` method to `WorkflowStore` that writes a `pending`-status event BEFORE making the external call, then updates it to `complete` after. On replay, if a `pending` event is found (no response), return `ErrAmbiguous` to the caller so they can decide whether to retry or fail.

This is the write-ahead approach (Option A from the original plan) — it's more work than the simple immediate flush (Option B, already done) but closes the remaining gap.

**Changes:**
- `errors.go`: Add `ErrAmbiguous` error code
- `db.go`: Add `FlushCallIntent(ctx, workflowID, event) error` to `WorkflowStore` interface and all 4 backends. Inserts event with `status = 'pending'` and no response field. Uses `ON CONFLICT DO NOTHING` for idempotency.
- `db.go`: Add `CompleteCallEvent(ctx, workflowID, step, response) error` to update the pending event to `complete` with the response.
- `engine.go` lines ~840-858: Before `freshCall`, insert a pending event. After the external call succeeds, update it to complete with the response. On failure, leave it pending (replay will detect).
- `engine.go` replay path: When a `pending` event is found, return `ErrAmbiguous` with the event details so the caller can check the external service.

**Estimated:** ~30 lines in errors.go, ~60 lines in db.go, ~80 lines in engine.go. ~2 days.

**Verification:**
1. Workflow that makes a DurableCall. Kill worker after external API call succeeds but before event flush completes. On replay, verify `ErrAmbiguous` is surfaced.
2. Workflow that makes a DurableCall. Normal execution. Verify pending → complete lifecycle in event_history.
3. Replay of a workflow with completed call events. Verify the cached response is returned, no duplicate external call.

---

### Phase G3 — Wire Version Compatibility Check (HIGH #12)

**Problem:** `ValidateVersionCompatibility` exists at `internal/host/version_compat.go` lines 11-76 with full backward/forward compatibility checks. The engine has a `versionValidateFn` hook at engine.go line 474 with a `WithVersionValidation` option at line 568. But `WithVersionValidation` is never called from any production code. Zero hits in `cmd/`.

This means v2 code can silently replay v1 event history even when the versions are incompatible, producing subtle divergence at runtime with no pre-flight error.

**Fix:** In `cmd/cleat-worker/main.go`, in the dispatch path (around line 1129 where `executeCompiled` or `FinalizeWorkflowSegment` is called), call `WithVersionValidation(host.ValidateVersionCompatibility)` on the engine options when replaying (i.e., when the workflow has existing event history).

If the versions are compatible, execution proceeds. If incompatible, the workflow fails immediately with a structured error listing what's incompatible — no silent divergence.

**Changes:**
- `main.go`: In the dispatch loop, when constructing engine options for a replay, add `host.WithVersionValidation(host.ValidateVersionCompatibility)`. This is a single-line change in the option-building block.
- `engine.go` lines 663-664: The hook is already called; ensure the error is propagated clearly.

**Estimated:** ~5 lines in main.go. ~30 minutes.

**Verification:** Integration test:
1. Deploy workflow v1, execute to completion.
2. Deploy workflow v2 with an incompatible change (e.g., removed a host function import).
3. Replay v1's event history against v2 code.
4. Assert the replay fails with a clear error referencing version incompatibility, not a cryptic divergence.

---

## Stream H: Data Security & Lifecycle

**Theme:** Close the remaining gaps in data protection and event lifecycle management.

**Files touched:** `internal/host/db.go`, `internal/host/errors.go`, `cmd/cleat-worker/main.go`, `internal/host/engine.go`, `cmd/cleat-worker/config.go`

### Phase H1 — Wire Dead Letter Queue (MEDIUM #9)

**Problem:** `MoveToDeadLetterQueue` is defined in the `WorkflowStore` interface (db.go line 179), implemented in all 4 backends (Postgres at line 1403, MySQL at line 825, MSSQL at line 716, Sharded at line 373), and the dead-letter API reads from it. A Prometheus counter `cleat_workflows_dead_lettered_total` exists. But `MoveToDeadLetterQueue` is never called from any production code path — it is dead code, only exercised in tests.

Workflows that exhaust all retries are marked `failed`, not `dead_lettered`. The dead-letter API endpoint returns empty results. The distinction between `failed` (non-retryable) and `dead_lettered` (retries exhausted) is a documented concept in the original design but never wired.

**Fix:** In the workflow failure path (`main.go` around line 1070-1082), when a workflow fails and has retries remaining → retry. When a workflow fails and has exhausted retries → call `MoveToDeadLetterQueue` instead of `FailWorkflow`. The dead-letter store method should set `status = 'dead_lettered'` and record the error reason.

In the dead-letter API handler, ensure it reads `dead_lettered` status (already done — db.go line 88 queries for this). Add a `POST /api/workflows/:id/retry` endpoint to move a dead-lettered workflow back to `queued` for manual retry.

**Changes:**
- `main.go`: In the failure/retry decision block, when retries are exhausted, call `MoveToDeadLetterQueue` instead of `FailWorkflow`
- `main.go`: Add `POST /api/workflows/:id/retry` handler that calls a new `RetryWorkflow` store method
- `db.go`: Add `RetryWorkflow(ctx, workflowID) error` to `WorkflowStore` interface and all 4 backends. Transitions `dead_lettered` → `queued` with retry count reset.
- `engine.go`: In `DurableCallWithRetry`, when maxAttempts is reached, ensure the error code distinguishes "retries exhausted" from "non-retryable failure" so the caller can route to dead-letter

**Estimated:** ~30 lines in main.go, ~60 lines in db.go, ~20 lines in engine.go. ~1 day.

**Verification:**
1. Create a workflow that makes a DurableCall with `maxAttempts=3` to a failing endpoint.
2. Verify after 3 attempts the workflow is marked `dead_lettered`, not `failed`.
3. Hit `POST /api/workflows/:id/retry`, verify the workflow transitions to `queued` and is picked up by a worker.
4. Verify `cleat_workflows_dead_lettered_total` increments.
5. Verify the dead-letter API (`GET /api/dead-letter`) returns the workflow.

---

### Phase H2 — Enable Redaction by Default (CRITICAL #8 remaining gap)

**Problem:** Field-level redaction via `internal/host/redact.go` exists and is wired in main.go lines 1084-1088. But it is only enabled when `--require-auth` is true, which is the default when `--api-addr` is set. If `--api-addr` is not set (headless worker mode), redaction is off. Even when enabled, the pattern-based approach can miss sensitive data in non-standard field names.

Redaction is not encryption — data is still stored as plaintext in PostgreSQL. The original hostile review flagged "All request/response bodies stored in plaintext indefinitely" as CRITICAL. Redaction mitigates the most acute risk (secrets in event history) but does not provide encryption at rest.

**Fix (redaction hardening):**
- Change the default: `--redact` defaults to `true` unconditionally, not only when `--require-auth` is set.
- Add the flag to the Config struct properly and document it.
- Add a startup log line confirming redaction is enabled/disabled.

**Fix (encryption at rest — deferred):**
Encryption at rest (column-level encryption of event_history payloads using AES-256-GCM with a key from a KMS or environment variable) is architecturally feasible but requires design decisions about key management that are beyond a single-stream fix. Document this as a deferred feature with a concrete design note in the codebase.

**Changes:**
- `cmd/cleat-worker/config.go`: Change `RedactionEnabled` default to `true`
- `cmd/cleat-worker/main.go` line 92: Remove the conditional `|| *requireAuth`; redaction is always on by default
- `main.go`: Add `log.Printf("redaction enabled: %v", w.redactionEnabled)` at worker startup
- `docs/guide/security.md` (new): Document the threat model, what redaction covers, what it doesn't, and the encryption-at-rest design note

**Estimated:** ~10 lines config/main changes, ~60 lines documentation. ~0.5 days.

**Verification:**
1. Start worker with no auth, verify redaction is enabled and logs confirmation.
2. Start worker with `--redact=false`, verify redaction is off and logs confirmation.
3. Execute a workflow with a `"api_key": "sk-abc123"` field. Verify the event history has `"api_key": "[REDACTED]"`.

---

### Phase H3 — Structured Error Persistence (MEDIUM #12)

**Problem:** The original review noted that `CleatError` typed errors with `Code`, `Op`, `WorkflowID` fields are flattened to strings via `err.Error()` at persistence time. The `FailWorkflow` path stores only the error message string, losing the structured error code and operation context.

**What exists:** The `workflow_instances` table has `error_msg` (text). The `CleatError` type exists at `internal/host/errors.go` with `Code`, `Op`, `WorkflowID`, `Message` fields.

**Fix:** Add `error_code` and `error_op` columns to `workflow_instances`. Add a migration for all 3 database backends. Modify `FailWorkflow` to accept a `CleatError` struct instead of a raw string. In `main.go`, pass the structured error through. The API already returns `error_msg` — add `error_code` and `error_op` to the workflow detail response so operators and the UI can filter and display structured error information.

**Changes:**
- `migrations/{postgres,mysql,mssql}/012_add_error_structure.sql`: Add `error_code TEXT` and `error_op TEXT` columns to `workflow_instances`
- `db.go`: Change `FailWorkflow` signature to `FailWorkflow(ctx, workflowID, err CleatError) error` on the interface and all 4 implementations. Backfill existing rows with code=`UNKNOWN`.
- `main.go`: In the failure path, extract or construct a `CleatError` from the execution error and pass it to `FailWorkflow`
- `main.go` workflow detail handler: Include `error_code` and `error_op` in the JSON response
- UI: Display error code and operation alongside the error message

**Estimated:** ~30 lines migrations, ~60 lines db.go, ~30 lines main.go, ~20 lines API handler. ~1.5 days.

**Verification:**
1. Trigger a workflow failure with a typed `CleatError`. Verify `error_code` and `error_op` are populated in the database.
2. Fetch workflow detail via API. Verify the JSON response includes `error_code` and `error_op`.
3. Verify the UI displays the structured error fields.
4. Backward compat: verify old workflows (before migration) show `error_code: "UNKNOWN"`.

---

## Stream I: Operational Hardening

**Theme:** Close the remaining observability and operational gaps. Three MEDIUM items from the original review that have straightforward fixes.

**Files touched:** `internal/host/sharded_store.go`, `internal/host/db.go`, `cmd/cleat-worker/main.go`, `internal/wasm/build.go`, `.github/workflows/`

### Phase I1 — Batch Heartbeats (MEDIUM #6)

**Problem:** The original review flagged heartbeat writes as individual UPDATEs per workflow per heartbeat interval. At 10,000 concurrent workflows and a 5-second heartbeat interval, this is 2,000 writes/second purely for heartbeats.

**What to check:** Verify whether this was already batched. If individual UPDATEs are still being issued, batch them.

**Fix:** Replace per-workflow `UPDATE SET heartbeat_at = now() WHERE id = $1` with a single batched `UPDATE SET heartbeat_at = now() WHERE assigned_to = $1 AND status = 'running'` per heartbeat cycle per worker. This reduces N writes to 1 write per heartbeat cycle.

**Changes:**
- `db.go`: Add `BatchHeartbeat(ctx, workerID string, workflowIDs []string) error` to `WorkflowStore` interface (or use the WHERE clause approach with no IDs list needed)
- `main.go` heartbeat goroutine: Call the batch method instead of looping

**Estimated:** ~20 lines in db.go, ~20 lines in main.go. ~0.5 days.

**Verification:**
1. Start worker with 100 concurrent workflows.
2. Observe PostgreSQL query log — verify one heartbeat UPDATE per cycle, not 100 individual UPDATEs.
3. Verify heartbeat_at is updated for all running workflows assigned to the worker.

---

### Phase I2 — Parallel Shard Claims (MEDIUM #5)

**Problem:** The original review noted sequential shard iteration in `sharded_store.go`. Each shard is queried sequentially, so claim latency with 10 shards is 10× single-shard latency.

**What to check:** Verify whether `ClaimWorkflow` in `ShardedStore` (sharded_store.go lines 163-173) still iterates shards sequentially or whether it was parallelized.

**Fix (if still sequential):** Launch a goroutine per shard with a result channel. Collect results and return the first available workflow (not all need to complete). Use context cancellation to stop remaining goroutines once a claim is made.

**Changes:**
- `sharded_store.go`: Replace sequential shard loop with concurrent claims via goroutines + result channel + context cancellation on first success
- Add a metric `cleat_claim_shards_queried` histogram to track how many shards are tried before a claim succeeds

**Estimated:** ~50 lines in sharded_store.go. ~0.5 days.

**Verification:**
1. Run worker with 4 shards, 3 idle, 1 with queued workflows.
2. Verify claim latency matches single-shard latency, not 4×.
3. Verify the worker still correctly claims from all shards.

---

### Phase I3 — Build Tags in Transformer (MEDIUM #19)

**Problem:** The original review noted that the WASM build step in `internal/wasm/build.go` copies all `.go` files to the build directory without evaluating `//go:build` constraints. Platform-specific files (`_linux.go`, `_amd64.go`) or files behind build tags may be silently included. The closure validator analyzes all parsed AST files regardless of build constraints, potentially reporting false positives in platform-specific code.

**What to check:** Verify whether `//go:build` constraint parsing was added to the build step and closure validator.

**Fix:** In `internal/wasm/build.go`, before copying files to the build directory, parse `//go:build` constraints and evaluate against `GOOS=wasip1 GOARCH=wasm`. Skip files excluded for the WASM target. Warn on files with `_linux.go` or `_amd64.go` suffixes. In the closure validator, apply the same constraint evaluation before analyzing files.

**Changes:**
- `internal/wasm/build.go`: Add `go/build` constraint evaluation at file copy time. Skip files whose build constraints exclude `wasip1/wasm`.
- `internal/closure/`: Add build constraint awareness so platform-specific files aren't flagged

**Estimated:** ~80 lines in build.go, ~40 lines in closure. ~1 day.

**Verification:**
1. Create a workflow with a `_linux.go` file containing goroutines (banned in WASM). Compile to WASM. Verify the file is excluded and no goroutine error is raised.
2. Create a workflow with `//go:build !wasip1` constraint. Verify the file is excluded.
3. Verify a file with `//go:build wasip1` is included.

---

### Phase I4 — TinyGo in CI (MEDIUM #20)

**Problem:** Multiple WASM integration tests silently skip if TinyGo is not in `$PATH`. The TinyGo compilation path can break without detection in CI.

**Fix:** Install TinyGo in the CI workflow. Run the test suite with a TinyGo-specific build tag or flag. Fail the build if the TinyGo path breaks.

**Changes:**
- `.github/workflows/`: Add TinyGo installation step (download binary, add to PATH)
- `.github/workflows/`: Add a CI job that runs `go test ./... -tags=tinygo` or similar, ensuring TinyGo WASM tests execute
- Test files: Remove the `t.Skip` when TinyGo is not found; instead, use a build tag or `testing.Short()` guard so tests run in CI but can be skipped locally with `go test -short`

**Estimated:** ~30 lines in CI config, ~20 lines in test files. ~0.5 days.

**Verification:**
1. Push a change that breaks TinyGo compilation. Verify CI fails.
2. Push a change that works with TinyGo. Verify CI passes and TinyGo tests ran (not skipped).

---

### Phase I5 — Event History Pagination (MEDIUM #10)

**Problem:** The original review noted that `GET /api/workflows/:id/history` returns all events with no pagination. A workflow with millions of events produces a multi-GB response.

**What to check:** Verify whether `?offset=N&limit=M` query parameters exist on the history endpoint.

**Fix (if missing):** Add `?offset=0&limit=1000` pagination to the event history endpoint. The web UI fetches in pages. The default limit is 1000 events. The store method already returns all events — add an offset/limit variant or apply slicing in the handler.

**Changes:**
- `main.go` history handler: Parse `offset` and `limit` query parameters with defaults (0, 1000)
- `main.go`: Return `X-Total-Count` header with the total event count for the workflow
- `db.go` (optional optimization): Add `LoadEventsRange(ctx, workflowID, offset, limit)` for memory-efficient paging on very large histories

**Estimated:** ~30 lines in main.go handler, ~40 lines in db.go. ~0.5 days.

**Verification:**
1. Create a workflow with 5,000 events. Fetch history with `?offset=0&limit=100`. Verify 100 events returned with `X-Total-Count: 5000`.
2. Fetch with `?offset=4900&limit=100`. Verify the last 100 events.
3. Verify the web UI renders paged history with a "Load more" or pagination control.

---

## Issues That Cannot (or Should Not) Be Fixed

These are the remaining partially-fixed or open items where further work is architecturally infeasible, premature, or would cause more harm than good.

### 1. Encryption at Rest (CRITICAL #8)

The hostile review flagged plaintext event storage as CRITICAL. Field-level redaction (already implemented) mitigates the most acute risk — secrets like API keys, tokens, and passwords appearing in event history. True column-level encryption requires:
- A key management strategy (KMS, HashiCorp Vault, env-var passphrase)
- Key rotation and re-encryption procedures
- Performance impact analysis (encrypting every event on write, decrypting on read)
- Tradeoff between security and debuggability (encrypted events are opaque to operators)

This is a product decision, not a code fix. Document the design note and defer to post-launch.

### 2. Custom DWARF Stack Trace Parser (CRITICAL #13 / HIGH #17)

wazero already resolves WASM traps to source locations via DWARF debug info at runtime. The `resolveWasmTrap` hook in `dwarf_trap.go` formats these errors. Writing a custom DWARF parser to extract richer stack traces (all frames, variable values) would be ~2 weeks of work for marginal benefit over wazero's built-in resolution. The information is already present in error messages — it's just not as rich as a full Go stack trace. Accept as-is.

### 3. Encryption vs Redaction Scope

Redaction is pattern-based (`*token*`, `*secret*`, `*key*`, etc.) and can miss sensitive data in non-standard field names. Writing a fully general sensitive-data detector (scanning all string fields for JWT patterns, credit card numbers, etc.) is a product of ongoing refinement, not a one-time fix. The current pattern list covers the common cases. Accept as-is with documentation.

### 4. Cross-Shard Transactions

Already analyzed in the original plan — not needed because replay handles crash recovery. Child workflow creation is idempotent by workflow ID. Accept as architectural tradeoff.

### 5. Tenant Migration Safety

The scope of multi-tenant isolation is broader than this review. Accept as-is with the existing tenant-per-database/per-schema approach.

### 6. Non-Go WASM Maturity (Python/Rust)

Python WASM end-to-end tests exist in `python-sdk/tests/` and `internal/host/python_wasm_e2e_test.go`. The Python SDK has moved from `sdk/python/` to `python-sdk/`. Rust is at `crates/cleat-sdk/`. This is ongoing work tracked separately from hostile review fixes. Accept as-is.

---

## Stream Independence & Merge Order

```
Stream G: Correctness Close-Out
├── G1: Atomic ContinueAsNew (db.go + main.go)
├── G2: ErrAmbiguous (errors.go + db.go + engine.go)
└── G3: Version compat wiring (main.go one-liner)

Stream H: Data Security & Lifecycle
├── H1: Wire dead letter queue (main.go + db.go)
├── H2: Redaction defaults (config.go + main.go)
└── H3: Structured error persistence (migrations + db.go + main.go)

Stream I: Operational Hardening
├── I1: Batch heartbeats (db.go + main.go)
├── I2: Parallel shard claims (sharded_store.go)
├── I3: Build tags in transformer (build.go + closure)
├── I4: TinyGo in CI (.github/workflows/)
└── I5: Event history pagination (main.go handler)
```

**Overlap analysis:**
- G and H both touch `db.go` and `main.go`, but different functions: G touches `ContinueAsNew`/`flushEvent`/`freshCall`, H touches `FailWorkflow`/`MoveToDeadLetterQueue`/`redactionEnabled`
- I touches `db.go` (heartbeat method, pagination query) and `main.go` (heartbeat loop, history handler) — different functions from G and H
- No merge conflicts expected. Streams can be worked in parallel.

**Execution order:**
1. Stream G first (correctness — bugs that affect data integrity)
2. Stream H second (data lifecycle — depends on structured error types from G3 being understood)
3. Stream I anytime (operational — fully independent)

**Total estimated effort:** ~7 days for a single developer working sequentially, or ~3 days with 3 parallel streams.

## Verification Checklist

After all streams are merged:

- [ ] G1: ContinueAsNew crash test — no orphaned events
- [ ] G2: DurableCall with injected crash — ErrAmbiguous surfaced
- [ ] G3: Incompatible version replay — clear error, no silent divergence
- [ ] H1: Dead-letter flow: retries exhausted → dead_lettered → manual retry → queued → completed
- [ ] H2: Redaction enabled by default, confirmed in startup logs
- [ ] H3: Structured error fields populated on failure, visible in API/UI
- [ ] I1: Batch heartbeat — one UPDATE per cycle, not N
- [ ] I2: Parallel shard claims — latency matches single shard
- [ ] I3: Build tags — `_linux.go` files excluded from WASM build
- [ ] I4: TinyGo in CI — build fails if TinyGo path breaks
- [ ] I5: Event history pagination — `?offset=0&limit=100` returns 100 events with total count header

## Post-Fix Status Projection

After all three streams are implemented:

| Severity | Fixed | Partially Fixed | Still Open | Architectural Tradeoff |
|----------|-------|-----------------|------------|------------------------|
| CRITICAL (14) | 13 | 0 | 0 | 1 (encryption at rest deferred) |
| HIGH (21) | 20 | 0 | 0 | 1 (DWARF richness deferred) |
| MEDIUM (22) | 18 | 0 | 0 | 4 (cross-shard, tenant, etc.) |

**Every actionable CRITICAL and HIGH issue will be closed.** The remaining items are architectural tradeoffs with documented rationale, not bugs.
