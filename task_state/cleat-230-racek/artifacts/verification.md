# Race Condition Audit — Independent Verification Report (cleat-230-racek)

**Task:** cleat-230-racek (independent re-verification of cleat-230-race exploration findings)
**Date:** 2026-06-06
**Type:** Read-only audit — no code changes
**Method:** Line-by-line code inspection, grep verification, git diff analysis of uncommitted changes

---

## Summary

Independently re-verified all 7 findings and all 6 dead code claims from the cleat-230-race exploration report against the current codebase (develop HEAD, commit `fbaf750` + uncommitted changes). All findings confirmed. The uncommitted changes are error message formatting only — no impact on any finding.

---

## Finding 1: REAL RACE — Unprotected `bytes.Buffer` on shared Runtime

**Status: CONFIRMED**
**Files:** `engine/runtime.go:37-38,190-195`, `engine/backend_wazero.go:46-48`

- `runtime.go:37-38`: `stdout bytes.Buffer`, `stderr bytes.Buffer` on shared `Runtime` struct
- `runtime.go:60-63`: `Stdout()`/`Stderr()` read from buffers via `r.stdout.String()`/`r.stderr.String()`
- `runtime.go:190-195`: `InstantiateModuleNamed()` calls `r.stdout.Reset()`, `r.stderr.Reset()` and passes `&r.stdout`/`&r.stderr` to wazero `WithStdout`/`WithStderr`
- `backend_wazero.go:46-48`: `PerExecution()` returns `&wazeroBackend{rt: b.rt}` — **shares the same `*Runtime`** across concurrent executions
- `bytes.Buffer` is documented as not goroutine-safe for concurrent writes
- Three operations compete: Reset (InstantiateModuleNamed), Write (wazero via stdout/stderr), Read (Stdout/Stderr)
- Contrast with `wasmtimeBackend.PerExecution()` at `backend_wasmtime.go:62-64` which creates a fresh struct with only the shared engine, no shared buffers

**Line numbers:** Match exploration report exactly.

---

## Finding 2: DEAD CODE — `execEngines` sync.Map never written to

**Status: CONFIRMED**
**Files:** `cmd/cleat-worker/main.go:1025,2125`

- `main.go:1025`: `execEngines sync.Map` declared on Worker struct
- `main.go:2125`: Only usage is `w.execEngines.Load(wfID)` in `dispatchPendingUpdates`
- Zero `execEngines.Store()` calls anywhere in Go source (confirmed via grep across entire repo)
- Zero `execEngines.Delete()` calls anywhere in Go source
- `Load` always returns `!ok`, making `dispatchPendingUpdates` dead code
- Update dispatch system silently non-functional — updates persisted to DB but never dispatched to running engines

**Line numbers:** Match exploration report exactly.

---

## Finding 3: DESIGN — Watchdog restart can double-launch dispatch loops

**Status: CONFIRMED** (33 lines of drift: `watchdogLoop` at 2765, was 2798)
**File:** `cmd/cleat-worker/main.go:2821-2868`

- `restartLoop` at line 2821:
  - Cancels old per-loop context at line 2847 (`prev.cancel()`)
  - Waits for old goroutine's done channel with **5-second timeout** (lines 2848-2853)
  - **After timeout, launches new goroutine anyway** (lines 2856-2868)
- If old dispatch loop is stuck (slow DB call, idle sleep), both loops run concurrently
- Both will claim workflows from DB, doubling the configured concurrency limit
- Affects ALL per-loop goroutines (dispatch, heartbeat, reaper, schedule, etc.)
- The `consecutiveDBErrors` counter at line 1032 is accessed without atomics — safe in single-dispatch, but would race if double-dispatch occurs

**Line number drift:** `watchdogLoop` moved from 2798 to 2765 (33 lines of drift) due to intervening code additions. The `restartLoop` function itself is at the same line (2821) as the original report. This drift matches observations from cleat-230-racev and cleat-230-racec.

---

## Finding 4: DESIGN — Drain TOCTOU races

**Status: CONFIRMED**
**Files:** `cmd/cleat-worker/main.go:1273-1312,3301-3305`

**4a. Claim race after drain check:**
- Line 1273: `w.draining.Load()` checked before claiming
- Lines 1285/1312: `ClaimStickyWorkflows` / `ClaimWorkflows` DB calls
- Between check and claim, `handleDrainStart` at line 3143 sets `draining = true`
- Result: one extra workflow claimed and executed after drain begins

**4b. API workflows bypass drain:**
- `handleStartWorkflow` at line 3301 checks `memoryController.CanAcceptAPIWorkflows()` only (line 3302)
- **Never** checks `w.draining.Load()`
- Workflows started via API during drain are persisted but never claimed; abandoned on shutdown in single-worker deployments
- The server returns 200 for drain-period API workflow starts, misleading the caller

