# Exploration: cto-lap-031 — cleat 0.5 Trial Release Hardening Survey

**Explorer:** cto-lap-031 (claude explorer agent)
**Date:** 2026-06-05 (re-surveyed 2026-06-05 after material change)
**CEO Guidance:** `task_state/CEO-GUIDANCE.md` (2026-06-04, $100 budget, 6 items)
**Prior surveys:** Lap 90, cto-lap-017, cto-lap-019, cto-lap-021 (all 2026-06-04), cto-lap-survey (2026-06-05), cto-lap-030-001s (2026-06-05)

---

## 0. Material Change Since Original Survey

**NEW COMMIT `8d9b6f6`** landed 2026-06-05 07:01 — after the original cto-lap-031 exploration.

```
8d9b6f6 fix: handle empty/invalid JSON workflow results and cast query_state bytes to string
```

- **File:** `engine/db.go` (+8, -3)
- **Author:** rcownie, Co-Authored-By: Claude Opus 4.7
- **HEAD:** now `8d9b6f6` (was `1b7f8ed` at original survey)

### Changes

1. **"done" path in `PostgresStore.FinalizeWorkflowSegment`**: Validates `result` string is non-empty and valid JSON before writing to PostgreSQL `jsonb` column. Falls back to `"{}"` for empty/invalid results.
2. **`string(qsJSON)` casts**: Casts `[]byte` query state to `string` for `$N` placeholder bindings to avoid PostgreSQL bytea encoding.

### Impact per CEO Item

| Item | Impact | Detail |
|------|--------|--------|
| 1. ChildWorkflow API | None | No API files touched |
| 2. Multi-DB Fixes | Low | Fix is PostgreSQL-specific. MySQL/MSSQL stores (`engine/mysql_store.go`, `engine/mssql_store.go`) should be checked for similar empty-result handling gaps. The `string()` cast is semantically correct for Postgres but may differ on other dialects. |
| 3. SDK Tests | Low-positive | Empty results (common in error paths returning `("", err)`) now persist correctly. Previously caused "invalid input syntax for type json" on Postgres. This removes a spurious failure mode during SDK testing. |
| 4. CI Enforcement | None | No CI files touched |
| 5. Code Review | Medium | `engine/db.go` is a hot-path file in review scope. The `json.Valid()` guard is correct but dialect-specific. Review should verify MySQL/MSSQL equivalent code paths. The silently-discarded `json.Marshal` error at `backend_wasmtime.go:158` (from commit `1b7f8ed`) remains unfixed — this commit demonstrates the pattern for how to fix it (validate before write). |
| 6. Documentation | None | No doc implications |

---

## 1. Material Change Since Last Survey (Original, Still Valid)

**Original survey baseline:** HEAD at `1b7f8ed`. No new commits on develop between 2026-06-04 and original survey write. Uncommitted changes same as all prior surveys:

- `auth/middleware.go` — SPA asset path auth bypass
- `web/src/*` — admin dashboard Svelte 5 updates
- `cmd/cleat-worker/web/dist/*` — rebuilt dist assets
- `task_state/*` — CTO lap process files

---

## 2. Verified Claims from Prior Surveys

### Confirmed (no change from prior surveys)

| Claim | Status | Notes |
|-------|--------|-------|
| `auth/tenant_store.go` has 3 PostgreSQL-only queries using `admin.` schema | **Confirmed** | Lines 29, 39, 47. Only line 29 uses RETURNING (prior surveys overstated — lines 39 and 47 do not). |
| No task directories exist for cleat-231..236 | **Confirmed** | Zero dispatches across 8+ surveys |
| INDEX.md missing | **Confirmed** | `task_state/INDEX.md` does not exist |
| tasks.json missing | **Confirmed** | No machine-readable task registry |
| CI has 12 `continue-on-error: true` | **Confirmed** | 9 in ci.yml, 1 in ecosystem-ci.yml, 1 in release-notes-check.yml, 1 in ai-pr-review.yml |
| ARCHITECTURE.md stale | **Confirmed** | References `internal/host/` (now `engine/`), missing ChildWorkflow API docs, missing signal auth section, TinyGo references contradictory with deprecation note |
| ABI.md stale | **Confirmed** | Missing `cleat_poll_child`, `cleat_await_any_child`, priority param on `cleat_child_workflow_with_options` and `cleat_child_workflow_in_schema` |
| CHANGELOG.md thin | **Confirmed** | No 0.5.0 entry, essentially a placeholder template with 2 substantive entries out of 12 |
| SECURITY.md gaps | **Confirmed** | Missing signal auth section, missing encryption-at-rest section |
| Commit 1b7f8ed silently discards error | **Confirmed** | `engine/backend_wasmtime.go:158` — `escaped, _ := json.Marshal(string(input))` |

