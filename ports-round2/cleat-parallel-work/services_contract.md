# Services Contract

This document describes the host-side services required by the Cleat parallel
work port.  These services must be available for the workflows to function
correctly.

---

## 1. Workflow entry points (registered via `@durable_entry`)

These are compiled to WASM exports and do NOT require external services.
They are listed here for deployment reference.

| Export name               | Source function            | Input                          | Output                        |
|---------------------------|----------------------------|--------------------------------|-------------------------------|
| `fan_out_worker`          | `fan_out_worker`           | `{"task": "<string>"}`         | Aggregated result string      |
| `execute_subtask`         | `execute_subtask`          | `{"subtask": "<string>"}`      | `"<subtask>: DONE"`          |
| `fan_out_worker_stop_on_error` | `fan_out_worker_stop_on_error` | `{"task": "<string>"}`    | Aggregated result string      |

---

## 2. Host imports required

The compiled WASM module imports these functions from the `"env"` module.
All are provided by the Cleat host runtime.

| Import                           | Used by                    | Description                                  |
|----------------------------------|----------------------------|----------------------------------------------|
| `durable_child_workflow`         | `fan_out_worker`           | Start a child workflow (fan-out)             |
| `durable_await_all_children`     | `fan_out_worker`           | Wait for all child workflows (fan-in)        |
| `durable_sleep`                  | `execute_subtask`          | Simulate work (deterministic sleep)          |
| `durable_log`                    | both workflows             | Deterministic logging for observability      |

---

## 3. No external services needed

This port is self-contained.  Unlike the original Restate example, which uses
`ctx.run_typed()` to call local utility functions, this Cleat port:

- **Runs `split` and `aggregate` locally** (pure computation, no journaling).
- **Runs `execute_subtask` as a child workflow** with `durable_sleep` to
  simulate work.
- **Does NOT require any external service** (no `durable_call` targets).

If you were to replace `durable_sleep` with a real operation, you would need
a service registered with the Cleat host that can handle the operation:

| Service        | Operation        | Purpose                           |
|----------------|------------------|-----------------------------------|
| `task_executor` | `DoWork`        | Execute a real subtask            |

In that case, the child workflow would call:
```python
result = h.durable_call("task_executor", "DoWork", {"subtask": subtask})
```
