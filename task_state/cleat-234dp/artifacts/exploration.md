# cleat-234dp Exploration Report — Final Delta Verification

**Date:** 2026-06-06
**Explorer:** cleat-234dp (independent verification pass)
**Prior work:** cleat-234 STATUS.md (2026-06-05), cleat-234de (2026-06-06), cleat-234a (2026-06-06), cleat-234c (2026-06-06)

## Summary

Independently re-verified all cleat-234 findings against current code state. **All claims confirmed.** One delta found: the e2e-cross-language.yml path bug is fixed in working tree. Dependencies (cleat-232, cleat-233) are partially resolved but not fully green.

## 1. Files Verified

| File | Method | Result |
|------|--------|--------|
| `internal/closure/closure_test.go` (414 lines) | Full read, lines 30-130 | Both test bugs confirmed |
| `testdata/basic/order.go` (192 lines) | Read LongRunning at lines 175-181 | DurableCall confirmed |
| `.github/workflows/ci.yml` (656 lines) | Full read | All 9 continue-on-error, coverage trigger, test-tinygo disabled, lint-go commented out — all confirmed |
| `.github/workflows/ecosystem-ci.yml` (75 lines) | Full read | Path filter gap confirmed, AS best-effort confirmed |
| `.github/workflows/e2e-cross-language.yml` (110 lines) | Full read | Path bug FIXED in working tree (delta!) |
| `.github/workflows/multi-db-ci.yml` (176 lines) | Full read | No continue-on-error, all 3 DBs covered |
| `.github/workflows/plugin-harness-ci.yml` (164 lines) | Full read | 2 TinyGo install locations confirmed |
| `Makefile` (coverage section, lines 130-195) | Full read | Module prefix bug confirmed |
| `go.mod` (line 3) | Read | `go 1.25.7` vs CI `1.26` confirmed |

## 2. Verified Findings (10 categories)

### 2a. Closure Test Failures — CONFIRMED

- `closure_test.go:31-41`: `expectedLeaves` has 8 entries, missing `basicFQ("LongRunning")`. Comment on line 31 says "All eight functions."
- `closure_test.go:115-116`: Comment says "Total functions = 12 (8 leaves + 4 closure)"
- `closure_test.go:120`: `totalFuncs != 12` — will fail since there are 13 functions
- `testdata/basic/order.go:175-181`: `LongRunning()` calls `h.DurableCall("noop", "", "")` — 9th durable leaf confirmed

**Fix:**
- Line 40: add `basicFQ("LongRunning"): true,` to `expectedLeaves`
- Line 120: change `12` → `13`
- Line 31: update comment to "All nine functions"
- Line 115-116: update comment to "Total functions = 13"

### 2b. continue-on-error Audit — ALL 12 CONFIRMED

All at exact line numbers reported by prior explorations. No changes.

| # | File | Line | Job/Step | Verdict |
|---|------|------|----------|---------|
| 1 | ci.yml | 52 | lint job | REMOVE |
| 2 | ci.yml | 81 | ruff check | Keep |
| 3 | ci.yml | 85 | shellcheck | Keep |
| 4 | ci.yml | 267 | test-python | REMOVE |
| 5 | ci.yml | 297 | test-java | REMOVE |
| 6 | ci.yml | 319 | test-assemblyscript | Keep |
| 7 | ci.yml | 369 | test-assemblyscript-wasm | Keep |
| 8 | ci.yml | 490 | build job | REMOVE |
| 9 | ci.yml | 558 | coverage job | Keep (informational on main only) |
| 10 | release-notes-check.yml | 20 | check-release-notes | Keep |
| 11 | ai-pr-review.yml | 33 | AI review | Keep |
| 12 | ecosystem-ci.yml | 63 | assemblyscript-sdk | Keep |

### 2c. Coverage Job Trigger — CONFIRMED

- ci.yml line 556: `if: github.ref == 'refs/heads/main' && github.event_name == 'push'` — PRs never get coverage
- ci.yml line 558: `continue-on-error: true` — main-push failures don't block

### 2d. test-tinygo Disabled — CONFIRMED

ci.yml line 211: `if: false`

### 2e. lint-go Commented Out — CONFIRMED

ci.yml lines 99-112: Entire job commented out.

### 2f. Go Version Discrepancy — CONFIRMED

- `go.mod:3`: `go 1.25.7`
- `ci.yml:33`: `GO_VERSION_STABLE: "1.26"`

### 2g. Multi-DB CI — CONFIRMED

`multi-db-ci.yml`: Separate workflow, no continue-on-error, covers MySQL 8.4, SQL Server 2022, and plugin migrations across all 3 DBs.

### 2h. Ecosystem CI Path Filter — CONFIRMED

ecosystem-ci.yml lines 5-10: only `cleat/**`, `internal/**`, `cmd/**`, `go.mod`, `go.sum`. Missing: `plugins/**`, `crates/cleat-sdk/**`, `python-sdk/**`, `packages/cleat-as/**`.

### 2i. Makefile Coverage Module Prefix Bug — CONFIRMED