### Newly Verified (prior surveys were unable to confirm)

| Claim | Status | Detail |
|-------|--------|--------|
| 2 closure tests failing | **Confirmed** | `LongRunning` at `testdata/basic/order.go:175` calls `h.DurableCall()` in a loop. `TestComputeBasicIdentifiesDurableLeaves` expects 8 leaves, gets 9. `TestComputeBasicCorrectlyTagsPureFunctions` expects 12 functions, gets 13. Both will fail at runtime. |

### Refined (prior survey claims corrected)

| Claim | Prior Claim | Verified |
|-------|-------------|----------|
| Host import count | "69 unique cleat_* imports" | **Incorrect.** Actual count in `engine/imports.go`: 53 `cleat_*` exports (+3 non-cleat: `set_query_state`, `plugin_call`, `plugin_call_streaming`). Total: 56. The 69 was likely a repo-wide grep count including comments, docs, and task_state files. |
| "15 host imports" references | "stale in 6 files" | **Confirmed but undercounted.** 7 distinct files, 10 reference sites: SECURITY.md (1), LANGUAGE_SUPPORT.md (4), docs/explanation/architecture.md (1), docs/explanation/wasm-compilation.md (1), ABI.md (1, changelog entry), packages/cleat-as/assembly/index.ts (1), packages/cleat-as/assembly/memory.ts (1). |
| ABI.md "50" count | "50 documented" | **Stale.** Actual documented sections: 52 (2.1–2.52). Actual engine exports: 56. |

---

## 3. Per-Item Assessment

### Item 1: ChildWorkflow API Cleanup ($15)
**No change.** Commit `8d9b6f6` does not touch ChildWorkflow API files. Three APIs (`ChildWorkflow`, `ChildWorkflowWithOptions`, `ChildWorkflowTyped`) overlap. ARCHITECTURE.md and ABI.md need updates. Leaf-ready.

### Item 2: Multi-DB Test Fixes ($20)
**Minor scope addition.** Commit `8d9b6f6` adds PostgreSQL-specific JSONB handling. The MySQL and MSSQL stores (`engine/mysql_store.go`, `engine/mssql_store.go`) should be checked for equivalent empty-result validation. Root cause (admin schema prefix) unchanged. Leaf-ready.

### Item 3: SDK Test Passes ($15)
**Slight improvement.** Commit `8d9b6f6` fixes empty-result persistence on Postgres, removing a spurious failure mode. WASM input dispatch fix from `1b7f8ed` still relevant. Leaf-ready.

### Item 4: CI Enforcement ($15)
**No change.** Depends on items 2 and 3 completing first. 12 `continue-on-error: true` across 4 workflow files. Leaf-ready.

### Item 5: Code Review ($20)
**Scope expanded.** Commit `8d9b6f6` should be included in review scope. The `json.Valid()` + fallback pattern is good; the `string(qsJSON)` cast is dialect-specific. Review should verify MySQL/MSSQL store code paths in `engine/db.go`. The silently-discarded `json.Marshal` error at `engine/backend_wasmtime.go:158` (from commit `1b7f8ed`) remains. Leaf-ready.

### Item 6: Documentation Audit ($15)
**No change.** All target docs need updates. The "15" import count is stale in 7 files (10 sites). ARCHITECTURE.md needs module path fixes. Leaf-ready.

---

## 4. Dependency Status

Items 1, 2, 3, 5, 6 are independent and can start in parallel.
Item 4 depends on items 2 and 3 completing first.

Wave 1 (parallel): 231, 232, 233, 235, 236
Wave 2 (after 232+233): 234

---

## 5. Dispatch Status

**Still zero tasks dispatched across 8+ surveys.** The 6 leaf tasks (231-236) remain ready. The new commit `8d9b6f6` demonstrates active code changes — the longer dispatch is delayed, the more survey drift accumulates.

Three scope refinements from cto-lap-030-001s remain valid:
- cleat-231: Add ARCHITECTURE.md module path fix to scope
- cleat-235: Add `engine/backend_wasmtime.go` dispatch path + error handling to review
- cleat-236: Add ARCHITECTURE.md path fix + "15→53" import count fix

Plus one new:
- cleat-235: Add `engine/db.go` MySQL/MSSQL dialect audit to code review scope (from `8d9b6f6`)

---

## 6. Recommendation

**Dispatch immediately.** The material change (commit `8d9b6f6`) is a minor PostgreSQL bug fix that improves SDK test reliability and demonstrates the code is still being actively patched. It does not invalidate any prior survey findings. It does add one small scope item (MySQL/MSSQL dialect audit in `engine/db.go` for item 5).

The survey loop (8+ surveys, zero dispatches) is the bottleneck — not survey accuracy.
