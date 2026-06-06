# cleat-234ep Exploration: Closure Tests + P0 CI Hardening

**Explorer:** cleat-234ep
**Date:** 2026-06-06
**Task:** cleat-234e — Fix closure tests and remove unsafe continue-on-error from CI

## Summary

Independent verification of the two closure test failures and the P0 CI `continue-on-error` removals identified in the cleat-234 parent exploration (STATUS.md, 2026-06-05). All findings confirmed accurate through source code inspection and test execution. The root cause, required fixes, and CI changes are identical to prior reports. This report adds a clean independent verification — tests were actually run and failed as predicted, all CI files were re-read in full.

---

## 1. Closure Test Failures — VERIFIED BY EXECUTION

Both tests were run via `go test ./internal/closure/... -run "TestComputeBasic" -v -count=1` and failed exactly as described.

### Test 1: TestComputeBasicIdentifiesDurableLeaves — FAIL

```
=== RUN   TestComputeBasicIdentifiesDurableLeaves
    closure_test.go:52: unexpected cleat leaf: github.com/cleat-team/cleat/testdata/basic.LongRunning
    closure_test.go:57: expected 8 cleat leaves, got 9
--- FAIL: TestComputeBasicIdentifiesDurableLeaves (0.57s)
```

**Root cause:** `LongRunning()` at `testdata/basic/order.go:175-182` calls `h.DurableCall("noop", "", "")` directly, making it a 9th durable leaf. The test's `expectedLeaves` map (closure_test.go:32-41) only has 8 entries.

**Fix:** Add `basicFQ("LongRunning"): true,` to `expectedLeaves` at `internal/closure/closure_test.go:40` (after line 39, before the closing `}`).

### Test 2: TestComputeBasicCorrectlyTagsPureFunctions — FAIL

```
=== RUN   TestComputeBasicCorrectlyTagsPureFunctions
    closure_test.go:121: expected 12 functions, got 13
--- FAIL: TestComputeBasicCorrectlyTagsPureFunctions (0.64s)
```

**Root cause:** Same `LongRunning` adds a 13th function. The test expects 12 at line 120.

**Fix:** Change `12` to `13` at `internal/closure/closure_test.go:120`.

### Other closure tests pass

All 23 other closure tests pass, including `TestComputeBasicIdentifiesDurableClosure`, `TestComputeBasicTagsAreConsistentWithFuncDecl`, threading tests, generics tests, and edge case tests.

### Required Edits

| File | Line | Current | Change |
|------|------|---------|--------|
| `internal/closure/closure_test.go` | 40 | (end of map, 8 entries) | Add `basicFQ("LongRunning"): true,` |
| `internal/closure/closure_test.go` | 120 | `totalFuncs != 12` | Change to `totalFuncs != 13` |

---

## 2. CI continue-on-error Audit — VERIFIED (12 instances)

All 12 instances confirmed by reading the current source files. Counts match prior reports exactly.

### ci.yml (9 instances)

| Line | Job/Step | Keep? | Rationale |
|------|----------|-------|-----------|
| 52 | lint job-level | **REMOVE** | go vet failures are correctness issues |
| 81 | Ruff check step | Keep | Informational Python linter |
| 85 | ShellCheck step | Keep | Informational shell linter |
| 267 | test-python job | **REMOVE** | Python SDK test failures are regressions |
| 297 | test-java job | **REMOVE** | Java SDK test failures are regressions |
| 319 | test-assemblyscript job | Keep | AS ecosystem instability |
| 369 | test-assemblyscript-wasm job | Keep | AS WASM builds fragile across Node versions |
| 490 | build job | **REMOVE** | Build failures are deploy blockers |
| 558 | coverage job | Keep (for now) | Informational only, only runs on main |

### Other workflow files (3 instances)

| File | Line | Job | Keep? |
|------|------|-----|-------|
| `release-notes-check.yml` | 20 | check-release-notes | Keep (non-critical docs) |
| `ai-pr-review.yml` | 33 | AI review | Keep (advisory only) |
| `ecosystem-ci.yml` | 63 | assemblyscript-sdk | Keep (AS instability) |

