# Race Condition Audit — Exploration Report

**Task:** cleat-230-racee
**Scope:** engine/, cmd/cleat-worker/, internal/host/ — all production Go code
**Excluded:** tests, benchmarks, examples, plugin implementations

---

## Summary

Audited **4 layers** of the engine and worker for concurrent access issues:
1. Engine execution session (`engine/engine.go` — execSession)
2. Worker daemon (`cmd/cleat-worker/main.go` — Worker struct)
3. WASM runtime (`engine/runtime.go`, `engine/backend_wasmtime.go`)
4. Store/database layer (`engine/db.go`, `engine/mysql_store.go`, `engine/mssql_store.go`)

Found **1 real race condition**, **2 dead-code correctness bugs**, and **4 design-level concurrency issues**.

---

## Finding 1: REAL RACE — Unprotected `bytes.Buffer` on shared Runtime

**Severity:** Medium-High
**File:** `engine/runtime.go` lines 37-38, 60-63, 190-195
**Type:** Actual data race (detectable by `go test -race`)

The `Runtime` struct has two `bytes.Buffer` fields:
```go
stdout bytes.Buffer  // line 37
stderr bytes.Buffer  // line 38
```

Three operations compete on these buffers from different goroutines:
1. **Reset** — `InstantiateModuleNamed` at lines 190-191 calls `r.stdout.Reset()` and `r.stderr.Reset()`
2. **Write** — wazero writes WASM stdout/stderr into these buffers during `fn.Call()` (via `WithStdout`/`WithStderr` at lines 194-195)
3. **Read** — `Stdout()`/`Stderr()` at lines 60-63 call `r.stdout.String()` / `r.stderr.String()`

`bytes.Buffer` is explicitly documented as **not goroutine-safe** for concurrent writes.

**Why this races:** `wazeroBackend.PerExecution()` at `engine/backend_wazero.go:46` returns `&wazeroBackend{rt: b.rt}` — the **same** `*Runtime` is shared across all concurrent executions. Two goroutines executing different WASM modules will:
- Goroutine A calls `r.stdout.Reset()` while Goroutine B's wazero runtime is writing to `r.stdout`
- This corrupts the internal buffer state (offset/length mismatch), possibly causing panics or garbage output

**Impact in production:** `Stdout()`/`Stderr()` are only called from test code (unit_test.go:669-673). Writes happen on every WASM execution. `Reset()` happens before each WASM instantiation. The race is a read-write-reset tricycle with `Reset()` and wazero writes competing. Even without `Stdout()` calls, concurrent `Reset()` + write can corrupt the buffer.

**Fix:** Either make `PerExecution()` create independent stdout/stderr buffers (like the wasmtime backend does), or protect the buffers with a mutex.

---

## Finding 2: DEAD CODE BUG — `execEngines` sync.Map never written to

**Severity:** High (feature is silently broken)
**File:** `cmd/cleat-worker/main.go` lines 1025, 2125
**Type:** Missing implementation, not a race

The Worker struct declares:
```go
execEngines sync.Map // map[workflowID]*engine.Engine
```

But there is **zero** `execEngines.Store(...)` calls anywhere in the codebase. The only usage is a `Load` at line 2125 in `dispatchPendingUpdates()`:
```go
envVal, ok := w.execEngines.Load(wfID)
```

Since nothing ever stores into this map, `Load` always returns `!ok`, and update dispatch always skips. The entire `updateDispatchLoop` / `dispatchPendingUpdates` system is dead code. Updates queued via `POST /api/workflows/:id/updates` are persisted to the DB but **never dispatched to running workflow engines**.

**Fix:** Add `w.execEngines.Store(wf.ID, eng)` after engine creation (after line 1657 in executeWorkflow) and `w.execEngines.Delete(wf.ID)` alongside `w.inflight.Delete` at line 1380.

---

## Finding 3: DESIGN ISSUE — Watchdog restart can double-launch dispatch loops

