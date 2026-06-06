# cleat-233er Review Report

**Date:** 2026-06-05
**Reviewer:** cleat-233er (review pass on cleat-233ee exploration)
**Prior work:** exploration.md by cleat-233ee, STATUS.md (cleat-233e)

## Scope

Verify the cleat-233ee exploration findings for LANGUAGE_SUPPORT.md and DX_COMPARISON.md against the actual files on disk. Confirm all stale-content claims, line number references, and fix recommendations.

## Verified Claims — LANGUAGE_SUPPORT.md

### Stale Host Function Counts (15 → ~54)

| Line | Current Text | Exploration Claim | Verified |
|------|-------------|-------------------|----------|
| 11 | `Import 15 host functions` | → `~54` | PASS |
| 25 | `declaring the 15 extern imports` | → `~54` | PASS |
| 48 | `auto-generate the 15 import declarations` | → `~54` | PASS |
| 73 | `declaring the 15 native imports` | → `~54` | PASS |

All 4 occurrences verified. Actual count: 54 documented in ABI.md, 56 exported in imports.go (54 ABI + 2 internal Go-wasip1 helpers).

### Stale Host Function Counts (22 → ~54)

| Line | Current Text | Exploration Claim | Verified |
|------|-------------|-------------------|----------|
| 142 | `all 22 host imports are defined` | → `~54` | PASS |

Line 142 is the only "22" reference explicitly called out (item 5). The other "22" occurrences at lines 149 and 269 are covered by the section rewrite (item 6) and recommended-order rewrite respectively.

### Python WASM Section (Lines 149-170, 204-207, Summary Table)

| Claim | Verified |
|-------|----------|
| Lines 149-170: "critical gap" section obsolete (E2E now validated by cleat-233b) | PASS |
| Lines 204-207: Python verdict stale ("7-9 weeks" → "5-6 weeks") | PASS |
| Line 232: Summary table Python row ("WASM FFI wiring (2-3 wks)" → "WASM FFI validated") | PASS |

The exploration correctly identifies that the entire "critical gap" section (lines 149-170) needs rewriting, not just spot fixes. cleat-233b proved Python WASM E2E works.

### Rust SDK Line Counts (Stale)

| Line | Current Text | Actual | Verified |
|------|-------------|--------|----------|
| 5 | `Rust SDK is 537 lines (host_calls 290 + memory 126 + proc-macro 121)` | 2,011 total | PASS |
| 218 | `cleat-sdk crate (290 lines)` | host_calls.rs alone is 1,519 lines | PASS |

Actual source lines: host_calls.rs=1,519, lib.rs=69, entry.rs=209, test_attr.rs=132, lib.rs(macro)=82. Total=2,011.

## Verified Claims — DX_COMPARISON.md

### Double "end-to-end" Typos

| Lines | Claim | Verified |
|-------|-------|----------|
| 23-24 | `validated end-to-end\n   end-to-end.` — remove duplicate | PASS |
| 149-150 | `validated end-to-end\n   end-to-end — no Python workflow has been confirmed running` — contradiction fix | PASS |

Both verified. The line 149-150 case is especially bad: it simultaneously claims validation AND says nothing has been confirmed running.

### Duplicated Java Bullet Points (Lines 116-123)

Verified. Lines 120-123 are an exact duplicate of lines 110-114 (the 4 "Remaining critical issues" bullet points). The TeaVM tree-shaking FIXED paragraph at lines 116-119 was inserted, but the original bullet points below were not removed.

### TeaVM Tree-Shaking Contradiction

| Line | Current | Claim | Verified |
|------|---------|-------|----------|
| 356 | `TeaVM tree-shaking — TeaVM limitation, manual preservedClasses workaround exists` | Contradicts lines 116-119 (FIXED) | PASS |
| 116-119 | `TeaVM tree-shaking — FIXED` | Correct | PASS |

### componentize-py End-to-End Stale

| Line | Current | Claim | Verified |
|------|---------|-------|----------|
| 364 | `componentize-py end-to-end untested (Python)` | Stale (cleat-233b validated) | PASS |

### @durableEntry/TeaVM Confusion

| Line | Current | Claim | Verified |
|------|---------|-------|----------|
| 365 | `@durableEntry tree-shaking by TeaVM (Java — TeaVM limitation)` | Confusing: @durableEntry is AS, not Java | PASS — content verified, though exploration says line 370 (minor off-by-5) |

### Rust SDK Line Count (Stale)

| Line | Current | Actual | Verified |
|------|---------|--------|----------|
| 83 | `The smallest SDK at 1,090 lines` | ~2,011 | PASS |

