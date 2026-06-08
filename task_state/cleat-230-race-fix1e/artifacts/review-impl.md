# Implementation Review — cleat-230-race-fix1e

**Verdict: APPROVED** — no blockers, no SHOULD_FIX items.

## Changes Reviewed

| File | Change | Verdict |
|---|---|---|
| `engine/engine.go:1` | Added `"bytes"` import | Correct |
| `engine/engine.go:1344-1346` | Per-execution `execStdout`/`execStderr` `bytes.Buffer` in `executeComponent` | Correct |
| `engine/engine.go:1460-1462` | Replaced `InstantiateModuleNamed` with `instantiateModuleNamedWithWriters` using per-execution buffers | Correct |
| `engine/engine_component_race_test.go` | New concurrent race test for component-model path | Correct |

## Design Verification

The fix follows the same pattern as the already-reviewed `wazeroBackend.Execute()` fix from cleat-230-race-fix1:
- Per-execution `bytes.Buffer` instances (stack-allocated, discarded on return)
- Uses `instantiateModuleNamedWithWriters` (already exists in runtime.go)
- `Reset()` before each loop iteration keeps buffers bounded across multiple core module instantiations

The component race test constructively proves the fix: `Engine.Execute()` with a component binary routes through `isComponentWasm` → `executeComponent` → the instantiation loop at Step 3, which is the exact code path that was racy. 10 goroutines × 10 iterations = 100 concurrent executions, all passing cleanly with `-race`.

## NITs

**N1:** `Reset()` calls at lines 1460-1461 run before each loop iteration. The buffers are never read, so `Reset()` is not strictly necessary, but it prevents unbounded growth from modules that produce stdout/stderr output. Low cost, good hygiene.

## Test Results

| Test | Result |
|---|---|
| `TestComponentStdoutStderrRace -race -count=10` | PASS (0 races) |
| `TestRuntimeStdoutStderrRace -race -count=5` | PASS (0 races) |
| `TestRuntime\|TestWazero\|TestNewWazero -race -count=5` | PASS |
| `^Test[^EPCJAR] -race -count=3` | PASS |
| `go build ./engine/` | PASS |
