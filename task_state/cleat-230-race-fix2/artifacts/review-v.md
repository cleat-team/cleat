# Implementation Review — cleat-230-race-fix2v (Round 3 — Convergence)

**Reviewer:** cleat-230-race-fix2v
**Date:** 2026-06-06
**Verdict:** 0 BLOCKER, 2 SHOULD_FIX (1 non-deferrable, 1 test-quality)

## Prior findings reassessment

The prior review-v.md (Round 2) claimed 4 findings remained unresolved. On re-examination against the current working tree:

| Finding | Prior Status | Current Status |
|---------|-------------|----------------|
| S1: Defer ordering | Claimed unresolved | **Still valid** |
| S2: Test integration gap | Claimed unresolved | **Still valid** (test renamed to `_MapLifecycle`) |
| S3: Missing cleanup verification | Claimed unresolved | **RESOLVED** — test lines 2244-2248 verify Delete/Load cleanup |
| N1: Unchecked type assertion | Claimed unresolved | **RESOLVED** — comma-ok assertion at lines 2141-2144 |

The prior review-v.md incorrectly reported S3 and N1 as unresolved. Both were already addressed in the current code.

---

## Contract compliance

| CONTRACT requirement | Status |
|---|---|
| 1. `w.execEngines.Store(wfID, eng)` after engine creation | Done (line 1667) |
| 2. `w.execEngines.Delete(wfID)` alongside `w.inflight.Delete` | **S1 — defer ordering is wrong** |
| 3. Unit test for Store/Load/Delete lifecycle | Done (`TestExecEngines_MapLifecycle` — but see S2) |
| 4. Integration test for end-to-end update dispatch | Done (`TestDispatchPendingUpdates_WithEngine`) |
| 5. execEngines and inflight stay consistent | **S1 — violated during defer unwind window** |

---

## Findings

### [SHOULD_FIX] S1 (non-deferrable): Defer ordering creates execEngines/inflight inconsistency window

`cmd/cleat-worker/main.go:1388-1389`:

```go
defer w.inflight.Delete(wf.ID)
defer w.execEngines.Delete(wf.ID)
```

Go defers execute in LIFO order. At function exit, `execEngines.Delete` fires **before** `inflight.Delete`. This creates a window where `inflight` still has the workflow ID but `execEngines` does not:

```
T1: executeWorkflow exits, execEngines.Delete(wfID) fires
T2: dispatchPendingUpdates iterates inflight.Range → finds wfID
T3: dispatchPendingUpdates tries execEngines.Load(wfID) → !ok → skip
T4: inflight.Delete(wfID) fires
```

The `!ok` check at line 2136 prevents a crash, but for workflows that are **completing** (not suspending), a skipped update at T3 is permanently orphaned — no worker will ever claim the workflow again.

**Fix:** Swap the two defer lines:

```go
defer w.execEngines.Delete(wf.ID)
defer w.inflight.Delete(wf.ID)
```

With this ordering, `inflight.Delete` fires first. Since `dispatchPendingUpdates` gates on `inflight.Range`, it won't iterate over the wfID, and can't attempt `execEngines.Load`. By the time `execEngines.Delete` fires, no concurrent reader can reach it.

---

### [SHOULD_FIX] S2 (non-deferrable): `TestExecEngines_MapLifecycle` tests sync.Map API, not executeWorkflow integration

`cmd/cleat-worker/worker_daemon_test.go:2151` — The test manually calls `w.execEngines.Store()`, `w.execEngines.Load()`, and `w.execEngines.Delete()`. It exercises the `sync.Map` API, not the actual `executeWorkflow` integration.

The test was renamed from `TestExecEngines_StoreLoadDelete` to `TestExecEngines_MapLifecycle` (addressing the naming NIT), but the fundamental gap remains: if someone removes the `Store` (line 1667) or `Delete` (line 1389) calls from `executeWorkflow`, this test still passes.

**Fix:** Either:
- (preferred) Add a test that calls `executeWorkflow` and asserts execEngines contains the workflow ID during execution and is empty after
- (acceptable) Accept the gap, since `TestDispatchPendingUpdates_WithEngine` verifies the end-to-end Store → Load → dispatch → CompleteUpdateRequest contract, and the gap is only about guarding against accidental line removal

---

## Resolved findings

### S3: Cleanup verification in `TestDispatchPendingUpdates_WithEngine`

Lines 2244-2248 now verify Delete/Load cleanup:
```go
// Verify cleanup: Delete removes engine, Load returns !ok.
w.execEngines.Delete(wfID)
if _, ok := w.execEngines.Load(wfID); ok {
    t.Error("expected engine to be gone after Delete")
}
```

### N1: Type assertion safety

Lines 2141-2144 now use comma-ok:
```go
env, ok := envVal.(*engine.Engine)
if !ok {
    return true
}
```

---

## Test quality assessment

5 test functions for dispatch/engine lifecycle:

| Test | What it covers | Gap |
|---|---|---|
| `TestDispatchPendingUpdates_EmptyInflight` | No-op when inflight is empty | None |
| `TestDispatchPendingUpdates_NoEngine` | Skips when inflight has entry but execEngines doesn't | None |
| `TestExecEngines_MapLifecycle` | Manual Store/Load/Delete + DispatchUpdate | Doesn't test executeWorkflow integration — **S2** |
| `TestDispatchPendingUpdates_WithEngine` | Full dispatch pipeline with engine + Delete cleanup | None |
| (Drain-related tests) | Not part of cleat-230-race-fix2 scope | Cross-task contamination |

---

## Cross-task contamination

The working tree diff for `cmd/cleat-worker/main.go` and `cmd/cleat-worker/worker_daemon_test.go` contains changes from both cleat-230-race-fix2 (execEngines wiring) and cleat-230-race-fix3 (drain race fixes). The drain-related changes (cancel removal, post-claim drain check, handleDrainStatus cancel ordering, handleStartWorkflow drain guard, and 5 drain test functions) are out of scope for this review. This mixing makes it harder to isolate cleat-230-race-fix2 changes for review and merge.

---

## Summary

| Category | Count |
|----------|-------|
| BLOCKER | 0 |
| SHOULD_FIX (non-deferrable) | 2 (S1, S2) |
| SHOULD_FIX (deferrable) | 0 |
| NIT | 0 |

S1 is the one production-code correctness issue — a one-line swap. S2 is a test quality gap that could be deferred if the test rename (`_MapLifecycle`) is considered sufficient documentation of scope.

[OUTCOME:SHOULD_FIX]
