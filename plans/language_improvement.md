# Multi-Language SDK Production Readiness — Implementation Plan

## Bottom Line

Four non-Go SDKs at varying maturity levels. Two have **P0 blocking ABI bugs** that break WASM instantiation (Rust, Java). Python is closest to production with mostly packaging/testing gaps. AssemblyScript has a broken transform pass and untested harness.

All four streams are independent and can be worked in parallel.

---

## Stream P: Python SDK → Production (est. 1 day)

### P1 — Fix naming mismatch (BLOCKING)

The SDK code uses `cleat_*` prefix. The README uses `durable_*` prefix. Every code example
in the README will fail at runtime.

**Decision needed:** `cleat_*` or `durable_*`?
- `cleat_*` is already in the WASM ABI and all host imports — renaming the SDK is at least a day's work across ABI.md, all SDKs, all examples, all plugin stubs
- `durable_*` is more user-friendly but would require renaming every host import

**Recommended:** Keep `cleat_*` in code, fix README to match. File: `python-sdk/README.md`.

### P2 — PyPI packaging (HIGH)

1. **Fix build backend** — `pyproject.toml`: replace `setuptools.backends._legacy:_Backend` with `setuptools.build_meta`
2. **Add `include_package_data = true`** — without this, `wit/*.wit` and generated `_wit/` stubs are excluded from the wheel
3. **Add `[tool.setuptools.package-data]`** for auto-generated stubs
4. **Add `py.typed` marker** — empty file at `python-sdk/cleat_sdk/py.typed`
5. **Add `long_description_content_type`** for PyPI README rendering
6. **Create `publish-pypi.yml`** CI workflow (trusted publisher OIDC pattern)

### P3 — Testing gaps (HIGH)

1. **Add `test_vet.py`** — vet.py is 987 lines with 13 error codes and zero tests. Cover each error code with sample snippets.
2. **Install `componentize-py` in CI** — currently WASM compilation tests silently pass with zero coverage because the dependency is missing
3. **Add WASM compilation integration test** — write a minimal `@cleat_entry` workflow, run `build_wasm.py`, verify `.wasm` output
4. **Fix Makefile `test-wasm`** — line 98 `|| echo "No WASM tests found"` swallows real test failures

### P4 — Makefile improvements (MEDIUM)

1. Add `test`, `lint`, `coverage`, `build-dist`, `publish` targets
2. Align `build_wasm.py` componentize-py CLI syntax with Makefile (modern vs legacy flags)

### P5 — Documentation gaps (LOW)

1. Document `CleatTestHarness` in README (currently only `CleatClient` is documented)
2. Document or remove `virtual_object` decorator export

---

## Stream R: Rust SDK → Production (est. 2–3 days)

### R0 — P0 BREAKING: Fix ABI mismatches (BLOCKING)

These will cause WASM instantiation failures for any workflow using the affected functions:

1. **`cleat_call_with_retry` ABI mismatch** — `crates/cleat-sdk/src/host_calls.rs` lines 210-217.
   The Rust SDK declares `cleat_call_with_retry` with a single JSON-retry-policy string.
   The Go host exports `cleat_call_retry` with 4 individual i64 params (maxAttempts,
   initialIntervalMs, backoffCoefficient100x, maxIntervalMs) + nonRetryableErrorsJSON.
   **Fix:** Change Rust import to match Go's typed-param ABI. Update wrapper method.

2. **Unregistered WASM imports** — 8 imports (`cleat_run_detached`, `cleat_set_state`,
   `cleat_get_state`, `cleat_delete_state`, `cleat_incr_state`, `cleat_has_state`,
   `cleat_list_state`, `cleat_fetch`) have NO matching exports in
   `internal/host/imports.go`. These will fail WASM instantiation with unresolved-import
   errors.
   **Fix:** Add corresponding WASM exports to `internal/host/imports.go`, OR remove
   these from Rust SDK and reimplement as Go-level helpers in the proc-macro.

