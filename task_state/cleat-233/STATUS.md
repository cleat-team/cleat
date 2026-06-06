# cleat-233 Status

**Phase:** decomposed (ready for child tasks)
**Last updated:** 2026-06-05 (cleat-233ae exploration complete)
**Explored by:** cleat-233e
**Verified by:** cleat-233i
**Planned by:** cleat-233p

## Summary

Surveyed all three language SDKs. Rust (32 tests) and Python (443 tests) pass cleanly. AssemblyScript tests are blocked by a binaryen ESM incompatibility.

## Decomposition (cleat-233p PLAN.md)

5 child tasks defined, ordered by phase:

**Phase 1 (parallel):**
- **cleat-233a** (P0, 1-3h): Fix AssemblyScript test infrastructure (binaryen ESM) — **IN PROGRESS (cleat-233ae)**
  - Binaryen crash FIXED: updated assemblyscript from 0.26.7 to 0.27.32, removed explicit binaryen pin
  - smoke.spec.ts: 16/16 PASS, json-saga.spec.ts: 71/71 PASS
  - json-host.spec.ts: 0/19 FAIL — JSON host import stubs not invoked (pre-existing bug, was masked by the binaryen crash)
- **cleat-233b** (P1, ≤4h timebox): Python WASM E2E validation
- **cleat-233c** (P2, 1-3h): Rust SDK WASM integration test
- **cleat-233d** (P1, 15m): Fix ABI.md omissions (cleat_poll_child, cleat_await_any_child)

**Phase 2:**
- **cleat-233e** (P2, 20m): Update LANGUAGE_SUPPORT.md, DX_COMPARISON.md

Total estimate: 3-11h, within $15 budget.

## Key findings

- **Rust SDK**: 32 unit tests pass, 5 proc-macro tests pass. Mock-based (no WASM integration tests exist). ABI imports match ABI.md v5.
- **AssemblyScript SDK**: Tests FAIL to run — binaryen uses top-level `await` in ESM, as-pect loads via `require()`. `ERR_REQUIRE_ASYNC_MODULE`.
- **Python SDK**: 443 tests pass natively. WASM compilation pipeline exists but E2E execution never validated. 5 missing imports in pythonExpectedImports. 4h timebox recommended.
- **Commit 1b7f8ed**: Go-only DispatchWrapper fix. No SDK impact.

## Additional findings (cleat-233i)

- CI path bug in `.github/workflows/e2e-cross-language.yml` line 101: `./internal/host/...` should be `./engine/...`
- ABI.md missing `cleat_poll_child` and `cleat_await_any_child` (present in Rust SDK host_calls.rs)
- DX_COMPARISON.md has double "end-to-end" typo (lines 23, 149-150) and internal contradiction about Python WASM completeness (line 340 vs 150)

## Recommendation

Proceed with child task creation. Start with cleat-233a (P0 AS fix) and cleat-233d (quick ABI.md fix) in parallel.
