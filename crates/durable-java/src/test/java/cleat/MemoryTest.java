package cleat;

import static org.junit.jupiter.api.Assertions.*;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link Memory} bit-packing and encoding/decoding logic.
 * These are pure Java tests — no TeaVM or WASM dependency required.
 */
class MemoryTest {

    // ---- Export result encoding/decoding ----

    @Test
    void testEncodeExportResult() {
        long encoded = Memory.encodeExportResult(0, 42);
        assertEquals(0, Memory.decodeExportErrCode(encoded));
        assertEquals(42, Memory.decodeExportActualLen(encoded));
    }

    @Test
    void testEncodeExportResultNonZeroError() {
        long encoded = Memory.encodeExportResult(1, 12);
        assertEquals(1, Memory.decodeExportErrCode(encoded));
        assertEquals(12, Memory.decodeExportActualLen(encoded));
    }

    @Test
    void testEncodeExportResultLargeLength() {
        long encoded = Memory.encodeExportResult(0, 65535);
        assertEquals(0, Memory.decodeExportErrCode(encoded));
        assertEquals(65535, Memory.decodeExportActualLen(encoded));
    }

    @Test
    void testEncodeExportResultMaxLength() {
        // High 32 bits (actualLen) max = 0xFFFFFFFF
        long encoded = Memory.encodeExportResult(255, 0x7FFFFFFF);
        assertEquals(255, Memory.decodeExportErrCode(encoded));
        assertEquals(0x7FFFFFFF, Memory.decodeExportActualLen(encoded));
    }

    // ---- durable_call result decoding ----

    @Test
    void testDecodeCallResultSuccess() {
        // errCode=0, callErrorCode=0, responseLen=100
        long result = (100L << 40) | (0L << 8) | 0L;
        assertEquals(0, Memory.decodeCallErrCode(result));
        assertEquals(0, Memory.decodeCallErrorCode(result));
        assertEquals(100, Memory.decodeCallResponseLen(result));
    }

    @Test
    void testDecodeCallResultError() {
        // errCode=1, callErrorCode=5, responseLen=50
        long result = (50L << 40) | (5L << 8) | 1L;
        assertEquals(1, Memory.decodeCallErrCode(result));
        assertEquals(5, Memory.decodeCallErrorCode(result));
        assertEquals(50, Memory.decodeCallResponseLen(result));
    }

    @Test
    void testDecodeCallResultMaxValues() {
        // errCode=0xFF, callErrorCode=0xFFFFFFFF, responseLen=0xFFFFFF
        long result = (0xFFFFFFL << 40) | (0xFFFFFFFFL << 8) | 0xFFL;
        assertEquals(0xFF, Memory.decodeCallErrCode(result));
        assertEquals(0xFFFFFFFFL, Memory.decodeCallErrorCode(result));
        assertEquals(0xFFFFFF, Memory.decodeCallResponseLen(result));
    }

    // ---- Simple result decoding ----

    @Test
    void testDecodeSimpleResultSuccess() {
        // errCode=0, extra=42
        long result = (42L << 32) | 0L;
        assertEquals(0, Memory.decodeSimpleErrCode(result));
        assertEquals(42, Memory.decodeSimpleExtra(result));
    }

    @Test
    void testDecodeSimpleResultError() {
        // errCode=7, extra=12345
        long result = (12345L << 32) | 7L;
        assertEquals(7, Memory.decodeSimpleErrCode(result));
        assertEquals(12345, Memory.decodeSimpleExtra(result));
    }

    // ---- Sleep result decoding ----

    @Test
    void testDecodeSleepCompleted() {
        // status=0 (completed), duration=5000
        long result = 5000L; // bits 0-55 = duration, bits 56-63 = 0
        assertEquals(5000L, Memory.decodeSleepDuration(result));
        assertEquals(Memory.SLEEP_STATUS_COMPLETED, Memory.decodeSleepStatus(result));
    }

