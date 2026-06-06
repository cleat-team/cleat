# Plan Review: cleat-230-race-fix3 (Round 1, deepseek)

**Reviewer:** cleat-230-race-fix3r
**Target:** cleat-230-race-fix3 (plan review)
**Date:** 2026-06-06

## Summary

The three fixes correctly address the drain TOCTOU races identified in the exploration. The approach is minimal and surgical. Two plan-level gaps found: a missing integration test required by CONTRACT.md, and an unspecified context choice for `ReleaseWorkflow` in Fix 2 that could leak claimed workflows.

---

## Plan Review Checklist

### 1. Does the plan satisfy every requirement in CONTRACT.md?

| Deliverable | Covered? |
|---|---|
| `w.draining.Load()` check in `handleStartWorkflow`, return 503 | YES |
| Re-check `w.draining.Load()` after claim in dispatch loop, abort execution | YES |
| Consolidate drain-complete signaling: remove `w.cancel()` from dispatch loop, `handleDrainStatus` calls `w.cancel()` after `close(drainCh)` | YES |

All three CONTRACT deliverables map directly to Fix 1/2/3 in TASK.md.

### 2. Are there missing edge cases the plan doesn't address?

Two edge cases not discussed:

- **Fix 2 context choice**: The plan says "abort execution if drain started" but doesn't specify what context to use when releasing claimed workflows back to the DB. If `w.ctx` is used (the default assumption in the dispatch loop), and `handleDrainStatus` has already cancelled `w.ctx` during the claim window, `ReleaseWorkflow` fails silently. The claimed workflow retains `worker_id = w.id` but the worker is shutting down — it becomes orphaned until another worker's claim timeout. This was caught in implementation review and fixed with `context.Background()`, but the plan should have specified it.

- **Double-drain lifecycle**: After `drainOnce.Do` fires, `drainCh` is permanently closed. If a second `handleDrainStart` call sets `draining = true` again, `handleDrainStatus` can't close the channel a second time and won't call `w.cancel()`. This is harmless in practice (drain is called once per worker lifecycle) but the plan doesn't acknowledge the constraint.

### 3. Will the changes interact badly with any other part of the system?

MEDIUM coupling with `cleat-230-race-fix2` (both touch `cmd/cleat-worker/main.go` dispatch loop area, different functions). No coupling with fix1 or fix4. The 503 response in `handleStartWorkflow` is a new API behavior — callers that don't handle 503 will see it as a transient error, which is the correct semantics for "worker is draining."

### 4. Are the test cases sufficient?

| CONTRACT requirement | Covered? |
|---|---|
| Unit test: post-claim drain in dispatch loop | YES — `TestDispatchLoop_DrainAfterClaim` |
| Unit test: handleStartWorkflow during drain → 503 | YES — `TestAPIStartWorkflow_Draining` |
| Unit test: drainCh closed before context cancelled | YES — `TestDrainStatus_ClosesChannelBeforeCancel` |
| Integration test: full drain lifecycle | **NO** — CONTRACT.md requires this, TASK.md acceptance criteria omit it |

Two additional defensive tests were added that aren't in CONTRACT.md (`TestDrainStatus_DoesNotCloseChannelWhenInflight`, `TestDrainComplete_DoesNotBlock`), which is good.

**Missing coverage**: No test verifies the dispatch loop's inflight==0 return path (line 1240-1241) without also cancelling the context externally. `TestDispatchLoop_DrainAfterClaim` calls `w.cancel()` at test cleanup (line 3445), so the drain-complete return path in the dispatch loop isn't cleanly exercised.

### 5. Complexity assessment

The approach is appropriately simple — three localized changes:
- Fix 1: 1-line check in `handleStartWorkflow`
- Fix 2: 6-line re-check + release block in `dispatchLoop`
- Fix 3: 2-line move of `w.cancel()` from dispatch loop to `handleDrainStatus`

Could Fix 2 be simpler? Making the claim and drain check atomic would require changing the store interface. The re-check-after-claim approach is the right tradeoff.

### 6. Security

No new attack surface. 503 during drain is expected shutdown behavior. No new endpoints, no new external inputs.

---

## Findings

### SHOULD_FIX — Integration test gap between CONTRACT.md and TASK.md

**Severity:** SHOULD_FIX

CONTRACT.md line 35 explicitly requires:
> Integration test: full drain lifecycle (start drain, API rejects, dispatch stops, drain completes)

TASK.md acceptance criteria list five items but omit this requirement. The five unit tests verify individual invariants but don't chain the three fixes together in sequence. An integration test exercising (1) start drain → (2) API rejects with 503 → (3) claimed work released → (4) drainCh closes → (5) context cancelled would catch ordering bugs that unit tests miss.

The CONTRACT should either remove this requirement (if unit tests are deemed sufficient) or TASK.md should include it.

---

### SHOULD_FIX — Plan omits context choice for ReleaseWorkflow in Fix 2

**Severity:** SHOULD_FIX

Fix 2 in TASK.md says: "Re-check `w.draining.Load()` after DB claim in dispatch loop, abort execution if drain started." The plan doesn't specify what context to use when releasing claimed workflows.

If the implementer defaults to `w.ctx` (the context used throughout the dispatch loop), there's a narrow race window: `handleDrainStatus` could cancel `w.ctx` between the DB claim and the post-claim re-check, causing `ReleaseWorkflow` to fail silently. The claimed workflow would retain `worker_id = w.id` but the worker is shutting down — it becomes orphaned.

The correct choice is `context.Background()`, consistent with three other release sites in the same file (lines 1470, 1757, 2255). This was caught during implementation review and fixed, but the plan should have specified it.

---

### NIT — No test covers dispatch loop drain-complete return path

**Severity:** NIT

The dispatch loop's inflight==0 drain-complete path (line 1240-1241, where it logs "drain complete" and returns) is not cleanly tested. `TestDispatchLoop_DrainAfterClaim` calls `w.cancel()` at test cleanup which also causes the loop to exit, so the test doesn't distinguish which code path was taken.

---

### NIT — Sticky claim path not exercised in post-claim drain test

**Severity:** NIT

`TestDispatchLoop_DrainAfterClaim` sets `claimStickyWorkflowsFn` to return nil, so only the general claim path triggers the post-claim re-check. Since both sticky and general results merge into the same `wfs` slice before the re-check, release-path coverage is equivalent. Low value to fix.

---

## Acceptance Criteria Assessment

| Criterion | Plan covers? |
|-----------|-------------|
| 1. `handleStartWorkflow` returns 503 during drain | YES |
| 2. No workflow execution starts after drain is initiated | YES |
| 3. `drainCh` is always closed before root context cancellation | YES |
| 4. `DrainComplete()` never blocks indefinitely | YES |
| 5. Existing drain tests pass | YES |

---

## Findings Count

| Severity | Count |
|----------|-------|
| BLOCKER | 0 |
| SHOULD_FIX | 2 |
| NIT | 2 |

Both SHOULD_FIX items are deferrable: the integration test gap can be addressed by aligning CONTRACT.md and TASK.md (remove the requirement or add the test), and the context choice is a documentation/precision issue that was corrected in implementation.

[OUTCOME:SHOULD_FIX]
