# Exploration: cto-lap-032 — cleat 0.5 Trial Release Hardening Survey

**Explorer:** cto-lap-032 (claude explorer agent)
**Date:** 2026-06-05 (original), re-surveyed 2026-06-05
**CEO Guidance:** `task_state/CEO-GUIDANCE.md` (2026-06-04, $100 budget, 6 items)
**Prior surveys:** Lap 90, cto-lap-017, cto-lap-019, cto-lap-021 (2026-06-04), cto-lap-survey (2026-06-05), cto-lap-030-001s (2026-06-05), cto-lap-031 (2026-06-05 original + re-survey)

---

## RE-SURVEY (2026-06-05): Commit `98e32dd` Resolves Uncommitted Engine Changes

### What Changed

The primary finding of the original cto-lap-032 survey was **uncommitted engine hot-path changes** in `engine/engine.go` and `engine/db.go`. These have been committed as `98e32dd`.

### New HEAD: `98e32dd` (was `8d9b6f6`)

```
98e32dd fix: replay AwaitSignals checks signal store and uses cast for jsonb result
  engine/engine.go (+34/-7): signal store fallback in DurableAwaitSignals replay
                              path + workflowID fix in PollSignal/PollCancellation
  engine/db.go     (+2/-1):  ::jsonb cast in FinalizeWorkflowSegment
```

This commit contains exactly the changes the original survey detected as uncommitted:
- `DurableAwaitSignals` replay path now checks signal store for signals arriving after suspend
- `PollSignal` and `PollCancellation` use `s.engine.workflowID` instead of `""`
- Signal names parsed as JSON array in replay path (not comma-split)
- `::jsonb` cast added to `result` column UPDATE

### Engine Files Now Clean

`git status engine/engine.go engine/db.go` returns "nothing to commit, working tree clean."

### Impact Assessment

| CEO Item | Original Impact | Re-Survey Impact |
|----------|----------------|-----------------|
| 1. ChildWorkflow API | None | None |
| 2. Multi-DB Fixes | Low (`::jsonb` is PG-only) | Low (committed — now part of review scope, not blocking) |
| 3. SDK Tests | Low-positive (signal fix) | Low-positive (committed fix improves signal test reliability) |
| 4. CI Enforcement | None | None |
| 5. Code Review | Medium (2 hot-path files) | Medium (now reviewing committed code, not uncommitted diffs) |
| 6. Documentation | None | None |

### Remaining Uncommitted Changes

Same cosmetic set as all prior surveys: `auth/middleware.go`, web UI, dist assets, task_state files. No engine hot-path changes.

### Recommendation After Re-Survey

**Dispatch immediately.** The commit barrier has been removed. The engine changes that cto-lap-032 recommended committing before dispatch are now committed. All 6 CEO guidance items remain leaf-ready. This is the 10th survey with zero tasks dispatched.

---

## 0. Material Change Since Last Survey (cto-lap-031) [ORIGINAL SURVEY]

### Git HEAD

HEAD is still at `8d9b6f6` — no new commits since cto-lap-031 re-survey.

### NEW: Uncommitted hot-path changes in engine/

This is the first survey to detect **uncommitted changes to engine hot-path files**. Prior surveys only noted uncommitted changes to `auth/middleware.go`, web UI, dist assets, and task_state files — all cosmetic for the hardening lap. The current uncommitted set includes two engine files:

#### `engine/engine.go` (+33, -7): Signal routing bug fix + replay improvement

1. **`DurableAwaitSignals` replay path (lines ~2447-2475)**: When no `signal_received` event is found in the replay history, the code now checks the signal store (`workflow_signals` table) for signals that arrived after the workflow suspended. Previously it returned "no signal" — which was unreachable in practice but technically incorrect. Now it polls the signal store, records a synthetic `signal_received` event if found, and returns the signal payload. This closes a correctness gap.

2. **`PollSignal` and `PollCancellation` (lines ~2487, ~2576)**: Changed `signalStore.PollSignal(ctx, "", signalName)` to `signalStore.PollSignal(ctx, s.engine.workflowID, signalName)`. The empty string `""` was a bug — it would cause signal store lookups to either fail or conflate signals across workflows. Using the actual `workflowID` enables per-workflow signal isolation.

