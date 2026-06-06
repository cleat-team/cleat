# cleat-233dr Review Report

**Date:** 2026-06-05
**Reviewer:** cleat-233dr (review pass on cleat-233d)
**Prior work:** STATUS.md (cleat-233d, 2026-06-05)

## Scope

Verify the ABI.md fix for cleat_poll_child and cleat_await_any_child omissions. Confirm cross-reference consistency across ABI.md, engine/imports.go, and crates/cleat-sdk/src/host_calls.rs.

## Verified Claims

### 1. cleat_poll_child at ABI 2.24 — VERIFIED

Lines 636-660. Non-blocking poll, signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"status":"running|completed|failed","result":"...","error":"..."}`. Matches imports.go line 402 `.Export("cleat_poll_child")` and Rust SDK host_calls.rs line 224 `pub fn cleat_poll_child(...)`.

### 2. cleat_await_any_child at ABI 2.25 — VERIFIED

Lines 662-686. Blocking await, signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"run_id":"...","result":"...","error":"..."}`. Matches imports.go line 414 `.Export("cleat_await_any_child")` and Rust SDK host_calls.rs line 230 `pub fn cleat_await_any_child(...)`. Correctly documents deterministic sorted-order polling and suspend semantics.

### 3. Section renumbering (2.26–2.54) — VERIFIED

All sections from old 2.24 onward shifted up by 2:
- old 2.24 (cleat_run_detached) → 2.26
- old 2.49 (plugin_call) → 2.51
- old 2.50 (plugin_call_streaming) → 2.52
- old 2.51 (cleat_json_parse) → 2.53
- old 2.52 (cleat_json_stringify) → 2.54

### 4. Cross-reference updates — VERIFIED

| Location | Old Ref | New Ref | Status |
|---|---|---|---|
| Changelog v4 entry | `2.51`/`2.52` | `2.53`/`2.54` | Fixed |
| Section 6.1 NaN canonicalization | `ABI 2.52` | `ABI 2.54` | Fixed |
| Section 6.3 JSON output | `ABI 2.52` | `ABI 2.54` | Fixed |
| Section 6.5 UUID reference | `ABI 2.40` | `ABI 2.42` | Fixed |
| Section 6.6 Compliance matrix | `2.51`/`2.52` | `2.53`/`2.54` | Fixed |

### 5. Triple-source cross-reference — VERIFIED

All 54 host functions present in all three sources (excluding internal utilities cleat_poll_work, cleat_complete):

| Source | Count | Status |
|---|---|---|
| ABI.md sections 2.1–2.54 | 54 | Complete |
| engine/imports.go .Export(...) calls | 54 | Complete |
| crates/cleat-sdk/src/host_calls.rs extern "C" declarations | 54 | Complete |

## Additional Findings

### Bonus content (above scope)

The fix also added two paragraphs under ABI 2.20 (cleat_child_workflow_with_options):
- "SDK-level typed wrapper" — explains ChildWorkflowTyped convenience
- "Relationship" — clarifies the two child workflow variants and when to use each

These are correct and valuable but not mentioned in STATUS.md.

### Pre-existing: Section ordering inconsistency

Sections 2.53/2.54 (JSON helpers) appear before 2.51/2.52 (plugin extensions) in the document. This inverted ordering was present before the fix (old numbering was 2.51/2.52 before 2.49/2.50) and was preserved by renumbering. Not introduced by cleat-233d.

### Pre-existing: Rust SDK schedule_invoke naming bug — CONFIRMED

host_calls.rs line 194 declares `pub fn schedule_invoke(...)` without the `cleat_` prefix, but engine/imports.go line 707 exports `cleat_schedule_invoke`. All other 53 imports have the `cleat_` prefix. This would cause a WASM module link error at load time. Not introduced by cleat-233d — should be addressed in cleat-233c or filed separately.

## Risk Assessment

**No new risks.** The fix is purely documentation — no code changes, no ABI changes, no new host functions. The risk profile is zero.

## Verdict

**PASS.** The cleat-233d task is correctly and completely implemented. All documented claims verified against the actual file contents. No regressions. No missing sections. No stale cross-references. The pre-existing issues noted above are correctly identified and out of scope for this task.
