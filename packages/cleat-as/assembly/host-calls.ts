/**
 * AssemblyScript bindings for the 19 cleat WASM host function imports.
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
  decodeAwaitPromiseResult,
  OUT_BUF_SIZE,
  SCRATCH_BASE,
  OUTPUT_OFFSET,
  setWorkflowSuspended,
} from "./memory";

// ═══════════════════════════════════════════════
// 21 raw host function imports from "env" module
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

/**
 * 16. durable_create_promise: Create a new durable promise.
 * (import "env" "durable_create_promise") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_create_promise")
export declare function import_durable_create_promise(
  namePtr: i32,
  nameLen: i32,
  idOutPtr: i32,
  idOutMax: i32,
): i64;

/**
 * 17. durable_await_promise: Wait for a durable promise to resolve.
 * (import "env" "durable_await_promise") (param i32 i32 i64 i32 i32) (result i64)
 */
@external("env", "durable_await_promise")
export declare function import_durable_await_promise(
  idPtr: i32,
  idLen: i32,
  timeoutMs: i64,
  resultOutPtr: i32,
  resultOutMax: i32,
): i64;

/**
 * 18. durable_register_update_handler: Register a handler for update calls.
 * (import "env" "durable_register_update_handler") (param i32 i32) (result i64)
 */
@external("env", "durable_register_update_handler")
export declare function import_durable_register_update_handler(
  namePtr: i32,
  nameLen: i32,
): i64;

/**
 * 19. plugin_call: Host-only extension for plugin function calls.
 * (import "env" "plugin_call") (param i32 i32 i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "plugin_call")
export declare function import_plugin_call(
  pluginNamePtr: i32,
  pluginNameLen: i32,
  functionNamePtr: i32,
  functionNameLen: i32,
  inputPtr: i32,
  inputLen: i32,
  responsePtr: i32,
  responseMaxLen: i32,
): i64;

/**
 * 20. durable_workflow_id: Get the current workflow ID.
 * (import "env" "durable_workflow_id") (param i32 i32) (result i64)
 */
@external("env", "durable_workflow_id")
export declare function import_durable_workflow_id(
  idPtr: i32,
  idMaxLen: i32,
): i64;

/**
 * 21. durable_run_id: Get the current run ID.
 * (import "env" "durable_run_id") (param i32 i32) (result i64)
 */
@external("env", "durable_run_id")
export declare function import_durable_run_id(
  idPtr: i32,
  idMaxLen: i32,
): i64;

/**
 * 22. durable_send_signal_and_wait: Send a signal and wait for a response.
 * (import "env" "durable_send_signal_and_wait") (param i32 i32 i32 i32 i32 i32 i64 i32 i32) (result i64)
 */
@external("env", "durable_send_signal_and_wait")
export declare function import_durable_send_signal_and_wait(
  targetRunIdPtr: i32,
  targetRunIdLen: i32,
  signalNamePtr: i32,
  signalNameLen: i32,
  payloadPtr: i32,
  payloadLen: i32,
  timeoutMs: i64,
  responsePtr: i32,
  responseMaxLen: i32,
): i64;

/**
 * 23. durable_reply_to_signal: Respond to a signal from within a handler.
 * (import "env" "durable_reply_to_signal") (param i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_reply_to_signal")
export declare function import_durable_reply_to_signal(
  correlationIdPtr: i32,
  correlationIdLen: i32,
  responsePtr: i32,
  responseLen: i32,
): i64;

/**
 * 24. durable_signal_workflow: Send a signal to another workflow.
 * (import "env" "durable_signal_workflow") (param i32 i32 i32 i32 i32 i32) (result i64)
 */
