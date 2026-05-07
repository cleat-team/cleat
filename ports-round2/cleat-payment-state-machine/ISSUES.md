# Issues Found During Porting: Restate Payment State Machines to Cleat Python SDK

This document catalogs every issue encountered while porting the Restate Payment State Machines Python example to use the Cleat durable execution Python SDK. Issues are numbered and include severity, affected code, expected behavior, workaround, and recommendations.

---

## Issue #1: No Key-Scoped State (Virtual Object Equivalent)

**Severity**: Critical

**Description**: Cleat has no equivalent of Restate Virtual Objects with automatic key-scoped state isolation. In Restate, `VirtualObject("PaymentProcessor")` automatically scopes all `ctx.get()`/`ctx.set()` calls to the object instance key (e.g., the payment ID). In Cleat, `get_state()`/`set_state()` is global to the workflow execution.

**Affected code**: `workflow.py` lines 69-91:

```python
# In Restate:
status = await ctx.get(STATUS, type_hint=str) or "NEW"
ctx.set(STATUS, "COMPLETED_SUCCESSFULLY")

# In Cleat (workaround):
def _payment_state_key(payment_id: str, field: str) -> str:
    return f"payment:{payment_id}:{field}"
```

**Expected behavior**: The runtime should provide scoped state per payment ID without manual key prefixing.

**Workaround**: All state keys are manually prefixed with the entity ID. E.g., `payment:{payment_id}:status` instead of just `status`. This is error-prone because:
- Two developers might use different prefix schemes
- There is no enforcement that keys are properly scoped
- State cleanup (clear_all) cannot target a specific entity's keys without knowing them all

**Recommendation**: Add a key-scoped state API, either via:
1. A `VirtualObject`-like class that manages key prefixing automatically, or
2. A context manager or wrapper that scopes all state operations to an entity key:
   ```python
   async with ctx.scoped_state(entity_id="pay-123") as scope:
       status = await scope.get("status")
       scope.set("status", "COMPLETED")
   ```

---

## Issue #2: No `TerminalError` in Core SDK

**Severity**: Critical

**Description**: Restate provides `restate.TerminalError` as a first-class exception for non-retryable failures. The Cleat SDK has no such concept, meaning the runtime has no way to distinguish between transient errors (should retry) and permanent errors (should fail immediately).

**Affected code**: `accounts.py` line 87, `workflow.py` line 179:

```python
# In Restate:
from restate.exceptions import TerminalError
raise TerminalError("Amount must be greater than 0")

# In Cleat (workaround):
from cleat_sdk import TerminalError  # We had to create this ourselves
raise TerminalError("Amount must be greater than 0")
```

**Expected behavior**: Cleat should ship with a built-in `TerminalError` that the runtime recognizes and handles (no retry, propagate to caller).

**Workaround**: We created `cleat_sdk.TerminalError` in the SDK package (`_exceptions.py`). However, this is a user-space class. The runtime has no way to know it should not retry unless the SDK provides it.

**Recommendation**: Add `TerminalError` (or equivalent) to the core SDK, and ensure the runtime treats it as non-retryable. Also document the distinction between:
- `TerminalError`: permanent failure, do not retry
- Other exceptions: transient failure, retry with backoff

---

## Issue #3: No Delayed Fire-and-Forget (object_send Equivalent)

**Severity**: High

**Description**: Restate's `ctx.object_send(handler, key, send_delay=..., arg=...)` provides a durable delayed fire-and-forget mechanism. It schedules a handler invocation to run after a delay without blocking the caller. Cleat has `durable_sleep()` and `durable_call()` but they are blocking -- you sleep, then call, and must await the result.

**Affected code**: `workflow.py` lines 146-157:

```python
# In Restate:
ctx.object_send(expire, payment_id, send_delay=EXPIRY_TIMEOUT, arg=None)

# In Cleat (workaround):
async def _delayed_expiry():
    await ctx.durable_sleep(EXPIRY_TIMEOUT_MS)
    for field in ["status", "payment"]:
        ctx.clear_state(_payment_state_key(payment_id, field))

task = asyncio.create_task(_delayed_expiry())
```

**Expected behavior**: A fire-and-forget delayed invocation API that:
- Does not block the calling workflow
- Is durably recorded (survives restarts)
- Can be cancelled if the parent workflow compensates

