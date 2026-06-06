# cleat-233ee Exploration: Documentation Update for cleat-233e

**Explorer:** cleat-233ee
**Date:** 2026-06-05
**Task:** Explore LANGUAGE_SUPPORT.md and DX_COMPARISON.md for stale/incorrect content, given the completed status of dependency tasks (cleat-233a, 233b, 233c, 233d).

## Dependency Status Summary

| Task | Status | Key Result |
|------|--------|------------|
| cleat-233a | Done | AS tests: 106/106 PASS (binaryen fix + memory size fix) |
| cleat-233b | Done | Python WASM E2E: validated (hello_workflow → WASM → worker execution) |
| cleat-233c | Done | Rust WASM integration: 4 E2E tests PASS |
| cleat-233d | Done | ABI.md: 54 host functions documented (cleat_poll_child, cleat_await_any_child added) |

## Changes Required: LANGUAGE_SUPPORT.md

### 1. Line 11: Stale host function count
**Current:** `3. Import 15 host functions from the "env" module with (ptr, len) string protocol`
**Fix:** `3. Import ~54 host functions from the "env" module with (ptr, len) string protocol`
**Why:** ABI.md now documents 54 host functions.

### 2. Around line 25 (C section): Stale count
**Current:** `A cleat.h header declaring the 15 extern imports`
**Fix:** `A cleat.h header declaring the ~54 extern imports`

### 3. Around line 48 (C/Zig section): Stale count
**Current:** `Zig's comptime could auto-generate the 15 import declarations`
**Fix:** `Zig's comptime could auto-generate the ~54 import declarations`

### 4. Around line 74 (Java section): Stale count
**Current:** `HostCalls class declaring the 15 native imports via TeaVM's @Import annotations`
**Fix:** `HostCalls class declaring the ~54 native imports via TeaVM's @Import annotations`

### 5. Line 142: Stale count
**Current:** `all 22 host imports are defined, the @cleat_entry decorator generates WASM export wrappers`
**Fix:** `all ~54 host imports are defined, the @cleat_entry decorator generates WASM export wrappers`

### 6. Lines 149-170: Python WASM E2E now validated — section mostly OBSOLETE
**Current:** The "critical gap" section describes `_import_*` stubs raising `NotImplementedError` and declares the WASM pipeline "never been validated end-to-end." Phase 1 (End-to-End WASM Compilation, P0, ~2-3 weeks) is described as still needed.

**Reality:** cleat-233b proved Python WASM E2E works. `durable_call_workflow.py` compiled via componentize-py 0.23.0, loaded and executed in wasmtime backend, produced valid history events. 54 imports verified aligned in `TestPythonWasmAbiBoundary`.

**Fix:** Rewrite this section to reflect current reality:
- The WASM gap is closed: E2E validation passes
- Phase 1 is complete (steps 1-4 done)
- Phases 2-4 should be recategorized (Phase 2 becomes new Phase 1, etc.)
- Binary size note: actual WASM is 18.38 MB (documented as 5-20 MB — still accurate)

### 7. Lines 204-207: Python verdict stale
**Current:** `The remaining work is the WASM FFI wiring (~2-3 weeks) plus feature parity and ecosystem integration (~5-6 weeks). Total: ~7-9 weeks`
**Fix:** WASM FFI wiring is now validated. Remaining is feature parity + ecosystem (~5-6 weeks). Total: ~5-6 weeks.

### 8. Summary Table (line 232): Python row
**Current:** `Python | componentize-py | ✅ Done (4.5K lines, 80 tests) | ✅ Done (@cleat_entry) | 5-20 MB | WASM FFI wiring (2-3 wks) | 5th`
**Fix:** Showstopper column should reflect WASM FFI validated. E.g., `WASM FFI validated (E2E working)`.

## Changes Required: DX_COMPARISON.md

