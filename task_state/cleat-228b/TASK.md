# cleat-228b — WASM Debugger CLI

**Parent:** CEO Guidance 2026-05-25
**Budget:** $20 (re-scoped from $80; ~$60 of production code already merged)
**Priority:** 1 (last remaining 2026-05-22 item)
**Assigned to:** implementation agent (cleat-228b), explorer agent (cleat-228be)

## Scope

Implement the `cleatctl debug <workflow-id>` command — an interactive step-through debugger for WASM workflow replay. The engine plumbing (cleat-228a: `advanceReplayStep`, `ReplayStepCallback`, `ReplayQuit`) is complete and available in `engine/engine.go`.

## Background

The original debug.go (494 lines) was implemented but removed in commit 7514f61 because it referenced `ReplayStepCallback` types that had been temporarily removed from the engine. The engine types have since been restored (via the refactor in 3eeb74e) and are now in `engine/engine.go`.

Production fixes (command registration, brace bug, usage text) were merged via PR #56 (commit 3e20b63) but only touched `main.go` — the debug.go implementation file was never re-created.

## Deliverables

1. **Create `cmd/cleatctl/debug.go`** — Port the original 494-line implementation with updates:
   - Update import path: `internal/host` → `github.com/cleat-team/cleat/engine`
   - Update type references: `host.*` → `engine.*`
   - Wire `case "debug":` in main.go to call `runDebug()`
   - Fix the brace bug (missing `}` before `return` in runDebug)
   - Add `--watch` to usage text

2. **Fix mockStore fn fields** (`cmd/cleatctl/cleatctl_command_test.go`):
   - Add `countEventHistoryFn` field to mockStore struct
   - Add `loadEventHistoryPaginatedFn` field to mockStore struct
   - Wire them into the corresponding methods

3. **Create `cmd/cleatctl/cleatctl_debug_test.go`** — Tests for:
   - Step-through mode with mock replay
   - Watch mode polling
   - Error handling (missing workflow, no events, empty history)
   - Command parsing (next, continue, state, events, help, quit)

4. **Create `docs/how-to/debug-workflows.md`** — Usage guide with example session

## Files

- `cmd/cleatctl/debug.go` — NEW (primary implementation)
- `cmd/cleatctl/main.go` — wire debug command
- `cmd/cleatctl/cleatctl_command_test.go` — mockStore fn fields
- `cmd/cleatctl/cleatctl_debug_test.go` — NEW (tests)
- `docs/how-to/debug-workflows.md` — NEW (documentation)
- `engine/engine.go` — reference only (engine plumbing already done)
- `cmd/cleatctl/replay.go` — reference (loadWorkflowInstance, replayStubCaller, truncate are reusable)

## Engine API Already Available

- `engine.ReplayStepCallback` (line 635)
- `engine.ReplayStepAction`, `ReplayNext`, `ReplayQuit` (lines 638-644)
- `engine.WithReplayStepCallback(cb)` (line 895)
- `engine.NewEngine(rt, caller, opts...)` (line 918)
- `engine.NewRuntime(ctx, memPages, instrLimit)` (runtime.go:93)
- `engine.Replay(ctx, wasmBytes, entryPoint, input, history)` (line 987)
- `engine.WithDefName/WithDefVersion/WithWorkflowID` etc.

## Success Criteria

- `cleatctl debug <id> --entry-point <name>` steps through workflow events interactively
- Commands: next (n), continue (c), state (s), events (e), help (h), quit (q)
- `--watch` flag tails live events as they arrive
- Tests pass: `go test ./cmd/cleatctl/ -run "Debug" -count=1`
- Documentation complete with usage guide and example session

## What NOT to Do

- Don't modify the engine API — ReplayStepCallback infrastructure is done
- Don't change the WorkflowStore interface
- Don't change replay.go — it's a separate diagnostic tool
- Don't add new dependencies
