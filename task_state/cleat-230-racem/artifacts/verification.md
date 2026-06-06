# Race Condition Audit — Verification Report (cleat-230-racem)

**Task:** cleat-230-racem (5th independent verification)
**Verifier:** Explorer agent
**Date:** 2026-06-06
**Base:** develop HEAD (`fbaf750`) + uncommitted error message formatting

---

## Methodology

Each finding was independently verified by reading the actual source files at the cited locations and confirming the described behavior. Dead code was verified via exhaustive grep across all Go source files.

---

## Finding 1: REAL RACE — Unprotected `bytes.Buffer` on shared Runtime

**Status:** CONFIRMED

- `engine/runtime.go:37-38`: `stdout bytes.Buffer`, `stderr bytes.Buffer`
- `engine/runtime.go:60-63`: `Stdout()`/`Stderr()` call `r.stdout.String()` / `r.stderr.String()` — direct read
- `engine/runtime.go:190-195`: `InstantiateModuleNamed` calls `r.stdout.Reset()` and `r.stderr.Reset()` — direct write
- `engine/backend_wazero.go:46-48`: `PerExecution()` returns `&wazeroBackend{rt: b.rt}` — same `*Runtime` reference
- `bytes.Buffer` is documented as not goroutine-safe

---

## Finding 2: DEAD CODE — `execEngines` sync.Map never written to

**Status:** CONFIRMED

- `cmd/cleat-worker/main.go:1025`: `execEngines sync.Map` declared on Worker struct
- `cmd/cleat-worker/main.go:2125`: Only usage is `w.execEngines.Load(wfID)` in `dispatchPendingUpdates`
- Zero `execEngines.Store()` calls anywhere in Go source (confirmed via grep across entire repo)
- Zero `execEngines.Delete()` calls anywhere in Go source

---

## Finding 3: DESIGN — Watchdog restart double-launch

**Status:** CONFIRMED (33 lines drift)

- `watchdogLoop` at `cmd/cleat-worker/main.go:2765` (original report: 2798)
- `restartLoop` at line 2821: cancels old context, waits 5s (line 2848-2853), launches new goroutine regardless (line 2864-2868)
- If old loop is stuck, both run concurrently

---

## Finding 4: DESIGN — Drain TOCTOU races

**Status:** CONFIRMED

- **4a**: `w.draining.Load()` at line 1273, then `ClaimStickyWorkflows` at line 1285 and `ClaimWorkflows` at line 1312 — no re-check after claim
- **4b**: `handleStartWorkflow` at line 3301 checks `CanAcceptAPIWorkflows()` but never `w.draining.Load()` — no draining guard

---

## Finding 5: DESIGN — Drain notification ordering race

**Status:** CONFIRMED

- `dispatchLoop` line 1241: `w.cancel()` when drain + inflight==0
- `handleDrainStatus` lines 3172-3174: `drainOnce.Do(close(drainCh))` when draining && count==0
- If `w.cancel()` fires first, `drainCh` is never closed

---

## Finding 6: LATENT — `ShardedStore.Close()` without lock

**Status:** CONFIRMED

- `engine/sharded_store.go:74-80`: `Close()` iterates `s.shards` without `mu.RLock()`
- `engine/sharded_store.go:83-89`: `Shards()` correctly holds `mu.RLock()`

---

## Finding 7: LATENT — Compaction deadlock potential

**Status:** CONFIRMED

- `engine/db.go:2848-2872`: `CompactHistory` — DELETE from `event_history` (line 2856), then UPDATE `workflow_instances` (line 2863)
- Workflow execution UPDATEs `workflow_instances` first, then INSERTs into `event_history` — opposite order
- Unlikely in practice (non-overlapping row ranges), but locks are acquired in reverse order

---

## Dead Code Verification

| Item | Location | Verified |
|------|----------|----------|
| `completeMu`/`completeResult`/`completeErr` | `engine/runtime.go:54-56` | Zero `completeMu.` accesses in entire codebase. Fields only declared, never used. |
| `signals` map | `engine/engine.go:1671` | Zero `.signals` accesses in production code. Test-only. |
| `QueryHandlers()` | `engine/engine.go:4087` | Zero callers (`\.QueryHandlers\(\)` matches nothing) |
| `executeViaDispatcher` | `engine/backend_wasmtime.go:335` | Only definition, zero callers |
| `loadAllEventsForCompaction` | `engine/compaction.go:504` | Only definition + benchmark reference, zero production callers |
| `workEntryPoint`/`workInput` | `engine/runtime.go:46-47` | Only written (lines 483-484), never read from Runtime struct |

---

## Uncommitted Changes Impact

Changes in working tree:
- `engine/dwarf_trap.go`: "WASM trap:" → "wasm trap:" prefix change
- `engine/engine.go`: Added "host: workflow execution failed:" error wrapping
- `cmd/cleat-worker/main.go`: Added workflow ID to error messages

**Impact on findings:** None. All error message formatting only.

---

## Risk Assessment (confirmed)

| Risk | Likelihood | Impact | Overall |
|------|-----------|--------|---------|
| WASM stdout/stderr buffer race | Low | Low | Low |
| execEngines dead code | Certain | High | High |
| Watchdog double-dispatch | Low | Medium | Medium |
| Drain TOCTOU | Low-Medium | Low | Low |
| Drain notification race | Low | Low | Low |
| ShardedStore Close() | Very Low | Very Low | Very Low |
| Compaction deadlock | Very Low | Medium | Very Low |

---

## Cross-Reference

All 5 independent verifications (racer, racev, racec, racek, racem) concur on all findings and all dead code. The decomposition into 4 child tasks (fix1-fix4) correctly scopes the work.
