# cleat-233em Exploration Report

**Explorer:** cleat-233em
**Date:** 2026-06-05
**Task:** Independent post-233ek exploration of cleat-233e deliverables (LANGUAGE_SUPPORT.md, DX_COMPARISON.md)
**Prior work:** exploration.md (cleat-233ep), review.md (cleat-233er), implementation.md (cleat-233ei), verification.md (cleat-233ev), crosscheck.md (cleat-233ec), exploration-233ek.md (cleat-233ek)

## Scope

Third independent verification that cleat-233e changes are correct and complete. Read both target files in full, cross-referenced all claims against actual source code on disk (Rust SDK crates, Python SDK, WIT file, ABI.md, engine/imports.go).

## Confirmed: Previously Fixed Issues Still Fixed

All 13 changes from cleat-233e remain applied:
- `15 host` — 0 matches in either file ✅
- `22 host` — 0 matches ✅
- `4,508` — 0 matches ✅
- `34 WIT` — 0 matches ✅
- double `end-to-end` — 0 matches ✅
- `537 lines`, `290 lines`, `1,090 lines` — 0 matches ✅

## Status of cleat-233ek's 7 Findings: ALL STILL PRESENT

None of the 7 issues found by cleat-233ek on 2026-06-05 have been addressed.

### Finding 1 (HIGH): Rust "Remaining gaps" in DX_COMPARISON.md — UNCHANGED

**File:** DX_COMPARISON.md, lines 92-95

Current text:
```
**Remaining gaps:**
- No Saga API (all other SDKs have it)
- No `ContinueAsNew` high-level wrapper
- No `ctx.run()` / side-effect wrapper (architectural — shared by all SDKs)
```

All three items are factually wrong (verified today):

1. **"No Saga API"** — `crates/cleat-sdk/src/saga.rs` exists (168 lines). `Saga` and `SagaStep` are public types exported from `lib.rs:39`. Line 338 of DX_COMPARISON.md confirms: "All 4 original gaps closed (K/V state, resolve_promise, test harness, Saga)."

2. **"No ContinueAsNew high-level wrapper"** — `continue_as_new()` at `host_calls.rs:471` and `continue_as_new_versioned()` at `host_calls.rs:1313`. Both take `&self, input_json: &str` returning `Result<(), String>`.

3. **"No ctx.run() / side-effect wrapper"** — `cleat_side_effect` import at `host_calls.rs:281`. Architecture Gaps section (line 343) says "DONE."

The section at lines 92-95 directly contradicts the "202 Issues" summary at line 338.

### Finding 2 (HIGH): Rust SDK line count ~2,011 stale — UNCHANGED

**Files:** LANGUAGE_SUPPORT.md lines 4, 204-205; DX_COMPARISON.md line 82

Documented (LANGUAGE_SUPPORT.md line 4):
```
host_calls 1,519 + memory/lib 69 + proc-macro entry 209 + test_attr 132 + lib 82 = 2,011
```

Actual source files on disk (verified with `wc -l`, non-test, non-bin):

| File | Lines | Doc says |
|------|-------|----------|
| `cleat-sdk/src/host_calls.rs` | 1,520 | 1,519 |
| `cleat-sdk/src/lib.rs` | 69 | 69 |
| `cleat-sdk/src/memory.rs` | 170 | NOT COUNTED |
| `cleat-sdk/src/plugins.rs` | 459 | NOT COUNTED |
| `cleat-sdk/src/saga.rs` | 168 | NOT COUNTED |
| `cleat-sdk/src/version.rs` | 53 | NOT COUNTED |
| `cleat-macro/src/entry.rs` | 209 | 209 |
| `cleat-macro/src/test_attr.rs` | 132 | 132 |
| `cleat-macro/src/lib.rs` | 82 | 82 |
| **Total** | **2,862** | **2,011** |

Four SDK modules (memory.rs, plugins.rs, saga.rs, version.rs) totaling 850 lines are entirely absent from the documented count. The cleat-sdk crate alone is 2,439 lines, not 1,588.

