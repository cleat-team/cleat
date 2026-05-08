# Cleat Test Improvement Plan

## Context

Audit of 74 Go test files, 7 Python test files, 3 Java test files, 30 Rust test functions across the monorepo found:

- Core runtime & WASM host engine tests are solid (runtime_test.go, host_test.go, compaction_fuzz_test.go)
- AssemblyScript tests are **broken** (as-pect not installed, `npm test` crashes)
- Web dashboard (Svelte) has **zero tests**
- Auth package has **zero tests**
- 3 "fault" test files don't actually simulate faults (disk, clock, db-connection-loss)
- Most plugin tests only verify Info() constants and route existence — not business logic
- Python SDK tests are ~40% `assert callable(host.method)` — not behavioral
- No coverage tracking anywhere in the project
- Critical WASM/DB tests silently skip when dependencies missing

Full details: see inline comments in each action item below.

---

## Action Items

### 1. Fix AssemblyScript test infrastructure (BROKEN)

**Files:** `packages/cleat-as/package.json`, new `packages/cleat-as/as-pect.config.js`

**What:** `npm test` runs `asp` which doesn't exist. `@as-pect/cli` is not in devDependencies. The 1627-line test harness at `packages/cleat-as/test_runner/test-harness.ts` has never been runnable. CI job `test-assemblyscript` is a silent no-op.

**Steps:**
- Add `@as-pect/cli` and `@as-pect/core` to devDependencies in `packages/cleat-as/package.json`
- Create `as-pect.config.js` pointing at the test harness
- Run `npm test` and fix any import errors in `test-harness.ts`
- Verify CI job `test-assemblyscript` actually executes tests and fails on failure

**Done when:** `npm test` in `packages/cleat-as/` runs tests and at minimum the test harness's own tests pass.

**Effort:** S (2-4 hours, mostly dependency/config wiring)

---

### 2. Delete or fix fake "fault" tests

**Files:** `internal/host/fault_disk_test.go`, `internal/host/fault_clock_test.go`, `internal/host/fault_test.go` (TestFaultDBConnectionLoss only)

**What:** Three test scenarios claim to test failure modes but don't:
- `fault_disk_test.go` — "disk full" writes 1MB and declares success; "slow disk" adds `time.Sleep(500ms)` in test code
- `fault_clock_test.go` — "clock skew" and "time jump" just set DB timestamps, don't test actual skew
- `TestFaultDBConnectionLoss` — comment admits "We can't actually kill the DB connection"

**Steps:**
- Delete `fault_disk_test.go` entirely (disk-full needs loopback device or tmpfs with quota — out of scope for now)
- Delete `fault_clock_test.go` entirely (clock skew needs libfaketime or container clock manipulation — out of scope)
- Delete `TestFaultDBConnectionLoss` from `fault_test.go`
- File follow-up issues for real disk/clock fault injection if needed

**Done when:** The three fake test files/functions are removed. `go test ./internal/host/...` still passes with remaining genuine tests.

**Effort:** S (1 hour, deletions only)

---

### 3. Delete redundant test file

**Files:** `internal/plugin/capabilities_enforcement_test.go`

**What:** Every test case in this file duplicates tests already in `capabilities_test.go`. Four tests calling `ValidateCapabilities` with the same arguments already covered.

**Steps:**
- Verify no unique assertions exist in `capabilities_enforcement_test.go` (already confirmed)
- Delete the file

**Done when:** File removed. Tests still pass.

**Effort:** S (30 min)

---

### 4. Fix tautological assertions in embedded/runner_test.go

**File:** `cleat/embedded/runner_test.go`

**What:** 
- `TestWorkflowContextHasHostCalls` (line 59): writes `{"ok":true}` as output, then asserts output is `{"ok":true}`. The test function itself wrote the value it's checking. Replace with a test that exercises actual HostCalls methods (DurableLog, Now, etc.) and verifies they produce expected side effects.
- `TestDurableCallWorks` (line 101): asserts `{"result":"ok"}` but this is the runtime's default mock behavior, not DurableCall routing. Replace with a test that registers a mock via `OnCall` and verifies the correct mock is invoked.
- `TestNewRunnerCreatesNonNil`: acceptable as-is (guard against nil regression).

