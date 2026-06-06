# cleat-234cr Exploration Report

**Explorer:** cleat-234cr
**Date:** 2026-06-06
**Task:** cleat-234c — CI Enforcement (re-verification)
**Prior explorations:** cleat-234ce (2026-06-06), explorer agent / cto-lap-032 (2026-06-05)

## Verdict: ALL CLAIMS VERIFIED — NO CHANGES SINCE 2026-06-06

Every substantive claim from the prior exploration reports has been verified against current file contents. Zero regressions, zero fixes applied, zero new issues found. The codebase is identical to the state documented by cleat-234ce.

---

## 1. Closure Test Failures — VERIFIED (STILL BROKEN)

**`internal/closure/closure_test.go`** read directly. Both failures confirmed:

| Test | Line | Expected | Actual | Status |
|------|------|----------|--------|--------|
| `TestComputeBasicIdentifiesDurableLeaves` | 40 | 8 leaves (missing LongRunning) | 9 leaves | STILL FAILING |
| `TestComputeBasicCorrectlyTagsPureFunctions` | 120 | `totalFuncs != 12` | 13 funcs | STILL FAILING |

**Root cause confirmed:** `LongRunning()` at `testdata/basic/order.go:175` calls `h.DurableCall("noop", "", "")` at line 177, making it a durable leaf. The `expectedLeaves` map at line 32-41 contains 8 entries but should contain 9.

**Required fixes (unchanged):**
- Line 40 (after `basicFQ("notifyCustomer"): true,`): Add `basicFQ("LongRunning"): true,`
- Line 120: Change `totalFuncs != 12` to `totalFuncs != 13`

---

## 2. CI Workflow (`ci.yml`) — VERIFIED

### continue-on-error audit: 9 occurrences in ci.yml — all confirmed

| # | Line | Job/Step | continue-on-error | Verdict |
|---|------|----------|-------------------|---------|
| 1 | 52 | `lint` job | true | REMOVE |
| 2 | 81 | Ruff check step | true | Keep |
| 3 | 85 | ShellCheck step | true | Keep |
| 4 | 267 | `test-python` job | true | REMOVE |
| 5 | 297 | `test-java` job | true | REMOVE |
| 6 | 319 | `test-assemblyscript` job | true | Keep |
| 7 | 369 | `test-assemblyscript-wasm` job | true | Keep |
| 8 | 490 | `build` job | true | REMOVE |
| 9 | 558 | `coverage` job | true | Keep |

### Other CI claims verified:
- **lint-go commented out:** Lines 99-112 entirely commented out — confirmed
- **test-tinygo disabled:** Line 211 has `if: false` with comment "Temporarily skipped; Go version issues being explored on another branch" — confirmed
- **Coverage job gated:** Line 556: `if: github.ref == 'refs/heads/main' && github.event_name == 'push'` — PRs never check coverage — confirmed
- **Go version discrepancy:** `go.mod:3` has `go 1.25.7`, CI `ci.yml:33` has `GO_VERSION_STABLE: "1.26"` — confirmed

---

## 3. Other Workflow Files — VERIFIED

| File | Claim | Status |
|------|-------|--------|
| `release-notes-check.yml:20` | 1 `continue-on-error` | Confirmed |
| `ai-pr-review.yml:33` | 1 `continue-on-error` | Confirmed |
| `ecosystem-ci.yml:63` | 1 `continue-on-error` (assemblyscript-sdk) | Confirmed |
| `multi-db-ci.yml` | No `continue-on-error` | Confirmed (grep returned no matches) |
| `e2e-cross-language.yml` | No `continue-on-error` | Confirmed (grep returned no matches) |

### Ecosystem CI path filter (verified):
- Lines 5-10: `cleat/**`, `internal/**`, `cmd/**`, `go.mod`, `go.sum`
- Still missing `plugins/**` and `crates/cleat-sdk/**`

---

## 4. Makefile Coverage Check — VERIFIED

