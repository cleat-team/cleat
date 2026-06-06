# cleat-233a Contract

## Deliverables

- `packages/cleat-as/as-pect.config.mjs` — 1-line fix: `await` on `instantiate()` call
- All 106 AssemblyScript tests pass (`npm test` exits 0)

## Invariants

- No dependency changes required (binaryen, assemblyscript, as-pect versions unchanged)
- No changes to test files, source code, or compilation config
- Go baseline tests remain unaffected

## Success Criteria

- `npm test` in `packages/cleat-as/` runs all 3 spec files without error
- All 106 test assertions pass: smoke.spec.ts (16), json-host.spec.ts (19), json-saga.spec.ts (71)

## Coupling

- LOOSE with cleat-233b (Python WASM E2E) — parallel, no shared files
- LOOSE with cleat-233c (Rust WASM integration) — parallel, no shared files
- LOOSE with cleat-233d (ABI.md fix) — parallel, no shared files