#### `engine/db.go` (+1, -1): Extra jsonb cast safety

- Added `::jsonb` cast: `result = $3` → `result = $3::jsonb` in the "done" path UPDATE. This is a PostgreSQL-specific extra safety measure on top of the `json.Valid()` guard from commit `8d9b6f6`.

### Impact per CEO Item

| Item | Impact | Detail |
|------|--------|--------|
| 1. ChildWorkflow API | None | No ChildWorkflow files touched |
| 2. Multi-DB Fixes | **Low** | `::jsonb` cast is PostgreSQL-only. MySQL/MSSQL stores need different handling or no cast. The signal store `workflowID` parameter is dialect-agnostic — good. |
| 3. SDK Tests | **Low-positive** | Signal routing fix (empty→workflowID) prevents flaky signal tests in multi-workflow SDK scenarios |
| 4. CI Enforcement | None | No CI files touched |
| 5. Code Review | **Medium** | Two hot-path files with uncommitted bug fixes. The `DurableAwaitSignals` replay path is complex (signal store fallback + synthetic event recording). The `PollSignal`/`PollCancellation` workflowID fix is straightforward but critical for correctness. Both should be committed before dispatch. |
| 6. Documentation | None | No doc implications |

### Uncommitted Changes Summary

| File | Change | Severity | Prior Surveys Had This? |
|------|--------|----------|------------------------|
| `engine/engine.go` | Signal routing fix + replay improvement | **Medium** | No — NEW |
| `engine/db.go` | `::jsonb` cast | Low | No — NEW |
| `auth/middleware.go` | SPA asset path bypass | Low | Yes (unchanged) |
| `web/src/*` | Admin dashboard Svelte 5 updates | None | Yes (unchanged) |
| `cmd/cleat-worker/web/dist/*` | Rebuilt dist assets | None | Yes (unchanged) |
| `task_state/*` | CTO process files | None | Yes (active) |

---

## 1. Material Change Since CEO Guidance (Cumulative)

Cumulative material changes since CEO guidance was issued (2026-06-04):

| Change | When Detected | Impact |
|--------|--------------|--------|
| Commit `1b7f8ed`: WASM input dispatch fix | cto-lap-survey | Items 3, 5 |
| Commit `8d9b6f6`: jsonb validation + bytea cast fix | cto-lap-031 re-survey | Items 2, 3, 5 |
| Uncommitted `engine/engine.go`: signal routing fix | **cto-lap-032** | Item 5 |
| Uncommitted `engine/db.go`: `::jsonb` cast | **cto-lap-032** | Item 2, 5 |

---

## 2. Verified Claims from Prior Surveys

### Confirmed (no change from prior surveys)

| Claim | Status | Verification Method |
|-------|--------|-------------------|
| `auth/tenant_store.go` has 3 PostgreSQL-only queries | **Confirmed** | Lines 29, 39, 47 — `admin.` schema, `$N` placeholders |
| No task directories for cleat-231..236 | **Confirmed** | `task_state/cleat-23*` glob returns nothing |
| INDEX.md missing | **Confirmed** | File does not exist |
| tasks.json missing | **Confirmed** | File does not exist |
| CI has 12 `continue-on-error: true` | **Confirmed** | 9 ci.yml, 1 ecosystem-ci.yml, 1 ai-pr-review.yml, 1 release-notes-check.yml |
| ARCHITECTURE.md stale module paths | **Confirmed** | References `internal/host/`, `internal/wasm/`, `internal/auth/` — all moved |
| ARCHITECTURE.md missing ChildWorkflow API docs | **Confirmed** | "ChildWorkflow" not found in ARCHITECTURE.md |
| ABI.md missing `cleat_poll_child`, `cleat_await_any_child` | **Confirmed** | Neither appears in ABI.md |
| CHANGELOG.md has no 0.5.0 entry | **Confirmed** | "0.5.0" not found in CHANGELOG.md |
| SECURITY.md missing signal auth section | **Confirmed** | "signal auth" not found |
| SECURITY.md missing encryption-at-rest section | **Confirmed** | "encryption-at-rest" not found |
| 2 closure tests failing | **Confirmed** | `LongRunning` at `testdata/basic/order.go:175` calls `DurableCall()` — not in expected leaves (8 expected, 9 actual). `TestComputeBasicCorrectlyTagsPureFunctions` expects 12 total funcs (8+4), gets 13 (9+4). |
| Host imports: 53 `cleat_*` in `engine/imports.go` | **Confirmed** | `grep -oP 'Export\("\Kcleat_[^"]+' engine/imports.go \| sort -u \| wc -l` = 53 (+3 non-cleat: `set_query_state`, `plugin_call`, `plugin_call_streaming` = 56 total) |
| `json.Marshal` error silently discarded at `backend_wasmtime.go:158` | **Confirmed** | `escaped, _ := json.Marshal(string(input))` |
| `json.Valid` guard in `engine/db.go:1398` (from `8d9b6f6`) | **Confirmed** | Validates result before jsonb insert |

