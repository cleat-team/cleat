# cleat-233a Exploration Report

**Date:** 2026-06-05
**Explorer:** cleat-233ae
**Task:** Fix AssemblyScript test infrastructure (binaryen ESM)

## 1. What's here now?

### Files
- `packages/cleat-as/package.json` — SDK package manifest
- `packages/cleat-as/as-pect.config.mjs` — as-pect test runner config with custom `instantiate` hook providing JS stubs for `cleat_json_parse` and `cleat_json_stringify`
- `packages/cleat-as/as-pect.asconfig.json` — AS compiler config, enables `@as-pect/transform`
- `packages/cleat-as/assembly/__tests__/` — 3 spec files:
  - `smoke.spec.ts` (16 tests) — constants, encode/decode, escapeJson, Memory class
  - `json-saga.spec.ts` (71 tests) — JsonParser, JsonBuilder, Saga, StringBuilder, jsonEscape
  - `json-host.spec.ts` (19 tests) — JSON.parse, JSON.stringify, roundtrip (uses cleat_json_parse/stringify host imports)

### Dependency chain
- `@as-pect/cli@8.1.0` → requires `assemblyscript: ^0.27.2` (peer dep)
- `@as-pect/transform@8.1.0` → requires `assemblyscript: ^0.27.2`
- `@as-pect/core@8.1.0` → requires `assemblyscript: ^0.27.2`
- All as-pect packages have `"type": "module"`
- `assemblyscript@0.27.32` has `"type": "module"` — asc.js uses ESM `import` for binaryen
- All binaryen versions (110, 111, 112, 116) have `"type": "module"` AND top-level `await`

### Node.js environment
- Node v18.19.1
- ESM/CJS interop: `createRequire` can't load ESM modules with top-level `await` → `ERR_REQUIRE_ASYNC_MODULE`

## 2. Root causes (two bugs)

### Bug 1: AssemblyScript version mismatch (binaryen crash)

The `package.json` devDeps had `assemblyscript: "0.26.7"` and `binaryen: "112.0.0"`, incompatible with `@as-pect/cli@8.1.0` which requires `assemblyscript: ^0.27.2`. npm hoisted `assemblyscript@0.26.7` to the top level, overriding as-pect's needed `0.27.32`. The older asc.js triggered `ERR_REQUIRE_ASYNC_MODULE` when loading binaryen.

**Fix:** Align `assemblyscript` devDep to `0.27.32`, remove explicit `binaryen` pin (it's a transitive dep of assemblyscript).

### Bug 2: Missing `await` in `as-pect.config.mjs` (JSON host stubs silently failing)

`@assemblyscript/loader`'s `instantiate()` is **async** (returns `Promise<InstantiateResult>`). It was called without `await`:

```js
// BEFORE (broken):
const result = instantiate(binary, createImports(myImports));
wasmMemory = result.exports.memory;  // Promise.exports = undefined → wasmMemory = null
```

```js
// AFTER (fixed):
const result = await instantiate(binary, createImports(myImports));
wasmMemory = result.exports.memory;  // actual { module, instance, exports }
```

Without `await`, `result` was a Promise — `result.exports.memory` was `undefined`, so `wasmMemory` was `null`. The JSON stub functions check `if (!wasmMemory) return <empty>` and returned zero/empty results, causing all 19 `json-host.spec.ts` tests to fail silently (errCode=0, bytesWritten=0 decoded as empty string, not null — assertions failed on wrong value).

The pure-AS tests (smoke, json-saga) were unaffected because they don't call host imports.

## 3. Fixes applied

### package.json changes
```diff
- "assemblyscript": "0.26.7",
- "binaryen": "112.0.0"
+ "assemblyscript": "0.27.32"
```

### as-pect.config.mjs changes
```diff
- const result = instantiate(binary, createImports(myImports));
+ const result = await instantiate(binary, createImports(myImports));
```

## 4. Test results (verified 2026-06-05)

| Spec file | Tests | Result |
|-----------|-------|--------|
| `smoke.spec.ts` | 16 | 16/16 PASS |
| `json-saga.spec.ts` | 71 | 71/71 PASS |
| `json-host.spec.ts` | 19 | 19/19 PASS |
| **Total** | **106** | **106/106 PASS** |

## 5. Risks

1. **binaryen ESM fragility**: All versions of binaryen are ESM with top-level `await`. If any tool in the chain switches to `require('binaryen')`, tests break. Mitigation: as-pect's `asp` CLI uses `asc` as a binary (not via require), so ESM compatibility is handled by the Node.js binary.
2. **Node.js version sensitivity**: The `ERR_REQUIRE_ESM` behavior varies by Node.js version. Tests should be verified on the CI Node.js version (v18 likely).
3. **Hardcoded memory offsets**: SCRATCH_BASE at 10 MiB exceeds the initial memory page allocation, but AS stub runtime grows memory on demand. Not an issue in practice.

## 6. Budget

Time spent: ~2h. Within the 1-3h estimate. Both bugs are fixed. All 106 tests pass.

## 7. Recommendation

Task is complete. The two fixes (assemblyscript version alignment + `await` keyword) resolve all test failures. CI should be configured to run `npm test` in `packages/cleat-as/` without `continue-on-error`.
