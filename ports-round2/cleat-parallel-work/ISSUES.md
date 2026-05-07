# Issues Encountered During Port

This document captures every issue, gap, bug, and missing feature discovered
while porting the Restate ``parallelizework`` example to the Cleat Python SDK.

---

## Issue 1: No `ctx.run()` / `ctx.run_typed()` equivalent

**Severity**: Critical

**Description**: Restate's `ctx.run_typed()` records a deterministic Python
function call in the event journal.  The function executes once on first
run; on replay, the recorded result is returned without re-execution.
Cleat has no equivalent API.

The Restate example uses `run_typed` for three operations:
1. `split(task)` -- pure, no side effects
2. `execute_subtask(subtask)` -- uses `time.sleep()`, has side effects
3. `aggregate(results)` -- pure, no side effects

**Expected behavior**: A `HostCalls.run_typed(name, fn, **kwargs)` method
(or similar) that records a local function call in the event journal so
it can be replayed deterministically.

**Workaround used**: 
- Pure functions (`split`, `aggregate`) run locally without journaling.
  They are deterministic and produce the same output on replay.
- Side-effectful operations (`execute_subtask`) are wrapped as child
  workflows, which adds complexity and overhead.

**Recommendation**: Add a `durable_run` host import that records a
function's name, arguments, and return value (similar to
`durable_call` but for local WASM functions).  Or add a
`h.run_typed()` method that JSON-serialises inputs/outputs and
records them as a journal entry.

---

## Issue 2: No way to run multiple `durable_call()` calls in parallel

**Severity**: High

**Description**: `restate.gather()` can collect any list of promise-like
objects, including those from `ctx.run_typed()`.  Cleat's
`await_all_children()` only works with child workflow run IDs.  There is
no way to run multiple `durable_call()` invocations concurrently.

This means that if you need to call 3 external services in parallel, you
must either:
1. Wrap each call in a separate child workflow (heavyweight)
2. Call them sequentially (loses parallelism)

**Expected behavior**: A mechanism to run multiple `durable_call()` calls
concurrently, perhaps via an `await_all_calls()` or by returning
deferred/promise-like objects from `durable_call()`.

**Workaround used**: Wrapped each parallel unit of work in a child
workflow.  This works but adds significant overhead for simple calls.

**Recommendation**: Consider adding a `durable_call_async()` that returns
a handle, and a `gather()` method that awaits multiple handles.
Alternatively, allow `await_all_children` to accept handles from other
operations.

---

## Issue 3: `ChildResult.result` typing and JSON encoding confusion

**Severity**: Medium

**Description**: `ChildResult.result` is typed as `str` in the dataclass:
```python
@dataclass
class ChildResult:
    run_id: str
    result: str
    error: Optional[str] = None
```

However, the actual runtime behavior depends on how the host serialises
child workflow outputs.  When a child workflow returns a plain string,
the `@durable_entry` decorator serialises it with `json.dumps`, producing
a JSON-quoted string.  The host may embed this in the `await_all_children`
response.  The result is that `cr.result` may be `"hello"` (with inner
JSON quotes) or `hello` (decoded), depending on the host implementation.

The test harness returns the stub value as-is, which does not reflect
the real host's JSON encoding behavior.

**Expected behavior**: Clear documentation on what `ChildResult.result`
contains.  If it is always a JSON-encoded string, that should be
documented and the type annotation should make it clear.

**Workaround used**: In the parent workflow, used `json.loads(cr.result)`
to decode the child's return value.  In tests, stubbed with the
JSON-encoded version (`json.dumps("expected_value")`).

**Recommendation**: 
1. Document the JSON encoding contract for `ChildResult.result`.
2. Consider adding a `ChildResult.parsed_result()` convenience method
   that does `json.loads()` automatically.
3. Update the test harness to reflect the real host's JSON encoding
   behavior (e.g., auto-encode stub values).

---

## Issue 4: `@durable_entry` replaces the original function, complicating testing

**Severity**: Medium

**Description**: The `@durable_entry` decorator replaces the decorated
function with a WASM-export wrapper that takes raw memory pointers
(`args_ptr, args_len, out_ptr, max_out_len`).  This wrapper cannot be
called with a test harness directly.

To test with `CleatTestHarness`, you must access the original function
via `__wrapped__`:
```python
from app import fan_out_worker
result = fan_out_worker.__wrapped__(h, task="hello,world")
```

This is not documented in the SDK and is easy to miss.

**Expected behavior**: Either the decorator should preserve a
test-friendly interface, or there should be clear documentation on
how to test decorated functions.

**Workaround used**: Used `__wrapped__` to access the original function.
The test file documents this pattern.

**Recommendation**: 
1. Document the `__wrapped__` pattern in the SDK docs.
2. Consider adding a `@durable_entry(unsafe_test_mode=True)` option that
   preserves the original function signature alongside the WASM wrapper.
3. Or provide a `durable_entry.get_original(fn)` helper function.

---

## Issue 5: Test harness uses single stub per child workflow name

**Severity**: Medium

