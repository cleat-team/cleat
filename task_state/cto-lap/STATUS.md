# cto-lap

**Phase:** leaf_tasks_ready
**Lap:** 90
**Survey complete:** 2026-06-04
**Projects surveyed:** cleat engine
**CEO Guidance:** task_state/CEO-GUIDANCE.md (2026-06-04, $100, 6 items)

## Lap 90 Survey

New CEO guidance issued today for cleat 0.5 Trial Release Hardening — 6 items, $100 budget. Prior guidance (cleat-228b, cleat-230) superseded. clew CEO guidance (2026-05-30, data boundary hardening) separate concern — not in scope this lap.

### Key findings

- **ChildWorkflow API** (item 1): Three overlapping APIs. `ChildWorkflowWithOptions` is canonical but least used. `ChildWorkflow` is the base primitive (can't be removed). `ChildWorkflowTyped` is convenience wrapper. Neither ARCHITECTURE.md nor ABI.md documents them.
- **Multi-DB** (item 2): Root cause identified — `auth/tenant_store.go` has 3 hardcoded PostgreSQL-only queries with `admin.` schema, `$N` placeholders, and `RETURNING`. MySQL and MSSQL tests silently skipped in CI.
- **SDK tests** (item 3): Rust SDK well-tested. AssemblyScript has incomplete JS stubs (only 2 of 50+ host imports). Python WASM E2E NEVER validated — CI has path mismatch in e2e-cross-language.yml.
- **CI enforcement** (item 4): 2 closure tests fail (testdata drift). Coverage non-blocking, thresholds max 50% (CEO wants 75%). test-tinygo disabled. lint-go disabled.
- **Code review** (item 5): ~109K lines. Encryption AAD=nil gap. 670 defers look correct. SQL mostly parameterized. 2 medium-risk spots identified.
- **Documentation** (item 6): CHANGELOG.md critically thin (no 0.5.0 entry). SECURITY.md missing signal auth + encryption-at-rest. 3 docs stale on "15 imports" → should be 50.

### Prior task cleanup

- **cleat-228b** ($20): Production code merged via PR #56. Remaining test infra + docs absorbed into new items 3+6. Close.
- **cleat-230** ($20): Never started. Race audit absorbed into new item 5. Close.
- **cleat-228a**: Already closed (lap 89).

### Recommended dispatch

| Task | Subject | Budget | Depends On | Priority |
|------|---------|--------|------------|----------|
| cleat-231 | ChildWorkflow API cleanup + docs | $15 | — | 2 |
| cleat-232 | Multi-DB test fixes | $20 | — | 1 |
| cleat-233 | SDK test passes | $15 | — | 1 |
| cleat-234 | CI enforcement | $15 | cleat-232, cleat-233 | 1 |
| cleat-235 | Code review | $20 | — | 2 |
| cleat-236 | Documentation audit | $15 | — | 2 |

Items 231, 232, 233, 235, 236 can dispatch in parallel. Item 234 after 232+233 pass.

### Artifacts written

- `artifacts/exploration.md` — full survey, per-item analysis, risks, recommendations
- `artifacts/survey-output.json` — 0 decomps_ready (leaf tasks), 2 tasks_to_close (228b, 230), 6 leaf_tasks

## Lap 90 Re-Survey (cto-lap-017)

**Date:** 2026-06-04
**Type:** Re-confirmation survey
**Finding:** No material state change since Lap 90.

- No new commits on develop since CEO guidance issued.
- No tasks (231-236) dispatched. No task-specific state files created.
- 6 leaf tasks remain ready-to-dispatch (231, 232, 233, 235, 236 in wave 1; 234 after deps).
- 2 tasks remain for closure (228b, 230).
- Uncommitted changes unchanged (auth/middleware.go, web UI, dist assets).
- survey-output.json updated with survey_id cto-lap-017.

### Unresolved

1. Python WASM E2E viability — never validated. Timebox: 4 hours; escalate if insoluble.
2. Branch protection rules — GitHub admin access needed to verify.
3. Coverage 75% threshold — start enforcement at 50%, ratchet to 75%.
4. test-tinygo re-enable — verify Go 1.26 compatibility first.

## Lap 90 Re-Survey (cto-lap-019)

**Date:** 2026-06-04
**Type:** Re-confirmation survey
**Finding:** No material state change since cto-lap-017.

- No new commits on develop since CEO guidance issued.
- No tasks (231-236) dispatched. No task-specific state files created.
- 6 leaf tasks remain ready-to-dispatch (231, 232, 233, 235, 236 in wave 1; 234 after deps).
- 2 tasks remain for closure (228b, 230).
- Uncommitted changes unchanged (auth/middleware.go, web UI, dist assets).
- INDEX.md and tasks.json still missing (infrastructure gaps).
- survey-output.json updated with survey_id cto-lap-019.

### Unresolved (unchanged)

1. Python WASM E2E viability — never validated. Timebox: 4 hours; escalate if insoluble.
2. Branch protection rules — GitHub admin access needed to verify.
3. Coverage 75% threshold — start enforcement at 50%, ratchet to 75%.
4. test-tinygo re-enable — verify Go 1.26 compatibility first.

## Lap 90 Re-Survey (cto-lap-021)

**Date:** 2026-06-04
**Type:** Re-confirmation survey
**Finding:** No material state change since cto-lap-019.

- No new commits on develop since CEO guidance issued.
- No tasks (231-236) dispatched. No task-specific state files created.
- 6 leaf tasks remain ready-to-dispatch (231, 232, 233, 235, 236 in wave 1; 234 after deps).
- 2 tasks remain for closure (228b, 230).
- Uncommitted changes unchanged (auth/middleware.go, web UI, dist assets).
- INDEX.md and tasks.json still missing (infrastructure gaps).
- survey-output.json updated with survey_id cto-lap-021.

### Unresolved (unchanged)

1. Python WASM E2E viability — never validated. Timebox: 4 hours; escalate if insoluble.
2. Branch protection rules — GitHub admin access needed to verify.
3. Coverage 75% threshold — start enforcement at 50%, ratchet to 75%.
4. test-tinygo re-enable — verify Go 1.26 compatibility first.

## Lap 90 Re-Survey (cto-lap-031)

**Date:** 2026-06-04
**Type:** Re-confirmation survey
**Finding:** No material state change since cto-lap-021.

- No new commits on develop since CEO guidance issued.
- No tasks (231-236) dispatched. No task-specific state files created.
- 6 leaf tasks remain ready-to-dispatch (231, 232, 233, 235, 236 in wave 1; 234 after deps).
- 2 tasks remain for closure (228b, 230).
- Uncommitted changes unchanged (auth/middleware.go, web UI, dist assets).
- INDEX.md and tasks.json still missing (infrastructure gaps).
- survey-output.json updated with survey_id cto-lap-031.

### Unresolved (unchanged)

1. Python WASM E2E viability — never validated. Timebox: 4 hours; escalate if insoluble.
2. Branch protection rules — GitHub admin access needed to verify.
3. Coverage 75% threshold — start enforcement at 50%, ratchet to 75%.
4. test-tinygo re-enable — verify Go 1.26 compatibility first.

### Note

Sixth survey on the same CEO guidance (cto-lap-015, 017, 019, 021, 031). Zero tasks dispatched across all surveys. The bottleneck is dispatch latency, not missing information. Recommend CEO intervention to break the survey loop.

## Lap 90 Re-Survey (cto-lap-031, 2026-06-05 run)

**Date:** 2026-06-05
**Type:** Re-confirmation survey incorporating cto-lap-032 findings
**Finding:** Uncommitted engine hot-path changes compound staleness. Still zero dispatches.

- HEAD at 8d9b6f6 (unchanged). 2 commits since original survey (1b7f8ed, 8d9b6f6).
- cto-lap-032 detected uncommitted engine hot-path changes: engine/engine.go (+39/-7 signal routing fix) and engine/db.go (+1/-1 ::jsonb cast). These should be committed before dispatch.
- 6 leaf tasks (231-236) remain ready-to-dispatch.
- 2 tasks remain for closure (228b, 230).
- INDEX.md and tasks.json still missing.
- survey-output.json updated with survey_id cto-lap-031.

## Lap 90 Re-Survey (cto-lap-031, 2026-06-05 run 2)

**Date:** 2026-06-05
**Type:** Re-confirmation survey
**Finding:** State identical to prior run. No changes since 2026-06-05 07:01.

- HEAD at 8d9b6f6 (unchanged). No new commits.
- Same uncommitted engine hot-path changes: engine/engine.go signal routing fix, engine/db.go ::jsonb cast.
- 6 leaf tasks (231-236) remain ready-to-dispatch. Wave 1 (231, 232, 233, 235, 236), Wave 2 (234 after 232+233).
- 2 tasks remain for closure (228b, 230).
- INDEX.md and tasks.json still missing.
- survey-output.json updated.

## Lap 90 Re-Survey (cto-lap-031, 2026-06-05 run 3)

**Date:** 2026-06-05
**Type:** Re-confirmation survey
**Finding:** No material change. HEAD at 98e32dd. Engine hot-path changes committed.

- HEAD at 98e32dd (unchanged from cto-lap-032 re-survey). Three commits since CEO guidance: 1b7f8ed, 8d9b6f6, 98e32dd.
- Engine files clean — uncommitted hot-path changes committed as 98e32dd. No technical barrier to dispatch.
- 6 leaf tasks (231-236) remain ready-to-dispatch. Wave 1 (231, 232, 233, 235, 236), Wave 2 (234 after 232+233).
- 2 tasks remain for closure (228b, 230).
- INDEX.md and tasks.json still missing.
- survey-output.json updated with survey_id cto-lap-031 run 3.

## Lap 90 Re-Survey (cto-lap-032)

**Date:** 2026-06-05
**Type:** Re-confirmation survey following engine commit
**Finding:** COMMIT BARRIER RESOLVED. Primary cto-lap-032 finding (uncommitted engine changes) resolved by commit 98e32dd. Dispatch now unblocked.

- HEAD at 98e32dd. Three engine hot-path commits since CEO guidance (1b7f8ed, 8d9b6f6, 98e32dd) — all committed by other agents while tasks await dispatch.
- Engine files clean — uncommitted changes that cto-lap-032 originally detected are now committed. No technical barrier.
- 6 leaf tasks (231-236) remain ready-to-dispatch. Wave 1 (231, 232, 233, 235, 236), Wave 2 (234 after 232+233).
- 2 tasks remain for closure (228b, 230).
- INDEX.md and tasks.json still missing.
- survey-output.json updated with survey_id cto-lap-032.

**Escalation:** 10th survey, zero dispatches, ~24 hours elapsed. The commit barrier that justified earlier delays is removed. CEO intervention needed to break the survey loop.
