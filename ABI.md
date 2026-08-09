# Cleat WASM ABI Specification

This document defines the exact WebAssembly contract between workflow modules and the cleat host runtime. Any language that compiles to WASM and conforms to this ABI can produce workflow modules that run on the cleat host.

## Version

ABI version: 1 — the value of `CurrentABIVersion` in `wasm/metadata.go`, stamped into every
compiled workflow and used to gate redeploy compatibility in `engine/version_compat.go`.
The ABI is versioned separately from the workflow definition version. The host runtime
supports all ABI versions it was compiled for.

> This document previously claimed version 4, and its changelog below claimed 5. Neither
> was ever the shipped value. If you change `CurrentABIVersion`, change it here too.

---

## 1. Export Convention

Every workflow entry point must be exported from the WASM module with the following signature:

```
(func $entry_point_name (param i32 i32 i32 i32) (result i64))
```

Or in C/Rust notation:
```c
int64_t entry_point_name(const uint8_t* args_ptr, uint32_t args_len,
                          uint8_t* out_ptr, uint32_t max_out_len);
```

### Parameters

| WASM Param | Type | Description |
|---|---|---|
| `args_ptr` | `i32` | Pointer to input JSON in linear memory |
| `args_len` | `i32` | Byte length of input JSON |
| `out_ptr` | `i32` | Pointer to output buffer in linear memory |
| `max_out_len` | `i32` | Capacity of output buffer (1048576 bytes) |

### Return value (i64 packed)

| Bits | Meaning |
|---|---|
| 0-31 | Error code. 0 = success, non-zero = error |
| 32-63 | Actual number of bytes written to `out_ptr` |

### Suspension sentinel

If the workflow needs to suspend (e.g., for a timer or signal), the export returns the sentinel value:

```
0x4000000000000000  (1 << 62)
```

The host must check for this value before decoding the normal result. Suspension is not an error — the host will resume the workflow later by calling the export again with the same input and full event history.

### Input/Output format

All arguments are JSON-serialized into a single object. The export function deserializes them, calls the workflow logic, and writes the result into the output buffer.

**The result contract, normatively — this is the whole point of the ABI being language-agnostic.**
An entry point returns **a string containing a JSON-encoded object**. Not a JSON-encoded
string, and not a bare scalar. Every SDK conforms to the same rule, which is what lets a Go
host read a Java, Python, Rust or AssemblyScript guest's result with one decoder.

The distinction is not pedantic, and the failure mode is silent. `{"amount":250}` and
`"{\"amount\":250}"` are both "the result serialized as JSON" under a loose reading of the
previous sentence, and the second one decodes to a *string* where the caller expects an
object — so `amount` is unreachable and, once it is finally unwrapped, it is a string where
it should be a number. Getting this wrong does not fail; it produces a result that looks
plausible and is the wrong shape.

