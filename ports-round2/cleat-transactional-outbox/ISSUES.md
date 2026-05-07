# Issues Encountered During Porting

Each issue is numbered and includes severity, the exact code that caused
the problem, expected behaviour, workaround, and recommendation.

---

## Issue #1: `@durable_entry` Decorator Wraps the Original Function, Breaking Direct Invocation

- **Severity:** Critical
- **Category:** SDK design

### Description

The `@durable_entry(name)` decorator at
`python-sdk/cleat_sdk/entry.py:54` wraps the decorated function in a WASM
export wrapper.  The wrapper has signature:

```python
def export_wrapper(args_ptr: int, args_len: int, out_ptr: int, max_out_len: int) -> int
```

This makes it impossible to call the decorated function directly with
normal Python arguments for testing or local development:

```python
@durable_entry("place_order")
def place_order_workflow(h: HostCalls, customer: str, item: str, quantity: int) -> str:
    ...

# This DOES NOT work — the wrapper expects WASM memory pointers:
place_order_workflow(h, "Alice", "Widget", 2)
# TypeError: export_wrapper() missing 2 required positional arguments
```

### Expected Behaviour

The original function should remain callable directly.  The WASM export
wrapper should only affect the ABI layer, not hide the original function.

### Workaround

The decorator uses `@functools.wraps(func)` in `entry.py:108`, which sets
`export_wrapper.__wrapped__ = func`.  The original function can be
accessed via `__wrapped__`:

```python
place_order_workflow.__wrapped__(h, customer="Alice", item="Widget", quantity=2)
```

This is fragile — `__wrapped__` is an implementation detail of
`functools.wraps` and could be overwritten by other decorators.

### Recommendation

Add a public attribute (e.g. `export_wrapper._original_func = func` or
`export_wrapper.workflow_func = func`) so consumers can reliably access
the original function.  Alternatively, have the decorator return the
original function unchanged when the SDK is not running in a WASM
runtime (detectable via a module-level flag).

**Affected file:** `python-sdk/cleat_sdk/entry.py`, lines 108-166.

---

## Issue #2: No Equivalent to DBOS `set_event` / `get_event`

- **Severity:** High
- **Category:** Missing API

### Description

The original DBOS app uses `DBOS.set_event(key, value)` in the workflow
and `DBOS.get_event(handle.workflow_id, key)` in the HTTP handler.  This
is a blocking, bidirectional communication channel: the API starts the
workflow asynchronously, the workflow emits an event (the `order_id`),
and the API blocks on `get_event` until the event arrives.

Cleat has `HostCalls.set_query_state(key, value)` which stores a
key-value pair accessible to external callers, but there is no
corresponding `get_query_state` that blocks until a value is available,
and no `CleatClient` method for querying query state from a running
workflow.

### DBOS Code

```python
# Workflow:
DBOS.set_event(ORDER_ID_EVENT, order_id)

# API handler:
handle = DBOS.start_workflow(place_order_workflow, ...)
order_id = DBOS.get_event(handle.workflow_id, ORDER_ID_EVENT)  # blocks
```

### Cleat Equivalent (missing)

```python
# Workflow:
h.set_query_state(ORDER_ID_STATE_KEY, str(order_id))

# API handler (hypothetical):
run_id = client.start_workflow("place_order", input_data)
order_id = client.get_query_state(run_id, "order_id")  # would need to poll
```

### Workaround

Use polling in the API layer:

```python
while time.time() < deadline:
    state = runtime.get_query_state(run_id, "order_id")
    if state:
        return int(state)
    time.sleep(0.05)
```

This is less efficient and introduces latency.  There is no guarantee
that `set_query_state` is immediately visible to external callers.

### Recommendation

Add a blocking `await_query_state(run_id, key, timeout_ms)` method to the
CleatClient and a corresponding `get_query_state(run_id, key)` endpoint
on the Cleat host REST API.  Alternatively, document whether
`set_query_state` state is immediately consistent or eventually
consistent.

---

## Issue #3: No In-Process Host Runtime for Development

- **Severity:** High
- **Category:** Developer experience

