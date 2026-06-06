# cleat-234ce Exploration Report

**Explorer:** cleat-234ce
**Date:** 2026-06-06
**Task:** cleat-234 — CI Enforcement (verification of prior exploration)
**Previous exploration:** 2026-06-05 STATUS.md (explorer agent, cto-lap-032)

## Verdict: ALL CLAIMS VERIFIED — NO CHANGES SINCE 2026-06-05

Every substantive claim from the 2026-06-05 STATUS.md has been verified against current file contents. No regressions, no fixes applied, no new issues found.

---

## 1. Closure Test Failures — VERIFIED (STILL BROKEN)

**`internal/closure/closure_test.go`** read directly. Both failures confirmed:

| Test | Line | Expected | Actual | Status |
|------|------|----------|--------|--------|
| `TestComputeBasicIdentifiesDurableLeaves` | 40 | 8 leaves (missing LongRunning) | 9 leaves | STILL FAILING |
| `TestComputeBasicCorrectlyTagsPureFunctions` | 120 | `totalFuncs != 12` | 13 funcs | STILL FAILING |

**Root cause confirmed:** `LongRunning()` at `testdata/basic/order.go:175` calls `h.DurableCall("noop", "", "")` at line 177, making it a durable leaf. The test expectations in `closure_test.go` were never updated to account for it.

**Required fixes (unchanged from prior report):**
- Line 40 (after `basicFQ("notifyCustomer"): true,`): Add `basicFQ("LongRunning"): true,`
- Line 120: Change `totalFuncs != 12` to `totalFuncs != 13`

---

## 2. CI Workflow (`ci.yml`) — VERIFIED

### continue-on-error audit: 12 occurrences across 4 workflow files — all confirmed

**ci.yml (9 occurrences):**
| # | Line | Job/Step | continue-on-error | Verdict |
|---|------|----------|-------------------|---------|
| 1 | 52 | `lint` job | true | REMOVE (go vet should block) |
| 2 | 81 | Ruff check step | true | Keep (informational) |
| 3 | 85 | ShellCheck step | true | Keep (informational) |
| 4 | 267 | `test-python` job | true | REMOVE (Python regressions are real) |
| 5 | 297 | `test-java` job | true | REMOVE (Java regressions are real) |
| 6 | 319 | `test-assemblyscript` job | true | Keep (AS instability) |
| 7 | 369 | `test-assemblyscript-wasm` job | true | Keep (AS WASM fragile) |
| 8 | 490 | `build` job | true | REMOVE (build failures block deploy) |
| 9 | 558 | `coverage` job | true | Keep (main-only, informational) |

**release-notes-check.yml (1):**
| 10 | 20 | `check-release-notes` job | true | Keep (non-critical docs) |

**ai-pr-review.yml (1):**
| 11 | 33 | `review` job | true | Keep (advisory only) |

**ecosystem-ci.yml (1):**
| 12 | 63 | `assemblyscript-sdk` job | true | Keep (AS instability) |

### Other CI claims verified:
- **lint-go commented out:** Lines 99-112 entirely commented out ✅
- **test-tinygo disabled:** Line 211 has `if: false` ✅
- **Coverage job gated:** Line 556: `if: github.ref == 'refs/heads/main' && github.event_name == 'push'` — PRs never check coverage ✅
- **Go version discrepancy:** `go.mod:3` has `go 1.25.7`, CI `ci.yml:33` has `GO_VERSION_STABLE: "1.26"` ✅

---

## 3. Ecosystem CI (`ecosystem-ci.yml`) — VERIFIED

- Path filter (lines 5-10): `cleat/**`, `internal/**`, `cmd/**`, `go.mod`, `go.sum` — missing `plugins/**` and `crates/cleat-sdk/**` ✅
- No Go compilation step — confirmed, only SDK-level tests (Python, Rust, Java, AS) ✅
- `assemblyscript-sdk` has `continue-on-error: true` at line 63 ✅

---

## 4. Makefile Coverage Check — VERIFIED

