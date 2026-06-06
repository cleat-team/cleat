# cto-lap-033 Exploration

**Date:** 2026-06-06
**Explorer:** cto-lap-033

## 1. What's here now?

cto-lap-033 is a shell task auto-created by the CTO lap workflow for the next
CTO lap (lap 91). The task's purpose is to coordinate the CTO protocol: survey,
triage, cross-check, dispatch decisions, and CEO brief writing.

### Active tasks

| Task | Phase | Budget | Spent | Verdict |
|------|-------|--------|-------|---------|
| cleat-228b | complete (verified) | $20 | — | **CLOSE** — all 4 deliverables verified by cleat-228bm |
| cleat-230-logs | done | $5 | $2 | **CLOSE** — exploration complete, hand off to implementation |
| cleat-230-errors | implementing | $6.67 | $3 | **CHECK** — 3 fixes made, may be nearly done |
| cleat-230-race | decomposed | $10 | $10 | **DISPATCH** — dag.json ready with 4 children |
| cleat-230-race-fix1 | shell | $5 | $0 | Needs task files created |
| cleat-230-race-fix2 | shell | $15 | $0 | Needs task files created |
| cleat-230-race-fix3 | shell | $10 | $0 | Needs task files created |
| cleat-230-race-fix4 | shell | $10 | $3 | Needs task files created |

### Abandoned task shells

6 directories in task_state/ have no STATUS.md:
- cleat-232, cleat-232-tenant-store
- cleat-233, cleat-233b, cleat-233d
- cleat-234b, cleat-234d

These appear to be from a prior decomposition cycle that was never completed.
No task files exist. They should be cleaned up.

### CEO Guidance

2026-05-25 CEO guidance ($100) still governs:
- Item 1 (WASM debugger CLI, $80) → cleat-228b — **COMPLETE**
- Item 2 (Engine reliability polish, $20) → cleat-230 — **IN PROGRESS** (3 subtasks, $15 dispatched)
- Total spent: ~$20 of $100

Both items have been acted on. No new CEO guidance since May 25.

### ARCHITECTURE.md

Present at repo root (96 lines). Current — no module boundary changes detected.
But the race audit surfaced patterns (per-execution isolation, generation counters,
drain lifecycle) that warrant documentation in the Patterns section.

### Git State

```
fbaf750 Revert "Auto-implemented changes from clew pipeline laps"
14dec5e Auto-implemented changes from clew pipeline laps
010c2ed fix: pass WASM memory buffer to AwaitSignals handler in wasmtime backend
```

The most recent commit is a REVERT of an auto-implemented change. This happened
between lap 89 (May 25) and lap 90 (June 6). The pipeline auto-implemented
something incorrect and it was immediately reverted. This is a pipeline quality
concern that the lap 90 CEO brief flagged for investigation.

### Budget Status

| Item | Budget | Spent | Remaining |
|------|--------|-------|-----------|
| WASM debugger CLI (228b) | $80 | $20 | $60 |
| Engine reliability polish (230) | $20 | $15 | $5 |
| **Total** | **$100** | **$35** | **$65** |

Reliability polish breakdown:
- cleat-230-logs: $2 spent (budget $5)
- cleat-230-errors: $3 spent (budget $6.67)
- cleat-230-race exploration: $10 spent (budget $10)

## 2. What needs to change?

### High priority

1. **Close cleat-228b.** All 4 deliverables verified. `go test ./cmd/cleatctl/` passes
   with 35 debug tests. This is the last 2026-05-22 item — closing it completes the
   prior CEO guidance cycle.

2. **Dispatch cleat-230-race decomposition.** dag.json is ready at
   `task_state/cleat-230-race/artifacts/dag.json` with 4 independent child tasks.
   This should be the primary new work for this lap. Add to `decomps_ready` in
   survey-output.json.

3. **Create TASK.md/CONTRACT.md/STATUS.md for the 4 race-fix child tasks.**
   The dag.json defines contracts but the individual task files don't exist yet.
   The clew-decompose workflow should create these automatically when dispatched.

4. **Close cleat-230-logs.** Exploration done, report written. The log cleanup
   implementation should be a new leaf task (or re-scope cleat-230-logs as
   implementing).

5. **Check cleat-230-errors.** 3 fixes made. If complete, close it. If not,
   escalate — it should have been done in 1 session.

