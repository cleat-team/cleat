/**
 * AssemblyScript bindings for the 15 cleat WASM host function imports.
 *
 * Provides raw `@external` import declarations and the `HostCalls` class
 * that wraps each import with idiomatic AssemblyScript methods.
 *
 * Matches the Rust SDK at crates/durable-sdk/src/host_calls.rs and
 * the ABI specification in ABI.md.
 */

import {
  Memory,
  decodeCallResult,
  decodeSleepResult,
  decodeSimpleResult,
  decodeAwaitSignalsResult,
  decodePollSignalResult,
  decodePollCancellationResult,
  OUT_BUF_SIZE,
  SCRATCH_BASE,
  OUTPUT_OFFSET,
} from "./memory";

// ═══════════════════════════════════════════════
// 15 raw host function imports from "env" module
// ═══════════════════════════════════════════════

/**
 * 1. durable_sleep: Suspend workflow execution for a duration.
 * (import "env" "durable_sleep") (param i64) (result i64)
 */
@external("env", "durable_sleep")
export declare function import_durable_sleep(durationMs: i64): i64;

/**
 * 2. durable_now: Get current wall-clock time.
 * (import "env" "durable_now") (result i64)
 */
@external("env", "durable_now")
export declare function import_durable_now(): i64;

/**
 * 3. durable_random: Get a deterministic random value.
 * (import "env" "durable_random") (result i64)
 */
@external("env", "durable_random")
export declare function import_durable_random(): i64;

/**
 * 4. durable_log: Log a message to the host.
 * (import "env" "durable_log") (param i32 i32) (result i64)
 */
@external("env", "durable_log")
export declare function import_durable_log(msgPtr: i32, msgLen: i32): i64;

/**
 * 5. durable_version: Get the workflow definition version.
 * (import "env" "durable_version") (result i64)
 */
@external("env", "durable_version")
export declare function import_durable_version(): i64;

/**
 * 6. durable_min_version: Get the minimum supported version.
 * (import "env" "durable_min_version") (result i64)
 */
@external("env", "durable_min_version")
export declare function import_durable_min_version(): i64;

/**
 * 7. durable_defer: Register cleanup to run on workflow exit.
 * (import "env" "durable_defer") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_defer")
export declare function import_durable_defer(
  descPtr: i32,
  descLen: i32,
  deferIdPtr: i32,
  deferIdMaxLen: i32,
): i64;

/**
 * 8. durable_poll_cancellation: Check for cancellation request.
 * (import "env" "durable_poll_cancellation") (param i32 i32) (result i64)
 */
@external("env", "durable_poll_cancellation")
export declare function import_durable_poll_cancellation(
  reasonPtr: i32,
  reasonMaxLen: i32,
): i64;

/**
 * 9. durable_poll_signal: Poll for a specific pending signal.
 * (import "env" "durable_poll_signal") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_poll_signal")
export declare function import_durable_poll_signal(
  namePtr: i32,
  nameLen: i32,
  payloadPtr: i32,
  payloadMaxLen: i32,
): i64;

/**
 * 10. durable_continue_as_new: Start a new workflow run with fresh input.
 * (import "env" "durable_continue_as_new") (param i32 i32) (result i64)
 */
@external("env", "durable_continue_as_new")
export declare function import_durable_continue_as_new(
  inputPtr: i32,
  inputLen: i32,
): i64;

/**
 * 11. durable_child_workflow: Start a child workflow instance.
 * (import "env" "durable_child_workflow") (param i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_child_workflow")
export declare function import_durable_child_workflow(
  namePtr: i32,
  nameLen: i32,
  inputPtr: i32,
  inputLen: i32,
  runIdPtr: i32,
  runIdMaxLen: i32,
): i64;

/**
 * 12. durable_await_child: Wait for a child workflow to complete.
 * (import "env" "durable_await_child") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_await_child")
export declare function import_durable_await_child(
  runIdPtr: i32,
  runIdLen: i32,
  resultPtr: i32,
  resultMaxLen: i32,
): i64;

/**
 * 13. durable_await_signals: Wait for external signals with timeout.
 * (import "env" "durable_await_signals") (param i32 i32 i64 i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_await_signals")
export declare function import_durable_await_signals(
  namesPtr: i32,
  namesLen: i32,
  timeoutMs: i64,
  sigNamePtr: i32,
  sigNameMaxLen: i32,
  payloadPtr: i32,
  payloadMaxLen: i32,
): i64;

/**
 * 14. set_query_state: Set a key-value pair in query state.
 * (import "env" "set_query_state") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "set_query_state")
export declare function import_set_query_state(
  keyPtr: i32,
  keyLen: i32,
  valPtr: i32,
  valLen: i32,
): i64;

/**
 * 15. durable_call: Make a recorded API call to an external service.
 * (import "env" "durable_call") (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_call")
export declare function import_durable_call(
  svcPtr: i32,
  svcLen: i32,
  opPtr: i32,
  opLen: i32,
  reqPtr: i32,
  reqLen: i32,
  respPtr: i32,
  respMaxLen: i32,
): i64;

// ═══════════════════════════════════════════════
// High-level result types for HostCalls methods
// ═══════════════════════════════════════════════

/**
 * Generic result type representing either a success value or an error
 * message. Used by HostCalls methods that can fail with a descriptive
 * error string.
 */
