# cleat-233 Exploration Report

**Date:** 2026-06-05
**Explorer:** cleat-233e

## 1. What's here now?

### Rust SDK (`crates/`)
- **cleat-sdk**: 1489-line `host_calls.rs` with 50+ WASM imports matching ABI.md v5. Mock-based test harness in `test.rs` (MockHostCalls, CleatTest). 32 tests pass.
- **cleat-macro**: proc-macro for `#[cleat_entry]`. 8 integration tests + 5 trybuild compile-fail tests. All pass.
- **cleat-test**: shared test utilities crate.
- No workspace Cargo.toml — each crate is standalone.
- No WASM-based integration tests exist. All Rust tests are host-target (mock-based).

### AssemblyScript SDK (`packages/cleat-as/`)
- SDK source: `assembly/index.ts`, `cleat-entry.ts`, `saga.ts`, `plugins.ts`, `json.ts`, `utils.ts`
- Tests: 3 spec files (smoke, json-host, json-saga) using as-pect 8.1.0
- JSON host import stubs in `as-pect.config.mjs` (cleat_json_parse, cleat_json_stringify)
- Test runner: `test_harness.ts` with MockHostCalls
- **Tests FAIL to run**: binaryen module uses top-level `await` in ESM, but as-pect loads it via `require()`. `ERR_REQUIRE_ASYNC_MODULE` on both Node 18 and Node 25.

### Python SDK (`python-sdk/`)
- SDK source: `cleat_sdk/` with host_calls.py, entry.py, memory.py, types.py, plugins.py, vet.py
- **443 tests pass** (5.72s). All native — host_calls are stubs that raise NotImplementedError outside WASM.
- WASM compilation pipeline: `scripts/build_wasm.py` uses `componentize-py componentize` + WIT world definition at `wit/cleat.wit`. Pipeline exists but end-to-end WASM execution has never been validated.
- Test files cover: memory (bit-packing), host_calls (behavioral + delegation), entry (decorator), wasm_compilation (pipeline validation), types (Saga/ChildWorkflow/Defer), vet (static analysis, 13 error codes), test_harness (stubs, signals, promises, children, state).

### Go baseline
- `go.mod` says 1.25.7. Several `wasm/` tests fail expecting 1.26.
- `webhookingest` test fails with 401 (auth infrastructure, not SDK).
- Several integration tests (`tests/cross-language`, `tests/integrity`, `tests/scale`, `tests/upgrade`) fail to build — unrelated to SDKs.

## 2. What needs to change?

| Area | Change | Priority |
|---|---|---|
| AS SDK | Fix binaryen ESM incompatibility so tests run | P0 |
| AS SDK | Triage and fix any test failures once tests run | P0 |
| Python SDK | Timeboxed (4h) end-to-end WASM validation: compile hello_workflow.py via componentize-py, load in cleat worker, execute | P1 |
| Rust SDK | Verify SDK WASM compiles and works with the engine (no current WASM integration tests) | P2 |
| LANGUAGE_SUPPORT.md | Update line counts, test pass/fail status, known limitations | P2 |
| Commit 1b7f8ed | Reviewed — wraps input in DispatchWrapper for Go target only. No SDK impact. | Done |

### Commit 1b7f8ed review
The change wraps workflow input in `{"inputJSON":"<escaped>"}` format for the WASM entry point in `backend_wasmtime.go`. This only affects the Go WASM path (guarded by `if lang == "go"`). No impact on Rust, AssemblyScript, or Python SDKs.

## 3. What are the risks?

1. **AS SDK binaryen incompatibility**: The fix may be nontrivial. Options:
   - Upgrade as-pect to a version that uses `import()` for binaryen (may not exist)
   - Downgrade binaryen to a CJS-compatible version
   - Set `"type": "module"` in package.json (may break other things)
   - Write a wrapper that imports binaryen via dynamic `import()`
2. **Python WASM E2E**: componentize-py is emerging tech. The 22 host import stubs are the critical gap. Even if compilation works, running inside a cleat worker may reveal:
   - WIT binding mismatches
   - Memory layout issues (the SDK uses scratch offsets that must match)
   - SuspendSentinel propagation failures
3. **Rust WASM E2E**: No WASM integration tests exist. The mock-based tests prove SDK logic but not ABI conformance. A WASM-compiled Rust workflow hasn't been tested against the actual cleat engine since ABI v5 (50 host functions, JSON parse/stringify).

## 4. What's the complexity?

**NOT leaf-ready**. Requires decomposition into at least 3 child tasks:
- AS SDK test fix (unknown complexity due to binaryen issue)
- Python WASM E2E validation (4h timebox, escalate if insoluble)
- Rust SDK WASM validation + LANGUAGE_SUPPORT.md update

## Recommendation

Decompose into 3-4 child tasks:

1. **cleat-233a**: Fix AssemblyScript test infrastructure (binaryen ESM + triage any test failures)
2. **cleat-233b**: Python WASM end-to-end validation (4h timebox, escalate to CTO if insoluble)
3. **cleat-233c**: Rust SDK WASM integration test (compile + execute against cleat engine)
4. **cleat-233d**: Update LANGUAGE_SUPPORT.md with verified status, line counts, limitations
