# Log Cleanup Exploration Report

**Task:** cleat-230-logse
**Scope:** All Go source under `engine/`, `cmd/cleat-worker/`, `internal/`, `plugin/`, `cleat/`
**Excluded:** examples/, benchmarks/, tests, plugins/ (plugin implementations)

---

## Summary

Found **~85 `log.Printf` calls** and **~30 `fmt.Printf` debug calls** in production code.
The hot-path findings are the most concerning — 3 categories need immediate action.
The engine already has structured logging infrastructure (`slog.Logger`) wired through
`engine.Engine.log()` and `engine.App.Logger()`, but the store layer (`db.go`,
`mysql_store.go`, `mssql_store.go`) has no logger access and uses bare `log.Printf`
throughout.

---

## Category 1: ENGINE HOT PATH — Must Fix (12 sites, ~$3 of budget)

These fire **per-event** during workflow execution or replay. Every invocation hits them.

### 1a. engine/engine.go — Debug log.Printf in child workflow creation (3 calls)

| Line | Message | Severity |
|------|---------|----------|
| 2730 | `[engine] childWorkflowWithVersion: name=%q version=%d childVersion=%d...` | **DEBUG** — should be removed or gated |
| 2776 | `[engine] calling StartChildWorkflowAtomic: name=%q parentID=%s...` | **DEBUG** — should be removed or gated |
| 2780 | `[engine] StartChildWorkflowAtomic FAILED: %v` | **DUPLICATE** — error already logged via `s.engine.log().ErrorContext(...)` at line 2782 |

**Fix:** Delete lines 2730-2731, 2776, and 2780. The structured equivalent at line 2782 already captures the error. The two debug lines are development artifacts.

### 1b. engine/db.go — decryptAndRedactEventRecord failures (10 calls on 10 fields)

Lines 550, 557, 567, 574, 581, 588, 595, 602, 609, 616 — one `log.Printf` per encrypted field.

These fire when decryption fails during event replay. Each failure increments `decryptionErrorsTotal` metric. The `log.Printf` is noisy and un-structured.

**Fix:** The PostgresStore has no logger. Options:
- Add `logger *slog.Logger` field to PostgresStore (cleanest)
- Use `slog.Default().Warn(...)` as a stopgap
- Rate-limit: decrypt errors are already metered, so this is duplicate signal

### 1c. engine/db.go — decryptPayloadJSON failure (1 call)

Line 644. Same pattern as 1b.

### 1d. engine/compaction.go — Compaction summary (1 call)

Line 220: `log.Printf("compact: workflow=%s events=%d compacted=%d kept=%d state_size=%d", ...)`

This fires on every compaction run. Structured fields (workflow_id, event_count, etc.) should be logged via slog.

---

## Category 2: ENGINE MEDIUM PATH — Should Fix (20 sites, ~$1 of budget)

These fire per-workflow-operation but not per-event. Non-fatal error logging.

### 2a. engine/db.go — Idempotency update failures (8 calls)

Lines 1442, 1448, 1463, 1479, 1947, 1996, 2019, 2034, 2071.

All are non-fatal "best-effort" idempotency key updates. The log message is already clear:
`"idempotency update failed (non-fatal): %v"`. But they should include workflow_id/tenant_id
context via structured logging instead of bare printf.

### 2b. engine/db.go — Parent close policy enforcement (2 calls)

Lines 2019, 2034: `[store] enforceParentClosePolicy: begin TERMINATE/REQUEST_CANCEL tx: %v`

These log transaction begin errors. Should include workflow context via slog.

### 2c. engine/db.go — StartChildWorkflowAtomic debug (1 call)

Line 2344: `[engine] StartChildWorkflowAtomic: defName=%q defVersion=%d...`

**DEBUG** — should be removed or converted to slog.Debug.

### 2d. engine/db.go — Release concurrency keys (1 call)

Line 4224: `[db] release concurrency keys for run %s: %v`

Non-fatal cleanup error. Should use structured logging.

### 2e. engine/mysql_store.go — Same patterns as db.go (6 calls)

Lines 409, 457, 714, 720, 735, 899. Mirror of the Postgres store patterns.

### 2f. engine/mssql_store.go — Same patterns (9 calls)

Lines 953, 999, 1037, 1208, 1214, 1228, 1383, 1398, 2934. Mirror patterns.

**Fix for all Category 2:** Add `logger *slog.Logger` to PostgresStore/MySQLStore/MSSQLStore
structs. Plumb from StoreFactory (in `cmd/cleat-worker/main.go`). Use
`logger.WarnContext(ctx, msg, "workflow_id", wfID, "error", err)` pattern.

---

## Category 3: WORKER LOOP — Should Fix (4 sites, ~$0.5 of budget)

Periodic operations in the worker daemon.

### 3a. cmd/cleat-worker/memory_monitor.go (2 calls)

Lines 245, 262: Memory read/sample failures. The monitor already caches the last reading,
so these are non-fatal. The worker has a logger available via the App struct.

### 3b. cmd/cleat-worker/memory_controller.go (2 calls)

Lines 115, 184: Queue depth query failure, record memory sample failure.
Worker ID is in the message but should be a structured field.

### 3c. cmd/cleat-worker/app.go (2 calls)