**Steps:**
- Rewrite `TestWorkflowContextHasHostCalls` to call `h.DurableLog("test")`, `h.Now()`, etc. and verify output
- Rewrite `TestDurableCallWorks` to register `OnCall("svc", "op", ...)` and verify the response matches
- Remove the "expects specific string from mock" assertion pattern

**Done when:** Tests verify actual behavior, not tautologies.

**Effort:** S (2 hours)

---

### 5. Fix TestRustWorkflowCompensation assertion

**File:** `internal/host/rust_workflow_test.go`

**What:** Line 207-208 logs when `err == nil` instead of using `t.Error`/`t.Fatal`. The test passes regardless of whether compensation returns an error. The real assertions (foundRefund, foundRelease) are on lines 226-231 but the error-path assertion is missing.

**Steps:**
- Change `if err == nil { t.Log(...) }` to `if err == nil { t.Error("expected error from compensation path, got nil") }`

**Done when:** Test correctly fails if compensation doesn't produce an error.

**Effort:** S (15 min)

---

### 6. Consolidate test schema setup into a shared helper

**New file:** `internal/host/testutil/schema.go`

**Files to modify:**
- `internal/host/fault_test.go` (testDB function — extract shared parts)
- `internal/host/concurrency_test.go` (5 copies of CREATE TABLE concurrency_keys)
- `internal/host/integration_test.go` (setupFullTestSchema)
- `internal/host/fault_network_test.go`
- `internal/host/fault_disk_test.go` (if kept)
- `internal/host/fault_clock_test.go` (if kept)

**What:** The `concurrency_keys` CREATE TABLE is copy-pasted verbatim into 5 test functions. `testDB()` runs 15+ DDL statements every call. Multiple files define their own schema variants. A schema change requires hunting through 10+ files.

**Steps:**
- Create `internal/host/testutil/schema.go` with:
  - `SetupMinimalSchema(t, db)` — tables needed by all DB tests
  - `SetupFullSchema(t, db)` — all tables including RLS, signals, schedules
  - `CleanupTestData(t, db, runID)` — standard cleanup
- Migrate `concurrency_test.go` to use the helper (remove 5 duplicate CREATE TABLE blocks)
- Migrate `integration_test.go` to use `SetupFullSchema` instead of inline `setupFullTestSchema`
- Migrate `fault_test.go` to use `SetupMinimalSchema` instead of inline DDL in `testDB()`

**Done when:** Zero CREATE TABLE statements remain in test functions. All test schema managed in one place.

**Effort:** M (4-6 hours, many files to touch but mechanical)

---

### 7. Make WASM and DB tests mandatory in CI

**Files:** `.github/workflows/ci.yml`, `Makefile`

**What:** Critical WASM tests silently skip if TinyGo isn't installed. Rust WASM tests skip without cargo. DB tests skip if `CLEAT_TEST_DB` env var isn't set. `go test -short` skips 15+ tests. A developer can run `go test ./...` and get PASS with zero actual tests executed.

**Steps:**
- Add TinyGo installation step to CI `test-go` job (or dedicated `test-wasm` job)
- Add cargo + wasm32-wasip1 target installation for Rust WASM tests
- Add PostgreSQL service container to CI and set `CLEAT_TEST_DB`
- Remove `-short` flag from CI test invocations (keep it for local dev convenience)
- Add a CI check that counts skipped tests and fails if skip count exceeds threshold

**Done when:** CI runs WASM compilation tests, Rust cross-language tests, and DB integration tests on every PR. No silent skipping.

**Effort:** M (4-8 hours, CI debugging)

---

### 8. Add coverage tracking

