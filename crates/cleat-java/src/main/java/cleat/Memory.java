package cleat;

import java.nio.charset.StandardCharsets;

/**
 * Static helpers for WASM linear memory I/O and bit-packing.
 * <p>
 * Matches the cleat WASM ABI memory layout and bit-packing conventions
 * defined in {@code crates/cleat-sdk/src/memory.rs} and {@code ABI.md}.
 * <p>
 * <h3>Memory Layout</h3>
 * <pre>
 * Offset         | Size    | Use
 * ---------------+---------+-----------------------------------
 * 0xA00000 (10M) | Var     | Scratch region (input args)
 * 0xA10000       | 65536   | Output buffer (results to host)
 * </pre>
 * <p>
 * <h3>TeaVM WASM Memory Access</h3>
 * Under TeaVM's WASM target, linear memory is accessed through
 * {@link org.teavm.interop.Address}.  The SDK uses the scratch region at
 * {@value #SCRATCH_BASE} for packing input arguments to host calls and the
 * output buffer at {@value #OUTPUT_OFFSET} for reading results.
 */
public final class Memory {

    // ---- Constants ----

    /** Size of the output buffer in bytes (64 KiB). */
    public static final int OUT_BUF_SIZE = 65536;

    /** Scratch region base offset in WASM linear memory (10 MiB = 0xA00000). */
    public static final int SCRATCH_BASE = 10 * 1024 * 1024;

    /** Output buffer offset (10 MiB + 64 KiB = 0xA10000). */
    public static final int OUTPUT_OFFSET = SCRATCH_BASE + OUT_BUF_SIZE;

    /**
     * Suspend sentinel value returned by export functions when the workflow
     * needs to suspend (e.g. for a timer or signal).  Equal to {@code 1L << 62}.
     */
    public static final long SUSPEND_SENTINEL = 1L << 62;

    /**
     * Error code for non-retryable terminal errors.
     * Returned as the errCode in {@link #encodeExportResult(int, int)} when
     * a {@link TerminalError} is thrown by the workflow method.
     */
    public static final int TERMINAL_ERROR_CODE = 2;

    // Sleep status constants
    /** Sleep completed normally (replay path). */
    public static final int SLEEP_STATUS_COMPLETED = 0;
    /** Sleep should cause workflow suspension (fresh execution path). */
    public static final int SLEEP_STATUS_SUSPEND = 1;

    private Memory() {
        // Utility class — no instantiation.
    }

    // ---- Raw byte-level memory access via TeaVM Address ----

    /**
     * Read a single byte from WASM linear memory at the given address.
     * <p>
     * Uses {@link org.teavm.interop.Address#fromInt(int)}.{@code getByte()}
     * which TeaVM compiles to an {@code i32.load8_u} WASM instruction.
     *
     * @param address the linear memory offset
     * @return the byte value at that address
     */
    private static byte readByte(int address) {
        return org.teavm.interop.Address.fromInt(address).getByte();
    }

    /**
     * Write a single byte to WASM linear memory at the given address.
     * <p>
     * Uses {@link org.teavm.interop.Address#fromInt(int)}.{@code setByte()}
     * which TeaVM compiles to an {@code i32.store8} WASM instruction.
     *
     * @param address the linear memory offset
     * @param value   the byte value to write
     */
    private static void writeByte(int address, byte value) {
        org.teavm.interop.Address.fromInt(address).putByte(value);
    }

    // ---- String I/O ----

    /**
     * Read a UTF-8 encoded string from WASM linear memory at the given pointer
     * and length.
     *
     * @param ptr the memory offset (inclusive)
     * @param len the number of bytes to read
     * @return the decoded string, or empty string if {@code len <= 0}
     */
    public static String readString(int ptr, int len) {
        if (len <= 0) {
            return "";
        }
        byte[] bytes = new byte[len];
        for (int i = 0; i < len; i++) {
            bytes[i] = readByte(ptr + i);
        }
        return new String(bytes, StandardCharsets.UTF_8);
    }

    /**
     * Write a UTF-8 encoded string to WASM linear memory starting at the given
     * pointer, truncating to a maximum of {@code maxLen} bytes.
     *
     * @param ptr    the memory offset to start writing at
     * @param maxLen the maximum number of bytes to write
     * @param s      the string to write
     * @return the number of bytes actually written
     */
    public static int writeString(int ptr, int maxLen, String s) {
        if (s == null || maxLen <= 0) {
            return 0;
        }
        byte[] bytes = s.getBytes(StandardCharsets.UTF_8);
        int len = Math.min(bytes.length, maxLen);
        for (int i = 0; i < len; i++) {
            writeByte(ptr + i, bytes[i]);
        }
        return len;
    }

    // ---- Export result encoding / decoding ----

    /**
     * Encode an export result into the ABI format:
     * <pre>
     * low 32 bits  = errCode
     * high 32 bits = actualLen
     * </pre>
     *
     * @param errCode   0 for success, non-zero for error
     * @param actualLen number of bytes written to the output buffer
     * @return the packed i64 result
     */
    public static long encodeExportResult(int errCode, int actualLen) {
        return ((long) actualLen << 32) | (errCode & 0xFFFFFFFFL);
    }

    /**
     * Decode the error code from an export result (low 32 bits).
     */
    public static int decodeExportErrCode(long result) {
        return (int) (result & 0xFFFFFFFFL);
    }

    /**
     * Decode the actual length from an export result (high 32 bits).
     */
    public static int decodeExportActualLen(long result) {
        return (int) (result >>> 32);
    }

