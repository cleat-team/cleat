# Cleat Python SDK

Python SDK for the [Cleat](https://github.com/cleat-team/cleat) durable execution framework. Write workflows in Python, compile to WASM, and run on the Cleat engine.

```python
from cleat_sdk import HostCalls, cleat_entry

@cleat_entry("my_workflow")
def my_workflow(h: HostCalls, name: str) -> str:
    resp = h.call("greeter", "Greet", {"name": name})
    return resp
```

`@cleat_entry("my_workflow")` names the workflow explicitly, and **the parenthesised form is the preferred one** — `cleat_entry`'s own comment in `cleat_sdk/entry.py` calls the bare `@cleat_entry` *legacy*. The bare form is still accepted: with no argument the decorator falls through to the function's own name, which is the same string this spelling passes. Every example below uses the parenthesised form.

## Installation

Install into a virtual environment. A current Debian or Ubuntu refuses a global
`pip install` (PEP 668, `externally-managed-environment`), and `python3 -m venv`
there needs `sudo apt install python3-venv` first.

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install cleat-sdk 'componentize-py>=0.12.0'
```

Quote the requirement. Unquoted, `>=0.12.0` is a shell redirect: it creates a file
named `=0.12.0` and installs `componentize-py` with no constraint.

Or from source:

```bash
cd python-sdk/
pip install -e .
```

The package ships the WIT and the build script, so with the `cleat` CLI this is
everything `cleat build --target python` needs: no checkout of this repository.
(`cleat-sdk` 0.5.0 on PyPI predates that and ships neither.) For a complete
walk-through from an empty directory to a workflow that has run, see the
[Python quick start](../docs/tutorials/python-quick-start.md).

## Quick start

Define a workflow decorated with `@cleat_entry`. The framework injects a `HostCalls` instance as the first argument; additional arguments are deserialised from JSON input automatically.

```python
from dataclasses import dataclass
from cleat_sdk import HostCalls, cleat_entry

@dataclass
class GreetingRequest:
    name: str
    language: str = "en"

@cleat_entry("hello_workflow")
def hello_workflow(h: HostCalls, request: GreetingRequest) -> str:
    h.log(f"Hello workflow started for {request.name}")
    response = h.call(
        "greeter", "Greet",
        {"name": request.name, "language": request.language}
    )
    h.log(f"Got response: {response}")
    return response
```

The input is a JSON object keyed by the parameter names, so `my_workflow(h, name: str)` from the first snippet is started with `{"input": {"name": "Ada"}}`. A bare string (`{"input": "Ada"}`) is refused with `type: want object, got string`. Return a JSON object (a `dict`), not a bare string. The [Python quick start](../docs/tutorials/python-quick-start.md) runs one end to end.

The decorator generates a WASM export wrapper conforming to the Cleat ABI (`(args_ptr, args_len, out_ptr, max_out_len) -> i64`). On replay, completed calls return cached results instead of re-executing.

## HostCalls overview

The `HostCalls` class binds 48 WASM host function imports, grouped by category below.
The number is derived, not counted by hand -- see "Deriving the surface" at the end
of this file, because it was 36 here for long enough to be copied into three more
places.

### Workflow Identity
- `current_workflow_id() -> str` -- the current workflow's unique ID
- `current_run_id() -> str` -- the current run's unique ID

### Time & Random
- `now() -> int` -- wall-clock time in ms since epoch
- `random() -> int` -- deterministic random value (same on replay)
- `version() -> int` -- workflow definition version
- `min_version() -> int` -- minimum supported version

### Durable Execution
- `call(service, operation, request, timeout_ms=None) -> str` -- recorded API call
- `call_typed(service, operation, request, result_type) -> T` -- typed variant with automatic JSON deserialisation
- `call_with_retry(service, operation, request, retry) -> str` -- server-side retry, `retry` is a `RetryPolicy`
- `call_with_heartbeat(service, operation, request, heartbeat_interval_ms) -> str` -- long-running call that reports liveness
- `sleep(timeout_seconds) -> bool` -- suspend for a duration **in seconds** (survives restarts); `sleep_ms(timeout_ms)` is the milliseconds form
- `log(message) -> None` -- emit a log message
- `fetch(url, method="GET", headers=None, body="") -> (body, status)` -- durable HTTP fetch via host
- `send(service, operation, request) -> None` -- fire-and-forget (no response)
- `schedule_invoke(service, operation, request, delay_ms) -> None` -- delayed one-shot invocation

### Signals & Events
- `await_signals(signal_names, timeout_seconds) -> SignalResult` -- wait for external signals; `0` waits indefinitely, and `await_signals_ms` is the milliseconds form
- `poll_signal(name) -> (payload, found)` -- non-blocking signal check
- `poll_cancellation() -> (cancelled, reason)` -- check for cancellation

### Child Workflows
- `child_workflow(name, input) -> str` -- start child workflow, returns run ID
- `await_child(run_id) -> str` -- await a single child, returns result JSON
- `await_all_children(run_ids) -> list[ChildResult]` -- await multiple children concurrently

### State
- `set_query_state(key, value) -> None` -- set queryable state key-value pair

### Promises
- `create_promise(name, ttl_ms=None) -> str` -- create a durable promise, returns promise ID
- `await_promise(promise_id, timeout_seconds) -> PromiseResult` -- await a promise with timeout; `await_promise_ms` is the milliseconds form
- `resolve_promise(promise_id, value) -> None` -- resolve a promise from within the workflow
- `reject_promise(promise_id, error) -> None` -- reject a promise from within the workflow

### Update Handlers
- `register_update_handler(name, handler, validator=None) -> None` -- register update handler (bi-directional RPC)

There is no `register_query_handler`: it was removed 2026-08-09 because
nothing ever routed an external query to a registered handler -- it worked
only inside this SDK's own test harness. Use `set_query_state` to publish
state that any caller can read via `GET /api/workflows/:id/query?key=X`.

### Lifecycle
- `defer(description) -> str` -- register cleanup to run on exit, returns defer ID
- `continue_as_new(input) -> None` -- start a fresh run with new input
- `run_detached(name, input_json) -> None` -- start a workflow that outlives this one

### Plugin Calls
- `plugin_call(plugin_name, function_name, input) -> str` -- call a host plugin function

### `h.log` vs print/stdout

`log()` writes to the **workflow event history** -- it is deterministic, recorded, and replayed. Messages appear in the workflow's event log and are visible through the Cleat monitoring UI, even when replaying from history.

`print()` / `stdout` writes to the **host console** -- it is non-deterministic and **not** replayed. During replay, `print()` output will only appear if the workflow code actually executes (e.g., cached results skip execution entirely).

```python
@cleat_entry("my_workflow")
def my_workflow(h: HostCalls, name: str) -> str:
    # WRONG: stdout output is not deterministic, not visible in replay
    print(f"Processing {name}")

    # CORRECT: goes to the worker's logger, tagged with the workflow id, and
    # suppressed on replay so a resumed run does not re-emit it.
    # NOT recorded in event history -- see cleat#1308.
    h.log(f"Processing {name}")

    result = h.call("service", "Op", {"name": name})

    # WRONG: stdout is not replayed
    print(f"Result: {result}")

    # CORRECT: log is replayed deterministically
    h.log(f"Result: {result}")
    return result
```

**When to use each:**

| Use Case | Method | Reason |
|----------|--------|--------|
| Workflow-visible logging | `log()` | Survives replay, recorded in history |
| Debugging during development | `print()` | Immediate stdout output, no host call overhead |
| Structured observability | `log_kv()` | Key-value logging, recorded in history |
| External logging pipeline | `plugin_call("logger", "send", ...)` | Forward events to an external system |

Use `log` for messages that should appear in the workflow event history. Use `print` sparingly for development-time debugging only.

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
@cleat_entry("my_workflow")
def my_workflow(h: HostCalls, name: str) -> str:
    h.log(f"Processing {name}")
    result = h.call("service", "Op", {"name": name})
    return result

# WRONG: async is not supported
# @cleat_entry("my_workflow")
# async def my_workflow(h: HostCalls, name: str) -> str:  ...  # won't work
```

When a workflow calls `sleep()` or `await_signals()` on a fresh execution, the host suspends the workflow. On replay, the same calls return cached results without suspending. The user code never sees the suspend/resume cycle -- it is handled by the `@cleat_entry` wrapper.

### Converting async Python to synchronous WASM code

When porting async Python code to Cleat, every `async def` / `await` pattern must be replaced with synchronous equivalents.

**Before (async -- will not compile to WASM):**
```python
import asyncio
import aiohttp

async def fetch_data(url: str) -> dict:
    async with aiohttp.ClientSession() as session:
        async with session.get(url) as resp:
            return await resp.json()

async def process_order(order_id: str) -> dict:
    data = await fetch_data(f"https://api.example.com/orders/{order_id}")
    await asyncio.sleep(5)
    return {"order_id": order_id, "data": data}
```

**After (Cleat synchronous -- compiles to WASM):**
```python
from cleat_sdk import HostCalls, cleat_entry
import json

def fetch_data(h: HostCalls, url: str) -> dict:
    response, status = h.fetch(url, "GET")
    return json.loads(response)

@cleat_entry("process_order")
def process_order(h: HostCalls, order_id: str) -> dict:
    data = fetch_data(h, f"https://api.example.com/orders/{order_id}")
    h.sleep(5)              # instead of asyncio.sleep(5)
    return {"order_id": order_id, "data": data}
```

**Key conversion rules:**

| Async pattern | Cleat synchronous replacement |
|---------------|-------------------------------|
| `async def fn()` | `def fn()` |
| `await call()` | `call()` (synchronous) |
| `asyncio.sleep(n)` | `h.sleep(n)` |
| `aiohttp.ClientSession()` | `h.fetch()` or `h.call()` |
| `asyncio.gather(*tasks)` | `h.await_all_children(run_ids)` or sequential calls |
| `asyncio.wait_for(coro, timeout)` | `h.await_signals(names, timeout_seconds)` or `h.await_promise(id, timeout_seconds)` |
| `async for` (stream) | `h.plugin_call_streaming()` (iterator-based) |
| `async with` (context manager) | Regular `with` statement |

All Cleat workflows are synchronous. The host handles suspension and resumption transparently through the `SuspendSentinel` mechanism.

## Python Standard Library Compatibility

When compiled to WASM via `componentize-py`, **only a subset of the Python standard library is available**. The following tables document the compatibility status of commonly used modules.

### Fully available

These modules work without restrictions in WASM:

| Module | Notes |
|--------|-------|
| `json` | JSON encoding/decoding (heavily used by the SDK) |
| `math` | Mathematical functions |
| `datetime` | Date and time types (basic usage) |
| `collections` / `collections.abc` | Container data types |
| `typing` / `typing_extensions` | Type annotations |
| `dataclasses` | Data class definitions (preferred for input/output types) |
| `hashlib` | Cryptographic hash functions |
| `functools` | Decorators and higher-order functions |
| `itertools` | Iterator tools |
| `enum` | Enumerations |
| `decimal` | Decimal arithmetic |
| `uuid` | UUID generation |
| `copy` | Shallow/deep copy |
| `heapq` | Heap queue algorithm |
| `bisect` | Array bisection algorithm |
| `base64` | Base64 encoding/decoding |
| `struct` | Pack/unpack binary data (limited to basic types) |
| `abc` | Abstract base classes |
| `contextlib` | Context manager utilities |

### Limited availability

These modules are partially available with documented restrictions:

| Module | Status | Restrictions |
|--------|--------|--------------|
| `os` | ⚠️ Limited | `os.environ.get()`, `os.name` work. No file I/O (`os.open`, `os.listdir`), no `os.fork`, no `os.pipe` |
| `sys` | ⚠️ Limited | `sys.version`, `sys.platform`, `sys.getsizeof()` work. No `sys.stdin`, `sys.stdout`, `sys.stderr` access |
| `re` | ⚠️ Limited | Basic matching works. Backreferences and lookahead/lookbehind may fail in some builds |
| `pathlib` | ⚠️ Limited | Path manipulation works. No real filesystem I/O (`read_text`, `write_text`, etc.) |
| `random` | ⚠️ Limited | Basic PRNG works. Use `h.random()` for deterministic workflow randomness instead |
| `json` | ✅ Fully available, see above | |
| `time` | ⚠️ Limited | `time.time()` works. `time.sleep()` is not available (use `h.sleep()`) |

### Not available

These modules will **fail at import time or runtime** in WASM:

| Module | Reason |
|--------|--------|
| `asyncio` | Async/await not supported in WASM CPython |
| `threading` | No thread support in WASM |
| `multiprocessing` | No process support |
| `socket` | No raw socket access |
| `subprocess` | No process spawning |
| `signal` | No OS signal handling |
| `select` / `selectors` | No I/O multiplexing |
| `mmap` | No memory mapping |
| `ctypes` / `cffi` | No native code interop |
| `ssl` | No TLS/SSL (use host's `call` instead) |
| `http.client` / `urllib` | No outbound HTTP (use host calls instead) |
| `requests` | Third-party HTTP library (use `h.fetch()` instead) |
| `xml` / `xmlrpc` | XML parsing may be unavailable; avoid |
| `tkinter` | No GUI support in WASM |
| `concurrent.futures` | Depends on threading |
| `asyncio`-based libs | Any library depending on `asyncio` (e.g., `aiohttp`) |

### Making HTTP requests

Replace standard Python HTTP libraries with host-provided durable calls:

```python
# INSTEAD OF: import requests; requests.get(url)
# USE:
response = h.fetch(url, "GET", {}, None)

# INSTEAD OF: import urllib.request; urllib.request.urlopen(url)
# USE:
response = h.call("http", "fetch", {"url": url, "method": "GET"})
```

The host calls are recorded in the workflow history and replayed deterministically, which standard HTTP libraries cannot provide.

### No direct database access from WASM

WASM workflows **cannot make direct database connections**. Modules like `sqlite3`, `psycopg2`, `pymongo`, or any library that opens sockets or file descriptors will fail at import time or runtime in the WASM environment.

Data access must go through one of these deterministic alternatives:

1. **`call` to a service** -- Call a registered backend service that performs the database operation:

```python
# INSTEAD OF: import psycopg2; conn.execute("SELECT ...")
# USE:
response = h.call("db-service", "query", {"sql": "SELECT * FROM orders"})
```

2. **`plugin_call` to a host plugin** -- Use a host-registered plugin that provides database access:

```python
response = h.plugin_call("postgres", "query", {"sql": "SELECT * FROM orders"})
```

3. **`fetch` to a REST API** -- Call an external API that wraps database access:

```python
response, status = h.fetch("https://api.example.com/orders", "GET")
```

All three approaches are recorded in the workflow event history and survive replay, which direct database connections cannot guarantee.

## Unit Differences (Cleat vs. Other Frameworks)

The Python SDK's timeouts are in **seconds**, the same as Temporal's and DBOS's Python APIs. Each call that takes a timeout has a `_ms` variant (`sleep_ms`, `await_signals_ms`, `await_promise_ms`) for the millisecond form, and the WASM host ABI underneath uses `i64` milliseconds throughout. The two are easy to mix up, and the cost is not a failure: `h.sleep(5000)` is a legal call that suspends the workflow for about 83 minutes.

| Framework | Sleep Unit | Example |
|-----------|-----------|---------|
| **Cleat (Python)** | seconds; `sleep_ms` takes milliseconds | `h.sleep(5)` or `h.sleep_ms(5000)` |
| **Temporal** | Go: `time.Duration`, Java: `Duration`, Python: `timedelta` / seconds | `sleep(Duration.ofSeconds(5))` |
| **DBOS** | seconds | `DBOS.sleepSeconds(5)` |
| **Restate** | `Duration` / milliseconds (SDK-dependent) | `Duration.ofSeconds(5)` |

`h.call(..., timeout_ms=...)` and the `CleatTestHarness`'s `advance_time(ms)` are milliseconds, because they are named that way: read the parameter name.

**`CleatTestHarness` follows the same units** (cleat#3820). Its `sleep`, `await_signals` and `await_promise` take seconds and have `_ms` forms, as `HostCalls` does, so a test written against it behaves like the same call on a worker. Before cleat#3820 the harness took milliseconds in those three, so `h.sleep(5)` advanced its clock by 5 ms; a test written for that must change its numbers (see the upgrade note in the CHANGELOG).

Conversion examples when porting from other frameworks:

```python
# Temporal (Python): workflow.sleep(timedelta(seconds=5))
# Cleat:
h.sleep(5)

# DBOS: DBOS.sleepSeconds(5)
# Cleat:
h.sleep(5)

# Restate (Java): Duration.ofSeconds(5)
# Cleat:
h.sleep(5)
```

## Virtual Objects (Entity Workflows)

The Virtual Object pattern models long-lived stateful entities that process signals over time. Use `set_scope`/`get_scope`/`clear_scope` on `HostCalls` to scope all state operations to a specific entity instance.

### Complete entity workflow example

```python
from cleat_sdk import HostCalls, cleat_entry

@cleat_entry("counter_entity")
def counter_entity(h: HostCalls, instance_key: str) -> str:
    # Scope all state operations to this entity instance
    h.set_scope("counter", instance_key)

    # State is an ordinary local. Replay re-runs this code against the recorded
    # history, so the value is rebuilt, not stored: there is no state API to call.
    count = 0

    h.log(f"Counter {instance_key} starting at {count}")

    # Process signals in a loop (long-lived entity)
    while True:
        # Wait for the next signal
        result = h.await_signals(["increment", "reset", "get", "stop"], 0)

        if result.timed_out:
            continue

        name = result.name
        payload = result.payload

        if name == "increment":
            count += 1
            h.log(f"Counter {instance_key} incremented to {count}")

        elif name == "reset":
            count = 0
            h.log(f"Counter {instance_key} reset to 0")

        elif name == "get":
            # Return current value via query state
            h.set_query_state("value", str(count))

        elif name == "stop":
            h.log(f"Counter {instance_key} stopping at {count}")
            return f'{{"final_count": {count}}}'

    return f'{{"count": {count}}}'
```

This pattern:
- Uses `set_scope("counter", instance_key)` to isolate state per instance
- Loops on `await_signals` to stay alive across signals
- Cleans up with `clear_scope()` when switching instances or at the end
- Uses `continue_as_new()` periodically to compact history

## Signals vs Update Handlers

Cleat provides two patterns for external interaction with running workflows:

### Signal patterns (fire-and-forget)

Signals are one-way messages delivered to a workflow. The workflow polls or awaits them.

**Fire-and-forget signal** (`signal_workflow`):
```python
# From another workflow:
h.signal_workflow(target_run_id, "increment", '{"delta": 1}')
```

**Request-response with two signals.** `send_signal_and_wait` and `reply_to_signal` were
removed on 2026-09-06 (the WIT world has no interface for them, so they raise), and the
replacement is two ordinary signals, one in each direction:
```python
# Sender workflow:
h.signal_workflow(target_run_id, "approve_request",
                  f'{{"id": "req-42", "reply_to": "{h.current_run_id()}"}}')
reply = h.await_signals(["approve_reply"], 30)   # seconds; reply.timed_out if none came

# Receiver workflow:
request = h.await_signals(["approve_request"], 0)  # 0 waits indefinitely
# ... decode request.payload, read its "reply_to" run id, decide ...
h.signal_workflow(reply_to_run_id, "approve_reply", '{"approved": true}')
```

**Non-blocking signal poll** (`poll_signal`):
```python
payload, found = h.poll_signal("update")
if found:
    h.log(f"Received signal: {payload}")
```

### Update handler pattern (bi-directional RPC)

Update handlers provide synchronous request-response for external clients:

```python
@cleat_entry("approval_workflow")
def approval_workflow(h: HostCalls, input: str) -> str:
    # Register an "approve" update handler with validation
    def approve_handler(payload: str) -> str:
        # Process the update
        h.set_query_state("approved", payload)
        return '{"status": "approved"}'

    def approve_validator(payload: str) -> bool:
        data = json.loads(payload)
        return data.get("amount", 0) > 0

    h.register_update_handler("approve", approve_handler, approve_validator)

    # Workflow continues...
    result = h.await_signals(["done"], 60)
    return '{"status": "completed"}'
```

### When to use each

| Pattern | Method | Use Case |
|---------|--------|----------|
| Fire-and-forget signal | `signal_workflow` | Notifications, events without response needed |
| Request-response (two signals) | `signal_workflow` + `await_signals`, once each way | Cross-workflow RPC with a response |
| Non-blocking poll | `poll_signal` | Periodic checks without suspending |
| Update handler | `register_update_handler` | External client RPC with validation |

**Key differences:**
- Signals are recorded in event history and survive replay; update handlers execute during workflow init
- Update handlers support validators that run before the handler
- Signals support non-blocking polling (`poll_signal`); update handlers are always registered upfront
- Use signals for cross-workflow communication; use update handlers for external client interactions

## Per-call timeout limitations

Per-call timeouts are **under development** and not yet enforced on the host side during WASM execution. The `call_with_retry` host import does not accept a timeout parameter.

**Workaround:** Use `sleep` + polling for timeout-aware patterns:

```python
def call_with_timeout(
    h: HostCalls, service: str, op: str, request: Any, timeout_ms: int
) -> str:
    """Call a service with a client-side timeout using polling."""
    deadline = h.now() + timeout_ms
    last_error = None

    while h.now() < deadline:
        try:
            return h.call(service, op, request)
        except RuntimeError as e:
            last_error = e
            h.sleep(1)  # poll interval, in seconds

    raise RuntimeError(f"Call timed out after {timeout_ms}ms: {last_error}")
```

Host-side timeout enforcement is on the roadmap.

## all_handlers_finished absence

Cleat does **not** have an `all_handlers_finished` equivalent (found in Temporal for update/signal handler tracking). There is no built-in mechanism to wait until all in-flight update handlers have completed.

**Alternative patterns:**

1. **Manual handler counting** -- Count in-flight handlers in a local the handlers close over:

```python
@cleat_entry("tracked_workflow")
def tracked_workflow(h: HostCalls, input: str) -> str:
    pending_handlers = 0

    def handler(payload: str) -> str:
        nonlocal pending_handlers
        pending_handlers += 1
        try:
            return h.call("service", "Op", payload)
        finally:
            pending_handlers -= 1

    h.register_update_handler("process", handler)
    # ... workflow continues; check `pending_handlers == 0` before returning ...
```

2. **Durable promises** -- External callers signal completion via promises:

```python
@cleat_entry("promise_tracked_workflow")
def promise_tracked_workflow(h: HostCalls, input: str) -> str:
    promise_id = h.create_promise("all_handlers_done")

    def handler(payload: str) -> str:
        result = h.call("service", "Op", payload)
        # Signal done via the promise
        h.resolve_promise(promise_id, "done")
        return result

    h.register_update_handler("process", handler)

    # Wait for the promise instead of all_handlers_finished
    result = h.await_promise(promise_id, 30)
    return result.result
```

For coordinating multiple concurrent operations, use `await_all_children` with child workflows or `await_signals` with a quorum pattern instead.

## Multi-export WASM modules (Python)

A single Python file can export multiple workflow entry points using `@cleat_entry`:

```python
from cleat_sdk import HostCalls, cleat_entry

@cleat_entry("place_order")
def place_order(h: HostCalls, input: str) -> str:
    return h.call("orders", "Place", input)

@cleat_entry("cancel_order")
def cancel_order(h: HostCalls, input: str) -> str:
    return h.call("orders", "Cancel", input)

@cleat_entry("get_order_status")
def get_order_status(h: HostCalls, input: str) -> str:
    return h.call("orders", "Status", input)
```

When compiled to WASM, `componentize-py` generates a named export for each decorated function. The host dispatches invocations by matching the called workflow name to the export name. Each export wrapper handles ABI marshalling, suspend/resume, and error serialization independently.

## WASM Compilation Pipeline

Compile Python workflows to WASM using `componentize-py` from the [Bytecode Alliance](https://github.com/bytecodealliance/componentize-py), which translates Python bytecode into a WASM component conforming to the Cleat WIT world.

### Prerequisites

Python 3.10 or newer, in a virtual environment, with both packages installed
([Installation](#installation)):

```bash
pip install cleat-sdk 'componentize-py>=0.12.0'
componentize-py --version
```

If `componentize-py` is `command not found`, the environment is not active, or
its `bin/` directory is not on `PATH`.

### Compilation Command

Use the Cleat CLI. It resolves paths, runs the determinism check, derives the
entry point's input schema, and stamps `cleat.metadata`, none of which a bare
`componentize-py` invocation does:

```bash
mkdir -p out
cleat build --target python -o out ./workflow.py
```

`cleat build` runs the `python3` on `PATH` and finds the build script and the WIT
inside the installed `cleat-sdk`; inside a checkout of this repository it uses
`python-sdk/` instead. Flags go before the path.

A file with more than one `@cleat_entry` takes the entry explicitly:
`cleat build --target python -o out ./workflow.py:place_order`.

You should not need to call `componentize-py` yourself. If you must, the form for
current releases puts the WIT and the world **before** the subcommand, and names a
*module* rather than a file; `--wit-path` was removed:

```bash
componentize-py -d "$(python3 -c 'import cleat_sdk,os;print(os.path.dirname(cleat_sdk.__file__))')/wit" \
    -w cleat-workflow componentize hello -p . -o hello.wasm
```

That produces a bare component with **no** `cleat.metadata` (the name, version and
entry points `cleat build` stamps in). `python-sdk/scripts/build_wasm.py` is the
supported wrapper, and what `cleat build` runs.

### Handling Imports in WASM Context

When your workflow is compiled to WASM, `componentize-py` bundles a minimal CPython interpreter. Import behavior differs from native Python:

- **Pure-Python imports** -- Pure Python modules on the Python path work normally
- **C extension modules** -- Native code (`.so`/`.pyd`) does **not** work in WASM
- **stdlib modules** -- Only pure-Python stdlib modules are available (see [compatibility section](#python-standard-library-compatibility))
- **Third-party packages** -- Pure-Python packages (e.g., `langchain-core`) can work if they are on the Python path and do not depend on C extensions
- **Relative imports** -- Use absolute imports within your workflow file; relative imports may not resolve correctly in the WASM context

Always test your imports by compiling and running a minimal workflow before adding complex dependencies.

### Step-by-Step Example

The [Python quick start](../docs/tutorials/python-quick-start.md) builds, deploys
and runs a first workflow end to end, and is the page to follow.

### Using the Makefile

A `Makefile` in this directory is for working **on** the SDK, from a checkout. It
compiles every example workflow and stamps their metadata:

```bash
cd python-sdk/
make wasm       # Compile all example workflows to WASM
make test-wasm  # Compile and run WASM-specific tests
make clean      # Remove compiled WASM artifacts
```

See the [Makefile](./Makefile) for details.

### Troubleshooting

| Error | Likely Cause | Solution |
|-------|-------------|----------|
| `componentize-py: command not found` | Not installed in the active environment | `pip install 'componentize-py>=0.12.0'` in the environment you build from |
| `finding repo root: go.mod not found` | `cleat-sdk` 0.5.0, which ships no build script | Upgrade `cleat-sdk`, or build from a checkout |
| `has no build script` | An older `cleat-sdk` is active | `pip install --upgrade cleat-sdk` |
| `Error: unexpected argument '--wit-path' found` | A hand-written `componentize-py` command in the pre-0.13 form | Use `cleat build`, or the `-d … -w … componentize <module>` form above |
| `ModuleNotFoundError: No module named 'cleat_sdk'` | The virtual environment is not active | `. .venv/bin/activate` |
| `Error: Component model world 'cleat-workflow' not found` | WIT path does not contain the correct world | Verify WIT files; check the `world` name in `pyproject.toml` |
| `RuntimeError: async function` | Async code in workflow | Remove `async`/`await`; all Cleat workflows are synchronous |
| `Build FAILED (exit code 1)` without clear error | Try verbose mode | Add `--verbose` to see full `componentize-py` output |
| WASM binary > 25 MB | Expected for CPython-in-WASM | Normal; the runtime caches the binary after first load |
| WASM binary > 50 MB | Large or unnecessary imports | Check that imported modules are genuinely needed at runtime |
| `ImportError: ...` at WASM runtime | Module not bundled | Only pure-Python modules are available; C extensions will not work |

## Requirements

- **Python 3.10+** -- the SDK uses `typing.get_type_hints` and `inspect.signature` for parameter introspection
- **componentize-py >= 0.12.0** -- required for WASM compilation (installed via `pip install componentize-py`)
- **Cleat CLI** (`cleat build`) -- the supported way to build a workflow
- **WIT files** -- shipped inside the package (`cleat_sdk/wit/`); nothing to copy

Optional dependencies for development:

```bash
pip install "cleat-sdk[dev]"   # pytest, pytest-cov, ruff
```

## API reference

The SDK exposes six main modules via `cleat_sdk/`:

| Module | Contents |
|--------|----------|
| `host_calls` | `HostCalls`, `SuspendSentinel`, `RetryPolicy`, `SignalResult`, `ChildResult`, `PromiseResult` |
| `entry` | `cleat_entry`, `virtual_object` decorators |
| `memory` | WASM linear memory helpers and bit-packing decoders |
| `types` | `ChildWorkflow[T]`, `Saga`, `SagaStep`, `TerminalError`, `DurableDefer` |
| `test_harness` | `CleatTestHarness` — in-memory workflow test harness with mock host calls |
| `client` | `CleatClient` — REST client for programmatic workflow interaction |
| `plugins` | `Plugins`, `BlobPutResult`, `BlobGetResult`, `AwaitEventResult`, `EvaluateFlagResult`, `ProduceResult`, `SendWebhookResult`, `TriggerIncidentResult`, `ResolveIncidentResult`, `SendMessageResult`, `AwaitWebhookResult` |

### `cleat_entry(name=None)`

Decorator that marks a function as a Cleat workflow entry point. The decorated function must accept `HostCalls` as its first parameter. Additional parameters are deserialised from the workflow input JSON by name. The decorator handles suspend propagation (`SuspendSentinel` exception) and error serialisation.

### `virtual_object(name=None)`

Decorator similar to `@cleat_entry` but for entity workflows (virtual objects). Like `cleat_entry`, the decorated function must accept `HostCalls` as its first parameter. Sets `_is_virtual_object = True` for introspection by the build tooling.

### `HostCalls`

Core class binding the 48 WASM host function imports (derived -- see "Deriving the
surface"). Each method handles the pointer+length string protocol and bit-packed `i64` result decoding per the Cleat ABI. The host runtime guarantees deterministic replay — all side effects are recorded in the event history.

### Result types

- `SignalResult(name, payload, timed_out)` — returned by `await_signals`
- `ChildResult(run_id, result, error)` — returned by `await_all_children`
- `PromiseResult(result, timed_out, rejected)` — returned by `await_promise` (``rejected`` is ``True`` if the promise was rejected)
- `RetryPolicy(max_attempts, initial_interval_ms, backoff_coefficient, max_interval_ms, non_retryable_errors)` — for `call_with_retry`
- `SuspendSentinel` — exception raised to signal workflow suspension
- `TerminalError` — exception raised from a saga step to trigger immediate compensation (non-retryable)
- `CleatCallError` — raised when a durable call fails, with `CleatCallTimeoutError`,
  `CleatCallTransientError` and `CleatCallPermanentError` selected by the host's error code

#### A failed call raises; it does not come back as a response

`call`, `call_with_retry`, `call_with_heartbeat`, `child_workflow`,
`child_workflow_with_options`, `plugin_call` and `plugin_call_streaming` return
`result<string, call-failure>` in `wit/cleat.wit`, which the generated bindings lift into
"return the response, or raise". So a failure reaches you as a `CleatCallError`, and the
engine stopping your workflow reaches you as `SuspendSentinel` — neither can arrive as a
string, whatever the service on the other end returns.

This changed in IMPROVEMENT-PLAN 3.110. Before it, these returned a bare `string`, and both
cases arrived as ordinary successful responses: a failure as the error text, and a stop as
`""`. **If your workflow inspected a response for an error marker, delete that check** — the
`"__CLEAT_ERROR__:"` prefix the SDK looked for had no producer in the host and never appeared.

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

### `CleatTestHarness()`

A `HostCalls` subclass for unit-testing a workflow without WASM, a worker or a database. It records every call, answers from stubs you register first, and keeps a simulated clock. It is not the worker: it does not replay, so it checks what a workflow does with the answers it is given, not durability.

A function decorated with `@cleat_entry` is the WASM export wrapper, which takes the raw input JSON, not `(h, ...)`. The function you wrote is `.__wrapped__`: call that with the harness.

```python
from cleat_sdk import HostCalls, cleat_entry, CleatTestHarness

@cleat_entry("GreetWorkflow")
def GreetWorkflow(h: HostCalls, name: str) -> str:
    h.log(f"Hello {name}")
    return h.call("greeter", "Greet", {"name": name})

def test_greet_workflow():
    harness = CleatTestHarness()
    harness.stub_call("greeter", "Greet", '{"result": "Hello Alice"}')

    result = GreetWorkflow.__wrapped__(harness, "Alice")

    assert result == '{"result": "Hello Alice"}'
    assert harness.call_count("greeter", "Greet") == 1
    assert harness.assert_called("greeter", "Greet")
```

Key members:
- `stub_call(service, operation, response="", error=None)` -- stub a `call`; stubs for one service and operation are used in the order registered, and `error` makes the call raise
- `stub_signal(signal_name, payload)` -- queue a signal for `await_signals` and `poll_signal` to deliver
- `stub_promise(promise_id, value)` and `stub_reject_promise(promise_id, error)` -- settle a promise
- `stub_child_workflow(name, result, error=None)` and `register_child_stub(name, response)` -- stub a child workflow
- `advance_time(ms)`, `set_time(epoch_ms)` and `now_ms` -- the simulated clock, in **milliseconds**; `sleep` and `await_signals` take seconds, like `HostCalls`, and advance it. `await_signals(names, 0)` with no signal queued raises `SuspendSentinel`, as a wait with no deadline suspends on a worker
- `call_count(service, operation) -> int`, `assert_called(service, operation) -> bool` and `assert_not_called(...) -> bool` -- these **return** a bool, so write `assert harness.assert_called(...)`; `last_call(service, operation)` returns the last `CallRecord` or `None`; `call_history` lists them all
- `reset()` -- clear stubs, history, clock and promises

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

## Deriving the surface

The host-call numbers in this file are derived. Do not count by hand, and do not copy
a number from another document -- on 2026-09-13 four documents agreed at 36 and the
figure was 48, because it was one number copied rather than four measurements.

```
python3 - <<'EOF'
import ast, pathlib
t = ast.parse(pathlib.Path('python-sdk/cleat_sdk/host_calls.py').read_text())
print(len({a.asname for n in ast.walk(t) if isinstance(n, ast.ImportFrom)
           for a in n.names if a.asname and a.asname.startswith('_import_')}))
EOF
```

Parse, do not grep. A regex over `_import_*` returns **52**, and three of those are
names inside comments stating the import does **not** exist:

```
# There is no _import_cleat_register_query_handler here (removed 2026-08-09).
# There is no _import_cleat_send_signal_and_wait or
# _import_cleat_reply_to_signal here (removed 2026-09-06, ...)
```

A text search cannot tell a binding from a sentence denying one.

The WIT world in `wit/cleat.wit` declares **49** functions across 17 imported
interfaces. The SDK binds 48 of them; the unbound one is
`durable-register-query-handler`, matching the removal note above.
