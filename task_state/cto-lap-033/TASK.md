# cto-lap-033 — Run CTO Lap 91 (Next Lap)

**Priority:** 1
**Budget:** $5
**Project:** cleat
**Parent:** cto-lap-032 (CEO brief task)

## Purpose

Run the full CTO protocol for the next CTO lap. This is the CTO coordination task
that surveys project state, triages active tasks, cross-checks for issues, writes
survey-output.json for the automated dispatch pipeline, and produces the CEO brief.

The cto-lap-032 CEO brief (lap 90, 2026-06-06) identified these priorities:
1. Close cleat-228b (WASM debugger CLI — complete, verified)
2. Complete engine reliability polish (cleat-230-errors, cleat-230-race dispatch)
3. Investigate auto-implement revert (fbaf750)
4. Create lessons_learned/ directory

## Inputs

- `task_state/CEO-GUIDANCE.md` — 2026-05-25 guidance (2 items, $100 budget) still governs
- `task_state/cto-lap/artifacts/survey-output.json` — previous lap survey output
- `task_state/cto-lap/artifacts/ceo-brief-2026-06-06-lap90.md` — previous CEO brief
- `ARCHITECTURE.md` — current, may need updates from race audit patterns
- Active tasks: cleat-228b, cleat-230-logs, cleat-230-errors, cleat-230-race (+4 children)
- Abandoned task shells: cleat-232, cleat-232-tenant-store, cleat-233, cleat-233b, cleat-233d, cleat-234b, cleat-234d

## Deliverables

1. Updated `task_state/cto-lap/artifacts/survey-output.json` with decomps_ready (cleat-230-race dag), tasks_to_close (cleat-228b, cleat-230-logs), and any new decompositions
2. CEO brief in `task_state/cto-lap/artifacts/ceo-brief-<date>-lap<N>.md`
3. Updated `task_state/cto-lap/STATUS.md` with new lap number and findings

## Dependencies

- cto-lap-032 complete (CEO brief written for lap 90)
- cleat-228b verification (cleat-228bm) complete
- cleat-230-race dag.json (ready at `task_state/cleat-230-race/artifacts/dag.json`)