    // ---- cleat_call result decoding ----
    // bits 40-63 = responseLen (24 bits)
    // bits  8-39 = callErrorCode (32 bits)
    // bits  0-7  = errCode (8 bits)

    /**
     * Decode the response length from a {@code cleat_call} result
     * (bits 40-63).
     */
    public static int decodeCallResponseLen(long result) {
        return (int) ((result >>> 40) & 0xFFFFFFL);
    }

    /**
     * Decode the call error code from a {@code cleat_call} result
     * (bits 8-39).
     */
    public static int decodeCallErrorCode(long result) {
        return (int) ((result >>> 8) & 0xFFFFFFFFL);
    }

    /**
     * Decode the top-level errCode from a {@code cleat_call} result
     * (bits 0-7).  0 = success, 1 = error.
     */
    public static int decodeCallErrCode(long result) {
        return (int) (result & 0xFFL);
    }

    // ---- Simple result decoding (shared by many host calls) ----
    // bits 32-63 = extra
    // bits  0-7  = errCode

    /**
     * Decode the error code from a simple result (bits 0-7).
     * Used by: version, min_version, defer, child_workflow, await_child,
     * continue_as_new.
     */
    public static int decodeSimpleErrCode(long result) {
        return (int) (result & 0xFFL);
    }

    /**
     * Decode the extra value from a simple result (bits 32-63).
     * Used by: now (ms since epoch), random, version, min_version,
     * defer (deferIDLen), child_workflow (runIDLen), await_child (resultLen).
     */
    public static int decodeSimpleExtra(long result) {
        return (int) (result >>> 32);
    }

    // ---- Sleep result decoding ----
    // bits 56-63 = status (0 = completed, 1 = suspend)
    // bits  0-55 = durationMs

    /**
     * Decode the sleep duration from a {@code cleat_sleep} result
     * (bits 0-55).
     */
    public static long decodeSleepDuration(long result) {
        return result & 0x00FFFFFFFFFFFFFFL;
    }

    /**
     * Decode the sleep status from a {@code cleat_sleep} result
     * (bits 56-63).
     *
     * @return {@link #SLEEP_STATUS_COMPLETED} (0) on replay,
     *         {@link #SLEEP_STATUS_SUSPEND} (1) on fresh execution
     */
    public static int decodeSleepStatus(long result) {
        return (int) (result >>> 56);
    }

    // ---- await_signals result decoding ----
    // bits 48-63 = sigNameLen (16 bits)
    // bits 32-47 = payloadLen (16 bits)
    // bits 16-31 = timedOut (1 byte)
    // bits  0-15 = errCode (16 bits)

    /**
     * Decode the signal name length from {@code cleat_await_signals}
     * (bits 48-63).
     */
    public static int decodeAwaitSigNameLen(long result) {
        return (int) ((result >>> 48) & 0xFFFFL);
    }

    /**
     * Decode the payload length from {@code cleat_await_signals}
     * (bits 32-47).
     */
    public static int decodeAwaitPayloadLen(long result) {
        return (int) ((result >>> 32) & 0xFFFFL);
    }

    /**
     * Decode the timedOut flag from {@code cleat_await_signals}
     * (bits 16-31).
     *
     * @return true if the timeout expired before a signal was received
     */
    public static boolean decodeAwaitTimedOut(long result) {
        return ((result >>> 16) & 0xFFFFL) != 0;
    }

    /**
     * Decode the error code from {@code cleat_await_signals}
     * (bits 0-15).
     */
    public static int decodeAwaitErrCode(long result) {
        return (int) (result & 0xFFFFL);
    }

    // ---- await_promise result decoding ----
    // bits 32-63 = resultLen (reuses decodeSimpleExtra)
    // bits 16-23 = timedOut (8 bits, 0 or 1)
    // bits  0-15 = errCode (reuses decodeAwaitErrCode)

    /**
     * Decode the timedOut flag from {@code cleat_await_promise}
     * (bits 16-23).
     *
     * @return true if the timeout expired before the promise resolved
     */
    public static boolean decodeAwaitPromiseTimedOut(long result) {
        return ((result >>> 16) & 0xFFL) != 0;
    }

    // ---- poll_signal result decoding ----
    // bits 32-63 = payloadLen
    // bits  8-15 = found flag (0x0100 = 256 if found)
    // bits  0-7  = errCode

    /**
     * Decode the payload length from {@code cleat_poll_signal}
     * (bits 32-63).
     */
    public static int decodePollSigPayloadLen(long result) {
        return (int) (result >>> 32);
    }

    /**
     * Decode the found flag from {@code cleat_poll_signal}
     * (bits 8-15).  Returns true if the value equals 0x0100 (256).
     */
    public static boolean decodePollSigFound(long result) {
        return ((result >>> 8) & 0xFFL) == 0x0100;
    }

    /**
     * Decode the error code from {@code cleat_poll_signal}
     * (bits 0-7).
     */
    public static int decodePollSigErrCode(long result) {
        return (int) (result & 0xFFL);
    }

    // ---- poll_cancellation result decoding ----
    // bits 32-63 = reasonLen
    // bits  0-31 = cancelled (non-zero = true)

    /**
     * Decode the cancelled flag from {@code cleat_poll_cancellation}
     * (bits 0-31).  Returns true if non-zero.
     */
    public static boolean decodePollCancelCancelled(long result) {
        return (result & 0xFFFFFFFFL) != 0;
    }

    /**
     * Decode the cancellation reason length from
     * {@code cleat_poll_cancellation} (bits 32-63).
     */
    public static int decodePollCancelReasonLen(long result) {
        return (int) (result >>> 32);
    }
}
