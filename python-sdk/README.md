# Cleat Python SDK

Python SDK for the [Cleat](https://github.com/rcownie/durable) durable execution framework. Write workflows in Python, compile to WASM, and run on the Cleat engine.

```python
from cleat_sdk import HostCalls, durable_entry

@durable_entry
def my_workflow(h: HostCalls, name: str) -> str:
    resp = h.durable_call("greeter", "Greet", {"name": name})
    return resp
```

## Installation

```bash
pip install cleat-sdk
```

Or from source:

```bash
cd python-sdk/
pip install -e .
```

## Quick start

Define a workflow decorated with `@durable_entry`. The framework injects a `HostCalls` instance as the first argument; additional arguments are deserialised from JSON input automatically.

```python
from dataclasses import dataclass
from cleat_sdk import HostCalls, durable_entry

@dataclass
class GreetingRequest:
    name: str
    language: str = "en"

@durable_entry
def hello_workflow(h: HostCalls, request: GreetingRequest) -> str:
    h.durable_log(f"Hello workflow started for {request.name}")
    response = h.durable_call(
        "greeter", "Greet",
        {"name": request.name, "language": request.language}
    )
    h.durable_log(f"Got response: {response}")
    return response
```

The decorator generates a WASM export wrapper conforming to the Cleat ABI (`(args_ptr, args_len, out_ptr, max_out_len) -> i64`). On replay, completed calls return cached results instead of re-executing.

## HostCalls overview

The `HostCalls` class wraps all 22 WASM host imports grouped by category:

### Time & Random
- `now() -> int` -- wall-clock time in ms since epoch
- `random() -> int` -- deterministic random value (same on replay)
- `version() -> int` -- workflow definition version
- `min_version() -> int` -- minimum supported version

### Durable Execution
- `durable_call(service, operation, request) -> str` -- recorded API call
- `durable_call_typed(service, operation, request, result_type) -> T` -- typed variant with automatic JSON deserialisation
- `durable_call_with_retry(service, operation, request, retry_policy) -> str` -- server-side retry
- `durable_call_with_heartbeat(service, operation, request, interval_ms, progress_cb) -> str` -- long-running call with progress updates
- `durable_sleep(duration_ms) -> None` -- suspend for a duration (survives restarts)
- `durable_log(message) -> None` -- emit a log message

### Signals & Events
- `await_signals(signal_names, timeout_ms) -> SignalResult` -- wait for external signals
- `poll_signal(name) -> (payload, found)` -- non-blocking signal check
- `poll_cancellation() -> (cancelled, reason)` -- check for cancellation

### Child Workflows
- `child_workflow(name, input) -> str` -- start child workflow, returns run ID
- `await_child(run_id) -> str` -- await a single child, returns result JSON
- `await_all_children(run_ids) -> list[ChildResult]` -- await multiple children concurrently

### State
- `set_query_state(key, value) -> None` -- set queryable state key-value pair
- `set_state(key, value) -> None` -- set typed state (marshals to JSON)
- `get_state(key, result_type) -> T` -- get typed state (unmarshals from JSON)
- `delete_state(key) -> None` -- delete a state key
- `incr_state(key, delta=1) -> int` -- atomically increment a numeric state key

### Promises
- `create_promise(name) -> str` -- create a durable promise, returns promise ID
- `await_promise(promise_id, timeout_ms) -> PromiseResult` -- await a promise with timeout

### Lifecycle
- `durable_defer(description) -> str` -- register cleanup to run on exit, returns defer ID
- `continue_as_new(input) -> None` -- start a fresh run with new input
- `run_detached(fn) -> None` -- execute a function detached from cancellation
- `register_update_handler(name) -> None` -- register update handler for bi-directional RPC

### Plugin Calls
- `plugin_call(plugin_name, function_name, input) -> str` -- call a host plugin function

## Examples

The `examples/` directory contains ready-to-run workflows:

| File | Description |
|------|-------------|
| `hello_workflow.py` | Simple call-and-return workflow (see Quick start) |

## Build

Compile Python workflows to WASM using the Cleat CLI:

```bash
durable build --target python ./workflow.py
```

Under the hood, this uses `componentize-py` from the [Bytecode Alliance](https://github.com/bytecodealliance/componentize-py) to compile Python to a WASM component, then wraps it in a shim that maps the Cleat ABI to the component model.

Direct `componentize-py` usage:

```bash
componentize-py ./workflow.py -o workflow.wasm
```

## Requirements

- **Python 3.10+** -- the SDK uses `typing.get_type_hints` and `inspect.signature` for parameter introspection
- **componentize-py** -- required for WASM compilation (installed separately)
- **Cleat CLI** (`durable build`) -- for the full build pipeline

Optional dependencies for development:

```bash
pip install "cleat-sdk[dev]"   # pytest, pytest-cov, ruff
```

## API reference

The SDK exposes four main modules via `cleat_sdk/`:

| Module | Contents |
|--------|----------|
| `host_calls` | `HostCalls`, `SuspendSentinel`, `RetryPolicy`, `SignalResult`, `ChildResult`, `PromiseResult` |
| `entry` | `durable_entry` decorator |
| `memory` | WASM linear memory helpers and bit-packing decoders |
| `types` | `ChildWorkflow[T]`, `Saga`, `SagaStep`, `DurableDefer` |

### `durable_entry(name=None)`

Decorator that marks a function as a Cleat workflow entry point. The decorated function must accept `HostCalls` as its first parameter. Additional parameters are deserialised from the workflow input JSON by name. The decorator handles suspend propagation (`SuspendSentinel` exception) and error serialisation.

### `HostCalls`

Core class wrapping all 22 WASM host function imports. Each method handles the pointer+length string protocol and bit-packed `i64` result decoding per the Cleat ABI. The host runtime guarantees deterministic replay -- all side effects are recorded in the event history.

### Result types

- `SignalResult(name, payload, timed_out)` -- returned by `await_signals`
- `ChildResult(run_id, result, error)` -- returned by `await_all_children`
- `PromiseResult(result, timed_out)` -- returned by `await_promise`
- `RetryPolicy(max_attempts, initial_interval_ms, backoff_coefficient, max_interval_ms, non_retryable_errors)` -- for `durable_call_with_retry`
- `SuspendSentinel` -- exception raised to signal workflow suspension

## Further reading

- [Cleat project README](../../README.md) -- architecture, worker deployment, CLI reference, database schema
- [Cleat WASM ABI specification](../../ABI.md) -- full ABI contract, bit-packing layouts, memory layout