**Workaround**: We used `asyncio.create_task()` with `durable_sleep()` followed by cleanup. This has severe limitations:
- `asyncio.create_task()` is not a cleat primitive; it may not survive WASM compilation
- The delayed task is not durably recorded -- it will be lost on process restart
- The task cannot be cancelled if the payment is later refunded

**Recommendation**: Add a `schedule(service, operation, request, delay_ms)` method to `HostCalls` that provides durable delayed execution without blocking the caller. This could return a cancellation handle.

---

## Issue #4: No `object_call()` Cross-Service Invocation Pattern

**Severity**: High

**Description**: Restate's `ctx.object_call(withdraw, key=payment.account_id, arg=payment.amount_cents)` provides a typed, key-routed service invocation. The handler reference is passed as a first-class object. Cleat's `durable_call()` uses string-based service/operation routing.

**Affected code**: `workflow.py` lines 103-111:

```python
# In Restate:
payment_result = await ctx.object_call(
    withdraw, key=payment.account_id, arg=payment.amount_cents
)

# In Cleat:
result = await ctx.durable_call(
    "account",
    "withdraw",
    WithdrawRequest(
        account_id=payment.account_id,
        amount_cents=payment.amount_cents,
    ).model_dump(),
)
```

**Expected behavior**: Support for at least one of:
1. Decorator-based service references: `ctx.call(withdraw, key=..., arg=...)`
2. Strongly-typed routing with schema validation
3. Key-based routing (routing a call to a specific entity instance)

**Workaround**: String-based routing with manual request serialization. This loses type safety and key-routing semantics.

**Recommendation**: Consider adding a service registry that can resolve decorated functions to service names, enabling `ctx.call(account_withdraw, request)` syntax. Also consider adding key-routing support for entity-based services.

---

## Issue #5: No `clear_all()` Equivalent

**Severity**: Medium

**Description**: Restate's `ctx.clear_all()` clears all state for the current Virtual Object instance. Cleat has no equivalent -- you must clear each known key individually.

**Affected code**: `workflow.py` lines 196-199:

```python
# In Restate:
ctx.clear_all()

# In Cleat (workaround):
for field in ["status", "payment"]:
    ctx.clear_state(_payment_state_key(payment_id, field))
```

**Expected behavior**: A `clear_all_state()` method that removes all state for the current workflow execution.

**Workaround**: Track all keys used and clear them one by one. This is fragile because adding a new state key requires updating the cleanup code.

**Recommendation**: Add `clear_all_state()` or `reset_state()` to `HostCalls`. Alternatively, support state key prefix patterns (e.g., `clear_state_by_prefix("payment:pay-123:")`) which would also solve the Virtual Object key-scoping problem.

---

## Issue #6: No Virtual Object Serial Execution Guarantee

**Severity**: Critical

**Description**: Restate Virtual Objects guarantee that handlers execute serially per object key. This means concurrent requests for the same payment ID are queued and processed one at a time. Without this guarantee, a `makePayment` and `cancelPayment` call for the same payment could interleave, leading to race conditions.

**Affected code**: `workflow.py` lines 175-185:

```python
# In Restate: serial execution per key is automatic
# Two concurrent calls for the same payment-ID are queued

# In Cleat: no serial execution guarantee
# Concurrent calls for the same payment-id can interleave
```

**Expected behavior**: The runtime should provide serial execution per-workflow-ID, ensuring that concurrent invocations of the same entry point for the same payment ID are queued.

**Workaround**: None. The application is vulnerable to race conditions if the same payment ID is invoked concurrently. This is a fundamental runtime guarantee.

**Recommendation**: Ensure the Cleat runtime provides at-least-once execution with per-key serialization for workflow entry points. Document this as a core runtime guarantee.

---

## Issue #7: No `type_hint` Support in `get_state()`

**Severity**: Medium

**Description**: Restate's `ctx.get(key, type_hint=Payment)` automatically deserializes stored state into the specified type. Cleat's `get_state()` returns raw values.

**Affected code**: `workflow.py` lines 83-87:

```python
# In Restate:
payment = await ctx.get(PAYMENT, type_hint=Payment)

# In Cleat:
raw = ctx.get_state(_payment_state_key(payment_id, "payment"))
if isinstance(raw, dict):
    return Payment(**raw)
```

**Expected behavior**: `get_state(key, type_hint=SomeModel)` should automatically deserialize stored dicts into the specified type, especially for Pydantic models.

