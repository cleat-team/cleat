# cleat-234 Status

**Phase:** explored
**Last updated:** 2026-06-05
**Explored by:** explorer agent
**Dispatched by:** cto-lap-032

## Exploration Summary

Full CI audit complete. Five categories: CI workflow coverage, ecosystem CI, `continue-on-error` audit, branch protection, and closure test failures. Root causes identified for all issues.

---

## 1. CI Workflow (`ci.yml`) — Coverage Gaps

**Current state:** Runs on all PRs to main/develop. Covers: go vet, govulncheck, Go tests (race detector, 4 package groups), Python/Java/AS/Rust tests, fuzz, cluster-tests, build.

**Gaps identified:**

| Issue | Detail |
|-------|--------|
| **Coverage not run on PRs** | Coverage job (line 554) has `if: github.ref == 'refs/heads/main' && github.event_name == 'push'` — only fires on push to main. PRs never check coverage. |
| **test-tinygo disabled** | Job `test-tinygo` (line 211) has `if: false` with comment "Temporarily skipped; Go version issues being explored on another branch". When enabled, it tests `internal/closure/` which would catch the test failures in section 5. |
| **lint-go commented out** | `lint-go` job (lines 99-112) entirely commented out — golangci-lint action doesn't support Go 1.26 yet. This means no errcheck/staticcheck/gosec on PRs. |
| **Go version discrepancy** | `go.mod` declares `go 1.25.7`, but CI uses `GO_VERSION_STABLE: "1.26"`. The go.mod should be bumped to match, or CI should target 1.25. |
| **Multi-DB tests in separate workflow** | Postgres/MySQL/MSSQL tests are in `multi-db-ci.yml`, not `ci.yml`. This is architecturally fine but means a PR doesn't get a single unified status. The `multi-db-ci.yml` workflow runs correctly and covers all three databases. |
| **Scattered TinyGo installation** | TinyGo is installed in test-go (for internal package), plugin-harness-ci, and the disabled test-tinygo job — three different places with subtly different installation logic. Should be consolidated into a composite action or Make target. |

## 2. Ecosystem CI (`ecosystem-ci.yml`)

**Current state:** Runs on PRs that touch `cleat/`, `internal/`, `cmd/`, `go.mod`, `go.sum`. Tests Python SDK (pytest), Rust SDK (cargo test both crates), Java SDK (gradle test), AssemblyScript SDK (npm test).

**Gaps:**

| Issue | Detail |
|-------|--------|
| **AS SDK best-effort** | `assemblyscript-sdk` job (line 63) has `continue-on-error: true` — AS test failures don't block. Acceptable given AS ecosystem instability. |
| **No Go compilation step** | Ecosystem CI doesn't verify the Go SDK compiles cleanly. The `cleat` package is the public API that SDKs consume — it should at least build successfully. |
| **Path filter too narrow** | Only triggers on `cleat/**`, `internal/**`, `cmd/**`, `go.mod`, `go.sum`. Changes to `plugins/` or `crates/cleat-sdk/` that could break cross-SDK compatibility won't trigger ecosystem CI. |

## 3. `continue-on-error: true` Audit

12 occurrences across 4 workflow files:

| # | File | Line | Job/Step | Verdict | Rationale |
|---|------|------|----------|---------|-----------|
| 1 | ci.yml | 52 | Lint job | **REMOVE** | go vet is a core correctness check; failures should block |
| 2 | ci.yml | 81 | Ruff check step | Keep | Informational Python linter |
| 3 | ci.yml | 85 | ShellCheck step | Keep | Informational shell linter |
| 4 | ci.yml | 267 | test-python job | **REMOVE** | Python SDK test failures are real regressions |
| 5 | ci.yml | 297 | test-java job | **REMOVE** | Java SDK test failures are real regressions |
| 6 | ci.yml | 319 | test-assemblyscript job | Keep | AS test infrastructure is less stable |
| 7 | ci.yml | 369 | test-assemblyscript-wasm job | Keep | AS WASM builds are fragile across Node versions |
| 8 | ci.yml | 490 | build job | **REMOVE** | Build failures on main are deploy blockers |
| 9 | ci.yml | 558 | coverage job | Keep | Only runs on main pushes, informational |
| 10 | release-notes-check.yml | 20 | check-release-notes job | Keep | Non-critical documentation check |
| 11 | ai-pr-review.yml | 33 | AI review job | Keep | Advisory only |
| 12 | ecosystem-ci.yml | 63 | assemblyscript-sdk job | Keep | AS ecosystem instability |

**Safe to remove (5):** Lint job, test-python, test-java, build, and (once AS matures) test-assemblyscript.

## 4. Branch Protection

### main branch
- **Required status checks:** `Build`, `Lint`, `Developer Certificate of Origin` (only 3)
- **Missing from required checks:** `Test Go (core)`, `Test Go (internal)`, `Test Go (plugins)`, `Test Go (commands)`, `Vulnerability Check`, `Multi-DB CI / Test MySQL`, `Multi-DB CI / Test SQL Server`, `Plugin Migrations`, `Cross-Language E2E`, `Plugin Harness Tests`, `Test Rust`, `Fuzz Tests`, `Cluster Integration Tests`
- Admin enforcement: enabled
- Required reviews: 0 (none required!)
- DCO check via app ID 15368
- Conversation resolution: required

### develop branch
- **Required status checks:** NONE
- Admin enforcement: disabled
- Required reviews: none
- Conversation resolution: required
- This is the most dangerous gap — any code can be pushed to develop without passing any CI.

