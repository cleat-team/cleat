# Race Condition Audit — Cross-Check Verification (cleat-230-racec)

**Task:** cleat-230-racec
**Type:** Independent re-verification of cleat-230-race exploration findings
**Date:** 2026-06-06
**Method:** Grep, read, and line-by-line verification against current HEAD (develop, including uncommitted changes)

---

## Summary

Independently re-verified all 7 findings and all 6 dead code claims from the cleat-230-race exploration report. All findings confirmed. Line numbers match exploration report with only minor drift on Finding 3 (watchdogLoop at 2765 vs original 2798 — 33 lines of drift due to launchLoop refactoring between exploration and verification windows).

Uncommitted changes (error message formatting in engine.go and main.go) do not affect any findings.

---

## Finding 1: REAL RACE — Unprotected `bytes.Buffer` on shared Runtime

**Status:** CONFIRMED
**Files:** `engine/runtime.go:37-38,190-195`, `engine/backend_wazero.go:46-48`

- `runtime.go:37-38`: `stdout bytes.Buffer`, `stderr bytes.Buffer` on shared `Runtime` struct
- `runtime.go:60-61`: `Stdout()` calls `r.stdout.String()` (read)
- `runtime.go:62-63`: `Stderr()` calls `r.stderr.String()` (read)
- `runtime.go:190-191`: `InstantiateModuleNamed()` calls `r.stdout.Reset()`, `r.stderr.Reset()`
- `runtime.go:194-195`: wazero `WithStdout(&r.stdout)`, `WithStderr(&r.stderr)` — wazero writes concurrently
- `backend_wazero.go:46-48`: `PerExecution()` returns `&wazeroBackend{rt: b.rt}` — **same** `*Runtime` shared across concurrent executions

Three operations compete: Reset (InstantiateModuleNamed), Write (wazero via WithStdout/WithStderr), Read (Stdout/Stderr). `bytes.Buffer` is documented as not goroutine-safe for concurrent writes.

---

## Finding 2: DEAD CODE — `execEngines` sync.Map never written to

**Status:** CONFIRMED
**Files:** `cmd/cleat-worker/main.go:1025,2125`

- `main.go:1025`: `execEngines sync.Map` declared on Worker struct
- `main.go:2125`: Only usage is `w.execEngines.Load(wfID)` in `dispatchPendingUpdates`
- Zero `execEngines.Store()` calls anywhere in Go source (grep across entire repo)
- Zero `execEngines.Delete()` calls anywhere in Go source
- `Load` always returns `!ok`, making `dispatchPendingUpdates` dead code

---

## Finding 3: DESIGN — Watchdog restart double-launch

**Status:** CONFIRMED (33 lines of drift: watchdogLoop at line 2765, was 2798 in exploration)
**File:** `cmd/cleat-worker/main.go:2765,2821-2868`

- `restartLoop` at line 2821:
  - Cancels old per-loop context at line 2847 (`prev.cancel()`)
  - Waits for old goroutine's done channel with 5-second timeout (lines 2848-2853)
  - **After timeout, launches new goroutine anyway** (lines 2857-2868)
- If old dispatch loop is stuck on slow DB call, new loop starts while old one still runs
- Both will claim workflows from DB, execute concurrently, double concurrency limit

Line drift note: watchdogLoop definition moved from line 2798 to 2765 due to launchLoop refactoring (health tracker initialization was moved earlier). The restartLoop function is unchanged.

---

## Finding 4: DESIGN — Drain TOCTOU races

**Status:** CONFIRMED
**File:** `cmd/cleat-worker/main.go:1273-1312,3301-3408`

**4a. Claim race after drain check:**
- Line 1273: `w.draining.Load()` checked before claiming
- Lines 1285/1312: `ClaimStickyWorkflows` / `ClaimWorkflows` DB calls
- Between check and claim, `handleDrainStart` can set `draining = true`
- Result: one extra workflow claimed and executed after drain begins

**4b. API bypasses drain:**
- `handleStartWorkflow` at line 3301 checks `memoryController.CanAcceptAPIWorkflows()` only
- Never checks `w.draining.Load()`
- Workflows started via API during drain persist in DB as "ready" but never claimed

---

## Finding 5: DESIGN — Drain-complete notification race

**Status:** CONFIRMED
**Files:** `cmd/cleat-worker/main.go:1241,3173-3174`

- Two independent drain-complete detection paths:
  - `dispatchLoop` line 1241: calls `w.cancel()` when draining + inflight==0
  - `handleDrainStatus` lines 3173-3174: closes `drainCh` via `drainOnce.Do` when draining + inflight==0