export class DurableResult<T> {
  constructor(
    /** The success value. Only meaningful when `error` is null. */
    public readonly value: T,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this result carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Outcome of a `durableCall` operation. */
export class DurableCallOutcome {
  constructor(
    /** Response JSON from the service call. Empty on error. */
    public readonly response: string,
    /** Error message from the host, or null on success. */
    public readonly error: string | null,
    /** Structured call error code (reserved for future use). */
    public readonly callErrorCode: u32,
  ) {}

  /** Returns true when this outcome carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Result of `pollCancellation`. */
export class CancellationStatus {
  constructor(
    /** Whether cancellation has been requested. */
    public readonly cancelled: bool,
    /** Cancellation reason string. Empty if not cancelled. */
    public readonly reason: string,
  ) {}
}

/** Result of `pollSignal`. */
export class PollSignalOutcome {
  constructor(
    /** Signal payload JSON. Empty if not found or on error. */
    public readonly payload: string,
    /** Whether the signal was found. */
    public readonly found: bool,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this outcome carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Result of `awaitSignals`. */
export class AwaitSignalsOutcome {
  constructor(
    /** Name of the received signal. Empty on timeout or error. */
    public readonly signalName: string,
    /** Signal payload JSON. Empty on timeout or error. */
    public readonly payload: string,
    /** Whether the wait timed out without receiving any matching signal. */
    public readonly timedOut: bool,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this outcome carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

// ═══════════════════════════════════════════════
// HostCalls wrapper
// ═══════════════════════════════════════════════

/**
 * High-level AssemblyScript wrapper around the 15 cleat WASM host function
 * imports.
 *
 * Each method handles string I/O (encode input strings to memory, decode
 * output strings from memory), calls the raw import, and decodes the
 * bit-packed result into an idiomatic AssemblyScript return value.
 *
 * Usage:
 * ```ts
 * let host = new HostCalls();
 * let outcome = host.durableCall("payment", "charge", `{"amount": 100}`);
 * if (outcome.isError) {
 *   host.log("payment failed: " + outcome.error);
 * }
 * ```
 *
 * Mirrors Rust SDK `crates/durable-sdk/src/host_calls.rs` and
 * Go `durable.HostCalls` interface.
 */
export class HostCalls {
  /** Memory helper for string I/O in linear memory. */
  protected memory: Memory;

  /**
   * @param memory - Optional Memory instance. A default one is created if
   *                 not provided.
   */
  constructor(memory: Memory = new Memory()) {
    this.memory = memory;
  }

  // ────────────────────────────────────────────
  // 1. durable_call
  // ────────────────────────────────────────────

  /**
   * Make a recorded API call to an external service.
   *
   * Service, operation, and request JSON are encoded to the scratch buffer,
   * the host call is made, and the response is read from the output buffer.
   *
   * @param service      - Service name (e.g., "payment", "email").
   * @param operation    - Operation name (e.g., "charge", "send").
   * @param requestJson  - Request payload as a JSON string.
   * @returns The call outcome containing response JSON or error details.
   */
  durableCall(service: string, operation: string, requestJson: string): DurableCallOutcome {
    // Encode input strings sequentially into the scratch buffer
    let svcLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, service);
    let opOffset: usize = SCRATCH_BASE + svcLen;
    let opLen: i32 = this.memory.writeString(opOffset, OUT_BUF_SIZE, operation);
    let reqOffset: usize = opOffset + opLen;
    let reqLen: i32 = this.memory.writeString(reqOffset, OUT_BUF_SIZE, requestJson);

    // Call the host import
    let result: i64 = import_durable_call(
      SCRATCH_BASE as i32,
      svcLen,
      opOffset as i32,
      opLen,
      reqOffset as i32,
      reqLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Decode the packed result
    let decoded = decodeCallResult(result);
    let responseLen: i32 = decoded.responseLen as i32;

    // On error, the output buffer contains an error message
    if (decoded.errCode !== 0) {
      let errMsg: string =
        responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "unknown error";
      return new DurableCallOutcome("", errMsg, decoded.callErrorCode);
    }

    // Success: read the response
    let resp: string =
      responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "";
    return new DurableCallOutcome(resp, null, 0);
  }

  // ────────────────────────────────────────────
  // 2. durable_sleep
  // ────────────────────────────────────────────

  /**
   * Suspend workflow execution for a duration.
   *
   * On fresh execution, returns `true` to signal that the workflow should
   * suspend. On replay, returns `false` (the sleep already completed).
   *
   * @param durationMs - Sleep duration in milliseconds.
   * @returns `true` if the workflow should suspend, `false` if completed.
   */
  durableSleep(durationMs: i64): bool {
    let result: i64 = import_durable_sleep(durationMs);
    let decoded = decodeSleepResult(result);
    return decoded.status === 1; // SLEEP_STATUS_SUSPEND
  }

  // ────────────────────────────────────────────
  // 3. now
  // ────────────────────────────────────────────

  /**
   * Get the current wall-clock time.
   *
   * @returns Current time in milliseconds since Unix epoch.
   */
  now(): i64 {
    return import_durable_now();
  }

  // ────────────────────────────────────────────
  // 4. random
  // ────────────────────────────────────────────

  /**
   * Get a deterministic random value.
   *
   * The same value is returned on replay, ensuring determinism.
   *
   * @returns A deterministic i64 value.
   */
  random(): i64 {
    return import_durable_random();
  }

  // ────────────────────────────────────────────
  // 5. log
  // ────────────────────────────────────────────

  /**
   * Log a message to the host runtime.
   *
   * @param message - The message to log.
   */
  log(message: string): void {
    let msgLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, message);
    import_durable_log(SCRATCH_BASE as i32, msgLen);
  }

  // ────────────────────────────────────────────
  // 6. version
  // ────────────────────────────────────────────

  /**
   * Get the workflow definition version.
   *
   * @returns The current workflow version as a 32-bit integer.
   */
  version(): i32 {
    return import_durable_version() as i32;
  }

  // ────────────────────────────────────────────
  // 7. minVersion
  // ────────────────────────────────────────────

  /**
   * Get the minimum supported version for this workflow definition.
   *
   * @returns The minimum version as a 32-bit integer.
   */
  minVersion(): i32 {
    return import_durable_min_version() as i32;
  }

  // ────────────────────────────────────────────
  // 8. defer
  // ────────────────────────────────────────────

  /**
   * Register cleanup to run on workflow exit.
   *
   * @param description - Human-readable description of the deferred action.
   * @returns A DurableResult containing the defer ID on success.
   */
  defer(description: string): DurableResult<string> {
    let descLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, description);

    let result: i64 = import_durable_defer(
      SCRATCH_BASE as i32,
      descLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "defer error code: " + decoded.errCode.toString(),
      );
    }

    let deferId: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(deferId, null);
  }

  // ────────────────────────────────────────────
  // 9. pollCancellation
  // ────────────────────────────────────────────

  /**
   * Check if workflow cancellation has been requested.
   *
   * @returns The cancellation status, including any reason string.
   */
  pollCancellation(): CancellationStatus {
    let result: i64 = import_durable_poll_cancellation(OUTPUT_OFFSET as i32, OUT_BUF_SIZE);
    let decoded = decodePollCancellationResult(result);

    let reason: string =
      decoded.cancelled && decoded.reasonLen > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.reasonLen as i32)
        : "";

    return new CancellationStatus(decoded.cancelled, reason);
  }

  // ────────────────────────────────────────────
  // 10. pollSignal
  // ────────────────────────────────────────────

  /**
   * Poll for a specific pending signal.
   *
   * @param name - The signal name to poll for.
   * @returns The signal outcome with payload and found status.
   */
  pollSignal(name: string): PollSignalOutcome {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);

    let result: i64 = import_durable_poll_signal(
      SCRATCH_BASE as i32,
      nameLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodePollSignalResult(result);

    if (decoded.errCode !== 0) {
      return new PollSignalOutcome(
        "",
        false,
        "signal error code: " + decoded.errCode.toString(),
      );
    }

    let payload: string =
      decoded.found && decoded.payloadLen > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.payloadLen as i32)
        : "";

    return new PollSignalOutcome(payload, decoded.found, null);
  }

  // ────────────────────────────────────────────
  // 11. continueAsNew
  // ────────────────────────────────────────────

  /**
   * Start a new workflow run with fresh input (history compaction).
   *
   * After this call, the workflow should return the suspension sentinel
   * to let the host restart it with the new input.
   *
   * @param inputJson - New input JSON for the restarted workflow.
   * @returns An error message on failure, or `null` on success.
   */
  continueAsNew(inputJson: string): string | null {
    let inputLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, inputJson);

    let result: i64 = import_durable_continue_as_new(SCRATCH_BASE as i32, inputLen);
    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return "continue_as_new error code: " + decoded.errCode.toString();
    }

    return null;
  }

