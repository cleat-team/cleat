# cleat-233i Investigation Report

**Date:** 2026-06-05
**Investigator:** cleat-233i (verify pass on cleat-233e exploration)
**Prior exploration:** `exploration.md` (cleat-233e, 2026-06-05)

## Verified Findings (with corrections)

### Rust SDK — ✅ TESTS PASS (verified)
- `crates/cleat-sdk`: 32 unit tests pass (mock-based MockHostCalls)
- `crates/cleat-macro`: 8 integration + 5 trybuild UI tests pass
- `crates/cleat-sdk`: `cargo build --target wasm32-wasip1 --release` succeeds (CI runs this)
- No WASM integration tests exist — all tests use mocks. This is the key gap.
- `@cleat_entry` proc-macro appears ABI-conformant from static analysis.

**ABI.md discrepancy (CORRECTION from cleat-233e):**
- cleat-233e reported `child_workflow_with_options` and `child_workflow_in_schema` have an undocumented `priority: i64` param. **This is FALSE.** ABI.md §2.20-2.21 document the 5-param signatures including `priority: i64`.
- **CONFIRMED:** `cleat_poll_child` and `cleat_await_any_child` exist in `crates/cleat-sdk/src/host_calls.rs` (lines 224, 230; comments say ABI 2.44/2.45) but are **absent from ABI.md**. ABI.md §2.44 is `cleat_side_effect`, §2.45 is `cleat_workflow_id`.

### AssemblyScript SDK — ❌ TESTS FAIL (verified)
- Root cause: `binaryen@116.0.0-nightly.20240114` has `"type": "module"` in package.json and uses top-level `await` in `index.js`. `as-pect` loads it via `require()` (CommonJS) → `ERR_REQUIRE_ASYNC_MODULE`.
- Two binaryen versions in dependency tree: 110 (via as-pect→visitor-as→assemblyscript@0.25.2) and 116 (via assemblyscript@0.27.32). as-pect resolves binaryen@116.
- CI has `continue-on-error: true` on both `test-assemblyscript` and `test-assemblyscript-wasm` — failures are silent.
- Only `cleat_json_parse` and `cleat_json_stringify` have JS stubs in `as-pect.config.mjs`. All other `@external("env", ...)` imports lack stubs.
- Fix options: (a) pin binaryen to 110 (CJS), (b) set `"type": "module"` in AS package.json + update as-pect config, (c) replace as-pect with a different test runner.

### Python SDK — ✅ NATIVE TESTS PASS, WASM E2E UNVALIDATED (verified)
- 443 tests pass (`python3 -m pytest tests/`) in 6.09s.
- `TestPythonWasmEndToEnd` in `engine/python_wasm_e2e_test.go` is well-structured but requires `componentize-py` + `wasm-tools` (skips when tools missing).
- `TestPythonWasmAbiBoundary`: Python SDK expects 45 host imports; Go registers 50. **Gap of 5:**
  - Go has but Python doesn't: `cleat_continue_as_new_versioned`, `cleat_child_workflow_in_schema`, `cleat_set_scope`, `cleat_get_scope`, `cleat_uuid`
  - These 5 are NOT in `pythonExpectedImports` list (lines 157-204 of the test file).
- **CI PATH BUG (new finding):** `e2e-cross-language.yml` line 101 runs `./internal/host/...` but `internal/host/` does NOT exist on disk. The Python WASM E2E tests are in `./engine/...`. This CI job never finds the tests.
- The WASM compilation pipeline (`scripts/build_wasm.py` → `componentize-py componentize` → WIT world) exists and can produce `.wasm` files, but execution against a real cleat worker has never been performed.

### LANGUAGE_SUPPORT.md — STALE (verified)
- Line 11: "Import 15 host functions" — should be ~50.
- No other stale counts found in LANGUAGE_SUPPORT.md itself, but exploration flagged "15 imports" mentions in SECURITY.md and `docs/explanation/architecture.md` (not verified here, out of scope for SDK task).

### DX_COMPARISON.md — CONTRADICTIONS (verified)
- Line 23: "WASM compilation has been validated end-to-end end-to-end" — double "end-to-end" typo.
- Line 149-150: Same double "end-to-end" typo (copy-paste from line 23-24).
- Line 340: "Python: 0 issues remaining. All 16 original issues closed." — contradicts line 150 which says "no Python workflow has been confirmed running in a cleat worker." Internal contradiction on whether Python WASM is "done."

## What needs to change (updated prioritization)

| # | Change | Priority | Est. effort |
|---|--------|----------|-------------|
| 1 | Fix AS binaryen ESM so tests run | P0 (release blocker) | 1-3h |
| 2 | Fix CI path in e2e-cross-language.yml: `./internal/host/...` → `./engine/...` | P0 | 5 min |
| 3 | Add `cleat_poll_child`, `cleat_await_any_child` to ABI.md | P1 | 15 min |
| 4 | Add 5 missing Python imports to `pythonExpectedImports` (or fix Go registration) | P1 | 15 min |
| 5 | Python WASM E2E validation (4h timebox per TASK.md) | P1 | ≤4h |
| 6 | Update LANGUAGE_SUPPORT.md: fix "15 imports" → "50 imports" | P2 | 5 min |
| 7 | Fix DX_COMPARISON.md double "end-to-end" typo + Python WASM contradiction | P2 | 15 min |
| 8 | Rust WASM integration test (compile + execute against cleat engine) | P2 | 1-3h |

## Risk assessment (unchanged from cleat-233e)

1. **AS binaryen fix** may be nontrivial if as-pect can't be patched. Fallback: replace as-pect with a different test runner.
2. **Python WASM E2E** — componentize-py is emerging tech. Unknown unknowns are the primary risk.
3. **Rust WASM E2E gap** — mock tests pass but ABI conformance unverified since ABI v5 expansion to 50 functions.

## Decomposition recommendation (CONFIRMED from cleat-233e)

Still valid: decompose into 3-4 child tasks:
- cleat-233a: AssemblyScript test fix
- cleat-233b: Python WASM E2E (timeboxed)
- cleat-233c: Rust WASM integration test
- cleat-233d: Documentation updates (LANGUAGE_SUPPORT.md, ABI.md, DX_COMPARISON.md)
