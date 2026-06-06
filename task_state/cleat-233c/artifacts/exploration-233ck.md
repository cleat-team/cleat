# cleat-233ck Exploration Report

**Explorer:** cleat-233ck
**Date:** 2026-06-05
**Task:** cleat-233c — Rust SDK WASM integration test

## Status: COMPLETE (third independent verification)

The task cleat-233c was already completed before this exploration. All prior agent work (cleat-233ce, cleat-233cr, cleat-233ci, cleat-233cv, cleat-233cc) is verified accurate. No protocol file (`prompts/explorer-agent.md`) exists, so this report follows the established pattern from prior agents.

## Independent Verification Results

### Rust Crate Tests — 102 PASS (verified)

| Crate | Tests | Result |
|-------|-------|--------|
| cleat-sdk | 32 | PASS (0.00s) |
| cleat-macro | 8 unit + 5 UI | PASS (0.31s) |
| cleat-test | 57 | PASS (0.00s) |

All independently re-run from this session. Count and results match all prior reports (cleat-233ce, cleat-233cr, cleat-233ci, cleat-233cc).

### Go Integration Tests — 4 PASS (verified)

| Test | Duration | Result |
|------|----------|--------|
| TestRustWorkflowExecute | 0.17s | PASS — 4 service calls in history |
| TestRustWorkflowReplay | 0.21s | PASS — deterministic, 0 real calls on replay |
| TestRustWorkflowCancelOrder | 0.14s | PASS — cancellation entry point works |
| TestRustWorkflowCompensation | 0.15s | PASS — saga refund+release verified |

Four integration tests exist in `engine/rust_workflow_test.go`. All pass without `-short`. With `-short`, all four correctly skip (0.010s total).

### WASM Compilation — PASS

`cargo build --target wasm32-wasip1 --release` succeeds in `examples/rust-workflow`. One harmless unused-import warning (`use cleat_sdk::HostCalls` on lib.rs:6).

## Prior Fixes Confirmed

| Fix | Status | Evidence |
|-----|--------|----------|
| `schedule_invoke` ABI mismatch | APPLIED | `host_calls.rs:194`: `#[link_name = "cleat_schedule_invoke"]` |
| CI path (`./internal/host/...` → `./engine/...`) | FIXED | `e2e-cross-language.yml:101`: `./engine/...` |
| `cleat_json_stringify` cfg gating | FIXED | `host_calls.rs:297,306`: `#[cfg(target_arch = "wasm32")]` on both stringify and parse |
| `findProjectRoot` path resolution | FIXED | `rust_workflow_test.go:45-60`: go.mod walk-up traversal |

## Remaining Issues (unchanged)

1. **Build target mismatch** (MEDIUM): CLI (`build_rust.go:34`) uses `wasm32-unknown-unknown`, integration test (`rust_workflow_test.go:29`) uses `wasm32-wasip1`. The CLI comment says unknown-unknown avoids non-deterministic WASI imports, which is valid rationale. The test uses wasip1 because wazero's WASI support is useful during testing. Not a release blocker.
2. **Unused import warning** (LOW): `use cleat_sdk::HostCalls` in `examples/rust-workflow/src/lib.rs:6`. rustc can't see through `#[cleat_entry]` macro expansion. Cosmetic.

## Success Criteria (from PLAN.md)

- [x] At least one Rust workflow compiles to WASM and completes in a cleat worker
- [x] Go test validates WASM binary loads and executes (4 integration tests, not just smoke)
- [x] All existing Rust SDK tests continue to pass (102 tests)

## Conclusion

**cleat-233c is complete.** All success criteria are met. All critical bugs found by prior agents are fixed and verified. The two remaining cosmetic issues (build target mismatch, unused import) are non-blocking for the 0.5 release. No new issues found.

This is now the third independent exploration (after cleat-233ce and cleat-233cc) confirming the same results. Recommend closing the exploration loop on cleat-233c.
