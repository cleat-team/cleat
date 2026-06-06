# CONTRACT: cleat-231 — ChildWorkflow API Cleanup

## Deliverables

1. **Audit report**: List of all callers of `ChildWorkflow`, `ChildWorkflowWithOptions`, `ChildWorkflowTyped` with file paths and line numbers
2. **Canonical API decision**: Document which API is preferred, which are deprecated, with rationale
3. **Benchmarks updated**: All benchmark workflows use the canonical form
4. **ARCHITECTURE.md updated**: Document ChildWorkflow API, fix stale module paths
5. **ABI.md updated**: Document `cleat_child_workflow` host call and variants

## Invariants

- All three APIs (`ChildWorkflow`, `ChildWorkflowWithOptions`, `ChildWorkflowTyped`) remain functional
- No existing workflow callers broken
- Benchmark results not regressed

## API Surface

| Function | File | Action |
|----------|------|--------|
| `ChildWorkflow` | `cleat/selector.go` | Document deprecation preference |
| `ChildWorkflowWithOptions` | `cleat/selector.go` | Document as canonical |
| `ChildWorkflowTyped` | `cleat/selector.go` | Document relationship |
| WASM adapters | `wasm/adapter.go`, `wasm/usage.go` | Verify host call signatures |
| DAG plugin | `plugins/dag/dag.go` | Update to canonical form |

## Test Requirements

- `go test ./cleat/...` passes
- `go test ./wasm/...` passes
- `go test ./plugins/dag/...` passes
- Benchmark workflow compiles and runs

## Integration Points

- SDK users (Go, Rust, AssemblyScript) — ensure canonical API is documented for each
- ARCHITECTURE.md module path fix must also update the coupling matrix if module boundaries changed

## Coupling

- LOOSE with `cleat-236` (same ARCHITECTURE.md and ABI.md files)
- NONE with other leaf tasks