### Recommendations
1. Add at minimum `Test Go (core)`, `Test Go (internal)`, `Vulnerability Check`, `Cross-Language E2E` to required checks on main
2. Mirror main's required checks to develop (or at minimum add `Test Go`, `Vulnerability Check`, `Lint`)
3. Enable required reviews (at least 1) on both branches
4. Match required checks to actual CI job names — current names "Build" and "Lint" must exactly match what CI outputs

## 5. Closure Test Failures

**Both failures share the same root cause:** `LongRunning()` was added to `testdata/basic/order.go` but the test expectations weren't updated.

### TestComputeBasicIdentifiesDurableLeaves
- **Expected:** 8 durable leaves
- **Got:** 9 (extra leaf: `LongRunning`)
- **Why:** `LongRunning` calls `h.DurableCall("noop", "", "")` directly, making it a durable leaf. The test's `expectedLeaves` map doesn't include it.
- **Fix:** Add `basicFQ("LongRunning"): true` to `expectedLeaves` at `closure_test.go:40`

### TestComputeBasicCorrectlyTagsPureFunctions
- **Expected:** 12 functions total, durable+pure==12
- **Got:** 13 functions total
- **Why:** Same root cause — `LongRunning` adds a 13th function.
- **Fix:** Change `12` to `13` at `closure_test.go:120`

### Required changes in `internal/closure/closure_test.go`:

**Line 40** — Add `LongRunning` to `expectedLeaves`:
```go
basicFQ("LongRunning"):            true,  // add after line 39
```

**Line 120** — Bump function count:
```go
if totalFuncs != 13 {  // was 12
```

## 6. Coverage Enforcement Architecture

**Current mechanism** (`Makefile:137-195`): Per-package coverage thresholds enforced via `make coverage-check`. Package thresholds: `internal/` 50%, `internal/plugin/` 50%, `cleat/` 15%, `plugins/` 20%, `cmd/` 0%.

**Bugs and gaps:**

| Issue | Detail |
|-------|--------|
| **Wrong module prefix** | Makefile line 158 hardcodes `github.com/rcownie/cleat/` but go.mod has `github.com/cleat-team/cleat`. Coverage check likely fails to match packages correctly. |
| **No PR enforcement** | Coverage job only runs on push to main. PRs never see coverage results. |
| **No global threshold** | Only per-package thresholds exist. The task asks for a global 50%→75% ratchet. |
| **No ratchet mechanism** | No historical coverage tracking — the 50%→75% ratchet would need a stored baseline or a mechanism to prevent coverage drops. |
| **Doesn't fail on PRs** | Even if `coverage-check` ran on PRs, the `continue-on-error: true` on the coverage job would prevent it from blocking. |

## 7. test-tinygo Re-enable Assessment

**TinyGo compatibility:** TinyGo v0.41.0 (April 2026) added Go 1.26 support. v0.41.1 (April 22, 2026) added Go 1.25 backward compatibility. The CI's `test-tinygo` job downloads the latest TinyGo release, which would be v0.41.1.

**Verdict:** The "Go version issues" blocker appears resolved. The job can likely be re-enabled by removing `if: false` (ci.yml:211).

**Risk:** The job installs TinyGo and runs four package test suites (internal/wasm, internal/host, internal/closure, internal/analyzer) that are currently only run without TinyGo in the main test-go job. Enabling would add ~5 minutes to CI and would catch closure test regressions.

**Recommendation:** Enable with `continue-on-error: true` initially to observe stability, then remove the soft-fail once proven stable over a week of PRs.

## 8. Files That Need Changes (Priority Order)

| Priority | File | Lines | Change |
|----------|------|-------|--------|
| **P0** | `internal/closure/closure_test.go` | 40, 120 | Fix 2 test expectations for LongRunning |
| **P0** | `.github/workflows/ci.yml` | 52 | Remove `continue-on-error` from lint job |
| **P0** | `.github/workflows/ci.yml` | 490 | Remove `continue-on-error` from build job |
| **P1** | `.github/workflows/ci.yml` | 554-556 | Extend coverage job to run on PRs (not just main pushes) |
| **P1** | `.github/workflows/ci.yml` | 558 | Remove `continue-on-error` from coverage job (once on PRs) |
| **P1** | `Makefile` | 158 | Fix module prefix: `github.com/rcownie/cleat` → `github.com/cleat-team/cleat` |
| **P1** | `Makefile` | 137-195 | Add global aggregate threshold (50% start, ratchet to 75%) |
| **P2** | `.github/workflows/ci.yml` | 211 | Re-enable test-tinygo (remove `if: false`) |
| **P2** | `.github/workflows/ci.yml` | 267 | Remove `continue-on-error` from test-python |
| **P2** | `.github/workflows/ci.yml` | 297 | Remove `continue-on-error` from test-java |
| **P2** | `.github/workflows/ci.yml` | 99-112 | Uncomment lint-go if golangci-lint now supports Go 1.26 |
| **P3** | Branch protection (main/develop) | — | Add required status checks: Test Go, Vulncheck, Multi-DB, E2E (requires GitHub admin) |
| **P3** | `go.mod` | 3 | Resolve Go version discrepancy (1.25.7 vs CI's 1.26) |
| **P3** | `.github/workflows/ecosystem-ci.yml` | 6-11 | Broaden path filter to include `plugins/**`, `crates/cleat-sdk/**` |
| **P3** | `.github/workflows/ci.yml` | — | Consolidate TinyGo installation into a composite action |

## 9. Dependency Check

This task depends on cleat-232 (multi-DB dialect fixes) and cleat-233 (still pending). However, the closure test fixes and CI configuration changes in this task are independent of those dependencies — they can proceed regardless.