3. **`ChildWorkflowWithOptions` param count mismatch** — Rust passes 3 strings + 1 i32
   to `cleat_child_workflow_with_options`. Go host takes 4 strings + 1 i64 (the extra
   string is `parentClosePolicy`). **Fix:** Add `parentClosePolicy` param to Rust's
   WASM import and `ChildWorkflowOptions` struct.

### R1 — Missing host imports (HIGH)

Add WASM import + wrapper for each Go export the Rust SDK doesn't bind:

4. `cleat_continue_as_new_versioned` — `internal/host/imports.go` line 168
5. `cleat_call_heartbeat` — ABI 2.17, `internal/host/imports.go` line 240
6. `cleat_side_effect` — `internal/host/imports.go` line 394
7. `plugin_call_streaming` — `internal/host/imports.go` line 263

### R2 — Test harness gaps (HIGH)

8. **Add mock methods** — `acquire_lock_ms`, `release_lock`, `cleat_fetch`, `uuid`,
   `schedule_invoke_ms`, `child_workflow_with_options` are missing from `MockHostCalls`
9. **Add saga.rs tests** — 75 lines, zero tests. Cover: all-success, compensation-on-failure,
   multi-step ordering, error-in-compensation
10. **Add compile-fail tests for cleat-macro** — verify the proc-macro rejects async fn,
    non-Result return types, wrong param types, destructuring

### R3 — CI and build (HIGH)

11. **Add `cargo test` to main CI** — currently only `ecosystem-ci.yml` runs it, and
    that has a restricted path trigger. PRs can break Rust tests silently.
12. **Add WASM compilation test to CI** — `cargo build --target wasm32-wasip1 --release`
    for the example workflow
13. **Add `build.rs` for version constants** — `version.rs` hardcodes `WORKFLOW_VERSION = 1`
    because const-evaluation can't parse env vars. A `build.rs` with `cargo:rustc-env=`
    directives fixes this.
14. **Add `publish = false`** to both Cargo.toml files to prevent accidental publication

### R4 — Benchmarks (MEDIUM)

15. Create `crates/cleat-sdk/benches/` with: message encoding throughput, HostCalls
    wrapper overhead, JSON serialization cost. Wire into `[[bench]]` target.

---

## Stream AS: AssemblyScript SDK → Production (est. 2–3 days)

### AS1 — Fix transform pass (BLOCKING)

1. **Fix `_isHostCall` obsolete method names** — `packages/cleat-as/transform/index.js`.
   The determinism checker looks for `durableCall`, `durableSleep`, `durableLog`,
   `UUID`, `setEventCallback` — none of these match the current `HostCalls` class
   (which uses `cleatCall`, `cleatSleep`, `log`, `uuid`, etc.). The static analysis
   for non-determinism is effectively broken.

### AS2 — Missing ABI imports (HIGH)

2. **Add `cleat_call_retry`** — ABI 2.16. Raw `@external("env", "cleat_call_retry")`
   with 4 individual i64 params + nonRetryableErrorsJSON, plus typed wrapper method
3. **Add `cleat_call_heartbeat`** — ABI 2.17. Raw import with heartbeatIntervalMs i64,
   plus wrapper method
4. **Add `get_query_state`** — the SDK has `set_query_state` but not `get_query_state`

### AS3 — Test and CI gaps (HIGH)

5. **Write tests for MockHostCalls/TestEnv harness** — the harness exists but has zero
   tests. Create an as-pect test that registers stubs, runs a workflow, and asserts
   `assertCalled`/`callCount`
6. **Add end-to-end WASM compilation test to CI** — compile the as-workflow example
   to WASM and verify valid output with expected exports
7. **Fix example to use `@cleatEntry` decorator** — `examples/as-workflow/` bypasses
   the decorator and uses direct ABI exports with manual JSON parsing

### AS4 — npm packaging (MEDIUM)

