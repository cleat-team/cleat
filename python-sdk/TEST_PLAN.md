# Python WASM End-to-End Test Plan

## Overview

This document describes the end-to-end validation strategy for the Python-to-WASM
workflow execution path in the cleat durable execution engine. The pipeline is:

```
Python workflow  -->  componentize-py  -->  WASM Component Model binary
  -->  wasm-tools decompose  -->  Core WASM module
  -->  wazero runtime  -->  Go host (cleat-engine)
  -->  execute exports, service calls, signals, timers, ...
  -->  result & event history
```

Two levels of testing are provided:

1. **`TestPythonWasmAbiBoundary`** (static) — runs without any external tools.
2. **`TestPythonWasmEndToEnd`** (full pipeline) — requires componentize-py and
   wasm-tools to be installed.

---

## 1. ABI Boundary Test (Static)

**File:** `internal/host/python_wasm_e2e_test.go`
**Function:** `TestPythonWasmAbiBoundary`

This test verifies that every host function import the Python SDK expects is
registered in the Go host runtime. It does not require any WASM toolchain and
runs as part of the regular `go test ./internal/host/` suite.

### What it checks

1. **Import name coverage** — Iterates over the 36 host-call names that the
   Python SDK stubs (`host_calls.py`) expect under `(import "env" "<name>")`,
   and verifies each one appears in the Go host's `registerHostFunctions()`
   registrations within `internal/host/imports.go`. The source of truth for Go
   registrations is `registeredImportNames()` in the test file.

2. **Bit-packing conventions** — Five sub-tests verify that the bit layout used
   by Go's `memory.go` pack/unpack functions matches what the Python SDK's
   `memory.py` decoders expect:

   - `testBitPackingCleatCall` — `packDurableCallResult`:
     - bits 40-63 = responseLen (24-bit)
     - bits 8-39  = callErrorCode (32-bit)
     - bits 0-7   = errCode (8-bit)

   - `testBitPackingSleep` — `packSleepResult`:
     - bits 56-63 = status (8-bit)
     - bits 0-55  = durationMs (56-bit)

   - `testBitPackingAwaitSignals` — `packAwaitSignalsResult`:
     - bits 48-63 = sigNameLen (16-bit)
     - bits 32-47 = payloadLen (16-bit)
     - bits 16-31 = timedOut (16-bit)
     - bits 0-15  = errCode (16-bit)

   - `testBitPackingSimpleResult` — `packSimpleResult`:
     - bits 32-63 = extra (32-bit)
     - bits 0-7   = errCode (8-bit)

   - `testBitPackingExportResult` — `decodeExportResult` / `encode_export_result`:
     - bits 32-63 = actualLen (32-bit)
     - bits 0-31  = errCode (32-bit)

### Running

```shell
go test ./internal/host/ -run TestPythonWasmAbiBoundary -v
```

No prerequisites required. This test always runs (except in short mode if
the parent test suite uses `-short`).

---

## 2. End-to-End Pipeline Test (Full Integration)

**File:** `internal/host/python_wasm_e2e_test.go`
**Function:** `TestPythonWasmEndToEnd`

### Pipeline steps

| Step | Description | Tool | Output |
|------|-------------|------|--------|
| 1 | Compile `durable_call_workflow.py` to a WASM Component Model binary | `componentize-py` | `durable_call_workflow.wasm` |
| 2 | Decompose component model to a core WASM module | `wasm-tools component decompose` | `core.wasm` (or pass-through if already a core module) |
| 3 | Load core module into wazero runtime | `internal/host/runtime.go` — `NewRuntime()` | Wazero `Runtime` + `api.Module` |
| 4 | Call the `run` export with a JSON input | `Runtime.CallExport()` | Raw `i64` result |
| 5 | Decode result and event history | `Engine.Execute()` | `result`, `history`, `suspended`, `deferrals`, `queryState` |
| 6 | Verify event history contains expected `cleat_call` and `cleat_log` events | Test assertions | Pass/fail |
| 7 | Verify result is valid JSON | `json.Unmarshal` | Pass/fail |

### Workflow under test

**File:** `python-sdk/examples/durable_call_workflow.py`

The workflow:
1. Receives a `NotifyRequest` (user_id, message, channel)
2. Logs the start of processing via `h.cleat_log`
3. Makes a DurableCall to `notifier.SendNotification`
4. Logs the notification result
5. Returns the service response

### Mock service

The test uses `mockCaller` which implements `ServiceCaller` and returns
a canned response for `notifier.SendNotification`:

```json
{"status": "sent", "channel": "email", "message_id": "msg-123"}
```

### Expected event history

| Index | EventType | Details |
|-------|-----------|---------|
| 0 | `cleat_log` | "Notifying user u-test-42 via email" |
| 1 | `cleat_call` | Service=`notifier`, Op=`SendNotification` |
| 2 | `cleat_log` | "Notification result: ..." |

### Input

```json
{"request":{"user_id":"u-test-42","message":"Hello from Python WASM","channel":"email"}}
```

