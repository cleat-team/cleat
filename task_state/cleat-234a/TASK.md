# cleat-234a: Verify and update cleat-234 exploration

**Parent:** cleat-234 (CI Enforcement)
**Budget:** $3 (~0.5 days)
**Priority:** 1
**Type:** exploration-verification

## Scope

Verify the key claims in the cleat-234 exploration (2026-06-05) against current code state. The parent exploration is thorough but code may have changed. This verification ensures the implementation plan is based on current reality.

## Actions

1. Run closure tests to confirm they still fail as described
2. Verify ci.yml state: continue-on-error flags, coverage job trigger, test-tinygo status
3. Verify ecosystem-ci.yml state: path filters, AS best-effort
4. Verify Makefile coverage module prefix bug
5. Check if go.mod version discrepancy still exists
6. Verify the testdata/basic/order.go LongRunning function exists as described

## Key Files

- `internal/closure/closure_test.go`
- `internal/closure/testdata/basic/order.go`
- `.github/workflows/ci.yml`
- `.github/workflows/ecosystem-ci.yml`
- `Makefile`
- `go.mod`
