# cleat-233a Status

**Phase:** explored (fix applied, verified)
**Last updated:** 2026-06-05
**Explored by:** cleat-233ap

## Summary

AssemblyScript test infrastructure is fixed. All 106 tests pass (json-host: 19/19, json-saga: 71/71, smoke: 16/16).

## Root Cause

Two issues, one already resolved:

1. **Binaryen ESM** (PRE-EXISTING, ALREADY RESOLVED): Previously reported `ERR_REQUIRE_ASYNC_MODULE` when as-pect loaded binaryen via `require()`. This no longer occurs — tests run successfully. Likely resolved by Node version or dependency updates.

2. **Memory too small** (ROOT CAUSE OF 19 FAILURES): WASM module starts with 1 page (64KB) but `HostCalls` writes to `SCRATCH_BASE` at 10 MiB (160 pages). Memory access traps caused all 19 JSON host tests to fail silently.

## Fixes Applied

| File | Change | Reason |
|------|--------|--------|
| `packages/cleat-as/as-pect.asconfig.json` | Added `"initialMemory": 200` | Module needs 160+ pages for scratch buffer |
| `packages/cleat-as/as-pect.config.mjs` | Removed debug `console.error`; added `await` on `instantiate()` | Cleanup + correct async handling |

## Next Steps

- **Remove `continue-on-error: true`** from `.github/workflows/ci.yml` lines 319 and 369 (AS test jobs)
- Tests are **leaf-ready** — proceed to verification or planning
