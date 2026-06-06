# cleat-234ee Exploration: Closure Test Fixes + P0 CI Hardening

**Explorer:** cleat-234ee
**Date:** 2026-06-06
**Task:** cleat-234e — Fix closure tests and remove unsafe continue-on-error from CI

## Summary

Verified the two closure test failures and the P0 CI `continue-on-error` removals. All findings from the prior cleat-234 exploration (STATUS.md) are confirmed accurate. The root cause, required fixes, and CI changes are identical. This report adds independent verification by running the tests and re-inspecting the relevant code.

## 1. Closure Test Failures — VERIFIED

### Root Cause: `LongRunning()` added to testdata without updating expectations

**File:** `testdata/basic/order.go:175-182`
```go
func LongRunning(h cleat.HostCalls, iterations int) (string, error) {
    for i := 0; i < iterations; i++ {
        if _, err := h.DurableCall("noop", "", ""); err != nil {
            return "", err
        }
    }
    return "done", nil
}
```

`LongRunning` calls `h.DurableCall("noop", "", "")` directly — this makes it a 9th durable leaf. The test expectations still assume 8 leaves (the original 8 functions in order.go).

### Test 1: TestComputeBasicIdentifiesDurableLeaves — FAIL (verified)

```
--- FAIL: TestComputeBasicIdentifiesDurableLeaves (0.54s)
```

**Expected:** 8 durable leaves (checkItemAvailability, getDefaultPaymentMethod, fulfillOrder, reserveInventory, chargeCustomer, releaseReservation, refundPayment, notifyCustomer)
**Got:** 9 (above + LongRunning)

**Fix:** Add `basicFQ("LongRunning"): true` to `expectedLeaves` at `internal/closure/closure_test.go:40`.

### Test 2: TestComputeBasicCorrectlyTagsPureFunctions — FAIL (verified)

```
--- FAIL: TestComputeBasicCorrectlyTagsPureFunctions (0.55s)
```

**Why:** `totalFuncs` is expected to be 12 (8 leaves + 4 closure), but LongRunning adds a 13th function.

**Fix:** Change `totalFuncs != 12` to `totalFuncs != 13` at `internal/closure/closure_test.go:120`.

### Required Edits (unchanged from prior exploration)

| File | Line | Change |
|------|------|--------|
| `internal/closure/closure_test.go` | 40 | Add `basicFQ("LongRunning"): true,` to expectedLeaves |
| `internal/closure/closure_test.go` | 120 | Change `12` to `13` |

All other closure tests pass (23 total, 2 fail).

## 2. P0 CI Changes — VERIFIED

### 2.1 Lint job: Remove continue-on-error

**File:** `.github/workflows/ci.yml:52`
```yaml
lint:
    name: Lint
    runs-on: ubuntu-latest
    continue-on-error: true    # <-- REMOVE THIS LINE
```

The lint job runs `go vet ./...`, which is a correctness check. Its failures should block PRs. The two non-Go linters (Ruff, ShellCheck) already have their own `continue-on-error: true` at the step level (lines 81, 85), so removing the job-level soft-fail won't affect them.

### 2.2 Build job: Remove continue-on-error

**File:** `.github/workflows/ci.yml:490`
```yaml
build:
    name: Build
    runs-on: ubuntu-latest
    continue-on-error: true    # <-- REMOVE THIS LINE
```

Build failures on PRs are deploy blockers and should prevent merge. This is a straightforward removal.

## 3. Coverage Enforcement — Additional Verification

### 3.1 Makefile module prefix bug (line 158)

**Current:** `sub(/^github\.com\/rcownie\/cleat\//, "", path);`
**go.mod:** `module github.com/cleat-team/cleat`

The awk substitution won't match any package paths, so the coverage threshold check effectively operates on full package paths without stripping the module prefix. The per-package thresholds (internal: 50%, cleat: 15%, etc.) are compared against the full `github.com/cleat-team/cleat/internal` prefix — they likely still match because the `index(p, prefix "/") == 1` check in awk is a substring check. So this bug is **cosmetic** but should still be fixed.

### 3.2 Coverage job only runs on main pushes

**File:** `.github/workflows/ci.yml:556`
```yaml
if: github.ref == 'refs/heads/main' && github.event_name == 'push'
```

PRs never see coverage results. To enforce coverage on PRs, this guard needs to be extended to include PR events.

### 3.3 No global aggregate threshold

The Makefile only has per-package thresholds (cleat/: 15%, internal/: 50%, plugins/: 20%). There is no project-wide 50%→75% aggregate threshold as specified in the task requirements.

## 4. Go Version Discrepancy

| Source | Go Version |
|--------|-----------|
| `go.mod:3` | `go 1.25.7` |
| `ci.yml:33` (`GO_VERSION_STABLE`) | `"1.26"` |
| `ci.yml` test-go matrix | `["1.26"]` |

The go.mod declares 1.25.7 but CI compiles and tests with 1.26. Go 1.26 is backward-compatible with 1.25 code, so this isn't breaking anything, but it's a discrepancy that should be resolved: either bump go.mod to `go 1.26` or downgrade CI to 1.25.

## 5. Remaining continue-on-error Audit (12 total)

The prior exploration identified safe vs. unsafe continue-on-error. This is a re-verification:

| # | File | Line | Job/Step | Keep? |
|---|------|------|----------|-------|
| 1 | ci.yml | 52 | Lint job | **REMOVE** |
| 2 | ci.yml | 81 | Ruff check (step) | Keep |
| 3 | ci.yml | 85 | ShellCheck (step) | Keep |
| 4 | ci.yml | 267 | test-python job | **REMOVE** |
| 5 | ci.yml | 297 | test-java job | **REMOVE** |
| 6 | ci.yml | 319 | test-assemblyscript job | Keep |
| 7 | ci.yml | 369 | test-assemblyscript-wasm job | Keep |
| 8 | ci.yml | 490 | build job | **REMOVE** |
| 9 | ci.yml | 558 | coverage job | Keep (informational) |
| 10 | release-notes-check.yml | 20 | check-release-notes | Keep |
| 11 | ai-pr-review.yml | 33 | AI review | Keep |
| 12 | ecosystem-ci.yml | 63 | assemblyscript-sdk | Keep |

**Safe to remove (5):** Lint (52), test-python (267), test-java (297), build (490), and (once coverage runs on PRs) coverage (558).

## 6. Dependency Status

cleat-234 depends on cleat-232 (multi-DB fixes) and cleat-233 (SDK tests). However, the closure test fixes and `continue-on-error` removals are entirely independent of those dependencies:
- The test fixes only touch `internal/closure/closure_test.go`
- The CI config changes only touch `.github/workflows/ci.yml`
- Neither change interacts with multi-DB or SDK code

**These changes can be implemented now, regardless of cleat-232/233 status.**

## 7. Implementation Order

| Priority | Change | Dependencies |
|----------|--------|-------------|
| **P0** | Fix 2 closure test expectations | None |
| **P0** | Remove continue-on-error from lint job | None |
| **P0** | Remove continue-on-error from build job | None |
| **P1** | Fix Makefile module prefix | None |
| **P1** | Extend coverage to PRs | None |
| **P1** | Add global coverage threshold | None |
| **P2** | Re-enable test-tinygo | TinyGo v0.41.1 compatibility verified |
| **P2** | Remove continue-on-error from test-python, test-java | None |
