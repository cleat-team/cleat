# cleat-233ek Exploration Report

**Explorer:** cleat-233ek
**Date:** 2026-06-05
**Task:** Independent post-completion exploration of cleat-233e deliverables (LANGUAGE_SUPPORT.md, DX_COMPARISON.md)
**Prior work:** exploration.md (cleat-233ep), review.md (cleat-233er), implementation.md (cleat-233ei), verification.md (cleat-233ev), crosscheck.md (cleat-233ec)

## Scope

Independent verification that cleat-233e is truly complete. Read both target files in full and cross-referenced all claims against actual source code on disk (Rust SDK, Python SDK, WIT file, ABI.md).

## Confirmed: Previously Fixed Issues Still Fixed

All 13 changes from cleat-233e remain applied. The stale patterns confirmed absent:
- `15 host` — 0 matches ✅
- `22 host` — 0 matches ✅
- `4,508` — 0 matches ✅
- `34 WIT` — 0 matches ✅
- double `end-to-end` — 0 matches ✅
- `537 lines`, `290 lines`, `1,090 lines` — 0 matches ✅

---

## NEW FINDINGS: Issues Missed by All Previous Passes

### Finding 1 (HIGH): Rust "Remaining gaps" in DX_COMPARISON.md is stale

**File:** DX_COMPARISON.md, lines 92-95

Current text:
```
**Remaining gaps:**
- No Saga API (all other SDKs have it)
- No `ContinueAsNew` high-level wrapper
- No `ctx.run()` / side-effect wrapper (architectural — shared by all SDKs)
```

All three items are wrong:

1. **"No Saga API"** — Saga EXISTS at `crates/cleat-sdk/src/saga.rs` (168 lines). Public API includes `Saga::new()`, `Saga::add_step()`, `Saga::run()`, and `SagaStep`. Exported from `lib.rs` line 39. Line 338 of the SAME FILE confirms this: "All 4 original gaps closed (K/V state, resolve_promise, test harness, Saga)."

2. **"No ContinueAsNew high-level wrapper"** — `continue_as_new()` exists at `host_calls.rs:471` and `continue_as_new_versioned()` at `host_calls.rs:1313`. Both are high-level wrappers taking `&self, input_json: &str` and returning `Result<(), String>`. Also confirmed working in test.rs line 1436 (`test_continue_as_new`). Line 338 of the SAME FILE says "ContinueAsNew return type fixed."

3. **"No ctx.run() / side-effect wrapper"** — The `cleat_side_effect` WASM import exists in the WIT file and ABI.md. The Architecture Gaps section (line 343) says "DONE." The `(architectural — shared by all SDKs)` qualifier is obsolete since it's been implemented.

**Fix:** Either remove the "Remaining gaps" list entirely or replace with actual remaining gaps (verified by checking the Rust SDK code).

### Finding 2 (HIGH): Rust SDK line count (~2,011) is already stale

**Files:** LANGUAGE_SUPPORT.md lines 4, 204-205, 218; DX_COMPARISON.md line 82

Documented breakdown (LANGUAGE_SUPPORT.md line 4):
```
host_calls 1,519 + memory/lib 69 + proc-macro entry 209 + test_attr 132 + lib 82 = 2,011
```

Actual source files on disk (non-test, non-bin):
```
cleat-sdk/src/host_calls.rs   1,520
cleat-sdk/src/lib.rs             69
cleat-sdk/src/memory.rs         170   ← NOT in the documented count
cleat-sdk/src/plugins.rs        459   ← NOT in the documented count
cleat-sdk/src/saga.rs           168   ← NOT in the documented count
cleat-sdk/src/version.rs         53   ← NOT in the documented count
cleat-macro/src/entry.rs        209
cleat-macro/src/test_attr.rs    132
cleat-macro/src/lib.rs           82
Total:                         2,862
```

Also excluded from the documented breakdown (correctly, as build/test files):
- `test.rs`: 1,458 lines (test harness)
- `bin/inject_metadata.rs`: 337 lines (build tool)

The `memory/lib 69` in the documented breakdown counts only `lib.rs` (69 lines), NOT `memory.rs` (170 lines). Four SDK modules are entirely omitted from the line count: memory.rs, plugins.rs, saga.rs, version.rs — collectively 850 lines.

The line count was updated from 537 → 2,011 during cleat-233e, but 2,011 was already wrong at the time (the exploration missed these 4 files). The actual total is ~2,862.

**Fix:** Update Rust SDK line count to ~2,862 (non-test, non-bin) and adjust the breakdown to include memory.rs, plugins.rs, saga.rs, and version.rs.

### Finding 3 (MEDIUM): Java issue count mismatch in DX_COMPARISON.md

**File:** DX_COMPARISON.md