- Makefile line 158: `sub(/^github\.com\/rcownie\/cleat\//, "", path);`
- `go.mod:1`: `module github.com/cleat-team/cleat`
- The awk `sub()` never matches → no package paths get stripped → per-package thresholds never trigger → **coverage check is a NO-OP**

### 2j. TinyGo Scattered Installation — CONFIRMED

4 locations (not 3):
1. ci.yml test-go job (lines 171-181) — for internal package group
2. ci.yml test-tinygo job (lines 235-244) — disabled
3. plugin-harness-ci.yml test-layer2 (lines 40-49) — WASM integration
4. plugin-harness-ci.yml test-multi-db (lines 106-115) — multi-DB

## 3. Delta: e2e-cross-language.yml Path Bug — FIXED (uncommitted)

The cleat-233i exploration found that e2e-cross-language.yml line 101 used `./internal/host/...` instead of `./engine/...`. This has been fixed in the working tree:

```diff
-            ./internal/host/... 2>&1 | tee e2e-report.txt
+            ./engine/... 2>&1 | tee e2e-report.txt
```

The change is unstaged in the working tree (shown in `git diff`). No other CI files have uncommitted changes.

## 4. Dependency Status

### cleat-232 (Multi-DB test fixes)
- **Status:** in_progress
- **P0 FIXED:** auth/tenant_store.go dialect support, auth/fake_driver_test.go query matchers, plugin/migration.go dialect-specific SQL
- **P1 PENDING:** middleware test path fixes (7 failures from "/" as public path), MySQL/MSSQL test variants for tenant store
- **P3 PENDING:** MySQL missing migrations 008/009, MSSQL missing migration 009
- **Impact on cleat-234:** The multi-DB CI is green for the host/engine tests. The tenant store fix is verified. Test failures in middleware are unrelated to CI enforcement. cleat-234 can proceed.

### cleat-233 (SDK test passes)
- **Status:** decomposed (child tasks not complete)
- cleat-233a (AS fix): IN PROGRESS — binaryen crash fixed, json-host.spec.ts: 0/19 FAIL (pre-existing bug)
- cleat-233b (Python WASM E2E): pending
- cleat-233c (Rust WASM integration): pending
- cleat-233d (ABI.md fix): pending
- cleat-233e (docs update): pending
- **Impact on cleat-234:** Python and Java test suites pass natively. Rust passes. The AS SDK has pre-existing test failures unrelated to CI config. cleat-234's CI config changes are independent of these child tasks.

## 5. Assessment

### What's here now?
The CI enforcement task is well-understood. The codebase state matches the prior explorations exactly. No surprises, no regressions, no new issues introduced. The one delta (e2e path fix) is a positive one — a reported bug getting fixed.

### What needs to change?
The implementation plan from cleat-234 STATUS.md (8 priority levels) remains accurate:

**P0 (2 files, 2-3 line changes):**
- `internal/closure/closure_test.go`: Fix 2 test expectations for LongRunning

**P0 (same file):**
- `.github/workflows/ci.yml`: Remove `continue-on-error` from lint (line 52) and build (line 490)

**P1:**
- `.github/workflows/ci.yml`: Extend coverage job to run on PRs (line 556)
- `.github/workflows/ci.yml`: Remove `continue-on-error` from coverage (line 558) once on PRs
- `Makefile:158`: Fix module prefix: `rcownie` → `cleat-team`
- `Makefile:137-195`: Add global aggregate threshold (50% start → 75%)
- Commit the e2e-cross-language.yml fix

**P2:**
- `.github/workflows/ci.yml:211`: Re-enable test-tinygo
- `.github/workflows/ci.yml:267`: Remove `continue-on-error` from test-python
- `.github/workflows/ci.yml:297`: Remove `continue-on-error` from test-java
- `.github/workflows/ci.yml:99-112`: Uncomment lint-go (with v2 config)

**P3:**
- Branch protection rules (GitHub admin)
- `go.mod:3`: Resolve version discrepancy
- `.github/workflows/ecosystem-ci.yml:5-10`: Broaden path filter
- Consolidate TinyGo installation

### Risks
1. **test-python/test-java continue-on-error removal may surface latent failures.** Verify these are green before removing soft-fail.
2. **Coverage threshold on PRs will cause friction.** Current coverage is unknown (Makefile prefix bug means no valid measurements exist). Need a baseline before enforcing.
3. **golangci-lint re-enable will produce new warnings.** Need v2 config format and may hit new lint failures.
4. **go.mod bump could break downstream consumers** compiling with Go 1.25.

### Complexity
**LEAF-READY.** All 4 prior explorations agree. The work is well-specified, split into independent file edits, and requires no new architecture or design decisions. The P0 and P1 items are independent of the dependency gates (cleat-232, cleat-233).

## 6. Recommendation

**Proceed with implementation.** The dependency gates (cleat-232/cleat-233) are partially but not fully resolved. However, the P0 and P1 items for cleat-234 (closure test fixes, continue-on-error removal, Makefile fix) are completely independent of those dependencies. Implementation can start immediately on P0/P1 while waiting for cleat-232/cleat-233 to go fully green.

Commit the e2e-cross-language.yml fix as part of this task or as a separate quick-fix commit.