  // ────────────────────────────────────────────
  // 12. childWorkflow
  // ────────────────────────────────────────────

  /**
   * Start a child workflow instance.
   *
   * @param name      - Child workflow definition name.
   * @param inputJson - Input JSON for the child workflow.
   * @returns A DurableResult containing the child run ID on success.
   */
  childWorkflow(name: string, inputJson: string): DurableResult<string> {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);
    let inputOffset: usize = SCRATCH_BASE + nameLen;
    let inputLen: i32 = this.memory.writeString(inputOffset, OUT_BUF_SIZE, inputJson);

    let result: i64 = import_durable_child_workflow(
      SCRATCH_BASE as i32,
      nameLen,
      inputOffset as i32,
      inputLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "child_workflow error code: " + decoded.errCode.toString(),
      );
    }

    let runId: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(runId, null);
  }

  // ────────────────────────────────────────────
  // 13. awaitChild
  // ────────────────────────────────────────────

  /**
   * Wait for a child workflow to complete.
   *
   * If the child is not yet complete, the workflow should suspend by
   * returning the suspension sentinel.
   *
   * @param runId - The child workflow run ID.
   * @returns A DurableResult containing the child's result JSON on success.
   */
  awaitChild(runId: string): DurableResult<string> {
    let runIdLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, runId);

    let result: i64 = import_durable_await_child(
      SCRATCH_BASE as i32,
      runIdLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "await_child error code: " + decoded.errCode.toString(),
      );
    }

    let childResult: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(childResult, null);
  }

  // ────────────────────────────────────────────
  // 14. awaitSignals
  // ────────────────────────────────────────────

  /**
   * Wait for one or more external signals, with a timeout.
   *
   * Signal names are passed as a JSON array string, e.g.,
   * `'["payment_received","order_cancelled"]'`.
   *
   * The scratch buffer is split: the first portion holds the input
   * (signal names JSON), and the remainder serves as the payload output
   * buffer. The output buffer at `OUTPUT_OFFSET` holds the received
   * signal name.
   *
   * @param namesJson - JSON array of signal names to wait for.
   * @param timeoutMs - Timeout in milliseconds.
   * @returns The outcome with signal name, payload, and timeout status.
   */
  awaitSignals(namesJson: string, timeoutMs: i64): AwaitSignalsOutcome {
    // Write the signal names JSON into the lower portion of the scratch buffer
    let namesLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE / 2, namesJson);

    // The upper half of the scratch buffer is used for the payload output
    let payloadOffset: usize = SCRATCH_BASE + OUT_BUF_SIZE / 2;
    let payloadMaxLen: i32 = OUT_BUF_SIZE / 2;

    let result: i64 = import_durable_await_signals(
      SCRATCH_BASE as i32,
      namesLen,
      timeoutMs,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
      payloadOffset as i32,
      payloadMaxLen,
    );

    let decoded = decodeAwaitSignalsResult(result);

    if (decoded.errCode !== 0) {
      return new AwaitSignalsOutcome(
        "",
        "",
        false,
        "await_signals error code: " + decoded.errCode.toString(),
      );
    }

    let sigName: string =
      decoded.sigNameLen > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.sigNameLen as i32)
        : "";

    let payload: string =
      !decoded.timedOut && decoded.payloadLen > 0
        ? this.memory.readString(payloadOffset, decoded.payloadLen as i32)
        : "";

    return new AwaitSignalsOutcome(sigName, payload, decoded.timedOut, null);
  }

  // ────────────────────────────────────────────
  // 15. setQueryState
  // ────────────────────────────────────────────

  /**
   * Set a key-value pair in the workflow's query state.
   *
   * Query state can be read by external clients while the workflow is
   * running, enabling interactive queries.
   *
   * @param key   - Query state key.
   * @param value - Query state value.
   */
  setQueryState(key: string, value: string): void {
    let keyLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, key);
    let valOffset: usize = SCRATCH_BASE + keyLen;
    let valLen: i32 = this.memory.writeString(valOffset, OUT_BUF_SIZE, value);

    import_set_query_state(SCRATCH_BASE as i32, keyLen, valOffset as i32, valLen);
  }
}
