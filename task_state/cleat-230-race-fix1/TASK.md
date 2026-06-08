# cleat-230-race-fix1 — Fix WASM stdout/stderr buffer race

**Parent:** cleat-230-race (Race Condition Audit)
**Budget:** $5 (~0.25 engineer-day)
**Priority:** 2 (feature)
**Type:** Bug fix

## Task

Fix the unprotected `bytes.Buffer` race on the shared `Runtime` struct in the wazero WASM backend.

### Problem

`engine/backend_wazero.go:46` — `PerExecution()` returns `&wazeroBackend{rt: b.rt}`, sharing the same `*Runtime` across all concurrent workflow executions. The `Runtime` struct has two `bytes.Buffer` fields (`stdout`, `stderr`) that are concurrently:
- Reset in `InstantiateModuleNamed` (line 190-191)
- Written by wazero during `fn.Call()` via `WithStdout`/`WithStderr` (line 194-195)
- Read by `Stdout()`/`Stderr()` (line 60-63)

`bytes.Buffer` is explicitly documented as not goroutine-safe.

### Fix

Either:
- **Option A (preferred):** Make `PerExecution()` create independent `*bytes.Buffer` for stdout/stderr (like wasmtime backend does), or
- **Option B:** Add a `sync.Mutex` to protect stdout/stderr buffer access

### Acceptance criteria

1. No data race under parallel WASM execution
2. `go test -race ./engine/ -run TestRuntimeStdoutStderrRace -count=10` passes
3. Existing WASM tests pass (no regression)

### Out of scope

- wasmtime backend (already has per-execution isolation)
- Plugin WASM modules (these use their own Runtime instances)
