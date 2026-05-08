// Fixed AssemblyScript host-function bindings for cleat.
//
// Based on @cleat/sdk (packages/cleat-as) but with fixes for AS 0.27.32:
//   1. String.UTF8.encodeUnsafe signature changed in 0.27.32:
//      Old: encodeUnsafe(str: string, ptr: usize, len: usize): void
//      New: encodeUnsafe(str: usize, len: i32, buf: usize): usize
//   2. usize vs i32 comparisons need explicit casts.
//   3. No try/catch with --runtime stub (removed all try/catch blocks).
//
// Uses direct @external imports for the 18 cleat WASM host functions,
// and provides a minimal HostCalls wrapper.

// ═══════════════════════════════════════════
// Memory layout (from ABI.md section 3)
// ═══════════════════════════════════════════

/** Output buffer size in bytes (64 KiB). */
export const OUT_BUF_SIZE: i32 = 65536;

/** Scratch region base offset in linear memory (10 MiB = 0xA00000). */
export const SCRATCH_BASE: usize = 10485760;

/** Output buffer offset (0xA10000 = 10 MiB + 64 KiB). */
export const OUTPUT_OFFSET: usize = 10551296;

/** Suspend sentinel value (1 << 62 = 0x4000000000000000). */
export const SUSPEND_SENTINEL: i64 = 0x4000000000000000;

// ═══════════════════════════════════════════
// Raw host function imports ("env" module)
// ═══════════════════════════════════════════

@external("env", "cleat_sleep")
declare function import_durable_sleep(durationMs: i64): i64;

@external("env", "cleat_now")
declare function import_durable_now(): i64;

@external("env", "cleat_random")
declare function import_durable_random(): i64;

@external("env", "cleat_log")
declare function import_durable_log(msgPtr: i32, msgLen: i32): i64;

@external("env", "cleat_version")
declare function import_durable_version(): i64;

@external("env", "cleat_call")
declare function import_durable_call(
  svcPtr: i32, svcLen: i32,
  opPtr: i32, opLen: i32,
  reqPtr: i32, reqLen: i32,
  respPtr: i32, respMaxLen: i32,
): i64;

@external("env", "cleat_child_workflow")
declare function import_durable_child_workflow(
  namePtr: i32, nameLen: i32,
  inputPtr: i32, inputLen: i32,
  runIdPtr: i32, runIdMaxLen: i32,
): i64;

@external("env", "cleat_await_child")
declare function import_durable_await_child(
  runIdPtr: i32, runIdLen: i32,
  resultPtr: i32, resultMaxLen: i32,
): i64;

@external("env", "cleat_await_signals")
declare function import_durable_await_signals(
  namesPtr: i32, namesLen: i32,
  timeoutMs: i64,
  sigNamePtr: i32, sigNameMaxLen: i32,
  payloadPtr: i32, payloadMaxLen: i32,
): i64;

@external("env", "cleat_poll_signal")
declare function import_durable_poll_signal(
  namePtr: i32, nameLen: i32,
  payloadPtr: i32, payloadMaxLen: i32,
): i64;

@external("env", "cleat_poll_cancellation")
declare function import_durable_poll_cancellation(
  reasonPtr: i32, reasonMaxLen: i32,
): i64;

@external("env", "cleat_defer")
declare function import_durable_defer(
  descPtr: i32, descLen: i32,
  deferIdPtr: i32, deferIdMaxLen: i32,
): i64;

@external("env", "cleat_continue_as_new")
declare function import_durable_continue_as_new(inputPtr: i32, inputLen: i32): i64;

@external("env", "cleat_create_promise")
declare function import_durable_create_promise(
  namePtr: i32, nameLen: i32,
  idOutPtr: i32, idOutMax: i32,
): i64;

@external("env", "cleat_await_promise")
declare function import_durable_await_promise(
  idPtr: i32, idLen: i32,
  timeoutMs: i64,
  resultOutPtr: i32, resultOutMax: i32,
): i64;

@external("env", "cleat_register_update_handler")
declare function import_durable_register_update_handler(namePtr: i32, nameLen: i32): i64;

@external("env", "set_query_state")
declare function import_set_query_state(
  keyPtr: i32, keyLen: i32,
  valPtr: i32, valLen: i32,
): i64;

// ═══════════════════════════════════════════
// String I/O helpers (fixed for AS 0.27.32)
// ═══════════════════════════════════════════