**Severity:** Medium
**File:** `cmd/cleat-worker/main.go` lines 2798, 2821-2868
**Type:** Concurrency design flaw

The watchdog loop monitors background goroutine health. When a loop (dispatch, heartbeat, reaper, etc.) becomes stale, `restartLoop()` at line 2821:
1. Cancels the old per-loop context (line 2847)
2. Waits for the old goroutine's done channel with a **5-second timeout** (lines 2848-2853)
3. If timeout fires, launches a **new** goroutine anyway (lines 2857-2868)

If the old dispatch loop is stuck on a slow DB call or blocked in its idle sleep, the new loop starts while the old one is still running. Both will:
- Claim workflows from the DB
- Execute workflows in parallel (double the configured concurrency)
- Both add to `w.inflight` and `w.wg`

This affects ALL per-loop goroutines (heartbeat, reaper, schedule, dispatch, etc.) but is most consequential for dispatch.

**Fix:** Shorter timeout (5s is generous), or ensure the old goroutine cannot block indefinitely (use cancellable contexts for all long-running operations), or add a generation counter so the old loop detects it's been replaced.

---

## Finding 4: DESIGN ISSUE — Drain TOCTOU races

**Severity:** Low-Medium
**File:** `cmd/cleat-worker/main.go` lines 1273-1312, 3301-3408

Two related races in the drain mechanism:

**4a. New work claimed after drain starts:** The dispatch loop checks `w.draining.Load()` at line 1273, then claims work from DB at lines 1285/1312. Between the check and claim, `handleDrainStart` can set `draining = true`. Result: one extra workflow is claimed and executed after drain begins.

**4b. API workflows bypass drain entirely:** `handleStartWorkflow` (line 3301) checks `memoryController.CanAcceptAPIWorkflows()` but **never** checks `w.draining.Load()`. A workflow started via the API during drain is persisted as "ready" in the DB but never claimed (dispatch loop stops claiming during drain). In a single-worker deployment, this workflow is abandoned when the worker shuts down.

**Fix:** Check `w.draining.Load()` in `handleStartWorkflow` and return 503 during drain. For the dispatch loop race, re-check draining after the DB claim and abort execution if drain has started.

---

## Finding 5: DESIGN ISSUE — Drain-complete notification race

**Severity:** Low
**File:** `cmd/cleat-worker/main.go` lines 1241, 3173-3174
**Type:** Notification ordering bug

Two independent paths detect drain completion:
- `dispatchLoop` at line 1238-1241: counts inflight, if 0 calls `w.cancel()`
- `handleDrainStatus` at lines 3162-3175: counts inflight, if 0 closes `drainCh` via `drainOnce.Do`

If the dispatch loop fires `w.cancel()` first, the root context is cancelled. The HTTP server shuts down, and `handleDrainStatus` may be killed mid-response. The `drainCh` is never closed, so any external caller waiting on `DrainComplete()` blocks forever.

**Fix:** Have `handleDrainStatus` call `w.cancel()` itself after closing `drainCh`, and remove the `w.cancel()` from dispatchLoop's drain-detection path.

---

## Finding 6: LATENT RACE — ShardedStore.Close() without lock

**Severity:** Low (safe today, fragile for future)
**File:** `engine/sharded_store.go` line 75

```go
func (s *ShardedStore) Close() {
    for _, shard := range s.shards {  // no mu.RLock()
```

All other accesses to `s.shards` hold `mu.RLock()`. `Close()` does not. Safe today because shards are immutable after construction, but would race if dynamic shard reconfiguration is ever added.

**Fix:** Add `s.mu.RLock()` / `s.mu.RUnlock()` around the loop in `Close()`.

---

## Finding 7: LATENT — Compaction deadlock potential

**Severity:** Low
**File:** `engine/compaction.go` lines 189-223, `engine/db.go` lines 2848-2872
**Type:** Database lock ordering

