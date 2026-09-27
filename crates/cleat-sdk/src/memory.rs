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

/// Bit 31 of a host-call result: the host is refusing this call because the
/// workflow is in a defer segment and the call would start new work.
///
/// Distinct from [`SUSPEND_SENTINEL`], which is bit 62 and is what the guest
/// returns to the host from an export. This one travels the other way -- host
/// to guest, inside an ordinary result word -- and bit 31 was chosen because it
/// is the one bit free in all six result layouts a call that can start fresh
/// work returns. See IMPROVEMENT-PLAN 3.84 and ABI.md.
///
/// The engine's copy is `callSuspendSentinel` in `engine/memory.go`. The two
/// must agree, and nothing in either language can see the other, so
/// `TestTheRustSDKAgreesOnTheStopBit` in `engine/` reads this file and pins the
/// value.
pub const SUSPEND_STOP_BIT: i64 = 1 << 31;

/// Read a host-written result out of a guest-owned buffer, bounded by the
/// buffer itself.
///
/// # Why this exists, and why it takes a slice
///
/// The host does not always write the buffer it reports a length for. On a
/// bad-parameter refusal `engine/imports.go` returns `errBadParam` =
/// `0xFFFFFFFF_00000001` from 54 sites, before the handler runs, and the guest
/// decodes a LENGTH out of the very bits that carry the sentinel:
///
/// | layout | decoded length | against a 65536-byte buffer |
/// |---|---|---|
/// | `decode_simple_result` | 4294967295 | overruns by ~65535x |
/// | `decode_cleat_call_result` | 16777215 | overruns by ~256x |
///
/// So a wrapper that reads its buffer on the error path -- which is the RIGHT
/// thing to do, because the host's real message is usually there -- reads far
/// out of bounds unless something bounds it. `read_string` cannot: it takes a
/// raw pointer and has no idea how big the region is.
///
/// This takes the `&[u8]` instead, so the capacity travels with the data and
/// there is no second argument to get wrong. It is also entirely safe code,
/// which is the point: the buffers in `host_calls.rs` are ordinary `Vec<u8>`,
/// and reading them back never needed `unsafe` at all.
///
/// The Java SDK's `readOutput` has clamped since it was written
/// (`Math.min(maxLen, OUT_BUF_SIZE)`), and Go's `hostErrMessage` bounds-checks
/// for exactly this reason -- see IMPROVEMENT-PLAN 3.200, which says so and
/// which the Rust side did not follow.
pub fn read_result(buf: &[u8], len: u32) -> String {
    let n = (len as usize).min(buf.len());
    if n == 0 {
        return String::new();
    }
    String::from_utf8_lossy(&buf[..n]).into_owned()
}

/// Return the host's own error message, or `fallback` when it wrote none.
///
/// # Why a helper rather than a read at each site
///
/// A call that has an output buffer usually puts its failure reason there --
/// `engine/children.go` writes `rec.Err`, `AwaitPromise` writes
/// `rec.PromiseError`, `SideEffect` writes its `errMsg` -- and a guest that
/// reports the bare `errCode` instead throws away the only thing that says what
/// went wrong. That is IMPROVEMENT-PLAN 3.200, fixed there for the generated Go
/// adapters and left standing in this SDK for fifteen wrappers.
///
/// The fallback is load-bearing, not decoration. Not every call writes on every
/// error path: `cleat_json_parse` returns `packSimpleResult(1)` with nothing
/// written when its input is unreadable, so an unconditional read would replace
/// a useful message with an empty one. Preferring the host's text *when there is
/// any* is strictly better than either alternative.
///
/// Taking the fallback by value rather than by closure is deliberate: every
/// caller already had that `format!` written, so the change at each site is to
/// wrap it, and the diff shows exactly what was preserved.
pub fn host_message_or(buf: &[u8], len: u32, fallback: String) -> String {
    // Stop at the first NUL, and this is the whole correctness of the function.
    //
    // "The host wrote nothing" and "the host reported a length" are independent.
    // On a bad-parameter refusal `engine/imports.go` returns errBadParam BEFORE
    // the handler runs -- nothing is written -- yet the guest still decodes a
    // length out of the sentinel's bits: 4294967295 in the simple layout.
    // read_result clamps that to the buffer, so a naive is_empty() check sees
    // 65536 zero bytes, decides the host "wrote a message", and returns 64KB of
    // NULs as the error text. Measured, not imagined:
    //
    //     decoded len = 4294967295   returned len = 65536   all NUL = true
    //
    // That is strictly worse than the bare code it replaces. The host writes a
    // UTF-8 message with no interior NUL, and an unwritten buffer is zeroed, so
    // truncating at the first NUL is exactly "what the host actually wrote".
    let raw = read_result(buf, len);
    let msg = match raw.find('\0') {
        Some(i) => &raw[..i],
        None => &raw[..],
    };
    if msg.is_empty() {
        return fallback;
    }
    msg.to_string()
}

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

pub fn decode_incr_state_result(result: i64) -> (i64, u8) {
    let err_code = (result & 0xFF) as u8;
    let new_value = result >> 8;
    (new_value, err_code)
}

