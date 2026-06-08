# Review — cleat-230-race-fix1v

**Reviewer:** cleat-230-race-fix1v
**Date:** 2026-06-06
**Verdict:** APPROVED (no blockers)

---

## Independent Verification

- `go test -race -run TestRuntimeStdoutStderrRace -count=5 ./engine/` — PASS (no races)
- Code review of changed files — all changes correct

## Changes Reviewed

| File | Change | Verdict |
|---|---|---|
| `engine/backend_wazero.go` | Added `stdout`/`stderr` per-backend buffers; `Execute()` uses `instantiateModuleNamedWithWriters` | Correct |
| `engine/runtime.go` | Added goroutine-safety comment on shared `stdout`/`stderr`; removed 5 dead shared-mutable-state fields; added `instantiateModuleNamedWithWriters` helper | Correct |
| `engine/backend_wazero_race_test.go` | New concurrent race test (10 goroutines × 20 iterations) | Correct |
| `engine/host_test.go` | Removed non-existent `signals` field from `execSession` literal | Correct — field was already removed from struct in prior commit |
| `engine/mysql_store.go` | Removed unused `isDeadlockError` (no callers in file) | Correct — `compaction.go` has its own `isCompactionDeadlockError` |

## Dead Field Removal Verification

These fields were removed from `Runtime`:

- `workEntryPoint` / `workInput` — were set in `CallExportWithSuspend` (also removed) and consumed by `cleat_poll_work`, which already returned 0 unconditionally (prior commit). Safe removal.
- `completeMu` / `completeResult` / `completeErr` — were used by the old `cleat_complete` host handler. `cleat_complete` now uses context-based storage via `cleatComplete` struct (prior commit). Safe removal.

Zero matches for any of these fields remain in `runtime.go`.

---

## SHOULD_FIX

**SF1: `InstantiateModuleNamed` still writes to shared Runtime buffers — potential race in component-model path**

`Runtime.InstantiateModuleNamed` (runtime.go:177-186) resets and writes to the shared `r.stdout`/`r.stderr` buffers. The `executeComponent` path (engine.go:1453) calls this for each core module in a component-model bundle. If two component-model workflows execute concurrently on the same Runtime, this would still race.

The `wazeroBackend.Execute()` path is fixed (uses per-backend buffers via `instantiateModuleNamedWithWriters`), but the legacy `Engine.Execute()` → `executeComponent()` path bypasses this and hits the shared buffers. The Runtime struct comment already warns that `stdout`/`stderr` are "NOT goroutine-safe" for callers of `InstantiateModuleNamed`, which is good.

**Recommendation:** Either make `executeComponent` use `instantiateModuleNamedWithWriters` with per-execution buffers, or add explicit synchronization to `InstantiateModuleNamed`. This is out of scope for the current task (which only targets the `PerExecution().Execute()` path) but should be tracked as a follow-up.

---

## NIT

**N1: Prior review SF1 is now addressed**

The prior review (cleat-230-race-fix1r) flagged SF1 about lacking goroutine-safety documentation on `Runtime.stdout`/`stderr`. The current code (runtime.go:37-39) includes the comment:
```
// stdout/stderr are NOT goroutine-safe — they are shared across callers
// of InstantiateModuleNamed. Concurrent execution must use the
// wazeroBackend.Execute() path, which uses per-backend buffers.
```
This is present and adequate. The prior review's SF1 is resolved.

**N2: Prior review N1 (trailing blank line) appears resolved**

The current `Runtime` struct (runtime.go:35-45) has no trailing blank line before the closing brace. Either the prior review was looking at an intermediate state or the blank line was already cleaned up.

**N3: Race test could exercise actual output capture**

`TestRuntimeStdoutStderrRace` uses `minimalWasm()` (8 bytes: magic + version, no exports) and ignores the error from `Execute`. This verifies that `InstantiateModule` + `Reset` don't race, which is the core fix. A stronger test would use a WASM module that actually writes to stdout/stderr, to verify that parallel writes to per-backend buffers don't corrupt each other. Not blocking — the current test covers the race condition that was actually reported.

---

## Verified

| Claim | Result |
|---|---|
| Race test passes with `-race -count=5` | PASS (independently confirmed) |
| `signals` field removed from `execSession` struct | Confirmed: not present in engine.go:1662-1693 |
| `signals` field removed from test literal | Confirmed: not present in host_test.go newTestExecSession |
| `isDeadlockError` removed from mysql_store.go | Confirmed: zero matches in file |
| `isDeadlockError` removed from entire engine/ | Confirmed: zero matches |
| `workEntryPoint`, `workInput` removed from Runtime | Confirmed: zero matches in runtime.go |
| `completeMu`, `completeResult`, `completeErr` removed from Runtime | Confirmed: zero matches in runtime.go |
| `cleatComplete` uses context-based storage | Confirmed: reads from `ctx.Value(&cleatCompleteKey)` (imports.go:868) |
| `cleat_poll_work` returns 0 (no-op) | Confirmed: handler returns 0 unconditionally (imports.go:855) |
| `wazeroBackend.Execute` uses per-backend buffers | Confirmed: calls `instantiateModuleNamedWithWriters` with `&b.stdout`, `&b.stderr` (backend_wazero.go:81) |
| `PerExecution()` creates independent backend | Confirmed: `&wazeroBackend{rt: b.rt}` with zero-value `stdout`/`stderr` (backend_wazero.go:56) |
