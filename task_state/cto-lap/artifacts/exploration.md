# Exploration: cleat 0.5 Trial Release Hardening

**Explorer:** CTO Lap 90 survey
**Date:** 2026-06-04
**CEO Guidance:** `task_state/CEO-GUIDANCE.md` (2026-06-04, $100 budget, 6 items)
**Prior lap state:** Lap 89 dispatched cleat-228b ($20) and cleat-230 ($20) from old guidance.
Both are from the now-superseded CEO guidance; new guidance is a full reset.

---

## 1. What's Here Now

### Codebase at a glance

- **Module:** `github.com/cleat-team/cleat`, Go 1.25.7
- **~109K lines of Go** across 206 files
- **3 DB backends:** PostgreSQL (primary), MySQL, MSSQL
- **3 language SDKs:** Go (native), Rust (wasmtime), AssemblyScript (WASM)
- **Python SDK** exists but WASM FFI never validated end-to-end
- **14 CI jobs** in ci.yml, 16 total GitHub Actions workflows
- **50 host function imports** (ABI v5)
- **v0.1.0** tagged, CHANGELOG.md has no 0.5.0 entry

### Recent activity (since 2026-05-25)

5 commits on develop, all technical fixes:
- `a019068` — qualify admin schema tables, add wasmtime stub, add generation column
- `4734c7d` — debug logging to child workflow dispatch path
- `29dcbf9` — generate poll-only main stub in cleat build template
- `5c74011` — allow empty parentClosePolicy in ChildWorkflowWithOptions
- `09f8209` — bypass cleat_poll_work pointer-passing bug with fixed-memory protocol

No work has started on any of the 6 new CEO guidance items.

---

## 2. Per-Item Assessment

### Item 1: ChildWorkflow API Cleanup ($15)

**Files:** `cleat/selector.go`, `wasm/adapter.go`, `wasm/usage.go`, `plugins/dag/dag.go`, `ARCHITECTURE.md`, `ABI.md`

**Current state:**
- Three public APIs: `ChildWorkflow` (plain), `ChildWorkflowWithOptions`, `ChildWorkflowTyped`
- `ChildWorkflow` is the base primitive — most call sites (17+ tests, 2 production examples)
- `ChildWorkflowWithOptions` is the canonical/higher-fidelity API — used in only 1 production location (`plugins/dag/dag.go:254`)
- `ChildWorkflowTyped` is a convenience wrapper marshaling typed inputs to JSON — 1 production use (`examples/datapipeline/pipeline.go:56`)
- `ChildWorkflowWithOptions` not wired in localdev runner — options silently dropped
- `ChildWorkflowInSchema` is a 4th variant (separate WASM import, not part of public HostCalls) — not in scope
- Neither ARCHITECTURE.md nor ABI.md documents any of these APIs