**Description**: The test harness stores at most one stub per child
workflow name (in a `dict`):
```python
self._child_stubs: dict[str, _ChildStub] = {}
```

When multiple children of the same name are started, they all resolve
to the same stub result.  It is impossible to test scenarios where
different instances of the same child workflow produce different
results (e.g., one succeeds, one fails).

This is visible in the test harness's `await_child`:
```python
def await_child(self, run_id: str) -> str:
    for child_name, stub in self._child_stubs.items():
        if run_id.startswith(f"test-child-{child_name}"):
            ...
            return stub.result
```

**Expected behavior**: Support multiple stubs per child workflow name
(consumed in FIFO order), similar to how `stub_call` works:
```python
self._call_stubs: list[_CallStub] = []  # FIFO list
```

**Workaround used**: Tests assume all children of the same name produce
the same result.  Mixed success/failure scenarios were tested only
conceptually.

**Recommendation**: Change `_child_stubs` from `dict[str, _ChildStub]`
to `dict[str, list[_ChildStub]]` and consume stubs in FIFO order, or
allow per-instance stubbing by run_id.

---

## Issue 6: No child workflow cancellation

**Severity**: High

**Description**: In the original Restate example, if one parallel task
fails, the runtime can cancel other in-flight tasks.  Cleat has no API
to cancel a child workflow once started.

The stop-on-error variant (`fan_out_worker_stop_on_error`) detects
child failures but cannot cancel the remaining running children.

**Expected behavior**: A `cancel_child(run_id)` method on `HostCalls`
that allows the parent to cancel a running child workflow.

**Workaround used**: The parent detects failures via
`ChildResult.error` but cannot stop the other children.  This is
documented as a limitation.

**Recommendation**: Add `cancel_child_workflow(run_id)` to the HostCalls
API and the corresponding WASM import.

---

## Issue 7: `durable_sleep` blocks the workflow; no parallel timer support

**Severity**: Medium

**Description**: In Restate, you can start multiple timers concurrently
and await them all with `restate.gather()`:
```python
timer1 = ctx.sleep(5000)
timer2 = ctx.sleep(3000)
await restate.gather(timer1, timer2)
```

In Cleat, `durable_sleep()` is a blocking call.  To run multiple timers
in parallel, each must be in a separate child workflow.

**Expected behavior**: `durable_sleep()` should return a handle that
can be gathered, or there should be a non-blocking variant.

**Workaround used**: Each timer is inside a child workflow, which is
heavyweight for simple delayed execution.

**Recommendation**: Consider adding `durable_sleep_async(ms)` that
returns a sleep handle, and an `await_all_sleeps()` or integrate
with the existing `await_all_children` mechanism.

---

## Issue 8: `durable_call` does not support timeouts

**Severity**: Low

**Description**: Restate's `ctx.call()` supports configurable timeouts.
Cleat's `durable_call()` has no timeout parameter.  If the called
service hangs, the workflow hangs indefinitely.

This is not directly used in the parallel work port, but would be
needed for production use.

**Expected behavior**: A `timeout_ms` parameter on `durable_call()`,
similar to `await_signals(signal_names, timeout_ms)`.

**Workaround used**: N/A (not used in this port).

**Recommendation**: Add an optional `timeout_ms` parameter to
`durable_call()` and `durable_call_with_retry()`.

---

## Issue 9: `set_state` / `get_state` use `durable_call` internally, not a dedicated host import

**Severity**: Low

**Description**: The `set_state` and `get_state` methods delegate to
`durable_call("state", "set", ...)` and `durable_call("state", "get", ...)`.
This means state operations are recorded as generic `durable_call` entries
in the journal, not as dedicated state operations.

This works for simple cases but couples the state API to the availability
of a "state" service on the host.

**Expected behavior**: Dedicated WASM imports for `set_state` and
`get_state`, similar to `set_query_state`.

**Workaround used**: N/A (state is not used in this port, but noted as
a potential issue for other ports).

**Recommendation**: Add dedicated `durable_set_state` and
`durable_get_state` WASM imports, with host-side routing to the
state store.

---

## Issue 10: `@durable_entry` using `get_type_hints` may fail with Pydantic

**Severity**: Medium

**Description**: The `@durable_entry` decorator uses
`inspect.get_type_hints()` to identify the `HostCalls` parameter:
```python
hints = get_type_hints(func)
for pname in all_param_names:
    if pname in hints and hints[pname] is HostCalls:
        continue
    workflow_param_names.append(pname)
```

If a workflow parameter uses a Pydantic model type hint (e.g.,
`task: Task` where `Task` is a `pydantic.BaseModel`), and Pydantic's
metaclass system interferes with `get_type_hints()`, the decorator
may fail to correctly identify which parameters are workflow inputs
vs. injected dependencies.

**Expected behavior**: The decorator should handle Pydantic-typed
parameters gracefully, either by falling back to `inspect.signature()`
or by providing explicit parameter configuration.

**Workaround used**: Used primitive types (`str`) for workflow
parameters instead of Pydantic models.

**Recommendation**: Add explicit parameter metadata support to
`@durable_entry`, or test compatibility with Pydantic models and
document any limitations.
