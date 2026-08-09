# SDK README Template

> Use this template when creating or updating an SDK README. See
> `python-sdk/README.md` for a complete worked example.

---

## 1. Installation

Describe how users install the SDK for their target language.

```<language>
# Example install command
```

If the SDK is published to a package registry, include the command. If it is
installed from source, show both paths.

Dependencies the user needs before installing (e.g., minimum language version,
WASM toolchain).

---

## 2. Quick Start

A short, complete, runnable example -- 5 lines of workflow code if possible.
The example should:

1. Import the SDK
2. Define a workflow function (annotated with the SDK's entry-point decorator)
3. Call a host function (e.g., `cleat_call` to a service)
4. Return a result

```<language>
// 5-line minimal example
```

Show the expected output or behaviour.

---

## 3. HostCall API Reference

Document every host function exposed by the SDK in a table. Group by category.

### Workflow Identity

| Function | Signature | Description |
|----------|-----------|-------------|
| `current_workflow_id` | `() -> string` | Return the current workflow's unique ID |
| `current_run_id` | `() -> string` | Return the current run's unique ID |

### Time & Random

| Function | Signature | Description |
|----------|-----------|-------------|
| `now` | `() -> i64` | Wall-clock time in ms since epoch |
| `random` | `() -> i64` | Deterministic random value (same on replay) |
| `version` | `() -> i64` | Workflow definition version |
| `min_version` | `() -> i64` | Minimum supported version |

### Durable Execution

| Function | Signature | Description |
|----------|-----------|-------------|
| `cleat_call` | `(service, operation, request) -> string` | Recorded API call |
| `cleat_call_with_retry` | `(service, operation, request, retry_policy) -> string` | Server-side retry |
| `cleat_sleep` | `(duration_ms) -> ()` | Suspend for a duration |
| `cleat_log` | `(message) -> ()` | Emit a log message to the event history |
| `cleat_fetch` | `(url, method, headers, body) -> (body, status)` | Durable HTTP fetch |
| `cleat_send` | `(service, operation, request) -> ()` | Fire-and-forget |
| `schedule_invoke` | `(service, operation, request, delay_ms) -> ()` | Delayed one-shot |

### Signals & Events

| Function | Signature | Description |
|----------|-----------|-------------|
| `await_signals` | `(signal_names, timeout_ms) -> SignalResult` | Wait for external signals |
| `poll_signal` | `(name) -> (payload, found)` | Non-blocking signal check |
| `poll_cancellation` | `() -> (cancelled, reason)` | Check for cancellation |

### Child Workflows

| Function | Signature | Description |
|----------|-----------|-------------|
| `child_workflow` | `(name, input) -> string` | Start child, returns run ID |
| `await_child` | `(run_id) -> string` | Await a single child |
| `await_all_children` | `(run_ids) -> list<ChildResult>` | Await multiple children |

### State

| Function | Signature | Description |
|----------|-----------|-------------|
| `set_query_state` | `(key, value) -> ()` | Set queryable state |
| `set_state` | `(key, value) -> ()` | Set typed state |
| `get_state` | `(key, result_type) -> T` | Get typed state |
| `delete_state` | `(key) -> ()` | Delete a state key |
| `incr_state` | `(key, delta) -> int` | Atomically increment a numeric key |

### Promises

| Function | Signature | Description |
|----------|-----------|-------------|
| `create_promise` | `(name, ttl_ms) -> string` | Create a durable promise |
| `await_promise` | `(promise_id, timeout_ms) -> PromiseResult` | Await a promise |
| `resolve_promise` | `(promise_id, value) -> ()` | Resolve a promise |
| `reject_promise` | `(promise_id, error) -> ()` | Reject a promise |

### Update Handlers

| Function | Signature | Description |
|----------|-----------|-------------|
| `register_update_handler` | `(name, handler, validator) -> ()` | Register update handler |

Do not add a `register_query_handler`. Every SDK had one until 2026-08-09; it
recorded a handler name with the host, but no worker code ever routed an
external query back to it -- it worked only inside each SDK's own
in-process test harness, which called the registered closure directly. See
`docs/determinism.md`, "Why there is no RegisterQueryHandler". A new SDK
should expose `set_query_state` (queryable state any caller can read via
`GET /api/workflows/:id/query?key=X`) instead.

### Lifecycle

| Function | Signature | Description |
|----------|-----------|-------------|
| `cleat_defer` | `(description) -> string` | Register cleanup on exit |
| `continue_as_new` | `(input) -> ()` | Start fresh run with new input |
| `run_detached` | `(fn) -> ()` | Execute detached from cancellation |

### Plugin Calls

| Function | Signature | Description |
|----------|-----------|-------------|
| `plugin_call` | `(plugin_name, function_name, input) -> string` | Call a host plugin |

---

## 4. WASM Compilation

Describe how to compile a workflow written in this SDK to WebAssembly.

### Prerequisites

List the tools needed (compiler, WASM target, etc.).

### Build command

```bash
# Example compilation command
```

### Output

Describe what the compilation produces (`.wasm` file) and how to verify it.

```bash
# Verify the output
```

---

## 5. Constraints / Known Limitations

Document the constraints of the target language when compiled to WASM.
Structure as a table or bullet list.

| Constraint | Details |
|------------|---------|
| Async support | Whether async/await is supported |
| Standard library availability | Which stdlib modules work / are unavailable |
| Reflection | Whether runtime reflection is available |
| Concurrency | Whether threading is available |
| String operations | Any gotchas (regex, replace, etc.) |
| Memory model | Key memory layout details |

If the SDK has no special constraints beyond the general WASM environment,
note that here and link to the general WASM constraints doc.

### Unit Differences

Cleat uses **milliseconds** for all time-related host calls. Include a
comparison table if porting from other frameworks is expected:

| Framework | Sleep Unit | Example |
|-----------|-----------|---------|
| **Cleat** | milliseconds | `cleat_sleep(5000)` = 5 seconds |
| **Temporal** | varies | `Duration.ofSeconds(5)` |
| **DBOS** | seconds | `DBOS.sleepSeconds(5)` |

---

## 6. Testing Guide

Explain how to test workflows written in this SDK.

### Unit testing with test harness

If the SDK provides an in-memory test harness, show how to use it:

```<language>
// Example test
```

### Integration testing

For testing against a real or local Cleat runtime, describe the setup:

```<language>
// Example integration test
```

### Testing with Go TestEnv (cross-language)

For Go-based testing of WASM workflows compiled from this SDK:

```go
// Go test example using durabletest.TestEnv
```

---

## 7. Troubleshooting

Common errors and their solutions, in a table format.

| Error | Likely Cause | Solution |
|-------|-------------|----------|
| `ImportError: module not found` | SDK or dependency not installed | Verify installation steps |
| Module fails at WASM runtime | C extension or unavailable stdlib | Replace with pure-Python / pure-language equivalent or host call |
| WASM binary too large | Unnecessary imports | Audit dependencies |
| `async` / `await` not supported | Async code in workflow | Convert to synchronous |
| Build fails without clear error | Missing toolchain or flags | Pass `--verbose` flag |