**What needs to change:**
1. Audit all callers — already done (see survey above)
2. Decision: `ChildWorkflow` should NOT be removed (it's the fallback path for all three, and the base WASM import). Recommend: document `ChildWorkflowWithOptions` as the preferred API, `ChildWorkflow` as the minimal form, `ChildWorkflowTyped` as a convenience wrapper
3. Update benchmark `fanout.go` from `ChildWorkflow` to `ChildWorkflowWithOptions` for canonical usage
4. Wire `ChildWorkflowWithOptions` into localdev runner (`localdev.go:196-200`)
5. Document deprecation status in ARCHITECTURE.md and ABI.md

**Risks:** Low. The APIs share the same WASM import underneath. Changing call sites is mechanical. Wiring localdev is a ~5-line addition.

**Complexity:** Leaf-ready. ~2 days, $15 budget is appropriate. 4-5 files, ~50 lines of changes + doc updates.

---

### Item 2: Multi-DB Test Fixes ($20)

**Files:** `auth/tenant_store.go`, `engine/mysql_store.go`, `engine/mssql_store.go`, `engine/db.go`, migrations

**Current state — ROOT CAUSE identified:**

`auth/tenant_store.go` has 3 hardcoded PostgreSQL-only queries:
- Line 29: `INSERT INTO admin.tenants ... RETURNING tenant_id` — `admin.` schema doesn't exist in MySQL, `RETURNING` is PG-only, `$N` placeholders are PG-only
- Line 39: `INSERT INTO admin.tenant_api_keys ...` — same issues
- Line 47: `UPDATE admin.tenant_api_keys ...` — same issues

Additional dialect-unaware code:
- `plugin/migration.go:244`: `INSERT INTO admin.plugin_tables ... ON CONFLICT DO NOTHING` — known undocumented gap (comment lines 37-38)
- `plugin/tenant_db.go:57`: `SELECT ... FROM admin.tenant_roles ... $1` — no dialect support

Migration gaps:
- MySQL missing `008_rls_fail_closed.sql` and `009_generation.sql`
- MSSQL 008 is a no-op placeholder
- Migration count differs: Postgres (9), MySQL (7), MSSQL (8)

**CI:** MySQL and MSSQL tests are gated behind env vars (`CLEAT_TEST_MYSQL`, `CLEAT_TEST_MSSQL`) that are NOT set in CI. Tests are silently skipped.

**The good news:** `engine/db.go` PostgresStore dialected correctly handles `admin.` (only applies to Postgres). MySQLStore and MSSQLStore resolve tenant without schema qualifier. The store implementations are correct — it's the auth/ and plugin/ packages that lack dialect awareness.

**What needs to change:**
1. Make `auth/tenant_store.go` dialect-aware: accept a `Dialect` parameter, use appropriate placeholders (`$N` vs `?` vs `@pN`), conditional `RETURNING` vs `LAST_INSERT_ID()` vs `OUTPUT`
2. Fix `plugin/migration.go:244` — make `RegisterPluginTables` dialect-aware (already has a documented TODO)
3. Fix `plugin/tenant_db.go:57` — dialect-aware placeholder and schema
4. Add missing MySQL migrations (008, 009)
5. Add MySQL/MSSQL service containers to CI and set `CLEAT_TEST_MYSQL`/`CLEAT_TEST_MSSQL` env vars

**Risks:** Medium. The auth tests use a fake driver that matches SQL by substring — they'll need real-database tests or at minimum updated fake patterns for each dialect.

**Complexity:** Leaf-ready but non-trivial. ~2 days, $20 budget is tight if CI setup is included. 3-4 files in Go, 2-3 migration files, CI YAML changes.

---

### Item 3: SDK Test Passes ($15)

**Current state:**

**Rust SDK** — best shape:
- `crates/cleat-sdk/src/test.rs`: 25 inline tests with MockHostCalls
- `crates/cleat-macro/tests/basic.rs`: 7 integration tests for `#[cleat_entry]`
- `crates/cleat-macro/tests/compile_fail.rs`: 5 trybuild UI tests
- CI runs `cargo test` for both crates (blocking)
- **ABI issue:** `child_workflow_with_options` and `child_workflow_in_schema` have an undocumented `priority: i64` parameter not in ABI.md
- **ABI issue:** `cleat_poll_child` and `cleat_await_any_child` exist in Rust SDK but missing from ABI.md (section numbering jumps from 2.43 to 2.46)

**AssemblyScript SDK** — has issues:
- `packages/cleat-as/`: smoke tests + JSON parsing tests + saga tests
- CI runs but is `continue-on-error: true` (non-blocking)
- **Critical:** Only `cleat_json_parse` and `cleat_json_stringify` have JS stubs in the as-pect test harness. All other `@external("env", ...)` imports lack stubs — tests that instantiate HostCalls would fail at runtime
- LANGUAGE_SUPPORT.md mentions 11 known issues from fork/port
- AS 0.27.32 resolver assertion issue noted

**Python SDK** — never validated E2E:
- 9 test files, ~100+ tests in CleatTestHarness (pure Python, no WASM)
- `TestPythonWasmEndToEnd` in `engine/python_wasm_e2e_test.go` exists but is always skipped (requires componentize-py + wasm-tools)
- `TestPythonWasmAbiBoundary` expects 38 host names but Go registers 42 — gap of 5 missing imports
- The `e2e-cross-language.yml` CI references `./internal/host/...` but tests are in `engine/` and `tests/cross-language/`
- CI runs pytest (blocking on ecosystem-ci, continue-on-error on main CI)
- Host call imports from `wit_world.imports.*` but never tested against real cleat worker

**What needs to change:**
1. Rust: Fix ABI.md or Rust signatures for `child_workflow_with_options` priority param, add `cleat_poll_child`/`cleat_await_any_child` to ABI.md
2. AssemblyScript: Add JS stubs for all host imports in as-pect.config.mjs, fix resolver assertion, address 11 known issues
3. Python: Wire WASM E2E test in CI (fix path in e2e-cross-language.yml), validate componentize-py bindings against real worker
4. Update LANGUAGE_SUPPORT.md with accurate status

**Risks:** Medium-High. Python WASM E2E has NEVER been validated — unknown unknowns. AS stub gap means tests may fail in unexpected ways.

**Complexity:** Could be leaf but risks decomposition. AssemblyScript (~$5-7), Rust (~$3-5), Python (~$5-8). $15 total budget is tight if Python has hidden issues. Recommend: single leaf task with explicit timebox — if Python WASM hits insoluble issues, escalate.

---

### Item 4: CI Enforcement ($15)

**Files:** `.github/workflows/`, `internal/closure/`

**Current state:**

14 CI jobs in ci.yml. Key gaps:
- **Coverage:** Runs only on push-to-main, non-blocking (`continue-on-error: true`), thresholds max 50% (CEO wants 75%)
- **Coverage bug:** Makefile AWK script strips `github.com/rcownie/cleat/` but go.mod uses `github.com/cleat-team/cleat`
- **test-tinygo:** Disabled (`if: false`) — all Go WASM cross-language tests not running
- **lint-go:** Disabled (golangci-lint doesn't support Go 1.26 yet)
- **2 failing closure tests:** `TestComputeBasicIdentifiesDurableLeaves` and `TestComputeBasicCorrectlyTagsPureFunctions` — testdata added `LongRunning` function but test expectations weren't updated

**What needs to change:**
1. Fix closure tests: add `LongRunning` to expected leaves map, update count 12→13 (~5 lines in `internal/closure/closure_test.go`)
2. Move coverage to PRs, make blocking, raise thresholds toward 75%
3. Fix module path in Makefile coverage-check AWK script
4. Re-enable test-tinygo (or document permanent deprecation)
5. Add code coverage check: CI fails if coverage drops below threshold
6. Review branch protection rules on develop/main

**Risks:** Low. Closure tests are a mechanical fix. Coverage threshold enforcement may surface test gaps.

**Complexity:** Leaf-ready. ~2 days, $15 budget. Closure test fix is ~5 lines. Coverage config changes are YAML + Makefile. Branch protection rules require GitHub repo admin access.

---

### Item 5: Code Review ($20)

**Files:** `engine/`, `auth/`, `wasm/`, `cmd/cleat-worker/`

**Current state from high-level survey:**
- engine/ is 48K lines across 80 files; engine.go alone is 4,941 lines
- 670 defer statements across engine/ — patterns look correct on spot-check
- SQL: generally good. Parameterized queries in all store implementations. 2 medium-risk spots:
  - `engine/db.go:2450` — `fmt.Sprintf` for schema name in table reference
  - `engine/mysql_ops.go:972` — string concatenation for IN clause
- Encryption: AES-256-GCM in `engine/encryption.go`. **Documented gap:** AAD is nil — tenant ID not cryptographically bound to ciphertexts
- Auth: Bearer token + API key, SHA-256 hashing, two-layer rate limiting (per-IP + per-tenant)
- 12 mutexes in engine/ — no `-race` testing found in CI
- WASM component model: well-structured across 18 files + substantial CGo support

**What needs to change:**
1. Full review of engine hot paths (engine.go, db.go)
2. Audit AAD gap in encryption.go — add tenant binding or document why deferred
3. Verify schema name in db.go:2450 is never user-controlled
4. Run `go test -race` across engine/ and investigate any findings
5. Review auth middleware edge cases (rate limiting, API key rotation, tenant isolation)
6. Audit WASM component model for spec compliance
7. Review defer cleanup ordering in hot paths
8. Fix any findings

**Risks:** Low as exploration, high-value as remediation. The AAD gap and schema-name substitution are the most concrete concerns found.

**Complexity:** Leaf-ready as a review task. ~2 days, $20 budget. Pair with a senior developer agent. Key: findings → fixes, not just report.

---

### Item 6: Documentation Audit ($15)

**Files:** `*.md`, `docs/`

**Current state — 47 markdown files in docs/, 14 in root:**

| Priority | Doc | Issue |
|----------|-----|-------|
| **HIGH** | CHANGELOG.md | No v0.5.0 entry; nearly empty (156 words of boilerplate) |
| **HIGH** | SECURITY.md | Missing signal auth section; missing encryption-at-rest section; "15 host imports" → should be 50 |
| **HIGH** | LANGUAGE_SUPPORT.md | Contradicts DX_COMPARISON.md on Python WASM status; "15 imports" stale |
| **MEDIUM** | CONTRIBUTING.md | Says TinyGo is "default and only Go WASM target" (deprecated); uses old DURABLE_TEST_DB env var name |
| **MEDIUM** | ARCHITECTURE.md | Missing signal authorization section; needs ChildWorkflow API deprecation note |
| **MEDIUM** | DX_COMPARISON.md | Internal contradiction on Python WASM (line 23 vs 364); typo "end-to-end end-to-end" |
| **MEDIUM** | ABI.md | Missing `cleat_poll_child`, `cleat_await_any_child`; priority param mismatch with Rust SDK |
| **LOW** | README.md | 518 words, generally OK; verify quickstart testdata path and Discord badge |
| **LOW** | docs/explanation/architecture.md | "15 host functions" stale |

**What needs to change:**
1. Write CHANGELOG.md 0.5.0 entry summarizing all changes since 0.1.0
2. Add signal auth and encryption-at-rest sections to SECURITY.md
3. Fix stale "15 imports" → "50" in SECURITY.md, LANGUAGE_SUPPORT.md, docs/explanation/architecture.md
4. Update CONTRIBUTING.md: standard Go as default, fix DURABLE_TEST_DB → CLEAT_TEST_DB
5. Document ChildWorkflow API in ARCHITECTURE.md and ABI.md
6. Reconcile DX_COMPARISON.md Python WASM status; fix typo
7. Add missing ABI imports to ABI.md
8. Verify README.md quickstart works from scratch

**Risks:** Low. All mechanical doc updates. The only risk is ABI.md signature verification — must match actual code.

**Complexity:** Leaf-ready. ~2 days, $15 budget. High volume but low difficulty. Can parallelize: one agent on CHANGELOG+SECURITY, another on LANGUAGE_SUPPORT+ABI, another on CONTRIBUTING+README.

---

## 3. Cross-Cutting Issues

### Dependency Graph (per CEO guidance)

```
Items 1, 2, 3, 5, 6 → independent, can start in parallel
Item 4 (CI enforcement) → depends on 2 and 3 (tests must pass before enforcing)
```

### Prior task cleanup

- `cleat-228b` ($20, WASM debugger CLI, "implementing"): From old guidance. 10 days stale. Test infra fixes + docs remain but scope is superseded by new guidance items 3+6. Recommend: close as done (production code merged). Doc work absorbed into item 6.
- `cleat-230` ($20, Engine Reliability Polish): From old guidance. Created but never started. Scope partially overlaps with item 5 (code review). Recommend: close. Race audit absorbed into item 5.
- `cleat-228a`: Already closed (lap 89).

### cleat-internal tasks.json

Empty (`"tasks": {}`). Needs entries for the 6 new tasks created from this lap's CEO guidance.

---

## 4. Recommended Task Decomposition

All 6 items are independent enough for leaf tasks. No DAG-based decomposition needed. Each item maps directly to one task:

| Task ID | Subject | Budget | Depends On |
|---------|---------|--------|------------|
| cleat-231 | ChildWorkflow API cleanup + docs | $15 | — |
| cleat-232 | Multi-DB test fixes (MySQL, MSSQL) | $20 | — |
| cleat-233 | SDK test passes (Rust, AS, Python) | $15 | — |
| cleat-234 | CI enforcement + closure test fix | $15 | cleat-232, cleat-233 |
| cleat-235 | Code review (engine, auth, wasm) | $20 | — |
| cleat-236 | Documentation audit + updates | $15 | — |
| **Total** | | **$100** | |

Items 231, 232, 233, 235, 236 can dispatch immediately in parallel.
Item 234 (CI enforcement) should wait until 232 and 233 pass tests.

---

## 5. What NOT to Do (per CEO guidance)

- No new features, plugins, or SDKs
- No performance optimization (unless found during code review)
- No Docker/CI security hardening (separate concern, apps-226)
- No sharding, partitioning, HA
- No TinyGo deprecation work (already deprecated, just update docs)
- No new WASM imports (ABI frozen for 0.5)
- No clew-service changes (this lap is cleat engine only)

---

## 6. Unresolved Questions

1. **Python WASM E2E viability** — never validated. If componentize-py bindings have fundamental issues, the $15 SDK budget won't cover fixing them. Recommend: timebox Python E2E to 4 hours; escalate if unresolved.

2. **Branch protection rules** — require GitHub admin access to verify. Can't assess from code.

3. **Coverage 75% target** — current Makefile thresholds max at 50%. Raising to 75% as a blocking CI check may break the build. Recommend: set 75% as goal, start enforcement at 50%, ratchet up.

4. **test-tinygo re-enable** — was disabled for Go version issues. If Go 1.26 resolved them, re-enable. If not, document permanent deprecation in lieu of standard Go WASM.