That is exactly what happened to the Java SDK, and it happened *twice in one value*: the
entry point returned a `String` it had already hand-built as JSON, `CleatEntryProcessor` then
applied `JsonHelper.stringify()` to it, and separately the workflow embedded a host call's
JSON response into a string field. Fixed 2026-08-09 (#455) by returning a
`Map<String, Object>` from the entry point. See `tiers.yaml`'s `sdk-java` entry for the full
account, including the detail that converting the result to a `Map` too eagerly regressed a
numeric field into a string.

Known gap, recorded rather than implied: **Go has no typed-result path** — it has no
equivalent of the `Map<String, Object>` return that fixed Java, so Go workflows still
hand-build their result JSON and nothing enforces the contract at the type level.

**Input example** (for a Go `func PlaceOrder(h HostCalls, userID string, cart []CartItem) (string, error)`):
```json
{"UserID":"user-42","Cart":[{"SKU":"SKU-001","Quantity":2}]}
```

**Output example** (success):
```json
{"tracking_id":"TRACK-123456"}
```

**Output example** (error):
The function returns error code 1 in bits 0-31, and the output buffer contains:
```json
{"error":"payment failed: insufficient funds"}
```

---

## 2. Host Function Imports

All host functions are imported from the `"env"` WASM module. Every function returns `i64` with a bit-packed result. Strings cross the boundary as `(ptr: i32, len: i32)` pairs — input strings use `(ptr, actual_len)`, output buffers use `(out_ptr, max_len)` where the host writes the result and returns the actual length in the packed result.

### Core workflow calls

#### 2.1 `cleat_call`

Make a recorded API call to an external service.

```
(func (import "env" "cleat_call")
  (param i32 i32 i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `service_ptr` | `i32` | Service name pointer |
| `service_len` | `i32` | Service name length |
| `operation_ptr` | `i32` | Operation name pointer |
| `operation_len` | `i32` | Operation name length |
| `request_ptr` | `i32` | Request JSON pointer |
| `request_len` | `i32` | Request JSON length |
| `response_ptr` | `i32` | Output buffer for response |
| `response_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` — 0 = success, 1 = error |
| 8-39 | `callErrorCode` — 0 or 1 (reserved for structured error codes) |
| 40-63 | `responseLen` — bytes written to response buffer |

If `errCode == 0`, the response buffer contains valid JSON. If `errCode == 1`, the response buffer contains an error message string.

##### At-Least-Once Semantics

`cleat_call` provides at-least-once execution, not exactly-once. There is a crash window between the external call completing and the event being persisted to `event_history`. If the worker crashes during this window, replay will not find a recorded event for this step and will re-execute the call.

A write-ahead intent mechanism (`flushCallIntent` / `completeCallEvent` with the `pendingSentinel` sentinel) is being introduced to narrow this window and signal ambiguous outcomes. On replay, if the replay path finds a step whose error field equals the `pendingSentinel` sentinel, it returns an `ErrAmbiguous` error to inform the caller that the outcome is unknown.

Applications must:
- Design external services to be idempotent (include an idempotency key in every call).
- Handle the `ErrAmbiguous` error by checking the external service's state.
- Never assume a `DurableCall` happens exactly once.

See [docs/durable-calls.md](docs/durable-calls.md) for a detailed explanation of the at-least-once contract, the crash window, ambiguity detection, idempotency patterns, and comparison with other frameworks.

#### 2.2 `cleat_call_retry`

Server-side retry variant of `cleat_call`. Retries happen inside the host; one event is recorded regardless of attempt count.

```
(func (import "env" "cleat_call_retry")
  (param i32 i32 i32 i32 i32 i32 i64 i64 i64 i64 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `service_ptr` | `i32` | Service name pointer |
| `service_len` | `i32` | Service name length |
| `operation_ptr` | `i32` | Operation name pointer |
| `operation_len` | `i32` | Operation name length |
| `request_ptr` | `i32` | Request JSON pointer |
| `request_len` | `i32` | Request JSON length |
| `max_attempts` | `i64` | Maximum number of retry attempts |
| `initial_interval_ms` | `i64` | Initial retry interval in milliseconds |
| `backoff_coefficient_100x` | `i64` | Backoff coefficient scaled by 100x (e.g., 200 = 2.0x) |
| `max_interval_ms` | `i64` | Maximum retry interval in milliseconds |
| `non_retryable_errors_ptr` | `i32` | Pointer to JSON array of non-retryable error codes |
| `non_retryable_errors_len` | `i32` | Length of non-retryable errors JSON |
| `response_ptr` | `i32` | Output buffer for response |
| `response_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` — 0 = success, 1 = error |
| 8-39 | `callErrorCode` — 0 or 1 (reserved for structured error codes) |
| 40-63 | `responseLen` — bytes written to response buffer |

#### 2.3 `cleat_call_heartbeat`

Long-running call with progress updates. The host sends periodic progress updates; the progress callback is handled at the SDK layer.

```
(func (import "env" "cleat_call_heartbeat")
  (param i32 i32 i32 i32 i32 i32 i64 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `service_ptr` | `i32` | Service name pointer |
| `service_len` | `i32` | Service name length |
| `operation_ptr` | `i32` | Operation name pointer |
| `operation_len` | `i32` | Operation name length |
| `request_ptr` | `i32` | Request JSON pointer |
| `request_len` | `i32` | Request JSON length |
| `heartbeat_interval_ms` | `i64` | Heartbeat interval in milliseconds |
| `response_ptr` | `i32` | Output buffer for response |
| `response_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` — 0 = success, 1 = error |
| 8-39 | `callErrorCode` — 0 or 1 (reserved for structured error codes) |
| 40-63 | `responseLen` — bytes written to response buffer |

### Timing and randomness

#### 2.4 `cleat_sleep`

Suspend workflow execution for a duration.

```
(func (import "env" "cleat_sleep") (param i64) (result i64))
```

| Param | Type | Description |
|---|---|---|
| `duration_ms` | `i64` | Sleep duration in milliseconds |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-55 | `duration_ms` — echo of the input duration |
| 56-63 | `status` — 0 = completed (replay), 1 = suspend (fresh) |

On fresh execution, `status == 1` and the workflow should propagate the suspend by returning the suspension sentinel. On replay, `status == 0`.

#### 2.5 `cleat_now`

Get current wall-clock time.

```
(func (import "env" "cleat_now") (result i64))
```

Returns: current time in **milliseconds since Unix epoch** as `i64`.

#### 2.6 `cleat_random`

Get a deterministic random value.

```
(func (import "env" "cleat_random") (result i64))
```

Returns: deterministic `i64` value. The same value is returned on replay.

### Workflow metadata

#### 2.7 `cleat_log`

Log a message to the host.

```
(func (import "env" "cleat_log") (param i32 i32) (result i64))
```

| Param | Type | Description |
|---|---|---|
| `message_ptr` | `i32` | Log message pointer |
| `message_len` | `i32` | Log message length |

Return value is ignored.

#### 2.8 `cleat_version`

Get the workflow definition version.

```
(func (import "env" "cleat_version") (result i64))
```

Returns: workflow definition version as `i64` (cast from `uint32`).

#### 2.9 `cleat_min_version`

Get the minimum supported version for this workflow definition.

```
(func (import "env" "cleat_min_version") (result i64))
```

Returns: minimum version as `i64` (cast from `uint32`).

#### 2.10 `cleat_defer`

Register a cleanup callback to run on workflow exit.

```
(func (import "env" "cleat_defer")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `description_ptr` | `i32` | Defer description pointer |
| `description_len` | `i32` | Defer description length |
| `defer_id_ptr` | `i32` | Output buffer for defer ID |
| `defer_id_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `deferIDLen` — bytes written to defer ID buffer |

#### 2.11 `cleat_poll_cancellation`

Check if workflow cancellation has been requested.

```
(func (import "env" "cleat_poll_cancellation")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `reason_ptr` | `i32` | Output buffer for cancellation reason |
| `reason_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `cancelled` — non-zero if cancelled |
| 32-63 | `reasonLen` — bytes written to reason buffer (if cancelled) |

### Signal operations

#### 2.12 `cleat_poll_signal`

Poll for a specific pending signal.

```
(func (import "env" "cleat_poll_signal")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `name_ptr` | `i32` | Signal name pointer |
| `name_len` | `i32` | Signal name length |
| `payload_ptr` | `i32` | Output buffer for signal payload |
| `payload_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` |
| 8-15 | `found` flag — `0x0100` if signal was found |
| 32-63 | `payloadLen` — bytes written to payload buffer |

#### 2.13 `cleat_await_signals`

Wait for one or more external signals, with a timeout.

```
(func (import "env" "cleat_await_signals")
  (param i32 i32 i64 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `names_ptr` | `i32` | JSON array of signal names, e.g. `["payment_received","order_cancelled"]` |
| `names_len` | `i32` | JSON array length |
| `timeout_ms` | `i64` | Timeout in milliseconds |
| `sig_name_ptr` | `i32` | Output buffer for received signal name |
| `sig_name_max_len` | `i32` | Output buffer capacity (1048576) |
| `payload_ptr` | `i32` | Output buffer for signal payload |
| `payload_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-15 | `errCode` |
| 16-31 | `timedOut` — non-zero if timeout expired |
| 32-47 | `payloadLen` — bytes written to payload buffer |
| 48-63 | `sigNameLen` — bytes written to signal name buffer |

#### 2.14 `cleat_send_signal_and_wait`

Send a signal to another workflow and wait for a correlated reply.

```
(func (import "env" "cleat_send_signal_and_wait")
  (param i32 i32 i32 i32 i32 i32 i64 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `target_ptr` | `i32` | Target workflow run ID pointer |
| `target_len` | `i32` | Target workflow run ID length |
| `sig_ptr` | `i32` | Signal name pointer |
| `sig_len` | `i32` | Signal name length |
| `payload_ptr` | `i32` | Signal payload pointer |
| `payload_len` | `i32` | Signal payload length |
| `timeout_ms` | `i64` | Timeout in milliseconds |
| `resp_ptr` | `i32` | Output buffer for reply response |
| `resp_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `responseLen` — bytes written to response buffer |

#### 2.15 `cleat_reply_to_signal`

Reply to a correlated signal with a response payload.

```
(func (import "env" "cleat_reply_to_signal")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `correlation_ptr` | `i32` | Correlation ID pointer |
| `correlation_len` | `i32` | Correlation ID length |
| `resp_ptr` | `i32` | Response payload pointer |
| `resp_len` | `i32` | Response payload length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

#### 2.16 `cleat_signal_workflow`

Send a fire-and-forget signal to another workflow.

```
(func (import "env" "cleat_signal_workflow")
  (param i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `target_ptr` | `i32` | Target workflow run ID pointer |
| `target_len` | `i32` | Target workflow run ID length |
| `sig_ptr` | `i32` | Signal name pointer |
| `sig_len` | `i32` | Signal name length |
| `payload_ptr` | `i32` | Signal payload pointer |
| `payload_len` | `i32` | Signal payload length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

### Workflow continuation and children

#### 2.17 `cleat_continue_as_new`

Start a new workflow run with fresh input (history compaction).

```
(func (import "env" "cleat_continue_as_new")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `input_ptr` | `i32` | New input JSON pointer |
| `input_len` | `i32` | New input JSON length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

After this call, the workflow should return the suspension sentinel.

#### 2.18 `cleat_continue_as_new_versioned`

Start a new workflow run with fresh input and a new version (history compaction with version upgrade).

```
(func (import "env" "cleat_continue_as_new_versioned")
  (param i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `input_ptr` | `i32` | New input JSON pointer |
| `input_len` | `i32` | New input JSON length |
| `new_version` | `i32` | New workflow definition version |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

After this call, the workflow should return the suspension sentinel.

#### 2.19 `cleat_child_workflow`

Start a child workflow instance.

```
(func (import "env" "cleat_child_workflow")
  (param i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `name_ptr` | `i32` | Child workflow definition name |
| `name_len` | `i32` | Child workflow name length |
| `input_ptr` | `i32` | Input JSON pointer |
| `input_len` | `i32` | Input JSON length |
| `run_id_ptr` | `i32` | Output buffer for run ID |
| `run_id_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` |
| 32-63 | `runIDLen` — bytes written to run ID buffer |

#### 2.20 `cleat_child_workflow_with_options`

Start a child workflow instance with configurable version and parent close policy.

```
(func (import "env" "cleat_child_workflow_with_options")
  (param i32 i32 i32 i32 i64 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `name_ptr` | `i32` | Child workflow definition name pointer |
| `name_len` | `i32` | Child workflow name length |
| `input_ptr` | `i32` | Input JSON pointer |
| `input_len` | `i32` | Input JSON length |
| `version` | `i64` | Child workflow definition version |
| `policy_ptr` | `i32` | Parent close policy pointer |
| `policy_len` | `i32` | Parent close policy length |
| `run_id_ptr` | `i32` | Output buffer for run ID |
| `run_id_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` |
| 32-63 | `runIDLen` — bytes written to run ID buffer |

#### 2.21 `cleat_child_workflow_in_schema`

Start a child workflow instance in a different PostgreSQL schema for cross-instance cooperation.

```
(func (import "env" "cleat_child_workflow_in_schema")
  (param i32 i32 i32 i32 i32 i32 i64 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `schema_ptr` | `i32` | Target schema name pointer |
| `schema_len` | `i32` | Target schema name length |
| `name_ptr` | `i32` | Child workflow definition name pointer |
| `name_len` | `i32` | Child workflow name length |
| `input_ptr` | `i32` | Input JSON pointer |
| `input_len` | `i32` | Input JSON length |
| `version` | `i64` | Child workflow definition version |
| `policy_ptr` | `i32` | Parent close policy pointer |
| `policy_len` | `i32` | Parent close policy length |
| `run_id_ptr` | `i32` | Output buffer for run ID |
| `run_id_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` |
| 32-63 | `runIDLen` — bytes written to run ID buffer |

#### 2.22 `cleat_await_child`

Wait for a child workflow to complete.

```
(func (import "env" "cleat_await_child")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `run_id_ptr` | `i32` | Child run ID pointer |
| `run_id_len` | `i32` | Child run ID length |
| `result_ptr` | `i32` | Output buffer for child result |
| `result_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` |
| 32-63 | `resultLen` — bytes written to result buffer |

If the child is not complete, the workflow should suspend.

#### 2.23 `cleat_await_all_children`

Batch await for multiple child workflows. Returns a JSON array of child results.

```
(func (import "env" "cleat_await_all_children")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `run_ids_json_ptr` | `i32` | Pointer to JSON array of run IDs |
| `run_ids_json_len` | `i32` | Length of run IDs JSON |
| `results_ptr` | `i32` | Output buffer for results |
| `results_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` |
| 32-63 | `resultLen` — bytes written to results buffer |

#### 2.24 `cleat_run_detached`

Run a detached child workflow (fire-and-forget, no result expected).

```
(func (import "env" "cleat_run_detached")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `name_ptr` | `i32` | Child workflow definition name pointer |
| `name_len` | `i32` | Child workflow name length |
| `input_ptr` | `i32` | Input JSON pointer |
| `input_len` | `i32` | Input JSON length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

### Query and update handlers

#### 2.25 `cleat_register_query_handler`

Records a query handler name on the host side. **No worker code ever reads
this back out to route an external query to it** -- there is no dispatch
path from any HTTP route, CLI command, or worker loop to a registered
handler; the only thing that ever invoked one was each SDK's own in-process
test harness. Every SDK's public wrapper around this call was removed
2026-08-09 (see `docs/determinism.md`, "Why there is no
RegisterQueryHandler"). The import is still accepted by the engine, kept as
a no-op purely so guests already compiled against it still instantiate --
new code should not call it and should not expect anything to happen if it
does. Use `set_query_state` (2.27) instead: it is durable and externally
readable via `GET /api/workflows/:id/query?key=X` regardless of whether a
worker currently has the workflow loaded.

```
(func (import "env" "cleat_register_query_handler")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `name_ptr` | `i32` | Query handler name pointer |
| `name_len` | `i32` | Query handler name length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

#### 2.26 `cleat_register_update_handler`

Register an update handler for workflow updates (bi-directional RPC). Handler registration is one-way; no output buffer is needed.

```
(func (import "env" "cleat_register_update_handler")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `name_ptr` | `i32` | Update handler name pointer |
| `name_len` | `i32` | Update handler name length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

#### 2.27 `set_query_state`

Set a key-value pair in the workflow's query state.

```
(func (import "env" "set_query_state")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `key_ptr` | `i32` | Key pointer |
| `key_len` | `i32` | Key length |
| `value_ptr` | `i32` | Value pointer |
| `value_len` | `i32` | Value length |

Return value is ignored.

### Durable key-value state operations

#### 2.28 `cleat_set_state`

Set a key-value pair in the workflow's durable state.

```
(func (import "env" "cleat_set_state")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `key_ptr` | `i32` | Key pointer |
| `key_len` | `i32` | Key length |
| `val_ptr` | `i32` | Value pointer |
| `val_len` | `i32` | Value length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

#### 2.29 `cleat_get_state`

Get the value for a key in the workflow's durable state.

```
(func (import "env" "cleat_get_state")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `key_ptr` | `i32` | Key pointer |
| `key_len` | `i32` | Key length |
| `value_ptr` | `i32` | Output buffer for value |
| `value_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `valueLen` — bytes written to value buffer |

#### 2.30 `cleat_delete_state`

Delete a key from the workflow's durable state.

```
(func (import "env" "cleat_delete_state")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `key_ptr` | `i32` | Key pointer |
| `key_len` | `i32` | Key length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

#### 2.31 `cleat_incr_state`

Atomically increment a numeric state value by a delta.

```
(func (import "env" "cleat_incr_state")
  (param i32 i32 i64)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `key_ptr` | `i32` | Key pointer |
| `key_len` | `i32` | Key length |
| `delta` | `i64` | Amount to increment (may be negative) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `newValue` — new value after increment |

#### 2.32 `cleat_has_state`

Check if a key exists in the workflow's durable state.

```
(func (import "env" "cleat_has_state")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `key_ptr` | `i32` | Key pointer |
| `key_len` | `i32` | Key length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `exists` — 1 if key exists, 0 otherwise |

#### 2.33 `cleat_list_state`

List state keys matching a given prefix.

```
(func (import "env" "cleat_list_state")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `prefix_ptr` | `i32` | Key prefix pointer |
| `prefix_len` | `i32` | Key prefix length |
| `keys_ptr` | `i32` | Output buffer for JSON array of keys |
| `keys_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `keysLen` — bytes written to keys buffer |

### Durable promises

#### 2.34 `cleat_create_promise`

Create a named durable promise. Returns a promise ID that external callers use to resolve or reject the promise.

```
(func (import "env" "cleat_create_promise")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `name_ptr` | `i32` | Promise name pointer |
| `name_len` | `i32` | Promise name length |
| `promise_id_out_ptr` | `i32` | Output buffer for promise ID |
| `promise_id_out_max` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `promiseIDLen` — bytes written to promise ID buffer |

#### 2.35 `cleat_await_promise`

Wait for a promise to be resolved by an external caller. Blocks until resolved or timeout expires.

```
(func (import "env" "cleat_await_promise")
  (param i32 i32 i64 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `promise_id_ptr` | `i32` | Promise ID pointer |
| `promise_id_len` | `i32` | Promise ID length |
| `timeout_ms` | `i64` | Timeout in milliseconds |
| `result_out_ptr` | `i32` | Output buffer for result |
| `result_out_max` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-15 | `errCode` — 0 = success, non-zero = error |
| 16-31 | `timedOut` — non-zero if timeout expired |
| 32-63 | `resultLen` — bytes written to result buffer |

#### 2.36 `cleat_resolve_promise`

Resolve a durable promise with a value. External callers waiting on this promise will receive the value.

```
(func (import "env" "cleat_resolve_promise")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `promise_id_ptr` | `i32` | Promise ID pointer |
| `promise_id_len` | `i32` | Promise ID length |
| `value_ptr` | `i32` | Value pointer |
| `value_len` | `i32` | Value length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

#### 2.37 `cleat_reject_promise`

Reject a durable promise with an error message. External callers waiting on this promise will receive the error.

```
(func (import "env" "cleat_reject_promise")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `promise_id_ptr` | `i32` | Promise ID pointer |
| `promise_id_len` | `i32` | Promise ID length |
| `err_ptr` | `i32` | Error message pointer |
| `err_len` | `i32` | Error message length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

### Scoped state / virtual objects

#### 2.38 `cleat_set_scope`

Set the virtual object scope for the current workflow execution.

```
(func (import "env" "cleat_set_scope")
  (param i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `obj_type_ptr` | `i32` | Object type pointer |
| `obj_type_len` | `i32` | Object type length |
| `inst_key_ptr` | `i32` | Instance key pointer |
| `inst_key_len` | `i32` | Instance key length |
| `prev_scope_ptr` | `i32` | Output buffer for previous scope |
| `prev_scope_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `prevScopeLen` — bytes written to previous scope buffer |

#### 2.39 `cleat_get_scope`

Get the current virtual object scope.

```
(func (import "env" "cleat_get_scope")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `obj_type_ptr` | `i32` | Output buffer for object type |
| `obj_type_max_len` | `i32` | Output buffer capacity (1048576) |
| `inst_key_ptr` | `i32` | Output buffer for instance key |
| `inst_key_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-47 | `objTypeLen` — bytes written to object type buffer |
| 48-63 | `instKeyLen` — bytes written to instance key buffer |

#### 2.40 `cleat_uuid`

Generate a deterministic UUID from a seed value.

```
(func (import "env" "cleat_uuid")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `seed_ptr` | `i32` | Seed string pointer |
| `seed_len` | `i32` | Seed string length |
| `uuid_ptr` | `i32` | Output buffer for UUID |
| `uuid_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `uuidLen` — bytes written to UUID buffer |

### Locking

#### 2.41 `cleat_acquire_lock`

Acquire a distributed lock with a TTL.

```
(func (import "env" "cleat_acquire_lock")
  (param i32 i32 i64)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `key_ptr` | `i32` | Lock key pointer |
| `key_len` | `i32` | Lock key length |
| `ttl_ms` | `i64` | Time-to-live in milliseconds |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = acquired, non-zero = not acquired |

#### 2.42 `cleat_release_lock`

Release a previously acquired distributed lock.

```
(func (import "env" "cleat_release_lock")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `key_ptr` | `i32` | Lock key pointer |
| `key_len` | `i32` | Lock key length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

### Side effects

#### 2.43 `cleat_side_effect`

Record a non-deterministic computation result. On first execution, the result is stored in event history; on replay, the cached result is returned.

```
(func (import "env" "cleat_side_effect")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `result_ptr` | `i32` | Computed result pointer |
| `result_len` | `i32` | Computed result length |
| `out_ptr` | `i32` | Output buffer for cached result |
| `out_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `outputLen` — bytes written to output buffer |

### Workflow identity

#### 2.44 `cleat_workflow_id`

Get the current workflow's unique identifier.

```
(func (import "env" "cleat_workflow_id")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `id_ptr` | `i32` | Output buffer for workflow ID |
| `id_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `idLen` — bytes written to ID buffer |

#### 2.45 `cleat_run_id`

Get the current workflow run's unique identifier.

```
(func (import "env" "cleat_run_id")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `id_ptr` | `i32` | Output buffer for run ID |
| `id_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `idLen` — bytes written to ID buffer |

### Messaging / scheduling

#### 2.46 `cleat_send`

Send a fire-and-forget request to an external service.

```
(func (import "env" "cleat_send")
  (param i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `svc_ptr` | `i32` | Service name pointer |
| `svc_len` | `i32` | Service name length |
| `op_ptr` | `i32` | Operation name pointer |
| `op_len` | `i32` | Operation name length |
| `req_ptr` | `i32` | Request JSON pointer |
| `req_len` | `i32` | Request JSON length |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

#### 2.47 `cleat_schedule_invoke`

Schedule a delayed one-shot invocation of an external service operation.

```
(func (import "env" "cleat_schedule_invoke")
  (param i32 i32 i32 i32 i32 i32 i64)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `svc_ptr` | `i32` | Service name pointer |
| `svc_len` | `i32` | Service name length |
| `op_ptr` | `i32` | Operation name pointer |
| `op_len` | `i32` | Operation name length |
| `req_ptr` | `i32` | Request JSON pointer |
| `req_len` | `i32` | Request JSON length |
| `delay_ms` | `i64` | Delay in milliseconds before invocation |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |

### HTTP fetch

#### 2.48 `cleat_fetch`

Perform an HTTP fetch request. The method, URL, headers (JSON), and body are all configurable.

```
(func (import "env" "cleat_fetch")
  (param i32 i32 i32 i32 i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `method_ptr` | `i32` | HTTP method pointer (e.g. "GET", "POST") |
| `method_len` | `i32` | HTTP method length |
| `url_ptr` | `i32` | URL pointer |
| `url_len` | `i32` | URL length |
| `headers_ptr` | `i32` | JSON object of headers |
| `headers_len` | `i32` | Headers JSON length |
| `body_ptr` | `i32` | Request body pointer |
| `body_len` | `i32` | Request body length |
| `response_ptr` | `i32` | Output buffer for response |
| `response_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `responseLen` — bytes written to response buffer |

### JSON validation helpers

#### 2.51 `cleat_json_parse`

Parse and validate a JSON string. Returns the canonical (re-serialized) form of the input JSON.

```
(func (import "env" "cleat_json_parse")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `json_ptr` | `i32` | JSON string pointer |
| `json_len` | `i32` | JSON string length |
| `out_ptr` | `i32` | Output buffer for normalized JSON |
| `out_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success, non-zero = invalid JSON |
| 32-63 | `bytesWritten` — bytes written to output buffer |

If `errCode != 0`, the input was not valid JSON and `bytesWritten` is 0.

#### 2.52 `cleat_json_stringify`

Validate and re-serialize a JSON value. Identical behavior to `cleat_json_parse` — both parse then re-serialize via the host's `encoding/json`. Provided as a semantic alias for SDK ergonomics.

```
(func (import "env" "cleat_json_stringify")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `value_ptr` | `i32` | JSON value pointer |
| `value_len` | `i32` | JSON value length |
| `out_ptr` | `i32` | Output buffer for serialized JSON |
| `out_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success, non-zero = invalid JSON |
| 32-63 | `bytesWritten` — bytes written to output buffer |

### Plugin extensions

#### 2.49 `plugin_call`

Host-only extension for plugin function calls. Not included in the Go SDK generator; available for non-Go WASM modules.

```
(func (import "env" "plugin_call")
  (param i32 i32 i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `plugin_name_ptr` | `i32` | Plugin name pointer |
| `plugin_name_len` | `i32` | Plugin name length |
| `function_name_ptr` | `i32` | Function name pointer |
| `function_name_len` | `i32` | Function name length |
| `input_ptr` | `i32` | Input JSON pointer |
| `input_len` | `i32` | Input JSON length |
| `response_ptr` | `i32` | Output buffer for response |
| `response_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` |
| 8-39 | `callErrorCode` — 0 or 1 (reserved for structured error codes) |
| 40-63 | `responseLen` — bytes written to response buffer |

#### 2.50 `plugin_call_streaming`

Host-only extension for streaming plugin function calls. Same signature as `plugin_call`; the host handles streaming at the transport layer.

```
(func (import "env" "plugin_call_streaming")
  (param i32 i32 i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `plugin_name_ptr` | `i32` | Plugin name pointer |
| `plugin_name_len` | `i32` | Plugin name length |
| `function_name_ptr` | `i32` | Function name pointer |
| `function_name_len` | `i32` | Function name length |
| `input_ptr` | `i32` | Input JSON pointer |
| `input_len` | `i32` | Input JSON length |
| `response_ptr` | `i32` | Output buffer for response |
| `response_max_len` | `i32` | Output buffer capacity (1048576) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` |
| 8-39 | `callErrorCode` — 0 or 1 (reserved for structured error codes) |
| 40-63 | `responseLen` — bytes written to response buffer |

### Previously undocumented functions

> Added 2026-08-09. This document said "52 host functions" while the actual
> registered set is 59 on both backends (56 `cleat_*` exports plus
> `plugin_call`, `plugin_call_streaming`, `set_query_state`). Re-derived with:
>
> ```
> grep -oE '\.Export\("[a-zA-Z_]+"\)' engine/imports.go | sort -u | wc -l   # wazero: 59
> grep -oE '"cleat_[a-zA-Z_]+"|"set_query_state"|"plugin_call[a-zA-Z_]*"' \
>   engine/wasmtime_hostfuncs*.go engine/backend_wasmtime*.go | cut -d: -f2 | sort -u | wc -l   # wasmtime: 59
> ```
>
> Both backends register the identical set — there is no wazero/wasmtime
> split in what is importable, only in how it is enforced (see
> `docs/explanation/security-model.md`). The seven functions below existed in
> `engine/imports.go` and the wasmtime registration files with no ABI entry
> at all. The first five are ordinary host calls; the last two
> (`cleat_poll_work`, `cleat_complete`) are internal plumbing specific to the
> Go `wasip1` export/dispatch protocol, not calls a workflow author invokes
> directly — documented here for completeness, not as an integration target
> for a new language SDK.

#### 2.53 `cleat_await_any_child`

Wait for the first of several child workflows to complete (race semantics).
Companion to `cleat_await_all_children` (§2.23), which waits for all of them.

```
(func (import "env" "cleat_await_any_child")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `run_ids_json_ptr` | `i32` | Pointer to JSON array of candidate run IDs |
| `run_ids_json_len` | `i32` | Length of the run IDs JSON |
| `result_ptr` | `i32` | Output buffer for the winning child's result |
| `result_max_len` | `i32` | Output buffer capacity |

**Return packing:** same shape as `cleat_await_child` (§2.22) — `errCode` /
`resultLen`. Implemented in `engine/children.go`
(`(*execSession).AwaitAnyChild`).

#### 2.54 `cleat_poll_child`

Non-blocking check for whether a single child workflow has completed, without
suspending. Companion to `cleat_await_child` (§2.22), which blocks.

```
(func (import "env" "cleat_poll_child")
  (param i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `run_id_ptr` | `i32` | Child run ID pointer |
| `run_id_len` | `i32` | Child run ID length |
| `result_ptr` | `i32` | Output buffer for the child's result, if complete |
| `result_max_len` | `i32` | Output buffer capacity |

**Return packing:** `errCode` / `resultLen`, as `cleat_await_child`, except
this call never suspends — an incomplete child is reported as such rather
than blocking. Implemented in `engine/children.go`
(`(*execSession).PollChild`).

#### 2.55 `cleat_schedule_cron`

Register a recurring cron trigger for a workflow. Corresponds to
`HostCalls.ScheduleCron` and the `cron` package surfaced to Go SDK authors.

```
(func (import "env" "cleat_schedule_cron")
  (param i32 i32 i32 i32 i32 i32 i32 i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `workflow_name_ptr` / `_len` | `i32` | Target workflow definition name |
| `cron_expr_ptr` / `_len` | `i32` | Cron expression |
| `timezone_ptr` / `_len` | `i32` | IANA timezone; empty means the default timezone |
| `input_ptr` / `_len` | `i32` | Input JSON for each triggered run |
| `id_ptr` | `i32` | Output buffer for the new schedule ID |
| `id_max_len` | `i32` | Output buffer capacity |

**Return packing:** `errCode` / schedule-ID length, in the same 32/32 split
used by `cleat_child_workflow_with_options` (§2.20). Implemented in
`engine/schedules.go` (`(*execSession).ScheduleCron`).

#### 2.56 `cleat_delete_cron`

Remove a previously registered cron schedule by ID.

```
(func (import "env" "cleat_delete_cron")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `schedule_id_ptr` | `i32` | Schedule ID pointer |
| `schedule_id_len` | `i32` | Schedule ID length |

**Return packing:** `errCode` only (0 = success). Implemented in
`engine/schedules.go` (`(*execSession).DeleteCron`).

#### 2.57 `cleat_list_crons`

List the calling tenant's registered cron schedules as a JSON array.

```
(func (import "env" "cleat_list_crons")
  (param i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `out_ptr` | `i32` | Output buffer for the JSON array |
| `out_max_len` | `i32` | Output buffer capacity |

**Return packing:** `errCode` / bytes written, in the same shape as
`cleat_call`. Implemented in `engine/schedules.go`
(`(*execSession).ListCrons`).

#### 2.58 `cleat_poll_work` (internal — Go `wasip1` dispatch protocol)

Not a workflow-author-facing call. Generated Go WASM `main()` stubs call this
to receive the entry point name and input before dispatching; the host-side
implementation is a stub that always returns 0 (`engine/imports.go`). A new
language SDK does not need to replicate this — it exists because of how the
Go `wasip1` export wrapper is structured, not because of anything the ABI
requires generally.

```
(func (import "env" "cleat_poll_work")
  (param i32 i32 i32 i32)
  (result i64))
```

#### 2.59 `cleat_complete` (internal — Go `wasip1` dispatch protocol)

Not a workflow-author-facing call. The generated export wrapper calls this
immediately before returning, so the worker can capture the result even if
the Go WASI runtime subsequently calls `proc_exit` (which would otherwise
overwrite the normal return value). `status=0` means the result is a JSON
success payload; `status=1` means it is an error message.

```
(func (import "env" "cleat_complete")
  (param i32 i32 i32)
  (result i64))
```

| Param | Type | Description |
|---|---|---|
| `status` | `i32` | 0 = success, 1 = error |
| `result_ptr` | `i32` | Result or error message pointer |
| `result_len` | `i32` | Result or error message length |

---

## 3. Memory Layout

### Scratch Region

The host writes input JSON and reads output from a scratch region in the WASM module's
linear memory. The region's base is **not a fixed address**: the host places it one guard
page past the end of the module's current memory, with a floor of 10 MiB
(`engine/runtime.go`):

```
scratchBase  = max(currentMemorySize + 65536, 0xA00000)
inputOffset  = scratchBase
outputOffset = scratchBase + OutBufSize
```

The 10 MiB floor exists because some SDKs (Java/TeaVM, AssemblyScript) hardcode that
convention and break if the region moves below it. A module whose heap already exceeds
10 MiB gets a correspondingly higher base.

| Offset | Size | Use |
|---|---|---|
| `scratchBase` (≥ `0xA00000`, 10 MiB) | Variable, up to `OutBufSize` | Input JSON written by host |
| `scratchBase + OutBufSize` | `OutBufSize` bytes (1048576) | Output buffer read by host |

The host grows linear memory to at least `scratchBase + 2 × OutBufSize` before calling an
export. With the region at its 10 MiB floor that is `0xC00000` (12 MiB).

`OutBufSize` is `engine/memory.go`'s `DefaultOutBufSize` (1 MiB) and is the value passed as
every `*_max_len` parameter in this document.

> This section previously described a fixed `0xA00000`/`0xA10000` layout with a 65536-byte
> output buffer and a 10 MiB + 128 KiB growth target. All three were wrong: the buffer is
> 16× larger than stated, the output offset is 1 MiB past the base rather than 64 KiB, and
> the base is dynamic. An SDK sized from the old text would under-allocate.

### WASM Page Size

1 page = 65536 bytes (64 KiB). The host uses this for memory growth calculations.

---

## 4. Language Implementations

### Reference Implementations

| Language | Status | File |
|---|---|---|
| **Go** | Production | SDK: `cleat/runtime.go`, WASM gen: `internal/wasm/` |
| **Rust** | Proof of concept | `examples/rust-workflow/src/` |

### Implementing a new language

To add support for a new language, you need:

1. **Host function declarations** — Declare the 50 `env` imports with correct WASM types.
2. **String helpers** — Read/write strings from linear memory at `(ptr, len)` pairs.
3. **Bit-packing decode** — Extract result values from packed `i64` returns per the tables above.
4. **Export wrapper** — A function with signature `(args_ptr, args_len, out_ptr, max_out_len) -> i64` that deserializes JSON args, calls the workflow, serializes the result, and encodes the packed return.
5. **Build configuration** — Target `wasm32-wasip1` (or `wasm32-unknown-unknown` if no WASI needed) and produce a shared library (`.wasm`).

The Rust implementation at `examples/rust-workflow/src/` serves as a reference for non-Go languages.

---

## 5. Changelog

| Version | Date | Changes |
|---|---|---|
| — | 2026-08-09 | Documentation-only: added §2.53-2.59 for seven host functions (`cleat_await_any_child`, `cleat_poll_child`, `cleat_schedule_cron`, `cleat_delete_cron`, `cleat_list_crons`, `cleat_poll_work`, `cleat_complete`) that were registered in `engine/imports.go` and the wasmtime backend with no ABI entry at all. Updated documentation count from 52 to 59. As with the version-number note at the top of this file: no `CurrentABIVersion` bump, because nothing about the wire contract changed -- only what this document said about it. |
| 5 | 2026-05-15 | Added Section 6: Cross-Language Determinism specification covering IEEE 754 floats, map iteration order, JSON canonicalization, GC timing, and RNG. Added cross-language replay guarantee. |
| 4 | 2026-05-13 | Added `cleat_json_parse` (2.51) and `cleat_json_stringify` (2.52) host functions for JSON validation and normalization via the host runtime. Bumped ABI_VERSION to 4. |
| 3 | 2026-05-09 | Expanded from 22 to 50 documented host functions. Added all missing imports: `cleat_continue_as_new_versioned`, `cleat_child_workflow_with_options`, `cleat_child_workflow_in_schema`, `cleat_send_signal_and_wait`, `cleat_reply_to_signal`, `cleat_signal_workflow`, `cleat_set_scope`, `cleat_get_scope`, `cleat_uuid`, `cleat_acquire_lock`, `cleat_release_lock`, `cleat_side_effect`, `cleat_workflow_id`, `cleat_run_id`, `cleat_resolve_promise`, `cleat_reject_promise`, `cleat_send`, `cleat_schedule_invoke`, `cleat_register_query_handler`, `cleat_run_detached`, `cleat_set_state`, `cleat_get_state`, `cleat_delete_state`, `cleat_incr_state`, `cleat_has_state`, `cleat_list_state`, `cleat_fetch`, `plugin_call_streaming`. Reorganized into logical groups. Updated documentation count from 18 to 50. |
| 2 | 2026-05-06 | Added `cleat_call_retry`, `cleat_call_heartbeat`, `cleat_await_all_children`, and `plugin_call` host functions. Updated documentation count. |
| 1 | 2026-05-05 | Initial ABI specification. 15 host function imports, export convention, memory layout. |

---

## 6. Cross-Language Determinism

Cleat workflows must produce identical event histories and results when replayed, regardless of which language compiled the WASM module. This section defines the determinism contract that all language SDKs must satisfy.

### 6.1 IEEE 754 Float Arithmetic

All numeric operations MUST conform to IEEE 754-2019. The cleat host runtime does not canonicalize float values in linear memory — each SDK is responsible for ensuring deterministic float behavior.

**NaN canonicalization**: All NaN values MUST be normalized to a single canonical representation when serialized to JSON or compared for equality. The canonical NaN for f64 is quiet NaN with positive sign (`0x7FF8000000000000`). SDKs SHOULD route floats through `cleat_json_stringify` (ABI 2.52) rather than language-native float-to-string conversions, as the host's `encoding/json` produces consistent float representations.

**Minimum requirements**:

| Requirement | Details |
|---|---|
| IEEE 754 conformance | Required. Implementations must follow IEEE 754 for all float operations. |
| NaN type | Use quiet NaN (not signaling). The host interprets signaling NaN as quiet NaN when read from WASM linear memory. |
| NaN canonical form | Canonicalize to `+qNaN` before any comparison or JSON output. |
| Float→string | Route through host `cleat_json_stringify` for cross-language consistency. |

### 6.2 Map Iteration Order

When workflow behavior depends on map iteration order (e.g., iterating over a `HashMap` to produce a JSON request, or selecting the "first" entry), the iteration order MUST be deterministic.

**Rule**: Map keys MUST be iterated in sorted order (lexicographic string comparison by Unicode codepoint, consistent across all languages).

**Per-language guidance**:

| Language | Deterministic map type | Non-deterministic (avoid) |
|---|---|---|
| Go | `map[key]value` + `sort.Strings(keys)` before iteration | Raw `for k, v := range m` iteration |
| Rust | `BTreeMap<K, V>` (default for `serde_json::Map`) | `HashMap<K, V>` when used with non-default hasher or when order-dependent logic is applied |
| Python | `dict` + `sorted(d.keys())` or `sort_keys=True` | Raw `for k in d` (insertion-order in 3.7+ but not guaranteed across implementations) |
| AssemblyScript | Sort keys before iteration | Raw `Map.forEach` / `for (k in map)` |

**When this matters**: Map iteration order only matters when the workflow logic depends on it. If a map is only used for lookups (get by key), iteration order is irrelevant. If iteration order affects which host call is made next or what JSON is produced, use sorted iteration.

### 6.3 JSON Serialization

All JSON output from workflows MUST be canonical to enable bit-identical cross-language comparison.

**Canonical JSON requirements**:

| Requirement | Spec |
|---|---|
| Key ordering | Sorted lexicographically by Unicode codepoint (Go `encoding/json` default) |
| Whitespace | Compact: no trailing whitespace, no spaces after `:` or `,` |
| Null handling | Explicit `null` for optional fields, not omitted |
| Numeric precision | IEEE 754 double precision (f64). Minimal representation: `1.0` not `1.00`, `1e10` not `10000000000`. No leading zeros. |
| String escaping | Minimal required: `"`, `\`, control chars (U+0000–U+001F) escaped as `\n`, `\t`, `\\`, `\"`, or `\u00XX`. Unicode safe characters left unescaped. |
| Boolean | `true` / `false` (lowercase) |
| Array | `[...]` with no trailing comma |

**Implementation**: SDKs SHOULD route workflow result JSON through the host's `cleat_json_stringify` (ABI 2.52). The host normalizes JSON through Go's `encoding/json` (parse into `interface{}`, then `json.Marshal`), which produces sorted keys and canonical formatting.

For intermediate JSON (e.g., request payloads for `cleat_call`), canonical JSON is not strictly required since the engine matches replay events by step index and service/operation, not by request content. However, canonical intermediate JSON improves debuggability and reduces the risk of non-deterministic workflow behavior.

### 6.4 GC Timing

Garbage collection timing is NOT deterministic across languages, WASM runtimes, or even across runs of the same language. Workflows MUST NOT rely on GC timing for correctness.

**Rules**:

- **No finalizer-based cleanup**: Do not use destructors, `Drop` impls, `__del__`, `finalize()`, or any finalization mechanism for workflow-semantic operations (e.g., releasing resources, sending notifications).
- **Use `cleat_defer`**: The `cleat_defer` host function (ABI 2.10) records cleanup callbacks in event history. On replay, deferred callbacks are replayed deterministically.
- **Memory pressure is non-deterministic**: The point at which GC runs depends on memory pressure, which varies across languages and runs. A workflow that behaves differently when GC runs at different points is non-deterministic.

**Example — INCORRECT** (uses Drop for cleanup):
```rust
struct Reservation { id: String }
impl Drop for Reservation {
    fn drop(&mut self) {
        // WRONG: GC-timed — not replayable
        release_reservation(&self.id);
    }
}
```

**Example — CORRECT** (uses cleat_defer):
```rust
fn place_order(h: &HostCalls, input: PlaceOrderInput) -> Result<String, String> {
    h.cleat_defer("release_inventory", &serde_json::json!({"reservation_id": reservation_id}).to_string());
    // ... payment, shipping ...
    Ok(tracking_id)
}
```

### 6.5 Random Number Generation

All random values used in workflows MUST come from `cleat_random` (ABI 2.6). The host records the returned value in event history and replays the same value on subsequent replays.

**DO NOT USE**:
- Go: `math/rand`, `crypto/rand`
- Rust: `rand::random()`, `rand::thread_rng()`
- Python: `random.random()`, `secrets.*`
- AssemblyScript: `Math.random()`

These are non-deterministic and will cause replay divergence errors.

**Example — INCORRECT**:
```go
import "math/rand"
id := rand.Int63()  // WRONG: non-deterministic — replay will diverge
```

**Example — CORRECT**:
```go
id, _ := h.DurableRandom()  // Uses cleat_random — deterministic on replay
```

Use `cleat_uuid` (ABI 2.40) for deterministic UUID generation from a seed.

### 6.6 SDK Compliance Matrix

| Requirement | Go SDK | Rust SDK | Python SDK | AssemblyScript SDK |
|---|---|---|---|---|
| IEEE 754 floats | ✅ Go float64 | ✅ f64 | ✅ float | ✅ f64 |
| NaN canonicalization | ⚠️ via host json_stringify | ⚠️ via host json_stringify (added in cleat-207) | ⚠️ verify | ⚠️ verify |
| Map iteration order | ⚠️ manual sort required | ✅ BTreeMap default for serde_json::Map | ⚠️ sort_keys=True required | ⚠️ manual sort required |
| JSON canonical output | ✅ encoding/json | ⚠️ via host json_stringify (added in cleat-207) | ⚠️ verify componentize-py | ✅ via host json_stringify |
| GC-timing independence | ✅ manual defer | ✅ manual defer | ✅ manual defer | ✅ manual defer |
| RNG via cleat_random | ✅ | ✅ | ✅ | ✅ |
| cleat_json_parse (2.51) | ✅ host import | ✅ added in cleat-207 | ✅ via WIT bindings | ✅ host import |
| cleat_json_stringify (2.52) | ✅ host import | ✅ added in cleat-207 | ✅ via WIT bindings | ✅ host import |

✅ = compliant by default or already implemented  
⚠️ = requires explicit use of the recommended pattern (manual or verified)

### 6.7 Cross-Language Replay Guarantee

When a workflow is compiled from two different languages (e.g., Go and Rust) using SDKs that comply with this section, and both implementations make the same sequence of host calls with the same service/operation pairs:

1. **Execution**: Both produce the same event history (same events in same order)
2. **Replay**: History from language A can be replayed against WASM from language B
3. **Output**: Both produce bit-identical result JSON

The cleat engine matches replay events by step index, checking `EventType`, `Service`, and `Op` for equality. Request JSON is not compared during replay matching.

**Guarantee scope**: This guarantee applies to workflows that (a) use only the host functions listed in this ABI, (b) avoid language-specific non-deterministic features (raw map iteration, GC-based cleanup, language-native RNG), and (c) produce results serializable through the host's JSON normalization.