**Workaround**: Manual deserialization after retrieval. We added a basic `type_hint` parameter to our `HostCalls.get_state()` implementation, but this is not part of the runtime API.

**Recommendation**: Formally add `type_hint` support to `get_state()`, with automatic Pydantic model deserialization when `type_hint` has `model_validate` or is a dataclass.

---

## Issue #8: Closures and Lambda Capture in WASM Context

**Severity**: High

**Description**: The Saga pattern often uses closures/lambdas to capture variables for compensation actions. If Cleat workflows compile to WASM, closures that capture local state may not serialize/replay correctly.

**Affected code**: `workflow.py` :

```python
saga.add_step(
    action=lambda: ctx.durable_call(
        "account", "withdraw",
        WithdrawRequest(...).model_dump(),
    ),
    compensate=lambda: ctx.durable_call(
        "account", "deposit",
        DepositRequest(...).model_dump(),
    ),
    name="withdraw_from_account",
)
```

**Expected behavior**: Lambda captures in Saga steps should be serializable for WASM compilation and replay. The saga's `action` and `compensate` functions need to be durable.

**Workaround**: None directly. We used named lambda captures. If WASM compilation is a constraint, the Saga API may need to be redesigned to accept structured step definitions rather than closures.

**Recommendation**: If WASM compilation is a goal, provide a declarative Saga API:
```python
saga.add_step(
    action_service="account",
    action_operation="withdraw",
    action_input=WithdrawRequest(...),
    compensate_service="account",
    compensate_operation="deposit",
    compensate_input=DepositRequest(...),
)
```

---

## Issue #9: No `ctx.uuid()` / Deterministic ID Generation

**Severity**: Low

**Description**: Restate provides `ctx.uuid()` which generates deterministic UUIDs that replay consistently. Cleat has `ctx.random()` for deterministic random values but no UUID generation.

**Affected code**: Various (not used in our port, but common in Restate examples).

**Expected behavior**: `ctx.uuid()` or `ctx.generate_id()` that produces deterministic UUIDs based on the workflow ID and step number.

**Workaround**: Use `ctx.workflow_id` as a stable identifier, or string formatting with `ctx.random()`.

**Recommendation**: Add `uuid()` or `generate_id()` to `HostCalls` for deterministic ID generation.

---

## Issue #10: No Workflow Query/Status Expose API

**Severity**: Low

**Description**: Restate supports query handlers that expose workflow state without mutating it. Cleat has `set_query_state()` but no corresponding query/query-handler mechanism for external clients to read it.

**Affected code**: Not directly used in this port, but noted as a gap.

**Expected behavior**: External clients should be able to query workflow status (current state, progress) without invoking the workflow.

**Recommendation**: Document how `set_query_state` works with the runtime, and provide a query API for external clients.

---

## Issue #11: No Durable Delayed Execution (Scheduling)

**Severity**: High

**Description**: Restate's `ctx.object_send(handler, key, send_delay=..., arg=...)` provides durable delayed execution that survives process restarts. The Cleat workaround using `asyncio.create_task()` + `durable_sleep()` does NOT survive restarts because:

1. `asyncio.create_task()` is not persisted by the runtime
2. On process restart, all in-flight expiry tasks are lost
3. The expiry mechanism is best-effort and not durable

**Affected code**: `workflow.py` lines 318-335:

```python
def _schedule_expiry(ctx: HostCalls, payment_id: str) -> None:
    async def _delayed_expiry():
        await ctx.durable_sleep(EXPIRY_TIMEOUT_MS)
        for field in ["status", "payment"]:
            ctx.clear_state(_payment_state_key(payment_id, field))

    task = asyncio.create_task(_delayed_expiry())
    _expiry_tasks[payment_id] = task
```

**Expected behavior**: A durable scheduling API that:
- Survives process restarts
- Can be cancelled if needed
- Is recorded in the workflow event log

**Workaround**: `asyncio.create_task()` + `durable_sleep()`. This works for in-process testing but is not durable.

**Recommendation**: Add a scheduling primitive to `HostCalls`, e.g.:
```python
# Option 1: Schedule a fire-and-forget
schedule_id = await ctx.schedule(
    delay_ms=86400000,
    service="PaymentProcessor",
    operation="expire",
    request={"payment_id": "pay-123"},
)

# Option 2: Durable timer with callback
await ctx.set_timer(
    delay_ms=86400000,
    callback=lambda: ctx.durable_call("PaymentProcessor", "expire", ...),
)
```

