# cleat-233dp Exploration Report

**Date:** 2026-06-06
**Explorer:** cleat-233dp (sixth independent verification pass)
**Task:** Fix ABI.md omissions (cleat_poll_child, cleat_await_any_child)
**Status from STATUS.md:** done (fix already in working tree)
**Prior passes:** cleat-233dr (review), cleat-233dv (verify), cleat-233dc (cross-check), cleat-233de (deep dive), cleat-233dk (fifth verify)

## 1. What's here now?

### The fix (verified via `git diff HEAD -- ABI.md`)

Two new ABI sections inserted:

- **2.24 `cleat_poll_child`** (lines 636-660): Non-blocking poll. Signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"status":"running|completed|failed","result":"...","error":"..."}`.
- **2.25 `cleat_await_any_child`** (lines 662-686): Blocking await for any child. Signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"run_id":"...","result":"...","error":"..."}`. Documents deterministic sorted-order polling and suspend semantics.

### Section renumbering (independently verified via `grep '^#### 2\.' ABI.md`)

All sections from old 2.24 onward shifted up by 2. Sample:
- 2.26 `cleat_run_detached` (was 2.24) ✓
- 2.27 `cleat_register_query_handler` (was 2.25) ✓
- 2.29 `set_query_state` (was 2.27) ✓
- 2.53 `cleat_json_parse` (was 2.51) ✓
- 2.54 `cleat_json_stringify` (was 2.52) ✓

### Cross-reference updates (all 5 verified via git diff)

| Location | Old Ref | New Ref | Status |
|---|---|---|---|
| Changelog v4 entry | 2.51/2.52 | 2.53/2.54 | PASS |
| §6.1 NaN canonicalization | ABI 2.52 | ABI 2.54 | PASS |
| §6.3 JSON output | ABI 2.52 | ABI 2.54 | PASS |
| §6.5 UUID reference | ABI 2.40 | ABI 2.42 | PASS |
| §6.6 Compliance matrix | 2.51/2.52 | 2.53/2.54 | PASS |

### Triple-source cross-reference (independently counted)

| Source | Count | Method |
|---|---|---|
| ABI.md sections 2.1–2.54 | 54 | `grep -c '^#### 2\.' ABI.md` → 54 |
| engine/imports.go .Export() calls | 54 host + 2 internal | `grep '\.Export(' imports.go \| grep -v 'cleat_poll_work\|cleat_complete' \| wc -l` → 54 |
| crates/cleat-sdk/src/host_calls.rs extern "C" | 54 | `awk '/extern "C"/,/^}/' host_calls.rs \| grep -c 'pub fn '` → 54 |

All three sources agree on 54 host functions.

### Bonus content (above original scope)

Two prose paragraphs added under ABI §2.20:
- "SDK-level typed wrapper" — documents `ChildWorkflowTyped` convenience in Go SDK
- "Relationship" — clarifies when to use `cleat_child_workflow` vs `cleat_child_workflow_with_options`

### Uncommitted host_calls.rs improvements (complementary)

The working tree also has fixes to `crates/cleat-sdk/src/host_calls.rs`:
- ABI comment references for `cleat_poll_child` and `cleat_await_any_child` updated from 2.44/2.45 to 2.24/2.25
- `#[link_name = "cleat_schedule_invoke"]` added to the `schedule_invoke` extern declaration (fixes the pre-existing naming bug identified by all prior passes)

## 2. What needs to change?

**Nothing.** The fix is complete and correct. Six independent verification passes (cleat-233dr, cleat-233dv, cleat-233dc, cleat-233de, cleat-233dk, cleat-233dp) all agree.

## 3. What are the risks?

**Zero risk.** Documentation-only change. No code modifications in ABI.md's scope. No new host functions. No ABI changes.

## 4. Pre-existing issues (independently confirmed)

### a. Section ordering inconsistency — COSMETIC

Sections 2.53/2.54 (JSON helpers, lines 1292/1318) appear before 2.51/2.52 (plugin extensions, lines 1344/1373). Inverted ordering predates this fix. Cosmetic.

### b. schedule_invoke naming — BEING FIXED in working tree

Previously: `host_calls.rs:194` declared `pub fn schedule_invoke(...)` without `cleat_` prefix, but `engine/imports.go:707` exports `cleat_schedule_invoke`. **Now:** `#[link_name = "cleat_schedule_invoke"]` has been added (uncommitted) to bridge this gap. The WASM module will import `cleat_schedule_invoke` matching the engine export.

### c. ABI 2.20–2.21 missing priority: i64 in formal signature — CONFIRMED

The "Relationship" prose at line 553 mentions priority conceptually, but the formal WASM signature tables in §§2.20-2.21 don't include it. Both `engine/imports.go` and `host_calls.rs` include priority in their signatures. Pre-existing doc gap.

## 5. What's the complexity?

**TRIVIAL — already complete.** Six independent verification passes all concur. Documentation-only change with zero risk.

## 6. Recommendation

**Mark cleat-233d as complete.** No remaining work. The three pre-existing issues are correctly identified and either scoped to other tasks (priority docs gap, section ordering) or being fixed in the working tree (schedule_invoke link_name).
