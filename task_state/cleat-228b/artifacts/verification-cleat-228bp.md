# Verification Report — cleat-228bp

**Task:** cleat-228b (WASM Debugger CLI)
**Date:** 2026-06-06
**Scope:** Independent re-verification of exploration findings + implementation review

## Verified Claims

All exploration report findings (cleat-228be) re-verified against current codebase:

1. **Engine plumbing (Finding 1):** CONFIRMED. All 10 types/functions present in `engine/engine.go`.
2. **Reference implementation (Finding 2):** CONFIRMED. `loadWorkflowInstance`, `replayStubCaller`, `truncate` all in `replay.go`.
3. **Debug command stub (Finding 3):** STALE. Main.go already has `runDebug()` wired (uncommitted change).
4. **Original debug.go (Finding 4):** STALE. debug.go already exists (493 lines, created ~03:47 today).
5. **mockStore (Finding 5):** STALE. Both fn fields already added and wired.
6. **Test infrastructure (Finding 6):** CONFIRMED. Helpers available (`captureStdout`, `captureOutputs`, `withExitPanic`, `withStdin`).
7. **NewRuntime signature (Finding 7):** CONFIRMED. Signature `(ctx, uint32, uint64)` matches usage.

## Deliverables Status

| # | Deliverable | Lines | Status |
|---|-------------|-------|--------|
| 1 | debug.go | 493 | DONE (bug found) |
| 2 | mockStore fn fields | ~10 | DONE |
| 3 | cleatctl_debug_test.go | 0 | MISSING |
| 4 | debug-workflows.md | 162 | DONE |

## Bug: state/events commands broken in step-through mode

**Location:** `cmd/cleatctl/debug.go:263` (`displayStep`)

**Root cause:** `debugState.lastStep`, `lastEvent`, `lastQS` are only set in `callback()` when
`autoContinue` is true (lines 229-233). During normal step-through, the callback sends info
via `stepCh` but never updates these fields. `displayStep()` also doesn't save them.

**Impact:**
- `state`/`s` command: always prints "(no query state yet — advance at least one step)"
- `events`/`e` command: uses `lastStep+1` = 1, skipping event 0

**Fix (3 lines in `displayStep()`):**
```go
func (d *debugState) displayStep(info debugStepInfo) {
    d.lastStep = info.step      // ADD
    d.lastEvent = info.event    // ADD
    d.lastQS = info.qs          // ADD
    total := len(d.events)
    // ...
}
```

This is a pre-existing bug from the original debug.go (commit 7514f61^).

## Build Verification

```
go build ./cmd/cleatctl/     PASSED
go vet ./cmd/cleatctl/       PASSED
go test ./cmd/cleatctl/ -run "Debug" -count=1   PASSED (no tests)
```

## Uncommitted Changes

`cmd/cleatctl/main.go` has two uncommitted changes:
1. `case "debug":` now calls `runDebug()` instead of printing stub message
2. Usage text includes `[--watch]`

These are correct and match the task requirements.