### Description

The Cleat Python SDK provides:
- `CleatTestHarness` — a mock `HostCalls` that uses stubs (great for unit
  tests, but does not execute real service code)
- `CleatClient` — communicates with a remote Cleat host via REST (requires
  a running Cleat host)

There is no "local runtime" that runs the workflow function in-process
while routing `durable_call` to actual Python service implementations.
This means developers cannot run end-to-end tests or a local demo without
either (a) deploying a Cleat host or (b) building a custom runtime.

### Expected Behaviour

A `LocalRuntime`-like class (analogous to the `runtime.py` we had to
write) that:
1. Runs the workflow function directly (detecting non-WASM environment)
2. Routes `durable_call` to registered Python service instances
3. Stores query state in memory
4. Manages workflow lifecycle

### Workaround

We built `runtime.py` — a `_LocalHostCalls` class that extends
`CleatTestHarness` and overrides `durable_call` to dispatch to local
service instances.  This is ~120 lines of glue code that the SDK should
provide.

### Recommendation

Provide an official `LocalRuntime` or `DevRuntime` class in the SDK that
supports in-process workflow execution with configurable service routing.
This is essential for developer onboarding and local testing.

---

## Issue #4: `CleatTestHarness` Cannot Route Calls to Real Services

- **Severity:** Medium
- **Category:** Missing API

### Description

`CleatTestHarness.durable_call()` (in `test_harness.py:423`) only returns
pre-configured stub responses.  It cannot be configured to call an actual
service implementation for a given `(service, operation)` pair.

### Expected Behaviour

Ability to either:
- Register a "real" service handler that gets called instead of returning
  a stub response
- Subclass `CleatTestHarness` and override `durable_call` to add
  dispatching logic

### Workaround

We extended `CleatTestHarness` in `runtime.py` and overrode
`durable_call`:

```python
class _LocalHostCalls(CleatTestHarness):
    def __init__(self, ...):
        super().__init__()
        self._local_services = {}

    def register_service(self, name, instance):
        self._local_services[name] = instance

    def durable_call(self, service, operation, request):
        svc = self._local_services[service]
        method = getattr(svc, operation)
        return json.dumps(method(**request))
```

### Recommendation

Add a `register_service(name, instance)` method and a "passthrough" mode
to `CleatTestHarness` so that developers can route specific service calls
to real implementations while stubbing others.

---

## Issue #5: No Built-in Message Broker Plugin for Simple Messaging

- **Severity:** Medium
- **Category:** Missing plugin

### Description

The original DBOS app's `send_order_notification` is a `@DBOS.step()` that
simulates a message broker call.  The Cleat SDK has a `Plugins` class with
a Kafka plugin (`kafkaconnect.produce`) which requires a Kafka connector
configuration.  There is no generic "send message" or "publish to topic"
plugin that works without external infrastructure, and no simple way to
simulate a message broker in tests.

### Expected Behaviour

A `Plugins.send_message(topic, payload)` or similar that can be configured
to write to a message broker without requiring complex connector setup.
Or at minimum, clear documentation on how to implement a custom service
for messaging.

### Workaround

We defined a custom `NotifierService` that is called via
`durable_call("notifier", "send_notification", ...)`.  In tests, we
either stub it with `CleatTestHarness` or use a fast in-memory version.
For production, the notifier service would call the actual message broker
via its native client library.

### Recommendation

Add a lightweight "pubsub" or "messaging" service to the SDK that wraps
common message brokers (Kafka, RabbitMQ, SQS) with a uniform API, or
document the service-contract pattern for custom services.

---

## Issue #6: `CleatTestHarness.durable_call` Raises on Unstubbed Calls

- **Severity:** Medium
- **Category:** Developer experience

### Description

When testing with `CleatTestHarness`, every `durable_call` must have a
corresponding `stub_call` registered.  If one is missed, the test raises
a `RuntimeError` with "no stub registered".  This is fine for strict
testing but makes iterative development tedious — you must plan all
service interactions upfront.

### Expected Behaviour

An option to return a default empty response for unstubbed calls, or
a "record mode" that captures calls during a dry run.

