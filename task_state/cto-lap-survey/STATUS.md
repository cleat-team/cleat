# cto-lap-survey

**Phase:** completed
**Survey ID:** cto-lap-survey
**Date:** 2026-06-05
**Explorer agent:** claude

## Survey scope

Survey the cleat engine repo for the CEO guidance items. Verify claims from prior surveys (cto-lap-031 and earlier) against current repo state. Identify any material changes.

## Key finding: Material state change since cto-lap-031

Previous surveys (Lap 90, cto-lap-017, 019, 021, 031) all reported "No new commits on develop since CEO guidance issued. HEAD at a019068."

This is no longer true. Commit `1b7f8ed` was pushed to develop on 2026-06-04:

```
1b7f8ed fix: wrap workflow input in DispatchWrapper for WASM entry point
```

This commit modifies `engine/backend_wasmtime.go` (+4/-1 lines) to wrap workflow input in the DispatchWrapper envelope format expected by `gen_wasm_exports.go`. Previously, all WASM workflow inputs were arriving empty because the engine was passing flat JSON without the envelope.

**Impact on CEO guidance items:**
- Item 3 (SDK tests): Directly relevant. This fix affects WASM input dispatch, which is the path exercised by Go WASM cross-language tests and potentially Python WASM E2E.
- Item 5 (Code review): The changed code path in `engine/backend_wasmtime.go` should be reviewed as part of the WASM execution hot path.

## Verified claims from prior surveys

| Claim | Status | Notes |
|-------|--------|-------|
| `auth/tenant_store.go` has 3 PostgreSQL-only queries | **Confirmed** | Lines 29, 39, 47 still use `admin.` schema, `$N` placeholders, `RETURNING` |
| No task directories exist for cleat-231..236 | **Confirmed** | Zero dispatches across 6+ surveys |
| INDEX.md missing | **Confirmed** | Still absent |
| tasks.json missing | **Confirmed** | Still absent |
| CI has non-blocking jobs | **Confirmed** | 12 `continue-on-error: true` across ci.yml, ecosystem-ci.yml, others |
| ARCHITECTURE.md exists | **Confirmed** | 60+ lines, but stale — no ChildWorkflow API docs, no signal auth section |
| ABI.md exists | **Confirmed** | 47KB, last modified Jun 2 |
| All target docs present | **Confirmed** | CHANGELOG, SECURITY, LANGUAGE_SUPPORT, CONTRIBUTING, DX_COMPARISON, README all exist |

## Uncommitted changes (unchanged from prior surveys)

- `auth/middleware.go` — adding public SPA asset paths to auth bypass
- `web/src/*` — admin dashboard Svelte 5 updates
- `cmd/cleat-worker/web/dist/*` — rebuilt dist assets
- `task_state/*` — CTO lap process files

None of these affect the 6 CEO guidance items directly.

## Closure tests

`LongRunning` not found in `internal/closure/` (neither test expectations nor testdata). The prior survey's claim about "testdata added LongRunning function but test expectations weren't updated" could not be verified — `go test` was unavailable in this environment. Test file at `internal/closure/closure_test.go` expects 8 functions; testdata should be checked for drift.

## Recommendation

Material change detected (new commit 1b7f8ed on develop). This is the first code change since the CEO guidance was issued. The previous surveys' finding of "no commits" is stale. The 6 leaf tasks (231-236) should still be dispatched, but agents working on item 3 (SDK tests) and item 5 (code review) should be aware of the new commit's impact on WASM input dispatch.

Dispatch bottleneck remains: 6+ surveys, zero tasks launched. The infrastructure gaps (INDEX.md, tasks.json) persist.
