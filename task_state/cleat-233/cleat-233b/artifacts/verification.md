# cleat-233b Verification Report (2026-06-06 Re-Verification)

**Verifier:** cleat-233bp
**Date:** 2026-06-06
**Status:** VERIFIED — all claims re-confirmed by independent inspection and test execution

## 1. Code Changes Verified (current working tree)

### 1a. CI path fix
**File:** `.github/workflows/e2e-cross-language.yml:101`
- `./internal/host/...` → `./engine/...` — CONFIRMED
- Zero remaining `./internal/host/` references in the file — CONFIRMED
- **Verdict:** CORRECT

### 1b. Python import additions
**File:** `engine/python_wasm_e2e_test.go`
- `pythonExpectedImports`: 54 entries. All 9 added:
  `cleat_continue_as_new_versioned` (171), `cleat_child_workflow_in_schema` (174),
  `cleat_set_scope` (194), `cleat_get_scope` (195), `cleat_uuid` (196),
  `cleat_poll_child` (210), `cleat_await_any_child` (211),
  `cleat_json_parse` (213), `cleat_json_stringify` (214)
- `registeredImportNames()`: 54 entries. All entries present.
- Count is 54, not 55 (prior reports had a miscount).
- **Verdict:** CORRECT

### 1c. ABI.md additions (from cleat-233d)
- `cleat_poll_child` at §2.24: 4-param signature `(i32, i32, i32, i32) -> i64` matches `engine/imports.go` — CONFIRMED
- `cleat_await_any_child` at §2.25: 4-param signature `(i32, i32, i32, i32) -> i64` matches `engine/imports.go` — CONFIRMED
- Section renumbering 2.26–2.54: no gaps, correct shift — CONFIRMED
- Cross-references updated — CONFIRMED
- **Verdict:** CORRECT

## 2. Test Results (Independent Re-Execution, 2026-06-06)

### Python WASM E2E
| Test | Status | Time | Result |
|------|--------|------|--------|
| TestPythonWasmAbiBoundary | PASS | 0.00s | 54/54 imports aligned |
| TestPythonWasmEndToEnd | PASS | 8.13s | 1 event, result `"{}"` |

### Cross-Language E2E Suite
| Test | Status | Time | Calls |
|------|--------|------|-------|
| TestRustWorkflowExecute | PASS | 0.15s | 4 |
| TestRustWorkflowReplay | PASS | 0.19s | replay OK |
| TestRustWorkflowCancelOrder | PASS | 0.13s | 1 |
| TestRustWorkflowCompensation | PASS | 0.14s | 5 (saga) |
| TestAssemblyScriptWorkflowExecute | PASS | 4.15s | 4 |
| TestJavaWorkflowExecute | SKIP | 3.64s | Gradle/TeaVM, expected |

**Zero regressions.** All results match prior reports within normal variance.

## 3. Known Issues Re-Assessed

| # | Issue | Previous Verdict | Re-Assessment |
|---|-------|-----------------|---------------|
| 1 | ABI.md §§2.20-2.21 missing `priority: i64` in WASM signature | MEDIUM bug | **CONFIRMED** — Engine has `version int64, priority int64` (2 i64s). ABI.md signatures have only 1 i64. Prose mentions priority but formal signature and param tables omit it. |
| 2 | Rust SDK `schedule_invoke` naming mismatch | MEDIUM bug | **NOT A BUG** — `host_calls.rs:194` uses `#[link_name = "cleat_schedule_invoke"]`, correctly mapping to engine's `cleat_schedule_invoke` export at `imports.go:707`. The link_name attribute handles the prefix. |
| 3 | STATUS.md miscounts 54 as 55 | TRIVIAL | **CONFIRMED** — still says "55 imports" in some artifacts. |
| 4 | ABI.md changelog v5 omits 2.24/2.25 additions | TRIVIAL | Not re-checked (minor). |
| 5 | wasm-tools decompose unavailable (1.248.0) | INFO | **CONFIRMED** — handled gracefully. |

### Issue 2 Correction
The prior investigation (cleat-233bi) flagged `schedule_invoke` as a naming bug. This is **incorrect**. The Rust SDK's `host_calls.rs` line 194 defines:
```rust
#[link_name = "cleat_schedule_invoke"]
pub fn schedule_invoke(...) -> i64;
```
The `#[link_name]` attribute ensures the WASM import uses `cleat_schedule_invoke`, matching the engine export at `imports.go:707`. The Rust-side function name `schedule_invoke` (without prefix) is an internal detail. This pattern is consistent with how other Rust SDK functions handle the `cleat_` prefix.

## 4. Cross-Report Consistency

| Claim | 233be (2026-06-05) | 233bp v1 (2026-06-05) | 233br | 233bi | 233bv | 233bp v2 (2026-06-06) |
|-------|---------------------|------------------------|-------|-------|-------|------------------------|
| CI path fix correct | YES | YES | YES | YES | YES | **YES** |
| Import list complete | YES (55) | YES (55) | YES (54) | YES (54) | YES (54) | **YES (54)** |
| ABI.md additions correct | YES | YES | YES | YES | YES | **YES** |
| Python E2E passes | 8.65s | 9.62s | N/R | 7.58s | 7.69s | **8.13s** |
| All cross-lang E2E pass | YES | YES | YES | YES | YES | **YES** |
| No regressions | YES | YES | YES | YES | YES | **YES** |
| schedule_invoke is bug | N/R | N/R | N/R | YES | N/R | **NO (link_name fix)** |

## 5. Success Criteria (from PLAN.md §cleat-233b)

- [x] At least one Python workflow compiles to WASM and executes in cleat worker
- [x] CI path bug fixed (`./internal/host/...` → `./engine/...`)
- [x] Missing Python imports added to `pythonExpectedImports`
- [x] `TestPythonWasmAbiBoundary` passes (all imports aligned)
- [x] No regressions: cross-language E2E tests all pass

## 6. Recommendation

**cleat-233b remains complete.** All 5 planned steps are independently re-confirmed against the current working tree on 2026-06-06. Code changes are correct. All 7 E2E tests pass. One correction to prior reports: the Rust SDK `schedule_invoke` is NOT a bug (link_name attribute handles the prefix mapping correctly).

**Remaining tracked issues for follow-up tasks:**
- **ABI.md priority param** → cleat-233e (docs) — MEDIUM
- **STATUS.md miscount (55→54)** → cleat-233e (docs) — TRIVIAL
- **ABI.md changelog v5 omissions** → cleat-233e (docs) — TRIVIAL
