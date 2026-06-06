# Task Index — cleat

**Project:** cleat (Apache 2.0)
**Last updated:** 2026-06-06 (cto-lap-032 run 10)

## Active Tasks

| Task ID | Subject | Priority | Budget | Depends On | Phase | Wave |
|---------|---------|----------|--------|------------|-------|------|
| cleat-231 | ChildWorkflow API cleanup + docs | 2 | $15 | — | complete | 1 |
| cleat-232 | Multi-DB test fixes (MySQL, MSSQL) | 1 | $20 | — | in_progress | 1 |
| cleat-233 | SDK test passes (Rust, AS, Python) | 1 | $15 | — | executing | 1 |
| cleat-234 | CI enforcement + closure test fix | 1 | $15 | cleat-232, cleat-233 | explored | 2 |
| cleat-235 | Code review (engine, auth, wasm) | 2 | $20 | — | review_complete | 1 |
| cleat-236 | Documentation audit | 2 | $15 | — | completed (audit) | 1 |

**Total budget:** $100 / $100 allocated
**Wave 1 (parallel):** cleat-231, cleat-232, cleat-233, cleat-235, cleat-236 (max concurrency: 5)
**Wave 2 (after deps):** cleat-234

## Coupling Summary

| Task Pair | Coupling | Shared Concern |
|-----------|----------|---------------|
| cleat-231 ↔ cleat-236 | LOOSE | ARCHITECTURE.md, ABI.md |
| cleat-232 ↔ cleat-234 | MEDIUM | cleat-234 consumes green multi-db CI |
| cleat-232 ↔ cleat-235 | LOOSE | auth/tenant_store.go, engine/db.go |
| cleat-233 ↔ cleat-234 | MEDIUM | cleat-234 consumes green SDK tests |
| cleat-233 ↔ cleat-235 | LOOSE | wasm/ files |
| cleat-235 ↔ cleat-236 | LOOSE | review findings → doc updates |

No TIGHT coupling pairs — Wave 1 can run all 5 tasks in parallel.

## Completed Tasks

- **cleat-231** — ChildWorkflow API audit + ARCHITECTURE.md/ABI.md/runtime.go docs updates
- **cleat-236** — Documentation audit: 28 discrepancies across 8 docs identified

## CEO Guidance

`task_state/CEO-GUIDANCE.md` (2026-06-04, $100 budget, 6 items)
