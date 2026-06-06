# cleat-233b Code Review

**Reviewer:** cleat-233br
**Date:** 2026-06-05
**Status:** APPROVED — all changes correct, no issues found

## Changes Reviewed

Three files modified (all uncommitted in working tree):

### 1. `.github/workflows/e2e-cross-language.yml:101` — CI path fix

- `./internal/host/...` → `./engine/...`
- **Verified:** `internal/host/` does not exist on disk. `engine/` contains `python_wasm_e2e_test.go` and `rust_workflow_test.go`.
- **Verdict:** CORRECT

### 2. `engine/python_wasm_e2e_test.go` — Import list additions

9 imports added to both `pythonExpectedImports` (line 171-214) and `registeredImportNames()` (line 541-586):

| Import | registered in imports.go |
|---|---|
| `cleat_continue_as_new_versioned` | line 220 |
| `cleat_child_workflow_in_schema` | line 288 |
| `cleat_set_scope` | line 568 |
| `cleat_get_scope` | line 575 |
| `cleat_uuid` | line 587 |
| `cleat_poll_child` | line 402 |
| `cleat_await_any_child` | line 414 |
| `cleat_json_parse` | line 842 |
| `cleat_json_stringify` | line 848 |

- Both lists have 54 entries (the STATUS.md says "55 imports aligned" — this is a minor documentation miscount; the test passes, confirming all entries match).
- Every entry in `pythonExpectedImports` has a corresponding entry in `registeredImportNames()`, and every entry in `registeredImportNames()` is registered via `.Export()` in `engine/imports.go`.
- **Verdict:** CORRECT

### 3. `ABI.md` — Poll/await child + section renumbering (from cleat-233d)

- `cleat_poll_child` added at section 2.24: 4-param signature (run_id_ptr, run_id_len, result_ptr, result_max_len) → i64, correct return packing (errCode | resultLen).
- `cleat_await_any_child` added at section 2.25: 4-param signature (run_ids_ptr, run_ids_len, result_ptr, result_max_len) → i64, correct return packing (errCode | resultLen).
- Signatures verified against `engine/imports.go` — match exactly.
- Section renumbering: 2.24→2.26 through 2.52→2.54, all 30 subsequent sections correctly shifted by +2.
- Cross-reference updates verified:
  - Changelog v4 entry: 2.51→2.53, 2.52→2.54
  - NaN canonicalization (§6.1): 2.52→2.54
  - JSON canonicalization (§6.3): 2.52→2.54
  - RNG / cleat_uuid (§6.4): 2.40→2.42
  - SDK compliance matrix (§6.6): 2.51→2.53, 2.52→2.54
- ChildWorkflowTyped documentation added at §2.20 — accurate description, non-breaking addition.
- **Verdict:** CORRECT

## Cross-Reference Consistency

| Check | Result |
|---|---|
| `pythonExpectedImports` ⊆ `registeredImportNames()` | PASS (54/54) |
| `registeredImportNames()` ⊆ `engine/imports.go` exports | PASS (54/54) |
| ABI.md §2.24 signature = imports.go cleat_poll_child | PASS |
| ABI.md §2.25 signature = imports.go cleat_await_any_child | PASS |
| ABI.md renumbering (2.24-2.54) no gaps | PASS |
| ABI.md cross-references updated | PASS |
| CI path fix correct | PASS |
| No regressions (all E2E tests pass) | PASS |

## Recommendation

**APPROVE.** All changes are correct, internally consistent, and verified against the source of truth (`engine/imports.go`). Ready for commit. No blocking issues.

## Minor Note

The STATUS.md and exploration.md report "55 imports aligned" but the actual count is 54 entries in both lists. This is a documentation miscount in the status/exploration files — it does not affect code correctness. Can be corrected in cleat-233e (docs update task).
