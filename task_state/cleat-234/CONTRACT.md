# CONTRACT: cleat-234 — CI Enforcement

## Deliverables

1. **CI config updated**: All required checks run on PRs (Go test, vet, vulncheck, multi-db)
2. **Ecosystem CI config updated**: All SDKs tested in CI
3. **Coverage check added**: CI fails if coverage drops below threshold (start 50%, target 75%)
4. **Closure tests fixed**: `TestComputeBasicIdentifiesDurableLeaves` and `TestComputeBasicCorrectlyTagsPureFunctions` pass
5. **Branch protection review**: Document required checks for `develop` and `main`

## Invariants

- No reduction in test coverage
- CI remains fast enough for PR workflow (<15 min)
- `continue-on-error: true` reduced only where safe (don't break CI on flaky external deps)

## Dependency Gate

This task REQUIRES cleat-232 (multi-db tests green) and cleat-233 (SDK tests green) to complete first. Do not start implementation until dependencies are verified green.

## Key Changes

| File | Change |
|------|--------|
| `.github/workflows/ci.yml` | Add multi-db matrix, vet, vulncheck; remove unnecessary continue-on-error |
| `.github/workflows/ecosystem-ci.yml` | Ensure all SDKs covered |
| `internal/closure/` | Fix 2 failing tests (DurableCall in LongRunning miscounts leaves) |
| Coverage config | Add coverage threshold (50% initial, 75% target) |

## Test Requirements

- `go test ./internal/closure/...` passes (2 currently failing tests fixed)
- CI config changes verified by running CI on this branch
- Coverage threshold enforced but not overly strict initially

## Integration Points

- Consumes green test results from cleat-232 (multi-db) and cleat-233 (SDK)
- Branch protection requires GitHub admin — document what's needed, don't block on it
- Closure test root cause: `LongRunning` at `testdata/basic/order.go:175` calls `DurableCall()` — not in expected leaves (8 expected, 9 actual)

## Coupling

- MEDIUM with `cleat-232` (consumes green multi-db CI)
- MEDIUM with `cleat-233` (consumes green SDK tests)
- NONE with other leaf tasks
