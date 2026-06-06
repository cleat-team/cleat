# cleat-234bi Investigation: Independent Verification of cleat-234

**Investigator:** cleat-234bi
**Date:** 2026-06-06
**Task:** Independent verification of cleat-234 (CI Enforcement) claims against current code state

## Summary

All major claims from the cleat-234 exploration (STATUS.md, 2026-06-05) verified against current code. One critical new finding: the Makefile coverage check bug is more severe than previously reported.

## 1. Closure Test Failures — CONFIRMED

### Root cause verified independently

- `testdata/basic/order.go:175-182`: `LongRunning()` calls `h.DurableCall("noop", "", "")` directly — makes it a 9th durable leaf
- `internal/closure/closure_test.go:32-41`: `expectedLeaves` map has 8 entries, missing `LongRunning`
- `internal/closure/closure_test.go:120`: expects `totalFuncs != 12`, should be 13

### Path correction

Previous explorations correctly reference files. The testdata lives at repo-root `testdata/basic/order.go` (NOT `internal/closure/testdata/basic/`). The `basicFQ` helper in `closure_test.go:14` maps to `github.com/cleat-team/cleat/testdata/basic.` — consistent with the `LoadPackages` call. No `testdata/` directory exists under `internal/closure/`.

### Required fixes (unchanged)

| File | Line | Change |
|------|------|--------|
| `internal/closure/closure_test.go` | 40 | Add `basicFQ("LongRunning"): true,` |
| `internal/closure/closure_test.go` | 120 | Change `12` to `13` |

## 2. CI continue-on-error Audit — CONFIRMED (12 total, 4 files)

All 12 confirmed at exact line numbers:

| # | File | Line | Job/Step | Current Verdict |
|---|------|------|----------|-----------------|
| 1 | ci.yml | 52 | Lint job-level | REMOVE |
| 2 | ci.yml | 81 | Ruff (step) | Keep |
| 3 | ci.yml | 85 | ShellCheck (step) | Keep |
| 4 | ci.yml | 267 | test-python job | REMOVE |
| 5 | ci.yml | 297 | test-java job | REMOVE |
| 6 | ci.yml | 319 | test-assemblyscript job | Keep |
| 7 | ci.yml | 369 | test-assemblyscript-wasm job | Keep |
| 8 | ci.yml | 490 | build job | REMOVE |
| 9 | ci.yml | 558 | coverage job | Keep (informational) |
| 10 | release-notes-check.yml | 20 | check-release-notes | Keep |
| 11 | ai-pr-review.yml | 33 | AI review | Keep |
| 12 | ecosystem-ci.yml | 63 | assemblyscript-sdk | Keep |

**5 safe to remove**: lint (52), test-python (267), test-java (297), build (490), and coverage (558 — once enforced on PRs).

## 3. Coverage Job Trigger — CONFIRMED

`ci.yml:556`: `if: github.ref == 'refs/heads/main' && github.event_name == 'push'` — only fires on push to main. PRs never see coverage.

## 4. test-tinygo Disabled — CONFIRMED

`ci.yml:211`: `if: false` with comment "Temporarily skipped; Go version issues being explored on another branch".

## 5. lint-go Commented Out — CONFIRMED

Lines 99-112 entirely commented out. Comment says "action doesn't support Go 1.26 yet".

## 6. Go Version Discrepancy — CONFIRMED

| Source | Go Version |
|--------|-----------|
| `go.mod:3` | `go 1.25.7` |
| `ci.yml:33` (`GO_VERSION_STABLE`) | `"1.26"` |
| `ci.yml` test-go matrix | `["1.26"]` |

## 7. Ecosystem CI Path Filter — CONFIRMED

`ecosystem-ci.yml:5-10`: triggers only on `cleat/**`, `internal/**`, `cmd/**`, `go.mod`, `go.sum`. Missing `plugins/**` and `crates/cleat-sdk/**`.

## 8. Multi-DB CI — VERIFIED

`multi-db-ci.yml` exists and runs correctly. Covers MySQL (service container), SQL Server (service container), and plugin migrations (all three backends). No `continue-on-error: true` anywhere in this file. Clean.

## 9. CRITICAL NEW FINDING: Makefile Coverage Check Is Completely Non-Functional

**Severity: HIGHER than previously reported.**

Previous reports (cleat-234 STATUS.md, cleat-234a, cleat-234ee) describe the Makefile line 158 bug as "cosmetic" or "likely still works because of substring check." This is incorrect. The bug makes the coverage check a **complete no-op** — it never fails regardless of actual coverage.

### Root cause analysis

**Line 158:** `sub(/^github\.com\/rcownie\/cleat\//, "", path)`

The go.mod declares `github.com/cleat-team/cleat`. The regex `github\.com\/rcownie\/cleat\/` never matches any package path. So `path` retains its full form like `github.com/cleat-team/cleat/internal/host`.

**Lines 174-178:** Threshold matching:
```awk
if (p == prefix || index(p, prefix "/") == 1) {
    t = thresh[prefix];
    break;
}
```

