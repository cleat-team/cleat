# cleat-233dm Status

**Task:** Sixth independent verification pass on cleat-233d (ABI.md fix for cleat_poll_child, cleat_await_any_child)
**Status:** done — all claims verified independently
**Date:** 2026-06-05

## Summary

Independently verified cleat-233d's ABI.md fix. All six passes (including this one) concur: the fix is correct and complete.

## Verification Method

- Enumerated all 54 section headers in ABI.md (sections 2.1–2.54)
- Counted all 56 `.Export()` calls in `engine/imports.go` (54 host + 2 internal)
- Confirmed `cleat_poll_child` and `cleat_await_any_child` present in `crates/cleat-sdk/src/host_calls.rs`
- Verified `git diff HEAD -- ABI.md` shows correct insertion of §§2.24–2.25 and renumbering of §§2.26–2.54
- Verified 5 cross-reference updates via grep of ABI.md

## Findings

1. **cleat_poll_child (§2.24, line 636): PASS** — present with correct signature, matches engine and Rust SDK
2. **cleat_await_any_child (§2.25, line 662): PASS** — present with correct signature, matches engine and Rust SDK
3. **Section renumbering (2.26–2.54): PASS** — all sections shifted by +2
4. **Cross-reference updates: PASS** — all 5 references updated (changelog §v4, §§6.1, 6.3, 6.5, 6.6)
5. **Triple-source cross-reference: PASS** — 54 host functions in all three sources

## Pre-existing Issues Confirmed

- **schedule_invoke naming bug** — host_calls.rs:194 uses `schedule_invoke` without `cleat_` prefix; imports.go:707 exports `cleat_schedule_invoke`. WASM link error risk.
- **ABI.md §§2.20–2.21 missing priority: i64** — engine and Rust SDK both include it; ABI.md omits it from WASM signature
- **Section ordering (2.53/2.54 before 2.51/2.52)** — cosmetic, predates fix

## Artifact

`artifacts/exploration.md`
