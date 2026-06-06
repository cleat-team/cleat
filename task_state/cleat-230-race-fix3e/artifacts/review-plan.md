# Plan Review — cleat-230-race-fix3e

## Review round 1

### Contract coverage

| Invariant | Covered by | Status |
|-----------|-----------|--------|
| No new workflow execution starts after drain initiated | Fix 1 (API reject) + Fix 2 (post-claim release) | PASS |
| drainCh always closed before root context cancelled | Fix 3 (close then cancel inside drainOnce.Do) | PASS |
| DrainComplete() never blocks | Fix 3 (drainCh closed before cancel) | PASS |
| drainOnce.Do fires exactly once per drain cycle | Fix 3 (sync.Once guarantees single execution) | PASS |
| Dispatch loop inflight==0 doesn't cancel context | Fix 3a (loop returns without w.cancel()) | PASS |

### Edge cases considered

1. **Drain starts between sticky claim and general claim**: Fix 2 re-checks after both claims are combined into `wfs` slice. Covered.
2. **Drain starts between claim and executeWorkflow**: Fix 2 re-checks after DB claim but before `go w.executeWorkflow(wf)`. Covered.
3. **handleDrainStatus called multiple times before drain complete**: `drainOnce.Do` ensures `close(drainCh)` and `w.cancel()` execute exactly once even if status endpoint is polled repeatedly. Covered.
4. **Context cancelled before drainCh closed**: Impossible now — both are inside the same `drainOnce.Do` with close before cancel. Covered.

### Findings

- [NIT] No integration test for the full drain lifecycle (start drain → API rejects → dispatch stops → inflight drains → drain completes). Unit tests cover each invariant individually. Deferrable.

### Verdict: CONVERGED

0 BLOCKER, 0 SHOULD_FIX, 1 deferrable NIT. Plan is correct as-is.