### Refined: Host import count "15" claim

cto-lap-030-001s reported "69" based on a repo-wide grep including comments/docs/task_state. cto-lap-031 corrected this to 53 `cleat_*` exports from `engine/imports.go`. Confirmed again at 53 (+3 non-cleat = 56 total). The "15" in stale docs is ~3.5x too low; actual is ~3.5x higher.

---

## 3. Per-Item Assessment

### Item 1: ChildWorkflow API Cleanup ($15)
**No change.** Three APIs still overlap. ARCHITECTURE.md and ABI.md still missing ChildWorkflow docs. Leaf-ready.

### Item 2: Multi-DB Test Fixes ($20)
**Minor scope addition.** The uncommitted `::jsonb` cast in `engine/db.go` is PostgreSQL-specific — MySQL/MSSQL stores should be checked for equivalent handling. The `workflowID` fix in `engine/engine.go` is dialect-agnostic. Root cause (admin schema prefix in `auth/tenant_store.go`) unchanged. Leaf-ready.

### Item 3: SDK Test Passes ($15)
**Slight improvement.** The uncommitted signal routing fix (`""` → `s.engine.workflowID`) prevents signal conflation in multi-workflow SDK test scenarios, improving signal test reliability. Commit `8d9b6f6` already improved empty-result persistence. Leaf-ready.

### Item 4: CI Enforcement ($15)
**No change.** Still depends on items 2+3. 12 `continue-on-error: true`. 2 closure tests confirmed failing from code inspection. Leaf-ready.

### Item 5: Code Review ($20)
**Scope expanded.** Two new uncommitted changes to review:
- `engine/engine.go`: `DurableAwaitSignals` replay path signal store fallback — 30 lines of new code. Review for: synthetic event recording correctness, replay determinism, error handling.
- `engine/db.go`: `::jsonb` cast — simple but PostgreSQL-specific, check MySQL/MSSQL equivalents.
- Still pending from prior surveys: `engine/backend_wasmtime.go:158` discarded error, commit `1b7f8ed` and `8d9b6f6` review scope.

Recommend: commit the uncommitted engine changes before dispatching item 5, so the code review operates on committed code.

### Item 6: Documentation Audit ($15)
**No change.** All target docs need updates. ARCHITECTURE.md module paths still stale. "15" import count still wrong in ~7 files.

---

## 4. Dependency Status

Items 1, 2, 3, 5, 6 are independent and can start in parallel.
Item 4 depends on items 2 and 3 completing first.

Wave 1 (parallel): 231, 232, 233, 235, 236
Wave 2 (after 232+233): 234

---

## 5. Dispatch Status

**Still zero tasks dispatched across 9+ surveys.** The 6 leaf tasks (231-236) remain ready. No task directories exist. No INDEX.md. No tasks.json.

The uncommitted engine changes are the first "real" uncommitted work detected since the CEO guidance — they're bug fixes, not cosmetic. They should be committed before dispatch to avoid survey drift.

Ongoing scope refinements:
- cleat-231: Add ARCHITECTURE.md module path fix to scope
- cleat-232: Note `auth/` at root, not `internal/auth/`; add `::jsonb` dialect check
- cleat-235: Add uncommitted `engine/engine.go` signal routing review + `engine/db.go` `::jsonb` cast review + prior `backend_wasmtime.go:158` error handling + commits `1b7f8ed`/`8d9b6f6` review
- cleat-236: Add ARCHITECTURE.md path fix + "15→53" import count fix

---

## 6. Recommendation

