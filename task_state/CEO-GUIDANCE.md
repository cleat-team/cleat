# CEO Guidance — cleat 0.5 Trial Release Hardening

**Date:** 2026-06-04
**Budget:** ~$100 (~10 engineer-days)
**Target:** cleat engine is production-polished and ready for a 0.5 trial release. All tests pass, all SDKs work, CI enforces quality gates, documentation is complete and accurate.
**Repo:** `/localssd/rcownie/cleat` (Apache 2.0)

## TL;DR

Cleat has been running in production for clew-service for weeks. The engine is solid but accumulated inconsistencies during rapid development. The 0.5 release is about trust: every test passes on every database, every SDK builds and passes its test suite, every document is correct, and CI prevents regressions. No new features — this lap is hardening only.

## Current State

- v0.1.0 tagged, CHANGELOG.md tracks changes
- Postgres is the primary backend and works correctly
- MySQL and MSSQL backends exist but CI is failing (likely admin schema qualifier mismatches from the data-boundary fix)
- 3 language SDKs: Go (native), Rust (wasmtime), AssemblyScript (WASM)
- CI has multi-db, cross-language, plugin-harness, and ecosystem workflows
- Test coverage target is 75%; latest develop CI shows 2 Go test failures in `internal/closure`
- `ChildWorkflow` vs `ChildWorkflowWithOptions` vs `ChildWorkflowTyped` — three overlapping APIs causing confusion

## This Lap: 6 Items

### 1. ChildWorkflow API Cleanup ($15, ~2 days)

The codebase has three child workflow APIs that overlap confusingly:

- `ChildWorkflow(name, inputJSON)` — bare, no options
- `ChildWorkflowWithOptions(name, inputJSON, opts)` — full options struct
- `ChildWorkflowTyped(name, inputJSON)` — typed variant

**Actions:**
- Audit all callers across workflows, plugins (dag), benchmarks, and tests
- Determine if `ChildWorkflow` should be removed (deprecated in favor of WithOptions) or if the relationship should be clarified in docs
- Update benchmark workflows to use the canonical form
- Update ARCHITECTURE.md and ABI.md to document the correct API and deprecation status
- Ensure all three remain functional (backward compat) but document which is preferred
- Files: `cleat/selector.go`, `wasm/adapter.go`, `wasm/usage.go`, `plugins/dag/dag.go`, `ARCHITECTURE.md`, `ABI.md`

### 2. Multi-DB Test Fixes ($20, ~2 days)

MySQL and MSSQL CI jobs are failing. Likely causes:
- Admin schema qualifier (`admin.tenant_api_keys`) broke MySQL/MSSQL which don't have the `admin` schema
- Migration tests for plugin migrations may have similar schema issues

**Actions:**
- Run the full multi-db test suite locally for MySQL and MSSQL (Docker containers)
- Fix schema references: `admin.` prefix should be conditional on dialect (Postgres only)
- Fix any other dialect-specific SQL issues
- Verify all migrations pass on all three backends
- Target: green multi-db CI
- Files: `auth/tenant_store.go`, `engine/mysql_store.go`, `engine/mssql_store.go`, `engine/db.go`, migrations

### 3. SDK Test Passes ($15, ~2 days)

Language SDKs need to pass their tests:
- **Rust SDK**: wasmtime-based, `wasm/testdata/` — ensure test suite passes
- **AssemblyScript SDK**: WASM component model — ensure cross-language E2E tests pass
- **Python SDK**: PyPI publish workflow exists — ensure tests pass

**Actions:**
- Run each SDK's test suite, triage failures
- Fix any ABI mismatches or host call signature changes
- Update LANGUAGE_SUPPORT.md with current status of each SDK
- Files: `wasm/`, `sdks/`, `LANGUAGE_SUPPORT.md`

### 4. CI Enforcement ($15, ~2 days)

CI must block regressions for the 0.5 release standard.

**Actions:**
- Review `.github/workflows/ci.yml` — ensure it runs on all PRs and covers:
  - Go test (all packages)
  - Go vet
  - govulncheck
  - multi-db tests (Postgres, MySQL, MSSQL)
