/**
 * Memory helpers for WASM linear memory and bit-packing decoders.
 *
 * Matches the Rust SDK at crates/cleat-sdk/src/memory.rs and
 * the ABI specification in ABI.md.
 *
 * All 15 host functions return i64 with bit-packed results. The decode
 * functions in this module extract the individual fields from those packed
 * values. String I/O helpers read/write UTF-8 in linear memory at the
 * standard cleat memory layout offsets.
 */

// ──────────────────────────────────────────────
// Memory layout constants (from ABI.md §3)
// ──────────────────────────────────────────────

/** Output buffer size in bytes (64 KiB). */
export const OUT_BUF_SIZE: i32 = 65536;

/** Scratch region base offset in linear memory (10 MiB = 0xA00000). */
export const SCRATCH_BASE: usize = 10485760;

/** Output buffer offset (0xA10000 = 10 MiB + 64 KiB). */
export const OUTPUT_OFFSET: usize = 10551296;

/**
 * Suspend sentinel value (1 << 62 = 0x4000000000000000).
 * When an export returns this value, the host knows the workflow
 * needs to suspend (e.g., for a timer or signal).
 */
export const SUSPEND_SENTINEL: i64 = 0x4000000000000000;

/**
 * Bit 31 of a host-call result: the host is refusing this call because the
 * workflow is in a defer segment and the call would start new work.
 *
 * Distinct from `SUSPEND_SENTINEL`, which is bit 62 and is what the guest
 * returns to the host from an export. This one travels the other way — host to
 * guest, inside an ordinary result word — and bit 31 was chosen because it is
 * the one bit free in all six result layouts a call that can start fresh work
 * returns. See IMPROVEMENT-PLAN 3.84 and ABI.md.
 *
 * The engine's copy is `callSuspendSentinel` in `engine/memory.go`. The two
 * must agree, and nothing in either language can see the other, so
 * `TestTheAssemblyScriptSDKAgreesOnTheStopBit` in `engine/` reads this file and
 * pins the value.
 */
export const SUSPEND_STOP_BIT: i64 = 0x80000000;

// ──────────────────────────────────────────────
// Internal status constants
// ──────────────────────────────────────────────

/** Sleep completed (replay path). */
export const SLEEP_STATUS_COMPLETED: u8 = 0;

/** Sleep triggered suspend (fresh execution). */
export const SLEEP_STATUS_SUSPEND: u8 = 1;

/** PollSignal found flag value (bit 8 set = 0x0100). */
export const POLL_SIGNAL_FOUND: u32 = 0x0100;

/**
 * Error code used by the @cleatEntry transform when a workflow function
 * returns a TerminalError (non-retryable). The transform should return
 * encodeExportResult(TERMINAL_ERROR_CODE, 0) to signal a terminal failure.
 */
export const TERMINAL_ERROR_CODE: u32 = 2;

/**
 * Error code used by the @cleatEntry transform for regular errors
 * that may be retried.
 */
export const RETRYABLE_ERROR_CODE: u32 = 1;

// ──────────────────────────────────────────────
// String I/O
// ──────────────────────────────────────────────

/**
 * Read a UTF-8 string from WASM linear memory at `(ptr, len)`.
 * Returns an empty string when `len <= 0`.
 *
 * Uses `String.UTF8.decodeUnsafe` for zero-copy decoding where possible.
 *
 * @param ptr - Pointer to the start of the UTF-8 data.
 * @param len - Number of bytes to read.
 */
export function readString(ptr: usize, len: i32): string {
  if (len <= 0) return "";
  return String.UTF8.decodeUnsafe(ptr, len as usize);
}

/**
 * Write a UTF-8 string to WASM linear memory at `ptr`, truncating to
 * at most `maxLen` bytes. Returns the number of bytes actually written.
 *
 * @param ptr    - Destination pointer in linear memory.
 * @param maxLen - Maximum bytes to write (capacity of the buffer).
 * @param s      - String to encode.
 * @returns The number of bytes written.
 */
export function writeString(ptr: usize, maxLen: i32, s: string): i32 {
  if (maxLen <= 0) return 0;
  let len: i32 = s.length as i32;  // code units, not byteLength (which includes null terminator)
  if (len > maxLen) { len = maxLen; }
  let written: i32 = 0;
  if (len > 0) {
    written = String.UTF8.encodeUnsafe(changetype<usize>(s), len, ptr) as i32;
  }
  return written;
}

