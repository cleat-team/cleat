# cleat-233de Exploration Report — ABI Conformity Deep Dive (Updated)

**Date:** 2026-06-06 (original: 2026-06-05, updated re-verification)
**Explorer:** cleat-233de (explorer pass on cleat-233d + broader ABI cross-reference)
**Prior work:** cleat-233d (ABI.md fix), cleat-233e (initial survey), cleat-233i (verification)

## 1. What's here now?

Three sources define the WASM ABI surface:

- **`engine/imports.go`** (definitive/authoritative): 56 `.Export(...)` calls (54 public host functions + 2 internal: `cleat_poll_work`, `cleat_complete`)
- **`ABI.md`**: 54 documented sections (2.1–2.54)
- **`crates/cleat-sdk/src/host_calls.rs`** (Rust SDK): 54 `extern "C"` declarations

### cleat-233d fix verification (RE-VERIFIED)

The cleat-233d fix added `cleat_poll_child` (§2.24) and `cleat_await_any_child` (§2.25) to ABI.md. Verified:

- Both sections exist with correct WASM signatures (4 params each)
- Both parameter tables are complete and accurate
- Both return-packing descriptions are correct
- Sections 2.26–2.54 were renumbered to accommodate the insertions
- Cross-references updated (e.g., `2.51` → `2.53` for `cleat_json_parse`)

**Conclusion: The cleat-233d fix is correct and complete for its stated scope.**

### Changes since original 2026-06-05 exploration

The `schedule_invoke` naming mismatch (finding 2b in original report) has been **FIXED** in the working tree:
- `host_calls.rs:194` now has `#[link_name = "cleat_schedule_invoke"]` mapping the Rust-side `schedule_invoke` to the engine's `cleat_schedule_invoke` export
- Additionally, the ABI section comments for `cleat_poll_child` (2.44 → 2.24) and `cleat_await_any_child` (2.45 → 2.25) were corrected in host_calls.rs

Other ABI section number comments in host_calls.rs still reflect pre-renumbering values (e.g., `durablecreate_promise` labeled "ABI 2.20" but the section is now 2.36). This is cosmetic — Rust comments only, no functional impact.

## 2. What needs to change?

### 2a. `priority: i64` missing from ABI.md §§2.20–2.21 (STILL PRESENT)

Both `cleat_child_workflow_with_options` and `cleat_child_workflow_in_schema` accept a `priority: i64` parameter in both the engine and the Rust SDK. ABI.md's formal WASM signatures and parameter tables do NOT include it.

| Source | `_with_options` param count | `_in_schema` param count |
|---|---|---|
| engine/imports.go | 10 (includes priority) | 12 (includes priority) |
| Rust host_calls.rs | 10 (includes priority) | 12 (includes priority) |
| ABI.md signatures | 9 (missing priority) | 11 (missing priority) |

**Correction needed for ABI.md §2.20:**
- WASM signature: `(param i32 i32 i32 i32 i64 i32 i32 i32 i32)` → `(param i32 i32 i32 i32 i64 i64 i32 i32 i32 i32)`
- Parameter table: add `priority` row between `version` and `policy_ptr`

**Correction needed for ABI.md §2.21:**
- WASM signature: `(param i32 i32 i32 i32 i32 i32 i64 i32 i32 i32 i32)` → `(param i32 i32 i32 i32 i32 i32 i64 i64 i32 i32 i32 i32)`
- Parameter table: add `priority` row between `version` and `policy_ptr`

**Note on cleat-233i:** The verified exploration claimed the original cleat-233e was wrong to flag this — "ABI.md §2.20-2.21 document the 5-param signatures including priority: i64. This is FALSE." The verification is itself incorrect. Only the RELATIONSHIP prose at line 553 mentions priority in passing; the formal WASM signature and parameter table both omit it. This IS a real documentation gap.

### 2b. `schedule_invoke` naming mismatch — FIXED as of 2026-06-06

Previously: Rust SDK imported `schedule_invoke` (no `cleat_` prefix) but engine exported `cleat_schedule_invoke`.
Now: `#[link_name = "cleat_schedule_invoke"]` added at `host_calls.rs:194`. WASM link error resolved. **No further action needed.**

### 2c. ABI.md section ordering (COSMETIC, STILL PRESENT)

Sections appear in this order: ...2.49, 2.50, **2.53, 2.54, 2.51, 2.52**. Plugin extensions (2.51–2.52) are placed after JSON helpers (2.53–2.54). No semantic impact.

### 2d. Rust SDK ABI comment numbers — PARTIALLY UPDATED

Only `cleat_poll_child` (2.44→2.24) and `cleat_await_any_child` (2.45→2.25) had their ABI section comments updated. All other ~30 ABI comments in host_calls.rs still reference pre-renumbering section numbers. Cosmetic — Rust comments only, no functional impact.

## 3. What are the risks?

1. **`priority` omission is a correctness bug for non-Go SDK implementers.** Someone implementing a new language SDK from ABI.md alone would produce a WASM module with the wrong import signature for `cleat_child_workflow_with_options` and `cleat_child_workflow_in_schema`. The engine would reject the module with a link error (argument count mismatch). **Risk unchanged from original report.**

2. **`schedule_invoke` mismatch: RESOLVED.** The `#[link_name]` fix ensures correct WASM linking. **Risk eliminated.**

3. **ABI.md section ordering** could cause confusion for implementers reading sequentially, but this is low risk since section numbers are referenced.

## 4. What's the complexity?

**TRIVIAL.** One remaining non-cosmetic issue:

- **Fix A (priority params, ~15 min):** Update ABI.md §§2.20–2.21 WASM signatures and parameter tables. Simple text edits.

The `schedule_invoke` fix is already applied. The remaining issues (section ordering, stale Rust comments) are cosmetic.

## 5. Recommendation

**The cleat-233d task remains complete and correct.** The `cleat_poll_child` and `cleat_await_any_child` entries are properly documented with accurate signatures.

One remaining action item:

1. **Fix the `priority` ABI.md omission in §§2.20–2.21.** This can be folded into cleat-233e (docs update task) or done as a standalone 15-minute fix. The WASM signatures and parameter tables in ABI.md must include `priority: i64` to match the engine (imports.go lines 239, 264) and Rust SDK (host_calls.rs).

The `schedule_invoke` fix is already in the working tree — no further action needed for that item.