## Issues MISSED by cleat-233ee Exploration

These are present in the files but not mentioned in the 233ee exploration. They should be addressed during implementation.

### 1. Python SDK Line Count: "4,508" is Stale (HIGH)

Documented as 4,508 lines in 5 locations:

| File | Lines |
|------|-------|
| LANGUAGE_SUPPORT.md | 141, 268 |
| DX_COMPARISON.md | 22, 146, 389 |

Actual: `cleat_sdk/` package is ~13,500 lines (all .py excluding build/tests/examples). All 5 references should be updated.

### 2. WIT Import Count: "34" is Stale (MEDIUM)

Documented as "34 WIT imports" in DX_COMPARISON.md lines 22, 147, 389.

Actual: The WIT file (`python-sdk/wit/cleat.wit`) defines 18 interfaces with 49 function imports. All 3 references should be updated.

### 3. LANGUAGE_SUPPORT.md Line 269: "22 host imports" (LOW)

Line 269: `wiring the 22 host imports via componentize-py's WIT bindings (~2-3 weeks)` — another stale "22" count. Gets partially addressed by the verdict rewrite but should be explicitly fixed.

### 4. LANGUAGE_SUPPORT.md Line 218: Rust SDK "~537 lines total" (LOW)

Line 218: `~537 lines total for the SDK.` — the exploration calls out the "290 lines" but doesn't explicitly mention the "~537 lines total" also needs updating.

## Additional Verification Performed

### ABI.md (from cleat-233ep findings)

The 233ep exploration flagged `cleat_poll_work` and `cleat_complete` as missing from ABI.md. These ARE in imports.go (lines 856, 878) but NOT in ABI.md.

**Assessment:** These are Go wasip1-specific entry-point helpers (not general ABI host imports). The 54 ABI-documented functions cover the public ABI. The omission is likely intentional. The 233ee exploration correctly excluded this from scope.

### "202 Issues" Counts (DX_COMPARISON.md Lines 339-343)

The exploration says these "need verification." Quick check against the text:

| Line | Claim | Assessment |
|------|-------|------------|
| 339 | Go: 2 issues remaining | Internally consistent with text |
| 340 | Python: 0 issues remaining | Questionable — WASM execution only just validated. But text says "All 16 original issues closed" which could be true by their tracking. |
| 341 | AS: 3 issues | Consistent with listed constraints |
| 342 | Java: 2 issues | Consistent with listed issues |
| 343 | Rust: 0 issues remaining | Consistent with text |

These counts are internally consistent with the surrounding text. Verification against an external issue tracker is beyond scope, but no contradictions found within the file.

## Risk Assessment

**Zero risk.** All changes are documentation-only. No code modifications.

**Timing:** All dependency tasks (233a, 233b, 233c, 233d) are complete — no risk of documenting incomplete work.

**Completeness:** The 233ee exploration covers ~85% of issues. My review adds 4 missed items, most importantly the Python SDK line count (4,508 → ~13,500) and WIT import count (34 → 49).

## Verdict

**APPROVE with additions.** The cleat-233ee exploration is accurate and actionable. All claimed issues verified against live files. Add the 4 missed items (above) to the implementation scope.

## Recommended Implementation Order (Updated)

1. LANGUAGE_SUPPORT.md: fix "15" → "~54" (4 locations), "22" → "~54" (line 142 + line 269)
2. LANGUAGE_SUPPORT.md: rewrite Python WASM section (lines 149-170, 204-207, summary table 232)
3. LANGUAGE_SUPPORT.md: update Rust SDK line counts (lines 5, 218)
4. LANGUAGE_SUPPORT.md: update Python SDK line count (lines 141, 268: 4,508 → ~13,500)
5. DX_COMPARISON.md: fix double "end-to-end" (lines 23-24, 149-150) + contradiction
6. DX_COMPARISON.md: remove duplicate Java bullet points (lines 120-123)
7. DX_COMPARISON.md: fix TeaVM tree-shaking contradiction (line 356)
8. DX_COMPARISON.md: fix componentize-py line (line 364)
9. DX_COMPARISON.md: fix @durableEntry/TeaVM confusion (line 365)
10. DX_COMPARISON.md: update Python line count (lines 22, 146, 389: 4,508 → ~13,500)
11. DX_COMPARISON.md: update WIT import count (lines 22, 147, 389: 34 → 49)
12. DX_COMPARISON.md: update Rust SDK line count (line 83: 1,090 → ~2,011)
13. Final grep pass: zero matches for `"15 host"`, `"22 host"`, `"end-to-end\n.*end-to-end"`, `"4,508"`, `"34 WIT"`