**Files to create/modify:**
- `Makefile` (add coverage targets)
- `.github/workflows/ci.yml` (add coverage job)
- `.gitignore` (already has coverage patterns)

**What:** Zero coverage measurement exists. No `-coverprofile` flags, no `--cov` flags, no thresholds, no coverage reports.

**Steps:**
- Add `make coverage` target: `go test -coverprofile=coverage.out -covermode=atomic ./...`
- Add `make coverage-python` target: `pytest --cov=cleat_sdk --cov-report=html`
- Add `make coverage-report` that prints per-package percentages
- Add CI job that runs coverage and uploads results
- Set initial thresholds: 70% for `cleat/`, 60% for `internal/host/`, 50% for `plugins/`, 50% for `internal/plugin/`
- Do NOT gate PRs on thresholds yet (too many gaps — introduce gating after Phase 3)

**Done when:** `make coverage` produces a coverage report. CI runs it. Thresholds are recorded but not yet blocking.

**Effort:** M (3-5 hours)

---

### 9. Add auth tests (SECURITY-CRITICAL)

**New files:** `internal/auth/middleware_test.go`, `internal/auth/tenant_store_test.go`

**What:** `internal/auth/middleware.go` and `tenant_store.go` have zero tests. This is authentication/authorization code. If it breaks, tenants can access each other's data.

**Steps for middleware_test.go:**
- Test that requests without auth headers get 401
- Test that requests with valid tokens set tenant context
- Test that requests with invalid/expired tokens get 403
- Test that requests with malformed tokens don't panic
- Test that the middleware passes tenant ID through to downstream handlers
- Test RLS session variable is set correctly

