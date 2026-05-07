# Cleat Port: Parallel Work (Fan-out / Fan-in)

This directory contains a port of the Restate ``parallelizework`` example
to the Cleat durable execution Python SDK.

## Overview

The original Restate example demonstrates the **fan-out/fan-in** pattern:
split a task into subtasks, execute them in parallel, wait for all to
complete, and aggregate the results.

### Restate original

```python
@restate.Service("FanOutWorker").handler()
async def run(ctx: restate.Context, task: Task) -> Result:
    subtasks = await ctx.run_typed("split task", split, task=task)
    result_promises = [
        ctx.run_typed(f"execute {subtask}", execute_subtask, subtask=subtask)
        for subtask in subtasks.subtasks
    ]
    results_done = await restate.gather(*result_promises)
    results = [await result for result in results_done]
    return aggregate(results)
```

### Cleat equivalent

```python
@durable_entry
def fan_out_worker(h: HostCalls, task: str) -> str:
    # Split (pure function, runs locally)
    subtasks = [s.strip() for s in task.split(",") if s.strip()]

    # Fan out: start child workflows
    run_ids = [h.child_workflow("execute_subtask", {"subtask": s}) for s in subtasks]

    # Fan in: wait for all
    child_results = h.await_all_children(run_ids)

    # Collect and aggregate
    results = [json.loads(cr.result) for cr in child_results if not cr.error]
    return ", ".join(results)
```

---

## API Mapping: Restate -> Cleat

| Restate API                            | Cleat Equivalent                            | Notes                                              |
|----------------------------------------|---------------------------------------------|----------------------------------------------------|
| `restate.Service(name)`                | N/A (use `@durable_entry` directly)         | No service class needed in Cleat                   |
| `@service.handler()`                   | `@durable_entry(name)`                      | Cleat uses a decorator, not a handler method       |
| `ctx.run_typed(name, fn, **kwargs)`    | Pure function call OR `durable_call()`      | No `run_typed` equivalent; pure code runs locally  |
| `restate.gather(*promises)`            | `h.await_all_children(run_ids)`             | Only works with child workflow run_ids             |
| `await promise`                        | `json.loads(cr.result)` or `cr.result`      | ChildResult has result (str) and error (Optional)  |
| `ctx.awakeable()`                      | `h.create_promise()` / `h.await_promise()`  | Promises can be created/awaited/resolved           |
| `ctx.run(name, fn)`                    | No direct equivalent                        | Use `durable_call()` or child workflow             |
| `ctx.sleep(duration)`                  | `h.durable_sleep(ms)`                       | Same concept, different method name                |
| `ctx.signal()` / `ctx.promise()`       | `h.signal_workflow()` / `h.await_signals()` | Signal handling is supported                       |
| `restate.app(services)`                | N/A (WASM module)                           | Cleat compiles to WASM, no Python app server       |

---

## Key differences

### 1. Parallel execution model

**Restate**: Uses `restate.gather()` on a list of promise-like objects
created by `ctx.run_typed()`.  Works with any recorded operation.

**Cleat**: Uses `h.await_all_children(run_ids)` which only works with
child workflow run IDs.  Parallel `durable_call()` or `durable_sleep()`
is not directly supported -- you must wrap them in child workflows.

### 2. `ctx.run_typed` / `ctx.run` -- no equivalent

Restate's `ctx.run_typed()` records a pure Python function call in the
event journal, capturing its inputs and outputs for deterministic replay.
Cleat has no direct equivalent.  In this port:

- **Pure functions** (no side effects) run locally without journaling.
  They execute again on replay and produce the same result.
- **Side-effectful operations** must use `durable_call()`,
  `child_workflow()`, or other journaled HostCalls methods.

This is a significant gap.  Workflows that depend on `ctx.run()` for
enclosing non-deterministic code will need to restructure that code
into services called via `durable_call()` or child workflows.

### 3. Result type handling

Restate's `ctx.run_typed()` returns properly typed results matching
the Pydantic model.  Cleat's `ChildResult.result` is always a JSON
string that needs to be `json.loads()`'d.  This is more manual.

### 4. Error propagation

**Restate**: `restate.gather()` propagates exceptions from any failed
parallel task immediately (fail-fast).

**Cleat**: `h.await_all_children()` collects failures in
`ChildResult.error`.  The parent workflow decides how to handle them.
No automatic fail-fast unless you check each result.

### 5. Cancellation of parallel tasks

**Restate**: When using `restate.gather()`, if one task fails, the
Restate runtime can cancel the remaining in-flight tasks.

**Cleat**: No child workflow cancellation API exists.  Running children
continue to completion even if the parent detects a failure.

### 6. `@durable_entry` and testing

The `@durable_entry` decorator wraps functions for the WASM ABI.  For
testing with `CleatTestHarness`, use the `__wrapped__` attribute to
access the original function:
```python
from app import fan_out_worker
h = CleatTestHarness()
result = fan_out_worker.__wrapped__(h, task="hello,world")
```

---

## Files

| File                  | Purpose                                          |
|-----------------------|--------------------------------------------------|
| `app.py`              | Workflow definitions (fan_out_worker + child)    |
| `test_app.py`         | Tests using CleatTestHarness                     |
| `services_contract.md` | What host-side services are needed              |
| `ISSUES.md`           | Issues encountered during the port               |
