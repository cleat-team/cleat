# cleat-230-race-fix2k — Convergence: execEngines wiring finalization

**Parent:** cleat-230-race-fix2v (Round 3 review)
**Budget:** $2 (~0.1 engineer-day)
**Priority:** 3 (convergence/finalization)
**Type:** Convergence (review follow-through)

## Task

Address the 2 remaining SHOULD_FIX findings from review-v.md (Round 3 convergence review of cleat-230-race-fix2), and write the convergence STATUS.

### Background

review-v.md (Round 3) identified 2 remaining SHOULD_FIX findings:
1. **S1 (non-deferrable):** Defer ordering at lines 1388-1389 — claimed `execEngines.Delete` fires before `inflight.Delete` creating an orphan update window
2. **S2 (non-deferrable):** `TestExecEngines_MapLifecycle` tests `sync.Map` API directly, not `executeWorkflow` integration

### Investigation

**S1**: The current code at lines 1388-1389 already has the correct ordering:
```go
defer w.execEngines.Delete(wf.ID)  // registered first, fires SECOND
defer w.inflight.Delete(wf.ID)      // registered second, fires FIRST
```
This was fixed in cleat-230-race-fix2i. The review-v.md claim of "still valid" was based on a stale reading. **S1 is RESOLVED.**

**S2**: Add `TestExecuteWorkflow_ExecEnginesLifecycle` that exercises the actual `executeWorkflow` function and verifies the deferred `execEngines.Delete` fires correctly (no stale entries remain, even on error paths before `Store` is reached).

### Acceptance criteria

1. S1 verified as already fixed in current code
2. S2 addressed: new test calls `executeWorkflow` and verifies execEngines cleanup
3. All existing execEngines/dispatch tests pass
4. No regressions

### Out of scope

- Full happy-path executeWorkflow integration test (requires runtime/WASM setup)
- Changes to production code
