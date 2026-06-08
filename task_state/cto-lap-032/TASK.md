# cto-lap-032 — Write CEO Brief for CTO Lap

**Priority:** 1
**Budget:** $5
**Project:** cleat
**Parent:** cto-lap-031 (survey task)

## Purpose

Write the CEO brief summarizing the CTO lap's completed work, decisions, and
findings. This is the `writeBrief` step in the CTO lap workflow
(`clew/workflows/ctolap/workflow.go`).

## Inputs

- cto-lap-031 survey output (`artifacts/survey-output.json`)
- CEO-GUIDANCE.md for the cleat project
- Tasks dispatched, reviewed, and closed during this lap
- ARCHITECTURE.md changes (if any)
- Lessons learned from this lap

## Deliverables

- CEO brief in `task_state/cto-lap/artifacts/ceo-brief-<date>-lap<N>.md`
- Format follows previous briefs (see `clew/task_state/cto-lap/artifacts/` for examples):
  - Survey summary
  - Triage table
  - Cross-check findings
  - Launched this lap (with quick-review justifications)
  - Budget status table
  - What NOT to do
  - Next lap preview

## Dependencies

- **cto-lap-031** — must complete with survey output before brief can be written