### Workaround

Use `LocalRuntime` (from Issue #3) for integration-style development,
where services are real implementations and don't need stubs.

### Recommendation

Add a `strict` mode parameter (default `True`) to `CleatTestHarness` that,
when `False`, returns `"{}"` for unstubbed calls with a warning.

---

## Issue #7: No Equivalent to `DBOS.sql_session` for Direct DB Access

- **Severity:** Medium
- **Category:** Architectural gap

### Description

In DBOS, `@DBOS.transaction()` provides direct access to SQLAlchemy via
`DBOS.sql_session`.  This allows the workflow to directly execute
arbitrary SQL within the workflow's durability guarantees.

Cleat has no equivalent.  All DB access must go through
`durable_call("db", "operation", {...})` which calls a host-side service.
This means:
- Every DB operation must be defined as a host service method
- The service contract must be separately maintained
- There is no transaction context spanning multiple `durable_call`
  invocations
- Read queries (`list_orders`) bypass the workflow entirely

### Expected Behaviour

A mechanism for workflows to execute DB operations within the workflow's
durability scope, without requiring separate host service definitions for
every query.

### Workaround

Define all DB operations as host service methods and call them via
`durable_call`.  For read-only queries, call the DB service directly from
the API layer (bypassing the workflow).

### Recommendation

This is a fundamental architectural difference between DBOS (tightly
coupled to Postgres) and Cleat (WASM-sandboxed, language-agnostic).
Consider adding a "data service" abstraction that auto-generates host
service endpoints from SQLAlchemy table definitions, or support direct
DB access from the workflow via a WASM-compiled SQLite or comparable.

---

## Issue #8: `CleatClient` Has No Method to Retrieve Workflow Result

- **Severity:** Low
- **Category:** Missing API

### Description

`CleatClient` has `start_workflow()`, `send_signal()`, `get_query_state()`,
and `get_workflow_status()`.  The `get_workflow_status()` method returns
status metadata (state, timestamps) but does not return the workflow's
return value.

### Expected Behaviour

```python
result = client.get_workflow_result(run_id)  # returns the workflow's return value
```

### Workaround

Rely on `set_query_state` in the workflow to expose the return value
piecemeal, then read it via `get_query_state`.  Or call
`get_workflow_status` and parse the `result` field from the raw response
(undocumented).

### Recommendation

Add `get_workflow_result(run_id: str) -> Any` to `CleatClient` that
returns the deserialised return value of a completed workflow.

---

## Issue #9: Componentize-py / WASM Compilation Pipeline Not Documented

- **Severity:** Low
- **Category:** Documentation

### Description

The `@durable_entry` decorator generates WASM-export-compatible wrappers,
but the process for compiling a workflow module to WASM via
`componentize-py` is not documented in the SDK.

### Expected Behaviour

A `pyproject.toml` example or a CLI command showing how to compile a
Python workflow module to WASM:

```bash
componentize-py -f example.wit -o workflow.wasm workflow.py
```

### Workaround

Not applicable — we did not attempt WASM compilation in this port.

### Recommendation

Add a "Compiling to WASM" section to the SDK README with step-by-step
instructions, a minimal `wit` file, and the `componentize-py` command.

---

## Issue #10: `@durable_entry` Type Hints for Workflow Parameters Are Not Enforced at the Decorator Level

- **Severity:** Low
- **Category:** Developer experience

### Description

The `@durable_entry` decorator in `entry.py:80-104` inspects type hints
to determine which parameters are `HostCalls` vs. workflow parameters.
If a workflow parameter has no type hint, it may be silently excluded
from the parameter list, causing a `ValueError("Missing required
parameters")` at runtime.

### Expected Behaviour

A clear error message at decoration time (not at WASM call time) if a
parameter is not annotated, or a default assumption that un-annotated
parameters are workflow parameters (not `HostCalls`).

### Workaround

Always annotate all workflow parameters with their types.

### Recommendation

Improve the decorator's parameter resolution logic: if a parameter is
un-annotated, assume it is a workflow parameter (only `HostCalls` is
specially injected).
