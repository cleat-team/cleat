# Cleat Payment State Machine

A port of the [Restate Payment State Machines](https://github.com/restatedev/examples) advanced pattern to the Cleat durable execution Python SDK.

## Overview

This example implements a payment lifecycle state machine with:

- **State machine transitions**: Payments flow through NEW -> PROCESSING -> COMPLETED_SUCCESSFULLY or CANCELED
- **Concurrent cancellation support**: Cancellation can overtake or undo a payment
- **Compensation**: Completed payments are refunded when cancelled
- **Idempotent processing**: Payments are processed at most once
- **Expiry**: Automatic state cleanup after timeout

## Files

| File | Description |
|---|---|
| `accounts.py` | Account management (deposit/withdraw) |
| `workflow.py` | Payment state machine workflow |
| `app.py` | Application entry point and development harness |
| `cleat_sdk/` | Cleat Python SDK (hosted here until SDK is released) |
| `SERVICES_CONTRACT.md` | Required host runtime services |
| `ISSUES.md` | Catalog of issues found during porting |

## Migration Notes: Restate to Cleat API Mapping

This table maps every Restate SDK API call used in the original example to its Cleat equivalent.

### Core Concepts

| Restate Concept | Cleat Equivalent | Migration Notes |
|---|---|---|
| `restate.Service("Name")` / `restate.VirtualObject("Name")` | `@durable_entry(name="...")` | Cleat uses decorator-based registration. No Virtual Object semantics. |
| `@handler()` decorator | `@durable_entry(name="...")` | Same pattern, different decorator |
| `restate.ObjectContext` argument | `HostCalls` argument | The context object type is different |
| `restate.app([services])` | N/A | Cleat runtime manages registration via decorators |

### State Management

| Restate API | Cleat API | Migration Notes |
|---|---|---|
| `ctx.get(key, type_hint=T)` | `ctx.get_state(key)` | No type_hint support in base Cleat (#7) |
| `ctx.set(key, value)` | `ctx.set_state(key, value)` | Direct equivalent |
| `ctx.clear_all()` | `ctx.clear_state(key)` per key | No bulk clear (#5) |
| Virtual Object key-scoped state | Manual key prefixing | Cleat lacks Virtual Object scoping (#1) |

### Service Invocation

| Restate API | Cleat API | Migration Notes |
|---|---|---|
| `ctx.object_call(handler, key=..., arg=...)` | `ctx.durable_call(service, operation, request)` | String-based routing, no key-scoped routing (#4) |
| `ctx.object_send(handler, key=..., send_delay=..., arg=...)` | No equivalent | No delayed fire-and-forget (#3) |
| `ctx.call(handler)` | `ctx.durable_call(service, operation, request)` | General service call |

### Error Handling

| Restate API | Cleat API | Migration Notes |
|---|---|---|
| `TerminalError` | No built-in equivalent | Must be added to SDK (#2) |
| `from restate.exceptions import TerminalError` | `from cleat_sdk import TerminalError` | We created a local TerminalError. Runtime may not recognize it. |

### Timers and Scheduling

| Restate API | Cleat API | Migration Notes |
|---|---|---|
| `ctx.sleep(duration)` | `ctx.durable_sleep(ms)` | Direct equivalent (units differ: timedelta vs ms) |
| `ctx.object_send(..., send_delay=..., arg=...)` | No equivalent | Use durable_sleep + durable_call (blocking) (#3) |

### Identity

| Restate API | Cleat API | Migration Notes |
|---|---|---|
| `ctx.key()` | `ctx.key()` | Supported via workflow_id alias |
| `ctx.uuid()` | No equivalent | No deterministic UUID generation (#9) |

### Advanced Features (not used in original, but available)

| Restate API | Cleat API |
|---|---|
| Signals | `ctx.await_signals(names, timeout_ms)`, `ctx.poll_signal(name)` |
| Awakeables | `ctx.create_promise(name)`, `ctx.await_promise(id, timeout_ms)` |
| Child workflows | `ctx.child_workflow(name, input)`, `ctx.await_child(run_id)` |
| Sagas | `cleat_sdk.Saga` class |
| Query state | `ctx.set_query_state(key, value)` |
| Logging | `ctx.durable_log(msg)` |

## Running

```bash
cd cleat-payment-state-machine
python -m app
```

The development harness simulates payment and cancellation workflows using the HostCalls context directly. In production, the Cleat runtime would manage workflow execution.

## Key Design Decisions

1. **Manual key prefixing**: Since Cleat lacks key-scoped Virtual Object state, all state keys are prefixed with the entity type and ID (e.g., `payment:{payment_id}:status`). See ISSUES.md #1.

2. **Saga for compensation**: The payment workflow uses Cleat's `Saga` class to automatically refund the account if the withdrawal fails after debiting. See `workflow.py` for the saga pattern.

3. **Blocking expiry**: Without delayed fire-and-forget, the expiry mechanism uses `durable_sleep` + `clear_state`. This blocks the workflow during the timeout period. See ISSUES.md #3.

## Issues Summary

See [ISSUES.md](ISSUES.md) for the complete catalog. Key gaps:

- **Critical**: No Virtual Object / key-scoped state (#1)
- **Critical**: No TerminalError in core SDK (#2)
- **Critical**: No serial execution guarantee (#6)
- **High**: No delayed fire-and-forget (#3)
- **High**: No typed service referencing (#4)
- **High**: WASM closure/lambda concerns (#8)
- **Medium**: No clear_all (#5), no type_hint (#7)
- **Low**: No uuid(), no query API (#9, #10)
