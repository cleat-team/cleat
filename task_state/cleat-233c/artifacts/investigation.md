# cleat-233ci Investigation Report

**Investigator:** cleat-233ci (verify pass on cleat-233ce exploration + cleat-233cr review)
**Date:** 2026-06-05
**Prior work:** cleat-233ce exploration, cleat-233c worker implementation, cleat-233cr review

## Verified Findings

### Rust Crate Tests — 102 PASS (verified)

| Crate | Tests | Result |
|-------|-------|--------|
| cleat-sdk | 32 | PASS (0.00s) |
| cleat-macro | 8 unit + 5 UI | PASS (0.28s) |
| cleat-test | 57 | PASS (0.00s) |

Independently ran `cargo test` in all three crates. All green. Count matches review report.

### Go Integration Tests — 4 PASS (verified)

All 4 tests pass when run without `-short`:
- `TestRustWorkflowExecute` — 0.16s, 4 service calls verified
- `TestRustWorkflowReplay` — 0.26s, deterministic, 0 real calls on replay
- `TestRustWorkflowCancelOrder` — 0.17s, cancellation entry point works
- `TestRustWorkflowCompensation` — 0.14s, saga refund+release verified

`-short` mode correctly skips all 4 tests (0.009s total).

### WASM Compilation — PASS

- **wasm32-wasip1** (test target): SUCCEEDS, 1 unused-import warning
- **wasm32-unknown-unknown** (CLI target): SUCCEEDS (per review report)

### ABI Cross-Reference — 53 engine exports matched, 1 BUG FOUND

Engine exports 53 `cleat_*` host functions. Rust SDK declares 54 `pub fn cleat_*` in its extern block (the extra is `cleat_call_with_retry` which is a convenience name bridged via `#[link_name = "cleat_call_retry"]`).

Three `#[link_name]` bridges confirmed correct:
- `cleat_plugin_call` → `#[link_name = "plugin_call"]` ✓
- `cleat_plugin_call_streaming` → `#[link_name = "plugin_call_streaming"]` ✓
- `cleat_call_with_retry` → `#[link_name = "cleat_call_retry"]` ✓

Non-prefixed names that match:
- `set_query_state` → `set_query_state` ✓ (both without `cleat_` prefix)

**NEW FINDING — `schedule_invoke` ABI mismatch:**

- `crates/cleat-sdk/src/host_calls.rs:194`: `pub fn schedule_invoke(...)` — declared in the `extern "C"` block WITHOUT a `#[link_name]` attribute. Resolves to WASM import `schedule_invoke`.
- `engine/imports.go:707`: `.Export("cleat_schedule_invoke")` — engine exports the name WITH the `cleat_` prefix.

These don't match. At WASM instantiation time, wazero would reject the module because `schedule_invoke` is not an exported host function.

This is currently **latent** — the example workflow (`examples/rust-workflow/src/lib.rs`) never calls `schedule_invoke`, so the Rust compiler dead-code-eliminates the import from the compiled `.wasm`. Confirmed by `strings` on the WASM binary: only `cleat_call`, `cleat_json_stringify`, `cleat_poll_cancellation`, and `cleat_log` appear as imports.

**Impact**: Any Rust workflow that calls `HostCalls::schedule_invoke()` or `HostCalls::schedule_invoke_ms()` will fail at WASM module instantiation with a "missing import" error from wazero.

**Fix**: Add `#[link_name = "cleat_schedule_invoke"]` above line 194 in `crates/cleat-sdk/src/host_calls.rs`.

### Review Report Accuracy

| Claim | Review Report | My Verification | Verdict |
|-------|-------------|-----------------|---------|
| 102 Rust tests pass | PASS | PASS | Accurate |
| 4 Go integration tests pass | PASS (0.13-0.20s) | PASS (0.14-0.26s) | Accurate |
| -short skips all 4 | Yes | Yes | Accurate |
| WASM both targets compile | Yes | wasip1 confirmed | Accurate |
| findProjectRoot uses go.mod walk-up | Yes | Confirmed in code | Accurate |
| build target mismatch | Confirmed | Confirmed (CLI: unknown-unknown, test: wasip1) | Accurate |
| unused import warning | Confirmed | Confirmed (lib.rs:6) | Accurate |
| cleat_json_stringify cfg fixed | Both now gated | Confirmed (lines 296, 305) | Accurate |
| CI path fixed (./engine/...) | Fixed | Confirmed in e2e-cross-language.yml | Accurate |
| ABI imports matched (53/53) | Yes | 53 matched, 1 bug found (schedule_invoke) | **Incomplete** |
| WASM size ~103K-122K | Yes | Not re-measured (not critical) | Likely accurate |

### Additional Issues NOT in Review

**1. `schedule_invoke` ABI mismatch (MEDIUM/HIGH when triggered)**

Described in detail above. This was missed by both the exploration report (which claimed "53/53 matched") and the review report (which confirmed that claim). The mismatch only affects the `schedule_invoke` function; all other imports are correctly bridged.

Root cause: The Rust SDK extern block inconsistently drops the `cleat_` prefix for this one function. The `set_query_state` function similarly lacks the prefix, but the engine also exports it without the prefix, so that one matches.

### Remaining Low-Severity Items (unchanged from review)

1. **Build target mismatch** (MEDIUM): CLI uses `wasm32-unknown-unknown`, test uses `wasm32-wasip1`. The CLI comment says unknown-unknown avoids non-deterministic WASI imports, which is valid. The test uses wasip1 because wazero's WASI support is useful during testing. Not a release blocker but worth documenting.

2. **Unused import warning** (LOW): `use cleat_sdk::HostCalls` in `examples/rust-workflow/src/lib.rs:6`. rustc can't see through `#[cleat_entry]` macro expansion. Cosmetic.

## Conclusion

**cleat-233c is functionally complete.** All 4 success criteria from PLAN.md are met with passing tests. The exploration and review reports are substantially accurate.

**One actionable bug found**: `schedule_invoke` ABI mismatch in host_calls.rs. Fix is a one-line addition of `#[link_name = "cleat_schedule_invoke"]`. This should be fixed before any Rust workflow attempts to use scheduled invocations.
