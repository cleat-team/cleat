# Contract — cleat-230-errorse

## Deliverables

1. **Error audit report**: List of all error messages in engine hot paths, classified as actionable/needs-improvement
2. **Code changes**: Improved error messages that include context (what was happening, which component, what to do)
3. **Updated STATUS.md**: Documenting what was changed

## Invariants

- Error types and interfaces must not change — only the message strings
- Error messages must remain single-line (no multi-line errors in log output)
- No new dependencies or imports

## Integration Points

- `internal/host/engine.go` — WASM execution, replay, workflow lifecycle
- `cmd/cleat-worker/main.go` — worker startup, shutdown, signal handling
- Error messages flow through to: clew dashboard, worker logs, CLI output

## Test Requirements

- Existing tests must continue to pass
- If tests assert on specific error message strings, update assertions to match improved messages

## Coupling

- LOOSE with `cleat-230-racee` (same engine.go file, different functions)
- LOOSE with `cleat-230-logse` (same logging output, different concern)
- NONE with all other tasks