- **Wrong module prefix:** Line 158: `sub(/^github\.com\/rcownie\/cleat\//, "", path)` — uses old name. `go.mod:1` declares `github.com/cleat-team/cleat` — confirmed
- **No global threshold:** Only per-package thresholds (lines 143-151): `internal/host` 15%, `internal/host/testutil` 0%, `internal/plugin` 50%, `internal/migration` 0%, `internal` 50%, `cleat` 15%, `plugins` 20%, `cmd` 0%
- **No PR enforcement:** Coverage job (ci.yml:556) only runs on main pushes

---

## 5. TinyGo Installations — VERIFIED

4 places confirmed (matching cleat-234ce's correction from 3 to 4):

| File | Job | Lines |
|------|-----|-------|
| `ci.yml` | test-go (internal package, Go 1.26 only) | 171-181 |
| `ci.yml` | test-tinygo (disabled, `if: false`) | 235-244 |
| `plugin-harness-ci.yml` | test-layer2 | 40-49 |
| `plugin-harness-ci.yml` | test-multi-db | 106-115 |

---

## 6. Git History Since 2026-06-06

No commits touch any of the key files (`internal/closure/`, `.github/workflows/ci.yml`, `.github/workflows/ecosystem-ci.yml`, `Makefile`, `go.mod`) since the prior exploration.

---

## 7. Delta from cleat-234ce Report (2026-06-06)

| Category | cleat-234ce Finding | cleat-234cr Re-verification |
|----------|---------------------|----------------------------|
| Closure tests | BROKEN | BROKEN — identical |
| ci.yml continue-on-error | 9 occurrences | 9 — all same lines |
| Other workflows continue-on-error | 3 more (12 total) | 12 total — confirmed |
| Module prefix in Makefile | Wrong (`rcownie/cleat`) | Wrong — unchanged |
| Coverage on PRs | Not running | Not running — unchanged |
| test-tinygo | Disabled (`if: false`) | Disabled — unchanged |
| Go version discrepancy | 1.25.7 vs 1.26 | Still discrepant |
| Ecosystem CI path filter | Missing plugins + crates | Still missing |
| TinyGo install count | 4 places | 4 — confirmed |
| lint-go | Commented out | Still commented out |

**New issues found:** 0
**Prior issues resolved:** 0
**Corrections to prior reports:** 0

---

## 8. Files Requiring Changes (Priority-Ordered, Verified Current)

| Priority | File | Change |
|----------|------|--------|
| **P0** | `internal/closure/closure_test.go:40` | Add `LongRunning` to `expectedLeaves` |
| **P0** | `internal/closure/closure_test.go:120` | Change `12` → `13` |
| **P0** | `.github/workflows/ci.yml:52` | Remove `continue-on-error` from lint job |
| **P0** | `.github/workflows/ci.yml:490` | Remove `continue-on-error` from build job |
| **P1** | `.github/workflows/ci.yml:556` | Extend coverage to run on PRs |
| **P1** | `.github/workflows/ci.yml:558` | Remove `continue-on-error` from coverage (once on PRs) |
| **P1** | `Makefile:158` | Fix module prefix: `rcownie/cleat` → `cleat-team/cleat` |
| **P1** | `Makefile:137-195` | Add global aggregate threshold (50%→75%) |
| **P2** | `.github/workflows/ci.yml:211` | Re-enable test-tinygo (remove `if: false`) |
| **P2** | `.github/workflows/ci.yml:267` | Remove `continue-on-error` from test-python |
| **P2** | `.github/workflows/ci.yml:297` | Remove `continue-on-error` from test-java |
| **P2** | `.github/workflows/ci.yml:99-112` | Re-enable lint-go if golangci-lint supports Go 1.26 |
| **P3** | Branch protection (main/develop) | Add required status checks (GitHub admin needed) |
| **P3** | `go.mod:3` | Resolve Go version (1.25.7 vs CI's 1.26) |
| **P3** | `.github/workflows/ecosystem-ci.yml:5-10` | Add `plugins/**`, `crates/cleat-sdk/**` to path filter |
| **P3** | `.github/workflows/ci.yml` | Consolidate TinyGo install into composite action |