**1. Commit the uncommitted engine changes.** The signal routing fix (`""` → `workflowID`) and `::jsonb` cast are correctness improvements that should be on `develop` before leaf tasks are dispatched.

**2. Dispatch immediately after commit.** 9+ surveys, zero dispatches. The 6 items remain leaf-ready. The new uncommitted changes are more reason to dispatch — they show the code is still being fixed, and delaying dispatch means more survey drift.

**3. Escalation: dispatch bottleneck is now severe.** The survey loop has been running for ~24 hours across 9+ laps with zero task launches. The CEO must intervene to break the cycle.

---

## RE-SURVEY (2026-06-05 run 2): Dispatch Bottleneck Broken — Tasks Created

### What Changed

The dispatch bottleneck that consumed 10+ surveys has been resolved. All dispatch infrastructure now exists:

**New artifacts created:**
- `task_state/INDEX.md` — task table with coupling summary, wave plan, budget allocation
- `task_state/tasks.json` — JSON manifest with all 6 tasks, dispatch_plan, coupling matrix

**New task directories (all containing TASK.md, CONTRACT.md, STATUS.md):**
- `task_state/cleat-231/` — ChildWorkflow API Cleanup ($15, priority 2)
- `task_state/cleat-232/` — Multi-DB Test Fixes ($20, priority 1)
- `task_state/cleat-233/` — SDK Test Passes ($15, priority 1)
- `task_state/cleat-234/` — CI Enforcement ($15, priority 1, blocked on 232+233)
- `task_state/cleat-235/` — Code Review ($20, priority 2)
- `task_state/cleat-236/` — Documentation Audit ($15, priority 2)

All STATUS.md files read: **Phase: pending, Dispatched by: cto-lap-032**

### What Remains Unchanged

- HEAD at `98e32dd` (no new commits)
- Engine files clean (no uncommitted engine changes)
- Same cosmetic uncommitted files
- `auth/tenant_store.go` still has 3 PostgreSQL-only queries
- CI still has 12 `continue-on-error: true`
- 2 closure tests still failing
- ARCHITECTURE.md still stale
- All 6 tasks in `pending` phase — none started

### Updated Claims Table

| Claim | Run 1 | Run 2 |
|-------|-------|-------|
| No task directories for cleat-231..236 | **Confirmed** | **FALSE — all 6 exist** |
| INDEX.md missing | **Confirmed** | **FALSE — exists** |
| tasks.json missing | **Confirmed** | **FALSE — exists** |
| 6 leaf tasks ready, zero dispatched | **Confirmed** | **FALSE — all 6 dispatched as pending** |

### Updated Recommendation

**Shift from "dispatch" to "execute."** The survey loop's purpose (verify state, recommend dispatch) is fulfilled. The tasks are formally created and dispatched. The next action is for implementing agents to pick up Wave 1 tasks (cleat-231, 232, 233, 235, 236 in parallel). No further re-surveys needed unless a material change (new commits, task phase transitions) is detected.

---

## RE-SURVEY (2026-06-05 run 5): Execution Bottleneck Broken — 5 of 6 Tasks Active

### What Changed

**Major change: tasks are executing.** Five of six tasks have moved beyond `pending` phase. Only cleat-233 remains stuck.

**New commit:** `010c2ed` committed the wasmtime context fix that run 4 detected as an uncommitted engine diff. HEAD moved from `98e32dd` → `010c2ed`.

### Task Phase Transitions

| Task | Run 4 | Run 5 | Deliverables |
|------|-------|-------|-------------|
| cleat-231 | pending | **complete** | ChildWorkflow API audit, ARCHITECTURE.md + ABI.md updates, runtime.go doc comments |
| cleat-232 | pending | **explored** | Full multi-DB scan: 3 PG-only queries in auth/tenant_store.go, 2 plugin files with same bug |
| cleat-233 | pending | **pending** | No progress. Last blocked task. |
| cleat-234 | pending | **explored** | Full CI audit: branch protection gaps, closure test root cause, coverage enforcement gaps |
| cleat-235 | pending | **review_complete** | 14 code review findings (4 critical), 3 fixes applied to engine/ |
| cleat-236 | pending | **completed (audit)** | 28 documentation discrepancies across 8 files |

### New Uncommitted Engine Diffs