- **Wrong module prefix:** Line 158: `sub(/^github\.com\/rcownie\/cleat\//, "", path);` — uses old name. `go.mod:1` declares `github.com/cleat-team/cleat` ✅
- **No global threshold:** Only per-package thresholds (lines 143-151). No aggregate 50%→75% ratchet ✅
- **No PR enforcement:** Coverage job (ci.yml:554-556) only runs on main pushes ✅

---

## 5. TinyGo Installation — VERIFIED

TinyGo installed in **4 places** (prior report said 3, minor correction):

| File | Job | Lines |
|------|-----|-------|
| ci.yml | test-go (internal package, Go 1.26 only) | 170-181 |
| ci.yml | test-tinygo (disabled, `if: false`) | 235-244 |
| plugin-harness-ci.yml | test-layer2 | 40-49 |
| plugin-harness-ci.yml | test-multi-db | 106-115 |

Installation logic is subtly different across jobs (slightly different curl flags, different fallback patterns). Consolidation recommendation still valid.

---

## 6. test-tinygo Re-enable Assessment — VERIFIED

- `if: false` at ci.yml:211 confirmed
- Job runs `internal/wasm`, `internal/host`, `internal/closure`, `internal/analyzer` tests — would catch closure test regressions
- TinyGo v0.41.1 (April 22, 2026) added Go 1.25+ backward compatibility — the "Go version issues" blocker is resolved
- Re-enable recommendation unchanged: enable with `continue-on-error: true` initially, observe stability, then hard-fail

---

## 7. Multi-DB CI (`multi-db-ci.yml`) — VERIFIED

- 3 jobs: test-mysql, test-mssql, test-plugin-migrations (all 3 backends)
- No `continue-on-error` on any job — database failures block PRs ✅
- Plugin migration smoke test covers all 3 backends simultaneously ✅
- Separate from `ci.yml` — architecturally fine, but means no single unified status ✅

---

## 8. Cross-Language E2E (`e2e-cross-language.yml`) — VERIFIED

- Tests Rust, Python, AssemblyScript, Java WASM E2E through Go host runtime
- No `continue-on-error` — all language failures block the job ✅
- Runs on all PRs to main/develop ✅

---

## 9. Delta from Prior Report (2026-06-05)

| Item | Prior Status | Current Status |
|------|-------------|----------------|
| Closure test failures | BROKEN (9 leaves vs 8 expected) | BROKEN — unchanged |
| continue-on-error count | 12 across 4 files | 12 — confirmed |
| Module prefix in Makefile | Wrong (`rcownie/cleat`) | Wrong — unchanged |
| Coverage on PRs | Not running | Not running — unchanged |
| test-tinygo | Disabled (`if: false`) | Disabled — unchanged |
| Go version discrepancy | 1.25.7 vs 1.26 | Still discrepant — unchanged |
| Ecosystem CI path filter | Missing plugins + crates | Still missing — unchanged |
| TinyGo install count | 3 places reported | **4 places** (correction: plugin-harness-ci has 2, not 1) |

**New issues found:** 0
**Prior issues resolved:** 0
**Corrections to prior report:** 1 (TinyGo install count: 3 → 4)

---

## 10. Files Requiring Changes (Priority-Ordered, Verified Current)

| Priority | File | Change |
|----------|------|--------|
| **P0** | `internal/closure/closure_test.go:40` | Add `LongRunning` to `expectedLeaves` |
| **P0** | `internal/closure/closure_test.go:120` | Change `12` → `13` |
| **P0** | `.github/workflows/ci.yml:52` | Remove `continue-on-error` from lint job |
| **P0** | `.github/workflows/ci.yml:490` | Remove `continue-on-error` from build job |
| **P1** | `.github/workflows/ci.yml:554-556` | Extend coverage to run on PRs |
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

## 11. Dependency Status

- **cleat-232** (multi-DB): STATUS.md says "in_progress". Multi-db-ci.yml exists and is correct regardless. The closure test fixes and CI `continue-on-error` removals are independent of cleat-232 completion.
- **cleat-233** (SDK tests): STATUS.md says "executing". The Python/Java `continue-on-error` removals should ideally wait for cleat-233 to confirm test stability, but the closure fixes and other CI changes are independent.

**Recommendation:** Closure test fixes (P0) and lint/build `continue-on-error` removals (P0) can proceed immediately without dependency resolution.