- Review `.github/workflows/ecosystem-ci.yml` — ensure all SDKs are tested
- Add code coverage check: CI fails if coverage drops below 75%
- Review branch protection rules on `develop` and `main` — ensure required checks match CI
- Fix the `internal/closure` test failures (2 tests: `TestComputeBasicIdentifiesDurableLeaves`, `TestComputeBasicCorrectlyTagsPureFunctions`)
- Files: `.github/workflows/`, `internal/closure/`

### 5. Code Review ($20, ~2 days)

Comprehensive review of the entire codebase for correctness and safety.

**Actions:**
- Review all `engine/` hot paths for race conditions, error handling gaps, and resource leaks
- Review WASM component model implementation for spec compliance
- Verify encryption-at-rest paths (workflow payload encryption)
- Review auth middleware for edge cases (rate limiting, API key rotation, tenant isolation)
- Check all `defer` statements for correct cleanup ordering
- Review SQL queries for injection risks (parameterized queries)
- Any findings get fixed
- Files: `engine/`, `auth/`, `wasm/`, `cmd/cleat-worker/`

### 6. Documentation Audit ($15, ~2 days)

Documents must be accurate for a public release.

**Actions:**
- **ARCHITECTURE.md**: update with current component layout, mention all 3 DB backends, document signal authorization
- **ABI.md**: verify WASM import/export signatures match current code
- **CHANGELOG.md**: prepare 0.5.0 entry summarizing changes since 0.1.0
- **LANGUAGE_SUPPORT.md**: update with current SDK status, accurate line counts, known limitations
- **SECURITY.md**: verify accuracy, add signal auth and encryption-at-rest sections
- **CONTRIBUTING.md**: verify dev setup instructions work from scratch
- **README.md**: verify quickstart instructions, badges, links
- **DX_COMPARISON.md**: update if any competitive facts changed
- Fill any documentation gaps found during code review (item 5)
- Files: `*.md`

## Dependencies

Items 1, 2, 3, 5, and 6 are independent and can start in parallel.
Item 4 (CI enforcement) depends on items 2 and 3 (tests must pass before enforcing).

## Budget

| # | Item | Budget | Days | Priority |
|---|------|--------|------|----------|
| 1 | ChildWorkflow API cleanup | $15 | ~2 | 2 — DX consistency |
| 2 | Multi-DB test fixes | $20 | ~2 | 1 — release blocker |
| 3 | SDK test passes | $15 | ~2 | 1 — release blocker |
| 4 | CI enforcement | $15 | ~2 | 1 — prevents regressions |
| 5 | Code review | $20 | ~2 | 2 — quality baseline |
| 6 | Documentation audit | $15 | ~2 | 2 — public-facing |
| **Total** | | **$100** | **~10 days** | |

## Success Criteria

1. **All multi-db tests pass.** Postgres, MySQL, MSSQL — green CI on every PR.
2. **All language SDKs pass tests.** Go, Rust, AssemblyScript, Python — cross-language E2E green.
3. **CI blocks regressions.** Coverage ≥ 75%, all checks required on develop/main.
4. **ChildWorkflow APIs documented.** No confusion about which to use when.
5. **All docs accurate.** ARCHITECTURE, ABI, SECURITY, CONTRIBUTING, README — verified against current code.
6. **No known issues.** Code review findings resolved, closure tests fixed.

## What NOT to Do This Lap

- **New features.** No new engine capabilities, plugins, or SDKs.
- **Performance optimization.** Unless found during code review.
- **Docker/CI security hardening (gosec, non-root).** Separate concern (apps-226).
- **Sharding, partitioning, HA.** Scale work. Not needed for 0.5.
- **TinyGo deprecation.** Already deprecated; just update docs.
- **New WASM imports.** ABI is frozen for 0.5.
- **clew-service changes.** This lap is cleat engine only.

## Looking Ahead

After 0.5 ships, the next lap should focus on clew-service operational hardening (monitoring, alerting, backup/restore) and the partner demo (running clew on an open-source project to produce a mergeable PR).