- Race: if dispatch loop fires `w.cancel()` first, root context cancels, HTTP server shuts down, `handleDrainStatus` may be killed mid-response, `drainCh` never closed
- External callers waiting on `DrainComplete()` block forever

Additional detail: `drainCh` declared at line 1053, `drainOnce` at line 1054, both initialized at line 735.

---

## Finding 6: LATENT — ShardedStore.Close() without lock

**Status:** CONFIRMED
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

Compare with `Shards()` at lines 83-89 which properly holds `s.mu.RLock()`. Safe today because shards are immutable after construction, but fragile.

---

## Finding 7: LATENT — Compaction lock ordering vs execution

**Status:** CONFIRMED
**Files:** `engine/db.go:2848-2872`, `engine/compaction.go:189-223`

Lock acquisition order:
- Compaction: `DELETE FROM event_history` (line 2856) → `UPDATE workflow_instances` (line 2862)
- Workflow execution: `UPDATE workflow_instances` (claim) → `INSERT INTO event_history` (record event)

Opposite lock acquisition orders can cause deadlock (Postgres error 40P01). No retry logic in compaction path. Extremely unlikely in practice because compaction operates on old steps well behind the write head, and the DELETE predicate `step < keepStep` excludes concurrently inserted rows.

---

## Dead Code Verification

| Item | Location | Status | Evidence |
|------|----------|--------|----------|
| `completeMu`/`completeResult`/`completeErr` | `engine/runtime.go:54-56` | CONFIRMED | Zero `completeMu.` accesses in engine/ |
| `signals` map | `engine/engine.go:1671` | CONFIRMED | Zero `.signals` accesses in engine.go |
| `QueryHandlers()` | `engine/engine.go:4087` | CONFIRMED | Zero callers in entire Go source |
| `executeViaDispatcher` | `engine/backend_wasmtime.go:335` | CONFIRMED | Zero callers in entire Go source |
| `loadAllEventsForCompaction` | `engine/compaction.go:504` | CONFIRMED | Zero callers in entire Go source |
| `workEntryPoint`/`workInput` on Runtime | `engine/runtime.go:46-47` | CONFIRMED | Only writes at lines 483-484; zero reads anywhere. wasmtimeBackend has separate identically-named fields that ARE used. |

---

## Already-Safe Areas (confirmed)

Verified and confirmed safe — mutexes/channels/atomics used correctly:
- `execSession.mu` protecting stateStore, queryState, deferrals — all accesses hold mutex
- `wasmtimeBackend.PerExecution()` — creates fresh struct, no shared state
- `wasmLRUCache` / `WASMCache` — all methods lock `mu`
- `wazeroInitOnce` (sync.Once) — correct usage
- `scheduleMu` — correct defense against restart race
- `parentWakeCh` buffering — correct wake-up hint pattern
- PostgresStore/MySQLStore/MSSQLStore — no mutable shared state after construction
- `decryptAndRedactEventRecord` — read-only on store, per-call cipher state
- StreamEventHistory goroutine — local state, channels, *sql.DB goroutine-safety
- freshAwaitAllChildren goroutines — unique index per goroutine

---

## Uncommitted Changes Impact Assessment

Uncommitted changes in `engine/engine.go` and `cmd/cleat-worker/main.go` are error message formatting only:
- `main.go:1466,1504`: Added workflow ID prefix to FailWorkflow error messages
- `engine.go:1118,1120,1316,1318`: Added "host: workflow execution failed:" prefix to WASM trap errors, changed `%s` to `%w` for error wrapping

**No impact on any finding.** All line numbers, types, and concurrency patterns are unaffected.

---

## Risk Assessment (unchanged from original)

| Finding | Likelihood | Impact | Overall |
|---------|-----------|--------|---------|
| WASM stdout/stderr buffer race | Low (test path) | Low (debug output) | Low |
| execEngines dead code | Certain | High (feature broken) | High |
| Watchdog double-dispatch | Low | Medium | Medium |
| Drain TOCTOU | Low-Medium | Low | Low |
| Drain notification race | Low | Low | Low |
| ShardedStore Close() | Very Low | Low | Very Low |
| Compaction deadlock | Very Low | Medium | Very Low |

---

## Conclusion

All 7 findings from the cleat-230-race exploration report are confirmed against the current codebase (HEAD + uncommitted changes). All 6 dead code items are also confirmed. The decomposition at `../cleat-230-race/artifacts/dag.json` into 4 fix tasks (fix1–fix4) correctly scopes the fixes and remains accurate.
