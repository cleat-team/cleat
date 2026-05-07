# Migrating from Temporal

For a complete migration guide with detailed code examples and known gaps, see
the [Temporal to Cleat Migration Guide](../migration-temporal.md).

## Key differences at a glance

| Temporal Concept | Cleat Equivalent |
|---|---|
| Workflow + Activity distinction | Single `HostCalls` interface |
| `workflow.Context` | `HostCalls` parameter |
| `ExecuteActivity` | `DurableCall(service, operation, request)` |
| `GetSignalChannel` | `AwaitSignals(names, timeout)` |
| `ExecuteChildWorkflow` | `ChildWorkflow(name, input)` + `AwaitChild(runID)` |
| `NewTimer` / `workflow.Sleep` | `DurableSleep(duration)` |
| `Workflow.SideEffect` | Use `DurableCall` to a stub service |
| Worker Build IDs | WASM blob versioning (`cleat rollback`) |
| `ChildWorkflowOptions` | No equivalent (simpler API) |
| Interceptors / middleware | Not needed (WASM boundary) |

## Migration strategy

1. **Map activities to DurableCalls** -- Replace `ExecuteActivity(ctx, fn, args)`
   with `h.DurableCall("service", "operation", requestJSON)`. Cleat has no
   distinction between workflows and activities.
2. **Replace context propagation** -- Change `workflow.Context` to `cleat.HostCalls`
   as the first function parameter.
3. **Replace signals** -- Change `GetSignalChannel` to `AwaitSignals`.
4. **Replace child workflows** -- Change `ExecuteChildWorkflow` to
   `ChildWorkflow` + `AwaitChild`.
5. **Replace timers** -- Change `workflow.Sleep` to `DurableSleep` (both use
   `time.Duration` in Go).
6. **Simplify versioning** -- No need to keep old workers running. WASM blobs
   are versioned in the database. Rollback is `cleat rollback`.

## Notable gaps

- **Activity retry isolation** -- Temporal retries activities independently.
  Cleat's `call_with_retry()` provides server-side retry but the whole workflow
  step is re-executed on replay.
- **Cancellation propagation** -- Temporal has built-in context cancellation.
  Cleat uses explicit `PollCancellation()` calls.
- **List/Describe APIs** -- Cleat exposes REST equivalents at `/api/workflows`.
- **Side Effect** -- No direct equivalent. Use a stub `DurableCall` or `Random()`.

See the [full migration guide](../migration-temporal.md) for code comparisons
in Go, Python, and before/after examples.