---

## Issue #12: No HTTP/REST Transport Layer for Services

**Severity**: Medium

**Description**: Restate provides a built-in HTTP server (`restate.app()`) that serves services over HTTP. Cleat's `@durable_entry` decorators register functions but provide no transport layer for external invocation.

**Affected code**: `app.py` - The original served via:

```python
app = restate.app([payment_processor, account])
# Served via hypercorn
conf = hypercorn.Config()
conf.bind = ["0.0.0.0:9080"]
asyncio.run(hypercorn.asyncio.serve(app, conf))
```

In Cleat, there is no equivalent. The `HostCalls` object has no way to receive external HTTP requests.

**Expected behavior**: The Cleat runtime should provide a way to invoke `@durable_entry` functions via HTTP, either:
1. As part of the SDK itself (like `restate.app()`)
2. As a separate runtime component

**Workaround**: The development harness in `app.py` directly calls the registered functions with manually-created `HostCalls` contexts. This is not suitable for production.

**Recommendation**: Document the mechanism by which `@durable_entry` functions are invoked in production. If the runtime provides an HTTP gateway, document its API contract.

---

## Issue #13: No Integration Between Saga and HostCalls Context

**Severity**: Medium

**Description**: The `Saga` class is a standalone utility that has no integration with the `HostCalls` context. This means:

1. Saga cannot access the workflow's state or identity
2. Saga steps must capture `ctx` in closures (see Issue #8)
3. There is no way to make Saga steps durable in the workflow event log

**Affected code**: `workflow.py`:

```python
saga.add_step(
    action=lambda: ctx.durable_call(...),   # ctx captured in closure
    compensate=lambda: ctx.durable_call(...),
)
```

**Expected behavior**: The Saga should work naturally with the workflow context, either by:
1. Accepting `HostCalls` as a parameter to `execute(ctx)`
2. Providing a `SagaStep` type that references service/operation names

**Workaround**: Capture `ctx` in lambda closures. This works but is fragile for WASM (Issue #8).

**Recommendation**: Integrate Saga with HostCalls so steps can access the workflow context naturally:

```python
saga = Saga(ctx)
saga.add_step(
    action=("account", "withdraw", withdraw_request),
    compensate=("account", "deposit", deposit_request),
)
```

---

## Issue #14: No `@durable_entry` Type Safety / Input Validation

**Severity**: Low

**Description**: Restate's handler decorator supports typed inputs (via `type_hint`) and automatically deserializes request bodies. Cleat's `@durable_entry` returns the raw function signature, requiring manual deserialization of dict inputs.

**Affected code**: The `app.py` local handler registration:

```python
async def _deposit_handler(ctx: HostCalls, request: dict) -> None:
    req = DepositRequest(**request)  # Manual deserialization
    await deposit(ctx, req)
```

**Expected behavior**: The `@durable_entry` decorator or the runtime should automatically deserialize input dicts into the decorated function's parameter types.

**Recommendation**: Add type-based deserialization to the `@durable_entry` decorator or document the expected input format for runtime invocation.

---

## Issue #15: Cleat Python SDK Does Not Exist in Repository

**Severity**: Critical

**Description**: The Cleat Python SDK (`cleat_sdk`) does not exist anywhere in the repository. There are no Python packages, no setup.py/pyproject.toml, and no source files. The SDK described in the porting task (with `@durable_entry`, `HostCalls`, `Saga`, etc.) was not found.

This required creating a reference SDK implementation as part of the port, which:
1. May not match the actual SDK API if/when it is developed
2. Has no test coverage or CI
3. May have incomplete or incorrect behavior

**Expected behavior**: The Cleat Python SDK should exist as a published package (PyPI) or as source in the repository.

**Workaround**: We created `cleat_sdk/` as part of the port, providing the described API. This is a best-effort implementation and should be replaced with the official SDK once available.

**Recommendation**: Develop and publish the Cleat Python SDK. Key priorities:
1. Core `HostCalls` class with durable execution primitives
2. `@durable_entry` decorator with runtime registration
3. `TerminalError` for non-retryable failures
4. Virtual Object / key-scoped state support
5. `Saga` class for compensation patterns
