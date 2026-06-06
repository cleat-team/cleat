# cleat-233a Status

**Phase:** completed
**Last updated:** 2026-06-05
**Explored by:** cleat-233ae
**Verified by:** cleat-233ak (2026-06-05), cleat-233am (2026-06-05, re-verification), cleat-233av (2026-06-06)

## Summary

Two bugs in AssemblyScript test infrastructure fixed:

1. **AssemblyScript version mismatch** — `devDependencies` had `assemblyscript: "0.26.7"` but `@as-pect/cli@8.1.0` requires `^0.27.2`. npm hoisted the wrong version, causing `ERR_REQUIRE_ASYNC_MODULE` when loading binaryen. Fixed by aligning to `0.27.32`.

2. **Missing `await` in `as-pect.config.mjs`** — `@assemblyscript/loader.instantiate()` is async. Called without `await`, the result was a Promise, so `wasmMemory = result.exports.memory` was `undefined`. JSON host import stubs returned empty results, causing 19 silent test failures. Fixed with one keyword: `await`.

## Test Results (verified)

| Spec | Tests | Result |
|------|-------|--------|
| smoke.spec.ts | 16 | PASS |
| json-saga.spec.ts | 71 | PASS |
| json-host.spec.ts | 19 | PASS |
| **Total** | **106** | **PASS** |

## Success Criteria (from PLAN)

- [x] `npm test` in `packages/cleat-as/` runs all 3 spec files without error
- [x] All 106 test assertions pass
- [x] No package.json dependency conflicts (assemblyscript aligned with as-pect peer dep)

## Changes

- `packages/cleat-as/package.json` — assemblyscript 0.26.7 → 0.27.32, removed explicit binaryen pin
- `packages/cleat-as/as-pect.config.mjs` — added `await` before `instantiate()`
