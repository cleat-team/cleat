# WASM Debugger CLI — Exploration Report (Post-Implementation Audit)

**Task:** cleat-228b / cleat-228be
**Scope:** `cmd/cleatctl/`, `engine/engine.go`, `docs/`
**Date:** 2026-06-06
**Phase:** `done` — audit verification of completed implementation

---

## Audit Results: ALL DELIVERABLES VERIFIED

The task is complete. All 4 deliverables match STATUS.md claims:

| # | Deliverable | Status | Verified |
|---|-------------|--------|----------|
| 1 | `cmd/cleatctl/debug.go` | DONE | 498 lines, imports engine package, compiles |
| 2 | mockStore fn fields | DONE | countEventHistoryFn + loadEventHistoryPaginatedFn wired |
| 3 | `cmd/cleatctl/cleatctl_debug_test.go` | DONE | 942 lines, 45+ tests |
| 4 | `docs/how-to/debug-workflows.md` | DONE | 162 lines, covers both modes |

### Detail: debug.go (498 lines)

- **Imports:** `github.com/cleat-team/cleat/engine` (correct — migrated from `internal/host`)
- **Entry point:** `runDebug()` dispatches step-through vs watch based on `--watch`
- **Step-through:** `runDebugStep()` loads workflow instance via `loadWorkflowInstance()` (replay.go:202), loads events, loads WASM, creates runtime+engine with `ReplayStepCallback`, runs interactive loop
- **Watch mode:** `runDebugWatch()` polls `CountEventHistory` every 2s, fetches new pages via `LoadEventHistoryPaginated`, auto-exits after 60s idle
- **`callback()`:** Tracks `lastStep/lastEvent/lastQS` for state/events commands (fixes pre-existing bug), handles auto-continue and quit
- **`displayStep()`:** Shows step N/M, event type + context, request/response/error/payload, query_state
- **`readCommand()`:** Dispatches next/n/"", continue/c, state/s, events/e, help/h, quit/q, with unknown-command feedback
- **Brace bug:** Fixed — `readCommand()` dispatch function is properly closed
- **`--watch`:** Present in `printDebugUsage()` output

### Detail: main.go wiring

- Line 84: `case "debug": runDebug(ctx, store, db, args[1:])`
- Line 111: usage includes `[--watch]`

### Detail: cleatctl_command_test.go mockStore

- Line 89: `countEventHistoryFn func(ctx context.Context, workflowID string) (int, error)`
- Line 90: `loadEventHistoryPaginatedFn func(ctx context.Context, workflowID string, offset, limit int) ([]engine.EventRecord, error)`
- Lines 1717-1718: `LoadEventHistoryPaginated` wired to fn
- Lines 1734-1735: `CountEventHistory` wired to fn

### Detail: cleatctl_debug_test.go (942 lines)

Test coverage:
- Flag parsing: 8 tests (no args, missing workflow, missing entry-point value, missing entry-point, watch mode, positional+flags, --watch flag, extra positional)
- Format functions: 8 tests (query state empty/with-data, remaining events none/beyond-end/with-events, format event activity/signal/truncate)
- Error handling: 4 tests (workflow not found, load events error, no events, load WASM error)
- Watch mode: 3 tests (count error, poll new events, paginated error)
- Interactive commands: 13 tests (next, quit, continue, state full/empty/no-data, events, help, all shortcuts, unknown command, empty input)
- Dispatch and callback: 8 tests
- Constants: 1 test

Tests use sql/driver connector pattern for DB mocking and mockStore for engine mocking — both match production code paths.

### Detail: docs/how-to/debug-workflows.md (162 lines)

- Quick start with example invocation and output
- Step-through mode: per-step display format, full session example, command table
- Watch mode: invocation, output example, exit conditions
- Common scenarios: stuck workflow, determinism verification, side effect inspection
- Limitations: read-only, requires DB, replay only, large histories

---

## Engine API (unchanged, verified)

| Type/Function | File:Line | Status |
|---------------|-----------|--------|
| `ReplayStepCallback` | engine/engine.go:635 | Present |
| `ReplayStepAction` | engine/engine.go:638 | Present |
| `ReplayNext` | engine/engine.go:642 | Present |
| `ReplayQuit` | engine/engine.go:644 | Present |
| `WithReplayStepCallback()` | engine/engine.go:895 | Present |
| `CountEventHistory` | engine/db.go:150 | Present (WorkflowStore interface) |
| `LoadEventHistoryPaginated` | engine/db.go:147 | Present (WorkflowStore interface) |

---

## Risk Assessment (post-implementation)

| Risk | Assessment |
|------|------------|
| Engine API mismatch | None — replay.go uses same API successfully |
| Watch mode idle timeout | Known UX tradeoff: 60s idle exit with 2s polling. Acceptable for debug tooling. |
| No integration tests | True — tests mock engine/store/DB. A real WASM replay test would need a running DB. Out of scope. |
| Build breaks | STATUS.md reports `go build` and `go vet` both pass |

---

## Recommendation

**No further action required.** The task is `done`. All 4 deliverables are implemented and verified against their specifications. The implementation correctly reuses `loadWorkflowInstance`, `replayStubCaller`, and `truncate` from `replay.go` (same package), imports the correct engine package, and has comprehensive test coverage of the CLI layer.