The engine/backend_wasmtime.go and engine/db.go uncommitted changes are **new diffs** (not the ones from run 4):

- `engine/backend_wasmtime.go:158` — json.Marshal error check (cleat-235 fix #1)
- `engine/db.go:659` — `tx.Exec` → `tx.ExecContext` (cleat-235 fix #3)
- `engine/db.go:1342` — json.Valid guard in ContinueAsNew (cleat-235 fix #2)

The original run 4 diffs (AwaitSignals ctx fix, ::jsonb cast) are now committed as `010c2ed` and `98e32dd`.

### Stale Bookkeeping

INDEX.md and tasks.json still list all 6 tasks as `phase: pending`. They weren't updated as tasks progressed.

### Updated Claims Table

| Claim | Run 4 | Run 5 |
|-------|-------|-------|
| HEAD at 98e32dd | Confirmed | **FALSE — HEAD is 010c2ed** |
| No new commits | Confirmed | **FALSE — 1 new commit** |
| 6 tasks in pending | Confirmed | **FALSE — only cleat-233 pending** |
| Execution bottleneck (zero progress) | Confirmed | **FALSE — 5 tasks active** |

### Updated Recommendation

**Commit deliverables and close the survey.** The execution bottleneck is broken. Five tasks have produced concrete deliverables (code changes, audit reports, fix PRs). These should be committed to `develop` before they drift. cleat-233 is the only remaining gap and needs an implementing agent. The survey's information-gathering purpose is complete — no further re-surveys needed.

---

## RE-SURVEY (2026-06-05 run 6): Bookkeeping Fix — INDEX.md and tasks.json Updated

### What Changed

**No material changes since run 5.** HEAD at `010c2ed`. Same uncommitted files. Same task phases verified from individual STATUS.md files.

**Action taken:** Updated INDEX.md and tasks.json to reflect actual task phases (recommendation 5 from run 5). Both files previously showed all 6 tasks as `phase: pending`.

### Updated Phases

| Task | Was | Now |
|------|-----|-----|
| cleat-231 | pending | complete |
| cleat-232 | pending | explored |
| cleat-233 | pending | pending (unchanged) |
| cleat-234 | pending | explored |
| cleat-235 | pending | review_complete |
| cleat-236 | pending | completed (audit) |

### Remaining Open Items

1. Commit all task deliverables (~10 files from tasks 231, 235, 236)
2. Dispatch implementing agent for cleat-233 (SDK test passes)
3. Implement cleat-232 findings (multi-DB fixes, no code changes applied yet)
4. Implement cleat-234 findings (CI enforcement, depends on 232+233)

### Recommendation

Close cto-lap-032. The survey loop's purpose is fulfilled. Bookkeeping is now accurate. Remaining work belongs to implementing agents and the CTO coordination loop.

---

## RE-SURVEY (2026-06-05 run 8): cleat-233 Child Tasks Spawned, SDK Test Code Written

### Material Changes Since Run 7

**1. cleat-233 decomposed into 10+ child tasks.** The `task_state/cleat-233/` directory now contains a full tree of child task directories: `cleat-233a` through `cleat-233e`, plus `cleat-233dc`, `cleat-233de`, `cleat-233di`, `cleat-233dk`, `cleat-233dm`. This is massive progress from the single "decomposed" phase reported in run 7.

**2. cleat-233a completed — AssemblyScript tests all pass.** The child task fixed two bugs:
- AssemblyScript version mismatch (0.26.7 → 0.27.32) causing `ERR_REQUIRE_ASYNC_MODULE`
- Missing `await` in `as-pect.config.mjs` causing 19 silent test failures
- Result: all 106 tests pass (smoke: 16/16, json-saga: 71/71, json-host: 19/19)

**3. New SDK test code written.** Two engine test files modified since run 7:
- `engine/python_wasm_e2e_test.go` — **new file** (+17 lines): Python WASM E2E test infrastructure
- `engine/rust_workflow_test.go` — +13/-4 lines: Rust SDK integration test updates
- These are direct execution artifacts from cleat-233's child tasks (233b, 233c)

**4. CEO-GUIDANCE.md has major uncommitted diff.** The committed version (HEAD `010c2ed`) contains old WASM debugger guidance from 2026-05-25. The uncommitted version is the 0.5 Trial Release Hardening guidance from 2026-06-04 — 127 insertions, 81 deletions. This looks intentional: the CEO guidance file was updated in-place rather than committed.

**5. ABI.md and ARCHITECTURE.md heavily updated.** 126 and 57 lines changed respectively — task deliverables from cleat-231 (ChildWorkflow API docs) and cleat-236 (documentation audit fixes).

**6. cleat/runtime.go modified** — +22/-? lines: ChildWorkflow API doc comments from cleat-231.

### Run 8 Survey Results

| Artifact | Run 7 Status | Run 8 Status |
|----------|-------------|-------------|
| HEAD | `010c2ed` | **`010c2ed` — unchanged** |
| New commits | None | **None** |
| INDEX.md | Updated (run 7) | **Still accurate** |
| tasks.json | Updated (run 7) | **Still accurate** |
| cleat-231 | complete | **complete — unchanged** |
| cleat-232 | explored | **explored — unchanged (but 9 exploration passes logged)** |
| cleat-233 | decomposed | **decomposed with 10+ child tasks spawned; cleat-233a complete (106/106 AS tests pass)** |
| cleat-234 | explored | **explored — unchanged** |
| cleat-235 | review_complete | **review_complete — unchanged** |
| cleat-236 | completed (audit) | **completed (audit) — unchanged** |
| New engine test files | None | **engine/python_wasm_e2e_test.go (+17) + engine/rust_workflow_test.go (+17/-4)** |
| CEO-GUIDANCE.md diff | Small | **127 insertions, 81 deletions — rewritten from old WASM debugger to 0.5 hardening** |
| Uncommitted engine diffs | backend_wasmtime.go + db.go | **Same set — unchanged (cleat-235 fixes)** |

### Updated Claims Table

| Claim | Run 7 | Run 8 |
|-------|-------|-------|
| HEAD at `010c2ed` | Confirmed | **Confirmed** |
| No new commits | Confirmed | **Confirmed** |
| cleat-233 is decomposed with child tasks | Confirmed | **Confirmed + 10+ child dirs, 233a complete** |
| AS tests pass (106/106) | FALSE | **TRUE — cleat-233a fixed binaryen + await bugs** |
| SDK test code being written | Not yet | **TRUE — python_wasm_e2e_test.go + rust_workflow_test.go** |
| CEO-GUIDANCE.md matches hardening lap | Confirmed | **Needs verification — committed is old, uncommitted is current** |
| All 6 tasks have progressed beyond pending | Confirmed | **Confirmed — actively executing** |

### Assessment

**cleat-233 is no longer a bottleneck.** Run 7 identified cleat-233 as the last task without child tasks. It now has the most active child task tree of any task. The AssemblyScript fix (233a) is complete with all 106 tests passing. Python and Rust SDK test code is being written. The task is well into execution.

**Uncommitted deliverables are accumulating.** Tasks 231, 233, 235, and 236 have produced uncommitted changes to ~15+ files. CEO-GUIDANCE.md has a 208-line uncommitted rewrite. These should be committed before they drift further or conflict with each other.

**INDEX.md and tasks.json are now stale.** They show cleat-233 phase as "decomposed" but should reflect that child tasks are actively executing (233a complete, 233b-e in progress). The phase should probably be "executing" or "in_progress."

### Remaining Open Items

1. **Commit all task deliverables** — ~15+ files with uncommitted changes. This is now urgent since multiple tasks are touching overlapping files.
2. **Update INDEX.md and tasks.json** — cleat-233 phase should reflect child task execution.
3. **cleat-232 still explored-only** — 9 exploration passes, zero code fixes applied. The 3 PG-only queries in `auth/tenant_store.go` and 2 plugin files remain unfixed.
4. **cleat-234 blocked on 232** — CI enforcement can't proceed until multi-DB fixes are applied.

### Recommendation

**Keep cto-lap-032 open but shift to commit coordination.** The survey loop identified material progress. The remaining work is:
1. Commit all task deliverables to `develop` (urgent — files are drifting)
2. Dispatch an implementing agent for cleat-232 (multi-DB fixes — 9 exploration passes with zero implementation)
3. Update INDEX.md/tasks.json to reflect cleat-233 execution state

**One more re-survey warranted** after commits land or cleat-232 progresses beyond explored.