Also: `host_calls.rs` is 1,520 not 1,519 (Finding 6).

### Finding 3 (MEDIUM): Java issue count mismatch — UNCHANGED

**File:** DX_COMPARISON.md

Lines 108-112 list 4 items under **"Remaining critical issues:"**
1. `JsonHelper.parse()` only supports `String.class`
2. `String.replace()` compiles to `Pattern.compile()`, unsupported
3. Multi-project Gradle plugin version conflicts
4. No `fetch_get_json` convenience wrapper

Line 337 (202 Issues summary): **"Java: 2 issues — `JsonHelper` String.class only (TeaVM WASM limitation), Gradle conflicts."**

Items 2 and 4 are in the detailed section but absent from the summary.

### Finding 4 (MEDIUM): AS constraint count mismatch — UNCHANGED

**File:** DX_COMPARISON.md

Lines 128-136 list 6 items under **"Remaining critical constraints:"**
1. No try/catch
2. No closures
3. No async/await
4. No `any` type
5. SUSPEND_SENTINEL bug
6. `@durableEntry` transform partially fixed

Line 336 (202 Issues summary): **"AS: 3 issues — AS runtime limitations (no try/catch, no closures, no async/await)."**

Items 4-6 from the detailed section are absent from the summary.

### Finding 5 (MEDIUM): Python host import count inconsistency — UNCHANGED

- LANGUAGE_SUPPORT.md line 142: "all **~54** host imports are defined"
- DX_COMPARISON.md lines 22, 141, 385: "**49** WIT imports"
- Actual WIT file (`python-sdk/wit/cleat.wit`): **49** function imports (verified)
- Actual ABI.md: **54** host function subsections (verified)

The WIT file has 49 of the 54 ABI host functions. LANGUAGE_SUPPORT.md's "all ~54" is incorrect — it should say "49 of ~54."

### Finding 6 (LOW): host_calls.rs off by 1 — UNCHANGED

LANGUAGE_SUPPORT.md line 204, DX_COMPARISON.md line 82: "1,519" → actual: **1,520**.

### Finding 7 (LOW): Python test count understates total — UNCHANGED

LANGUAGE_SUPPORT.md lines 143, 218; DX_COMPARISON.md line 142: "80 tests pass (61 memory/encoding, 19 entry decorator)."

These are FFI binding tests only. The full Python SDK has 402 test functions across 8 test files (test_entry.py:21, test_host_calls.py:90, test_local_host.py:85, test_memory.py:73, test_test_harness.py:51, test_types.py:12, test_vet.py:57, test_wasm_compilation.py:13). The distinction between "FFI tests" (80) and "full suite" (402) is not made clear.

## NEW FINDINGS: Issues Not Caught by Any Prior Pass

### Finding 8 (MEDIUM): LANGUAGE_SUPPORT.md line 4 "memory/lib 69" label is wrong

Line 4: `host_calls 1,519 + memory/lib 69 + proc-macro entry 209 + test_attr 132 + lib 82`

The label "memory/lib 69" implies both memory.rs and lib.rs total 69 lines. Actual: lib.rs = 69, memory.rs = 170. The label attribute for 69 should be just "lib 69" — memory.rs is entirely unaccounted for.

### Finding 9 (MEDIUM): LANGUAGE_SUPPORT.md line 204 Rust crate line count is stale

Line 204: `cleat-sdk crate (~1,588 lines: host_calls 1,519 + lib 69)`

The cleat-sdk crate actually has 6 source files: host_calls (1,520), lib (69), memory (170), plugins (459), saga (168), version (53) = **2,439 lines**. The documented "~1,588" omits 851 lines across 4 files. This compounds Finding 2.

### Finding 10 (LOW): LANGUAGE_SUPPORT.md Rust section omits key SDK components

The Rust "Already Done" section (lines 202-205) doesn't mention saga.rs (168 lines, pub Saga API), plugins.rs (459 lines, typed plugin wrappers), memory.rs (170 lines), or version.rs (53 lines). These are significant SDK features — especially saga.rs, since DX_COMPARISON.md line 92 incorrectly says the Rust SDK has "No Saga API."

