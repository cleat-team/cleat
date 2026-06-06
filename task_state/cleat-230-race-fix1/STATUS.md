# STATUS — cleat-230-race-fix1

**Phase:** complete
**Created:** 2026-06-06T09:00:00Z
**Completed:** 2026-06-06T09:45:00Z
**Verified:** 2026-06-06 (cleat-230-race-fix1p)
**Budget:** $5
**Spent:** $2

## Summary

Fixed the WASM stdout/stderr buffer race in the wazero backend by making PerExecution() create independent `*bytes.Buffer` instances per backend, instead of sharing the Runtime's buffers.

## Changes

1. **engine/backend_wazero.go**: Added `stdout`/`stderr` `bytes.Buffer` fields to `wazeroBackend`. `Execute()` now uses per-backend buffers via the new `instantiateModuleNamedWithWriters` helper.

2. **engine/runtime.go**: Added private `instantiateModuleNamedWithWriters` method that accepts custom stdout/stderr writers instead of using the shared Runtime buffers.

3. **engine/backend_wazero_race_test.go** (new): `TestRuntimeStdoutStderrRace` — concurrent `PerExecution().Execute()` calls, verified race-free with `-race -count=10`.

### Collateral fixes (pre-existing build breaks)
- engine/host_test.go: Removed non-existent `signals` field from `execSession` literal
- engine/mysql_store.go: Removed duplicate `isDeadlockError` (consolidated in compaction.go)

## Verification

- `go test -race -run TestRuntimeStdoutStderrRace -count=10 ./engine/` — PASS (no races)
- `go test -race -run 'TestRuntime|TestWazero|TestNewWazero' ./engine/ -count=5` — PASS (5 iterations, no regression)
- `go test -race -run '^Test[^EPCJAR]' ./engine/ -count=3` — PASS (all unit tests)
