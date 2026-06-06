# cleat-231: ChildWorkflow API Cleanup

**Budget:** $15 (~2 days)
**Priority:** 2 (DX consistency)
**Status:** pending
**Depends on:** none

## Scope

The codebase has three child workflow APIs that overlap confusingly:
- `ChildWorkflow(name, inputJSON)` — bare, no options
- `ChildWorkflowWithOptions(name, inputJSON, opts)` — full options struct
- `ChildWorkflowTyped(name, inputJSON)` — typed variant

## Actions

1. Audit all callers across workflows, plugins (dag), benchmarks, and tests
2. Determine if `ChildWorkflow` should be deprecated in favor of `WithOptions`, or if the relationship should be clarified in docs
3. Update benchmark workflows to use the canonical form
4. Update ARCHITECTURE.md and ABI.md to document the correct API and deprecation status
5. Ensure all three remain functional (backward compat) but document which is preferred

## Key Files

- `cleat/selector.go`
- `wasm/adapter.go`
- `wasm/usage.go`
- `plugins/dag/dag.go`
- `ARCHITECTURE.md`
- `ABI.md`

## Additional Scope (from surveys)

- Fix stale module paths in ARCHITECTURE.md (references to `internal/host/`, `internal/wasm/`, `internal/auth/` which have moved)
