# Cleat Python SDK

Python SDK for the [Cleat](https://github.com/rcownie/cleat) durable execution framework. Write workflows in Python, compile to WASM, and run on the Cleat engine.

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

### durable_log vs print/stdout

`durable_log()` writes to the **workflow event history** -- it is deterministic, recorded, and replayed. Messages appear in the workflow's event log and are visible through the Cleat monitoring UI, even when replaying from history.

`print()` / `stdout` writes to the **host console** -- it is non-deterministic and **not** replayed. During replay, `print()` output will only appear if the workflow code actually executes (e.g., cached results skip execution entirely).

```python
@durable_entry
def my_workflow(h: HostCalls, name: str) -> str:
    # WRONG: stdout output is not deterministic, not visible in replay
    print(f"Processing {name}")

    # CORRECT: recorded in event history, visible in replay
    h.durable_log(f"Processing {name}")

    result = h.durable_call("service", "Op", {"name": name})

    # WRONG: stdout is not replayed
    print(f"Result: {result}")

    # CORRECT: durable_log is replayed deterministically
    h.durable_log(f"Result: {result}")
    return result
```

**When to use each:**

| Use Case | Method | Reason |
|----------|--------|--------|
| Workflow-visible logging | `durable_log()` | Survives replay, recorded in history |
| Debugging during development | `print()` | Immediate stdout output, no host call overhead |
| Structured observability | `log_kv()` | Key-value logging, recorded in history |
| External logging pipeline | `plugin_call("logger", "send", ...)` | Forward events to an external system |

Use `durable_log` for messages that should appear in the workflow event history. Use `print` sparingly for development-time debugging only.

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
from cleat_sdk import HostCalls, durable_entry
import json

def fetch_data(h: HostCalls, url: str) -> dict:
    response, status = h.durable_fetch(url, "GET")
    return json.loads(response)

@durable_entry
def process_order(h: HostCalls, order_id: str) -> dict:
    data = fetch_data(h, f"https://api.example.com/orders/{order_id}")
    h.durable_sleep(5000)           # instead of asyncio.sleep(5)
    return {"order_id": order_id, "data": data}
```

**Key conversion rules:**

| Async pattern | Cleat synchronous replacement |
|---------------|-------------------------------|
| `async def fn()` | `def fn()` |
| `await call()` | `call()` (synchronous) |
| `asyncio.sleep(n)` | `h.durable_sleep(n * 1000)` |
| `aiohttp.ClientSession()` | `h.durable_fetch()` or `h.durable_call()` |
| `asyncio.gather(*tasks)` | `h.await_all_children(run_ids)` or sequential calls |
| `asyncio.wait_for(coro, timeout)` | `h.await_signals(names, timeout_ms)` or `h.await_promise(id, timeout_ms)` |
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
| `time` | ⚠️ Limited | `time.time()` works. `time.sleep()` is not available (use `h.durable_sleep()`) |

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
| `ssl` | No TLS/SSL (use host's `durable_call` instead) |
| `http.client` / `urllib` | No outbound HTTP (use host calls instead) |
| `requests` | Third-party HTTP library (use `h.durable_fetch()` instead) |
| `xml` / `xmlrpc` | XML parsing may be unavailable; avoid |
| `tkinter` | No GUI support in WASM |
| `concurrent.futures` | Depends on threading |
| `asyncio`-based libs | Any library depending on `asyncio` (e.g., `aiohttp`) |

### Making HTTP requests

Replace standard Python HTTP libraries with host-provided durable calls:

```python
# INSTEAD OF: import requests; requests.get(url)
# USE:
response = h.durable_fetch(url, "GET", {}, None)

# INSTEAD OF: import urllib.request; urllib.request.urlopen(url)
# USE:
response = h.durable_call("http", "fetch", {"url": url, "method": "GET"})
```

The host calls are recorded in the workflow history and replayed deterministically, which standard HTTP libraries cannot provide.

### No direct database access from WASM

WASM workflows **cannot make direct database connections**. Modules like `sqlite3`, `psycopg2`, `pymongo`, or any library that opens sockets or file descriptors will fail at import time or runtime in the WASM environment.

Data access must go through one of these deterministic alternatives:

1. **`durable_call` to a service** -- Call a registered backend service that performs the database operation:

```python
# INSTEAD OF: import psycopg2; conn.execute("SELECT ...")
# USE:
response = h.durable_call("db-service", "query", {"sql": "SELECT * FROM orders"})
```

2. **`plugin_call` to a host plugin** -- Use a host-registered plugin that provides database access:

```python
response = h.plugin_call("postgres", "query", {"sql": "SELECT * FROM orders"})
```

3. **`durable_fetch` to a REST API** -- Call an external API that wraps database access:

```python
response, status = h.durable_fetch("https://api.example.com/orders", "GET")
```

All three approaches are recorded in the workflow event history and survive replay, which direct database connections cannot guarantee.

## Unit Differences (Cleat vs. Other Frameworks)

Cleat uses **milliseconds** for all time-related host calls. This is important when porting workflows from other frameworks:

| Framework | Sleep Unit | Example |
|-----------|-----------|---------|
| **Cleat** | milliseconds | `durable_sleep(5000)` = 5 seconds |
| **Temporal** | Go: `time.Duration`, Java: `Duration`, Python: `timedelta` / seconds | `sleep(Duration.ofSeconds(5))` |
| **DBOS** | seconds | `DBOS.sleepSeconds(5)` |
| **Restate** | `Duration` / milliseconds (SDK-dependent) | `Duration.ofSeconds(5)` |

Cleat's `durable_sleep(duration_ms)` always takes **milliseconds**. The `advance_time(ms)` method in the test harness also uses milliseconds. This is consistent with the WASM host ABI which uses `i64` milliseconds for all timing operations.

Conversion examples when porting from other frameworks:

```python
# Temporal (Python): workflow.sleep(timedelta(seconds=5))
# Cleat:
h.durable_sleep(5000)  # 5 seconds in ms

# DBOS: DBOS.sleepSeconds(5)
# Cleat:
h.durable_sleep(5_000)

# Restate (Java): Duration.ofSeconds(5)
# Cleat:
h.durable_sleep(5_000)

# Helper pattern for readability:
def sleep_seconds(h: HostCalls, secs: int) -> None:
    """Sleep for the given number of seconds."""
    h.durable_sleep(secs * 1000)

sleep_seconds(h, 5)  # 5 seconds
```

## Virtual Objects (Entity Workflows)

The Virtual Object pattern models long-lived stateful entities that process signals over time. Use `set_scope`/`get_scope`/`clear_scope` on `HostCalls` to scope all state operations to a specific entity instance.

### Complete entity workflow example

```python
from cleat_sdk import HostCalls, durable_entry

@durable_entry
def counter_entity(h: HostCalls, instance_key: str) -> str:
    # Scope all state operations to this entity instance
    h.set_scope("counter", instance_key)

    # Initialize or restore state
    count = 0
    if h.has_state("count"):
        count = int(h.get_state("count", int))

    h.durable_log(f"Counter {instance_key} starting at {count}")

    # Process signals in a loop (long-lived entity)
    while True:
        # Wait for the next signal
        result = h.await_signals(["increment", "reset", "get", "stop"], None)

        if result.timed_out:
            continue

        name = result.name
        payload = result.payload

        if name == "increment":
            count = h.incr_state("count", 1)
            h.durable_log(f"Counter {instance_key} incremented to {count}")

        elif name == "reset":
            h.set_state("count", 0)
            count = 0
            h.durable_log(f"Counter {instance_key} reset to 0")

        elif name == "get":
            # Return current value via query state
            h.set_query_state("value", str(count))

        elif name == "stop":
            h.durable_log(f"Counter {instance_key} stopping at {count}")
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

**Request-response with signals** (`send_signal_and_wait` / `reply_to_signal`):
```python
# Sender workflow:
response = h.send_signal_and_wait(
    target_run_id, "approve_request",
    '{"id": "req-42"}', timeout_ms=30000
)

# Receiver workflow (inside signal handler):
result = h.await_signals(["approve_request"], None)
# Parse correlation ID from payload and respond:
h.reply_to_signal(correlation_id, '{"approved": true}')
```

**Non-blocking signal poll** (`poll_signal`):
```python
payload, found = h.poll_signal("update")
if found:
    h.durable_log(f"Received signal: {payload}")
```

### Update handler pattern (bi-directional RPC)

Update handlers provide synchronous request-response for external clients:

```python
@durable_entry
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
    result = h.await_signals(["done"], 60000)
    return '{"status": "completed"}'
```

### When to use each

| Pattern | Method | Use Case |
|---------|--------|----------|
| Fire-and-forget signal | `signal_workflow` | Notifications, events without response needed |
| Request-response (signal) | `send_signal_and_wait` / `reply_to_signal` | Cross-workflow RPC with a response |
| Non-blocking poll | `poll_signal` | Periodic checks without suspending |
| Update handler | `register_update_handler` | External client RPC with validation |

**Key differences:**
- Signals are recorded in event history and survive replay; update handlers execute during workflow init
- Update handlers support validators that run before the handler
- Signals support non-blocking polling (`poll_signal`); update handlers are always registered upfront
- Use signals for cross-workflow communication; use update handlers for external client interactions

## Per-call timeout limitations

Per-call timeouts are **under development** and not yet enforced on the host side during WASM execution. The `durable_call_with_retry` host import does not accept a timeout parameter.

**Workaround:** Use `durable_sleep` + polling for timeout-aware patterns:

```python
def call_with_timeout(
    h: HostCalls, service: str, op: str, request: Any, timeout_ms: int
) -> str:
    """Call a service with a client-side timeout using polling."""
    deadline = h.now() + timeout_ms
    last_error = None

    while h.now() < deadline:
        try:
            return h.durable_call(service, op, request)
        except RuntimeError as e:
            last_error = e
            h.durable_sleep(1000)  # poll interval

    raise RuntimeError(f"Call timed out after {timeout_ms}ms: {last_error}")
```

Host-side timeout enforcement is on the roadmap.

## all_handlers_finished absence

Cleat does **not** have an `all_handlers_finished` equivalent (found in Temporal for update/signal handler tracking). There is no built-in mechanism to wait until all in-flight update handlers have completed.

**Alternative patterns:**

1. **Manual handler counting** -- Track handler invocations with state keys:

```python
@durable_entry
def tracked_workflow(h: HostCalls, input: str) -> str:
    h.set_state("pending_handlers", 0)

    def handler(payload: str) -> str:
        h.incr_state("pending_handlers", 1)
        try:
            result = h.durable_call("service", "Op", payload)
            return result
        finally:
            h.incr_state("pending_handlers", -1)

    h.register_update_handler("process", handler)
    # ... workflow continues ...
```

2. **Durable promises** -- External callers signal completion via promises:

```python
@durable_entry
def promise_tracked_workflow(h: HostCalls, input: str) -> str:
    promise_id = h.create_promise("all_handlers_done")

    def handler(payload: str) -> str:
        result = h.durable_call("service", "Op", payload)
        # Signal done via the promise
        h.resolve_promise(promise_id, "done")
        return result

    h.register_update_handler("process", handler)

    # Wait for the promise instead of all_handlers_finished
    result = h.await_promise(promise_id, 30000)
    return result.result
```

For coordinating multiple concurrent operations, use `await_all_children` with child workflows or `await_signals` with a quorum pattern instead.

## Multi-export WASM modules (Python)

A single Python file can export multiple workflow entry points using `@durable_entry`:

```python
from cleat_sdk import HostCalls, durable_entry

@durable_entry
def place_order(h: HostCalls, input: str) -> str:
    return h.durable_call("orders", "Place", input)

@durable_entry
def cancel_order(h: HostCalls, input: str) -> str:
    return h.durable_call("orders", "Cancel", input)

@durable_entry
def get_order_status(h: HostCalls, input: str) -> str:
    return h.durable_call("orders", "Status", input)
```

When compiled to WASM, `componentize-py` generates a named export for each decorated function. The host dispatches invocations by matching the called workflow name to the export name. Each export wrapper handles ABI marshalling, suspend/resume, and error serialization independently.

## WASM Compilation Pipeline

Compile Python workflows to WASM using `componentize-py` from the [Bytecode Alliance](https://github.com/bytecodealliance/componentize-py), which translates Python bytecode into a WASM component conforming to the Cleat WIT world.

### Prerequisites

Install the required tools:

```bash
# Install componentize-py (requires Python 3.10+)
pip install componentize-py>=0.12.0

# Install the Cleat Python SDK (ensures cleat_sdk is importable)
pip install cleat-sdk
# Or from source:
# cd python-sdk/ && pip install -e .
```

Verify the installation:

```bash
componentize-py --version
# Expected output: componentize-py 0.x.y
```

If you encounter `command not found`, ensure your Python Scripts/bin directory is on `PATH`:

```bash
# Unix/macOS:
export PATH="$HOME/.local/bin:$PATH"
# Windows:
# pip install --user will add to %APPDATA%\Python\Scripts
```

### Configuration (pyproject.toml)

The `componentize-py` tool reads WIT configuration from `pyproject.toml` under the `[tool.componentize-py]` table. The canonical configuration for this SDK is:

```toml
[tool.componentize-py]
wit_path = "wit/"
world = "cleat-workflow"
```

- `wit_path` -- directory containing the Cleat WIT files (`.wit` files defining the WASM component interface)
- `world` -- the WIT world your workflow component implements (must match what `componentize-py` expects)

If you are writing a workflow outside this repository, copy the `wit/` directory from the SDK or install the SDK as a dependency so the WIT files are discoverable.

### Compilation Command

The basic `componentize-py` invocation (modern syntax):

```bash
componentize-py componentize my_workflow.py \
  --wit-path wit/ \
  --world cleat-workflow \
  -o my_workflow.wasm
```

Required flags:

| Flag | Purpose |
|------|---------|
| `componentize <file>` | Subcommand; the Python file containing your `@durable_entry` workflow function |
| `--wit-path <dir>` | Path to the directory with the Cleat WIT files (`cleat.wit`) |
| `--world <name>` | The WIT world to implement (e.g. `cleat-workflow`) |
| `-o <file>` | Output WASM file path |

The SDK directory must be on `PYTHONPATH` so `cleat_sdk` is importable:

```bash
export PYTHONPATH=/path/to/python-sdk:$PYTHONPATH
componentize-py componentize my_workflow.py --wit-path wit/ --world cleat-workflow -o my_workflow.wasm
```

The Cleat CLI provides a convenience wrapper that handles paths automatically:

```bash
cleat build --target python ./workflow.py
```

### Handling Imports in WASM Context

When your workflow is compiled to WASM, `componentize-py` bundles a minimal CPython interpreter. Import behavior differs from native Python:

- **Pure-Python imports** -- Pure Python modules on the Python path work normally
- **C extension modules** -- Native code (`.so`/`.pyd`) does **not** work in WASM
- **stdlib modules** -- Only pure-Python stdlib modules are available (see [compatibility section](#python-standard-library-compatibility))
- **Third-party packages** -- Pure-Python packages (e.g., `langchain-core`) can work if they are on the Python path and do not depend on C extensions
- **Relative imports** -- Use absolute imports within your workflow file; relative imports may not resolve correctly in the WASM context

Always test your imports by compiling and running a minimal workflow before adding complex dependencies.

### Step-by-Step Example

Compile the `hello_workflow.py` example to WASM:

```bash
# 1. Install dependencies
pip install componentize-py>=0.12.0
pip install -e python-sdk/

# 2. Verify the WIT directory exists
ls python-sdk/wit/cleat.wit

# 3. Compile the workflow
cd python-sdk/
componentize-py componentize examples/hello_workflow.py \
  --wit-path wit/ \
  --world cleat-workflow \
  -o hello_workflow.wasm

# 4. Verify the output
ls -lh hello_workflow.wasm
# Expected: hello_workflow.wasm (~5-15 MB for CPython + workflow code)

# 5. Inspect the WASM exports (requires wasm-tools or wasm-objdump)
wasm-tools dump hello_workflow.wasm | head -20
# Or:
wasm-objdump -x hello_workflow.wasm | grep Export
```

The output WASM file can be loaded by the Cleat worker runtime. The compiled component exports entry points that dispatch to your `@durable_entry`-decorated functions.

### Using the Makefile

A `Makefile` in the SDK root provides convenience targets:

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
| `componentize-py: command not found` | Not installed or not on `PATH` | `pip install componentize-py`; verify `PATH` includes `~/.local/bin` |
| `Error: No such file or directory: 'wit/...'` | WIT path is incorrect | Pass `--wit-path` pointing to the directory with `cleat.wit` |
| `ModuleNotFoundError: No module named 'cleat_sdk'` | SDK not on Python path | `pip install -e python-sdk/` or set `PYTHONPATH` |
| `Error: Component model world 'cleat-workflow' not found` | WIT path does not contain the correct world | Verify WIT files; check the `world` name in `pyproject.toml` |
| `RuntimeError: async function` | Async code in workflow | Remove `async`/`await`; all Cleat workflows are synchronous |
| `Build FAILED (exit code 1)` without clear error | Try verbose mode | Add `--verbose` to see full `componentize-py` output |
| WASM binary > 25 MB | Expected for CPython-in-WASM | Normal; the runtime caches the binary after first load |
| WASM binary > 50 MB | Large or unnecessary imports | Check that imported modules are genuinely needed at runtime |
| `ImportError: ...` at WASM runtime | Module not bundled | Only pure-Python modules are available; C extensions will not work |

## Requirements

- **Python 3.10+** -- the SDK uses `typing.get_type_hints` and `inspect.signature` for parameter introspection
- **componentize-py >= 0.12.0** -- required for WASM compilation (installed via `pip install componentize-py`)
- **Cleat CLI** (`cleat build`) -- for the full build pipeline (optional; use `componentize-py` directly as shown above)
- **WIT files** -- the `wit/` directory defining the Cleat component model interface

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