8. **Fix `package.json` `files` field** — add `transform/` directory (currently excluded
   from npm publish, so users can't use `--transform @cleat/transform`)
9. **Replace `isReplaying()` stub** — currently returns `false` with TODO. Either add
   `cleat_is_replaying` to the host ABI or document the sleep-return-value pattern

---

## Stream J: Java SDK → Production (est. 2–3 days)

### J1 — Fix ABI mismatches (BLOCKING)

1. **`cleat_call_retry` wrong import name + param layout** — imports
   `cleat_call_with_retry` with JSON blob params. ABI 2.16 specifies
   `cleat_call_retry` with 4 individual i64 params + nonRetryableErrorsJSON.
   Fix import name and parameter layout.
2. **`awaitChild` doesn't handle suspension sentinel** — the AS SDK checks
   `(result & (1 << 62)) != 0` for `Suspend` signal. Java does not, so it
   will read garbage from the output buffer on incomplete children. Add
   suspension detection.

### J2 — Determinism bugs (BLOCKING)

3. **`awaitSignalsWithQuorumMs` uses `System.currentTimeMillis()`** —
   `HostCalls.java` ~line 2190. This returns wall-clock time that differs
   on replay, breaking workflow determinism. Replace with `this.now()`.

### J3 — Missing host imports (HIGH)

4. **Add `cleat_call_heartbeat`** — ABI 2.17. `@Import` declaration + wrapper method

### J4 — Test gaps (HIGH)

5. **Write tests for TestHostCalls** — JUnit 5 tests that instantiate `TestHostCalls`,
   register stubs, call `cleatCall`, assert `assertCalled`/`callCount`
6. **Add annotation processor test** — `CleatEntryProcessor` is untested. Use
   `javax.tools.JavaCompiler` or google/compile-testing to verify generated output

### J5 — Publishing and build (MEDIUM)

7. **Configure Maven Central publish** — add `maven-publish` + `signing` plugins,
   POM metadata, Javadoc/source JARs
8. **Switch build optimization to `FULL`** — currently `BALANCED`; production
   WASM should use `FULL`
9. **Make `acquireLock`/`releaseLock`/`pluginCall` return `CleatResult`** —
   currently throw `RuntimeException` while every other method returns `CleatResult<T>`

---

## Cross-Cutting (all streams)

### C1 — ABI.md is out of date

The ABI spec documents 22 functions but 40+ imports exist across SDKs (state ops,
locks, fetch, cron, signal_send+wait, run_detached, register_query_handler, etc.).
Update ABI.md to document all host imports or split into core + extensions.

### C2 — No cross-language end-to-end CI test

No CI compiles a workflow from any non-Go language and runs it against the cleat
host runtime. Add a CI job that: compiles Rust/Python/AS/Java examples to WASM →
loads into Go host → executes → verifies event history.

---

## Stream Independence

```
Stream P (Python)     — 1 day,  python-sdk/ only
Stream R (Rust)       — 2-3 days, crates/cleat-sdk/ + crates/cleat-macro/ + internal/host/imports.go
Stream AS (Assembly)  — 2-3 days, packages/cleat-as/ only
Stream J (Java)       — 2-3 days, crates/cleat-java/ only
Cross-cutting          — 1 day,  ABI.md + CI workflows
```

No file conflicts between streams. Stream R touches `internal/host/imports.go` —
no other stream touches Go host files.

## Execution Order

```
Day 1: All four streams start in parallel
  ├─ Stream P: naming fix + package config + Makefile targets
  ├─ Stream R R0: fix the 3 blocking ABI bugs
  ├─ Stream AS AS1: fix transform pass obsolete names
  └─ Stream J J1-J2: fix ABI mismatches + determinism bug

Day 2: Testing and CI
  ├─ Stream P: vet tests, WASM CI, Makefile targets
  ├─ Stream R: missing imports + mock methods + CI cargo test
  ├─ Stream AS: test harness tests + WASM compilation CI
  └─ Stream J: TestHostCalls tests + annotation processor test

Day 3: Polish and publish
  ├─ Stream P: PyPI publish workflow, doc updates
  ├─ Stream R: build.rs + benchmarks + publish = false
  ├─ Stream AS: npm publish fixes, example fix
  └─ Stream J: Maven publish config + build optimization
```
