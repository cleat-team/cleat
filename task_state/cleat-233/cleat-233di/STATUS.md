# cleat-233di Status

**Task:** cleat-233d investigation (verify ABI.md fix)
**Status:** done — investigation complete
**Date:** 2026-06-05

## Investigation Result

The cleat-233d task is correctly and completely implemented. All claims verified against file contents and `git diff HEAD`.

## Verified Claims

### 1. cleat_poll_child at ABI 2.24 — CONFIRMED

Lines 636-660. Non-blocking poll. Signature `(i32, i32, i32, i32) -> i64`. Return packing: bits 0-31 = errCode, 32-63 = resultLen. Result: `{"status":"running|completed|failed","result":"...","error":"..."}`.

### 2. cleat_await_any_child at ABI 2.25 — CONFIRMED

Lines 662-686. Blocking await. Signature `(i32, i32, i32, i32) -> i64`. Return packing: bits 0-31 = errCode, 32-63 = resultLen. Result: `{"run_id":"...","result":"...","error":"..."}`. Correctly documents deterministic sorted-order polling and suspend semantics.

### 3. Section renumbering (2.26–2.54) — CONFIRMED

All sections from old 2.24 onward shifted up by 2. Verified via `git diff HEAD -- ABI.md`:
- old 2.24 (cleat_run_detached) → 2.26
- old 2.25 (cleat_register_query_handler) → 2.27
- ...
- old 2.37 (cleat_reject_promise) → 2.39
- etc.

### 4. Cross-reference updates — CONFIRMED

| Location | Old Ref | New Ref | Status |
|---|---|---|---|
| Changelog v4 | 2.51/2.52 | 2.53/2.54 | Updated |
| Section 6.1 NaN | 2.52 | 2.54 | Updated |
| Section 6.3 JSON | 2.52 | 2.54 | Updated |
| Section 6.5 UUID | 2.40 | 2.42 | Updated |
| Section 6.6 Compliance | 2.51/2.52 | 2.53/2.54 | Updated |

### 5. Triple-source cross-reference — CONFIRMED

| Source | Count | Notes |
|---|---|---|
| ABI.md sections 2.1–2.54 | 54 | Complete |
| engine/imports.go .Export(...) | 54 (56 - 2 internal) | `cleat_poll_work`, `cleat_complete` excluded |
| crates/cleat-sdk/src/host_calls.rs | 54 extern "C" | All prefixed with `cleat_` (except schedule_invoke bug) |

All three sources agree on 54 host functions.

### 6. Bonus content — CONFIRMED

Two paragraphs added under ABI 2.20:
- "SDK-level typed wrapper" explains ChildWorkflowTyped convenience
- "Relationship" clarifies when to use each child workflow variant

These are correct and valuable.

## Pre-existing Issues (not introduced by cleat-233d)

### Section ordering inconsistency

Sections 2.53/2.54 (JSON helpers) appear before 2.51/2.52 (plugin extensions) in the document. This inverted ordering was present before the fix (old numbering was 2.51/2.52 before 2.49/2.50) and was preserved by renumbering.

### Rust SDK schedule_invoke naming bug

`crates/cleat-sdk/src/host_calls.rs` line 194 declares `pub fn schedule_invoke(...)` without the `cleat_` prefix. All other 53 imports use the prefix. The engine exports `cleat_schedule_invoke` at `engine/imports.go` line 707. This would cause a WASM module link error at load time. Should be addressed in cleat-233c or filed separately.

## Risk Assessment

No new risks. The fix is purely documentation — no code changes, no ABI changes, no new host functions.

## Verdict

**PASS.** All cleat-233d claims verified independently against current file state. No regressions. No stale cross-references.