/** Read a UTF-8 string from linear memory at (ptr, len). */
export function readString(ptr: usize, len: i32): string {
  if (len <= 0) return "";
  // AS 0.27.32: decodeUnsafe(ptr: usize, len: usize) - this still works
  return String.UTF8.decodeUnsafe(ptr, len as usize);
}

/** Write a string to linear memory at ptr, up to maxLen bytes. */
export function writeString(ptr: usize, maxLen: i32, s: string): i32 {
  if (maxLen <= 0) return 0;
  if (s.length === 0) return 0;

  let byteLen: i32 = String.UTF8.byteLength(s) as i32;
  let writeLen: i32 = byteLen;

  if (writeLen > maxLen) {
    writeLen = maxLen;
  }

  if (writeLen > 0) {
    // AS 0.27.32: encodeUnsafe(str: usize, len: i32, buf: usize): usize
    String.UTF8.encodeUnsafe(changetype<usize>(s), byteLen, ptr);
  }

  return writeLen;
}

// ═══════════════════════════════════════════
// Decoders for bit-packed i64 results
// ═══════════════════════════════════════════

/** Decoded durable_call result. */
class CallResult {
  constructor(
    public readonly responseLen: u32,
    public readonly callErrorCode: u32,
    public readonly errCode: u8,
  ) {}
}

/** Decoded durable_sleep result. */
class SleepDecoded {
  constructor(
    public readonly status: u8,
    public readonly durationMs: i64,
  ) {}
}

/** Decoded durable_await_signals result. */
class AwaitSignalsDecoded {
  constructor(
    public readonly sigNameLen: u16,
    public readonly payloadLen: u16,
    public readonly timedOut: bool,
    public readonly errCode: u16,
  ) {}
}

/** Decoded simple result (child_workflow, etc). */
class SimpleDecoded {
  constructor(
    public readonly extra: u32,
    public readonly errCode: u8,
  ) {}
}

function decodeCallResult(result: i64): CallResult {
  let r: u64 = result as u64;
  return new CallResult(
    ((r >> 40) & 0xFFFFFF) as u32,
    ((r >> 8) & 0xFFFFFFFF) as u32,
    (r & 0xFF) as u8,
  );
}

function decodeSleepResult(result: i64): SleepDecoded {
  let r: u64 = result as u64;
  return new SleepDecoded(
    ((r >> 56) & 0xFF) as u8,
    (r & 0x00FFFFFFFFFFFFFF) as i64,
  );
}

function decodeAwaitSignalsResult(result: i64): AwaitSignalsDecoded {
  let r: u64 = result as u64;
  return new AwaitSignalsDecoded(
    ((r >> 48) & 0xFFFF) as u16,
    ((r >> 32) & 0xFFFF) as u16,
    ((r >> 16) & 0xFFFF) !== 0,
    (r & 0xFFFF) as u16,
  );
}

function decodeSimpleResult(result: i64): SimpleDecoded {
  let r: u64 = result as u64;
  return new SimpleDecoded(
    (r >> 32) as u32,
    (r & 0xFF) as u8,
  );
}

// ═══════════════════════════════════════════
// High-level result types
// ═══════════════════════════════════════════