Lines 55, 57: HTTP API start/error. One-time startup, but the worker already has a logger.
Should use structured logging for consistency.

---

## Category 4: STARTUP / ONE-TIME — Lower Priority (12 sites, ~$0.5 of budget)

These fire once at startup or periodically (not per-workflow).

| File | Lines | Description |
|------|-------|-------------|
| `internal/telemetry/tracing.go` | 59 | OTel endpoint startup message |
| `engine/version_metrics.go` | 161, 163 | Stale version alerts |
| `engine/versioned_loader.go` | 188, 214 | Workflow deploy/deprecate |
| `engine/plugin_loader.go` | 357, 407 | Plugin deploy/deprecate |
| `engine/wasm_disk_cache.go` | 51, 115, 120, 125, 160, 163, 252 | Cache operations |
| `engine/version_gc.go` | 126 | Version GC |

**Fix:** Low urgency. These are one-time or infrequent. Migrate to slog when touching the file for other reasons.

---

## Category 5: DEBUG fmt.Printf — Should Be Removed or Gated (30 sites)

### 5a. engine/backend_wasmtime.go (~15 fmt.Printf calls)

Lines 102, 2232, 2239, 2249, 2258, 2274, 2281, 2294, 2313, 2315, 2857, 3050, 3063, 3067.

All prefixed with `[WIT_SCAN]`, `[WIT_DUMP]`, `[DISPATCH]`, `[DEFINE]`, `[CGO_COMPONENT]`.
These are development diagnostics for the WIT (WebAssembly Interface Types) component model
scanner. They fire during WASM module loading.

**These should be removed or gated behind a `--debug-wasm` flag.** They are not structured
logs — they're debug printfs that spam stdout.

### 5b. engine/wit_dylib_stack.go (~15 fmt.Printf calls)

Lines 177, 183, 185, 231, 235, 243, 274, 650, 656, 659, 661, 663, 667.

Same category as 5a — WIT component model debug diagnostics.

**Fix for Category 5:** Either delete (the code is stable) or gate behind a debug flag.

---

## Category 6: ACCEPTABLE — No Action Needed

| File | Reason |
|------|--------|
| `cleat/runtime.go:1529,2156,2165` | SDK misuse warnings — only fires on developer error, not in production |
| `plugin/recovery.go:122,145` | Plugin panic recovery — crash diagnostics, appropriate for printf |
| `plugin/tenant_db.go:61` | One-time startup fallback message |
| `plugin/migration.go:194,206` | Migration skip messages — one-time at startup |
| `cmd/cleat-bench/main.go:*` | CLI benchmark tool |
| `cmd/cleat/run_embedded.go:*` | CLI tool |
| `migration/runner.go:*` | Migration CLI tool |
| `examples/widget-store-as/host/main.go:*` | Example code |
| `internal/telemetry/tracing.go:59` | One-time startup (already in Cat 4, but lowest priority) |

---

## Already Clean

- `cmd/cleat-worker/main.go` — 0 log.Printf calls. Uses slog with JSON handler throughout.
- All 22 plugins use slog.Info/slog.Error via `plugin.Environment.Logger`.
- `engine/app.go` — Uses slog throughout. Has `Logger()` accessor.

---

## Implementation Plan

A developer agent can fix all hot-path items in a single session:

### Phase 1 — Quick wins (delete debug lines): ~15 minutes
1. Delete `engine/engine.go:2730-2731` (debug log)
2. Delete `engine/engine.go:2776` (debug log)
3. Delete `engine/engine.go:2780` (duplicate error log — already at 2782 via slog)
4. Delete `engine/db.go:2344` (debug log)

### Phase 2 — Store logger plumbing: ~2 hours
1. Add `logger *slog.Logger` field to `PostgresStore`, `MySQLStore`, `MSSQLStore` structs
2. Add `WithLogger(*slog.Logger)` option to each store
3. Plumb logger from `StoreFactory` construction in `cmd/cleat-worker/main.go`
4. Convert Category 1b, 1c, and all Category 2 log.Printf calls to `s.logger.WarnContext(...)`
5. Convert `engine/compaction.go:220` to structured log

### Phase 3 — Worker loop: ~30 minutes
1. Convert `memory_monitor.go` and `memory_controller.go` log.Printf to use worker's logger
2. Convert `app.go:55,57` HTTP server messages to slog

### Phase 4 — WIT debug cleanup: ~15 minutes
1. Delete or gate all `fmt.Printf` calls in `backend_wasmtime.go` and `wit_dylib_stack.go`

### Phase 5 — Startup logs (optional, low priority): ~30 minutes
1. Convert Category 4 one-time messages to slog if time permits

---

## Risks

- **Store logger plumbing touches 3 store backends.** Postgres, MySQL, MSSQL stores mirror
  each other's patterns. Changes should be applied consistently.
- **Debug printf removal.** The WIT scanner debug output might be useful for future Component
  Model debugging. Consider gating behind `--debug-wit` flag instead of deleting.
- **No test changes needed.** Log output format changes don't affect any test assertions.

## Coupling

- NONE with cleat-230-race (race condition audit) — different files, no shared state
- NONE with cleat-230-error (error message quality) — different concerns, no shared types
