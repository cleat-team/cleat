# Test Improvement Plan — Implementation Progress

Started: 2026-05-07

## Status Key
- 🔴 Not started
- 🟡 In progress
- 🟢 Done
- ❌ Failed

---

## Batch 1: Small / Independent (5 items, all parallel)

| # | Item | Status | Agent | Result |
|---|------|--------|-------|--------|
| 1 | Fix AssemblyScript tests | 🟢 | a623c394 | Installed @as-pect/cli, created config, fixed WASM start/instantiate. All 16 tests PASS (was broken entirely before). |
| 2 | Delete fake fault tests | 🟢 | acdc87da | Deleted fault_disk_test.go, fault_clock_test.go, TestFaultDBConnectionLoss. Build+test pass. |
| 3 | Delete redundant capabilities test | 🟢 | a96546f | Deleted capabilities_enforcement_test.go. Build+test pass. |
| 4 | Fix tautological assertions | 🟢 | ab9afbfb | Rewrote TestDurableCallWorks (deterministic replay check) and TestWorkflowContextHasHostCalls (exercises DurableLog/Now/WorkflowID). All 11 tests pass. |
| 5 | Fix Rust compensation test | 🟢 | a93da3ff | Changed t.Log to t.Error on line 207. Build passes. |

## Batch 2: Medium / Independent (6 items)

| # | Item | Status | Agent | Result |
|---|------|--------|-------|--------|
| 6 | Consolidate test schemas | 🟢 | abdaad23 | Created testutil/schema.go with 4 helpers. Migrated fault_test.go, concurrency_test.go, integration_test.go. Removed ~200 lines of duplicated DDL. |
| 9 | Add auth tests | 🟢 | a58696f3 | Created 3 files, 38 tests: middleware (21), tenant_store (17), fake driver. Full round-trip coverage. |
| 11 | Add AI agent tests | 🟢 | a84710f6 | Created agent_test.go with 11 tests: happy path, tools, max steps, errors, context, config. All pass. |
| 12 | Add baseline tests for untested pkgs | 🟢 | afc52448 | Created 8 test files: virtualobject, version, plugingen/{go,python,rust,ir}, cleat-gen-plugin, localdev. All pass. |
| 13 | Add Rust proc-macro tests | 🟢 | acbbb116 | Created tests/basic.rs with 8 integration tests: success, error, no-input, custom types, deserialization error, null ptr, inner fn, empty struct. All pass. |
| 16 | Add concurrency tests | 🟢 | a0f435d5 | Added 4 tests: concurrent signal+timer race, multiple signals, signal replay determinism, 10-goroutine delivery. All pass under -race. |

## Batch 3: CI/Coverage (depends on Batch 1 + 2)

| # | Item | Status | Agent | Result |
|---|------|--------|-------|--------|
| 7 | Make WASM/DB tests mandatory in CI | 🟢 | ab861aa1 | Added PostgreSQL service, TinyGo, Rust wasm32 target to CI. Skip-count warnings. No more silent skipping. |
| 8 | Add coverage tracking | 🟢 | a830f40f | Added 5 coverage targets to Makefile, CI coverage job, threshold checks (non-blocking). |

## Batch 4: Large items (4 items)

| # | Item | Status | Agent | Result |
|---|------|--------|-------|--------|
| 10 | Add behavioral plugin tests | 🟢 | a8d7d8e8 | kvstore: 12 new tests. eventstore: 8 new tests. notifications: 11 new tests. Fixed Scan bug in kvstore/routes.go. All 50 pass. |
| 14 | Upgrade Python SDK tests | 🟢 | af26b834 | Rewrote test_host_calls.py: 123 tests (47 new behavioral). Replaced assert-callable with summary checks. Added conftest.py. |
| 15 | Add web dashboard tests | 🟢 | a42f9282 | 79 tests: 43 cost, 26 API, 6 StatusBadge, 4 SummaryCard. Vitest + testing-library. All pass. |
| 17 | Add real fault injection tests | 🟢 | ae4304c2 | Created 5 tests: injected call error, non-retryable classification, event persistence w/ DB error, claim w/ DB error, injector toggle. All pass. |

---

## Activity Log
