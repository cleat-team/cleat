# Implementation Notes — cleat-230-race-fix1e

## What changed

### `engine/engine.go`
- Added `"bytes"` import
- Added per-execution `execStdout`/`execStderr` `bytes.Buffer` in `executeComponent()` (line 1344)
- Replaced `e.rt.InstantiateModuleNamed()` with `e.rt.instantiateModuleNamedWithWriters(&execStdout, &execStderr)` at line 1460-1462

### `engine/engine_component_race_test.go` (new)
- `minimalComponentWasm()` — builds a valid Component Model binary inline with a single core module (empty "run" export) and instance export (sort=0x05)
- `TestComponentStdoutStderrRace` — 10 goroutines × 10 iterations of concurrent `Engine.Execute()` calls through the component path

## Why this pattern

The `executeComponent` function directly uses `e.rt` (the Engine's shared Runtime) rather than going through the `WasmBackend` abstraction. The `wazeroBackend.Execute()` path was already fixed in cleat-230-race-fix1, but `executeComponent` bypasses it entirely for component-model binaries.

The fix gives `executeComponent` its own per-call `bytes.Buffer` pair, matching the pattern from `wazeroBackend.Execute()`. These buffers are stack-allocated and discarded when the function returns, so there's zero shared mutable state across concurrent executions.

## Component export sort detail

The test component binary uses `sort=instance` (0x05) for the component export, not `sort=func` (0x01). The component parser (`wasm/component.go`) sets `InstanceIndex=-1` for func exports, which would cause `resolvedExports[-1]` to panic in `executeComponent` Step 5. Instance exports correctly set `InstanceIndex` to the actual instance index, allowing the entry point resolution to find the instantiated module.
