# CONTRACT — cleat-230-race-fix1

## Deliverables

1. Protected stdout/stderr access on `Runtime` struct — per-execution `*bytes.Buffer` instances
2. `go test -race` regression test that fails before fix and passes after
3. No API changes to Runtime struct public surface

## Invariants

- `Runtime.Stdout()` / `Runtime.Stderr()` remain safe to call from any goroutine
- Per-execution buffers are independent — concurrent WASM executions do not share buffer state
- Wazero execution path still captures stdout/stderr correctly

## API Surface

No changes to public API. `Runtime.Stdout()` and `Runtime.Stderr()` continue to work as before. Internal change: `InstantiateModuleNamed` uses per-execution buffers instead of shared buffers.

## Integration Points

- `engine/backend_wazero.go` — `PerExecution()` (line 46-48): allocate independent buffers
- `engine/runtime.go` — `InstantiateModuleNamed` (line 189-198): use per-execution buffers

## Test Requirements

- New test: `TestRuntimeStdoutStderrRace` in `engine/runtime_test.go` (or appropriate test file)
  - Spawns N goroutines each calling InstantiateModuleNamed + reading Stdout/Stderr
  - Run with `-race` flag to detect data races
  - Must pass with `-count=10` for statistical confidence
- All existing tests must pass (`go test ./engine/...`)

## Coupling

- LOOSE with `cleat-230-race-fix4` (both touch engine/runtime.go but fix4 only removes dead code fields)
- NONE with `cleat-230-race-fix2`, `cleat-230-race-fix3`
