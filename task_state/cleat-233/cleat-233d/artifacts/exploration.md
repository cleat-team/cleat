# cleat-233d Exploration Report

**Date:** 2026-06-05
**Explorer:** cleat-233dv
**Task:** Fix ABI.md omissions (cleat_poll_child, cleat_await_any_child)
**Status from STATUS.md:** done (fix already in working tree)
**Prior review:** cleat-233dr — PASS

## 1. What's here now?

### The fix (already applied, uncommitted)

ABI.md has been updated with two new sections:

- **2.24 `cleat_poll_child`** (lines 636-660): Non-blocking poll, signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"status":"running|completed|failed","result":"...","error":"..."}`.
- **2.25 `cleat_await_any_child`** (lines 662-686): Blocking await for any child, signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"run_id":"...","result":"...","error":"..."}`. Documents deterministic sorted-order polling and suspend semantics.

All subsequent sections (old 2.24 cleat_run_detached through old 2.52 cleat_json_stringify) renumbered upward by 2.

### Triple-source cross-reference (independently verified)

| Source | Count | Excludes | Status |
|---|---|---|---|
| ABI.md sections 2.1–2.54 | 54 | — | Complete |
| engine/imports.go .Export() calls | 56 (54 host + 2 internal) | cleat_poll_work, cleat_complete | Complete |
| crates/cleat-sdk/src/host_calls.rs extern "C" | 54 host imports | Rust-side wrappers | Complete |

All three sources enumerate the same 54 host functions. cleat_poll_child and cleat_await_any_child are present in all three with matching signatures.

### Cross-reference updates verified

| Location | Old Ref | New Ref | Fixed |
|---|---|---|---|
| Changelog v4 entry | 2.51/2.52 | 2.53/2.54 | Yes |
| Section 6.1 NaN canonicalization | ABI 2.52 | ABI 2.54 | Yes |
| Section 6.3 JSON output | ABI 2.52 | ABI 2.54 | Yes |
| Section 6.5 UUID reference | ABI 2.40 | ABI 2.42 | Yes |
| Section 6.6 Compliance matrix | 2.51/2.52 | 2.53/2.54 | Yes |

## 2. What needs to change?

**Nothing.** The fix is complete. This is a documentation-only change with no code modifications.

## 3. What are the risks?

**Zero risk.** No code changes, no ABI changes, no new host functions. The fix is purely additive documentation — two sections that were always present in the engine and Rust SDK but missing from ABI.md.

## 4. Pre-existing issues (confirmed, out of scope)

### a. Section ordering inconsistency

Sections 2.53/2.54 (JSON helpers) appear before 2.51/2.52 (plugin extensions) in the document. This inverted ordering was present before the fix and was preserved by the renumbering. Cosmetic — does not affect correctness.

### b. Rust SDK schedule_invoke naming bug

`host_calls.rs:194` declares `pub fn schedule_invoke(...)` without the `cleat_` prefix, but `engine/imports.go:707` exports `cleat_schedule_invoke`. All other 53 imports use the `cleat_` prefix. This would cause a WASM module link error at load time. Pre-existing — scoped to cleat-233c.

## 5. What's the complexity?

**TRIVIAL — already done.** The task was a documentation-only ABI.md update. The fix was already applied before exploration began. Independent verification confirms correctness.

## 6. Recommendation

**Mark cleat-233d as complete.** The fix is correct, complete, and independently verified. No remaining work. The two pre-existing issues (section ordering, schedule_invoke naming) are correctly scoped to other tasks.