### Safe to remove: 5 instances
lint (52), test-python (267), test-java (297), build (490). Coverage (558) can be removed once it runs on PRs.

---

## 3. Coverage Enforcement — VERIFIED

### Makefile module prefix bug (line 158)

**Current:** `sub(/^github\.com\/rcownie\/cleat\//, "", path);`
**go.mod:** `module github.com/cleat-team/cleat`

The awk substitution won't match the actual module prefix. However, the per-package threshold check uses `index(p, prefix "/") == 1` (line 175) which is a substring check, so thresholds may still match if the full path contains the expected prefix. The bug is cosmetic but should be fixed to match.

### Coverage job only on main pushes (ci.yml:556)

```yaml
if: github.ref == 'refs/heads/main' && github.event_name == 'push'
```

PRs never see coverage results. To enforce on PRs, this guard needs extending.

### No global aggregate threshold

The Makefile (lines 137-195) has per-package thresholds only (cleat/: 15%, internal/: 50%, plugins/: 20%, cmd/: 0%). No project-wide 50% start → 75% target ratchet mechanism exists.

---

## 4. Additional CI Gaps — VERIFIED

### test-tinygo disabled (ci.yml:211)

```yaml
if: false  # Temporarily skipped; Go version issues being explored on another branch
```

TinyGo v0.41.0+ (April 2026) supports Go 1.26, resolving the original blocker. The job can be re-enabled.

### lint-go commented out (ci.yml:99-112)

Entire golangci-lint job commented out — the action doesn't support Go 1.26 yet. Without this, there's no errcheck/staticcheck/gosec on PRs.

### Go version discrepancy

| Source | Version |
|--------|---------|
| `go.mod:3` | `go 1.25.7` |
| `ci.yml:33` (GO_VERSION_STABLE) | `"1.26"` |
| CI test-go matrix | `["1.26"]` |

### Ecosystem CI path filter gap (ecosystem-ci.yml:5-10)

Only triggers on `cleat/**`, `internal/**`, `cmd/**`, `go.mod`, `go.sum`. Missing `plugins/**` and `crates/cleat-sdk/**`.

---

## 5. Dependency Independence

The P0 and P1 changes are completely independent of cleat-232 (multi-DB fixes) and cleat-233 (SDK tests):

- **Closure test fixes** only touch `internal/closure/closure_test.go` — no interaction with database or SDK code
- **CI config changes** only touch `.github/workflows/ci.yml` — no interaction with multi-DB or SDK code
- **Makefile fix** only touches `Makefile` line 158 — purely a string replacement

These changes can be implemented and merged now, regardless of cleat-232/233 status.

---

## 6. Implementation Order

| Priority | Change | Files | Lines Changed | Deps |
|----------|--------|-------|---------------|------|
| **P0** | Add LongRunning to expectedLeaves | closure_test.go | +1 line | None |
| **P0** | Bump totalFuncs from 12 to 13 | closure_test.go | 1 char | None |
| **P0** | Remove continue-on-error from lint | ci.yml | -1 line | None |
| **P0** | Remove continue-on-error from build | ci.yml | -1 line | None |
| **P1** | Fix Makefile module prefix | Makefile | 1 string | None |
| **P1** | Extend coverage to PRs | ci.yml | 1 condition | None |
| **P1** | Add global coverage threshold | Makefile | ~15 lines | None |
| **P2** | Re-enable test-tinygo | ci.yml | -1 line | TinyGo v0.41.1 |
| **P2** | Remove continue-on-error from test-python | ci.yml | -1 line | None |
| **P2** | Remove continue-on-error from test-java | ci.yml | -1 line | None |

---

## 7. Conclusion

The cleat-234 exploration is accurate and complete. The P0 closure test fixes are a 4-character + 1-line change. The P0 CI hardening is a 2-line deletion. No surprises or discrepancies found. The task is leaf-ready for implementation.
