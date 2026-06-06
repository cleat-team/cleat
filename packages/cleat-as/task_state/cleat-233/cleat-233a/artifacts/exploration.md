# cleat-233a Exploration Report

**Date:** 2026-06-05
**Explorer:** cleat-233ae
**Status:** Fix identified and applied — all 106 tests pass

## Root Cause

The AssemblyScript test infrastructure failure was NOT a binaryen ESM incompatibility as initially diagnosed. The real bug is a **missing `await`** in `packages/cleat-as/as-pect.config.mjs` line 116.

### What was happening

1. `as-pect.config.mjs` defines a custom `instantiate` function for WASM test module loading
2. It calls `@assemblyscript/loader`'s `instantiate(binary, imports)` which is **async** and returns `Promise<InstantiateResult>`
3. The code assigned `const result = instantiate(...)` — `result` was a pending Promise, not the `{ module, instance, exports }` object
4. `result.exports.memory` threw `TypeError: Cannot read properties of undefined (reading 'memory')`
5. This unhandled error crashed the Node.js child process spawned by the `asp` CLI wrapper, producing exit code 7
6. The binaryen source dump on stderr was Node.js's standard error output format for the module where the error originated

### The fix

```diff
-    const result = instantiate(binary, createImports(myImports));
+    const result = await instantiate(binary, createImports(myImports));
```

One word: `await`. No dependency changes, no version pins, no module format changes needed.

### Why binaryen wasn't the problem

- binaryen@112, @116, and @110 all load fine as ESM when imported via Node 18's ESM loader
- `assemblyscript@0.27.32` imports binaryen via `import * from "binaryen"` (static ESM import) — works correctly
- The as-pect CLI wrapper (`bin/asp.js`) spawns a child `node` process that loads everything as ESM — the module resolution chain works
- The `as-pect.asconfig.json` `"transform": ["@as-pect/transform"]` loads fine via dynamic import
- All compilation steps succeed — the crash is during WASM **instantiation**, not compilation

## What changed

### `packages/cleat-as/as-pect.config.mjs` (1-line fix)

Line 116: added `await` before `instantiate(binary, createImports(myImports))`.

### `packages/cleat-as/package.json` (no change)

The `devDependencies.binaryen` was temporarily changed during investigation but the original `112.0.0` version works fine with the `await` fix. No change needed.

## Test Results

```
✔ assembly/__tests__/json-host.spec.ts  Pass: 19 / 19
✔ assembly/__tests__/json-saga.spec.ts  Pass: 71 / 71
✔ assembly/__tests__/smoke.spec.ts      Pass: 16 / 16

[Result]: ✔ Pass!  (106 / 106)
```

## What was investigated but ruled out

1. **Binaryen version pinning**: Not needed. Binaryen loads correctly in ESM context.
2. **`"type": "module"` in package.json**: Not needed. All relevant packages already use ESM.
3. **Replacing as-pect with a different test runner**: Not needed. as-pect works correctly.
4. **`createImports` merge logic**: The original `createImports(myImports)` correctly uses `Object.assign` to merge custom env stubs. No change needed.
5. **Node.js version**: Node 18.19.1 works fine with all ESM features used (top-level await, import.meta.url, dynamic import).

## Next steps

The fix is ready. This unblocks:
- cleat-233e (documentation update — needs cleat-233a completed)
- cleat-234 (CI enforcement — depends on cleat-233)
