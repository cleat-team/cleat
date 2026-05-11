// Memory helpers for WASM linear memory.
// Matches the ABI from internal/host/memory.go.

/// Output buffer size constant (matches Go's outBufSize = 65536).
pub const OUT_BUF_SIZE: u32 = 65536;

/// Scratch region base offset in linear memory (10 MiB).
pub const SCRATCH_BASE: u32 = 10 * 1024 * 1024; // 0xA00000

/// Output buffer offset = scratch base + outBufSize.
pub const OUTPUT_OFFSET: u32 = SCRATCH_BASE + OUT_BUF_SIZE; // 0xA10000

/// Suspend sentinel (1 << 62). Matches runtime.go line 153.
pub const SUSPEND_SENTINEL: i64 = 1 << 62;

/// Read a string from WASM linear memory at (ptr, len).
/// Matches readWasmString in memory.go.
///
/// # Safety
///
/// `ptr` must point to a valid region of WASM linear memory at least `len` bytes long.
/// The caller must ensure that `ptr` is correctly aligned and that the memory region
/// is not concurrently mutated.
pub unsafe fn read_string(ptr: *const u8, len: u32) -> String {
    if len == 0 {
        return String::new();
    }
    let slice = unsafe { std::slice::from_raw_parts(ptr, len as usize) };
    String::from_utf8_lossy(slice).into_owned()
}

/// Write a string to WASM linear memory, truncating to max_len.
/// Returns the number of bytes written. Matches writeWasmString in memory.go.
///
/// # Safety
///
/// `ptr` must point to a valid region of WASM linear memory at least `max_len` bytes long.
/// The caller must ensure that `ptr` is correctly aligned and that the memory region
/// is not concurrently read or written during the copy.
pub unsafe fn write_string(ptr: *mut u8, max_len: u32, s: &str) -> u32 {
    let data = s.as_bytes();
    let len = (data.len() as u32).min(max_len);
    if len > 0 {
        unsafe {
            std::ptr::copy_nonoverlapping(data.as_ptr(), ptr, len as usize);
        }
    }
    len
}

/// Decode the export result: (errCode, actualLen).
/// Matches decodeExportResult in memory.go lines 68-70.
pub fn decode_export_result(result: u64) -> (u32, u32) {
    let err_code = (result & 0xFFFF_FFFF) as u32;
    let actual_len = (result >> 32) as u32;
    (err_code, actual_len)
}

/// Encode the export result for return from an export function.
/// Matches the convention: low 32 bits = errCode, high 32 bits = actualLen.
pub fn encode_export_result(err_code: u32, actual_len: u32) -> i64 {
    ((actual_len as u64) << 32 | (err_code as u64)) as i64
}

/// Pack a cleat_call result. Matches packDurableCallResult in memory.go lines 50-52.
/// bits 40-63 = responseLen (24 bits)
/// bits 8-39  = callErrorCode (32 bits)
/// bits 0-7   = errCode (8 bits)
pub fn decode_cleat_call_result(result: i64) -> (u32, u32, u8) {
    let r = result as u64;
    let response_len = ((r >> 40) & 0xFF_FFFF) as u32;
    let call_error_code = ((r >> 8) & 0xFFFF_FFFF) as u32;
    let err_code = (r & 0xFF) as u8;
    (response_len, call_error_code, err_code)
}

/// Pack a simple result: bits 32-63 = extra, bits 0-7 = errCode.
/// Matches packSimpleResult in memory.go lines 56-62.
pub fn decode_simple_result(result: i64) -> (u32, u8) {
    let r = result as u64;
    let extra = (r >> 32) as u32;
    let err_code = (r & 0xFF) as u8;
    (extra, err_code)
}

/// Decode sleep result. Matches engine.go lines 588-590.
/// bits 56-63 = status (0 = completed, 1 = suspend)
/// bits 0-55  = durationMs
pub const SLEEP_STATUS_COMPLETED: u8 = 0;
pub const SLEEP_STATUS_SUSPEND: u8 = 1;

pub fn decode_sleep_result(result: i64) -> (u8, i64) {
    let r = result as u64;
    let status = ((r >> 56) & 0xFF) as u8;
    // Mask to 56 bits for signed value
    let duration_ms = (r & 0x00FF_FFFF_FFFF_FFFF) as i64;
    (status, duration_ms)
}

/// Decode await_signals result. Matches engine.go lines 592-598.
/// bits 48-63 = sigNameLen (16 bits)
/// bits 32-47 = payloadLen (16 bits)
/// bits 16-23 = timedOut flag (1 byte)
/// bits 0-15  = errCode (16 bits)
pub fn decode_await_signals_result(result: i64) -> (u16, u16, bool, u16) {
    let r = result as u64;
    let sig_name_len = ((r >> 48) & 0xFFFF) as u16;
    let payload_len = ((r >> 32) & 0xFFFF) as u16;
    let timed_out = ((r >> 16) & 0xFFFF) != 0;
    let err_code = (r & 0xFFFF) as u16;
    (sig_name_len, payload_len, timed_out, err_code)
}

/// Decode await_promise result. ABI 2.21.
/// bits 32-63 = resultLen (32 bits)
/// bits 16-23 = timedOut flag (1 byte)
/// bits 0-15  = errCode (16 bits)
pub fn decode_await_promise_result(result: i64) -> (u32, bool, u16) {
    let r = result as u64;
    let result_len = ((r >> 32) & 0xFFFF_FFFF) as u32;
    let timed_out = ((r >> 16) & 0xFF) != 0;
    let err_code = (r & 0xFFFF) as u16;
    (result_len, timed_out, err_code)
}

/// PollSignal found flag (matches engine.go line 500: 0x0100).
pub const POLL_SIGNAL_FOUND: u32 = 0x0100;

/// Decode poll_signal result. Matches engine.go lines 500-504.
/// bits 32-63 = payloadLen
/// bits 8-15  = found flag (0x0100)
/// bits 0-7   = errCode
pub fn decode_poll_signal_result(result: i64) -> (u32, bool, u8) {
    let r = result as u64;
    let payload_len = (r >> 32) as u32;
    let flags = (r & 0xFFFF_FFFF) as u32;
    let err_code = (flags & 0xFF) as u8;
    let found = (flags >> 8) != 0;
    (payload_len, found, err_code)
}

/// Decode poll_cancellation result. Matches engine.go lines 488, 491.
/// bits 32-63 = reasonLen
/// bits 0-7   = cancelled flag (1 = cancelled)
pub fn decode_poll_cancellation_result(result: i64) -> (u32, bool) {
    let r = result as u64;
    let reason_len = (r >> 32) as u32;
    let cancelled = (r & 0xFFFF_FFFF) != 0;
    (reason_len, cancelled)
}

/// Decode get_scope result: upper 32 bits = objTypeLen, lower 32 bits = instKeyLen.
pub fn decode_get_scope_result(result: i64) -> (u32, u32) {
    let r = result as u64;
    let obj_type_len = (r >> 32) as u32;
    let inst_key_len = (r & 0xFFFF_FFFF) as u32;
    (obj_type_len, inst_key_len)
}

/// Decode incr_state result: low byte = err_code, bits 8-63 = new value (shifted right 8).
pub fn decode_incr_state_result(result: i64) -> (i64, u8) {
    let err_code = (result & 0xFF) as u8;
    let new_value = result >> 8;
    (new_value, err_code)
}

/// Decode has_state result: non-zero means the key exists.
pub fn decode_has_state_result(result: i64) -> bool {
    result != 0
}
