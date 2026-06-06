# cleat-231 Status

**Phase:** complete
**Last updated:** 2026-06-05
**Dispatched by:** cto-lap-032

## Deliverables

1. **Audit report** — See below. All callers identified across workflows, plugins, benchmarks, tests.
2. **Canonical API decision** — No deprecation. `ChildWorkflow` for simple cases, `ChildWorkflowWithOptions` for configured children, `ChildWorkflowTyped` as typed convenience wrapper.
3. **Benchmarks** — `benchmarks/workflows/fanout.go` already uses canonical `ChildWorkflow` form. No change needed.
4. **ARCHITECTURE.md** — Module paths fixed (stale `internal/host/` → `engine/`, etc.). Child Workflow API section added.
5. **ABI.md** — Clarified relationship between `cleat_child_workflow` and `cleat_child_workflow_with_options`. Documented typed SDK wrapper.

## Changes Made

| File | Change |
|------|--------|
| `cleat/runtime.go` | Updated doc comments on interface methods and HostCallsOptions fields to clarify API relationships |
| `ARCHITECTURE.md` | Fixed 15 stale module paths in Module Boundaries and Coupling Matrix tables. Added "Child Workflow API" section. |
| `ABI.md` | Added SDK-level typed wrapper note and API relationship documentation after section 2.20 |
| `task_state/cleat-231/STATUS.md` | This file — audit, decision, changelog |

## Test Results

- `go test github.com/cleat-team/cleat/cleat/...` — all packages PASS
- `go test github.com/cleat-team/cleat/cleat -run ChildWorkflow -v` — all 10 ChildWorkflow tests PASS
- `go vet github.com/cleat-team/cleat/cleat` — no warnings
- `go vet github.com/cleat-team/cleat/benchmarks/workflows` — no warnings
- `wasm/` and `plugins/dag/` test suites have pre-existing failures on `develop` (unrelated to this task)

## Audit Report — ChildWorkflow API Callers

### API Definitions (all in `cleat/runtime.go`)

| API | Signature | WASM Import | Role |
|-----|-----------|-------------|------|
| `ChildWorkflow` | `(name, inputJSON string) (runID, error)` | `cleat_child_workflow` | Base primitive, 2 WASM params |
| `ChildWorkflowWithOptions` | `(name, inputJSON string, opts ChildWorkflowOptions) (runID, error)` | `cleat_child_workflow_with_options` | Extended variant, 5 WASM params |
| `ChildWorkflowTyped` | `(name string, request interface{}) (runID, error)` | `cleat_child_workflow` (via delegation) | Go-level typed convenience wrapper |

### Runtime Implementation Relationships

- `ChildWorkflowTyped` → marshals `request` to JSON → calls `ChildWorkflow` (runtime.go:1743)
- `ChildWorkflowWithOptions` → tries direct handler first → falls back to `ChildWorkflow` (runtime.go:1698-1704)

### Workflow Callers

**`ChildWorkflow` (bare):**
- `benchmarks/workflows/fanout.go:30` — `h.ChildWorkflow("noop_child", "{}")`
- `examples/widget-store-as/host/main.go:431` — `h.ChildWorkflow("dispatchOrder", ...)`
- Tests: `cleat/embedded/runner_test.go:188`, `cleat/cleattest/cleattest_primitives_test.go`

**`ChildWorkflowWithOptions`:**
- `plugins/dag/dag.go:254` — `h.ChildWorkflowWithOptions(wfName, inputStr, ChildWorkflowOptions{Priority: task.Priority})`

**`ChildWorkflowTyped`:**
- `examples/datapipeline/pipeline.go:56` — `h.ChildWorkflowTyped("process_item", childInput)`

## Canonical API Decision

**Recommendation: No deprecation. Use each API for its intended purpose.**

| API | When to Use | Status |
|-----|------------|--------|
| `ChildWorkflow` | Default/no-option child workflow starts. Lightweight at WASM boundary. | **Preferred for simple cases** |
| `ChildWorkflowWithOptions` | When version pinning, parent close policy, or priority needed. | **Canonical for configured children** |
| `ChildWorkflowTyped` | Type-safe Go ergonomics. Marshals input automatically. | **Convenience wrapper** |

**Rationale:** `ChildWorkflow` is the lightweight base primitive. Deprecating it would force all callers to construct an empty `ChildWorkflowOptions{}` struct, adding noise for the common case. `ChildWorkflowWithOptions` is the right choice when options are actually needed. `ChildWorkflowTyped` is a thin Go-level convenience that delegates to `ChildWorkflow`.

## Invariants Preserved

- All three APIs remain functional
- No existing workflow callers broken
- Benchmark code unchanged (already used canonical form)
- `go vet` clean on changed packages
