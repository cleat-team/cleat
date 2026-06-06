# cleat-233dm Exploration Report

**Date:** 2026-06-05
**Explorer:** cleat-233dm
**Task:** Sixth independent verification pass on cleat-233d (Fix ABI.md omissions)
**Status from STATUS.md:** done (fix already in working tree)
**Prior passes:** cleat-233dr (review), cleat-233dv (verify), cleat-233dc (cross-check), cleat-233de (deep dive), cleat-233dk (fifth verify)

## 1. What's here now?

### The fix (verified via `git diff HEAD -- ABI.md`)

Two new ABI sections inserted:

- **2.24 `cleat_poll_child`** (lines 636-660): Non-blocking poll. Signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"status":"running|completed|failed","result":"...","error":"..."}`. Does not suspend.
- **2.25 `cleat_await_any_child`** (lines 662-686): Blocking await for any child. Signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"run_id":"...","result":"...","error":"..."}`. Documents deterministic sorted-order polling and suspend semantics.

All sections from old 2.24 onward shifted up by 2 (2.26–2.54).

### Bonus content (above original scope)

The fix also added two prose paragraphs under ABI §2.20:
- "SDK-level typed wrapper" — documents `ChildWorkflowTyped` convenience in Go SDK
- "Relationship" — clarifies when to use `cleat_child_workflow` vs `cleat_child_workflow_with_options`

### Triple-source cross-reference (independently compiled)

| Source | Count | Method |
|---|---|---|
| ABI.md sections 2.1–2.54 | 54 | `grep -c '^#### 2\.' ABI.md` — 54 matches |
| engine/imports.go .Export() calls | 56 (54 host + 2 internal) | `grep -c '\.Export(' engine/imports.go` — 56 matches; exclude cleat_poll_work, cleat_complete |
| crates/cleat-sdk/src/host_calls.rs extern "C" | 54 | Manual enumeration of extern block |

All three sources enumerate the same 54 host functions. cleat_poll_child and cleat_await_any_child are present in all three with matching signatures.

### Cross-reference updates (independently verified)

| Location | Old Ref | New Ref | Verified |
|---|---|---|---|
| Changelog v4 entry (line 1451) | 2.51/2.52 | 2.53/2.54 | PASS |
| §6.1 NaN canonicalization (line 1466) | ABI 2.52 | ABI 2.54 | PASS |
| §6.3 JSON output (line 1510) | ABI 2.52 | ABI 2.54 | PASS |
| §6.5 UUID reference (line 1567) | ABI 2.40 | ABI 2.42 | PASS |
| §6.6 Compliance matrix (lines 1579-1580) | 2.51/2.52 | 2.53/2.54 | PASS |

### Section renumbering (independently verified)

All sections from 2.26 through 2.54 are correctly renumbered. Sample verification:
- 2.26 `cleat_run_detached` (was 2.24) — confirmed at line 688
- 2.27 `cleat_register_query_handler` (was 2.25) — confirmed at line 713
- 2.29 `set_query_state` (was 2.27) — confirmed at line 755
- 2.53 `cleat_json_parse` (was 2.51) — confirmed at line 1292
- 2.54 `cleat_json_stringify` (was 2.52) — confirmed at line 1318

## 2. What needs to change?

**Nothing.** The fix is complete and correct. All cross-references are updated. All three sources agree. This is a documentation-only change with zero code or ABI modifications.

## 3. What are the risks?

**Zero risk.** No code changes. No new host functions. No ABI changes. No behavior changes. Pure documentation — listing two functions that already exist in the engine and Rust SDK.

## 4. Pre-existing issues (confirmed independently)

### a. `schedule_invoke` naming mismatch — CONFIRMED BUG

`host_calls.rs:194` declares `pub fn schedule_invoke(...)` without `cleat_` prefix, but `engine/imports.go:707` exports `cleat_schedule_invoke`. All other 53 host functions have matching `cleat_` prefixes.

This would cause a WASM module link error at load time: the module imports `schedule_invoke` but the host only provides `cleat_schedule_invoke`.

Not introduced by this task. Should be addressed in cleat-233c (Rust WASM integration test) or filed as a separate fix.

### b. ABI.md §§2.20–2.21 missing `priority: i64` — CONFIRMED DOCUMENTATION GAP

- `engine/imports.go:258` includes `priority int64` in `ChildWorkflowWithOptions`
- `engine/imports.go:287` includes `priority int64` in `ChildWorkflowInSchema`
- `host_calls.rs:66` includes `priority: i64` in the extern declaration
- ABI.md §2.20 WASM signature and parameter table do not include priority

The "Relationship" prose at line ~553 mentions priority conceptually, but the formal WASM signature does not. Not introduced by this task. First identified by cleat-233de.

### c. Section ordering inconsistency — COSMETIC

Sections 2.53/2.54 (JSON helpers) appear before 2.51/2.52 (plugin extensions) in the document. This inverted ordering predates the fix and was preserved by renumbering. Cosmetic — does not affect correctness.

### d. `set_query_state` and `plugin_call*` lack `cleat_` prefix — CONSISTENT (not bugs)

These functions are exported without the `cleat_` prefix in imports.go, and the Rust SDK uses matching `#[link_name]` attributes where needed:
- `set_query_state`: both engine and Rust SDK use this name — consistent
- `plugin_call` / `plugin_call_streaming`: engine exports these names; Rust SDK uses `#[link_name = "plugin_call"]` on `pub fn cleat_plugin_call(...)` — consistent

## 5. What's the complexity?

**TRIVIAL — already complete.** The five prior verification passes (cleat-233dr, cleat-233dv, cleat-233dc, cleat-233de, cleat-233dk) all reach the same conclusion. This sixth independent pass confirms all findings.

## 6. Recommendation

**Mark cleat-233d as complete.** The fix is correct, complete, and verified six ways. No remaining work. The three pre-existing issues (schedule_invoke naming, priority docs gap, section ordering) are either correctly scoped to other tasks or cosmetic.