---

## Additional Verification

### ABI.md Host Function Count: 54 ✅

Verified by counting `#### 2.X` subsections with `wc -l`: 54 subsections. LANGUAGE_SUPPORT.md correctly references "~54."

### ABI.md — Missing Exports (not imports)

`cleat_poll_work` and `cleat_complete` are in `engine/imports.go` (lines 856, 878) as WASM **exports** (`.Export()`), not imports. ABI.md documents the import interface. These exports may belong in a separate export specification — the original exploration's claim that they're "missing from ABI.md" may be out of scope.

### Python WIT Import Count: 49 ✅

Independent count from `python-sdk/wit/cleat.wit`: 49 function imports across 18 interfaces. DX_COMPARISON.md correctly reports 49.

### Rust Saga API: Present and Public ✅

`saga` module exported at `lib.rs:9` as `pub mod saga`. `Saga` and `SagaStep` re-exported at `lib.rs:39` as `pub use saga::{Saga, SagaStep}`.

---

## Consolidated Issue List

| # | Severity | Source | File(s) | Issue |
|---|----------|--------|---------|-------|
| 1 | HIGH | 233ek F1 | DX_COMPARISON.md:92-95 | Rust "Remaining gaps" all 3 items stale; contradiction with line 338 |
| 2 | HIGH | 233ek F2 | LANGUAGE_SUPPORT.md:4,204; DX_COMPARISON.md:82 | Rust SDK line count: doc says ~2,011, actual is 2,862 |
| 3 | MEDIUM | 233ek F3 | DX_COMPARISON.md:108-112,337 | Java issues: section lists 4, summary says 2 |
| 4 | MEDIUM | 233ek F4 | DX_COMPARISON.md:128-136,336 | AS constraints: section lists 6, summary says 3 |
| 5 | MEDIUM | 233ek F5 | LANGUAGE_SUPPORT.md:142 | Python host imports: "all ~54" — WIT has only 49 |
| 6 | LOW | 233ek F6 | LANGUAGE_SUPPORT.md:204, DX_COMPARISON.md:82 | host_calls.rs: 1,519 → 1,520 |
| 7 | LOW | 233ek F7 | LANGUAGE_SUPPORT.md:143,218; DX_COMPARISON.md:142 | Python tests: says 80, full suite is 402 |
| 8 | MEDIUM | NEW | LANGUAGE_SUPPORT.md:4 | "memory/lib 69" label wrong — should be "lib 69" (memory.rs is 170, not counted) |
| 9 | MEDIUM | NEW | LANGUAGE_SUPPORT.md:204 | cleat-sdk crate "~1,588 lines" stale — actual is 2,439 |
| 10 | LOW | NEW | LANGUAGE_SUPPORT.md:202-205 | Rust section omits saga.rs, plugins.rs, memory.rs, version.rs |

## Verdict

**Cleat-233e fixed the known stale content, but 10 issues remain.** Seven were found by cleat-233ek and none have been addressed. Three additional issues (8-10) are new — two are compounding findings that deepen the Rust SDK line count problem, and one is about the Rust section's omission of key SDK components.

The most impactful issues are:
1. **DX_COMPARISON.md lines 92-95** (Finding 1) — directly contradicts the 202 Issues summary at line 338 and understates the Rust SDK's capabilities
2. **Rust SDK line count** (Findings 2, 8, 9) — 2,011 should be ~2,862, with proper breakdown

The files are better than before cleat-233e but still not fully accurate. Recommended fix order:
1. Fix DX_COMPARISON.md Rust "Remaining gaps" (Finding 1) — remove stale items, align with line 338
2. Update Rust SDK line count to ~2,862 with correct breakdown (Findings 2, 8, 9) 
3. Fix Java and AS count mismatches in 202 Issues summary (Findings 3, 4)
4. Fix Python host import count (Finding 5)
5. Fix host_calls.rs count (Finding 6) and Python test count (Finding 7)
6. Add saga/plugins/memory/version to Rust section (Finding 10)
