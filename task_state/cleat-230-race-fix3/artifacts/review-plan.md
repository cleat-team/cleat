Plan review complete. Here's the summary:

## cleat-230-race-fix3r — Plan Review Results

**Outcome: [OUTCOME:SHOULD_FIX]** — 0 BLOCKERs, 2 SHOULD_FIX, 2 NITs

### Findings

**SHOULD_FIX — Integration test gap between CONTRACT.md and TASK.md**  
CONTRACT.md requires an integration test for the full drain lifecycle. TASK.md acceptance criteria omit it. The 5 unit tests verify individual invariants but don't chain the three fixes in sequence.

**SHOULD_FIX — Plan omits context choice for ReleaseWorkflow in Fix 2**  
The plan doesn't specify what context to use when releasing claimed workflows in the post-claim abort path. `w.ctx` could be cancelled by `handleDrainStatus`, causing a silent release failure. `context.Background()` is correct (this was caught and fixed during implementation).

**NIT** — No test cleanly covers the dispatch loop's drain-complete return path (line 1240-1241). Test cleanup `w.cancel()` muddles which code path exits the loop.

**NIT** — `TestDispatchLoop_DrainAfterClaim` only exercises the general claim path (sticky mock returns nil). Equivalent coverage since both paths merge into the same `wfs` slice.

### Core fixes are sound

All three fixes in TASK.md correctly address the drain TOCTOU races. The approach is minimal and surgical — exactly right for this bug fix. The ordering guarantee (close drainCh → cancel context, sequentially within `drainOnce.Do`) is correct.

Convergence: 2 deferrable SHOULD_FIX, 0 BLOCKERs. Plan passes.