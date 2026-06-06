# Exploration: cto-lap-030-001s — cleat 0.5 Trial Release Hardening

**Explorer:** cto-lap-030-001s (claude explorer agent)
**Date:** 2026-06-05
**CEO Guidance:** `task_state/CEO-GUIDANCE.md` (2026-06-04, $100 budget, 6 items)
**Prior surveys:** Lap 90, cto-lap-017, cto-lap-019, cto-lap-021, cto-lap-031 (2026-06-04), cto-lap-survey (2026-06-05)

---

## 1. Material Changes Since Prior Surveys

### Git HEAD

HEAD is at `1b7f8ed` — same as cto-lap-survey. No new commits since then.

```
1b7f8ed fix: wrap workflow input in DispatchWrapper for WASM entry point
```

### Uncommitted changes

Unchanged from all prior surveys:
- `auth/middleware.go` — SPA asset path bypass
- `web/src/*` — admin dashboard Svelte 5 updates
- `cmd/cleat-worker/web/dist/*` — rebuilt dist assets
- `task_state/*` — CTO process files

**Verdict: No material change since cto-lap-survey.** The one change it found (commit 1b7f8ed) remains the only change since CEO guidance.

---

## 2. Verified Claims from Prior Surveys

### Confirmed (verified against current code)

| Claim | Source | Verification |
|-------|--------|-------------|
| `auth/tenant_store.go` has 3 PostgreSQL-only queries | Lap 90 exploration | **Confirmed.** Lines 29, 39, 47 still use `admin.` schema, `$N` placeholders, `RETURNING` |
| No task directories for cleat-231..236 | All surveys | **Confirmed.** Zero dispatches across 7+ surveys |
| INDEX.md missing | All surveys | **Confirmed.** `task_state/INDEX.md` does not exist |
| tasks.json missing | All surveys | **Confirmed.** No machine-readable task registry |
| CI has 12 `continue-on-error: true` | Lap 90 exploration | **Confirmed.** Across ci.yml (9), ecosystem-ci.yml (1), ai-pr-review.yml (1), release-notes-check.yml (1) |
| CHANGELOG.md thin, no 0.5.0 entry | Lap 90 exploration | **Confirmed.** Only boilerplate [Unreleased] and [0.1.0] sections, 53 lines |
| SECURITY.md missing signal auth section | Lap 90 exploration | **Confirmed.** Signal auth not mentioned anywhere in SECURITY.md |
| SECURITY.md missing encryption-at-rest section | Lap 90 exploration | **Confirmed.** Encryption not mentioned beyond event history checksums |
| "15 host imports" stale in docs | Lap 90 exploration | **Confirmed.** Found in SECURITY.md:143, LANGUAGE_SUPPORT.md:11, docs/explanation/architecture.md:119, ABI.md:1398, packages/cleat-as/assembly/index.ts:28, packages/cleat-as/assembly/memory.ts:7 |
| ARCHITECTURE.md missing ChildWorkflow API docs | Lap 90 exploration | **Confirmed.** "ChildWorkflow" not found anywhere in ARCHITECTURE.md |
| ARCHITECTURE.md missing signal auth section | Lap 90 exploration | **Confirmed.** |
| ABI.md missing `cleat_poll_child`, `cleat_await_any_child` | Lap 90 exploration | **Confirmed.** Neither appears in ABI.md. `cleat_child_workflow_with_options` IS documented at §2.20 |
| `LongRunning` in testdata but not in test expectations | Lap 90 exploration | **Confirmed.** `testdata/basic/order.go:175` calls `h.DurableCall()` in a loop. `internal/closure/closure_test.go:31-41` expects 8 leaves — `LongRunning` is missing |
| ARCHITECTURE.md module paths stale | cto-lap-survey | **Confirmed.** References `internal/host/`, `internal/wasm/`, etc. but these are now at `engine/`, `cleat/`, `auth/` (refactor commit `3eeb74e`) |

### NEW finding: Package structure refactor not reflected in ARCHITECTURE.md

Commit `3eeb74e` ("refactor: promote internal packages to public — engine as a library") moved major packages out of `internal/`:

| Old path (ARCHITECTURE.md) | New actual path |
|---------------------------|-----------------|
| `internal/host/` | `engine/` |
| `internal/wasm/` | `cleat/` (wasm functionality absorbed) |
| `internal/wasmrw/` | Removed or merged |
| `internal/migration/` | Moved (not in `internal/`) |
| `internal/auth/` | `auth/` (root level) |
| `internal/plugin/` | `cleat/plugin.go` (public) |

Current `internal/` packages: only `analyzer`, `callgraph`, `closure`, `plugingen`, `telemetry`, `transform`.

The coupling matrix in ARCHITECTURE.md still references `internal/host`, `internal/wasm`, etc. — all stale.

### NEW finding: Actual host function count is ~69, not 50

Prior surveys said "50 host function imports (ABI v5)." A grep for unique `cleat_*` import names across `wasm/`, `cleat/`, and `engine/` found **69** unique host function names. The stale "15" in 6 doc files is even more wrong than previously reported. The "50" figure in the Lap 90 exploration is also an undercount.

### NEW finding: Closure test failure is real and reproducible from code inspection

`testdata/basic/order.go` defines 9 functions calling `h.DurableCall()`:
1. `checkItemAvailability`
2. `getDefaultPaymentMethod`
3. `fulfillOrder`
4. `reserveInventory`
5. `chargeCustomer`
6. `releaseReservation`
7. `refundPayment`
8. `notifyCustomer`
9. **`LongRunning`** — NOT in test expectations