pub fn decode_has_state_result(result: i64) -> bool {
    result != 0
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The bad-parameter sentinel decodes to a length no buffer satisfies.
    ///
    /// This is the whole reason `read_result` exists, so it is asserted against
    /// the sentinel's real value rather than against a made-up large number.
    /// `engine/imports.go` returns `errBadParam` = 0xFFFFFFFF_00000001 from 54
    /// sites when it cannot read a guest string -- BEFORE the handler runs, so
    /// nothing has been written to the buffer at all -- and both result layouts
    /// decode a length out of the bits carrying the sentinel.
    #[test]
    fn err_bad_param_decodes_to_a_length_no_buffer_satisfies() {
        let bad = 0xFFFFFFFF_00000001u64 as i64;

        let (simple_len, err) = decode_simple_result(bad);
        assert_eq!(err, 1, "the low byte of errBadParam is 1");
        assert_eq!(simple_len, 4_294_967_295);
        assert!(
            simple_len as usize > OUT_BUF_SIZE as usize,
            "decode_simple_result gives {simple_len} against a {OUT_BUF_SIZE}-byte buffer"
        );

        let (call_len, _call_err, err2) = decode_cleat_call_result(bad);
        assert_eq!(err2, 1);
        assert_eq!(call_len, 16_777_215);
        assert!(
            call_len as usize > OUT_BUF_SIZE as usize,
            "decode_cleat_call_result gives {call_len} against a {OUT_BUF_SIZE}-byte buffer"
        );
    }

    /// read_result clamps to the buffer rather than to the reported length.
    ///
    /// Falsification: `String::from_utf8_lossy(&buf[..len as usize])` in place
    /// of the clamp panics here with a slice-index error -- in the SDK proper,
    /// where the read is from a raw pointer, the same mistake is an
    /// out-of-bounds READ rather than a panic, which is why this is checked on
    /// the safe helper.
    #[test]
    fn read_result_clamps_a_bogus_length_to_the_buffer() {
        let buf = vec![b'x'; 16];

        let (bogus, _) = decode_simple_result(0xFFFFFFFF_00000001u64 as i64);
        assert_eq!(read_result(&buf, bogus).len(), 16);

        // and the ordinary cases still behave
        assert_eq!(read_result(&buf, 4), "xxxx");
        assert_eq!(read_result(&buf, 0), "");
        assert_eq!(read_result(&[], 99), "");
    }

    /// host_message_or prefers the host's text, and falls back when there is none.
    ///
    /// The NUL case is the one that matters and it is not hypothetical: on a
    /// bad-parameter refusal the host writes nothing and the guest still decodes
    /// a length of 4294967295, so a naive is_empty() check returns 64KB of NULs
    /// as the error message -- worse than the bare code it replaced.
    #[test]
    fn host_message_or_prefers_the_hosts_text_and_falls_back_when_there_is_none() {
        let fallback = || "host error code 1".to_string();

        // The host wrote something: use it.
        let mut wrote = vec![0u8; 64];
        wrote[..11].copy_from_slice(b"no such run");
        assert_eq!(host_message_or(&wrote, 11, fallback()), "no such run");

        // The host wrote nothing and reported a real zero.
        assert_eq!(host_message_or(&[0u8; 64], 0, fallback()), "host error code 1");

        // The host wrote nothing and reported errBadParam's bogus length. This
        // is the case a plain is_empty() gets wrong.
        let (bogus, _) = decode_simple_result(0xFFFFFFFF_00000001u64 as i64);
        let got = host_message_or(&vec![0u8; OUT_BUF_SIZE as usize], bogus, fallback());
        assert_eq!(got, "host error code 1",
            "an unwritten buffer with a bogus length must fall back, not return {} NUL bytes",
            got.len());

        // A real message that does not fill the buffer keeps its own bytes and
        // does not drag the trailing zeros along.
        let mut partial = vec![0u8; 512];
        partial[..5].copy_from_slice(b"boom!");
        assert_eq!(host_message_or(&partial, 512, fallback()), "boom!");
    }

    /// Every error branch must read the SAME buffer and length the success path
    /// reads, and this is the property the wiring can actually get wrong.
    ///
    /// The 26 call sites were converted mechanically, deriving the buffer and
    /// length from each function's own success-path `read_result`. That is
    /// exactly the kind of edit that is right in bulk and wrong in one place,
    /// and the failure is silent: a wrapper that reads a NEIGHBOURING buffer
    /// still compiles, still returns a String, and reports another call's data
    /// as this call's error message.
    ///
    /// It is not covered end-to-end. The plugin harness does not drive an error
    /// through any of these paths -- in an in-memory environment they succeed or
    /// suspend -- so nothing else in the tree would notice. This checks the
    /// invariant directly instead of hoping a fixture reaches it.
    ///
    /// `await_signals_ms` is the deliberate exception: it has two buffers, and
    /// the signal-NAME one is the one a reason is written into. It is excluded
    /// by name rather than by loosening the rule for everyone.
    #[test]
    fn every_error_branch_reads_the_same_buffer_as_its_success_path() {
        let src = include_str!("host_calls.rs");

        let mut mismatched = Vec::new();
        let mut checked = 0;

        for (i, _) in src.match_indices("host_message_or(") {
            // the enclosing fn name
            let head = &src[..i];
            let fn_start = match head.rfind("\n    pub fn ") {
                Some(p) => p + "\n    pub fn ".len(),
                None => continue,
            };
            let name: String = src[fn_start..]
                .chars()
                .take_while(|c| c.is_alphanumeric() || *c == '_')
                .collect();
            if name == "await_signals_ms" {
                continue; // two buffers, documented at the call site
            }

            // what the error branch reads
            let call = &src[i..];
            let open = call.find('(').unwrap();
            let args = &call[open + 1..];
            let err_buf: String = args
                .trim_start()
                .trim_start_matches('&')
                .chars()
                .take_while(|c| c.is_alphanumeric() || *c == '_')
                .collect();

            // what the success path reads, in the same function
            let body = &src[fn_start..];
            let ok = match body.find("memory::read_result(&") {
                Some(p) => &body[p + "memory::read_result(&".len()..],
                None => continue,
            };
            let ok_buf: String = ok
                .chars()
                .take_while(|c| c.is_alphanumeric() || *c == '_')
                .collect();

            checked += 1;
            if err_buf != ok_buf {
                mismatched.push(format!("  {name}: error branch reads {err_buf}, success path reads {ok_buf}"));
            }
        }

        // Without this the loop can pass by matching nothing, which is how a
        // renamed helper turns a guard into a no-op.
        assert!(
            checked >= 10,
            "only {checked} host_message_or call sites were checked, far below the 13 wired in \
             IMPROVEMENT-PLAN 3.251. The scan has stopped matching and this guard is now vacuous."
        );
        assert!(
            mismatched.is_empty(),
            "error branches reading a different buffer than their own success path:\n{}\n\n\
             A wrapper that reads a neighbouring buffer compiles and returns a String, so it \
             reports another call's data as this call's error message.",
            mismatched.join("\n")
        );
    }

    /// `host_calls.rs` must not read a host-reported length from a raw pointer.
    ///
    /// `read_string` takes a `*const u8` and cannot bound anything, so every
    /// call site that passes a host-reported length is an out-of-bounds read
    /// waiting for a bad-parameter refusal. All 40 sites in `host_calls.rs`
    /// were converted to `read_result`; this stops the 41st being written.
    ///
    /// `read_string` itself is NOT deleted and must not be: `cleat-macro`'s
    /// generated entry point calls it on `(args_ptr, args_len)` handed in by
    /// the host, where there is no slice to bound against and the host is the
    /// one that chose the length. That is a different situation from reading
    /// back a buffer the guest allocated, and conflating them is what this
    /// test exists to prevent.
    ///
    /// Reads the file rather than grepping the repo, so it cannot be satisfied
    /// by a comment elsewhere and needs no tooling to run.
    #[test]
    fn host_calls_reads_every_buffer_through_read_result() {
        let src = include_str!("host_calls.rs");

        let unbounded = src
            .lines()
            .enumerate()
            .filter(|(_, l)| {
                let t = l.trim_start();
                !t.starts_with("//") && !t.starts_with("///") && t.contains("read_string(")
            })
            .map(|(i, l)| format!("  {}: {}", i + 1, l.trim()))
            .collect::<Vec<_>>();

        assert!(
            unbounded.is_empty(),
            "host_calls.rs reads a buffer through read_string, which takes a raw \
             pointer and bounds nothing:\n{}\n\n\
             Use memory::read_result(&buf, len) instead. On a bad-parameter refusal the \
             host returns errBadParam without writing the buffer, and the length decodes \
             to 4294967295 (simple layout) or 16777215 (call layout) against a 65536-byte \
             buffer. See IMPROVEMENT-PLAN 3.244.",
            unbounded.join("\n")
        );

        // A narrow backstop, and worth being honest about its scope: a rename
        // or a moved file is caught by the COMPILER long before this runs
        // (include_str! fails on a missing path, and the tests below call
        // read_result by name), which was checked rather than assumed -- an
        // attempted rename produced four E0425s and never reached this
        // assertion.
        //
        // What it does still catch is the case the first assertion cannot: a
        // host_calls.rs that reads no buffers at all, where "no unbounded
        // reads" is true and vacuous.
        assert!(
            src.contains("read_result("),
            "host_calls.rs contains no read_result( call at all, so the check above passed \
             vacuously rather than because the reads are bounded."
        );
    }

    /// A length shorter than the buffer must NOT be rounded up to it.
    ///
    /// Clamping is a maximum, not a replacement: the host reports how much it
    /// actually wrote into a buffer that is almost always larger, so a helper
    /// that returned the whole buffer would append thousands of NUL bytes to
    /// every successful result. That failure passes the clamp test above.
    #[test]
    fn read_result_does_not_round_a_short_length_up() {
        let mut buf = vec![0u8; 64];
        buf[..5].copy_from_slice(b"hello");
        assert_eq!(read_result(&buf, 5), "hello");
    }
}
