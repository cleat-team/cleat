# cleat-233cv Verification Report

**Verifier:** cleat-233cv
**Date:** 2026-06-05
**Prior work:** cleat-233ce (exploration), cleat-233c (worker), cleat-233cr (review), cleat-233ci (investigation)

## Summary

Verified the `schedule_invoke` ABI mismatch found by cleat-233ci, applied the fix, and confirmed all tests pass.

## Bug: schedule_invoke ABI mismatch

**Root cause:** `crates/cleat-sdk/src/host_calls.rs:194` declared the WASM import as `schedule_invoke` without a `cleat_` prefix, lacking a `#[link_name]` attribute. The engine exports `cleat_schedule_invoke` (`engine/imports.go:707`). These names don't match — wazero would reject any WASM module that imports `schedule_invoke`.

**Why latent:** The example workflow never calls `schedule_invoke`, so the compiler dead-code-eliminates the import. Any workflow that does call it would fail at module instantiation.

**Fix:** Added `#[link_name = "cleat_schedule_invoke"]` above line 194 in `host_calls.rs`.

## Audit of all unprefixed functions

Only two extern block functions lack the `cleat_` prefix:

| Function | Rust name | Engine export | Match? |
|----------|-----------|---------------|--------|
| `set_query_state` | `set_query_state` | `set_query_state` | Yes |
| `schedule_invoke` | `schedule_invoke` | `cleat_schedule_invoke` | **No (FIXED)** |

All other 50+ functions have `cleat_` prefix matching the engine.

## Test Results

| Component | Tests | Result |
|-----------|-------|--------|
| crates/cleat-sdk | 32 | PASS |
| crates/cleat-macro | 8 unit + 5 UI | PASS |
| crates/cleat-test | 57 | PASS |
| engine/rust_workflow_test.go | 4 | PASS |
| WASM compilation (wasm32-wasip1) | — | SUCCEEDS |

- **Total: 102 Rust tests, 4 Go integration tests — all PASS**
- WASM build succeeds (1 harmless unused-import warning, pre-existing)

## Remaining Issues (from prior reports, unchanged)

1. **Build target mismatch** (MEDIUM): CLI uses `wasm32-unknown-unknown`, test uses `wasm32-wasip1`
2. **Unused import warning** (LOW): `use cleat_sdk::HostCalls` in `examples/rust-workflow/src/lib.rs:6`

## Conclusion

The `schedule_invoke` ABI mismatch is fixed. All tests pass. cleat-233c is complete.