`closure_test.go` expects exactly 8 leaves. The test `TestComputeBasicIdentifiesDurableLeaves` will fail because the closure analysis will find 9 leaves but the test expects 8. The `TestComputeBasicCorrectlyTagsPureFunctions` failure is a secondary effect.

### Unconfirmed

| Claim | Reason |
|-------|--------|
| MySQL/MSSQL CI jobs actively failing | `go` not available in this environment. Code inspection confirms the root cause (PostgreSQL-only queries), but cannot verify current CI state |
| Python WASM E2E never validated | Cannot run componentize-py toolchain |
| Branch protection rules require updates | Requires GitHub admin access |
| test-tinygo can be re-enabled | Cannot run Go 1.26 tests |

---

## 3. Updated Per-Item Assessment

### Item 1: ChildWorkflow API Cleanup ($15)
**No change.** The exploration remains accurate. 3 APIs, `ChildWorkflowWithOptions` under-used, localdev not wired, no ARCHITECTURE.md/ABI.md coverage. New finding: ARCHITECTURE.md module paths are stale on top of missing API docs — fixing both in the same pass would be efficient.

### Item 2: Multi-DB Test Fixes ($20)
**No change.** Root cause confirmed. 3 PostgreSQL-only queries in `auth/tenant_store.go`. Migration gap (MySQL missing 008, 009). CI env vars not set. New finding: the package refactor (`internal/auth/` → `auth/`) means the file location changed but the queries didn't — the fix is the same.

### Item 3: SDK Test Passes ($15)
**Subtle change.** Commit 1b7f8ed fixes WASM input dispatch — this is the path SDK tests exercise. The fix may resolve some existing failures. But error handling in the fix (`escaped, _ := json.Marshal(string(input))` — error silently discarded) should be reviewed. New finding: 69 actual host imports vs. 50 claimed — AssemblyScript stub gap is larger than previously reported.

### Item 4: CI Enforcement ($15)
**No change.** Depends on items 2+3. 12 `continue-on-error: true` confirmed. Closure test failure confirmed from code inspection.

### Item 5: Code Review ($20)
**Minor change.** Commit 1b7f8ed adds code to review in `engine/backend_wasmtime.go`. Specifically: `json.Marshal` error silently discarded, `fmt.Sprintf` in WASM hot path. New finding: ARCHITECTURE.md module paths are stale — review scope should include updating these.

### Item 6: Documentation Audit ($15)
**Scope expanded.** New finding: 69 host imports (not 50), "15" stale in 6 files (not 3). ARCHITECTURE.md has stale module paths from the `internal/` → `public/` refactor — this is now a high-priority doc fix beyond what earlier surveys captured.

---

## 4. Dispatch Status

**Still zero tasks dispatched.** 7+ surveys, no leaf tasks launched. The 6 tasks (231-236) remain ready:
- Wave 1 (parallel): 231, 232, 233, 235, 236
- Wave 2 (after 232+233): 234

Prior tasks for closure: 228b, 230.

---

## 5. Updated Risk Assessment

| Risk | Prior | Updated | Reason |
|------|-------|---------|--------|
| Python WASM E2E unknown | Medium-High | Medium-High | Still never validated |
| Closure test fix complexity | Low (5-line fix) | Low-Medium | `LongRunning` is calling `DurableCall` — adding it to expected leaves is still ~5 lines, but may also need `TestComputeBasicCorrectlyTagsPureFunctions` fix |
| ARCHITECTURE.md staleness | Medium | **High** | Module paths from `internal/` refactor are wrong. Any agent reading ARCHITECTURE.md will navigate to wrong paths. This is a blocker for dispatch — fix before launching leaf tasks. |
| Host import count wrong in docs | Low | Medium | "15" is wrong by 4.6x (69 actual). 6 files affected, not 3. SDK developers reading stale docs will have wrong mental model of ABI surface. |
| Dispatch bottleneck | High | **Critical** | 7+ surveys, zero dispatches. The 6 items are well-understood. The CTO must dispatch or the CEO must intervene. |

---

## 6. Recommendation

### Immediate actions

1. **Fix ARCHITECTURE.md module paths before dispatching leaf tasks.** Agents reading stale paths will waste time and make errors. This is a ~15-minute fix: update the Module Boundaries table and Coupling Matrix with current paths.

2. **Dispatch leaf tasks 231-236 now.** All 6 items are leaf-ready. The exploration findings add detail but don't change the decomposition decisions. The only blocker is the dispatch mechanism itself.

3. **Update the "15 host imports" claim to "69" in all affected docs.** This is broader than prior surveys captured (6 files, not 3).

4. **Close tasks 228b and 230.** Both superseded by the new CEO guidance.

### Leaf task refinements

- **cleat-231** (ChildWorkflow API): Add to scope: fix ARCHITECTURE.md module paths in the same pass
- **cleat-232** (Multi-DB): Note that `auth/tenant_store.go` is now at root `auth/`, not `internal/auth/`
- **cleat-233** (SDK tests): Include review of the new WASM input dispatch code (commit 1b7f8ed) for error handling
- **cleat-235** (Code review): Add `engine/backend_wasmtime.go` WASM dispatch hot path to review scope
- **cleat-236** (Documentation): Add ARCHITECTURE.md module path fix and "15→69" host import count to scope

### Escalation

The dispatch bottleneck (7+ surveys, zero tasks launched) is now **critical**. The CTO agent protocol says the clew-cto-lap workflow handles automatic dispatch — but it hasn't triggered across 7 survey laps. Recommend the CEO either:
- Manually dispatch the 6 leaf tasks
- Fix the dispatch pipeline (clew-cto-lap workflow)
- Authorize the CTO to launch leaf tasks directly without the workflow
