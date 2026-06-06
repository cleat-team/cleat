# cleat-233a Status: AssemblyScript Test Infrastructure Fix

**Explored by:** cleat-233ae
**Date:** 2026-06-05
**Status:** fix ready

## Summary

Root cause: missing `await` in `as-pect.config.mjs` line 116. The `@assemblyscript/loader`'s `instantiate()` is async but wasn't awaited — `result` was a Promise, `result.exports.memory` threw TypeError. Fixed with a single `await`.

All 106 tests pass (smoke: 16, json-host: 19, json-saga: 71).

## Changes

- `packages/cleat-as/as-pect.config.mjs`: Added `await` before `instantiate(binary, createImports(myImports))`