// ──────────────────────────────────────────────
// Result type classes for bit-packing decoders
// ──────────────────────────────────────────────

/** Decoded export return value (low 32 = errCode, high 32 = actualLen). */
export class ExportDecode {
  constructor(
    public readonly errCode: u32,
    public readonly actualLen: u32,
  ) {}
}

/** Decoded cleat_call result. */
export class CallResult {
  constructor(
    /** Bytes written to the response buffer (bits 40-63, 24 bits). */
    public readonly responseLen: u32,
    /** Structured call error code (bits 8-39, 32 bits). */
    public readonly callErrorCode: u32,
    /** Error code: 0 = success, 1 = error (bits 0-7, 8 bits). */
    public readonly errCode: u8,
  ) {}
}

/** Decoded cleat_sleep result. */
export class SleepResult {
  constructor(
    /** Sleep status: 0 = completed (replay), 1 = suspend (fresh). */
    public readonly status: u8,
    /** Echo of the input duration in milliseconds (bits 0-55). */
    public readonly durationMs: i64,
  ) {}
}

/** Decoded simple result (defer, continue_as_new, child_workflow, await_child). */
export class SimpleResult {
  constructor(
    /** Extra data (bits 32-63) — varies by call type. */
    public readonly extra: u32,
    /** Error code (bits 0-7). */
    public readonly errCode: u8,
  ) {}
}

/** Decoded cleat_await_signals result. */
export class AwaitSignalsResult {
  constructor(
    /** Signal name length in bytes (bits 48-63, 16 bits). */
    public readonly sigNameLen: u16,
    /** Payload length in bytes (bits 32-47, 16 bits). */
    public readonly payloadLen: u16,
    /** Whether the wait timed out (bits 16-23, non-zero = timed out). */
    public readonly timedOut: bool,
    /** Error code (bits 0-15, 16 bits). */
    public readonly errCode: u16,
  ) {}
}

/** Decoded cleat_poll_signal result. */
export class PollSignalResult {
  constructor(
    /** Payload length in bytes (bits 32-63). */
    public readonly payloadLen: u32,
    /** Whether the signal was found (bits 8-15, 0x0100 = found). */
    public readonly found: bool,
    /** Error code (bits 0-7). */
    public readonly errCode: u8,
  ) {}
}

/** Decoded cleat_poll_cancellation result. */
export class PollCancellationResult {
  constructor(
    /** Whether cancellation has been requested (bits 0-31, non-zero = yes). */
    public readonly cancelled: bool,
    /** Cancellation reason length in bytes (bits 32-63). */
    public readonly reasonLen: u32,
  ) {}
}

// ──────────────────────────────────────────────
// Bit-packing encode / decode
// ──────────────────────────────────────────────

/**
 * Encode the export return value.
 *
 * Convention: low 32 bits = errCode (0 = success), high 32 bits = actualLen
 * (bytes written to the output buffer).
 *
 * @param errCode   - Error code (0 = success).
 * @param actualLen - Bytes written to the output buffer.
 * @returns Packed i64 for WASM return.
 */
export function encodeExportResult(errCode: u32, actualLen: u32): i64 {
  return ((actualLen as u64) << 32 | (errCode as u64)) as i64;
}

/**
 * Decode the export return value into (errCode, actualLen).
 *
 * @param result - Packed i64 from the WASM export return.
 * @returns An ExportDecode with the two fields.
 */
export function decodeExportResult(result: i64): ExportDecode {
  let r: u64 = result as u64;
  let errCode: u32 = (r & 0xFFFFFFFF) as u32;
  let actualLen: u32 = (r >> 32) as u32;
  return new ExportDecode(errCode, actualLen);
}

/**
 * Decode a cleat_call result.
 *
 * Bit layout:
 *   bits  0-7  = errCode (8 bits)
 *   bits  8-39 = callErrorCode (32 bits)
 *   bits 40-63 = responseLen (24 bits)
 */
export function decodeCallResult(result: i64): CallResult {
  let r: u64 = result as u64;
  let responseLen: u32 = ((r >> 40) & 0xFFFFFF) as u32;
  let callErrorCode: u32 = ((r >> 8) & 0xFFFFFFFF) as u32;
  let errCode: u8 = (r & 0xFF) as u8;
  return new CallResult(responseLen, callErrorCode, errCode);
}