    @Test
    void testDecodeSleepSuspend() {
        // status=1 (suspend), duration=10000
        long result = (1L << 56) | 10000L;
        assertEquals(10000L, Memory.decodeSleepDuration(result));
        assertEquals(Memory.SLEEP_STATUS_SUSPEND, Memory.decodeSleepStatus(result));
    }

    @Test
    void testDecodeSleepLargeDuration() {
        // Maximum 56-bit duration
        long result = 0x00FFFFFFFFFFFFFFL;
        assertEquals(0x00FFFFFFFFFFFFFFL, Memory.decodeSleepDuration(result));
        // status should be 0 since bits 56-63 are clear
        assertEquals(0, Memory.decodeSleepStatus(result));
    }

    // ---- await_signals result decoding ----

    @Test
    void testDecodeAwaitSignalsSuccess() {
        // errCode=0, timedOut=0, payloadLen=20, sigNameLen=10
        long result = (10L << 48) | (20L << 32) | (0L << 16) | 0L;
        assertEquals(0, Memory.decodeAwaitErrCode(result));
        assertFalse(Memory.decodeAwaitTimedOut(result));
        assertEquals(20, Memory.decodeAwaitPayloadLen(result));
        assertEquals(10, Memory.decodeAwaitSigNameLen(result));
    }

    @Test
    void testDecodeAwaitSignalsTimedOut() {
        // errCode=0, timedOut=1, payloadLen=0, sigNameLen=0
        long result = (1L << 16);
        assertTrue(Memory.decodeAwaitTimedOut(result));
        assertEquals(0, Memory.decodeAwaitPayloadLen(result));
        assertEquals(0, Memory.decodeAwaitSigNameLen(result));
    }

    // ---- poll_signal result decoding ----

    @Test
    void testDecodePollSignalFound() {
        // errCode=0, found=0x0100, payloadLen=30
        long result = (30L << 32) | (0x0100L << 8) | 0L;
        assertEquals(0, Memory.decodePollSigErrCode(result));
        assertTrue(Memory.decodePollSigFound(result));
        assertEquals(30, Memory.decodePollSigPayloadLen(result));
    }

    @Test
    void testDecodePollSignalNotFound() {
        // errCode=0, found=0, payloadLen=0
        long result = 0L;
        assertEquals(0, Memory.decodePollSigErrCode(result));
        assertFalse(Memory.decodePollSigFound(result));
        assertEquals(0, Memory.decodePollSigPayloadLen(result));
    }

    // ---- poll_cancellation result decoding ----

    @Test
    void testDecodePollCancelNotCancelled() {
        long result = 0L;
        assertFalse(Memory.decodePollCancelCancelled(result));
        assertEquals(0, Memory.decodePollCancelReasonLen(result));
    }

    @Test
    void testDecodePollCancelCancelled() {
        // cancelled=1, reasonLen=15
        long result = (15L << 32) | 1L;
        assertTrue(Memory.decodePollCancelCancelled(result));
        assertEquals(15, Memory.decodePollCancelReasonLen(result));
    }

    @Test
    void testDecodePollCancelCancelledNoReason() {
        // cancelled=1, reasonLen=0
        long result = 1L;
        assertTrue(Memory.decodePollCancelCancelled(result));
        assertEquals(0, Memory.decodePollCancelReasonLen(result));
    }

    // ---- Constants ----

    @Test
    void testSuspendSentinel() {
        assertEquals(1L << 62, Memory.SUSPEND_SENTINEL);
        assertEquals(0x4000000000000000L, Memory.SUSPEND_SENTINEL);
    }

    @Test
    void testMemoryLayoutConstants() {
        assertEquals(10 * 1024 * 1024, Memory.SCRATCH_BASE);          // 0xA00000
        assertEquals(65536, Memory.OUT_BUF_SIZE);                     // 64 KiB
        assertEquals(10 * 1024 * 1024 + 65536, Memory.OUTPUT_OFFSET); // 0xA10000
    }
}