### 1. Lines 23-24: Double "end-to-end" typo
**Current:**
```
23	   integration) and `componentize-py` WASM compilation has been validated end-to-end
24	   end-to-end.
```
**Fix:** Remove duplicate "end-to-end":
```
integration) and `componentize-py` WASM compilation has been validated end-to-end.
```

### 2. Lines 149-150: Double "end-to-end" typo + INTERNAL CONTRADICTION
**Current:**
```
149	   the `componentize-py` WASM compilation pipeline has been validated end-to-end
150	   end-to-end — no Python workflow has been confirmed running in a cleat worker.
```
**Problem:** This is contradictory. It says "has been validated end-to-end" AND "no Python workflow has been confirmed running." The "end-to-end" was likely inserted in a prior edit but the negation wasn't removed.

**Reality:** cleat-233b confirmed Python workflow runs in cleat worker. Both halves of the sentence are wrong in different ways.

**Fix:**
```
the `componentize-py` WASM compilation pipeline has been validated end-to-end — Python
workflows compile and execute successfully in a cleat worker via the wasmtime backend.
```

### 3. Lines 116-123: DUPLICATED bullet points in Java section
**Problem:** Lines 110-114 list remaining critical Java issues. Lines 116-119 insert the TeaVM tree-shaking FIXED paragraph. Lines 120-123 then DUPLICATE the exact same 4 bullet points from lines 110-114.

**Fix:** Remove lines 120-123 (the duplicate bullet points after the TeaVM fix paragraph).

### 4. Line 356: TeaVM tree-shaking marked as limitation (contradicts line 116-119)
**Current:** `TeaVM tree-shaking — TeaVM limitation, manual preservedClasses workaround exists`
**Reality:** Lines 116-119 already document the FIX via WorkflowEntry reference chain.
**Fix:** `TeaVM tree-shaking — FIXED via WorkflowEntry reference chain`

### 5. Line 364: componentize-py end-to-end stale
**Current:** `componentize-py end-to-end untested (Python)`
**Reality:** Validated by cleat-233b.
**Fix:** Remove this line entirely, or update to `componentize-py end-to-end validated (Python)`.

### 6. Line 370: `@durableEntry` tree-shaking mentions TeaVM
**Current:** `@durableEntry tree-shaking by TeaVM (Java — TeaVM limitation)`
**Note:** `@durableEntry` is an AssemblyScript concept, not Java. This line is confusing — might mean `@CleatEntry` for Java. But TeaVM tree-shaking is already marked FIXED at line 116-119. Consider removing or clarifying.

## Additional Issues Found (Beyond PLAN Scope)

### LANGUAGE_SUPPORT.md
- **AssemblyScript section (implied):** The doc describes AS as "Severely Constrained" with no mention that all 106 AS tests now pass (cleat-233a fix). Since this doc is about language *viability* not test status, this may be acceptable — the AS runtime constraints (no try/catch, no closures) are still real. But the "3 remaining issues" count in DX_COMPARISON.md line 341 is accurate.

- **Rust section (line 218):** Says "Already Done" — accurate, but could mention the 4 WASM integration tests now pass (cleat-233c). Minor.

### DX_COMPARISON.md
- **Line 16 (ABI.md fix):** The `cleat_await_child` function referenced — this was renamed to `cleat_await_any_child` during cleat-233d. Should be verified.
- **Lines 357-358:** Minor dup of "Remaining critical issues" content.

## Recommended Execution Order

1. Fix LANGUAGE_SUPPORT.md (5 min): stale host function counts (5 locations) + Python WASM section rewrite
2. Fix DX_COMPARISON.md (10 min): double "end-to-end" (2 locations) + contradiction fix + dedup + stale line removal
3. Verify changes: grep for "15 host", "22 host", "end-to-end end-to-end" — should return zero matches

## Risk Assessment

- **Zero risk:** Documentation-only changes, no code modifications
- **Timing:** All dependency tasks (233a, 233b, 233c, 233d) are complete — no stale-data race
- **Verification:** Easy to verify by grep patterns listed above