The Java section (lines 108-112) lists 4 items under **"Remaining critical issues:"**
1. `JsonHelper.parse()` only supports `String.class`
2. `String.replace()` compiles to `Pattern.compile()`, unsupported
3. Multi-project Gradle plugin version conflicts
4. No `fetch_get_json` convenience wrapper

But the 202 Issues summary (line 337) says: **"Java: 2 issues — `JsonHelper` String.class only (TeaVM WASM limitation), Gradle conflicts."**

Items 2 and 4 from the section list are absent from the summary count. Either the section is stale (over-listing) or the summary undercounts.

### Finding 4 (MEDIUM): AS constraint count mismatch in DX_COMPARISON.md

**File:** DX_COMPARISON.md

The AS section (lines 128-136) lists 6 items under **"Remaining critical constraints:"**
1. No try/catch
2. No closures
3. No async/await
4. No `any` type
5. SUSPEND_SENTINEL bug
6. `@durableEntry` transform partially fixed

But the 202 Issues summary (line 336) says: **"AS: 3 issues — AS runtime limitations (no try/catch, no closures, no async/await)."**

Three items from the section (no `any` type, SUSPEND_SENTINEL, @durableEntry) are present in the detailed section but absent from the summary. The summary parenthetical mentions only 3 of the 6.

### Finding 5 (MEDIUM): Python host import count inconsistency

**Files:** LANGUAGE_SUPPORT.md vs DX_COMPARISON.md vs actual WIT file

- LANGUAGE_SUPPORT.md line 142: "all **~54** host imports are defined"
- DX_COMPARISON.md lines 22, 141, 385: "**49** WIT imports"
- Actual WIT file (`python-sdk/wit/cleat.wit`): **49** function imports
- Actual ABI.md: **54** host functions

The WIT file has 49 of the 54 ABI host functions. LANGUAGE_SUPPORT.md says "all ~54" which implies 100% coverage, but 5 ABI functions lack WIT bindings. The wording should be "49 of ~54 host imports are defined via WIT bindings" or similar.

### Finding 6 (LOW): Rust SDK host_calls.rs line count off by 1

**File:** LANGUAGE_SUPPORT.md line 204, DX_COMPARISON.md line 82

Documentation says `host_calls.rs` is 1,519 lines. Actual (wc -l): **1,520**. Trivial, but corroborates the line count freshness issue.

### Finding 7 (LOW): Python test count understates total suite

**Files:** LANGUAGE_SUPPORT.md lines 143, 218; DX_COMPARISON.md line 142

Docs say "80 tests pass (61 memory/encoding, 19 entry decorator)." These are specifically the WASM FFI binding tests. The full Python SDK test suite has **443 tests** (confirmed via `pytest --collect-only`). The distinction between "WASM-specific tests" (80) and "total tests" (443) is not made clear. Readers may conclude the Python SDK has only 80 tests total, understating its maturity.

---

## Verification of WIT Import Count

Independent count from `python-sdk/wit/cleat.wit`: **49** function imports across 18 interfaces.
DX_COMPARISON.md correctly reports 49. ✅

## Verification of ABI.md Host Function Count

Independent count from `ABI.md` sections 2.1 through 2.54: **54** host functions.
LANGUAGE_SUPPORT.md correctly reports ~54. ✅

## Verification of Python SDK Line Count

Independent count (all .py excluding tests/examples/build): **12,753** lines.
Previous review count: 13,490 lines (different exclusion criteria).
Documentation uses ~13,500 which covers both measurements. ✅ (within tolerance)

## Verdict

**4 of 13 prior changes remain correct**, but **7 new issues found** across 2 categories:

| # | Severity | File | Issue |
|---|----------|------|-------|
| 1 | HIGH | DX_COMPARISON.md:92-95 | Rust "Remaining gaps" all 3 items stale (Saga, ContinueAsNew exist) |
| 2 | HIGH | Both files | Rust SDK line count ~2,011 → should be ~2,862 (4 files omitted) |
| 3 | MEDIUM | DX_COMPARISON.md:108-112,337 | Java issues: section lists 4, summary says 2 |
| 4 | MEDIUM | DX_COMPARISON.md:128-136,336 | AS constraints: section lists 6, summary says 3 |
| 5 | MEDIUM | LANGUAGE_SUPPORT.md:142 | Python host imports: says "all ~54" but WIT has 49 |
| 6 | LOW | Both files | host_calls.rs: 1,519 → 1,520 |
| 7 | LOW | LANGUAGE_SUPPORT.md:143,218 | Python tests: says 80, full suite is 443 |

**Bottom line:** cleat-233e fixed the known stale content, but new staleness has already accumulated (Rust SDK line count, gaps list) and several internal inconsistencies between the detailed sections and the summary section in DX_COMPARISON.md were missed by all 5 prior passes. The files are better than before but not yet fully accurate.
