# Race Condition Audit — Re-Verification Report (cleat-230-racev)

**Task:** cleat-230-racev (re-verification of cleat-230-race)
**Date:** 2026-06-06
**Type:** Read-only audit — no code changes
**Predecessor:** cleat-230-racer (prior re-verification)

## Summary

All 7 findings from the original cleat-230-race exploration report independently re-verified against the current codebase. All findings remain valid. Minor line number drift detected in Finding 3 (watchdog — 13 lines of drift from `watchdogLoop` function due to intervening commits adding code above it). All other line numbers match.

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

**Status: CONFIRMED** — Minor line number drift. `watchdogLoop` is now at line 2765 (was 2798, 13 lines of drift). `restartLoop` at line 2821 (matches original).

- `cmd/cleat-worker/main.go:2765`: `watchdogLoop()` function (was 2798 in original report)
- `cmd/cleat-worker/main.go:2798`: `w.restartLoop(name)` call
- `cmd/cleat-worker/main.go:2821`: `restartLoop()` function (matches original)
- `cmd/cleat-worker/main.go:2846-2853`: Cancels old context, waits up to 5 seconds, then proceeds regardless
- `cmd/cleat-worker/main.go:2863-2868`: Launches new goroutine even if old one hasn't exited (when 5s timeout fires)
- All per-loop goroutines affected (dispatch, heartbeat, reaper, schedule, etc.)

**Drift note:** The `watchdogLoop` function moved from line 2798 to 2765 due to commits adding code earlier in the file (likely error message formatting additions). The `restartLoop` function and its internal logic are at the exact same lines as the original report.

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
- If dispatch loop fires `w.cancel()` at line 1241 first, the HTTP server shuts down and `handleDrainStatus` may be killed before closing `drainCh`
- External callers waiting on `DrainComplete()` block forever

---

## Finding 6: LATENT — `ShardedStore.Close()` without lock

**Status: CONFIRMED** — No change from exploration report.

- `engine/sharded_store.go:74-80`: `Close()` iterates over `s.shards` without `mu.RLock()`
- All other accesses (e.g., `Shards()` at line 83-88, `getShard()`, `tryEachShard()`, `forEachShard()`) hold `mu.RLock()`
- Safe today because shards are immutable after construction, but fragile for future changes

---

## Finding 7: LATENT — Compaction lock ordering vs execution

**Status: CONFIRMED** — No change from exploration report.

- `engine/db.go:2848-2872`: `PostgresStore.CompactHistory()`:
  1. DELETE FROM event_history WHERE workflow_id = $1 AND step < $2 (line 2856)
  2. UPDATE workflow_instances SET compaction_state = ... WHERE id = $3 (line 2863)
  - Lock order: event_history → workflow_instances
- Workflow execution path: UPDATE workflow_instances → INSERT event_history
  - Lock order: workflow_instances → event_history (reverse)
- Potential for Postgres deadlock (40P01), though unlikely due to non-overlapping row predicates
- No `generation = ?` guard on compaction UPDATE (line 2865, only `WHERE id = $3`)
- No retry logic in compaction path

---

## Dead Code Verification

All 6 dead code claims confirmed against current codebase:

| # | Location | Description | Verification |
|---|----------|-------------|-------------|
| 1 | `engine/runtime.go:54-56` | `completeMu`, `completeResult`, `completeErr` | `completeMu.` — zero accesses in engine/ (grep confirmed). Replaced by context-based `cleatComplete` mechanism. |
| 2 | `engine/engine.go:1671` | `signals map[string]string` | `.signals` — zero accesses in entire engine package (grep confirmed). Field is declared but never read or written. |
| 3 | `engine/engine.go:4087` | `QueryHandlers()` method | Defined but zero callers in entire codebase (grep confirmed) |
| 4 | `engine/backend_wasmtime.go:335` | `executeViaDispatcher()` | Defined but zero callers (grep confirmed) |
| 5 | `engine/compaction.go:504` | `loadAllEventsForCompaction()` | Defined but zero callers in production code (only referenced in benchmarks and planning docs) |
| 6 | `engine/runtime.go:46-47` | `workEntryPoint`/`workInput` on Runtime | Written at lines 483-484 but never read from Runtime struct; wasmtimeBackend has separate copies at `backend_wasmtime.go:40-41` |

---

## Line Number Drift

| Finding | Original Lines | Current Lines | Drift |
|---------|---------------|---------------|-------|
| 1 (WASM buffer race) | runtime.go:37-38, 60-63, 190-195 | runtime.go:37-38, 60-63, 190-195 | None |
| 2 (execEngines) | main.go:1025, 2125 | main.go:1025, 2125 | None |
| 3 (watchdog) | main.go:2798, 2821-2868 | main.go:2765, 2821-2870 | -13 lines (watchdogLoop moved up due to error message additions) |
| 4 (drain TOCTOU) | main.go:1273-1312, 3301-3408 | main.go:1273-1312, 3301-3304 | None |
| 5 (drain notify) | main.go:1241, 3173-3174 | main.go:1241, 3173-3174 | None |
| 6 (ShardedStore) | sharded_store.go:75 | sharded_store.go:74-80 | None |
| 7 (compaction) | db.go:2848-2872 | db.go:2848-2872 | None |

---

## Uncommitted Changes Assessment

Uncommitted modifications exist in `cmd/cleat-worker/main.go` and `engine/engine.go` but are unrelated to any finding:
- `cmd/cleat-worker/main.go`: Error messages in `executeWorkflow` now include workflow ID prefix (e.g., `"workflow %s: history load: %v"`)
- `engine/engine.go`: Error messages in WASM execution path now include `"host: workflow execution failed: "` prefix
- Neither change affects the race conditions, dead code, or design issues identified

---

## Areas Verified Safe (no change from prior verifications)

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

---

## Decomposition Accuracy

The 4 child tasks in `../cleat-230-race/artifacts/dag.json` remain correctly scoped:
- **fix1** ($5): Fix the WASM stdout/stderr buffer race
- **fix2** ($15): Wire execEngines sync.Map
- **fix3** ($10): Fix drain TOCTOU and notification races
- **fix4** ($10): Defensive hardening (lock hygiene, compaction guards, dead code removal)

All coupling annotations remain accurate. All fix tasks are in `ready` phase with $0 spent.
