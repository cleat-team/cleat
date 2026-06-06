# Exploration: cto-lap-survey — cleat 0.5 Trial Release Hardening Re-Survey

**Explorer:** cto-lap-survey (claude explorer agent)
**Date:** 2026-06-05
**CEO Guidance:** `task_state/CEO-GUIDANCE.md` (2026-06-04, $100 budget, 6 items)
**Prior surveys:** Lap 90, cto-lap-017, cto-lap-019, cto-lap-021, cto-lap-031 (all 2026-06-04)

---

## 1. Material Change Since Last Survey

**Previous surveys all stated:** "No new commits on develop since CEO guidance issued. HEAD at a019068."

**Current state:** HEAD is at `1b7f8ed` — one new commit since the prior surveys:

```
1b7f8ed fix: wrap workflow input in DispatchWrapper for WASM entry point
Author: rcownie
Date:   2026-06-04
```

**What changed:** `engine/backend_wasmtime.go` (+4/-1). The WASM entry point wrapper (`gen_wasm_exports.go`) expects workflow input wrapped in a `DispatchWrapper` envelope (`{"inputJSON":"<escaped>"}`), but the engine was passing flat JSON. All WASM workflow inputs were arriving empty. The fix wraps input in the correct envelope format before writing to WASM memory.

**Relevance to CEO guidance:**
- **Item 3 (SDK tests):** This fix changes WASM input dispatch behavior. The Go WASM cross-language tests, Rust wasmtime tests, and Python WASM E2E tests all exercise this path. Agents working on SDK tests must be aware of this change.
- **Item 5 (Code review):** The changed code path in `engine/backend_wasmtime.go` is part of the WASM execution hot path. It uses `json.Marshal` + `fmt.Sprintf` in the hot path — worth reviewing for performance and correctness under concurrent workloads.

---

## 2. Verified Claims from Prior Surveys

### Confirmed (no change)

| Claim | Detail |
|-------|--------|
| `auth/tenant_store.go` PostgreSQL-only | 3 queries with `admin.` schema, `$N` placeholders, `RETURNING` — lines 29, 39, 47 |
| No task directories (231-236) | Zero dispatches across all surveys |
| INDEX.md missing | `task_state/INDEX.md` does not exist |
| tasks.json missing | No machine-readable task registry |
| CI non-blocking jobs | 12 `continue-on-error: true` in workflow YAML files |
| ARCHITECTURE.md stale | No ChildWorkflow API docs, no signal auth, mentions TinyGo as current |
| ABI.md stale | Missing `cleat_poll_child`, `cleat_await_any_child`; priority param not documented |
| CHANGELOG.md thin | No 0.5.0 entry, 156 words of boilerplate |
| SECURITY.md gaps | Missing signal auth section, missing encryption-at-rest section, "15 imports" stale |

### Unconfirmed

| Claim | Reason |
|-------|--------|
| 2 closure tests failing (TestComputeBasicIdentifiesDurableLeaves, TestComputeBasicCorrectlyTagsPureFunctions) | `go test` unavailable. `LongRunning` not found in closure package grep. Test expects 8 functions. |
| MySQL/MSSQL CI jobs failing | Cannot run CI locally |
| Python WASM E2E never validated | Cannot run tests |

---

## 3. Uncommitted Changes (no material impact on guidance items)

- `auth/middleware.go` — adding `/`, `/index.html`, `/assets/*`, `/favicon.ico` to public auth bypass (SPA asset serving)
- `web/src/App.svelte`, `Sidebar.svelte`, `api.ts`, `WorkflowDetail.svelte`, `WorkflowList.svelte`, `vite.config.ts` — admin dashboard updates
- `cmd/cleat-worker/web/dist/*` — rebuilt assets from web changes
- `task_state/CEO-GUIDANCE.md`, `cto-lap/STATUS.md`, `cto-lap/artifacts/survey-output.json`, `cto-lap/session.json` — CTO process files

---

## 4. Updated Per-Item Assessment

### Item 1: ChildWorkflow API Cleanup ($15)
**No change from prior surveys.** The new commit does not touch any ChildWorkflow API files.

### Item 2: Multi-DB Test Fixes ($20)
**No change from prior surveys.** Root cause still valid. No new migration files. No dialect-awareness in auth/ or plugin/ packages.

### Item 3: SDK Test Passes ($15)
**Changed.** The new commit fixes WASM input dispatch, which directly affects this item. The fix may resolve some WASM input-related test failures but introduces new behavior that should be verified across all SDKs (Go WASM, Rust wasmtime, potentially Python).

### Item 4: CI Enforcement ($15)
**No change from prior surveys.** Depends on items 2 and 3 completing first.

### Item 5: Code Review ($20)
**Changed.** The new commit introduces a code change in `engine/backend_wasmtime.go` that should be included in the review scope. Specifically:
- `json.Marshal` + `fmt.Sprintf` in the WASM dispatch hot path — check error handling and allocation
- The fix uses `escaped, _ := json.Marshal(string(input))` — error is silently discarded

### Item 6: Documentation Audit ($15)
**No change from prior surveys.** Doc files unchanged since Jun 2.

---

## 5. Dispatch Status

**Still zero tasks dispatched.** The 6 leaf tasks (231-236) remain ready:
- Wave 1 (parallel): 231, 232, 233, 235, 236
- Wave 2 (after 232+233): 234

Two prior tasks ready for closure: 228b, 230.

---

## 6. Recommendation

1. **Update survey-output.json** with the new HEAD commit reference (1b7f8ed)
2. **Dispatch now.** The survey loop (6+ surveys, zero dispatches) is the bottleneck. All 6 items are well-understood leaf tasks. The one material change (commit 1b7f8ed) adds context but does not change the decomposition decisions.
3. **Notify agents** working on items 3 and 5 about the new commit's relevance.
4. **Create INDEX.md and tasks.json** as part of dispatch — they are infrastructure prerequisites, not separate tasks.