Compaction and workflow execution acquire row locks in opposite orders:
- Compaction: `event_history` (DELETE) → `workflow_instances` (UPDATE)
- Workflow execution: `workflow_instances` (UPDATE) → `event_history` (INSERT)

Postgres would detect a true deadlock and abort one transaction with error 40P01. The compaction path has no retry logic. In practice, compaction runs on old events (steps well behind the write head), and the DELETE predicate `step < keepStep` excludes rows concurrently being inserted, making an actual deadlock extremely unlikely.

**Fix:** Add a simple retry loop (3 attempts with exponential backoff) around `CompactHistory`. Also add a `generation = ?` guard on the UPDATE for consistency with other workflow operations.

---

## Findings Already Ruled Out (no race condition)

| Area | Verdict |
|------|---------|
| execSession.mu protecting stateStore | Fully protected — every access holds the mutex |
| execSession.mu for queryState writes | Protected (SetQueryState locks) |
| execSession.mu for deferrals writes | Protected (DurableDefer locks) |
| WASM handler field (wasmtimeBackend) | Safe — PerExecution() creates fresh struct |
| wasmLRUCache | Safe — all methods lock mu |
| WASMCache | Safe — all methods lock mu |
| wazeroInitOnce (sync.Once) | Safe — correct usage |
| scheduleMu | Safe — correct defense against restart race |
| parentWakeCh buffering | Safe — correct wake-up hint pattern |
| PostgresStore/MySQLStore/MSSQLStore | Safe — no mutable shared state after construction |
| decryptAndRedactEventRecord | Safe — read-only on store, per-call cipher state |
| StreamEventHistory goroutine | Safe — local state, channels, *sql.DB goroutine-safety |
| freshAwaitAllChildren goroutines | Safe — unique index per goroutine |

---

## Dead Code Found

| Location | Description |
|----------|-------------|
| `engine/runtime.go:54-56` | `completeMu`, `completeResult`, `completeErr` — never accessed; context-based mechanism used instead |
| `engine/engine.go:1671` | `signals` map — never read or written in production code |
| `engine/engine.go:4087` | `QueryHandlers()` method — never called |
| `engine/backend_wasmtime.go:335-409` | `executeViaDispatcher` — defined but never called |
| `engine/compaction.go:504` | `loadAllEventsForCompaction` — defined but never called |
| `engine/engine.go:46-47` | `workEntryPoint`/`workInput` on Runtime — written but never read (wazero path uses a no-op stub) |

---

## What's Already Good

The engine has strong concurrency discipline in several areas:

- **Per-execution isolation** — wasmtimeBackend.PerExecution() creates fresh structs
- **sync.Mutex on shared maps** — stateStore is consistently protected
- **sync.Map for inflight tracking** — correct for concurrent insert/delete/iterate
- **drainOnce for one-shot drain** — prevents double-close
- **sync.Once for one-time init** — wazero warmup
- **SELECT ... FOR UPDATE SKIP LOCKED** — DB-level concurrency for workflow claiming
- **Non-blocking channel sends** — parentWakeCh uses select/default

---

## Risk Assessment

| Risk | Likelihood | Impact | Overall |
|------|-----------|--------|---------|
| WASM stdout/stderr buffer race | Low (only manifests under test) | Low (corrupted debug output) | Low |
| execEngines dead code (updates broken) | Certain (100% of requests) | High (feature non-functional) | High |
| Watchdog double-dispatch | Low (requires DB slowness + watchdog) | Medium (double concurrency) | Medium |
| Drain TOCTOU | Low-Medium | Low (best-effort drain) | Low |
| Drain notification race | Low | Low (blocks external callers) | Low |
| ShardedStore Close() | Very Low (no dynamic shards) | Low | Very Low |
| Compaction deadlock | Very Low (non-overlapping rows) | Medium (failed compaction) | Very Low |
