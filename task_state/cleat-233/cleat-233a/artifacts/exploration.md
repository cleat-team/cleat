# cleat-233a Exploration Report

**Date:** 2026-06-05
**Explorer:** cleat-233ap
**Task:** Fix AssemblyScript test infrastructure

## Key Finding

The binaryen ESM issue previously reported is **no longer blocking**. Tests ran successfully on first attempt. The actual problem was a **memory size mismatch**: the WASM module starts with 1 page (64KB) but `HostCalls` writes to `SCRATCH_BASE` at 10 MiB (160 pages), causing "memory access out of bounds" traps.

## Root Cause: Memory Size Too Small

- `assembly/memory.ts` defines `SCRATCH_BASE = 10485760` (10 MiB, 160 pages)
- `assembly/memory.ts` defines `OUTPUT_OFFSET = 10551296` (10 MiB + 64 KiB, 161 pages)
- AS compiler default `initialMemory: 0` → 1 page (64KB)
- When WASM code writes to offset 10,485,760, it traps with "memory access out of bounds"
- All 19 JSON host tests failed silently (no error message from the trap)

## Fix Applied

Added `"initialMemory": 200` to `as-pect.asconfig.json` compiler options. This tells the AssemblyScript compiler to start the module with 200 pages (12.8 MiB), enough for the scratch buffer region.

### Before
```
as-pect.asconfig.json options:
  runtime: stub
  exportRuntime: true
  exportStart: _start
  outFile: output.wasm
  optimize: false
  // initialMemory: not set → defaults to 0 (1 page)
```

### After
```
as-pect.asconfig.json options:
  runtime: stub
  initialMemory: 200  ← NEW
  exportRuntime: true
  exportStart: _start
  outFile: output.wasm
  optimize: false
```

## Test Results After Fix

```
✔ assembly/__tests__/json-host.spec.ts   Pass: 19 / 19
✔ assembly/__tests__/json-saga.spec.ts   Pass: 71 / 71
✔ assembly/__tests__/smoke.spec.ts       Pass: 16 / 16
Summary: 106 / 106 passed, Result: ✔ Pass!
```

## Cleanup

Removed debug `console.error` statement from `as-pect.config.mjs` line 76 (leftover from prior debugging session).

## Additional Observations

### Binaryen ESM — not currently blocking
- Both binaryen@110 and binaryen@116 have `"type": "module"` in their package.json
- Tests run fine via `npm test` → `asp` → node child process
- The ESM issue may have been specific to certain Node versions or resolved by dependency updates
- If the issue recurs, the fallback is using `import()` instead of `require()` in the as-pect pipeline

### Missing host import stubs — known limitation
- `as-pect.config.mjs` provides stubs for only `cleat_json_parse` and `cleat_json_stringify`
- The other 50 `@external("env", ...)` imports have no JS stubs
- This is fine for the current 3 test files (they only call JSON host functions)
- New tests calling other host functions would need additional stubs
- The Go test runner (`test_runner/test_runner.go`) provides full host imports via wazero

### CI configuration
- `.github/workflows/ci.yml` lines 319, 369: `continue-on-error: true` on both AS test jobs
- These were set when tests couldn't run due to binaryen issues
- Should be removed now that tests pass

## Files Changed

| File | Change |
|------|--------|
| `packages/cleat-as/as-pect.asconfig.json` | Added `"initialMemory": 200` |
| `packages/cleat-as/as-pect.config.mjs` | Removed debug `console.error` |

## Recommendation

Task is **leaf-ready**. Two changes already applied:
1. Memory fix (1 line in as-pect.asconfig.json)
2. Debug cleanup (1 line removal in as-pect.config.mjs)

Optional follow-up (defer to parent task cleat-233):
- Remove `continue-on-error: true` from CI (`.github/workflows/ci.yml` lines 319, 369)
- Could reduce `initialMemory` from 200 to ~165 pages for efficiency, but 200 is safe