@external("env", "durable_signal_workflow")
export declare function import_durable_signal_workflow(
  targetRunIdPtr: i32,
  targetRunIdLen: i32,
  signalNamePtr: i32,
  signalNameLen: i32,
  payloadPtr: i32,
  payloadLen: i32,
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

/** Outcome of a plugin_call operation. */
export class PluginCallOutcome {
  constructor(
    /** Response JSON from the plugin. Empty on error. */
    public readonly response: string,
    /** Error message, or null on success. */
    public readonly error: string | null,
    /** Structured call error code. */
    public readonly callErrorCode: u32,
  ) {}
  get isError(): bool { return this.error !== null; }
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

/** Result of `createPromise`. */
export class PromiseResult {
  constructor(
    /** The promise ID string. Empty on error. */
    public readonly value: string,
    /** Error message, or null on success. */
    public readonly error: string | null,
  ) {}

  /** Returns true when this result carries an error. */
  get isError(): bool {
    return this.error !== null;
  }
}

/** Result of `awaitPromise`. */
export class AwaitPromiseOutcome {
  constructor(
    /** The resolved promise value. Empty on timeout or error. */
    public readonly value: string,
    /** Whether the wait timed out. */
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
 * High-level AssemblyScript wrapper around the 19 cleat WASM host function
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

  /** Current scope prefix for virtual object state operations. */
  private _scopePrefix: string = "";

  /**
   * @param memory - Optional Memory instance. A default one is created if
   *                 not provided.
   */
  constructor(memory: Memory = new Memory()) {
    this.memory = memory;
  }

  /**
   * Write a string to the scratch buffer with bounds checking.
   *
   * Throws a descriptive error if the string does not fit in the remaining
   * buffer space, preventing silent memory corruption from buffer overflow.
   *
   * @param offset    - Write offset in the scratch buffer.
   * @param remaining - Remaining bytes available at offset.
   * @param s         - String to write.
   * @param label     - Human-readable label for error messages.
   * @returns The number of bytes written.
   */
  private writeScratch(offset: usize, remaining: i32, s: string, label: string): i32 {
    if (remaining <= 0) {
      throw new Error(
        "scratch buffer overflow: no space remaining for '" + label + "'",
      );
    }
    let byteLen: i32 = String.UTF8.byteLength(s) as i32;
    if (byteLen > remaining) {
      throw new Error(
        "scratch buffer overflow: '" +
          label +
          "' requires " +
          byteLen.toString() +
          " bytes but only " +
          remaining.toString() +
          " remain",
      );
    }
    return this.memory.writeString(offset, remaining, s);
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
    let remaining: i32 = OUT_BUF_SIZE - svcLen;
    let opLen: i32 = this.writeScratch(opOffset, remaining, operation, "operation");
    let reqOffset: usize = opOffset + opLen;
    remaining -= opLen;
    let reqLen: i32 = this.writeScratch(reqOffset, remaining, requestJson, "requestJson");

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
    let shouldSuspend: bool = decoded.status === 1;
    if (shouldSuspend) {
      setWorkflowSuspended();
    }
    return shouldSuspend;
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
    let remaining: i32 = OUT_BUF_SIZE - nameLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson, "inputJson");

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

    // Check for suspend signal: host sets bit 62 when child not yet complete
    if ((result as u64 & (1 << 62)) != 0) {
      setWorkflowSuspended();
      return new DurableResult<string>("", "");
    }

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
    let remaining: i32 = OUT_BUF_SIZE - keyLen;
    let valLen: i32 = this.writeScratch(valOffset, remaining, value, "value");

    import_set_query_state(SCRATCH_BASE as i32, keyLen, valOffset as i32, valLen);
  }

  // ────────────────────────────────────────────
  // 16. createPromise
  // ────────────────────────────────────────────

  /**
   * Create a new durable promise with the given name.
   *
   * The host allocates a promise ID and writes it to the output buffer.
   *
   * @param name - The promise name.
   * @returns A PromiseResult containing the promise ID on success.
   */
  createPromise(name: string): PromiseResult {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);

    let result: i64 = import_durable_create_promise(
      SCRATCH_BASE as i32,
      nameLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new PromiseResult(
        "",
        "create_promise error code: " + decoded.errCode.toString(),
      );
    }

    let promiseId: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new PromiseResult(promiseId, null);
  }

  // ────────────────────────────────────────────
  // 17. awaitPromise
  // ────────────────────────────────────────────

  /**
   * Wait for a durable promise to resolve, with a timeout.
   *
   * If the promise is not yet resolved when the timeout elapses, the
   * workflow should suspend by returning the suspension sentinel.
   *
   * @param id        - The promise ID to wait for.
   * @param timeoutMs - Timeout in milliseconds.
   * @returns The outcome with the resolved value and timeout status.
   */
  awaitPromise(id: string, timeoutMs: i64): AwaitPromiseOutcome {
    let idLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, id);

    let result: i64 = import_durable_await_promise(
      SCRATCH_BASE as i32,
      idLen,
      timeoutMs,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeAwaitPromiseResult(result);

    if (decoded.errCode !== 0) {
      return new AwaitPromiseOutcome(
        "",
        false,
        "await_promise error code: " + decoded.errCode.toString(),
      );
    }

    let promiseResult: string =
      decoded.resultLen > 0
        ? this.memory.readString(OUTPUT_OFFSET, decoded.resultLen as i32)
        : "";

    return new AwaitPromiseOutcome(promiseResult, decoded.timedOut, null);
  }

  // ────────────────────────────────────────────
  // 18. registerUpdateHandler
  // ────────────────────────────────────────────

  /**
   * Register a handler for update calls on this workflow.
   *
   * Update handlers allow external clients to send update requests to
   * the workflow while it is executing.
   *
   * @param name - The update handler name.
   */
  registerUpdateHandler(name: string): void {
    let nameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);
    import_durable_register_update_handler(SCRATCH_BASE as i32, nameLen);
  }

  // ────────────────────────────────────────────
  // 19. pluginCall
  // ────────────────────────────────────────────

  /**
   * Call a plugin function via the host runtime.
   *
   * Plugin name, function name, and input JSON are encoded to the scratch
   * buffer sequentially, the host call is made, and the response is read
   * from the output buffer.
   *
   * @param pluginName    - Name of the plugin (e.g., "blobstore", "slacknotify").
   * @param functionName  - Plugin function name (e.g., "put", "get").
   * @param inputJson     - Input payload as a JSON string.
   * @returns The plugin call outcome with response JSON or error details.
   */
  pluginCall(pluginName: string, functionName: string, inputJson: string): PluginCallOutcome {
    // Encode input strings sequentially into the scratch buffer
    let pluginNameLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, pluginName);
    let fnOffset: usize = SCRATCH_BASE + pluginNameLen;
    let remaining: i32 = OUT_BUF_SIZE - pluginNameLen;
    let fnLen: i32 = this.writeScratch(fnOffset, remaining, functionName, "functionName");
    let inputOffset: usize = fnOffset + fnLen;
    remaining -= fnLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson, "inputJson");

    // Call the host import
    let result: i64 = import_plugin_call(
      SCRATCH_BASE as i32,
      pluginNameLen,
      fnOffset as i32,
      fnLen,
      inputOffset as i32,
      inputLen,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    // Decode the packed result (same bit layout as durable_call)
    let decoded = decodeCallResult(result);
    let responseLen: i32 = decoded.responseLen as i32;

    // On error, the output buffer contains an error message
    if (decoded.errCode !== 0) {
      let errMsg: string =
        responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "unknown error";
      return new PluginCallOutcome("", errMsg, decoded.callErrorCode);
    }

    // Success: read the response
    let resp: string =
      responseLen > 0 ? this.memory.readString(OUTPUT_OFFSET, responseLen) : "";
    return new PluginCallOutcome(resp, null, 0);
  }

  // ────────────────────────────────────────────
  // 20. currentWorkflowId
  // ────────────────────────────────────────────

  /**
   * Get the current workflow ID from the host runtime.
   *
   * @returns The workflow ID string.
   */
  currentWorkflowId(): string {
    let result: i64 = import_durable_workflow_id(OUTPUT_OFFSET as i32, OUT_BUF_SIZE);
    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0 || decoded.extra === 0) {
      return "";
    }
    return this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
  }

  // ────────────────────────────────────────────
  // 21. setScope
  // ────────────────────────────────────────────

  /**
   * Set the state key prefix for virtual object instances.
   * All subsequent setQueryState calls are automatically prefixed
   * with "vo:<objectType>:<instanceKey>:".
   *
   * @param objectType  - The virtual object type name.
   * @param instanceKey - The instance key for this specific object.
   * @returns The previous scope prefix (empty string if none was set).
   */
  setScope(objectType: string, instanceKey: string): string {
    let prev: string = this._scopePrefix;
    this._scopePrefix =
      objectType.length > 0 && instanceKey.length > 0
        ? "vo:" + objectType + ":" + instanceKey + ":"
        : "";
    return prev;
  }

  // ────────────────────────────────────────────
  // 22. getScope
  // ────────────────────────────────────────────

  /**
   * Get the current virtual object scope.
   *
   * @returns A tuple [objectType, instanceKey] or ["", ""] if no scope
   *          is set.
   */
  getScope(): string[] {
    if (this._scopePrefix.length === 0) {
      return ["", ""];
    }
    let trimmed: string = this._scopePrefix.substring(0, this._scopePrefix.length - 1);
    let parts: string[] = trimmed.split(":");
    if (parts.length === 3 && parts[0] === "vo") {
      return [parts[1], parts[2]];
    }
    return ["", ""];
  }

  // ────────────────────────────────────────────
  // 23. clearScope
  // ────────────────────────────────────────────

  /**
   * Remove the current scope and return the previous scope prefix.
   *
   * @returns The scope prefix that was active before clearing (empty
   *          string if none was set).
   */
  clearScope(): string {
    let prev: string = this._scopePrefix;
    this._scopePrefix = "";
    return prev;
  }

  // ────────────────────────────────────────────
  // 24. uuid — deterministic ID generation
  // ────────────────────────────────────────────

  /**
   * Return a deterministic UUID scoped to the current workflow
   * and the given seed. Same seed always produces the same UUID for
   * this workflow instance.
   *
   * Uses a 128-bit FNV-1a hash of "{workflowID}:{seed}" to produce
   * a UUID-formatted string.
   *
   * @param seed - A seed string that determines the UUID within this
   *               workflow.
   * @returns A UUID-formatted string.
   */
  uuid(seed: string): string {
    let wfId: string = this.currentWorkflowId();
    let data: string = wfId + ":" + seed;

    // FNV-1a 128-bit hash for deterministic UUID generation
    let h1: u64 = 0xcbf29ce484222325;
    let h2: u64 = 0x6c62272e07bb0142;
    for (let i: i32 = 0; i < data.length; i++) {
      let c: u32 = data.charCodeAt(i);
      h1 ^= c;
      h1 *= 0x100000001b3;
      h2 ^= c;
      h2 *= 0x100000001b3;
    }

    // Set UUIDv5 version bits in byte 6 (top nibble of h1's 7th byte)
    // h1 byte 6 is at bits 8-15: clear top nibble, set version 5
    h1 = (h1 & ~(u64(0xf0) << 8)) | (u64(0x50) << 8);

    // Set UUID variant 1 bits in byte 8 (top nibble of h2's 1st byte)
    // h2 byte 0 is at bits 56-63: clear top 2 bits, set variant 1
    h2 = (h2 & ~(u64(0xc0) << 56)) | (u64(0x80) << 56);

    // Format as UUID: XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX
    let hexChars: string = "0123456789abcdef";
    let parts: string[] = ["", "", "", "", ""];

    // Part 0 (8 hex chars): first 32 bits from h1 (bits 63-32)
    // Part 1 (4 hex chars): next 16 bits from h1 (bits 31-16)
    // Part 2 (4 hex chars): last 16 bits from h1 (bits 15-0)
    let temp: u64 = h1;
    for (let i: i32 = 0; i < 8; i++) {
      parts[0] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }
    for (let i: i32 = 0; i < 4; i++) {
      parts[1] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }
    for (let i: i32 = 0; i < 4; i++) {
      parts[2] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }

    // Part 3 (4 hex chars): first 16 bits from h2 (bits 63-48)
    // Part 4 (12 hex chars): remaining 48 bits from h2 (bits 47-0)
    temp = h2;
    for (let i: i32 = 0; i < 4; i++) {
      parts[3] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }
    for (let i: i32 = 0; i < 12; i++) {
      parts[4] += hexChars.charAt((temp >> 60) as u32);
      temp <<= 4;
    }

    return parts[0] + "-" + parts[1] + "-" + parts[2] + "-" + parts[3] + "-" + parts[4];
  }

  // ────────────────────────────────────────────
  // 25. sendSignalAndWait — send a signal and wait for a response
  // ────────────────────────────────────────────

  /**
   * Send a signal to a target workflow and wait for a response.
   *
   * The signal carries an embedded correlation ID. The target workflow
   * should use `replyToSignal` to send a response back.
   *
   * @param targetRunId - The target workflow's run ID.
   * @param signalName  - The signal name to send.
   * @param payload     - The signal payload JSON.
   * @param timeoutMs   - Maximum wait time in milliseconds.
   * @returns A DurableResult containing the response on success.
   */
  sendSignalAndWait(
    targetRunId: string,
    signalName: string,
    payload: string,
    timeoutMs: i64,
  ): DurableResult<string> {
    let targetLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, targetRunId);
    let sigOffset: usize = SCRATCH_BASE + targetLen;
    let remaining: i32 = OUT_BUF_SIZE - targetLen;
    let sigLen: i32 = this.writeScratch(sigOffset, remaining, signalName, "signalName");
    let payloadOffset: usize = sigOffset + sigLen;
    remaining -= sigLen;
    let payloadLen: i32 = this.writeScratch(payloadOffset, remaining, payload, "payload");

    let result: i64 = import_durable_send_signal_and_wait(
      SCRATCH_BASE as i32,
      targetLen,
      sigOffset as i32,
      sigLen,
      payloadOffset as i32,
      payloadLen,
      timeoutMs,
      OUTPUT_OFFSET as i32,
      OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return new DurableResult<string>(
        "",
        "send_signal_and_wait error code: " + decoded.errCode.toString(),
      );
    }
    let response: string = this.memory.readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult<string>(response, null);
  }

  // ────────────────────────────────────────────
  // 26. replyToSignal — respond to a signal from within a handler
  // ────────────────────────────────────────────

  /**
   * Send a response back to the sender of a signal.
   *
   * Only valid inside a signal handler context where the correlation ID
   * was embedded in the received signal payload.
   *
   * @param correlationId - The correlation ID from the received signal payload.
   * @param response      - The response payload JSON.
   * @returns An error message on failure, or null on success.
   */
  replyToSignal(correlationId: string, response: string): string | null {
    let cidLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, correlationId);
    let respOffset: usize = SCRATCH_BASE + cidLen;
    let remaining: i32 = OUT_BUF_SIZE - cidLen;
    let respLen: i32 = this.writeScratch(respOffset, remaining, response, "response");

    let result: i64 = import_durable_reply_to_signal(
      SCRATCH_BASE as i32,
      cidLen,
      respOffset as i32,
      respLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "reply_to_signal error code: " + decoded.errCode.toString();
    }
    return null;
  }

  // ────────────────────────────────────────────
  // 27. awaitSignalsWithQuorum — wait for quorum of signals
  // ────────────────────────────────────────────

  /**
   * Wait for at least minCount signals from the named set.
   *
   * Collects signals until minCount is reached, maxRejections is exceeded,
   * or the timeout expires.
   *
   * @param namesJson     - JSON array of signal names to wait for.
   * @param minCount      - Minimum number of signals required to proceed.
   * @param maxRejections - Maximum rejections tolerated (-1 to disable).
   * @param timeoutMs     - Maximum wait time in milliseconds.
   * @returns The collected signal results.
   */
  awaitSignalsWithQuorum(
    namesJson: string,
    minCount: i32,
    maxRejections: i32,
    timeoutMs: i64,
  ): AwaitSignalsOutcome[] {
    // Simple implementation: call awaitSignals in a loop.
    let results: AwaitSignalsOutcome[] = [];
    let deadline: i64 = this.now() + timeoutMs;
    let rejectionCount: i32 = 0;

    while (results.length < minCount) {
      let remainingMs: i64 = deadline - this.now();
      if (remainingMs <= 0) {
        throw new Error(
          "quorum timeout: got " + results.length.toString() + "/" + minCount.toString() + " signals",
        );
      }

      let outcome: AwaitSignalsOutcome = this.awaitSignals(namesJson, remainingMs);
      if (outcome.timedOut) {
        throw new Error(
          "quorum timeout: got " + results.length.toString() + "/" + minCount.toString() + " signals",
        );
      }
      if (outcome.isError) {
        throw new Error("quorum signal error: " + (outcome.error as string));
      }

      results.push(outcome);

      // Check for rejection if maxRejections >= 0.
      if (maxRejections >= 0 && outcome.payload.length > 0) {
        // Look for "rejected": true in the JSON payload.
        if (outcome.payload.indexOf('"rejected":true') !== -1 || outcome.payload.indexOf('"rejected": true') !== -1) {
          rejectionCount++;
          if (rejectionCount > maxRejections) {
            throw new Error(
              "quorum exceeded max rejections (" + maxRejections.toString() + ")",
            );
          }
        }
      }
    }
    return results;
  }

  // ────────────────────────────────────────────
  // 28. signalWorkflow — send a signal to another workflow
  // ────────────────────────────────────────────

  /**
   * Send a signal to a target workflow (fire-and-forget).
   *
   * Unlike sendSignalAndWait, this method does not wait for a response.
   * The signal is enqueued and the workflow continues immediately.
   *
   * @param targetRunId - The target workflow's run ID.
   * @param signalName  - The signal name to send.
   * @param payload     - The signal payload JSON.
   * @returns An error message on failure, or null on success.
   */
  signalWorkflow(targetRunId: string, signalName: string, payload: string): string | null {
    let targetLen: i32 = this.memory.writeString(SCRATCH_BASE, OUT_BUF_SIZE, targetRunId);
    let sigOffset: usize = SCRATCH_BASE + targetLen;
    let remaining: i32 = OUT_BUF_SIZE - targetLen;
    let sigLen: i32 = this.writeScratch(sigOffset, remaining, signalName, "signalName");
    let payloadOffset: usize = sigOffset + sigLen;
    remaining -= sigLen;
    let payloadLen: i32 = this.writeScratch(payloadOffset, remaining, payload, "payload");

    let result: i64 = import_durable_signal_workflow(
      SCRATCH_BASE as i32,
      targetLen,
      sigOffset as i32,
      sigLen,
      payloadOffset as i32,
      payloadLen,
    );

    let decoded = decodeSimpleResult(result);
    if (decoded.errCode !== 0) {
      return "signal_workflow error code: " + decoded.errCode.toString();
    }
    return null;
  }
}
