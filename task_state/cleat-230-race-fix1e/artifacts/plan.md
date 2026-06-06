# Plan — cleat-230-race-fix1e

## Summary

Extend the WASM stdout/stderr buffer race fix to the `Engine.executeComponent()` path by giving it per-execution `bytes.Buffer` instances, matching the pattern already used in `wazeroBackend.Execute()`.

## Changes

### 1. `engine/engine.go` — `executeComponent()` (line 1338)

**Change**: Add per-execution stdout/stderr buffers and use `instantiateModuleNamedWithWriters`.

At line 1342 (after `const componentAdapterModule`), add:
```go
var execStdout, execStderr bytes.Buffer
```

At line 1453, replace:
```go
mod, err := e.rt.InstantiateModuleNamed(instantiateCtx, cm, fmt.Sprintf("__core_%d__", i))
```
with:
```go
execStdout.Reset()
execStderr.Reset()
mod, err := e.rt.instantiateModuleNamedWithWriters(instantiateCtx, cm, fmt.Sprintf("__core_%d__", i), &execStdout, &execStderr)
```

The `Reset()` before each instantiation keeps buffers from growing unboundedly across the loop iterations. The loop is sequential (single goroutine per `executeComponent` call), so no intra-call race.

### 2. `engine/engine_component_race_test.go` (new) — race regression test

**Test**: `TestComponentStdoutStderrRace` — concurrent `Engine.Execute()` calls with a minimal valid component binary, verified race-free with `-race -count=10`.

The test builds a component binary inline (using the same LEB128 helpers from the `wasm` package pattern). The binary has:
- A valid component header
- A core WASM module with an empty "run" export (empty function body)
- A component instance section (Instantiate module 0 with no args)
- A component export of sort=instance pointing to instance 0

The component export must use sort=instance (0x05), not sort=func (0x01), because the component parser sets `InstanceIndex = -1` for func exports, which would cause a slice bounds panic in `executeComponent` Step 5.

`Engine.Execute()` with no custom backends detects the component header and routes to `executeComponent()`, which instantiates the core module via `InstantiateModuleNamed` (line 1453) — exactly the code path being fixed. The `Execute()` call returns an error (the empty function body produces no results from `CallExport`), but that's expected — the race condition is in Step 3 (instantiation), which executes before the error.

```go
func TestComponentStdoutStderrRace(t *testing.T) {
    ctx := context.Background()
    rt, err := NewRuntime(ctx, 0, 0)
    if err != nil {
        t.Fatalf("NewRuntime: %v", err)
    }
    defer rt.Close(ctx)

    eng := NewEngine(rt, nil)
    wasmBytes := minimalComponentWasm()

    const goroutines = 10
    const iterations = 10
    var wg sync.WaitGroup
    wg.Add(goroutines)

    for g := 0; g < goroutines; g++ {
        go func() {
            defer wg.Done()
            for i := 0; i < iterations; i++ {
                _, _, _, _, _, err := eng.Execute(ctx, wasmBytes, "run", nil)
                _ = err // expected: component path instantiates successfully but
                        // CallExport returns error (empty function body yields void)
            }
        }()
    }
    wg.Wait()
}
```

The `minimalComponentWasm()` helper builds the binary inline in the test file — no cross-package dependency needed.

## No other files change

- `engine/runtime.go` — no changes (`instantiateModuleNamedWithWriters` already exists)
- `engine/backend_wazero.go` — no changes (already fixed in fix1)
- `wasm/` — no changes (component binary built inline in test)

## Risk assessment

- **Low risk**: Drop-in replacement — same wazero API, same module config pattern, just directing stdout/stderr to local buffers
- **No API change**: `instantiateModuleNamedWithWriters` is already tested by `wazeroBackend.Execute()`
- **Test safety**: The race test uses a minimal valid component binary (not a real workflow), no side effects