### Medium priority

6. **Investigate the auto-implement revert (fbaf750).** The lap 90 CEO brief flagged
   this as a pipeline quality concern. Root-cause: was the auto-implementation wrong,
   or was the revert premature?

7. **Create lessons_learned/ directory.** The lap 90 brief notes this doesn't exist.
   The race audit and the auto-implement revert are candidates for first entries.

8. **Clean up abandoned task shells.** 6 directories with no task files should be
   removed to avoid confusion.

### Low priority

9. **Update ARCHITECTURE.md.** Add patterns from the race audit: per-execution
   isolation, generation counters for watchdog, drain lifecycle invariants.

10. **Budget reconciliation.** The CEO guidance allocated $80 for cleat-228b but
    only $20 was spent. The remaining $60 is available. The reliability polish
    budget ($20) is nearly fully allocated ($15 spent + $40 for race-fix children).

## 3. What are the risks?

- **Budget overrun on cleat-230-race children.** The 4 race-fix tasks total $40
  budget, but only $5 remains in the declared reliability polish budget. This is
  a re-scoping decision the CTO needs to make — either expand the budget or trim
  the scope.

- **cleat-228b regression risk (Low).** The implementation is verified and tests
  pass. The only risk is if the engine API (ReplayStepCallback) changes in a
  future refactor, but that's normal maintenance.

- **Race fix coupling (Low-Medium).** fix2 and fix3 are MEDIUM coupled (both touch
  cmd/cleat-worker/main.go). The dag.json correctly marks this. They should not
  be dispatched in parallel.

- **Abandoned task shells are confusing.** 6 empty directories in task_state/
  with no task files. Agents traversing the directory may waste time checking them.

- **Pipeline auto-implement quality.** The revert pattern suggests the pipeline
  produced incorrect code. This could recur and waste budget on future laps.

- **No lessons_learned/ directory.** Without it, cross-check can't surface findings
  from prior work. The lap 90 brief explicitly called this out.

## 4. What's the complexity?

**Leaf-ready for the CTO agent.** This is a coordination/decision task, not an
implementation task. The CTO protocol is well-established (cto-agent.md). The
survey output format is standardized. The CEO brief format has prior examples.

No decomposition needed. The task is: survey, decide, write survey-output.json,
write CEO brief. All within a single CTO agent session.

The cleat-230-race dag.json is the only decomposition to dispatch — the
clew-decompose workflow handles the mechanical work of creating child task files
and spawning clew-leaf-task children.

## 5. State of the cleat project (as of this exploration)

- **CEO Guidance (May 25, $100)**: Both items acted on. Item 1 (debugger) COMPLETE.
  Item 2 (reliability polish) IN PROGRESS. No new guidance received in 12 days.
- **Active tasks**: 4 (228b closing, 230-logs closing, 230-errors checking,
  230-race dispatching). 4 children queued.
- **Git activity**: 12 commits since May 25. Mostly pipeline reliability fixes,
  WASM backend fixes, and the reverted auto-implementation.
- **ARCHITECTURE.md**: Present and current (96 lines). Coupling matrix accurate.
  Patterns section could be expanded with race audit findings.
- **Budget**: $65 remaining of $100. Reliability polish sub-budget nearly exhausted
  ($5 remaining) but race children total $40 — over budget without re-scoping.
- **Known issues**: Auto-implement revert, abandoned task shells, missing
  lessons_learned/, budget mismatch on race decomposition.

## 6. Recommendation

1. **Dispatch cto-lap-033 as the CTO agent** (not an explorer agent). The
   exploration is complete. The task is running the CTO protocol — survey, triage,
   cross-check, dispatch decisions, CEO brief. This needs the CTO agent prompt
   (`prompts/cto-agent.md`), not the explorer pattern.

2. **In survey-output.json:**
   - `decomps_ready`: cleat-230-race (dag.json at `task_state/cleat-230-race/artifacts/dag.json`, no dependencies)
   - `tasks_to_close`: cleat-228b, cleat-230-logs
   - Address the budget overrun — either expand reliability polish budget or trim race children

3. **Before dispatching race children:** Create the 4 child task directories with
   TASK.md/CONTRACT.md/STATUS.md, or let the clew-decompose workflow do it.

4. **Clean up:** Remove the 6 abandoned task shell directories.
