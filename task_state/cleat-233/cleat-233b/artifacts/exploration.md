# cleat-233b Exploration Report

**Date:** 2026-06-05
**Explorer:** cleat-233be
**Scope:** Python WASM end-to-end validation (PLAN.md §cleat-233b)

## 1. What's here now?

### PLANNED items vs actual state

The PLAN.md defines 5 steps for cleat-233b. Status of each:

| # | Planned step | Status |
|---|---|---|
| 1 | Fix CI path bug (`./internal/host/...` → `./engine/...`) | **Done** (in working tree) |
| 2 | Add 5 missing imports + 4 more to `pythonExpectedImports` | **Done** (in working tree) |
| 3 | Compile `hello_workflow.py` via componentize-py | **Verified working** |
| 4 | Load in cleat worker, execute minimal workflow | **Verified working** |
| 5 | 4h timebox; escalate if insoluble | **Not needed** |

### Toolchain

All prerequisites are installed and functional on this system:
- `componentize-py` 0.23.0 — available at `/home/rcownie/.local/bin/`
- `wasm-tools` 1.248.0 — available at `/home/rcownie/.cargo/bin/`
- Python 3.12.3
- Go 1.25.7

### Compilation

Both example workflows compile to WASM Component Model binaries:
- `hello_workflow.py` → 19.26 MB WASM (includes CPython)
- `durable_call_workflow.py` → 19.27 MB WASM

Metadata stamping produces a non-fatal warning ("unsupported WASM version") for Component Model binaries — stamp_metadata.py only handles WASM v1. This is cosmetic and doesn't affect execution.

### Execution (Go E2E tests)

Both Go tests pass:

```
TestPythonWasmEndToEnd     PASS (7.31s)
TestPythonWasmAbiBoundary  PASS (0.00s)
```

- **TestPythonWasmEndToEnd**: Full round trip verified — compiles durable_call_workflow.py, loads in wasmtime backend, executes, produces 1 EventTypeCall event with service="notifier", result is valid JSON.
- **TestPythonWasmAbiBoundary**: All 55 Python-expected imports match Go registered imports. Bit-packing conventions for cleat_call, sleep, await_signals, simple result, and export result all verified.

### 443 native Python SDK tests all pass (5.92s).

### Component Model execution path

The E2E test uses the wasmtime backend (CGO build tag). The execution flow:
1. `DetectLanguage` returns "python" (Component Model header bytes 0x0d 0x00 0x01 0x00)
2. `isComponentWasm` detects Component Model binary
3. `ExecuteComponentCGo` (native CGo) or `ExecuteComponent` (manual decomposition) handles the multi-module instantiation
4. The WIT "run" export dispatches to the `@cleat_entry` decorator via `WitWorld.run`

### wasm-tools decompose

wasm-tools 1.248.0 removed the `component decompose` subcommand. The build script (`build_wasm.py`) and test (`python_wasm_e2e_test.go`) both handle this gracefully:
- Build script: detects the error and prints a warning, skip is non-fatal
- Test: `canDecompose()` returns false for wasm-tools >= 1.230, test proceeds with the Component Model binary directly
- Wasmtime backend: `ExecuteComponentCGo` handles Component Model binaries natively without decomposition

## 2. What needs to change?

### Already changed (in working tree, uncommitted)

- `.github/workflows/e2e-cross-language.yml`: `./internal/host/...` → `./engine/...` (1 line)
- `engine/python_wasm_e2e_test.go`: Added 9 imports to both `pythonExpectedImports` and `registeredImportNames` (cleat_continue_as_new_versioned, cleat_child_workflow_in_schema, cleat_set_scope, cleat_get_scope, cleat_uuid, cleat_poll_child, cleat_await_any_child, cleat_json_parse, cleat_json_stringify)

### Still needed (documentation — cleat-233e scope, not cleat-233b)

- LANGUAGE_SUPPORT.md line 11: "Import 15 host functions" → "Import ~50 host functions"
- DX_COMPARISON.md line 23, 149-150: Remove doubled "end-to-end"
- DX_COMPARISON.md line 340 vs line 150: Resolve contradiction — Python WASM E2E IS now validated, update both lines

### Optional improvements

- Upgrade stamp_metadata.py to handle WASM Component Model binaries (currently produces a non-fatal warning)
- Add a wazero-only Python Component Model test for builds without CGO

## 3. What are the risks?

**Low risk.** The Python WASM E2E pipeline is functional. Remaining concerns:

1. **wasmtime backend requirement**: The E2E test requires CGO (wasmtime-go). The wazero component execution path in `engine.go:960-965` (`executeComponent`) is the fallback for builds without CGO. This path calls `wasm.ParseComponentBundle` — I did not test that this path successfully executes Python workflows. If wazero's component support is incomplete, Python WASM execution would be wasmtime-only.

2. **CPython WASM size**: 18.5 MB per workflow. Documented as expected but worth noting for cold-start latency.

3. **componentize-py version sensitivity**: The build script assumes componentize-py 0.13+ flag format (`-d`/`-w` at top level). Future breaking changes in componentize-py could break the pipeline.

## 4. What's the complexity?

**TRIVIAL — already done.** The remaining completion steps:
- Commit the staged changes (CI fix + import additions)
- That's it for cleat-233b scope

The documentation fixes (LANGUAGE_SUPPORT.md, DX_COMPARISON.md) belong to cleat-233e.

## 5. Recommendation

**Mark cleat-233b as complete.** All 5 planned steps are done:
1. CI path fix: in working tree
2. Import list fix: in working tree
3. WASM compilation: verified
4. E2E execution: verified (test passes)
5. Timebox: not needed

Remaining work is scoped to cleat-233d (ABI.md — already done) and cleat-233e (docs update). No code changes needed for cleat-233b.

## 6. Re-verification (cleat-233be, 2026-06-05)

Full re-verification of all cleat-233b deliverables:

### Test results (fresh run)
- **TestPythonWasmEndToEnd**: PASS (9.31s) — 1 EventTypeCall event, result `"\"{}\""`
- **TestPythonWasmAbiBoundary**: PASS (0.00s) — 55 imports aligned
- **TestRustWorkflowExecute**: PASS (0.18s) — 4 service calls
- **TestRustWorkflowReplay**: PASS (0.25s) — deterministic replay
- **TestRustWorkflowCancelOrder**: PASS (0.15s) — cancellation path
- **TestRustWorkflowCompensation**: PASS (0.16s) — saga compensation (5 calls)

### Code fixes (still in working tree, uncommitted)
- `.github/workflows/e2e-cross-language.yml:101`: `./internal/host/...` → `./engine/...` ✅
- `engine/python_wasm_e2e_test.go`: 9 imports added to both `pythonExpectedImports` and `registeredImportNames()` ✅

### ABI.md (from cleat-233d)
- `cleat_poll_child` at section 2.24 ✅
- `cleat_await_any_child` at section 2.25 ✅

### Verdict
**No regressions. All success criteria still met. cleat-233b remains complete.**
