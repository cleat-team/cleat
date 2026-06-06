# cleat-233d Status

**Task:** Fix ABI.md omissions (cleat_poll_child, cleat_await_any_child)
**Status:** done (fix already in working tree)
**Date:** 2026-06-05

## Verification

The fix for this task was already applied as uncommitted changes to ABI.md:

- **cleat_poll_child** — documented at ABI section 2.24. Non-blocking poll, signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"status":"running|completed|failed","result":"...","error":"..."}`.
- **cleat_await_any_child** — documented at ABI section 2.25. Blocking await for any child, signature `(i32, i32, i32, i32) -> i64`. Returns JSON `{"run_id":"...","result":"...","error":"..."}`.

Section numbers for all subsequent functions (2.26–2.54) were renumbered accordingly, and ABI references throughout the document (e.g., `2.51` → `2.53`, `2.52` → `2.54`) were updated.

## Cross-Reference Check

All three sources now agree on 54 host functions (excluding internal utilities `cleat_poll_work` and `cleat_complete`):

| Source | Host functions | Missing |
|---|---|---|
| `ABI.md` | 54 (sections 2.1–2.54) | none |
| `engine/imports.go` | 54 exported | none |
| `crates/cleat-sdk/src/host_calls.rs` | 54 WASM imports | none |

All signatures match. Verified by comparing `.Export(...)` names from imports.go against ABI.md section headers and Rust SDK `extern "C"` declarations.

## Verification Pass (cleat-233dv)

**Date:** 2026-06-05
**Explorer:** cleat-233dv
**Result:** PASS — independent verification confirms all claims.

- ABI.md sections 2.24 (cleat_poll_child) and 2.25 (cleat_await_any_child) present and correct
- All three sources (ABI.md, imports.go, host_calls.rs) enumerate the same 54 host functions
- Section renumbering (2.26–2.54) verified correct
- Five cross-reference updates in changelog, sections 6.1, 6.3, 6.5, 6.6 all verified
- Zero risk: documentation-only change, no code or ABI modifications
- Exploration artifact: `artifacts/exploration.md`

## Verification Pass (cleat-233dc)

**Date:** 2026-06-05
**Explorer:** cleat-233dc (cross-check exploration)
**Result:** PASS — independent cross-check confirms all claims.

- All cleat-233d deliverables verified via `git diff HEAD -- ABI.md`
- Cross-task consistency checked against cleat-233a, cleat-233b, cleat-233c — no conflicts
- Confirmed pre-existing issues: §§2.20-2.21 missing priority: i64 (first found by cleat-233de), schedule_invoke naming bug
- Recommends filing follow-up for priority param documentation gap
- Exploration artifact: `../cleat-233dc/artifacts/exploration.md`

## Verification Pass (cleat-233dk)

**Date:** 2026-06-05
**Explorer:** cleat-233dk (fifth independent verification)
**Result:** PASS — all claims confirmed.

- Independently enumerated all 54 extern "C" declarations in host_calls.rs and all 56 .Export() calls in imports.go
- Verified 54 host functions in ABI.md sections 2.1–2.54
- Confirmed cleat_poll_child (2.24) and cleat_await_any_child (2.25) signatures match engine and Rust SDK
- Verified all 5 cross-reference updates (changelog, §§6.1, 6.3, 6.5, 6.6) are correct
- Confirmed section renumbering 2.26–2.54 is correct
- Confirmed 3 pre-existing issues: schedule_invoke naming bug, priority: i64 docs gap in §§2.20-2.21, §2.53/2.54 ordering
- Zero risk: documentation-only change, no code modifications
- Exploration artifact: `artifacts/exploration-233dk.md`

## Verification Pass (cleat-233dp)

**Date:** 2026-06-06
**Explorer:** cleat-233dp (sixth independent verification)
**Result:** PASS — all claims confirmed.

- Independently counted 54 host functions in all three sources (ABI.md sections, imports.go exports, host_calls.rs extern declarations)
- Verified cleat_poll_child (2.24) and cleat_await_any_child (2.25) present with correct signatures matching engine and Rust SDK
- Verified all 5 cross-reference updates (changelog, §§6.1, 6.3, 6.5, 6.6) against `git diff HEAD -- ABI.md`
- Confirmed section renumbering 2.26–2.54 is correct
- Noted schedule_invoke naming bug now has `#[link_name = "cleat_schedule_invoke"]` fix in uncommitted host_calls.rs changes
- Noted host_calls.rs ABI comment references for cleat_poll_child/cleat_await_any_child updated from 2.44/2.45 to 2.24/2.25
- Zero risk: documentation-only change
- Exploration artifact: `../cleat-233dp/artifacts/exploration.md`

## Observation (out of scope)

The Rust SDK `host_calls.rs` imports `schedule_invoke` (without `cleat_` prefix, line 194) but the engine exports `cleat_schedule_invoke` (imports.go line 707). This naming mismatch would cause a WASM link error at module load time. This is a pre-existing bug, not introduced by this task. Should be addressed in cleat-233c (Rust WASM integration) or filed as a separate fix.