export class DurableCallOutcome {
  constructor(
    public readonly response: string,
    public readonly error: string | null,
    public readonly callErrorCode: u32,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

export class AwaitSignalsOutcome {
  constructor(
    public readonly signalName: string,
    public readonly payload: string,
    public readonly timedOut: bool,
    public readonly error: string | null,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

export class DurableResult {
  constructor(
    public readonly value: string,
    public readonly error: string | null,
  ) {}

  get isError(): bool {
    return this.error !== null;
  }
}

// ═══════════════════════════════════════════
// Minimal HostCalls wrapper
// ═══════════════════════════════════════════

export class HostCalls {

  // ── durable_sleep ──

  durableSleep(durationMs: i64): bool {
    let result: i64 = import_durable_sleep(durationMs);
    let decoded = decodeSleepResult(result);
    return decoded.status === 1; // 1 = SLEEP_STATUS_SUSPEND
  }

  // ── durable_call ──

  durableCall(service: string, operation: string, requestJson: string): DurableCallOutcome {
    let svcLen: i32 = this.writeScratch(SCRATCH_BASE, OUT_BUF_SIZE, service);
    let opOffset: usize = SCRATCH_BASE + svcLen;
    let remaining: i32 = OUT_BUF_SIZE - svcLen;
    let opLen: i32 = this.writeScratch(opOffset, remaining, operation);
    let reqOffset: usize = opOffset + opLen;
    remaining -= opLen;
    let reqLen: i32 = this.writeScratch(reqOffset, remaining, requestJson);

    let result: i64 = import_durable_call(
      SCRATCH_BASE as i32, svcLen,
      opOffset as i32, opLen,
      reqOffset as i32, reqLen,
      OUTPUT_OFFSET as i32, OUT_BUF_SIZE,
    );

    let decoded = decodeCallResult(result);
    let responseLen: i32 = decoded.responseLen as i32;

    if (decoded.errCode !== 0) {
      let errMsg: string = responseLen > 0 ? readString(OUTPUT_OFFSET, responseLen) : "unknown error";
      return new DurableCallOutcome("", errMsg, decoded.callErrorCode);
    }

    let resp: string = responseLen > 0 ? readString(OUTPUT_OFFSET, responseLen) : "";
    return new DurableCallOutcome(resp, null, 0);
  }

  // ── durable_log ──

  log(message: string): void {
    let msgLen: i32 = writeString(SCRATCH_BASE, OUT_BUF_SIZE, message);
    import_durable_log(SCRATCH_BASE as i32, msgLen);
  }

  // ── child_workflow ──

  childWorkflow(name: string, inputJson: string): DurableResult {
    let nameLen: i32 = writeString(SCRATCH_BASE, OUT_BUF_SIZE, name);
    let inputOffset: usize = SCRATCH_BASE + nameLen;
    let remaining: i32 = OUT_BUF_SIZE - nameLen;
    let inputLen: i32 = this.writeScratch(inputOffset, remaining, inputJson);

    let result: i64 = import_durable_child_workflow(
      SCRATCH_BASE as i32, nameLen,
      inputOffset as i32, inputLen,
      OUTPUT_OFFSET as i32, OUT_BUF_SIZE,
    );

    let decoded = decodeSimpleResult(result);

    if (decoded.errCode !== 0) {
      return new DurableResult("", "child_workflow error code: " + decoded.errCode.toString());
    }

    let runId: string = readString(OUTPUT_OFFSET, decoded.extra as i32);
    return new DurableResult(runId, null);
  }

  // ── await_signals ──

  awaitSignals(namesJson: string, timeoutMs: i64): AwaitSignalsOutcome {
    let namesLen: i32 = writeString(SCRATCH_BASE, OUT_BUF_SIZE / 2, namesJson);

    let payloadOffset: usize = SCRATCH_BASE + OUT_BUF_SIZE / 2;
    let payloadMaxLen: i32 = OUT_BUF_SIZE / 2;

    let result: i64 = import_durable_await_signals(
      SCRATCH_BASE as i32, namesLen,
      timeoutMs,
      OUTPUT_OFFSET as i32, OUT_BUF_SIZE,
      payloadOffset as i32, payloadMaxLen,
    );

    let decoded = decodeAwaitSignalsResult(result);

    if (decoded.errCode !== 0) {
      return new AwaitSignalsOutcome("", "", false, "await_signals error code: " + decoded.errCode.toString());
    }

    let sigName: string = decoded.sigNameLen > 0 ? readString(OUTPUT_OFFSET, decoded.sigNameLen as i32) : "";
    let payload: string = !decoded.timedOut && decoded.payloadLen > 0 ? readString(payloadOffset, decoded.payloadLen as i32) : "";

    return new AwaitSignalsOutcome(sigName, payload, decoded.timedOut, null);
  }

  // ── set_query_state ──

  setQueryState(key: string, value: string): void {
    let keyLen: i32 = writeString(SCRATCH_BASE, OUT_BUF_SIZE, key);
    let valOffset: usize = SCRATCH_BASE + keyLen;
    let remaining: i32 = OUT_BUF_SIZE - keyLen;
    let valLen: i32 = this.writeScratch(valOffset, remaining, value);

    import_set_query_state(SCRATCH_BASE as i32, keyLen, valOffset as i32, valLen);
  }

  // ── internal scratch buffer writer ──

  private writeScratch(offset: usize, remaining: i32, s: string): i32 {
    if (remaining <= 0) {
      // Can't throw without try/catch, just return 0
      return 0;
    }
    let byteLen: i32 = String.UTF8.byteLength(s) as i32;
    if (byteLen > remaining) {
      byteLen = remaining;
    }
    return writeString(offset, remaining, s);
  }
}

// ═══════════════════════════════════════════
// Export encoding helpers
// ═══════════════════════════════════════════

export function encodeExportResult(errCode: u32, actualLen: u32): i64 {
  return ((actualLen as u64) << 32 | (errCode as u64)) as i64;
}
