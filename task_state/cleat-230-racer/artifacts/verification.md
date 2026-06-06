# Race Condition Audit — Re-Verification Report

**Task:** cleat-230-racer (re-verification of cleat-230-race)
**Date:** 2026-06-06
**Type:** Read-only audit — no code changes

## Summary

All 7 findings from the original exploration report independently re-verified against the current codebase. Line numbers confirmed matching. All 6 dead code claims also confirmed.

---

## Finding 1: REAL RACE — Unprotected `bytes.Buffer` on shared Runtime

**Status: CONFIRMED** — No change from exploration report.

- `engine/runtime.go:37-38`: `stdout bytes.Buffer`, `stderr bytes.Buffer` on shared `Runtime` struct
- `engine/runtime.go:60-63`: `Stdout()`/`Stderr()` read from buffers
- `engine/runtime.go:190-195`: `InstantiateModuleNamed()` calls `r.stdout.Reset()`/`r.stderr.Reset()` and passes `&r.stdout`/`&r.stderr` to wazero module config
- `engine/backend_wazero.go:46-48`: `PerExecution()` returns `&wazeroBackend{rt: b.rt}` — shares the same `*Runtime`
- `bytes.Buffer` is documented as not goroutine-safe for concurrent writes

---

## Finding 2: DEAD CODE — `execEngines` sync.Map never written to

**Status: CONFIRMED** — No change from exploration report.

- `cmd/cleat-worker/main.go:1025`: `execEngines sync.Map` declared
- `cmd/cleat-worker/main.go:2125`: Only `execEngines.Load(wfID)` call
- Zero `execEngines.Store()` or `execEngines.Delete()` calls anywhere in Go source (confirmed via grep across entire repo)
- Update dispatch system (`dispatchPendingUpdates`) is dead code — updates are persisted to DB but never dispatched to running engines

---

## Finding 3: DESIGN — Watchdog restart can double-launch loops

**Status: CONFIRMED** — No change from exploration report.

- `cmd/cleat-worker/main.go:2821`: `restartLoop()` function
- `cmd/cleat-worker/main.go:2846-2853`: Cancels old context, waits up to 5 seconds, then proceeds regardless
- `cmd/cleat-worker/main.go:2863-2868`: Launches new goroutine even if old one hasn't exited (when 5s timeout fires)
- All per-loop goroutines affected (dispatch, heartbeat, reaper, schedule, etc.)

---

## Finding 4: DESIGN — Drain TOCTOU races

**Status: CONFIRMED** — No change from exploration report.

**4a (claim race):**
- `cmd/cleat-worker/main.go:1273`: `w.draining.Load()` check
- `cmd/cleat-worker/main.go:1285`: `ClaimStickyWorkflows` — work claimed after check
- `cmd/cleat-worker/main.go:1312`: `ClaimWorkflows` — work claimed after check
- Between check and claim, `handleDrainStart` can set `draining = true`

**4b (API bypass):**
- `cmd/cleat-worker/main.go:3301-3304`: `handleStartWorkflow` checks `memoryController.CanAcceptAPIWorkflows()` but never checks `w.draining.Load()`
- Workflows started via API during drain are persisted but never claimed; abandoned on shutdown in single-worker deployments

---

## Finding 5: DESIGN — Drain-complete notification race

**Status: CONFIRMED** — No change from exploration report.

- `cmd/cleat-worker/main.go:1236-1242`: dispatch loop detects inflight==0 during drain, calls `w.cancel()`
- `cmd/cleat-worker/main.go:3172-3175`: `handleDrainStatus` also detects drain complete, closes `drainCh` via `drainOnce.Do`
- If dispatch loop fires `w.cancel()` first, the HTTP server shuts down and `handleDrainStatus` may be killed before closing `drainCh`
- External callers waiting on `DrainComplete()` block forever

---

## Finding 6: LATENT — `ShardedStore.Close()` without lock

**Status: CONFIRMED** — No change from exploration report.

- `engine/sharded_store.go:74-80`: `Close()` iterates over `s.shards` without `mu.RLock()`
- All other accesses (e.g., `Shards()`, `getShard()`, `tryEachShard()`, `forEachShard()`) hold `mu.RLock()`
- Safe today because shards are immutable after construction, but fragile for future changes

---

## Finding 7: LATENT — Compaction lock ordering vs execution

**Status: CONFIRMED** — No change from exploration report.

- `engine/db.go:2848-2872`: `PostgresStore.CompactHistory()`:
  1. DELETE FROM event_history WHERE step < keepStep (line 2856)
  2. UPDATE workflow_instances SET compaction_state = ... WHERE id = $3 (line 2863)
  - Lock order: event_history → workflow_instances
- Workflow execution path: UPDATE workflow_instances → INSERT event_history
  - Lock order: workflow_instances → event_history (reverse)
- Potential for Postgres deadlock (40P01), though unlikely due to non-overlapping row predicates
- No `generation = ?` guard on compaction UPDATE (line 2865)
- No retry logic in compaction path

---

## Dead Code Verification

All 6 dead code claims confirmed against current codebase:

| # | Location | Description | Verification |
|---|----------|-------------|-------------|
| 1 | `engine/runtime.go:54-56` | `completeMu`, `completeResult`, `completeErr` | `completeMu.` — zero accesses in engine/ (grep confirmed). Replaced by context-based `cleatComplete` mechanism. |
| 2 | `engine/engine.go:1671` | `signals map[string]string` | `.signals` — zero accesses in engine.go (grep confirmed) |
| 3 | `engine/engine.go:4087` | `QueryHandlers()` method | Defined but zero callers in *.go files (grep confirmed) |
| 4 | `engine/backend_wasmtime.go:335` | `executeViaDispatcher()` | Defined but zero callers (grep confirmed) |
| 5 | `engine/compaction.go:504` | `loadAllEventsForCompaction()` | Defined but zero callers in production code (only referenced in benchmarks) |
| 6 | `engine/runtime.go:46-47` | `workEntryPoint`/`workInput` on Runtime | Written at lines 483-484 but never read from Runtime struct; wasmtimeBackend has separate copies |

---

## Line Number Drift

All line numbers from the original exploration report match the current codebase exactly. No drift detected. This indicates no changes have been made to the audited files since the original exploration.

---

## Areas Verified Safe (no change)

- `execSession.mu` protecting `stateStore` — every access holds the mutex
- `execSession.mu` for `queryState` and `deferrals` writes
- `wasmtimeBackend.PerExecution()` — creates fresh structs (safe)
- `wasmLRUCache`/`WASMCache` — all methods lock `mu`
- `wazeroInitOnce` — correct `sync.Once` usage
- `scheduleMu` — correct defense against restart race
- `parentWakeCh` — correct non-blocking channel sends
- All store implementations — no mutable shared state after construction
- `inflight sync.Map` — correct for concurrent insert/delete/iterate
- `drainOnce` — prevents double-close
- `SELECT ... FOR UPDATE SKIP LOCKED` — DB-level concurrency for claiming

---

## Risk Assessment (unchanged)

| Risk | Likelihood | Impact | Overall |
|------|-----------|--------|---------|
| WASM stdout/stderr buffer race | Low | Low (corrupted debug output) | Low |
| execEngines dead code (updates broken) | Certain | High (feature non-functional) | High |
| Watchdog double-dispatch | Low | Medium (double concurrency) | Medium |
| Drain TOCTOU | Low-Medium | Low (best-effort drain) | Low |
| Drain notification race | Low | Low (blocks external callers) | Low |
| ShardedStore Close() | Very Low | Low | Very Low |
| Compaction deadlock | Very Low | Medium (failed compaction) | Very Low |
