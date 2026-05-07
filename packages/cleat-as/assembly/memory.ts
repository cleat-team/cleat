/**
 * Memory helpers for WASM linear memory and bit-packing decoders.
 *
 * Matches the Rust SDK at crates/durable-sdk/src/memory.rs and
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

// ──────────────────────────────────────────────
// Internal status constants
// ──────────────────────────────────────────────

/** Sleep completed (replay path). */
export const SLEEP_STATUS_COMPLETED: u8 = 0;

/** Sleep triggered suspend (fresh execution). */
export const SLEEP_STATUS_SUSPEND: u8 = 1;

/** PollSignal found flag value (bit 8 set = 0x0100). */
export const POLL_SIGNAL_FOUND: u32 = 0x0100;

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
  if (s.length === 0) return 0;

  let byteLen: i32 = String.UTF8.byteLength(s) as i32;
  let writeLen: i32 = byteLen;

  if (writeLen > maxLen) {
    writeLen = maxLen;
  }

  if (writeLen > 0) {
    String.UTF8.encodeUnsafe(changetype<usize>(s), writeLen, ptr);
  }

  return writeLen as i32;
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

/** Decoded durable_call result. */
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

/** Decoded durable_sleep result. */
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

/** Decoded durable_await_signals result. */
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

/** Decoded durable_poll_signal result. */
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

/** Decoded durable_poll_cancellation result. */
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
 * Decode a durable_call result.
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
 * Decode a durable_sleep result.
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
 * Decode a durable_await_signals result.
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
 * Decode a durable_poll_signal result.
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
 * Decode a durable_poll_cancellation result.
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

/** Decoded durable_await_promise result. */
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
 * Decode a durable_await_promise result.
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