/**
 * Decode a simple result (used by defer, continue_as_new, child_workflow,
 * and await_child).
 *
 * Bit layout:
 *   bits  0-7  = errCode (8 bits)
 *   bits 32-63 = extra (32 bits)
 */
export function decodeSimpleResult(result: i64): SimpleResult {
  let r: u64 = result as u64;
  let extra: u32 = (r >> 32) as u32;
  let errCode: u8 = (r & 0xFF) as u8;
  return new SimpleResult(extra, errCode);
}

/**
 * Decode a cleat_sleep result.
 *
 * Bit layout:
 *   bits  0-55 = durationMs (56 bits)
 *   bits 56-63 = status (8 bits): 0 = completed, 1 = suspend
 */
export function decodeSleepResult(result: i64): SleepResult {
  let r: u64 = result as u64;
  let status: u8 = ((r >> 56) & 0xFF) as u8;
  let durationMs: i64 = (r & 0x00FFFFFFFFFFFFFF) as i64;
  return new SleepResult(status, durationMs);
}

/**
 * Decode a cleat_await_signals result.
 *
 * Bit layout:
 *   bits  0-15 = errCode (16 bits)
 *   bits 16-31 = timedOut flag (non-zero = timed out)
 *   bits 32-47 = payloadLen (16 bits)
 *   bits 48-63 = sigNameLen (16 bits)
 */
export function decodeAwaitSignalsResult(result: i64): AwaitSignalsResult {
  let r: u64 = result as u64;
  let sigNameLen: u16 = ((r >> 48) & 0xFFFF) as u16;
  let payloadLen: u16 = ((r >> 32) & 0xFFFF) as u16;
  let timedOut: bool = ((r >> 16) & 0xFFFF) !== 0;
  let errCode: u16 = (r & 0xFFFF) as u16;
  return new AwaitSignalsResult(sigNameLen, payloadLen, timedOut, errCode);
}

/**
 * Decode a cleat_poll_signal result.
 *
 * Bit layout:
 *   bits  0-7  = errCode (8 bits)
 *   bits  8-15 = found flag (0x0100 = signal found)
 *   bits 32-63 = payloadLen (32 bits)
 */
export function decodePollSignalResult(result: i64): PollSignalResult {
  let r: u64 = result as u64;
  let payloadLen: u32 = (r >> 32) as u32;
  let flags: u32 = (r & 0xFFFFFFFF) as u32;
  let errCode: u8 = (flags & 0xFF) as u8;
  let found: bool = (flags >> 8) !== 0;
  return new PollSignalResult(payloadLen, found, errCode);
}

/**
 * Decode a cleat_poll_cancellation result.
 *
 * Bit layout:
 *   bits  0-31 = cancelled flag (non-zero = cancelled)
 *   bits 32-63 = reasonLen (32 bits)
 */
export function decodePollCancellationResult(result: i64): PollCancellationResult {
  let r: u64 = result as u64;
  let reasonLen: u32 = (r >> 32) as u32;
  let cancelled: bool = (r & 0xFFFFFFFF) !== 0;
  return new PollCancellationResult(cancelled, reasonLen);
}

/** Decoded cleat_await_promise result. */
export class AwaitPromiseResult {
  constructor(
    /** Result length in bytes (bits 32-63). */
    public readonly resultLen: u32,
    /** Whether the promise wait timed out (bits 16-23, non-zero = timed out). */
    public readonly timedOut: bool,
    /** Error code (bits 0-15, 16 bits). */
    public readonly errCode: u16,
  ) {}
}

/**
 * Decode a cleat_await_promise result.
 *
 * Bit layout:
 *   bits  0-15 = errCode (16 bits)
 *   bits 16-23 = timedOut flag (non-zero = timed out)
 *   bits 32-63 = resultLen (32 bits)
 */
export function decodeAwaitPromiseResult(result: i64): AwaitPromiseResult {
  let r: u64 = result as u64;
  let resultLen: u32 = (r >> 32) as u32;
  let timedOut: bool = ((r >> 16) & 0xFF) !== 0;
  let errCode: u16 = (r & 0xFFFF) as u16;
  return new AwaitPromiseResult(resultLen, timedOut, errCode);
}

// ──────────────────────────────────────────────
// Memory helper class
// ──────────────────────────────────────────────