**Line numbers:** Match exploration report exactly (1273, 1285, 1312, 3301).

---

## Finding 5: DESIGN — Drain-complete notification race

**Status: CONFIRMED**
**Files:** `cmd/cleat-worker/main.go:1241,3172-3175`

- Two independent drain-complete detection paths:
  - `dispatchLoop` line 1241: calls `w.cancel()` when `draining && inflight==0`
  - `handleDrainStatus` lines 3172-3175: closes `drainCh` via `drainOnce.Do` when `draining && count==0`
- Race: if dispatch loop fires `w.cancel()` at line 1241 first, root context cancels, HTTP server shuts down, `handleDrainStatus` may be killed mid-response
- `drainCh` is never closed → external callers waiting on `DrainComplete()` block forever
- `drainOnce` properly prevents double-close, but close may never happen at all

**Line numbers:** Match exploration report exactly (1241, 3173-3174).

---

## Finding 6: LATENT — `ShardedStore.Close()` without lock

**Status: CONFIRMED**
**File:** `engine/sharded_store.go:74-80`

```go
func (s *ShardedStore) Close() {
    for _, shard := range s.shards {  // line 75 — no mu.RLock()
        if shard.Close != nil {
            shard.Close()
        }
    }
}
```

Compare with `Shards()` at lines 83-89 which properly holds `s.mu.RLock()`. All other accesses (`getShard`, `tryEachShard`, `forEachShard`, `ReapStaleInstances`, etc.) hold `s.mu.RLock()`. Safe today because shards are immutable after construction (`NewShardedStore` creates the slice and never modifies it), but fragile for future changes like dynamic shard rebalancing.

**Line numbers:** Match exploration report exactly (74-80).

---

## Finding 7: LATENT — Compaction lock ordering vs execution

**Status: CONFIRMED**
**Files:** `engine/db.go:2848-2872`, `engine/compaction.go:189-223`

Lock acquisition order in `PostgresStore.CompactHistory`:
1. `DELETE FROM event_history WHERE workflow_id = $1 AND step < $2` (line 2856) — acquires row locks on event_history
2. `UPDATE workflow_instances SET compaction_state = ... WHERE id = $3` (line 2863) — acquires row lock on workflow_instances

Workflow execution path acquires locks in reverse order:
1. `UPDATE workflow_instances` (claim the workflow) → acquires row lock on workflow_instances
2. `INSERT INTO event_history` (record event) → acquires row lock on event_history

Postgres would detect a true deadlock (error 40P01) and abort one transaction. The compaction path has **no retry logic**. In practice, extremely unlikely because:
- Compaction operates on old steps (`step < keepStep`) well behind the write head
- The DELETE predicate excludes rows concurrently being inserted

Additional concerns:
- No `generation` guard on the compaction UPDATE (line 2865: only `WHERE id = $3`, no `AND generation = ?`)
- No retry loop around `CompactHistory` for transient deadlock errors

**Line numbers:** Match exploration report exactly (2848-2872 in db.go, 189-223 in compaction.go).

---

## Dead Code Verification

All 6 dead code claims independently confirmed against current codebase:

| # | Location | Description | Verification |
|---|----------|-------------|-------------|
| 1 | `engine/runtime.go:54-56` | `completeMu`, `completeResult`, `completeErr` | Zero `completeMu.` accesses in engine/ (grep confirmed). Replaced by context-based `cleatComplete` mechanism (runtime.go:23-28). |
| 2 | `engine/engine.go:1671` | `signals map[string]string` | Zero `.signals` accesses in entire engine package (grep confirmed). Field declared but never read or written. |
| 3 | `engine/engine.go:4087` | `QueryHandlers()` method | Defined but zero callers in entire codebase (grep confirmed). Related `QueryHandlers` interface at `cleat/runtime.go:344` is part of the public API but the `execSession` method is never invoked. |
| 4 | `engine/backend_wasmtime.go:335` | `executeViaDispatcher()` | Defined but zero callers (grep confirmed). The dispatcher mechanism was refactored and this function is orphaned. |
| 5 | `engine/compaction.go:504` | `loadAllEventsForCompaction()` | Defined but zero callers in production code. Only referenced in `benchmarks/db_bench_test.go:400`. The `CompactWorkflowHistory` function at compaction.go:189 instead uses `store.LoadEventHistory(ctx, workflowID)`. |
| 6 | `engine/runtime.go:46-47` | `workEntryPoint`/`workInput` on Runtime | Written at lines 483-484 but **never read** from Runtime struct. The `wasmtimeBackend` has separate identically-named fields at `backend_wasmtime.go:40-41` that ARE used (read at lines 2732, 2742, 2747). The Runtime struct fields serve no purpose. |

---

## Line Number Drift Summary