**Steps for tenant_store_test.go:**
- Test tenant creation, lookup, deletion
- Test tenant listing with pagination
- Test tenant ID uniqueness enforcement
- Test that tenants are isolated (tenant A can't see tenant B)

**Done when:** Auth middleware has test coverage for all HTTP status paths (200, 401, 403) and tenant store CRUD operations are tested.

**Effort:** M (6-10 hours, security-critical code needs careful tests)

---

### 10. Add behavioral tests for high-value plugins

**Files to create/modify:**
- `plugins/kvstore/kvstore_test.go` (rewrite, currently 91 lines of trivial tests)
- `plugins/eventstore/eventstore_test.go` (add functional tests)
- `plugins/notifications/notifications_test.go` (add functional tests)

**What:** Most plugin tests only verify `Info()` returns correct name/version, `Init()` doesn't crash, and `RegisterRoutes()` creates route patterns. Zero functional testing of plugin behavior. Model to follow: `plugins/blobstore/blobstore_test.go`.

**Steps for kvstore:**
- Create in-memory store via test helper (like blobstore's testMemBackend)
- Test PUT/GET/DELETE key lifecycle
- Test listing by prefix
- Test TTL expiry
- Test large values (up to MaxValueSize)
- Test concurrent access to same key
- Test that GET on nonexistent key returns proper error

**Steps for eventstore:**
- Test append event, read events by stream
- Test event ordering (monotonic sequence numbers)
- Test subscription delivery
- Test that events are idempotent (duplicate append doesn't duplicate)

**Steps for notifications:**
- Set up fake SMTP server (net/smtpd test helper)
- Test email send with valid template
- Test that invalid template returns error
- Test SMS/Push notification through mock providers

**Done when:** Each of the 3 plugins has at least one behavioral test that exercises actual business logic (not just config parsing).

**Effort:** L (12-20 hours across 3 plugins)

---

### 11. Add AI agent tests

**New file:** `cleat/ai/agent/agent_test.go`

**What:** `cleat/ai/agent/agent.go` has zero tests. This is the AI agent integration for workflow execution.

**Steps:**
- Test agent creation with valid config
- Test agent executes a simple tool and returns result
- Test agent handles tool execution failure gracefully
- Test agent respects max_turns limit
- Test agent passes context through to LLM calls
- Test that agent can be embedded in a workflow via HostCalls

**Done when:** Agent lifecycle (create, execute tool, return result, handle error) is covered.

**Effort:** M (6-10 hours)

---

### 12. Add baseline tests for all untested packages

**New files to create:**
- `cleat/virtualobject_test.go` — test RegisterVirtualObject, GetVirtualObject, duplicate registration panic, empty name panic
- `cleat/version_test.go` — test version comparison, constraint checking
- `internal/plugingen/go_test.go` — test Go code generation from IR (mirrors typescript_test.go)
- `internal/plugingen/python_test.go` — test Python code generation from IR
- `internal/plugingen/rust_test.go` — test Rust code generation from IR
- `internal/plugingen/ir_test.go` — test IR construction from manifest
- `crates/cleat-macro/tests/` — test the `#[cleat_entry]` proc-macro compiles and produces correct WASM exports
- `cmd/cleat-gen-plugin/main_test.go` — test plugin scaffolding code generation

**What:** 13 packages/directories have zero tests. Each needs at minimum a smoke test proving the public API works.

**Steps per package (standard pattern):**
1. Test constructor/registration returns non-nil
2. Test the primary operation (generate, transform, etc.)
3. Test one error path (invalid input, missing dependency, etc.)
4. Test one edge case (empty input, max value, etc.)

**Done when:** `go test ./...` reports `ok` (not `[no test files]`) for every package in the list above.

**Effort:** L (16-24 hours across 8+ packages)

---

### 13. Add Rust proc-macro tests

**New files:** `crates/cleat-macro/tests/basic.rs`, `crates/cleat-macro/tests/compile_fail/`

**What:** The `#[cleat_entry]` proc-macro crate has zero tests. If the macro generates broken code, nothing catches it until runtime.

**Steps:**
- Add integration test that applies `#[cleat_entry]` to a function and verifies it compiles
- Add test that the generated export wrapper has the correct signature
- Add trybuild compile-fail tests for incorrect usage (wrong parameter types, missing HostCalls, etc.)
- Wire into `cargo test`

**Done when:** `cargo test` in `crates/cleat-macro/` runs at least 3 tests (happy path, wrong signature, missing arg).

**Effort:** M (4-8 hours, proc-macro testing has toolchain complexity)

---

### 14. Upgrade Python SDK tests from method-existence to behavioral

**File to modify:** `python-sdk/tests/test_host_calls.py`

**What:** ~40% of the 849-line test file is `assert callable(host.method_name)`. Another large portion uses `mock.patch` to test mock behavior rather than actual integration. The `test_harness.py` exists but isn't exercised by the tests.

**Steps:**
- Delete all `TestHostCallsMethodExistence` tests (the `assert callable(...)` pattern). Replace with a single `test_all_expected_methods_present` that checks the method count and names in one test.
- Delete `TestHostCallsErrorHandling` (the NotImplementedError wrapping tests — these test the test infrastructure)
- Add tests that use `CleatTestEnv` from `test_harness.py` to run actual workflow patterns:
  - DurableCall round-trip (call a stubbed service, get response)
  - DurableSleep advances virtual clock
  - AwaitSignals returns when signal delivered
  - ChildWorkflow spawns and returns result
  - Saga compensates on failure
  - PollCancellation detects cancellation
- Add `conftest.py` with shared `CleatTestEnv` fixture

**Done when:** `test_host_calls.py` contains zero `assert callable(...)` tests and at least 5 behavioral tests using `CleatTestEnv`.

**Effort:** L (12-20 hours, effectively a rewrite of the test file)

---

### 15. Add web dashboard tests

**Files to create:** `web/src/lib/__tests__/`, `web/src/components/__tests__/`, vitest config

**What:** The Svelte 5 dashboard at `web/` has zero tests. No component tests, no API client tests, no E2E tests. `package.json` doesn't have a test script. This is a user-facing application with zero verification.

**Steps:**
- Install vitest, `@testing-library/svelte`, `jsdom`
- Add `"test": "vitest run"` and `"test:watch": "vitest"` scripts to `web/package.json`
- Add `vitest.config.ts`
- Add tests for `web/src/lib/api.ts` (API client — mock fetch, test request building, error handling, response parsing)
- Add tests for `web/src/lib/cost.ts` (cost calculation logic)
- Add tests for 3 key components: WorkflowList, WorkflowDetail, DagVisualization (renders without crashing, loading state, error state, empty state)
- Add test for App.svelte routing (navigates to correct pages)

**Done when:** `npm test` in `web/` exists, runs, passes, and covers the API client + 3 components.

**Effort:** L (16-24 hours, entire test infrastructure needs to be built from scratch)

---

### 16. Add concurrency/race tests for selector and signal delivery

**Files to modify:** `cleat/selector_test.go`, `cleat/cleattest/cleattest_test.go`

**What:** The selector is a polling/coordination construct that could have race conditions. Current tests use pre-determined mock behavior — they can never find a case where timer and signal fire at the same tick. The project has almost no real concurrency tests despite being a workflow engine.

**Steps:**
- Add `TestSelectorConcurrentSignalAndTimer` — fire signal delivery and timer expiry in separate goroutines simultaneously, verify exactly one wins
- Add `TestConcurrentSignalDelivery` — deliver signals from 10 goroutines concurrently, verify all are received exactly once
- Add `TestSelectorSignalDuringReplay` — verify selector replays deterministically
- Run with `-race` (already enabled in Makefile) and fix any races found

**Done when:** Selector has at least 2 concurrent tests that pass under `-race`.

**Effort:** M (4-8 hours, concurrency bugs may be found and need fixing)

---

### 17. Add real fault injection tests

**New file:** `internal/host/fault_injection_test.go`

**What:** After deleting the 3 fake fault test files (item 2), add real fault injection using `internal/host/fault_injector.go` (which already exists and wraps the store with error injection).

**Steps:**
- Test that `FaultInjector` can inject errors at specific call points
- Test that stored procedures return errors when fault injector is configured
- Test that the engine retries on injected transient errors
- Test that the engine eventually succeeds when fault injection is disabled
- Test that a workflow makes progress despite intermittent DB errors (events already persisted survive)
- Test that ClaimWorkflow returns nil (not panics) when DB is unavailable

**Done when:** At least 3 tests verify the engine handles injected faults correctly (retries, doesn't corrupt state, doesn't lose already-persisted events).

**Effort:** M (6-10 hours, need to understand fault_injector.go API)

---

## Summary

| # | Item | Effort | Depends on |
|---|------|--------|------------|
| 1 | Fix AssemblyScript tests | S | — |
| 2 | Delete fake fault tests | S | — |
| 3 | Delete redundant capabilities test | S | — |
| 4 | Fix tautological assertions | S | — |
| 5 | Fix Rust compensation test | S | — |
| 6 | Consolidate test schemas | M | — |
| 7 | Make WASM/DB tests mandatory in CI | M | 6 |
| 8 | Add coverage tracking | M | 7 |
| 9 | Add auth tests | M | — |
| 10 | Add behavioral plugin tests | L | 6 |
| 11 | Add AI agent tests | M | — |
| 12 | Add baseline tests for untested pkgs | L | — |
| 13 | Add Rust proc-macro tests | M | — |
| 14 | Upgrade Python SDK tests | L | — |
| 15 | Add web dashboard tests | L | — |
| 16 | Add concurrency tests | M | — |
| 17 | Add real fault injection tests | M | 2, 6 |

**Total effort:** ~3-4 weeks for one person (5 small + 8 medium + 4 large items)

**Recommended order:** Start with items 1-5 (small, unblock others, fix broken/misleading things immediately). Then items 6-8 (infrastructure, unblock everything else). Then 9, 11, 15 (critical gaps — auth, AI, web). Then 10, 12, 13, 14 (breadth). Then 16, 17 (depth/hardening).
