# cleat-230-race-fix1k — Extend stdout/stderr race fix to executeComponent path

**Parent:** cleat-230-race-fix1 (Fix WASM stdout/stderr buffer race)
**Budget:** $2 (~0.1 engineer-day)
**Priority:** 2 (feature)
**Type:** Bug fix

## Task

Extend the WASM stdout/stderr buffer race fix from `wazeroBackend.Execute()` to the `Engine.executeComponent()` path for WASM Component Model binaries.

### Problem

`engine/engine.go:1453` — `executeComponent()` calls `e.rt.InstantiateModuleNamed()` directly, which writes to the shared `Runtime.stdout`/`stderr` buffers. When two component-model workflows execute concurrently on the same Runtime, this races.

The `wazeroBackend.Execute()` path was already fixed in cleat-230-race-fix1 (uses per-backend buffers via `instantiateModuleNamedWithWriters`), but `executeComponent()` bypasses the backend abstraction entirely and hits the shared Runtime buffers directly.

### Fix

In `Engine.executeComponent()` (engine.go:1453), replace the `e.rt.InstantiateModuleNamed()` call with `e.rt.instantiateModuleNamedWithWriters()` using per-execution `bytes.Buffer` instances.

### Acceptance criteria

1. `go test -race -run TestComponentStdoutStderrRace -count=10 ./engine/` passes
2. Existing tests pass (no regression)

### Out of scope

- Legacy `executeCompiled` path (also uses shared buffers, but is exercised by the wazeroBackend path in production)
- `executeInternalDefer` path (called sequentially after execution, no concurrent risk)
- Plugin WASM modules (use their own Runtime instances)