| Finding | Original Lines | Current Lines | Drift |
|---------|---------------|---------------|-------|
| 1 (WASM buffer race) | runtime.go:37-38, 60-63, 190-195 | runtime.go:37-38, 60-63, 190-195 | None |
| 2 (execEngines) | main.go:1025, 2125 | main.go:1025, 2125 | None |
| 3 (watchdog) | watchdogLoop:2798, restartLoop:2821 | watchdogLoop:2765, restartLoop:2821 | -33 lines on watchdogLoop |
| 4 (drain TOCTOU) | main.go:1273-1312, 3301-3408 | main.go:1273-1312, 3301-3305 | None |
| 5 (drain notify) | main.go:1241, 3173-3174 | main.go:1241, 3173-3174 | None |
| 6 (ShardedStore) | sharded_store.go:75 | sharded_store.go:74-80 | None |
| 7 (compaction) | db.go:2848-2872 | db.go:2848-2872 | None |

---

## Uncommitted Changes Impact Assessment

Uncommitted modifications in `cmd/cleat-worker/main.go`, `engine/engine.go`, and `engine/dwarf_trap.go` are error message formatting changes only:
- `main.go:1466,1504`: Added workflow ID prefix to FailWorkflow error messages
- `engine/dwarf_trap.go:37`: Changed "WASM trap:" prefix to "wasm trap:"
- `engine/engine.go:1118,1120,1316,1318`: Added "host: workflow execution failed:" prefix, changed `%s` to `%w` for error wrapping

**No impact on any finding.** All line numbers, types, and concurrency patterns are unaffected.

---

## Areas Verified Safe (unchanged from previous audits)

Independently confirmed the following areas remain race-free:

- `execSession.mu` protecting `stateStore` — every access holds the mutex (11 uses across engine.go)
- `execSession.mu` for `queryState` and `deferrals` writes — consistently locked
- `wasmtimeBackend.PerExecution()` — creates fresh struct with independent store (safe)
- `wasmLRUCache` / `WASMCache` — all methods lock `mu`
- `wazeroInitOnce` — correct `sync.Once` usage
- `scheduleMu` — correct defense against restart race
- `parentWakeCh` buffering — correct non-blocking channel sends with `select/default`
- All store implementations (`PostgresStore`, `MySQLStore`, `MSSQLStore`) — no mutable shared state after construction
- `inflight sync.Map` — correct for concurrent insert/delete/iterate
- `drainOnce` — prevents double-close (though close may never happen — see Finding 5)
- `SELECT ... FOR UPDATE SKIP LOCKED` — DB-level concurrency for workflow claiming
- `circuitOpen atomic.Bool` — correct atomic usage
- `consecutiveDBErrors` — safe today (single dispatch loop goroutine); would race if Finding 3 double-dispatch fires

---

## Risk Assessment (unchanged)

| Finding | Likelihood | Impact | Overall |
|---------|-----------|--------|---------|
| WASM stdout/stderr buffer race | Low (test path only) | Low (corrupted debug output) | Low |
| execEngines dead code | Certain (100% of requests) | High (feature non-functional) | High |
| Watchdog double-dispatch | Low (requires DB slowness + watchdog) | Medium (double concurrency) | Medium |
| Drain TOCTOU | Low-Medium | Low (best-effort drain) | Low |
| Drain notification race | Low | Low (blocks external callers) | Low |
| ShardedStore Close() | Very Low (no dynamic shards) | Low | Very Low |
| Compaction deadlock | Very Low (non-overlapping rows) | Medium (failed compaction) | Very Low |

---

## Decomposition Accuracy

The 4 child tasks in `../cleat-230-race/artifacts/dag.json` remain correctly scoped and independent:

- **fix1** ($5): Fix the WASM stdout/stderr buffer race — per-execution buffers or mutex
- **fix2** ($15): Wire execEngines sync.Map — Store on creation, Delete on cleanup
- **fix3** ($10): Fix drain TOCTOU and notification races — draining check in handleStartWorkflow, re-check after claim, consolidate cancel
- **fix4** ($10): Defensive hardening — lock hygiene, compaction guards, dead code removal

Coupling annotations remain accurate. All 4 tasks can run in parallel (they touch different functions/sections even when sharing files).

---

## Additional Observation: Scope Note

The original TASK.md lists `internal/host/` as in-scope but this directory does not exist. The `internal/` packages present (analyzer, callgraph, closure, plugingen, telemetry, transform) are static analysis and code generation tools with no goroutine spawns or shared mutable state. No concurrency issues found in `internal/`.

---

## Conclusion

All 7 findings from the cleat-230-race exploration report are confirmed against the current codebase (develop HEAD + uncommitted changes). All 6 dead code items are also confirmed. No new race conditions were discovered beyond those already documented. The decomposition at `../cleat-230-race/artifacts/dag.json` into 4 fix tasks remains accurate and ready for dispatch.
