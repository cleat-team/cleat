# Cleat WASM ABI Specification

This document defines the exact WebAssembly contract between workflow modules and the cleat host runtime. Any language that compiles to WASM and conforms to this ABI can produce workflow modules that run on the cleat host.

## Version

ABI version: 1. The ABI is versioned separately from the workflow definition version. The host runtime supports all ABI versions it was compiled for.

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
| `max_out_len` | `i32` | Capacity of output buffer (65536 bytes) |

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

All arguments are JSON-serialized into a single object. The export function deserializes them, calls the workflow logic, and serializes the result as JSON into the output buffer.

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
| `response_max_len` | `i32` | Output buffer capacity (65536) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` — 0 = success, 1 = error |
| 8-39 | `callErrorCode` — 0 or 1 (reserved for structured error codes) |
| 40-63 | `responseLen` — bytes written to response buffer |

If `errCode == 0`, the response buffer contains valid JSON. If `errCode == 1`, the response buffer contains an error message string.

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
| `response_max_len` | `i32` | Output buffer capacity (65536) |

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
| `response_max_len` | `i32` | Output buffer capacity (65536) |

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
| `defer_id_max_len` | `i32` | Output buffer capacity (65536) |

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
| `reason_max_len` | `i32` | Output buffer capacity (65536) |

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
| `payload_max_len` | `i32` | Output buffer capacity (65536) |

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
| `sig_name_max_len` | `i32` | Output buffer capacity (65536) |
| `payload_ptr` | `i32` | Output buffer for signal payload |
| `payload_max_len` | `i32` | Output buffer capacity (65536) |

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
| `resp_max_len` | `i32` | Output buffer capacity (65536) |

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
| `run_id_max_len` | `i32` | Output buffer capacity (65536) |

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
| `run_id_max_len` | `i32` | Output buffer capacity (65536) |

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
| `run_id_max_len` | `i32` | Output buffer capacity (65536) |

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
| `result_max_len` | `i32` | Output buffer capacity (65536) |

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
| `results_max_len` | `i32` | Output buffer capacity (65536) |

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

Register a read-only query handler for the workflow.

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
| `value_max_len` | `i32` | Output buffer capacity (65536) |

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
| `keys_max_len` | `i32` | Output buffer capacity (65536) |

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
| `promise_id_out_max` | `i32` | Output buffer capacity (65536) |

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
| `result_out_max` | `i32` | Output buffer capacity (65536) |

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
| `prev_scope_max_len` | `i32` | Output buffer capacity (65536) |

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
| `obj_type_max_len` | `i32` | Output buffer capacity (65536) |
| `inst_key_ptr` | `i32` | Output buffer for instance key |
| `inst_key_max_len` | `i32` | Output buffer capacity (65536) |

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
| `uuid_max_len` | `i32` | Output buffer capacity (65536) |

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
| `out_max_len` | `i32` | Output buffer capacity (65536) |

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
| `id_max_len` | `i32` | Output buffer capacity (65536) |

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
| `id_max_len` | `i32` | Output buffer capacity (65536) |

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
| `response_max_len` | `i32` | Output buffer capacity (65536) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-31 | `errCode` — 0 = success |
| 32-63 | `responseLen` — bytes written to response buffer |

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
| `response_max_len` | `i32` | Output buffer capacity (65536) |

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
| `response_max_len` | `i32` | Output buffer capacity (65536) |

**Return packing:**

| Bits | Meaning |
|---|---|
| 0-7 | `errCode` |
| 8-39 | `callErrorCode` — 0 or 1 (reserved for structured error codes) |
| 40-63 | `responseLen` — bytes written to response buffer |

---

## 3. Memory Layout

### Scratch Region

The host writes input JSON and reads output at fixed offsets in the WASM module's linear memory:

| Offset | Size | Use |
|---|---|---|
| `0xA00000` (10 MiB) | Variable | Input JSON written by host |
| `0xA10000` (10 MiB + 64 KiB) | 65536 bytes | Output buffer read by host |

The host ensures linear memory is at least `0xA20000` bytes (10 MiB + 128 KiB) before calling an export. If the module's memory is smaller, the host grows it (`memory.grow`).

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
| 3 | 2026-05-09 | Expanded from 22 to 50 documented host functions. Added all missing imports: `cleat_continue_as_new_versioned`, `cleat_child_workflow_with_options`, `cleat_child_workflow_in_schema`, `cleat_send_signal_and_wait`, `cleat_reply_to_signal`, `cleat_signal_workflow`, `cleat_set_scope`, `cleat_get_scope`, `cleat_uuid`, `cleat_acquire_lock`, `cleat_release_lock`, `cleat_side_effect`, `cleat_workflow_id`, `cleat_run_id`, `cleat_resolve_promise`, `cleat_reject_promise`, `cleat_send`, `cleat_schedule_invoke`, `cleat_register_query_handler`, `cleat_run_detached`, `cleat_set_state`, `cleat_get_state`, `cleat_delete_state`, `cleat_incr_state`, `cleat_has_state`, `cleat_list_state`, `cleat_fetch`, `plugin_call_streaming`. Reorganized into logical groups. Updated documentation count from 18 to 50. |
| 2 | 2026-05-06 | Added `cleat_call_retry`, `cleat_call_heartbeat`, `cleat_await_all_children`, and `plugin_call` host functions. Updated documentation count. |
| 1 | 2026-05-05 | Initial ABI specification. 15 host function imports, export convention, memory layout. |
