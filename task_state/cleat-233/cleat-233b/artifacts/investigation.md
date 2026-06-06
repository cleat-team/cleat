# cleat-233b Investigation Report

**Investigator:** cleat-233bi
**Date:** 2026-06-05
**Status:** COMPLETE — all cleat-233b deliverables independently verified

## 1. Scope

Independent verification of all claims made by cleat-233be (exploration), cleat-233bp (verification), and cleat-233br (review) reports for task cleat-233b (Python WASM E2E validation).

## 2. Code Changes Verified

### 2a. CI path fix

**File:** `.github/workflows/e2e-cross-language.yml:101`

- Confirmed: `./internal/host/...` → `./engine/...`
- Verified no other stale `./internal/host/` paths remain in this file
- Verify: `engine/` directory exists and contains `python_wasm_e2e_test.go`, `rust_workflow_test.go`
- **Verdict:** CORRECT

### 2b. Python import additions

**File:** `engine/python_wasm_e2e_test.go`

- `pythonExpectedImports`: 54 entries total. 9 added: `cleat_continue_as_new_versioned`, `cleat_child_workflow_in_schema`, `cleat_set_scope`, `cleat_get_scope`, `cleat_uuid`, `cleat_poll_child`, `cleat_await_any_child`, `cleat_json_parse`, `cleat_json_stringify`
- `registeredImportNames()`: 54 entries total. 4 added: `cleat_poll_child`, `cleat_await_any_child`, `cleat_json_parse`, `cleat_json_stringify` (the other 5 were already present)
- **Note:** The review report says "9 imports added to both lists" — only 4 were added to `registeredImportNames()`; the other 5 pre-existed. This does not affect correctness.
- **Verdict:** CORRECT

### 2c. ABI.md additions (from cleat-233d)

- `cleat_poll_child` at §2.24: 4-param signature `(i32, i32, i32, i32) -> i64` matches `engine/imports.go:402`
- `cleat_await_any_child` at §2.25: 4-param signature `(i32, i32, i32, i32) -> i64` matches `engine/imports.go:414`
- Section renumbering §2.26–§2.54 verified correct
- Cross-references in changelog, §§6.1, 6.3, 6.4, 6.6 verified updated
- **Verdict:** CORRECT

## 3. Test Results (Independent Re-Execution)

### Python WASM

| Test | Status | Time | Details |
|------|--------|------|---------|
| TestPythonWasmAbiBoundary | PASS | 0.00s | 54 host imports match |
| TestPythonWasmEndToEnd | PASS | 7.58s | 1 call event, result `"{}"` |

### Cross-Language E2E

| Test | Status | Time |
|------|--------|------|
| TestRustWorkflowExecute | PASS | 0.16s |
| TestRustWorkflowReplay | PASS | 0.19s |
| TestRustWorkflowCancelOrder | PASS | 0.17s |
| TestRustWorkflowCompensation | PASS | 0.16s |
| TestAssemblyScriptWorkflowExecute | PASS | 3.88s |
| TestJavaWorkflowExecute | SKIP | 3.89s (Gradle/TeaVM, expected) |

**No regressions.** All results match the exploration and verification reports within normal variance.

## 4. Undocumented Issues Found

### 4a. ABI.md §§2.20–2.21 missing `priority: i64` parameter

The formal WASM signatures and parameter tables in ABI.md for `cleat_child_workflow_with_options` (§2.20) and `cleat_child_workflow_in_schema` (§2.21) omit the `priority: i64` parameter. Both `engine/imports.go` and `crates/cleat-sdk/src/host_calls.rs` include it.

| Source | `_with_options` params | `_in_schema` params |
|---|---|---|
| engine/imports.go | 10 (includes priority) | 12 (includes priority) |
| Rust host_calls.rs | 10 (includes priority) | 12 (includes priority) |
| **ABI.md** | **9** (missing priority) | **11** (missing priority) |

The prose at line 553 mentions "5 params: name, input, version, parentClosePolicy, priority" in passing, but the formal WASM signatures and parameter tables do not reflect this. A new SDK implementer following ABI.md would produce a WASM module with wrong import signatures — the engine would reject it with a link error.

**Severity:** MEDIUM — correctness bug for non-Go SDK implementers.
**Recommendation:** Fix in cleat-233e (docs update task) or a dedicated follow-up. Trivial 15-min fix.

### 4b. `schedule_invoke` naming mismatch

Rust SDK imports `schedule_invoke` (no `cleat_` prefix, `host_calls.rs:194`) but engine exports `cleat_schedule_invoke` (`imports.go:707`). Pre-existing bug, not introduced by cleat-233b. Already noted in cleat-233d/STATUS.md as out-of-scope.

**Severity:** MEDIUM — blocks Rust WASM workflows that call `schedule_invoke`.
**Recommendation:** Fix in cleat-233c (Rust WASM integration task). Trivial 5-min fix (add `#[link_name = "cleat_schedule_invoke"]` or rename).

### 4c. Documentation miscount in STATUS.md

The STATUS.md and verification.md report "55 imports aligned" but the actual count is 54 entries in both `pythonExpectedImports` and `registeredImportNames()`. The review report correctly flagged this as a miscount.

**Severity:** TRIVIAL — cosmetic only.
**Recommendation:** Fix in cleat-233e (docs update task).

## 5. Cross-Report Consistency

| Claim | cleat-233be | cleat-233bp | cleat-233br | cleat-233bi |
|-------|-------------|-------------|-------------|-------------|
| CI path fix correct | YES | YES | YES | YES |
| Import list complete | YES (55) | YES (55) | YES (54) | YES (54) |
| ABI.md additions correct | YES | YES | YES | YES |
| Python E2E passes | YES (8.65s) | YES (9.62s) | (not run) | YES (7.58s) |
| No regressions | YES | YES | YES | YES |

The "55" vs "54" discrepancy across reports is a propagated miscount. The code is correct (54 entries).

## 6. CONTRACT.md Invariant Check

| Invariant | Status |
|-----------|--------|
| Go SDK (native) continues to work | Not directly tested but engine tests all pass |
| WASM ABI frozen for 0.5 — no new host calls, no signature changes | CONFIRMED — no signature changes in this task |
| Existing workflow tests not broken | CONFIRMED — all E2E tests pass |

## 7. Recommendation

**cleat-233b is complete and verified.** All 5 planned steps are done and independently confirmed:

1. CI path fix: in working tree, verified correct
2. Import list fix: in working tree, 54/54 entries aligned
3. WASM compilation: verified working (18.38 MB component)
4. E2E execution: verified (test passes, 1 event, valid JSON output)
5. Timebox (4h): not needed — everything works

**Three issues remain for follow-up tasks:**
- **ABI.md priority param** → cleat-233e (docs)
- **schedule_invoke naming** → cleat-233c (Rust WASM)
- **STATUS.md miscount (55→54)** → cleat-233e (docs)

None of these are blocking for cleat-233b completion. All are already tracked.
