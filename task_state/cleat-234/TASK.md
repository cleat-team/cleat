# cleat-234: CI Enforcement

**Budget:** $15 (~2 days)
**Priority:** 1 (prevents regressions)
**Status:** pending
**Depends on:** cleat-232, cleat-233

## Scope

CI must block regressions for the 0.5 release standard.

## Actions

1. Review `.github/workflows/ci.yml` — ensure it runs on all PRs and covers:
   - Go test (all packages)
   - Go vet
   - govulncheck
   - multi-db tests (Postgres, MySQL, MSSQL)
2. Review `.github/workflows/ecosystem-ci.yml` — ensure all SDKs are tested
3. Add code coverage check: CI fails if coverage drops below 75%
   - Start enforcement at 50%, ratchet to 75%
4. Review branch protection rules on `develop` and `main` — ensure required checks match CI
5. Fix the `internal/closure` test failures (2 tests):
   - `TestComputeBasicIdentifiesDurableLeaves`
   - `TestComputeBasicCorrectlyTagsPureFunctions`

## Key Files

- `.github/workflows/ci.yml`
- `.github/workflows/ecosystem-ci.yml`
- `internal/closure/` — failing tests

## Additional Scope (from surveys)

- CI has 12 `continue-on-error: true` across 4 workflow files — reduce where safe
- Coverage start at 50% threshold, ratchet to 75%
- Branch protection rules require GitHub admin access to verify
- test-tinygo re-enable: verify Go 1.26 compatibility first
