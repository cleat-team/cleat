# PLAN: cleat-233 — SDK Test Passes

**Planner:** cleat-233p
**Date:** 2026-06-05
**Phase:** planning → ready for decomposition

## Summary

Exploration (cleat-233e) and investigation (cleat-233i) are complete. Rust (32 tests) and Python (443 tests) pass natively. AssemblyScript tests fail to run (binaryen ESM). Python WASM E2E never validated. 5 documentation gaps found. Decompose into 5 child tasks, ordered by dependency and priority.

## Child Tasks

### cleat-233a: Fix AssemblyScript test infrastructure (P0, est. 1-3h)

**Problem:** binaryen@116 uses ESM with top-level `await`. as-pect loads it via `require()` → `ERR_REQUIRE_ASYNC_MODULE`.

**Fix options (ordered by preference):**
1. Pin binaryen to v110 (CJS-compatible) in packages/cleat-as/package.json
2. Set `"type": "module"` in AS package.json + rework as-pect config to use dynamic import
3. Replace as-pect with a different test runner

**Success criteria:**
- `npm test` or equivalent in `packages/cleat-as/` runs all 3 spec files without error
- All test assertions pass (or failures are triaged and fixed)
- CI step `test-assemblyscript` must NOT have `continue-on-error: true` (or remain with a tracked issue)

**Key files:**
- `packages/cleat-as/package.json`
- `packages/cleat-as/as-pect.config.mjs`
- `packages/cleat-as/assembly/__tests__/`

**Depends on:** nothing

### cleat-233b: Python WASM end-to-end validation (P1, ≤4h timebox)

**Problem:** Python WASM compilation pipeline exists but execution against a real cleat worker has never been validated. 5 ABI imports are missing from `pythonExpectedImports`.

**Steps:**
1. Fix CI path bug: `e2e-cross-language.yml` line 101 `./internal/host/...` → `./engine/...` (5 min)
2. Add 5 missing imports to `pythonExpectedImports` in `engine/python_wasm_e2e_test.go`: `cleat_continue_as_new_versioned`, `cleat_child_workflow_in_schema`, `cleat_set_scope`, `cleat_get_scope`, `cleat_uuid` (15 min)
3. Compile `hello_workflow.py` via componentize-py → .wasm
4. Load in cleat worker, execute a minimal workflow, verify it completes
5. Timebox to 4 hours total. If E2E can't succeed within 4h, document the blocker and escalate.

**Success criteria:**
- At least one Python workflow compiles to WASM and executes successfully in a cleat worker
- OR: documented limitation with specific blocker (escalation-ready)

**Key files:**
- `engine/python_wasm_e2e_test.go`
- `.github/workflows/e2e-cross-language.yml`
- `python-sdk/scripts/build_wasm.py`
- `python-sdk/wit/cleat.wit`

**Depends on:** nothing (parallel with 233a)

### cleat-233c: Rust SDK WASM integration test (P2, est. 1-3h)

**Problem:** All 32 Rust tests use mocks. No WASM-compiled Rust workflow has been tested against the real cleat engine since ABI v5 (50 host functions).

**Steps:**
1. Verify `cargo build --target wasm32-wasip1 --release` succeeds in `crates/cleat-sdk` (already confirmed)
2. Write a minimal Rust workflow using `#[cleat_entry]`
3. Compile to WASM, load in cleat worker, execute
4. If possible, wire into `engine/` Go test (like `TestPythonWasmEndToEnd` pattern)
5. If not possible within budget, document the gap and add a manual test script

**Success criteria:**
- At least one Rust workflow compiles to WASM and completes in a cleat worker
- OR: Go test that validates the WASM binary loads without error (smoke test)

**Key files:**
- `crates/cleat-sdk/src/host_calls.rs`
- `crates/cleat-macro/`
- `engine/backend_wasmtime.go`

**Depends on:** nothing (parallel with 233a, 233b)

### cleat-233d: Fix ABI.md omissions (P1, est. 15 min)

**Problem:** `cleat_poll_child` and `cleat_await_any_child` exist in Rust SDK `host_calls.rs` (comments say ABI 2.44/2.45) but are absent from ABI.md.

**Fix:** Add both functions to ABI.md in their correct slot positions (verify against `engine/imports.go` for definitive numbering).

**Success criteria:**
- ABI.md lists all 50+ host functions defined in `engine/imports.go`
- Cross-reference: `crates/cleat-sdk/src/host_calls.rs` ↔ `ABI.md` ↔ `engine/imports.go` agree

**Key files:**
- `ABI.md`
- `engine/imports.go`
- `crates/cleat-sdk/src/host_calls.rs`

**Depends on:** nothing

### cleat-233e: Update documentation (P2, est. 20 min)

**Problem:** LANGUAGE_SUPPORT.md has stale "15 imports" count (should be ~50). DX_COMPARISON.md has double "end-to-end" typo and internal contradiction about Python WASM completeness.

**Fixes:**
1. LANGUAGE_SUPPORT.md line 11: "Import 15 host functions" → "Import ~50 host functions"
2. DX_COMPARISON.md line 23, 149-150: Remove doubled "end-to-end"
3. DX_COMPARISON.md line 340: Update Python status to match reality (tests pass natively, WASM E2E status reflects cleat-233b outcome)

**Success criteria:**
- LANGUAGE_SUPPORT.md accurately reflects current line counts and test status for all 4 SDKs
- DX_COMPARISON.md has no internal contradictions about Python WASM readiness

**Key files:**
- `LANGUAGE_SUPPORT.md`
- `DX_COMPARISON.md`

**Depends on:** cleat-233a, cleat-233b, cleat-233c (to know final status for each SDK)

## Execution Order

```
Phase 1 (parallel, no dependencies):
  cleat-233a (AS test infra)    ← P0, start immediately
  cleat-233b (Python WASM E2E)  ← P1, 4h timebox
  cleat-233c (Rust WASM)        ← P2
  cleat-233d (ABI.md fix)       ← P1, quick win

Phase 2 (after Phase 1):
  cleat-233e (docs update)      ← P2, needs final status from all SDK tasks
```

## Budget Allocation

| Task | Est. | Priority | 
|------|------|----------|
| cleat-233a | 1-3h | P0 |
| cleat-233b | ≤4h (timeboxed) | P1 |
| cleat-233c | 1-3h | P2 |
| cleat-233d | 15m | P1 |
| cleat-233e | 20m | P2 |
| **Total est.** | **3-11h** | Within $15 budget |

## Invariants (from CONTRACT.md)

- Go SDK (native) continues to work — verify `go test ./...` still green after any SDK changes
- WASM ABI frozen for 0.5 — no new host calls, no signature changes
- Existing workflow tests not broken
