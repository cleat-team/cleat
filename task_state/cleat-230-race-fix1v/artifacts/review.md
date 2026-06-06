# Review — cleat-230-race-fix1v

**Reviewer:** cleat-230-race-fix1v
**Date:** 2026-06-06
**Verdict:** APPROVED (no blockers)

---

## Independent Verification

- `go test -race -run TestRuntimeStdoutStderrRace -count=5 ./engine/` — PASS (no races)
- `go test -race -run 'TestRuntime|TestWazero|TestNewWazero' ./engine/ -count=3` — PASS (no regressions)
- Code review of all changed files — all changes correct and consistent

## Changes Reviewed

| File | Change | Verdict |
|---|---|---|
| `engine/backend_wazero.go` | Added `stdout`/`stderr` per-backend buffers; `Execute()` uses `instantiateModuleNamedWithWriters` | Correct |
| `engine/runtime.go` | Added goroutine-safety comment on shared `stdout`/`stderr`; removed 5 dead shared-mutable-state fields; added `instantiateModuleNamedWithWriters` helper | Correct |
| `engine/backend_wazero_race_test.go` | New concurrent race test (10 goroutines x 20 iterations) | Correct |
| `engine/host_test.go` | Removed non-existent `signals` field from `execSession` literal | Correct |
| `engine/mysql_store.go` | Removed unused `isDeadlockError` (no callers in file) | Correct |

## Dead Field Removal Verification

These fields were removed from `Runtime`:

- `workEntryPoint` / `workInput` — were set in `CallExportWithSuspend` (also removed) and consumed by `cleat_poll_work`, which already returned 0 unconditionally (prior commit). Safe removal.
- `completeMu` / `completeResult` / `completeErr` — were used by the old `cleat_complete` host handler. `cleat_complete` now uses context-based storage via `cleatComplete` struct (prior commit). Safe removal.

Zero matches for any of these fields remain in `runtime.go`.

---

## Findings

### SHOULD_FIX

**SF1: `InstantiateModuleNamed` still writes to shared Runtime buffers — latent race in legacy execution paths**

`Runtime.InstantiateModuleNamed` (runtime.go:177-186) resets and writes to the shared `r.stdout`/`r.stderr` buffers. Two execution paths still use this directly:

1. `executeCompiled` (engine.go:1168) — calls `e.rt.InstantiateModule()` which delegates to `InstantiateModuleNamed`
2. `executeComponent` (engine.go:1453) — calls `e.rt.InstantiateModuleNamed()` directly for each core module in a component-model bundle

The `wazeroBackend.Execute()` path is correctly fixed via `instantiateModuleNamedWithWriters` with per-backend buffers. But if a worker handles both legacy and backend paths concurrently, or if `executeComponent` instantiates multiple core modules in parallel (future optimization), the shared buffers still race.

The Runtime struct comment at lines 37-40 already documents this limitation:
```
// stdout/stderr are NOT goroutine-safe — they are shared across callers
// of InstantiateModuleNamed. Concurrent execution must use the
// wazeroBackend.Execute() path, which uses per-backend buffers.
```

**Recommendation:** Either make `executeComponent` and `executeCompiled` use `instantiateModuleNamedWithWriters` with per-execution buffers, or convert `InstantiateModuleNamed` to always use internally-allocated buffers (deprecating the shared fields). This is out of scope for the current task but should be tracked. The comment mitigates the risk by making the constraint explicit.

---

### NIT

**N1: `InstantiateModuleNamed` still exposes shared-buffer semantics as a public API**

`Runtime.InstantiateModuleNamed` is a public method that mutates shared state (`r.stdout.Reset()`, then writes via `WithStdout(&r.stdout)`). Any external caller (e.g., plugin instantiation in `internal/plugin/`) that calls this concurrently on the same `Runtime` could still race. The comment warns about this, but a future-refactor that makes `Stdout()`/`Stderr()` return the shared buffer contents is a footgun in waiting.

**N2: `InstantiateModuleNamed` calls `r.stdout.Reset()` but `instantiateModuleNamedWithWriters` does not reset its caller-provided buffers**

In `wazeroBackend.Execute()` (backend_wazero.go:79-80), the caller explicitly calls `b.stdout.Reset()` and `b.stderr.Reset()` before calling `instantiateModuleNamedWithWriters`. This is correct — the helper shouldn't reset caller-owned buffers — but the asymmetry with `InstantiateModuleNamed` (which *does* reset internally) could be confusing. A comment on `instantiateModuleNamedWithWriters` noting that callers are responsible for Reset would help.

---

## Verified Claims

| Claim | Result |
|---|---|
| Race test passes with `-race -count=5` | PASS (independently confirmed) |
| `signals` field removed from `execSession` struct | Confirmed: not present in engine.go:1662-1701 |
| `signals` field removed from test literal | Confirmed: not present in host_test.go |
| `isDeadlockError` removed from mysql_store.go | Confirmed: zero matches in file |
| `isDeadlockError` removed from entire engine/ | Confirmed: zero matches (only `isCompactionDeadlockError` remains in compaction.go) |
| `workEntryPoint`, `workInput` removed from Runtime | Confirmed: zero matches in runtime.go |
| `completeMu`, `completeResult`, `completeErr` removed from Runtime | Confirmed: zero matches in runtime.go |
| `cleatComplete` uses context-based storage | Confirmed: reads from `ctx.Value(&cleatCompleteKey)` |
| `cleat_poll_work` returns 0 (no-op) | Confirmed: handler returns 0 unconditionally |
| `wazeroBackend.Execute` uses per-backend buffers | Confirmed: calls `instantiateModuleNamedWithWriters` with `&b.stdout`, `&b.stderr` |
| `PerExecution()` creates independent backend | Confirmed: `&wazeroBackend{rt: b.rt}` with zero-value `stdout`/`stderr` |
| All WASM-related tests pass with `-race` | PASS (independently confirmed) |
