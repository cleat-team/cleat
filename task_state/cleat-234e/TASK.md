# cleat-234e: Fix closure tests + P0 CI hardening

**Parent:** cleat-234 (CI Enforcement)
**Budget:** $3 (~0.5 days)
**Priority:** 1
**Type:** exploration-verification

## Scope

Verify and prepare implementation of the P0/P1 closure test fixes and CI hardening items from cleat-234. This sub-task covers the changes that are independent of cleat-232/cleat-233 dependencies and can proceed immediately.

## Actions

1. Run closure tests to confirm failure state
2. Verify root cause: LongRunning() at testdata/basic/order.go:175
3. Verify the exact lines needing changes in closure_test.go (lines 40, 120)
4. Confirm CI continue-on-error state at ci.yml:52 (lint) and ci.yml:490 (build)
5. Verify Makefile module prefix bug at line 158
6. Verify coverage job only runs on main pushes (ci.yml:556)
7. Confirm test-tinygo disabled state (ci.yml:211) and golangci-lint commented out (ci.yml:99-112)
8. Count and classify all 12 continue-on-error instances across 4 workflow files

## Key Files

- `internal/closure/closure_test.go`
- `testdata/basic/order.go`
- `.github/workflows/ci.yml`
- `.github/workflows/ecosystem-ci.yml`
- `.github/workflows/release-notes-check.yml`
- `.github/workflows/ai-pr-review.yml`
- `Makefile`
- `go.mod`
