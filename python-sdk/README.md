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

The `HostCalls` class wraps all 36 WASM host function imports grouped by category:

### Workflow Identity
- `current_workflow_id() -> str` -- the current workflow's unique ID
- `current_run_id() -> str` -- the current run's unique ID

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
- `durable_fetch(url, method, headers, body) -> (body, status)` -- durable HTTP fetch via host
- `durable_send(service, operation, request) -> None` -- fire-and-forget (no response)
- `schedule_invoke(service, operation, request, delay_ms) -> None` -- delayed one-shot invocation

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
- `create_promise(name, ttl_ms=None) -> str` -- create a durable promise, returns promise ID
- `await_promise(promise_id, timeout_ms) -> PromiseResult` -- await a promise with timeout
- `resolve_promise(promise_id, value) -> None` -- resolve a promise from within the workflow
- `reject_promise(promise_id, error) -> None` -- reject a promise from within the workflow

### Update & Query Handlers
- `register_update_handler(name, handler, validator=None) -> None` -- register update handler (bi-directional RPC)
- `register_query_handler(name, handler) -> None` -- register read-only query handler

### Lifecycle
- `durable_defer(description) -> str` -- register cleanup to run on exit, returns defer ID
- `continue_as_new(input) -> None` -- start a fresh run with new input
- `run_detached(fn) -> None` -- execute a function detached from cancellation

### Plugin Calls
- `plugin_call(plugin_name, function_name, input) -> str` -- call a host plugin function

## Examples

The `examples/` directory contains ready-to-run workflows:

| File | Description |
|------|-------------|
| `hello_workflow.py` | Simple call-and-return workflow (see Quick start) |
| `child_workflow.py` | Parent workflow that starts and awaits a child |
| `saga_workflow.py` | Saga pattern with compensating transactions |
| `update_handler_workflow.py` | Workflow with an "approve" update handler and validator |

## Synchronous Execution Model

**All Cleat Python workflows are SYNCHRONOUS.** The `async`/`await` pattern is NOT supported. Workflows execute as plain Python functions that may be suspended and resumed by the host runtime through the `SuspendSentinel` exception mechanism.

```python
# CORRECT: synchronous code
@durable_entry
def my_workflow(h: HostCalls, name: str) -> str:
    h.durable_log(f"Processing {name}")
    result = h.durable_call("service", "Op", {"name": name})
    return result

# WRONG: async is not supported
# @durable_entry
# async def my_workflow(h: HostCalls, name: str) -> str:  ...  # won't work
```

When a workflow calls `durable_sleep()` or `await_signals()` on a fresh execution, the host suspends the workflow. On replay, the same calls return cached results without suspending. The user code never sees the suspend/resume cycle -- it is handled by the `@durable_entry` wrapper.

## Python Standard Library Compatibility

When compiled to WASM via `componentize-py`, **only a subset of the Python standard library is available**.

### Compatible modules (work in WASM)

- `json` -- JSON encoding/decoding (heavily used by the SDK)
- `dataclasses` -- data class definitions (preferred for input/output types)
- `typing` / `typing_extensions` -- type annotations
- `functools` -- decorators and higher-order functions
- `math` -- mathematical functions
- `re` -- regular expressions
- `enum` -- enumerations
- `collections` / `collections.abc` -- container data types
- `itertools` -- iterator tools
- `datetime` -- date and time (basic usage)
- `decimal` -- decimal arithmetic
- `uuid` -- UUID generation
- `copy` -- shallow/deep copy
- `os` -- limited (no subprocess, no filesystem)
- `pathlib` -- limited (no real filesystem I/O)

### Incompatible modules (NOT available in WASM)