/**
 * Convenience wrapper around the string I/O functions and encoding.
 *
 * Provides both static methods (used by the `@cleat/transform` transformer
 * plugin in generated ABI wrappers) and instance methods (used by the
 * `HostCalls` class).
 *
 * Static usage (from transformer-generated code):
 * ```ts
 * let json = Memory.readString(argsPtr, argsLen);
 * let written = Memory.writeString(outPtr, maxOutLen, result);
 * return Memory.encodeExportResult(0, written);
 * ```
 *
 * Instance usage (from HostCalls):
 * ```ts
 * let bytes = this.memory.writeString(ptr, max, str);
 * let str = this.memory.readString(ptr, len);
 * ```
 */
export class Memory {
  // ── Static methods (used by @cleat/transform) ──

  /** Read a UTF-8 string from memory at `(ptr, len)`. */
  static readString(ptr: usize, len: i32): string {
    return readString(ptr, len);
  }

  /** Write a string to memory at `ptr`, truncating to `maxLen` bytes. */
  static writeString(ptr: usize, maxLen: i32, s: string): i32 {
    return writeString(ptr, maxLen, s);
  }

  /**
   * Encode the export return value.
   *
   * Convenience wrapper around `encodeExportResult` for use in
   * transformer-generated ABI export wrappers.
   */
  static encodeExportResult(errCode: u32, actualLen: u32): i64 {
    return encodeExportResult(errCode, actualLen);
  }

  // ── Instance methods (used by HostCalls) ──

  /** Read a UTF-8 string from memory at `(ptr, len)`. */
  readString(ptr: usize, len: i32): string {
    return Memory.readString(ptr, len);
  }

  /** Write a string to memory at `ptr`, truncating to `maxLen` bytes. */
  writeString(ptr: usize, maxLen: i32, s: string): i32 {
    return Memory.writeString(ptr, maxLen, s);
  }
}

// ──────────────────────────────────────────────
// Workflow suspension detection
// ──────────────────────────────────────────────

/**
 * Global flag set by HostCalls methods when the workflow needs to suspend
 * (e.g., `cleatSleep` returned "should suspend" on a fresh execution).
 *
 * The @cleatEntry transform-generated wrapper resets this flag before
 * calling the inner function and checks it afterward. If the flag is set,
 * the wrapper returns the SUSPEND_SENTINEL i64 to the host instead of
 * writing the result to the output buffer.
 *
 * This approach avoids requiring try/catch (not available with --runtime stub)
 * while allowing clean suspension detection across host-call boundaries.
 */
let _workflowSuspended: bool = false;

/** Returns `true` if the workflow suspended during the last host call. */
export function isWorkflowSuspended(): bool {
  return _workflowSuspended;
}

/** Reset the suspension flag before calling a user workflow function. */
export function resetWorkflowSuspended(): void {
  _workflowSuspended = false;
}

/** Set the suspension flag — called by HostCalls methods. */
export function setWorkflowSuspended(): void {
  _workflowSuspended = true;
}

/**
 * Report whether the host refused this call because the workflow is running as
 * a defer segment, setting the suspension flag if so.
 *
 * **Call this before decoding any field of the result.** Order is the contract,
 * not a style preference: in the await-signals layout bit 31 lands inside the
 * timed-out field, which `decodeAwaitSignalsResult` reads as
 * `(r >> 16) & 0xFFFF`, so a caller that decoded first would turn a stop into
 * an ordinary timeout and the workflow would carry on — doing the new work the
 * defer segment exists to prevent, with nothing to see.
 *
 * **This SDK cannot unwind, and that makes the guarantee weaker here than in
 * the others.** Go panics, Java throws, Rust returns `Err(CallError::Suspended)`
 * — each of those takes the workflow body out of its own control flow. This
 * runtime has no exceptions (`--runtime stub`), so all a stop can do is set the
 * flag and hand the caller an error result. A workflow body that ignores both
 * keeps running.
 *
 * What makes that acceptable rather than a hole is where the enforcement lives:
 * the host refuses *every* call for the rest of the segment, not just the first
 * one, so a guest that runs on cannot reach anything durable. It can burn
 * instructions and return a value the host discards, because the segment's
 * terminal outcome was decided before it started. The flag is how the guest
 * finds out; the host is what makes it true.
 *
 * @param result the raw result word from a host call
 * @returns `true` if the host refused the call
 */
