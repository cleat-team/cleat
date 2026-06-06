# cleat-233e Exploration Report

**Explorer:** cleat-233ep
**Date:** 2026-06-05
**Task:** Update LANGUAGE_SUPPORT.md and DX_COMPARISON.md

## Dependent Task Status

| Task | Status | Impact on 233e |
|------|--------|---------------|
| cleat-233a (AS tests) | COMPLETED — 106/106 pass | LANGUAGE_SUPPORT.md can reflect this |
| cleat-233b (Python WASM E2E) | NOT STARTED (empty artifacts) | BLOCKER — need outcome for Python status |
| cleat-233c (Rust WASM integration) | NOT STARTED (empty artifacts) | BLOCKER — need outcome for Rust status |
| cleat-233d (ABI.md fix) | NOT STARTED (empty artifacts) | Minor — ABI.md already has poll_child & await_any_child |

**Recommendation**: cleat-233e cannot be fully completed until 233b and 233c report outcomes. However, the known corrections listed below can be applied immediately, and the remaining Python/Rust status can be updated after dependent tasks complete.

---

## LANGUAGE_SUPPORT.md — Issues Found

### 1. "15 host functions" / "15 imports" — stale count (6 occurrences)

The ABI now defines 54 host functions in ABI.md; `engine/imports.go` exports 56. All "15" references are from ABI v1 (May 2025).

| Line | Current Text | Fix |
|------|-------------|-----|
| 11 | `Import 15 host functions` | `Import ~50 host functions` |
| 25 | `declaring the 15 extern imports` | `declaring the ~50 extern imports` |
| 49 | `auto-generate the 15 import declarations` | `auto-generate the ~50 import declarations` |
| 73 | `declaring the 15 native imports` | `declaring the ~50 native imports` |

### 2. "22 host imports" — stale count (3 occurrences)

The Python section references "22 host imports" — this was the ABI v2 count. Now it's ~50+.

| Line | Current Text | Fix |
|------|-------------|-----|
| 141 | `all 22 host imports are defined` | `all ~50 host imports are defined` |
| 161 | `Wire the 22 host imports` | `Wire the ~50 host imports` |
| 269 | `wiring the 22 host imports via` | `wiring the ~50 host imports via` |

### 3. Rust SDK line count — stale

**Line 5**: `Rust SDK is 537 lines (host_calls 290 + memory 126 + proc-macro 121)`

Actual source lines (excluding build artifacts):
- `crates/cleat-sdk/src/host_calls.rs`: 1,519 lines
- `crates/cleat-sdk/src/lib.rs`: 69 lines
- `crates/cleat-macro/src/entry.rs`: 209 lines
- `crates/cleat-macro/src/test_attr.rs`: 132 lines
- `crates/cleat-macro/src/lib.rs`: 82 lines
- **Total**: ~2,011 lines

**Line 218**: `cleat-sdk crate (290 lines)` — actually 1,519 lines for host_calls.rs alone.

**Fix**: Update Rust SDK line counts to reflect the SDK hardening pass.

### 4. Python "148" line references (lines 148 & 162)

Line 148 references "The 22 `_import_*` functions in `host_calls.py`" — the number 22 is stale. Fix: "The ~50 `_import_*` functions".

### 5. Python section needs update for cleat-233a outcome

The AssemblyScript verdict at line 103-110 says AS is "only TypeScript-flavored" with various constraints. With cleat-233a completed (all 106 AS tests pass, 0.27.32, ready for CI), the description should reflect the current working state while noting the language subset limitations are real but surmountable.

---

## DX_COMPARISON.md — Issues Found

### 1. Double "end-to-end" (2 occurrences)

| Lines | Current Text |
|-------|-------------|
| 25-26 | `validated end-to-end\n   end-to-end.` |
| 149-150 | `validated end-to-end\n   end-to-end — no Python workflow` |

**Fix**: Remove the doubled "end-to-end" on the second line in both cases.

### 2. Python WASM status contradiction

- **Line 22-26**: Says WASM compilation "has been validated end-to-end" (positive)
- **Line 149-150**: Says pipeline "has been validated end-to-end — no Python workflow has been confirmed running in a cleat worker" (contradiction in same sentence)
- **Line 340**: "WASM compilation validated end-to-end (hello_workflow.py -> 19.2 MB WASM binary)" (positive, specific)
- **Line 364**: "`componentize-py` end-to-end untested (Python)" (says NOT tested)

These four statements all describe the same thing differently. The truth: `componentize-py` CAN produce valid WASM binaries (compilation works), but no Python workflow has been loaded and executed in a real cleat worker (execution untested). This is the cleat-233b scope. All four references should converge on one consistent description.

**Recommendation**: Unify to a single clear statement: "`componentize-py` WASM compilation validated (produces valid .wasm binaries). Runtime execution against a cleat worker NOT YET validated — tracked in cleat-233b."

### 3. Rust SDK line count — stale

**Line 83**: `The smallest SDK at 1,090 lines (host_calls expanded from 537 lines after SDK hardening pass)`

Actual source: ~2,011 lines. **Fix**: Update to current count.

### 4. Python line count / WIT imports count

**Line 24**: `4,508 lines, 34 WIT imports` — this may be stale. Should be verified against current python-sdk/.
**Lines 147, 389**: Same counts repeated.

**Line 147**: `34 WIT imports defined` — verify against current WIT file.

### 5. "202 issues" claims need verification

**Line 339**: "Go: 2 issues remaining" — verify current count.
**Line 340**: "Python: 0 issues remaining" — verify (especially given 233b not done).
**Line 341**: "AS: 3 issues" — verify after cleat-233a fix.
**Line 342**: "Java: 2 issues" — verify.
**Line 343**: "Rust: 0 issues remaining" — verify.

---

## ABI.md — Additional Finding

`cleat-233d` was created to add `cleat_poll_child` and `cleat_await_any_child` to ABI.md. Both are already present (sections 2.24 and 2.25). The primary task scope is already satisfied by the current file.

However, two functions in `engine/imports.go` are MISSING from ABI.md:
- `cleat_poll_work` (line 856 in imports.go)
- `cleat_complete` (line 878 in imports.go)

These should be added to ABI.md either as part of cleat-233d or as a follow-up.

---

## Recommended Execution Order

1. **Apply known fixes now** (independent of 233b/233c):
   - LANGUAGE_SUPPORT.md: fix all "15" -> "~50" and "22" -> "~50" references (9 occurrences)
   - LANGUAGE_SUPPORT.md: update Rust SDK line counts
   - DX_COMPARISON.md: fix double "end-to-end" (2 occurrences)
   - DX_COMPARISON.md: unify Python WASM status to one consistent statement

2. **After cleat-233b completes**: Update Python status in both files.
3. **After cleat-233c completes**: Update Rust status in LANGUAGE_SUPPORT.md.
4. **Final pass**: Verify all line counts, issue counts, and test statuses against current code.
