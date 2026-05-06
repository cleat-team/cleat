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

### 2.1 `durable_call`

Make a recorded API call to an external service.

```
(func (import "env" "durable_call")
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

### 2.2 `durable_sleep`

Suspend workflow execution for a duration.

```
(func (import "env" "durable_sleep") (param i64) (result i64))
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

### 2.3 `durable_now`

Get current wall-clock time.

```
(func (import "env" "durable_now") (result i64))
```

Returns: current time in **milliseconds since Unix epoch** as `i64`.

### 2.4 `durable_random`

Get a deterministic random value.

```
(func (import "env" "durable_random") (result i64))
```

Returns: deterministic `i64` value. The same value is returned on replay.

### 2.5 `durable_log`

Log a message to the host.

```
(func (import "env" "durable_log") (param i32 i32) (result i64))
```

| Param | Type | Description |
|---|---|---|
| `message_ptr` | `i32` | Log message pointer |
| `message_len` | `i32` | Log message length |

Return value is ignored.

### 2.6 `durable_version`

Get the workflow definition version.

```
(func (import "env" "durable_version") (result i64))
```

Returns: workflow definition version as `i64` (cast from `uint32`).

### 2.7 `durable_min_version`

Get the minimum supported version for this workflow definition.

```
(func (import "env" "durable_min_version") (result i64))
```

Returns: minimum version as `i64` (cast from `uint32`).

### 2.8 `durable_defer`

Register a cleanup callback to run on workflow exit.

```
(func (import "env" "durable_defer")
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

### 2.9 `durable_poll_cancellation`

Check if workflow cancellation has been requested.

```
(func (import "env" "durable_poll_cancellation")
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

### 2.10 `durable_poll_signal`

Poll for a specific pending signal.

```
(func (import "env" "durable_poll_signal")
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

### 2.11 `durable_continue_as_new`

Start a new workflow run with fresh input (history compaction).

```
(func (import "env" "durable_continue_as_new")
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

### 2.12 `durable_child_workflow`

Start a child workflow instance.

```
(func (import "env" "durable_child_workflow")
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

### 2.13 `durable_await_child`

Wait for a child workflow to complete.

```
(func (import "env" "durable_await_child")
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

### 2.14 `durable_await_signals`

Wait for one or more external signals, with a timeout.

```
(func (import "env" "durable_await_signals")
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

### 2.15 `set_query_state`

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

### 2.16 `durable_call_retry`

Server-side retry variant of `durable_call`. Retries happen inside the host; one event is recorded regardless of attempt count.

```
(func (import "env" "durable_call_retry")
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

### 2.17 `durable_call_heartbeat`

Long-running call with progress updates. The host sends periodic progress updates; the progress callback is handled at the SDK layer.

```
(func (import "env" "durable_call_heartbeat")
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

### 2.18 `durable_await_all_children`

Batch await for multiple child workflows. Returns a JSON array of child results.

```
(func (import "env" "durable_await_all_children")
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

### 2.19 `plugin_call`

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
| **Go** | Production | SDK: `durable/runtime.go`, WASM gen: `internal/wasm/` |
| **Rust** | Proof of concept | `examples/rust-workflow/src/` |

### Implementing a new language

To add support for a new language, you need:

1. **Host function declarations** — Declare the 18 `env` imports (plus the optional `plugin_call` extension) with correct WASM types.
2. **String helpers** — Read/write strings from linear memory at `(ptr, len)` pairs.
3. **Bit-packing decode** — Extract result values from packed `i64` returns per the tables above.
4. **Export wrapper** — A function with signature `(args_ptr, args_len, out_ptr, max_out_len) -> i64` that deserializes JSON args, calls the workflow, serializes the result, and encodes the packed return.
5. **Build configuration** — Target `wasm32-wasip1` (or `wasm32-unknown-unknown` if no WASI needed) and produce a shared library (`.wasm`).

The Rust implementation at `examples/rust-workflow/src/` serves as a reference for non-Go languages.

---

## 5. Changelog

| Version | Date | Changes |
|---|---|---|
| 2 | 2026-05-06 | Added `durable_call_retry`, `durable_call_heartbeat`, `durable_await_all_children`, and `plugin_call` host functions. Updated documentation count. |
| 1 | 2026-05-05 | Initial ABI specification. 15 host function imports, export convention, memory layout. |
