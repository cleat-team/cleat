# cleat-233: SDK Test Passes

**Budget:** $15 (~2 days)
**Priority:** 1 (release blocker)
**Status:** pending
**Depends on:** none

## Scope

Language SDKs need to pass their tests for the 0.5 release:
- **Rust SDK**: wasmtime-based, `wasm/testdata/` — ensure test suite passes
- **AssemblyScript SDK**: WASM component model — ensure cross-language E2E tests pass
- **Python SDK**: PyPI publish workflow exists — ensure tests pass

## Actions

1. Run each SDK's test suite, triage failures
2. Fix any ABI mismatches or host call signature changes
3. Update LANGUAGE_SUPPORT.md with current status of each SDK
4. Timebox Python WASM E2E to 4 hours; escalate if insoluble

## Key Files

- `wasm/` — Rust SDK and WASM test infrastructure
- `python-sdk/` — Python SDK
- `packages/` — AssemblyScript SDK
- `crates/` — Rust SDK crates
- `LANGUAGE_SUPPORT.md`

## Additional Scope (from surveys)

- Review commit `1b7f8ed` error handling (WASM input dispatch fix)
- Python WASM E2E viability never validated — timebox and escalate
