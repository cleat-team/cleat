# cto-lap-030-001s

**Phase:** done
**Started:** 2026-06-05
**Completed:** 2026-06-05
**Explorer:** claude

## Recommendation

**Leaf-ready.** The 6 CEO guidance items (cleat-231 through cleat-236) are all well-understood and leaf-ready. No decomposition needed. Dispatch immediately.

## Key Findings

### No material change since cto-lap-survey

HEAD at `1b7f8ed` (same as cto-lap-survey). Uncommitted changes unchanged. Zero task dispatches across 7+ surveys.

### NEW: Critical ARCHITECTURE.md staleness

The `internal/` → public refactor (commit `3eeb74e`) moved `host`→`engine/`, `auth`→`auth/`, `wasm`+`plugin`→`cleat/`, but ARCHITECTURE.md Module Boundaries table and Coupling Matrix still reference `internal/host/`, `internal/wasm/`, etc. This is a **High** risk — any agent reading ARCHITECTURE.md navigates to wrong paths.

### NEW: Host import count is 69, not 50

"15 host imports" is stale in 6 files (SECURITY.md, LANGUAGE_SUPPORT.md, docs/explanation/architecture.md, ABI.md, packages/cleat-as/assembly/index.ts, packages/cleat-as/assembly/memory.ts). Actual count: 69 unique `cleat_*` imports.

### Confirmed: Closure test failure

`LongRunning` in `testdata/basic/order.go:175` calls `h.DurableCall()` but is not in test expectations (expects 8 leaves, has 9). The two tests will fail as prior surveys claimed.

### Confirmed: All 18 prior survey claims verified

All key claims from Lap 90 through cto-lap-survey verified against current code:
- `auth/tenant_store.go` has 3 PostgreSQL-only queries ✓
- No task directories for 231-236 ✓
- INDEX.md + tasks.json missing ✓
- CI has 12 `continue-on-error: true` ✓
- CHANGELOG.md thin, no 0.5.0 entry ✓
- SECURITY.md missing signal auth + encryption-at-rest ✓
- ARCHITECTURE.md missing ChildWorkflow API docs ✓
- ABI.md missing `cleat_poll_child`, `cleat_await_any_child` ✓

## Scope Refinements for Leaf Tasks

| Task | Refinement |
|------|-----------|
| cleat-231 | Add ARCHITECTURE.md module path fix to scope |
| cleat-232 | Note `auth/` at root, not `internal/auth/` |
| cleat-233 | Include review of commit 1b7f8ed error handling |
| cleat-235 | Add `engine/backend_wasmtime.go` dispatch path to review |
| cleat-236 | Add ARCHITECTURE.md path fix + "15→69" import count fix |

## Escalation

**Dispatch bottleneck is critical.** 7+ surveys, zero tasks launched. The clew-cto-lap workflow hasn't triggered. CEO intervention needed to break the survey loop.
