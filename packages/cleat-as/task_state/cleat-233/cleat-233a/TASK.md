# cleat-233a: Fix AssemblyScript Test Infrastructure

**Budget:** $3 (1-3h)
**Priority:** P0 (release blocker)
**Status:** fix ready
**Depends on:** none

## Scope

Fix the AssemblyScript test infrastructure so all tests in `packages/cleat-as/` run and pass.

## Actions

1. ~~Investigate binaryen ESM incompatibility~~ → Root cause was different: missing `await` in instantiate function
2. Apply 1-line fix in `as-pect.config.mjs`: `await` the async `instantiate()` call
3. Verify all 106 tests pass: `npm test`

## Key Files

- `packages/cleat-as/as-pect.config.mjs`
- `packages/cleat-as/assembly/__tests__/`
