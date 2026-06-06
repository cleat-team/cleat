# cleat-233de Status

**Task:** Explorer pass on cleat-233d (ABI.md fix) + broader ABI conformity deep dive
**Status:** re-verified (originally done 2026-06-05)
**Date:** 2026-06-06

## Summary (Updated)

Re-verified cleat-233d's fix is still correct — `cleat_poll_child` (§2.24) and `cleat_await_any_child` (§2.25) are properly documented. One of the two original findings (`schedule_invoke` naming mismatch) has been fixed in the working tree. The other (`priority: i64` missing from §§2.20–2.21) remains.

## Findings

1. **cleat-233d work: VERIFIED CORRECT (unchanged)** — Both functions documented, signatures match engine exports, section renumbering correct.

2. **NEW: ABI.md §§2.20–2.21 missing `priority: i64` (STILL PRESENT)** — Engine and Rust SDK both include it; ABI.md WASM signatures and parameter tables do not. Prose at line 553 mentions it but isn't formal spec.

3. **`schedule_invoke` naming mismatch: RESOLVED** — `#[link_name = "cleat_schedule_invoke"]` added at `host_calls.rs:194`. The Rust SDK now correctly links to the engine's export. No further action needed.

4. **Rust SDK ABI comment numbers: PARTIALLY UPDATED** — Only `cleat_poll_child` and `cleat_await_any_child` had their ABI section comments corrected (2.44→2.24, 2.45→2.25). ~30 other ABI comments still reference pre-renumbering section numbers. Cosmetic only.

## Recommendation

Fold the `priority` ABI.md fix into an existing task (cleat-233e for docs). 15 minutes of work. The `schedule_invoke` fix is already applied.