This checks if `p` exactly equals `prefix` OR if `prefix/` appears at position 1 in `p`. Since `p` is the full module path (e.g., `github.com/cleat-team/cleat/internal`), neither condition ever matches:
- `p == prefix` fails because the module path is prepended
- `index(p, prefix "/") == 1` fails because `prefix/` does not appear at position 1

**Line 180:** `if (t < 0) continue` — every package gets `t = -1` and is skipped.

**Result:** The awk script runs, prints a table, finds no matching thresholds, prints "Coverage check PASSED", and exits 0. Always. For every package. Regardless of actual coverage.

### Example trace

For a function in `internal/host/workflow.go`:
```
p = "github.com/cleat-team/cleat/internal/host"    (after stripping filename)
prefix = "internal/host"

p == "internal/host"?  → false
index("github.com/cleat-team/cleat/internal/host", "internal/host/")?  → 0 ("internal/host/" not found as substring)
```

t stays -1, package skipped. Coverage check passes vacuously.

### Fix required

Two changes to `Makefile:158`:
1. Fix the module prefix: `github\.com\/rcownie\/cleat` → `github\.com\/cleat-team\/cleat`
2. This alone won't fully fix it — the `index(p, prefix "/") == 1` check assumes prefix matches at position 1, which it will after the `sub()` strips the module prefix (e.g., path becomes `internal/host/something.go` → after file strip → `internal/host`, then `index("internal/host", "internal/host/")` — still returns 0 because no trailing "/").

Wait — even after fixing the prefix, the threshold check has a subtle issue. After `sub()` strips the module prefix, path becomes e.g. `internal/host`. Then `index("internal/host", "internal/host/")` returns 0 because there's no trailing "/". But `index("internal/host/something", "internal/host/")` would match at position 1. So for the EXACT prefix match (when the path equals the prefix exactly with no sub-package), the check fails. For sub-packages (e.g., `internal/host/subpkg`), it works.

**Complete fix for the coverage check:**

Option A: Fix both the prefix and the index check:
```awk
sub(/^github\.com\/cleat-team\/cleat\//, "", path);
# ... in threshold loop:
if (p == prefix || index(p, prefix "/") == 1 || p == prefix) { ... }
```

Option B: Rebuild with Go-native coverage enforcement using `go test -cover` flags instead of awk post-processing.

Recommendation: Option B for reliability, or at minimum fix the module prefix in Option A.

## 10. Additional Pre-existing Issues Noted

### 10.1 CLEAT_TEST_DB vs CLEAT_TEST_POSTGRES inconsistency

`multi-db-ci.yml` header comment says `CLEAT_TEST_POSTGRES` env var but the test infrastructure uses `CLEAT_TEST_DB` for PostgreSQL. Minor documentation issue, not a bug.

### 10.2 15 workflow files in .github/workflows/

The exploration focuses on ci.yml, ecosystem-ci.yml, multi-db-ci.yml, release-notes-check.yml, and ai-pr-review.yml. 10 additional workflows exist (branch-naming, cla-check, dco-check, e2e-cross-language, labeler, plugin-harness-ci, publish-pypi, release, semantic-pull-request, stale). None of these were audited for continue-on-error or coverage gaps. Low priority for this task.

### 10.3 BasicFQ namespace inconsistency

`closure_test.go:14` uses the namespace `github.com/cleat-team/cleat/testdata/basic` but the testdata files live at repo-root `testdata/basic/`. This works because Go resolves the package path via the module, not the filesystem. Not a bug, just a naming quirk worth noting for future test authors.

## 11. Implementation Readiness Assessment

### Can proceed immediately (no dependency blockers):
- P0: Fix closure test expectations (2 lines in closure_test.go)
- P0: Remove `continue-on-error` from lint job (ci.yml:52)
- P0: Remove `continue-on-error` from build job (ci.yml:490)
- P1: Fix Makefile module prefix (Makefile:158) — HIGHER PRIORITY than previously assessed due to complete non-functionality of coverage checks
- P1: Rebuild coverage enforcement with correct module prefix AND fix the threshold matching logic
- P2: Remove `continue-on-error` from test-python, test-java

### Should wait for cleat-232/cleat-233 green:
- P2: Re-enable test-tinygo
- P3: Branch protection changes
- P3: Add global coverage threshold + ratchet

## 12. Risk Assessment

**Highest risk: Coverage enforcement is a complete no-op.** The Makefile coverage-check target has never actually enforced any threshold. When the module prefix is fixed, it will suddenly start failing for packages below threshold (potentially many). This means:
1. Coverage enforcement should be rolled out gradually (50% → ratchet up)
2. The first PR that fixes the prefix bug WILL cause CI failures
3. Those failures are REAL coverage gaps, not false positives

**Second risk: Go version discrepancy.** CI compiles with Go 1.26 but go.mod says 1.25.7. This means local development and CI may have different behavior. The go.mod should be bumped to match CI.