### Running

```shell
go test ./internal/host/ -run TestPythonWasmEndToEnd -v
```

---

## 3. Prerequisites

### Required tools

| Tool | Minimum version | Install command |
|------|----------------|-----------------|
| `componentize-py` | >= 0.12.0 | `pip install componentize-py` |
| `wasm-tools` | latest | `cargo install wasm-tools` |
| `python3` | >= 3.10 | system package |
| `cleat-sdk` | local | `pip install -e python-sdk/` |

### Why decomposition is needed

`componentize-py` produces WASM Component Model binaries (wrapped in the
component model layer). The wazero runtime only supports core WASM modules.
Therefore the binary must be decomposed using `wasm-tools component decompose`
to extract the core module before loading into wazero.

If `wasm-tools` is unavailable, the test checks whether the binary is already
a core module by attempting a direct load. This enables testing with pre-decomposed
modules but is not the primary path.

### Environment verification

The `pythonWasmTestHelper` struct checks for all prerequisites in
`toolsAvailable()`:

- `componentize-py` on PATH
- `wasm-tools` on PATH
- `python3` on PATH
- `python-sdk/examples/` directory exists

If any are missing, the test skips with a descriptive message.

---

## 4. CI Integration

### Current status

The e2e test (`TestPythonWasmEndToEnd`) is **not yet runnable in CI** because
neither `componentize-py` nor `wasm-tools` are installed in the build environment.
The test correctly skips with a clear message when prerequisites are missing.

### To enable in CI

1. Install `componentize-py`:
   ```shell
   pip install componentize-py>=0.12.0
   ```

2. Install `wasm-tools`:
   ```shell
   cargo install wasm-tools
   ```

3. Install the SDK in development mode:
   ```shell
   pip install -e python-sdk/
   ```

4. Run the test:
   ```shell
   go test ./internal/host/ -run TestPythonWasmEndToEnd -v
   ```

### Short mode

Both tests respect `testing.Short()`. When running with `go test -short`,
`TestPythonWasmEndToEnd` is skipped.

---

## 5. ABI Contract Summary

The Python WASM ABI is defined by the WIT file at `python-sdk/wit/cleat.wit`.
All host functions are imported as `(import "env" "<name>")` with the following
calling convention:

### String parameters

Strings are passed as two `i32` values: pointer (offset in WASM linear memory)
and length. The scratch space starts at `SCRATCH_BASE = 0xA00000`.

### Return values

All host functions return a single `i64` with bit-packed fields. The bit layout
varies by function family (see section 1 for details).

### Export signature

The workflow entry point (the WIT `run` export) has the signature:

```wit
run: func(args-ptr: u32, args-len: u32, out-ptr: u32, max-out-len: u32) -> u64
```

The `u64` return is bit-packed:
- bits 0-31  = errCode
- bits 32-63 = actualLen (bytes written to `out-ptr`)

### Host function registration

The following 36 host functions are registered in `internal/host/imports.go`
and verified by `TestPythonWasmAbiBoundary`:

```
cleat_call, cleat_call_retry, cleat_call_heartbeat,
cleat_sleep, cleat_now, cleat_random,
cleat_log, cleat_version, cleat_min_version,
cleat_defer, cleat_poll_cancellation, cleat_poll_signal,
cleat_continue_as_new, cleat_continue_as_new_versioned,
cleat_child_workflow, cleat_child_workflow_with_options,
cleat_await_child, cleat_await_signals, cleat_await_all_children,
set_query_state, cleat_side_effect,
plugin_call, plugin_call_streaming,
cleat_create_promise, cleat_await_promise,
cleat_resolve_promise, cleat_reject_promise,
cleat_send, cleat_schedule_invoke,
cleat_register_update_handler, cleat_register_query_handler,
cleat_workflow_id, cleat_run_id,
cleat_send_signal_and_wait, cleat_reply_to_signal,
cleat_signal_workflow,
cleat_acquire_lock, cleat_release_lock
```

---

## 6. Makefile Targets

The `python-sdk/Makefile` provides the following targets for WASM compilation:

| Target | Description |
|--------|-------------|
| `make wasm` | Compile all example workflows (including `durable_call_workflow`) to WASM |
| `make stamped` | Compile and stamp metadata onto all workflows |
| `make durable_call_workflow.wasm` | Compile a single workflow |
| `make test-wasm` | Compile and run WASM-specific Python tests |
| `make clean` | Remove all WASM artifacts |

---

## 7. Future Work

- **Pre-compiled WASM modules** — Check in pre-compiled `.wasm` blobs for CI
  so the e2e test can run without the toolchain.
- **Docker-based toolchain** — Provide a Docker image with componentize-py and
  wasm-tools pre-installed for CI and local development.
- **More workflow patterns** — Add tests for child workflows, timers, signals,
  side effects, continue-as-new, and error handling across the ABI boundary.
- **Property-based bit-packing tests** — Use fuzzing to verify that random
  values round-trip correctly through the Go and Python encode/decode functions.