export function stopRequested(result: i64): bool {
  if ((result & SUSPEND_STOP_BIT) !== 0) {
    setWorkflowSuspended();
    return true;
  }
  return false;
}

// ──────────────────────────────────────────────
// Defer phase
// ──────────────────────────────────────────────

/**
 * True while the guest is draining its defer table.
 *
 * IMPROVEMENT-PLAN §3.35 phase 4. Two things a defer body must not do, both
 * measured on this SDK 2026-09-02 before they were blocked:
 *
 *   * **Register another defer.** The table is drained BEFORE the first body
 *     runs -- it has to be, or a body that registers would extend the slice
 *     being walked -- so the new registration lands in a table nobody walks
 *     again. The host had already minted an ID and written a durable `defer`
 *     event for it, so a *completed* workflow's history carried a pending
 *     defer that nothing anywhere could ever run. That is §3.70's defect
 *     exactly, arrived at by a different road.
 *   * **Call continueAsNew.** Worse: the host recorded a `continue_as_new`
 *     event at step 3 AND the wrapper went on to report the workflow's
 *     already-decided result. One history with two contradictory terminal
 *     facts; the worker stores `done`, and the continuation silently never
 *     happens.
 *
 * The flag lives here, not in defer.ts, so that `host-calls.ts` can read it
 * without importing `defer.ts` -- which imports `host-calls.ts`, and a cycle
 * between two modules with top-level initialisers is a start-function ordering
 * hazard under `--runtime stub`. `memory.ts` has no imports at all.
 */
let _inDeferPhase: bool = false;

/** Returns `true` while defer bodies are running. */
export function isInDeferPhase(): bool {
  return _inDeferPhase;
}

/** Marks the start and end of the defer drain. Called by `runDeferred`. */
export function setInDeferPhase(v: bool): void {
  _inDeferPhase = v;
}

// ──────────────────────────────────────────────
// Terminal error detection
// ──────────────────────────────────────────────

/**
 * Global flag set when the workflow returns a TerminalError (non-retryable).
 * The @cleatEntry transform-generated wrapper checks this flag after
 * calling the inner function. If set, the wrapper returns
 * encodeExportResult(TERMINAL_ERROR_CODE, 0) instead of a retryable error.
 */
let _terminalError: string = "";

/** Returns the terminal error message, or empty string if none. */
export function getTerminalError(): string {
  return _terminalError;
}

/** Set a terminal error message. */
export function setTerminalError(msg: string): void {
  _terminalError = msg;
}

/** Clear the terminal error flag. */
export function clearTerminalError(): void {
  _terminalError = "";
}

// ──────────────────────────────────────────────
// JSON escaping utilities
// ──────────────────────────────────────────────

/**
 * Escape a string for JSON embedding.
 * Replaces control characters and reserved characters with standard JSON
 * escape sequences.
 *
 * @param s - String to escape.
 * @returns The escaped string safe for embedding in JSON.
 */
export function escapeJson(s: string): string {
    let result = "";
    for (let i: i32 = 0; i < s.length; i++) {
        let c = s.charCodeAt(i);
        if (c == 0x22) { result += "\\\""; }
        else if (c == 0x5c) { result += "\\\\"; }
        else if (c == 0x08) { result += "\\b"; }
        else if (c == 0x0c) { result += "\\f"; }
        else if (c == 0x0a) { result += "\\n"; }
        else if (c == 0x0d) { result += "\\r"; }
        else if (c == 0x09) { result += "\\t"; }
        else if (c < 0x20) {
            result += "\\u00";
            result += hexDigit((c >> 4) & 0xf);
            result += hexDigit(c & 0xf);
        }
        else { result += String.fromCharCode(c); }
    }
    return result;
}

function hexDigit(n: i32): string {
    if (n < 10) return String.fromCharCode(0x30 + n);
    return String.fromCharCode(0x61 + n - 10);
}

/**
 * Decode cleat_get_scope result.
 * bits 32-63 = objTypeLen, bits 0-31 = instKeyLen.
 * Matches Rust SDK decode_get_scope_result.
 */
export function decodeGetScopeResult(result: i64): u32[] {
  let r = result as u64;
  let objTypeLen = <u32>((r >> 32) & 0xFFFFFFFF);
  let instKeyLen = <u32>(r & 0xFFFFFFFF);
  return [objTypeLen, instKeyLen];
}