- `asyncio` -- async/await not supported
- `threading` -- no thread support in WASM
- `socket` -- no raw socket access
- `subprocess` -- no process spawning
- `multiprocessing` -- no process support
- `signal` -- no OS signal handling
- `select` / `selectors` -- no I/O multiplexing
- `mmap` -- no memory mapping
- `ctypes` / `cffi` -- no native code interop
- `ssl` -- no TLS/SSL (use the host's `durable_call` instead)
- `http.client` / `urllib` / `requests` -- no outbound HTTP (use the host's `durable_call` instead)

To make HTTP requests, use the host-provided `h.durable_call("http", "fetch", ...)` or `h.durable_fetch()` instead of `urllib` or `requests`.

## Unit Differences (Cleat vs. Other Frameworks)

Cleat uses **milliseconds** for all time-related host calls. This is important when porting workflows from other frameworks:

| Framework | Sleep Unit | Example |
|-----------|-----------|---------|
| **Cleat** | milliseconds | `durable_sleep(5000)` = 5 seconds |
| **Temporal** | Go: `time.Duration`, Java: `Duration`, Python: `timedelta` / seconds | `sleep(Duration.ofSeconds(5))` |
| **DBOS** | seconds | `DBOS.sleepSeconds(5)` |
| **Restate** | `Duration` / milliseconds (SDK-dependent) | `Duration.ofSeconds(5)` |

Cleat's `durable_sleep(duration_ms)` always takes **milliseconds**. The `advance_time(ms)` method in the test harness also uses milliseconds. This is consistent with the WASM host ABI which uses `i64` milliseconds for all timing operations.

## Build

Compile Python workflows to WASM using the Cleat CLI:

```bash
durable build --target python ./workflow.py
```

Under the hood, this uses `componentize-py` from the [Bytecode Alliance](https://github.com/bytecodealliance/componentize-py) to compile Python to a WASM component, then wraps it in a shim that maps the Cleat ABI to the component model.

Direct `componentize-py` usage:

```bash
componentize-py -d cleat_sdk -o workflow.wasm componentize my_workflow.py
```

The pyproject.toml in this directory provides the canonical `componentize-py` configuration. Make sure the `cleat_sdk` package is importable (e.g., `pip install -e python-sdk/` or set `PYTHONPATH`).

## Requirements

- **Python 3.10+** -- the SDK uses `typing.get_type_hints` and `inspect.signature` for parameter introspection
- **componentize-py** -- required for WASM compilation (installed separately)
- **Cleat CLI** (`durable build`) -- for the full build pipeline

Optional dependencies for development:

```bash
pip install "cleat-sdk[dev]"   # pytest, pytest-cov, ruff
```

## API reference

The SDK exposes six main modules via `cleat_sdk/`:

| Module | Contents |
|--------|----------|
| `host_calls` | `HostCalls`, `SuspendSentinel`, `RetryPolicy`, `SignalResult`, `ChildResult`, `PromiseResult` |
| `entry` | `durable_entry` decorator |
| `memory` | WASM linear memory helpers and bit-packing decoders |
| `types` | `ChildWorkflow[T]`, `Saga`, `SagaStep`, `TerminalError`, `DurableDefer` |
| `client` | `CleatClient` — REST client for programmatic workflow interaction |
| `plugins` | `Plugins`, `BlobPutResult`, `BlobGetResult`, `AwaitEventResult`, `EvaluateFlagResult`, `ProduceResult`, `SendWebhookResult`, `TriggerIncidentResult`, `ResolveIncidentResult`, `SendMessageResult`, `AwaitWebhookResult` |

### `durable_entry(name=None)`

Decorator that marks a function as a Cleat workflow entry point. The decorated function must accept `HostCalls` as its first parameter. Additional parameters are deserialised from the workflow input JSON by name. The decorator handles suspend propagation (`SuspendSentinel` exception) and error serialisation.

### `HostCalls`

Core class wrapping all 36 WASM host function imports. Each method handles the pointer+length string protocol and bit-packed `i64` result decoding per the Cleat ABI. The host runtime guarantees deterministic replay — all side effects are recorded in the event history.

### Result types

- `SignalResult(name, payload, timed_out)` — returned by `await_signals`
- `ChildResult(run_id, result, error)` — returned by `await_all_children`
- `PromiseResult(result, timed_out, rejected)` — returned by `await_promise` (``rejected`` is ``True`` if the promise was rejected)
- `RetryPolicy(max_attempts, initial_interval_ms, backoff_coefficient, max_interval_ms, non_retryable_errors)` — for `durable_call_with_retry`
- `SuspendSentinel` — exception raised to signal workflow suspension
- `TerminalError` — exception raised from a saga step to trigger immediate compensation (non-retryable)

### Saga

- `Saga.add_step(step_or_name, action=None, compensate=None)` — register a step via ``SagaStep`` instance or closure-based callables
- `Saga.add_step_fn(name, action, compensate=None)` — register a step via callables that receive ``HostCalls`` (avoids closure-capture issues across WASM suspend/resume)
- `Saga.execute(terminal_exceptions=None)` — run all steps, compensating on ``TerminalError`` or any type in *terminal_exceptions*

### `CleatClient(base_url, timeout)`

External REST client for interacting with the Cleat host:

- `start_workflow(name, input, idempotency_key=None) -> str` — start a workflow, returns run ID
- `send_signal(run_id, signal_name, payload) -> None` — send a signal to a running workflow
- `resolve_promise(promise_id, value) -> None` — resolve a durable promise
- `get_query_state(run_id, key) -> Any` — read queryable workflow state
- `get_workflow_status(run_id) -> dict` — get workflow status, result, and timestamps

### `Plugins(host)`

Typed convenience wrappers for cleat plugin host functions. Provides methods for:
- Blobstore: `blobstore_put(key, data, ...)`, `blobstore_get(key)`
- Event Triggers: `await_event(event_type, timeout_ms)`
- Feature Flags: `evaluate_flag(key, context)`
- Kafka: `produce(config_id, value, key, headers)`
- Notifications: `send_webhook(webhook_id, event_type, payload)`
- PagerDuty: `trigger_incident(...)`, `resolve_incident(...)`
- Slack: `send_message(config_id, text, channel, blocks)`
- Webhook Ingestion: `await_webhook(source_id, event_type)`

## Further reading

- [Cleat project README](../../README.md) -- architecture, worker deployment, CLI reference, database schema
- [Cleat WASM ABI specification](../../ABI.md) -- full ABI contract, bit-packing layouts, memory layout
