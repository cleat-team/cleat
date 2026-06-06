# CONTRACT: cleat-233 — SDK Test Passes

## Deliverables

1. **Rust SDK**: All `wasm/testdata/` tests pass. Any ABI mismatches fixed.
2. **AssemblyScript SDK**: Cross-language E2E tests pass.
3. **Python SDK**: Tests assessed. If viable, tests pass. If not, documented limitation with escalation summary.
4. **LANGUAGE_SUPPORT.md**: Updated with current SDK status, accurate line counts, known limitations.

## Invariants

- Go SDK (native) continues to work — no regressions
- WASM ABI frozen for 0.5 — no new host calls, no signature changes
- Existing workflow tests not broken by SDK fixes

## API Surface

| SDK | Test Entry | Key Concern |
|-----|-----------|-------------|
| Go (native) | `go test ./...` | Baseline — must stay green |
| Rust | `cargo test` (wasmtime) | ABI compatibility with engine imports |
| AssemblyScript | Cross-language E2E | WASM component model compliance |
| Python | `pytest` | Viability TBD (4h timebox) |

## Test Requirements

- Rust SDK: all tests pass
- AssemblyScript: cross-language E2E green
- Python: tests pass OR documented limitation with rationale
- No regressions in Go test suite
- LANGUAGE_SUPPORT.md accurately reflects current state

## Integration Points

- WASM imports defined in `engine/imports.go` (53 `cleat_*` exports) — SDKs must match
- ABI.md is the contract — this task verifies compliance
- Commit `1b7f8ed` changed WASM input dispatch — review for SDK impact

## Coupling

- MEDIUM with `cleat-234` (cleat-234 consumes green SDK tests for CI enforcement)
- LOOSE with `cleat-235` (code review may find WASM issues)
- LOOSE with `cleat-236` (LANGUAGE_SUPPORT.md shared)
- NONE with other leaf tasks
